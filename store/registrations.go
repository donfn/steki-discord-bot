package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const regColumns = `id, user_id, guild_id, channel_id, category_id, step, email, code,
	code_expires_at, code_sent_count, failed_attempts, last_activity_at, nudged_at,
	created_at, completed_at, outcome`

func scanRegistration(row pgx.Row) (*Registration, error) {
	var r Registration
	err := row.Scan(&r.ID, &r.UserID, &r.GuildID, &r.ChannelID, &r.CategoryID, &r.Step,
		&r.Email, &r.Code, &r.CodeExpiresAt, &r.CodeSentCount, &r.FailedAttempts,
		&r.LastActivityAt, &r.NudgedAt, &r.CreatedAt, &r.CompletedAt, &r.Outcome)
	if err != nil {
		return nil, noRows(err)
	}
	return &r, nil
}

func collectRegistrations(rows pgx.Rows) ([]*Registration, error) {
	defer rows.Close()
	var out []*Registration
	for rows.Next() {
		r, err := scanRegistration(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertMemberJoin records that a member joined. On a rejoin it bumps
// join_count and clears left_at, which is what makes repeat joiners visible to
// moderators.
func (s *Store) UpsertMemberJoin(ctx context.Context, userID, username string) error {
	_, err := s.pool.Exec(ctx, `
		insert into members (user_id, username, last_join_at)
		values ($1, $2, now())
		on conflict (user_id) do update set
			username     = excluded.username,
			last_join_at = now(),
			join_count   = members.join_count + 1,
			left_at      = null`,
		userID, username)
	return err
}

// EnsureMember creates a bare member row if one does not exist, without
// touching join counters. Used by backfills and moderator commands.
func (s *Store) EnsureMember(ctx context.Context, userID, username string) error {
	_, err := s.pool.Exec(ctx, `
		insert into members (user_id, username)
		values ($1, $2)
		on conflict (user_id) do nothing`,
		userID, username)
	return err
}

// MarkMemberLeft stamps left_at.
func (s *Store) MarkMemberLeft(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx,
		`update members set left_at = now() where user_id = $1`, userID)
	return err
}

// GetMember returns a member or ErrNotFound.
func (s *Store) GetMember(ctx context.Context, userID string) (*Member, error) {
	var m Member
	err := s.pool.QueryRow(ctx, `
		select user_id, username, first_seen_at, last_join_at, join_count,
		       left_at, verified_at, verified_email, kicked_count
		from members where user_id = $1`, userID).
		Scan(&m.UserID, &m.Username, &m.FirstSeenAt, &m.LastJoinAt, &m.JoinCount,
			&m.LeftAt, &m.VerifiedAt, &m.VerifiedEmail, &m.KickedCount)
	if err != nil {
		return nil, noRows(err)
	}
	return &m, nil
}

// EnsureOpenRegistration returns the member's open registration, creating one
// if none exists, and reports whether it created it. The insert relies on the
// partial unique index registrations_one_open_per_user, so two concurrent
// GUILD_MEMBER_ADD events for the same user cannot produce two rows — and
// therefore cannot produce two register channels.
func (s *Store) EnsureOpenRegistration(ctx context.Context, userID, guildID string) (*Registration, bool, error) {
	row := s.pool.QueryRow(ctx, `
		insert into registrations (user_id, guild_id)
		values ($1, $2)
		on conflict (user_id) where outcome is null do nothing
		returning `+regColumns, userID, guildID)

	reg, err := scanRegistration(row)
	if err == nil {
		return reg, true, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, false, err
	}

	reg, err = s.OpenRegistration(ctx, userID)
	if err != nil {
		return nil, false, err
	}
	return reg, false, nil
}

// OpenRegistration returns the member's current open registration.
func (s *Store) OpenRegistration(ctx context.Context, userID string) (*Registration, error) {
	return scanRegistration(s.pool.QueryRow(ctx,
		`select `+regColumns+` from registrations
		 where user_id = $1 and outcome is null`, userID))
}

// RegistrationByChannel looks up the open registration attached to a channel.
func (s *Store) RegistrationByChannel(ctx context.Context, channelID string) (*Registration, error) {
	return scanRegistration(s.pool.QueryRow(ctx,
		`select `+regColumns+` from registrations
		 where channel_id = $1 and outcome is null`, channelID))
}

// AttachChannel records the channel and category a registration was given.
func (s *Store) AttachChannel(ctx context.Context, regID, channelID, categoryID string) error {
	_, err := s.pool.Exec(ctx, `
		update registrations
		set channel_id = $2, category_id = nullif($3, ''), last_activity_at = now()
		where id = $1`, regID, channelID, categoryID)
	return err
}

// Touch refreshes last_activity_at and clears any nudge, so a member who comes
// back after the 1h nudge starts the idle clock over.
func (s *Store) Touch(ctx context.Context, regID string) error {
	_, err := s.pool.Exec(ctx, `
		update registrations
		set last_activity_at = now(), nudged_at = null
		where id = $1`, regID)
	return err
}

// BeginCodeStep stores the submitted address and the freshly generated code,
// and moves the registration to the awaiting_code step.
func (s *Store) BeginCodeStep(ctx context.Context, regID, email, code string, expiresAt time.Time) error {
	_, err := s.pool.Exec(ctx, `
		update registrations set
			step             = $2,
			email            = $3,
			code             = $4,
			code_expires_at  = $5,
			code_sent_count  = code_sent_count + 1,
			failed_attempts  = 0,
			last_activity_at = now(),
			nudged_at        = null
		where id = $1`,
		regID, StepAwaitingCode, email, code, expiresAt)
	return err
}

// IncrementFailedAttempts records a wrong code and returns the new count.
func (s *Store) IncrementFailedAttempts(ctx context.Context, regID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		update registrations
		set failed_attempts = failed_attempts + 1, last_activity_at = now(), nudged_at = null
		where id = $1
		returning failed_attempts`, regID).Scan(&n)
	if err != nil {
		return 0, noRows(err)
	}
	return n, nil
}

// ResetToEmailStep destroys the stored code after too many wrong guesses and
// sends the member back to step one.
func (s *Store) ResetToEmailStep(ctx context.Context, regID string) error {
	_, err := s.pool.Exec(ctx, `
		update registrations set
			code = null, code_expires_at = null,
			failed_attempts = 0, step = $2, email = null,
			last_activity_at = now(), nudged_at = null
		where id = $1`, regID, StepAwaitingEmail)
	return err
}

// CloseRegistration marks a registration terminal. Closing frees the partial
// unique index so the member may later open a new one.
func (s *Store) CloseRegistration(ctx context.Context, regID, outcome string) error {
	_, err := s.pool.Exec(ctx, `
		update registrations
		set outcome = $2, completed_at = now()
		where id = $1 and outcome is null`, regID, outcome)
	return err
}

// CloseOpenForUser closes whatever registration a user currently has open.
func (s *Store) CloseOpenForUser(ctx context.Context, userID, outcome string) error {
	_, err := s.pool.Exec(ctx, `
		update registrations
		set outcome = $2, completed_at = now()
		where user_id = $1 and outcome is null`, userID, outcome)
	return err
}

// ClearVerification forgets that a member verified, so that nothing, the
// reaper's role restore included, treats them as verified until they register
// again. With release, the addresses bound to them are freed as well, which
// lets the member register with a different one or someone else with theirs;
// without it, the member keeps their claim and can re-register with it. It
// returns how many addresses were released.
func (s *Store) ClearVerification(ctx context.Context, userID string, release bool) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		update members set verified_at = null, verified_email = null
		where user_id = $1`, userID); err != nil {
		return 0, err
	}

	var released int64
	if release {
		for _, q := range []string{
			`delete from verified_emails where user_id = $1`,
			`delete from legacy_email_hashes where user_id = $1`,
		} {
			tag, err := tx.Exec(ctx, q, userID)
			if err != nil {
				return 0, err
			}
			released += tag.RowsAffected()
		}
	}

	if _, err := tx.Exec(ctx, `
		update registrations
		set outcome = $2, completed_at = now()
		where user_id = $1 and outcome is null`, userID, OutcomeModerator); err != nil {
		return 0, err
	}
	return released, tx.Commit(ctx)
}

// MarkNudged stamps the 1h in-channel nudge so it is sent only once.
func (s *Store) MarkNudged(ctx context.Context, regID string) error {
	_, err := s.pool.Exec(ctx,
		`update registrations set nudged_at = now() where id = $1`, regID)
	return err
}

// MarkVerified is the transactional completion of a registration: it records
// the address, links it to the member, and closes the registration. Doing all
// three in one transaction is what prevents a half-verified member.
//
// It returns ErrEmailTaken when the address is already bound to a different
// account. The submit-time dedup check cannot rule that out on its own, since
// two members may submit the same address before either enters their code.
func (s *Store) MarkVerified(ctx context.Context, regID, userID, email string) error {
	email = NormalizeEmail(email)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var claimed string
	err = tx.QueryRow(ctx, `
		insert into verified_emails (email, user_id, domain, source)
		values ($1, $2, $3, $4)
		on conflict (email) do update set user_id = excluded.user_id
			where verified_emails.user_id is null
			   or verified_emails.user_id = excluded.user_id
		returning email`,
		email, userID, EmailDomain(email), SourceLive).Scan(&claimed)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrEmailTaken
	}
	if err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `
		update members set verified_at = now(), verified_email = $2 where user_id = $1`,
		userID, email); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `
		update registrations
		set outcome = $2, step = $3, completed_at = now()
		where id = $1 and outcome is null`,
		regID, OutcomeVerified, StepDone); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// ListNudgeCandidates returns open registrations idle longer than idle that
// have not yet been nudged.
func (s *Store) ListNudgeCandidates(ctx context.Context, idle time.Duration, limit int) ([]*Registration, error) {
	rows, err := s.pool.Query(ctx, `
		select `+regColumns+` from registrations
		where outcome is null
		  and nudged_at is null
		  and channel_id is not null
		  and last_activity_at <= now() - make_interval(secs => $1)
		order by last_activity_at asc
		limit $2`, idle.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	return collectRegistrations(rows)
}

// ListKickCandidates returns open registrations idle longer than idle.
func (s *Store) ListKickCandidates(ctx context.Context, idle time.Duration, limit int) ([]*Registration, error) {
	rows, err := s.pool.Query(ctx, `
		select `+regColumns+` from registrations
		where outcome is null
		  and last_activity_at <= now() - make_interval(secs => $1)
		order by last_activity_at asc
		limit $2`, idle.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	return collectRegistrations(rows)
}

// ListOpen returns every open registration, oldest activity first. Used by the
// reconciler and by /reg stuck.
func (s *Store) ListOpen(ctx context.Context) ([]*Registration, error) {
	rows, err := s.pool.Query(ctx, `
		select `+regColumns+` from registrations
		where outcome is null
		order by last_activity_at asc`)
	if err != nil {
		return nil, err
	}
	return collectRegistrations(rows)
}

// IncrementKicked bumps a member's kick counter.
func (s *Store) IncrementKicked(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx,
		`update members set kicked_count = kicked_count + 1 where user_id = $1`, userID)
	return err
}
