package store

import (
	"context"
	"encoding/json"
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
	body, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		insert into registration_events (user_id, reg_id, kind, detail, actor_id, created_at)
		values (?1, ?2, ?3, ?4, ?5, ?6)`,
		userID, nullStr(regID), kind, string(body), nullStr(actorID), ms(time.Now()))
	return err
}

// History returns a member's audit trail, newest first.
func (s *Store) History(ctx context.Context, userID string, limit int) ([]*Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		select id, user_id, reg_id, kind, detail, actor_id, created_at
		from registration_events
		where user_id = ?1
		order by created_at desc, id desc
		limit ?2`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*Event
	for rows.Next() {
		var e Event
		var detail string
		if err := rows.Scan(&e.ID, &e.UserID, &e.RegID, &e.Kind, &detail,
			&e.ActorID, timeDest{&e.CreatedAt}); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(detail), &e.Detail); err != nil {
			e.Detail = map[string]any{"raw": detail}
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// CountEventsByKind counts events of each kind since a point in time, for the
// stats command.
func (s *Store) CountEventsByKind(ctx context.Context, since time.Time) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `
		select kind, count(*) from registration_events
		where created_at >= ?1
		group by kind`, ms(since))
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
