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
func (s *Store) RecordFlaggedPost(ctx context.Context, userID, guildID, tokenID string, now time.Time, window time.Duration) (bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
		insert into media_guard (user_id, guild_id) values ($1, $2)
		on conflict (user_id) do nothing`, userID, guildID); err != nil {
		return false, fmt.Errorf("insert media_guard: %w", err)
	}

	var windowStart, exemptUntil *time.Time
	if err := tx.QueryRow(ctx, `
		select window_start, exempt_until from media_guard
		where user_id = $1 for update`, userID).Scan(&windowStart, &exemptUntil); err != nil {
		return false, err
	}

	if exemptUntil != nil && now.Before(*exemptUntil) {
		return false, tx.Commit(ctx)
	}

	tripped := FlagTrips(windowStart, now, window)
	if tripped {
		_, err = tx.Exec(ctx, `
			update media_guard set
				guild_id      = $2,
				window_start  = null,
				token_id      = $3,
				restricted_at = $4,
				dm_channel_id = null,
				dm_message_id = null,
				updated_at    = now()
			where user_id = $1`, userID, guildID, tokenID, now)
	} else {
		_, err = tx.Exec(ctx, `
			update media_guard set guild_id = $2, window_start = $3, updated_at = now()
			where user_id = $1`, userID, guildID, now)
	}
	if err != nil {
		return false, err
	}
	return tripped, tx.Commit(ctx)
}

// SetGuardDM remembers which DM carries the member's verification link.
func (s *Store) SetGuardDM(ctx context.Context, userID, tokenID string, dm GuardDM) error {
	_, err := s.pool.Exec(ctx, `
		update media_guard set dm_channel_id = $3, dm_message_id = $4, updated_at = now()
		where user_id = $1 and token_id = $2`, userID, tokenID, dm.ChannelID, dm.MessageID)
	return err
}

// GuardTokenActive reports whether tokenID is the member's outstanding,
// unused verification link.
func (s *Store) GuardTokenActive(ctx context.Context, userID, tokenID string) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `
		select exists (select 1 from media_guard where user_id = $1 and token_id = $2)`,
		userID, tokenID).Scan(&ok)
	return ok, err
}

// ConsumeGuardToken marks tokenID used and exempts the member until
// exemptUntil. It returns ErrNotFound when the link was already used or has
// been superseded by a newer one, which makes every link single-use.
func (s *Store) ConsumeGuardToken(ctx context.Context, userID, tokenID string, exemptUntil time.Time) (GuardDM, error) {
	var dm GuardDM
	var ch, msg *string
	err := s.pool.QueryRow(ctx, `
		update media_guard set
			token_id      = null,
			restricted_at = null,
			exempt_until  = $3,
			updated_at    = now()
		where user_id = $1 and token_id = $2
		returning dm_channel_id, dm_message_id`, userID, tokenID, exemptUntil).Scan(&ch, &msg)
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
func (s *Store) ForceRestriction(ctx context.Context, userID, guildID, tokenID string, now time.Time) error {
	_, err := s.pool.Exec(ctx, `
		insert into media_guard (user_id, guild_id, token_id, restricted_at)
		values ($1, $2, $3, $4)
		on conflict (user_id) do update set
			guild_id      = excluded.guild_id,
			window_start  = null,
			token_id      = excluded.token_id,
			restricted_at = excluded.restricted_at,
			dm_channel_id = null,
			dm_message_id = null,
			updated_at    = now()`, userID, guildID, tokenID, now)
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
func (s *Store) GuardStatus(ctx context.Context, userID string) (*GuardState, error) {
	var g GuardState
	err := s.pool.QueryRow(ctx, `
		select window_start, exempt_until, restricted_at, token_id is not null
		from media_guard where user_id = $1`, userID).
		Scan(&g.WindowStart, &g.ExemptUntil, &g.RestrictedAt, &g.LinkPending)
	if err != nil {
		return nil, noRows(err)
	}
	return &g, nil
}

// ClearExemption ends a member's post-captcha grace period early, reporting
// whether one was running.
func (s *Store) ClearExemption(ctx context.Context, userID string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `
		update media_guard set exempt_until = null, updated_at = now()
		where user_id = $1 and exempt_until > now()`, userID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
