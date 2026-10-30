package store

import (
	"context"
	"time"
)

// Kinds of IP block.
const (
	IPChallenge = "challenge"
	IPBanned    = "ban"
)

// ipBanHistory is how long a block is kept after it ended, so moderators can
// see an address's past.
const ipBanHistory = 90 * 24 * time.Hour

// IPBan is one block the HTTP shield put on an address.
type IPBan struct {
	IP        string
	Kind      string // IPChallenge or IPBanned
	Reason    string
	StartedAt time.Time
	Until     time.Time
}

// RecordIPBan stores a block and prunes the ones that ended long ago.
func (s *Store) RecordIPBan(ctx context.Context, b IPBan) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		insert into ip_bans (ip, kind, reason, started_at, until)
		values (?1, ?2, ?3, ?4, ?5)`,
		b.IP, b.Kind, b.Reason, ms(b.StartedAt), ms(b.Until)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `delete from ip_bans where until < ?1`,
		ms(b.StartedAt.Add(-ipBanHistory))); err != nil {
		return err
	}
	return tx.Commit()
}

// ActiveIPBans returns the blocks still running at now, oldest first, so a
// later block of an address replaces an earlier one when they are replayed.
func (s *Store) ActiveIPBans(ctx context.Context, now time.Time) ([]IPBan, error) {
	rows, err := s.db.QueryContext(ctx, `
		select ip, kind, reason, started_at, until from ip_bans
		where until > ?1 order by started_at, id`, ms(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IPBan
	for rows.Next() {
		var b IPBan
		if err := rows.Scan(&b.IP, &b.Kind, &b.Reason, timeDest{&b.StartedAt}, timeDest{&b.Until}); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
