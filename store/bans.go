package store

import (
	"context"
	"strings"
	"time"
)

// BanResult says which addresses a ban closed.
type BanResult struct {
	Emails []string // addresses known in plaintext
	Hashes int      // addresses known only as an imported hash
}

// BanEmails bars every address tied to a member from registering again: the
// ones recorded as verified, the imported hashes bound to them, and extra, an
// address a moderator knows about that the bot does not. It must run before
// the member's data is erased, since that unlinks their addresses.
func (s *Registry) BanEmails(ctx context.Context, userID, extra, reason, by string) (BanResult, error) {
	var res BanResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer func() { _ = tx.Rollback() }()

	now := ms(time.Now())
	ban := func(hash string, email *string) error {
		_, err := tx.ExecContext(ctx, `
			insert into banned_emails (hash, email, user_id, reason, banned_by, banned_at)
			values (?1, ?2, ?3, ?4, ?5, ?6)
			on conflict (hash) do update set
				email = coalesce(banned_emails.email, excluded.email),
				user_id = coalesce(banned_emails.user_id, excluded.user_id)`,
			hash, email, nullIfEmpty(userID), nullIfEmpty(reason), nullIfEmpty(by), now)
		return err
	}

	rows, err := tx.QueryContext(ctx, `select email from verified_emails where user_id = ?1`, userID)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			rows.Close()
			return res, err
		}
		res.Emails = append(res.Emails, e)
	}
	if err := rows.Close(); err != nil {
		return res, err
	}
	if extra = NormalizeEmail(extra); extra != "" {
		res.Emails = append(res.Emails, extra)
	}
	for _, e := range res.Emails {
		e := e
		if err := ban(LegacyHash(e), &e); err != nil {
			return res, err
		}
	}

	hashes, err := tx.QueryContext(ctx, `select sha256 from legacy_email_hashes where user_id = ?1`, userID)
	if err != nil {
		return res, err
	}
	var legacy []string
	for hashes.Next() {
		var h string
		if err := hashes.Scan(&h); err != nil {
			hashes.Close()
			return res, err
		}
		legacy = append(legacy, h)
	}
	if err := hashes.Close(); err != nil {
		return res, err
	}
	for _, h := range legacy {
		// A hash whose plaintext is also known was banned above.
		var known bool
		if err := tx.QueryRowContext(ctx,
			`select exists (select 1 from banned_emails where hash = ?1 and email is not null)`, h).Scan(&known); err != nil {
			return res, err
		}
		if err := ban(h, nil); err != nil {
			return res, err
		}
		if !known {
			res.Hashes++
		}
	}
	return res, tx.Commit()
}

// IsEmailBanned reports whether an address may not register.
func (s *Registry) IsEmailBanned(ctx context.Context, email string) (bool, error) {
	var banned bool
	err := s.db.QueryRowContext(ctx,
		`select exists (select 1 from banned_emails where hash = ?1)`, LegacyHash(email)).Scan(&banned)
	return banned, err
}

// UnbanEmails lifts the ban on every address closed with a member's ban, and
// on email as well when one is given. It returns how many it lifted.
func (s *Registry) UnbanEmails(ctx context.Context, userID, email string) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	total := 0
	lift := func(query, arg string) error {
		res, err := tx.ExecContext(ctx, query, arg)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		total += int(n)
		return err
	}
	if err := lift(`delete from banned_emails where user_id = ?1`, userID); err != nil {
		return 0, err
	}
	if email = strings.TrimSpace(email); email != "" {
		if err := lift(`delete from banned_emails where hash = ?1`, LegacyHash(email)); err != nil {
			return 0, err
		}
	}
	return total, tx.Commit()
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
