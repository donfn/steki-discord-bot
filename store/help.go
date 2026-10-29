package store

import (
	"context"
	"time"
)

// RecordHelpAccess notes that the bot let a member into the help channel.
func (s *Store) RecordHelpAccess(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `
		insert into help_access (user_id, granted_at) values (?1, ?2)
		on conflict (user_id) do update set granted_at = excluded.granted_at`,
		userID, ms(time.Now()))
	return err
}

// HasHelpAccess reports whether the bot let a member into the help channel.
func (s *Store) HasHelpAccess(ctx context.Context, userID string) (bool, error) {
	var ok bool
	err := s.db.QueryRowContext(ctx,
		`select exists (select 1 from help_access where user_id = ?1)`, userID).Scan(&ok)
	return ok, err
}

// ForgetHelpAccess drops a member's grant once their overwrite is removed.
func (s *Store) ForgetHelpAccess(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `delete from help_access where user_id = ?1`, userID)
	return err
}

// StaleHelpAccess lists members with help access but no open registration,
// whose access should already have been removed.
func (s *Store) StaleHelpAccess(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		select h.user_id from help_access h
		where not exists (
			select 1 from registrations r
			where r.user_id = h.user_id and r.outcome is null)`)
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
