package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Kinds of log evidence; see migrations/0001_init.sql.
const (
	EvidenceSubmitted = "submitted"
	EvidenceInUse     = "in_use"
)

// evidenceChunk bounds how many hashes one query binds.
const evidenceChunk = 500

// LogEvidence is one log line that ties an email hash to a member (a
// submission) or to a plaintext address (an "already in use" notice).
type LogEvidence struct {
	MessageID string
	Hash      string
	Kind      string
	UserID    string // the submitting member, for EvidenceSubmitted
	Email     string // the refused address, for EvidenceInUse when it hashes to Hash
	PostedAt  time.Time
}

// LogScan is how far the scan of one channel has got.
type LogScan struct {
	ChannelID string
	OldestID  string
	NewestID  string
	Complete  bool
	Scanned   int64
}

// LogScanState returns the scan state of a channel; a channel never scanned
// has a zero state.
func (s *Store) LogScanState(ctx context.Context, channelID string) (LogScan, error) {
	st := LogScan{ChannelID: channelID}
	var oldest, newest *string
	err := s.db.QueryRowContext(ctx, `
		select oldest_id, newest_id, complete, scanned
		from log_scan_state where channel_id = ?1`, channelID).
		Scan(&oldest, &newest, &st.Complete, &st.Scanned)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if oldest != nil {
		st.OldestID = *oldest
	}
	if newest != nil {
		st.NewestID = *newest
	}
	return st, err
}

// SaveLogPage stores one page of evidence together with the scan position
// after it, so a scan interrupted at any point resumes exactly where the last
// saved page ended.
func (s *Store) SaveLogPage(ctx context.Context, st LogScan, evidence []LogEvidence) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, e := range evidence {
		if _, err := tx.ExecContext(ctx, `
			insert into legacy_log_evidence (message_id, sha256, kind, user_id, email, posted_at)
			values (?1, ?2, ?3, nullif(?4, ''), nullif(?5, ''), ?6)
			on conflict do nothing`,
			e.MessageID, e.Hash, e.Kind, e.UserID, e.Email, ms(e.PostedAt)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		insert into log_scan_state (channel_id, oldest_id, newest_id, complete, scanned, updated_at)
		values (?1, nullif(?2, ''), nullif(?3, ''), ?4, ?5, ?6)
		on conflict (channel_id) do update set
			oldest_id = excluded.oldest_id, newest_id = excluded.newest_id,
			complete = excluded.complete, scanned = excluded.scanned,
			updated_at = excluded.updated_at`,
		st.ChannelID, st.OldestID, st.NewestID, st.Complete, st.Scanned, ms(time.Now())); err != nil {
		return err
	}
	return tx.Commit()
}

// LegacyClaim is a recorded address that may still lack an owner or a
// plaintext form: every imported hash, and every plaintext address with no
// owner.
type LegacyClaim struct {
	Hash   string
	Email  string  // set for a verified_emails row
	UserID *string // current owner, if any
}

// LegacyClaims lists the claims reconciliation can still improve.
func (s *Store) LegacyClaims(ctx context.Context) ([]LegacyClaim, error) {
	var out []LegacyClaim
	rows, err := s.db.QueryContext(ctx, `select sha256, user_id from legacy_email_hashes`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var c LegacyClaim
		if err := rows.Scan(&c.Hash, &c.UserID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		out = append(out, c)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = s.db.QueryContext(ctx, `select email from verified_emails where user_id is null`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var email string
		if err := rows.Scan(&email); err != nil {
			return nil, err
		}
		out = append(out, LegacyClaim{Hash: LegacyHash(email), Email: email})
	}
	return out, rows.Err()
}

// EvidenceFor returns the stored evidence for the given hashes, by hash.
func (s *Store) EvidenceFor(ctx context.Context, hashes []string) (map[string][]LogEvidence, error) {
	out := map[string][]LogEvidence{}
	for start := 0; start < len(hashes); start += evidenceChunk {
		chunk := hashes[start:min(start+evidenceChunk, len(hashes))]
		args := make([]any, len(chunk))
		for i, h := range chunk {
			args[i] = h
		}
		rows, err := s.db.QueryContext(ctx, `
			select message_id, sha256, kind, coalesce(user_id, ''), coalesce(email, ''), posted_at
			from legacy_log_evidence
			where sha256 in (?`+strings.Repeat(",?", len(chunk)-1)+`)
			order by posted_at`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var e LogEvidence
			if err := rows.Scan(&e.MessageID, &e.Hash, &e.Kind, &e.UserID, &e.Email, timeDest{&e.PostedAt}); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[e.Hash] = append(out[e.Hash], e)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ResolveClaim records what reconciliation found for a claim: its owner, its
// plaintext address, or both. An owner is only ever filled in, never
// replaced. A hash whose plaintext is found becomes a verified_emails row,
// which is what /email whois and the dedup check read first.
func (s *Store) ResolveClaim(ctx context.Context, c LegacyClaim, owner, email string) error {
	var ownerArg *string
	if owner != "" {
		ownerArg = &owner
	}

	if c.Email != "" {
		_, err := s.db.ExecContext(ctx, `
			update verified_emails set user_id = ?2
			where email = ?1 and user_id is null`, c.Email, ownerArg)
		return err
	}

	if email == "" {
		_, err := s.db.ExecContext(ctx, `
			update legacy_email_hashes set user_id = ?2
			where sha256 = ?1 and user_id is null`, c.Hash, ownerArg)
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	if ownerArg == nil {
		ownerArg = c.UserID
	}
	email = NormalizeEmail(email)
	if _, err := tx.ExecContext(ctx, `
		insert into verified_emails (email, user_id, domain, source, verified_at)
		values (?1, ?2, ?3, ?4, ?5)
		on conflict (email) do update set
			user_id = coalesce(verified_emails.user_id, excluded.user_id)`,
		email, ownerArg, EmailDomain(email), SourceDiscordLog, ms(time.Now())); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`delete from legacy_email_hashes where sha256 = ?1`, c.Hash); err != nil {
		return err
	}
	return tx.Commit()
}
