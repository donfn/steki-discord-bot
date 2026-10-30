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
func (s *Store) SyncMemberActivity(ctx context.Context, listedAt time.Time, entries []ActivityEntry) error {
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
func (s *Store) MemberActivities(ctx context.Context) ([]MemberActivity, error) {
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

// SetGhost records whether a member now holds the ghost role.
func (s *Store) SetGhost(ctx context.Context, userID string, ghost bool) error {
	_, err := s.db.ExecContext(ctx,
		`update member_activity set ghost = ?2 where user_id = ?1`, userID, ghost)
	return err
}
