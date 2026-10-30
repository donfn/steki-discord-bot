package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const regColumns = `id, user_id, guild_id, channel_id, category_id, step, email, code,
	code_expires_at, code_sent_count, failed_attempts, last_activity_at, nudged_at,
	created_at, completed_at, outcome`

func scanRegistration(row scanner) (*Registration, error) {
	var r Registration
	err := row.Scan(&r.ID, &r.UserID, &r.GuildID, &r.ChannelID, &r.CategoryID, &r.Step,
		&r.Email, &r.Code, nullTimeDest{&r.CodeExpiresAt}, &r.CodeSentCount, &r.FailedAttempts,
		timeDest{&r.LastActivityAt}, nullTimeDest{&r.NudgedAt}, timeDest{&r.CreatedAt},
		nullTimeDest{&r.CompletedAt}, &r.Outcome)
	if err != nil {
		return nil, noRows(err)
	}
	return &r, nil
}

func collectRegistrations(rows *sql.Rows) ([]*Registration, error) {
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
	_, err := s.db.ExecContext(ctx, `
		insert into members (user_id, username, first_seen_at, last_join_at)
		values (?1, ?2, ?3, ?3)
		on conflict (user_id) do update set
			username     = excluded.username,
			last_join_at = excluded.last_join_at,
			join_count   = members.join_count + 1,
			left_at      = null`,
		userID, username, ms(time.Now()))
	return err
}

// EnsureMember creates a bare member row if one does not exist, without
// touching join counters. Used by backfills and moderator commands.
func (s *Store) EnsureMember(ctx context.Context, userID, username string) error {
	now := ms(time.Now())
	_, err := s.db.ExecContext(ctx, `
		insert into members (user_id, username, first_seen_at, last_join_at)
		values (?1, ?2, ?3, ?3)
		on conflict (user_id) do nothing`,
		userID, username, now)
	return err
}

// MarkMemberLeft stamps left_at.
func (s *Store) MarkMemberLeft(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx,
		`update members set left_at = ?2 where user_id = ?1`, userID, ms(time.Now()))
	return err
}

// GetMember returns a member or ErrNotFound.
func (s *Store) GetMember(ctx context.Context, userID string) (*Member, error) {
	var m Member
	err := s.db.QueryRowContext(ctx, `
		select user_id, username, first_seen_at, last_join_at, join_count,
		       left_at, verified_at, verified_email, kicked_count
		from members where user_id = ?1`, userID).
		Scan(&m.UserID, &m.Username, timeDest{&m.FirstSeenAt}, timeDest{&m.LastJoinAt}, &m.JoinCount,
			nullTimeDest{&m.LeftAt}, nullTimeDest{&m.VerifiedAt}, &m.VerifiedEmail, &m.KickedCount)
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
	row := s.db.QueryRowContext(ctx, `
		insert into registrations (id, user_id, guild_id, last_activity_at, created_at)
		values (?1, ?2, ?3, ?4, ?4)
		on conflict (user_id) where outcome is null do nothing
		returning `+regColumns, newID(), userID, guildID, ms(time.Now()))

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
	return scanRegistration(s.db.QueryRowContext(ctx,
		`select `+regColumns+` from registrations
		 where user_id = ?1 and outcome is null`, userID))
}

// RegistrationByChannel looks up the open registration attached to a channel.
func (s *Store) RegistrationByChannel(ctx context.Context, channelID string) (*Registration, error) {
	return scanRegistration(s.db.QueryRowContext(ctx,
		`select `+regColumns+` from registrations
		 where channel_id = ?1 and outcome is null`, channelID))
}

// AttachChannel records the channel and category a registration was given.
func (s *Store) AttachChannel(ctx context.Context, regID, channelID, categoryID string) error {
	_, err := s.db.ExecContext(ctx, `
		update registrations
		set channel_id = ?2, category_id = nullif(?3, ''), last_activity_at = ?4
		where id = ?1`, regID, channelID, categoryID, ms(time.Now()))
	return err
}

// Touch refreshes last_activity_at and clears any nudge, so a member who comes
// back after the 1h nudge starts the idle clock over.
func (s *Store) Touch(ctx context.Context, regID string) error {
	_, err := s.db.ExecContext(ctx, `
		update registrations
		set last_activity_at = ?2, nudged_at = null
		where id = ?1`, regID, ms(time.Now()))
	return err
}

// BeginCodeStep stores the submitted address and the freshly generated code,
// and moves the registration to the awaiting_code step.
func (s *Store) BeginCodeStep(ctx context.Context, regID, email, code string, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		update registrations set
			step             = ?2,
			email            = ?3,
			code             = ?4,
			code_expires_at  = ?5,
			code_sent_count  = code_sent_count + 1,
			failed_attempts  = 0,
			last_activity_at = ?6,
			nudged_at        = null
		where id = ?1`,
		regID, StepAwaitingCode, email, code, ms(expiresAt), ms(time.Now()))
	return err
}

// IncrementFailedAttempts records a wrong code and returns the new count.
func (s *Store) IncrementFailedAttempts(ctx context.Context, regID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		update registrations
		set failed_attempts = failed_attempts + 1, last_activity_at = ?2, nudged_at = null
		where id = ?1
		returning failed_attempts`, regID, ms(time.Now())).Scan(&n)
	if err != nil {
		return 0, noRows(err)
	}
	return n, nil
}

// ResetToEmailStep destroys the stored code after too many wrong guesses and
// sends the member back to step one.
func (s *Store) ResetToEmailStep(ctx context.Context, regID string) error {
	_, err := s.db.ExecContext(ctx, `
		update registrations set
			code = null, code_expires_at = null,
			failed_attempts = 0, step = ?2, email = null,
			last_activity_at = ?3, nudged_at = null
		where id = ?1`, regID, StepAwaitingEmail, ms(time.Now()))
	return err
}

// CloseRegistration marks a registration terminal. Closing frees the partial
// unique index so the member may later open a new one.
func (s *Store) CloseRegistration(ctx context.Context, regID, outcome string) error {
	_, err := s.db.ExecContext(ctx, `
		update registrations
		set outcome = ?2, completed_at = ?3
		where id = ?1 and outcome is null`, regID, outcome, ms(time.Now()))
	return err
}

// CloseOpenForUser closes whatever registration a user currently has open.
func (s *Store) CloseOpenForUser(ctx context.Context, userID, outcome string) error {
	_, err := s.db.ExecContext(ctx, `
		update registrations
		set outcome = ?2, completed_at = ?3
		where user_id = ?1 and outcome is null`, userID, outcome, ms(time.Now()))
	return err
}

// ClearVerification forgets that a member verified, so that nothing, the
// reaper's role restore included, treats them as verified until they register
// again. With release, the addresses bound to them are freed as well, which
// lets the member register with a different one or someone else with theirs;
// without it, the member keeps their claim and can re-register with it. It
// returns how many addresses were released.
func (s *Store) ClearVerification(ctx context.Context, userID string, release bool) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		update members set verified_at = null, verified_email = null
		where user_id = ?1`, userID); err != nil {
		return 0, err
	}

	var released int64
	if release {
		for _, q := range []string{
			`delete from verified_emails where user_id = ?1`,
			`delete from legacy_email_hashes where user_id = ?1`,
		} {
			res, err := tx.ExecContext(ctx, q, userID)
			if err != nil {
				return 0, err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return 0, err
			}
			released += n
		}
	}

	if _, err := tx.ExecContext(ctx, `
		update registrations
		set outcome = ?2, completed_at = ?3
		where user_id = ?1 and outcome is null`, userID, OutcomeModerator, ms(time.Now())); err != nil {
		return 0, err
	}
	return released, tx.Commit()
}

// ForgetResult counts what ForgetMember removed or released.
type ForgetResult struct {
	Members        int64
	Registrations  int64
	Events         int64
	EmailsUnlinked int64
}

// Found reports whether the database held anything about the member.
func (r ForgetResult) Found() bool {
	return r.Members+r.Registrations+r.Events+r.EmailsUnlinked > 0
}

// ForgetMember erases everything the database holds about a member: the member
// row, their registrations, their audit trail, and their media-guard and
// help-channel state. The addresses bound to them stay recorded, because an
// address is spent for good, but lose their owner.
func (s *Store) ForgetMember(ctx context.Context, userID string) (ForgetResult, error) {
	var out ForgetResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()

	steps := []struct {
		query string
		count *int64
	}{
		{`update verified_emails set user_id = null where user_id = ?1`, &out.EmailsUnlinked},
		{`update legacy_email_hashes set user_id = null where user_id = ?1`, &out.EmailsUnlinked},
		{`delete from registration_events where user_id = ?1`, &out.Events},
		{`delete from help_access where user_id = ?1`, nil},
		{`delete from media_guard where user_id = ?1`, nil},
		{`delete from registrations where user_id = ?1`, &out.Registrations},
		{`delete from members where user_id = ?1`, &out.Members},
	}
	for _, step := range steps {
		res, err := tx.ExecContext(ctx, step.query, userID)
		if err != nil {
			return ForgetResult{}, err
		}
		if step.count != nil {
			n, err := res.RowsAffected()
			if err != nil {
				return ForgetResult{}, err
			}
			*step.count += n
		}
	}
	return out, tx.Commit()
}

// MarkNudged stamps the 1h in-channel nudge so it is sent only once.
func (s *Store) MarkNudged(ctx context.Context, regID string) error {
	_, err := s.db.ExecContext(ctx,
		`update registrations set nudged_at = ?2 where id = ?1`, regID, ms(time.Now()))
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
	now := ms(time.Now())

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var claimed string
	err = tx.QueryRowContext(ctx, `
		insert into verified_emails (email, user_id, domain, source, verified_at)
		values (?1, ?2, ?3, ?4, ?5)
		on conflict (email) do update set user_id = excluded.user_id
			where verified_emails.user_id is null
			   or verified_emails.user_id = excluded.user_id
		returning email`,
		email, userID, EmailDomain(email), SourceLive, now).Scan(&claimed)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrEmailTaken
	}
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		update members set verified_at = ?3, verified_email = ?2 where user_id = ?1`,
		userID, email, now); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `
		update registrations
		set outcome = ?2, step = ?3, completed_at = ?4
		where id = ?1 and outcome is null`,
		regID, OutcomeVerified, StepDone, now); err != nil {
		return err
	}

	return tx.Commit()
}

// ListNudgeCandidates returns open registrations idle longer than idle that
// have not yet been nudged.
func (s *Store) ListNudgeCandidates(ctx context.Context, idle time.Duration, limit int) ([]*Registration, error) {
	rows, err := s.db.QueryContext(ctx, `
		select `+regColumns+` from registrations
		where outcome is null
		  and nudged_at is null
		  and channel_id is not null
		  and last_activity_at <= ?1
		order by last_activity_at asc
		limit ?2`, ms(time.Now().Add(-idle)), limit)
	if err != nil {
		return nil, err
	}
	return collectRegistrations(rows)
}

// ListKickCandidates returns open registrations idle longer than idle.
func (s *Store) ListKickCandidates(ctx context.Context, idle time.Duration, limit int) ([]*Registration, error) {
	rows, err := s.db.QueryContext(ctx, `
		select `+regColumns+` from registrations
		where outcome is null
		  and last_activity_at <= ?1
		order by last_activity_at asc
		limit ?2`, ms(time.Now().Add(-idle)), limit)
	if err != nil {
		return nil, err
	}
	return collectRegistrations(rows)
}

// ListOpen returns every open registration, oldest activity first. Used
// by /reg reconcile and /reg stuck.
func (s *Store) ListOpen(ctx context.Context) ([]*Registration, error) {
	rows, err := s.db.QueryContext(ctx, `
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
	_, err := s.db.ExecContext(ctx,
		`update members set kicked_count = kicked_count + 1 where user_id = ?1`, userID)
	return err
}
