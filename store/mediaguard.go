package store

import (
	"context"
	"fmt"
	"time"
)

// GuardDM locates the direct message that carried a verification link, so its
// button can be disabled once the link is used. Both fields may be empty when
// the member's DMs were closed.
type GuardDM struct {
	ChannelID string
	MessageID string
}

// FlagTrips reports whether a flagged post at now trips the guard, given the
// start of the member's open window. It is the whole counting rule: the second
// flagged post inside window trips it.
func FlagTrips(windowStart *time.Time, now time.Time, window time.Duration) bool {
	return windowStart != nil && now.Sub(*windowStart) < window
}

// RecordFlaggedPost counts a post carrying an attachment or a link. When it is
// the member's second such post within window it closes the window, records
// tokenID as the outstanding verification link and returns true. A member
// still inside a post-captcha exemption is never tripped.
func (s *Guard) RecordFlaggedPost(ctx context.Context, userID, guildID, tokenID string, now time.Time, window time.Duration) (bool, error) {
	// Transactions begin IMMEDIATE, so this read-then-write cannot interleave
	// with another writer: the lock a SELECT ... FOR UPDATE would take is held
	// from the start.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		insert into media_guard (user_id, guild_id, updated_at) values (?1, ?2, ?3)
		on conflict (user_id) do nothing`, userID, guildID, ms(time.Now())); err != nil {
		return false, fmt.Errorf("insert media_guard: %w", err)
	}

	var windowStart, exemptUntil *time.Time
	if err := tx.QueryRowContext(ctx, `
		select window_start, exempt_until from media_guard
		where user_id = ?1`, userID).
		Scan(nullTimeDest{&windowStart}, nullTimeDest{&exemptUntil}); err != nil {
		return false, err
	}

	if exemptUntil != nil && now.Before(*exemptUntil) {
		return false, tx.Commit()
	}

	tripped := FlagTrips(windowStart, now, window)
	if tripped {
		_, err = tx.ExecContext(ctx, `
			update media_guard set
				guild_id      = ?2,
				window_start  = null,
				token_id      = ?3,
				restricted_at = ?4,
				dm_channel_id = null,
				dm_message_id = null,
				updated_at    = ?5
			where user_id = ?1`, userID, guildID, tokenID, ms(now), ms(time.Now()))
	} else {
		_, err = tx.ExecContext(ctx, `
			update media_guard set guild_id = ?2, window_start = ?3, updated_at = ?4
			where user_id = ?1`, userID, guildID, ms(now), ms(time.Now()))
	}
	if err != nil {
		return false, err
	}
	return tripped, tx.Commit()
}

// SetGuardDM remembers which DM carries the member's verification link.
func (s *Guard) SetGuardDM(ctx context.Context, userID, tokenID string, dm GuardDM) error {
	_, err := s.db.ExecContext(ctx, `
		update media_guard set dm_channel_id = ?3, dm_message_id = ?4, updated_at = ?5
		where user_id = ?1 and token_id = ?2`,
		userID, tokenID, dm.ChannelID, dm.MessageID, ms(time.Now()))
	return err
}

// GuardTokenActive reports whether tokenID is the member's outstanding,
// unused verification link.
func (s *Guard) GuardTokenActive(ctx context.Context, userID, tokenID string) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx, `
		select exists (select 1 from media_guard where user_id = ?1 and token_id = ?2)`,
		userID, tokenID).Scan(&ok)
	return ok, err
}

// ConsumeGuardToken marks tokenID used and exempts the member until
// exemptUntil. It returns ErrNotFound when the link was already used or has
// been superseded by a newer one, which makes every link single-use.
func (s *Guard) ConsumeGuardToken(ctx context.Context, userID, tokenID string, exemptUntil time.Time) (GuardDM, error) {
	var dm GuardDM
	var ch, msg *string
	err := s.db.QueryRowContext(ctx, `
		update media_guard set
			token_id      = null,
			restricted_at = null,
			exempt_until  = ?3,
			updated_at    = ?4
		where user_id = ?1 and token_id = ?2
		returning dm_channel_id, dm_message_id`,
		userID, tokenID, ms(exemptUntil), ms(time.Now())).Scan(&ch, &msg)
	if err != nil {
		return dm, noRows(err)
	}
	if ch != nil {
		dm.ChannelID = *ch
	}
	if msg != nil {
		dm.MessageID = *msg
	}
	return dm, nil
}

// ForceRestriction records an outstanding verification link without counting
// a post, for detections strong enough to restrict on their own (the same
// message across many channels). It ignores any post-captcha exemption.
func (s *Guard) ForceRestriction(ctx context.Context, userID, guildID, tokenID string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		insert into media_guard (user_id, guild_id, token_id, restricted_at, updated_at)
		values (?1, ?2, ?3, ?4, ?5)
		on conflict (user_id) do update set
			guild_id      = excluded.guild_id,
			window_start  = null,
			token_id      = excluded.token_id,
			restricted_at = excluded.restricted_at,
			dm_channel_id = null,
			dm_message_id = null,
			updated_at    = excluded.updated_at`,
		userID, guildID, tokenID, ms(now), ms(time.Now()))
	return err
}

// GuardState is a member's media guard row, for the /guard status command.
type GuardState struct {
	WindowStart  *time.Time
	ExemptUntil  *time.Time
	RestrictedAt *time.Time
	LinkPending  bool
}

// GuardStatus returns a member's media guard state, or ErrNotFound when they
// have never posted an attachment or a link.
func (s *Guard) GuardStatus(ctx context.Context, userID string) (*GuardState, error) {
	var g GuardState
	err := s.db.QueryRowContext(ctx, `
		select window_start, exempt_until, restricted_at, token_id is not null
		from media_guard where user_id = ?1`, userID).
		Scan(nullTimeDest{&g.WindowStart}, nullTimeDest{&g.ExemptUntil},
			nullTimeDest{&g.RestrictedAt}, &g.LinkPending)
	if err != nil {
		return nil, noRows(err)
	}
	return &g, nil
}

// ClearExemption ends a member's post-captcha grace period early, reporting
// whether one was running.
func (s *Guard) ClearExemption(ctx context.Context, userID string) (bool, error) {
	now := ms(time.Now())
	res, err := s.db.ExecContext(ctx, `
		update media_guard set exempt_until = null, updated_at = ?2
		where user_id = ?1 and exempt_until > ?2`, userID, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ForgetMember erases a member's guard state and audit trail, for /reg forget.
func (s *Guard) ForgetMember(ctx context.Context, userID string) error {
	if _, err := s.db.ExecContext(ctx, `delete from media_guard where user_id = ?1`, userID); err != nil {
		return err
	}
	_, err := s.ForgetEvents(ctx, userID)
	return err
}
