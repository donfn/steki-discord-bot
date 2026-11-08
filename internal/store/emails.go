package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

// NormalizeEmail lowercases and trims an address so that dedup is not defeated
// by casing. All reads and writes of verified_emails go through this.
func NormalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// LegacyHash reproduces the unsalted SHA-256 the previous MongoDB
// implementation stored in `usedEmails`, so historical rows remain comparable.
func LegacyHash(email string) string {
	sum := sha256.Sum256([]byte(NormalizeEmail(email)))
	return hex.EncodeToString(sum[:])
}

// EmailDomain returns the part after the @, lowercased.
func EmailDomain(email string) string {
	normalized := NormalizeEmail(email)
	if i := strings.LastIndex(normalized, "@"); i >= 0 {
		return normalized[i+1:]
	}
	return ""
}

// IsLegacyHash reports whether s is a SHA-256 hex digest rather than an
// address, so commands can accept either where a moderator may only have the
// hash.
func IsLegacyHash(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// EmailUse describes how an address is already spoken for.
type EmailUse struct {
	InUse  bool
	UserID *string // owner, when known
	Legacy bool    // matched only an imported hash, so the owner may be unknown
}

// BlocksUser reports whether the address is unavailable to userID: it is in
// use and not already that member's own, as it is when a verified member
// leaves and rejoins.
func (u EmailUse) BlocksUser(userID string) bool {
	return u.InUse && (u.UserID == nil || *u.UserID != userID)
}

// CheckEmail reports whether an address has already been used to verify an
// account. It tries the plaintext table first, then falls back to the imported
// legacy hashes, so addresses registered before the Postgres migration still
// cannot be reused.
func (s *Registry) CheckEmail(ctx context.Context, email string) (EmailUse, error) {
	email = NormalizeEmail(email)

	var owner *string
	err := s.db.QueryRowContext(ctx,
		`select user_id from verified_emails where email = ?1`, email).Scan(&owner)
	if err == nil {
		return EmailUse{InUse: true, UserID: owner}, nil
	}
	if !errors.Is(noRows(err), ErrNotFound) {
		return EmailUse{}, err
	}

	err = s.db.QueryRowContext(ctx,
		`select user_id from legacy_email_hashes where sha256 = ?1`, LegacyHash(email)).Scan(&owner)
	if err == nil {
		return EmailUse{InUse: true, UserID: owner, Legacy: true}, nil
	}
	if !errors.Is(noRows(err), ErrNotFound) {
		return EmailUse{}, err
	}

	return EmailUse{}, nil
}

// AddVerifiedEmail records an address, optionally bound to an owner.
func (s *Registry) AddVerifiedEmail(ctx context.Context, email string, userID *string, source string) error {
	email = NormalizeEmail(email)
	_, err := s.db.ExecContext(ctx, `
		insert into verified_emails (email, user_id, domain, source, verified_at)
		values (?1, ?2, ?3, ?4, ?5)
		on conflict (email) do update set
			user_id = coalesce(verified_emails.user_id, excluded.user_id)`,
		email, userID, EmailDomain(email), source, ms(time.Now()))
	return err
}

// RemoveVerifiedEmail deletes an address and any legacy hash of it, so a
// moderator freeing an address frees it in both representations.
func (s *Registry) RemoveVerifiedEmail(ctx context.Context, email string) (bool, error) {
	email = NormalizeEmail(email)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `delete from verified_emails where email = ?1`, email)
	if err != nil {
		return false, err
	}
	removed, err := res.RowsAffected()
	if err != nil {
		return false, err
	}

	res, err = tx.ExecContext(ctx, `delete from legacy_email_hashes where sha256 = ?1`, LegacyHash(email))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	removed += n

	if err := tx.Commit(); err != nil {
		return false, err
	}
	return removed > 0, nil
}

// RemoveLegacyHash deletes a raw hash, for the rare case where only the hash is
// known. Mirrors the old `removeHash` admin command.
func (s *Registry) RemoveLegacyHash(ctx context.Context, hash string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`delete from legacy_email_hashes where sha256 = ?1`,
		strings.ToLower(strings.TrimSpace(hash)))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// WhoIs returns the stored record for an address.
func (s *Registry) WhoIs(ctx context.Context, email string) (*VerifiedEmail, error) {
	email = NormalizeEmail(email)
	v, err := scanVerifiedEmail(s.db.QueryRowContext(ctx, `
		select email, user_id, domain, source, verified_at
		from verified_emails where email = ?1`, email))
	if err != nil {
		return nil, noRows(err)
	}
	return v, nil
}

func scanVerifiedEmail(row scanner) (*VerifiedEmail, error) {
	var v VerifiedEmail
	if err := row.Scan(&v.Email, &v.UserID, &v.Domain, &v.Source, timeDest{&v.VerifiedAt}); err != nil {
		return nil, err
	}
	return &v, nil
}

// EmailsForUser lists every address bound to a Discord account.
func (s *Registry) EmailsForUser(ctx context.Context, userID string) ([]*VerifiedEmail, error) {
	rows, err := s.db.QueryContext(ctx, `
		select email, user_id, domain, source, verified_at
		from verified_emails where user_id = ?1 order by verified_at asc`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*VerifiedEmail
	for rows.Next() {
		v, err := scanVerifiedEmail(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// AddLegacyHash stores an imported hash that could not be resolved to plaintext.
func (s *Registry) AddLegacyHash(ctx context.Context, hash string, userID *string, source string) error {
	_, err := s.db.ExecContext(ctx, `
		insert into legacy_email_hashes (sha256, user_id, source, imported_at)
		values (?1, ?2, ?3, ?4)
		on conflict (sha256) do update set
			user_id = coalesce(legacy_email_hashes.user_id, excluded.user_id)`,
		strings.ToLower(strings.TrimSpace(hash)), userID, source, ms(time.Now()))
	return err
}

// GetLegacyHash returns the stored record for a hash.
func (s *Registry) GetLegacyHash(ctx context.Context, hash string) (*LegacyHashRecord, error) {
	var r LegacyHashRecord
	err := s.db.QueryRowContext(ctx, `
		select sha256, user_id, source, imported_at
		from legacy_email_hashes where sha256 = ?1`,
		strings.ToLower(strings.TrimSpace(hash))).
		Scan(&r.Hash, &r.UserID, &r.Source, timeDest{&r.ImportedAt})
	if err != nil {
		return nil, noRows(err)
	}
	return &r, nil
}

// LegacyHashesForUser lists every imported hash bound to a Discord account.
func (s *Registry) LegacyHashesForUser(ctx context.Context, userID string) ([]*LegacyHashRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
		select sha256, user_id, source, imported_at
		from legacy_email_hashes where user_id = ?1 order by imported_at asc`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*LegacyHashRecord
	for rows.Next() {
		var r LegacyHashRecord
		if err := rows.Scan(&r.Hash, &r.UserID, &r.Source, timeDest{&r.ImportedAt}); err != nil {
			return nil, err
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// SetLegacyHashOwner binds a hash to an account, or to nobody when owner is
// nil. It reports whether the hash exists.
func (s *Registry) SetLegacyHashOwner(ctx context.Context, hash string, owner *string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`update legacy_email_hashes set user_id = ?2 where sha256 = ?1`,
		strings.ToLower(strings.TrimSpace(hash)), owner)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// EmailEdit says what UpdateVerifiedEmail changes about a recorded address.
type EmailEdit struct {
	NewEmail string  // corrects the address itself; empty keeps it
	Owner    *string // binds the address to this account; nil leaves the owner alone
	Unlink   bool    // drops the owner; ignored when Owner is set
}

// UpdateVerifiedEmail corrects an address, changes who owns it, or both, and
// returns the record as it now stands. It returns ErrNotFound when the address
// is not recorded and ErrEmailTaken when NewEmail already is.
//
// A member who verified with this address keeps it only while they remain the
// owner: when the owner changes, or the address is corrected, their link to it
// follows or is dropped, so no member points at an address another account owns.
func (s *Registry) UpdateVerifiedEmail(ctx context.Context, email string, edit EmailEdit) (*VerifiedEmail, error) {
	email = NormalizeEmail(email)
	target := email
	if edit.NewEmail != "" {
		target = NormalizeEmail(edit.NewEmail)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// members.verified_email references the address being renamed; check the
	// reference once the whole transaction is consistent, not per statement.
	if _, err := tx.ExecContext(ctx, `pragma defer_foreign_keys = on`); err != nil {
		return nil, err
	}

	cur, err := scanVerifiedEmail(tx.QueryRowContext(ctx, `
		select email, user_id, domain, source, verified_at
		from verified_emails where email = ?1`, email))
	if err != nil {
		return nil, noRows(err)
	}

	if target != email {
		var taken bool
		if err := tx.QueryRowContext(ctx,
			`select exists (select 1 from verified_emails where email = ?1)`, target).Scan(&taken); err != nil {
			return nil, err
		}
		if taken {
			return nil, ErrEmailTaken
		}
	}

	owner := cur.UserID
	switch {
	case edit.Owner != nil:
		owner = edit.Owner
	case edit.Unlink:
		owner = nil
	}

	if _, err := tx.ExecContext(ctx, `
		update verified_emails set email = ?2, user_id = ?3, domain = ?4
		where email = ?1`, email, target, owner, EmailDomain(target)); err != nil {
		return nil, err
	}
	if target != email {
		if _, err := tx.ExecContext(ctx,
			`update members set verified_email = ?2 where verified_email = ?1`, email, target); err != nil {
			return nil, err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		update members set verified_email = null
		where verified_email = ?1 and user_id is not ?2`, target, owner); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	cur.Email, cur.UserID, cur.Domain = target, owner, EmailDomain(target)
	return cur, nil
}

// EmailStats summarises the two email tables for /bot stats.
type EmailStats struct {
	Verified       int
	VerifiedLinked int
	LegacyHashes   int
}

// EmailStats counts stored addresses.
func (s *Registry) EmailStats(ctx context.Context) (EmailStats, error) {
	var st EmailStats
	err := s.db.QueryRowContext(ctx, `
		select
			(select count(*) from verified_emails),
			(select count(*) from verified_emails where user_id is not null),
			(select count(*) from legacy_email_hashes)`).
		Scan(&st.Verified, &st.VerifiedLinked, &st.LegacyHashes)
	return st, err
}
