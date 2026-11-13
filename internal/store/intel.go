package store

import (
	"context"
	"time"
)

// Intel kinds.
const (
	IntelMedia = "media"
	IntelLink  = "link"
)

// IntelEntry is one image or link in the intelligence feed.
type IntelEntry struct {
	ID     int64
	Kind   string
	Key    string // the file's SHA-256, or the normalised link
	Label  string
	UserID string // the member it was first seen from
}

// AddIntel records an item and returns its ID. added is false when the item
// was already in the feed. quick is a media item's name and size, which finds
// it again without downloading anything.
func (s *Guard) AddIntel(ctx context.Context, kind, key, quick, label, userID string) (id int64, added bool, err error) {
	err = s.db.QueryRowContext(ctx, `
		insert into intel (kind, key, quick, label, user_id, added_at) values (?1, ?2, ?3, ?4, ?5, ?6)
		on conflict (kind, key) do nothing
		returning id`, kind, key, nullStr(quick), label, userID, ms(time.Now())).Scan(&id)
	if err == nil {
		return id, true, nil
	}
	if err = noRows(err); err != ErrNotFound {
		return 0, false, err
	}
	err = s.db.QueryRowContext(ctx, `select id from intel where kind = ?1 and key = ?2`, kind, key).Scan(&id)
	return id, false, err
}

// MatchIntel returns the feed's entry for an item, or ErrNotFound.
func (s *Guard) MatchIntel(ctx context.Context, kind, key string) (IntelEntry, error) {
	e := IntelEntry{Kind: kind}
	err := s.db.QueryRowContext(ctx, `
		select id, label, user_id from intel where kind = ?1 and key = ?2`, kind, key).
		Scan(&e.ID, &e.Label, &e.UserID)
	return e, noRows(err)
}

// IntelByQuick returns the media entries that may be a file of this name and
// size. The caller confirms a candidate by comparing the file's hash with Key.
func (s *Guard) IntelByQuick(ctx context.Context, quick string) ([]IntelEntry, error) {
	rows, err := s.db.QueryContext(ctx, `
		select id, key, label, user_id from intel where kind = 'media' and quick = ?1`, quick)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IntelEntry
	for rows.Next() {
		e := IntelEntry{Kind: IntelMedia}
		if err := rows.Scan(&e.ID, &e.Key, &e.Label, &e.UserID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountIntelHit notes that an entry was seen again.
func (s *Guard) CountIntelHit(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `
		update intel set hits = hits + 1, last_hit_at = ?2 where id = ?1`, id, ms(time.Now()))
	return err
}

// RemoveIntel deletes an entry and returns it, or ErrNotFound when it is
// already gone.
func (s *Guard) RemoveIntel(ctx context.Context, id int64) (IntelEntry, error) {
	var e IntelEntry
	err := s.db.QueryRowContext(ctx, `
		delete from intel where id = ?1 returning id, kind, label, user_id`, id).
		Scan(&e.ID, &e.Kind, &e.Label, &e.UserID)
	return e, noRows(err)
}
