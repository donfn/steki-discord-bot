package store

import (
	"context"
	"time"
)

// ActivityEntry is what a full member list tells the index about one member.
// LastMessageAt is nil when no message was seen for them.
type ActivityEntry struct {
	UserID        string
	JoinedAt      time.Time
	LastMessageAt *time.Time
}

// MemberActivity is one row of the activity index.
type MemberActivity struct {
	UserID        string
	JoinedAt      time.Time
	LastMessageAt *time.Time
	Ghost         bool
}

// SyncMemberActivity makes the index match a full member list: it adds the
// members it lacks, refreshes join times, keeps the later of the stored and
// the given last message, and removes members the list no longer holds.
//
// listedAt is the moment before the list was fetched. A row written after it
// (a join or a message that arrived while the list was loading) is kept even
// though the list cannot name it.
func (s *Activity) SyncMemberActivity(ctx context.Context, listedAt time.Time, entries []ActivityEntry) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		insert into member_activity (user_id, joined_at, last_message_at, synced_at)
		values (?1, ?2, ?3, ?4)
		on conflict (user_id) do update set
			joined_at       = excluded.joined_at,
			last_message_at = coalesce(max(member_activity.last_message_at, excluded.last_message_at),
			                           member_activity.last_message_at, excluded.last_message_at),
			synced_at       = excluded.synced_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := ms(time.Now())
	for _, e := range entries {
		if _, err := stmt.ExecContext(ctx, e.UserID, ms(e.JoinedAt), msPtr(e.LastMessageAt), now); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `delete from member_activity where synced_at < ?1`, ms(listedAt)); err != nil {
		return err
	}
	return tx.Commit()
}

// MemberActivities returns the whole index.
func (s *Activity) MemberActivities(ctx context.Context) ([]MemberActivity, error) {
	rows, err := s.db.QueryContext(ctx,
		`select user_id, joined_at, last_message_at, ghost from member_activity order by user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemberActivity
	for rows.Next() {
		var a MemberActivity
		if err := rows.Scan(&a.UserID, timeDest{&a.JoinedAt}, nullTimeDest{&a.LastMessageAt}, &a.Ghost); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// RecordMessage notes that a member just sent a message. A member the index
// does not know yet is added, with joinedAt as the start of their clock. The
// ghost flag is cleared unless keepGhost says they still hold the role, which
// the caller removes before clearing it with SetGhost.
func (s *Activity) RecordMessage(ctx context.Context, userID string, at, joinedAt time.Time, keepGhost bool) error {
	_, err := s.db.ExecContext(ctx, `
		insert into member_activity (user_id, joined_at, last_message_at, synced_at)
		values (?1, ?2, ?3, ?3)
		on conflict (user_id) do update set
			last_message_at = coalesce(max(member_activity.last_message_at, excluded.last_message_at),
			                           excluded.last_message_at),
			ghost           = case when ?4 then member_activity.ghost else 0 end`,
		userID, ms(joinedAt), ms(at), keepGhost)
	return err
}

// RecordJoin adds a member who just joined, or restarts the clock of one who
// came back. Leaving removed their role, so the flag starts clear.
func (s *Activity) RecordJoin(ctx context.Context, userID string, joinedAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		insert into member_activity (user_id, joined_at, synced_at) values (?1, ?2, ?3)
		on conflict (user_id) do update set
			joined_at = excluded.joined_at,
			ghost     = 0,
			synced_at = excluded.synced_at`,
		userID, ms(joinedAt), ms(time.Now()))
	return err
}

// DeleteMemberActivity forgets a member who left.
func (s *Activity) DeleteMemberActivity(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `delete from member_activity where user_id = ?1`, userID)
	return err
}

// GhostCandidates lists up to limit members who are not flagged as ghosts yet
// but whose last message, or failing that their join, is older than cutoff.
func (s *Activity) GhostCandidates(ctx context.Context, cutoff time.Time, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		select user_id from member_activity
		where ghost = 0 and max(joined_at, coalesce(last_message_at, 0)) < ?1
		order by max(joined_at, coalesce(last_message_at, 0))
		limit ?2`, ms(cutoff), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetGhost records whether a member now holds the ghost role.
func (s *Activity) SetGhost(ctx context.Context, userID string, ghost bool) error {
	_, err := s.db.ExecContext(ctx,
		`update member_activity set ghost = ?2 where user_id = ?1`, userID, ghost)
	return err
}
