package store

import (
	"context"
	"time"
)

// LogEvent appends to the audit trail. regID and actorID may be empty.
//
// Audit writes must never break the flow they describe, so callers generally
// log the returned error and carry on rather than aborting.
func (s *Store) LogEvent(ctx context.Context, userID, regID, kind string, detail map[string]any, actorID string) error {
	if detail == nil {
		detail = map[string]any{}
	}
	_, err := s.pool.Exec(ctx, `
		insert into registration_events (user_id, reg_id, kind, detail, actor_id)
		values ($1, nullif($2, '')::uuid, $3, $4, nullif($5, ''))`,
		userID, regID, kind, detail, actorID)
	return err
}

// History returns a member's audit trail, newest first.
func (s *Store) History(ctx context.Context, userID string, limit int) ([]*Event, error) {
	rows, err := s.pool.Query(ctx, `
		select id, user_id, reg_id::text, kind, detail, actor_id, created_at
		from registration_events
		where user_id = $1
		order by created_at desc, id desc
		limit $2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.UserID, &e.RegID, &e.Kind, &e.Detail,
			&e.ActorID, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// CountEventsByKind counts events of each kind since a point in time, for the
// stats command.
func (s *Store) CountEventsByKind(ctx context.Context, since time.Time) (map[string]int, error) {
	rows, err := s.pool.Query(ctx, `
		select kind, count(*) from registration_events
		where created_at >= $1
		group by kind`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, err
		}
		out[kind] = n
	}
	return out, rows.Err()
}
