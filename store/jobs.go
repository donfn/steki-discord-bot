package store

import (
	"context"
	"time"
)

// JobDone reports whether the one-off job has finished.
func (s *Store) JobDone(ctx context.Context, name string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `select count(*) from one_off_jobs where name = ?1`, name).Scan(&n)
	return n > 0, err
}

// MarkJobDone records that the one-off job finished.
func (s *Store) MarkJobDone(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `insert into one_off_jobs (name, done_at) values (?1, ?2)
		on conflict (name) do nothing`, name, ms(time.Now()))
	return err
}
