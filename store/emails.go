package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
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
func (s *Store) CheckEmail(ctx context.Context, email string) (EmailUse, error) {
	email = NormalizeEmail(email)

	var owner *string
	err := s.pool.QueryRow(ctx,
		`select user_id from verified_emails where email = $1`, email).Scan(&owner)
	if err == nil {
		return EmailUse{InUse: true, UserID: owner}, nil
	}
	if !errors.Is(noRows(err), ErrNotFound) {
		return EmailUse{}, err
	}

	err = s.pool.QueryRow(ctx,
		`select user_id from legacy_email_hashes where sha256 = $1`, LegacyHash(email)).Scan(&owner)
	if err == nil {
		return EmailUse{InUse: true, UserID: owner, Legacy: true}, nil
	}
	if !errors.Is(noRows(err), ErrNotFound) {
		return EmailUse{}, err
	}

	return EmailUse{}, nil
}

// AddVerifiedEmail records an address, optionally bound to an owner.
func (s *Store) AddVerifiedEmail(ctx context.Context, email string, userID *string, source string) error {
	email = NormalizeEmail(email)
	_, err := s.pool.Exec(ctx, `
		insert into verified_emails (email, user_id, domain, source)
		values ($1, $2, $3, $4)
		on conflict (email) do update set
			user_id = coalesce(verified_emails.user_id, excluded.user_id)`,
		email, userID, EmailDomain(email), source)
	return err
}

// RemoveVerifiedEmail deletes an address and any legacy hash of it, so a
// moderator freeing an address frees it in both representations.
func (s *Store) RemoveVerifiedEmail(ctx context.Context, email string) (bool, error) {
	email = NormalizeEmail(email)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `delete from verified_emails where email = $1`, email)
	if err != nil {
		return false, err
	}
	removed := tag.RowsAffected()

	tag, err = tx.Exec(ctx, `delete from legacy_email_hashes where sha256 = $1`, LegacyHash(email))
	if err != nil {
		return false, err
	}
	removed += tag.RowsAffected()

	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	return removed > 0, nil
}

// RemoveLegacyHash deletes a raw hash, for the rare case where only the hash is
// known. Mirrors the old `removeHash` admin command.
func (s *Store) RemoveLegacyHash(ctx context.Context, hash string) (bool, error) {
	tag, err := s.pool.Exec(ctx,
		`delete from legacy_email_hashes where sha256 = $1`,
		strings.ToLower(strings.TrimSpace(hash)))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// WhoIs returns the stored record for an address.
func (s *Store) WhoIs(ctx context.Context, email string) (*VerifiedEmail, error) {
	email = NormalizeEmail(email)
	var v VerifiedEmail
	err := s.pool.QueryRow(ctx, `
		select email, user_id, domain, source, verified_at
		from verified_emails where email = $1`, email).
		Scan(&v.Email, &v.UserID, &v.Domain, &v.Source, &v.VerifiedAt)
	if err != nil {
		return nil, noRows(err)
	}
	return &v, nil
}

// EmailsForUser lists every address bound to a Discord account.
func (s *Store) EmailsForUser(ctx context.Context, userID string) ([]*VerifiedEmail, error) {
	rows, err := s.pool.Query(ctx, `
		select email, user_id, domain, source, verified_at
		from verified_emails where user_id = $1 order by verified_at asc`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*VerifiedEmail
	for rows.Next() {
		var v VerifiedEmail
		if err := rows.Scan(&v.Email, &v.UserID, &v.Domain, &v.Source, &v.VerifiedAt); err != nil {
			return nil, err
		}
		out = append(out, &v)
	}
	return out, rows.Err()
}

// AddLegacyHash stores an imported hash that could not be resolved to plaintext.
func (s *Store) AddLegacyHash(ctx context.Context, hash string, userID *string, source string) error {
	_, err := s.pool.Exec(ctx, `
		insert into legacy_email_hashes (sha256, user_id, source)
		values ($1, $2, $3)
		on conflict (sha256) do update set
			user_id = coalesce(legacy_email_hashes.user_id, excluded.user_id)`,
		strings.ToLower(strings.TrimSpace(hash)), userID, source)
	return err
}

// EmailStats summarises the two email tables for /bot stats.
type EmailStats struct {
	Verified       int
	VerifiedLinked int
	LegacyHashes   int
}

// EmailStats counts stored addresses.
func (s *Store) EmailStats(ctx context.Context) (EmailStats, error) {
	var st EmailStats
	err := s.pool.QueryRow(ctx, `
		select
			(select count(*) from verified_emails),
			(select count(*) from verified_emails where user_id is not null),
			(select count(*) from legacy_email_hashes)`).
		Scan(&st.Verified, &st.VerifiedLinked, &st.LegacyHashes)
	return st, err
}
