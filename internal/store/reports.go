package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// MaterialReported holds a file a valid report pulled off the page until an
// admin decides. Unlike MaterialReview the bucket copy stays, so putting the
// file back is instant.
const MaterialReported = "reported"

const (
	// ReportsPerWeek is how many reports a member may file in ReportWindow.
	ReportsPerWeek = 5
	ReportWindow   = 7 * 24 * time.Hour
)

// States of a report.
const (
	ReportChecking = "checking"
	ReportValid    = "valid"
	ReportInvalid  = "invalid"
)

// ErrReportDuplicate is returned when the member already reported the file.
var ErrReportDuplicate = errors.New("store: file already reported by this member")

// ReportLimitError is returned when the member used up the week's reports.
// ResetsAt is when the oldest of them stops counting.
type ReportLimitError struct{ ResetsAt time.Time }

func (e *ReportLimitError) Error() string { return "store: weekly report limit reached" }

// Report is one member's report against one material.
type Report struct {
	ID            int64
	AttachmentID  string
	ReporterID    string
	Reason        string
	CreatedAt     time.Time
	Status        string
	Note          string
	CardMessageID *string
}

// ReserveReport records a report as checking, unless the member used up the
// week's reports or reported the file before. It is one statement, so
// parallel requests cannot go past the limit. left is how many reports the
// member has after this one.
func (s *Library) ReserveReport(ctx context.Context, attachmentID, reporterID, reason string, now time.Time) (id int64, left int, err error) {
	cut := ms(now.Add(-ReportWindow))
	res, err := s.db.ExecContext(ctx, `insert into material_reports (attachment_id, reporter_id, reason, created_at)
		select ?1, ?2, ?3, ?4
		where (select count(*) from material_reports where reporter_id = ?2 and created_at > ?5) < ?6
		on conflict do nothing`, attachmentID, reporterID, reason, ms(now), cut, ReportsPerWeek)
	if err != nil {
		return 0, 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	if n == 0 {
		var dup int
		if err := s.db.QueryRowContext(ctx, `select count(*) from material_reports
			where reporter_id = ?1 and attachment_id = ?2`, reporterID, attachmentID).Scan(&dup); err != nil {
			return 0, 0, err
		}
		if dup > 0 {
			return 0, 0, ErrReportDuplicate
		}
		var oldest sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `select min(created_at) from material_reports
			where reporter_id = ?1 and created_at > ?2`, reporterID, cut).Scan(&oldest); err != nil {
			return 0, 0, err
		}
		return 0, 0, &ReportLimitError{ResetsAt: time.UnixMilli(oldest.Int64).Add(ReportWindow)}
	}
	id, err = res.LastInsertId()
	if err != nil {
		return 0, 0, err
	}
	var used int
	if err := s.db.QueryRowContext(ctx, `select count(*) from material_reports
		where reporter_id = ?1 and created_at > ?2`, reporterID, cut).Scan(&used); err != nil {
		return 0, 0, err
	}
	return id, max(0, ReportsPerWeek-used), nil
}

// SettleReport records the verdict on a report.
func (s *Library) SettleReport(ctx context.Context, id int64, status, note string) error {
	_, err := s.db.ExecContext(ctx, `update material_reports set status = ?2, verdict_note = ?3 where id = ?1`,
		id, status, nullStr(note))
	return err
}

// DropReport deletes a report whose check could not run, which gives the
// member the report back.
func (s *Library) DropReport(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `delete from material_reports where id = ?1 and status = ?2`, id, ReportChecking)
	return err
}

// PullReported takes an approved, listed file off the page for a valid
// report. It reports false when the file was not listed any more (somebody
// reported it first, or it was removed meanwhile).
func (s *Library) PullReported(ctx context.Context, homeChannelID, attachmentID, note string) (*Material, bool, error) {
	res, err := s.db.ExecContext(ctx, `update materials
		set status = ?3, screen_note = ?4, screened_at = ?5
		where attachment_id = ?1 and home_channel_id = ?2 and `+visibleMaterial,
		attachmentID, homeChannelID, MaterialReported, nullStr(note), ms(time.Now()))
	if err != nil {
		return nil, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	m, err := s.MaterialByID(ctx, attachmentID)
	return m, n > 0, err
}

// MaterialByID returns a material whatever its state, or ErrNotFound.
func (s *Library) MaterialByID(ctx context.Context, attachmentID string) (*Material, error) {
	return scanMaterial(s.db.QueryRowContext(ctx, `select `+materialColumns+` from materials
		where attachment_id = ?1`, attachmentID))
}

const reportColumns = `id, attachment_id, reporter_id, reason, created_at, status, verdict_note, card_message_id`

func scanReport(row scanner) (*Report, error) {
	var r Report
	var note sql.NullString
	if err := row.Scan(&r.ID, &r.AttachmentID, &r.ReporterID, &r.Reason, timeDest{&r.CreatedAt},
		&r.Status, &note, &r.CardMessageID); err != nil {
		return nil, noRows(err)
	}
	r.Note = note.String
	return &r, nil
}

// ReportsToAnnounce lists settled reports, valid or not, whose admin card is
// not posted yet.
func (s *Library) ReportsToAnnounce(ctx context.Context, limit int) ([]*Report, error) {
	rows, err := s.db.QueryContext(ctx, `select `+reportColumns+` from material_reports
		where status in (?1, ?2) and card_message_id is null order by id limit ?3`, ReportValid, ReportInvalid, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Report
	for rows.Next() {
		r, err := scanReport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetReportCard remembers the admin card of a report.
func (s *Library) SetReportCard(ctx context.Context, id int64, messageID string) error {
	_, err := s.db.ExecContext(ctx, `update material_reports set card_message_id = ?2 where id = ?1`, id, messageID)
	return err
}

// ResolveReport records an admin's decision on a reported file: remove takes
// it down for good, otherwise it goes back on the page. It reports false when
// the file was no longer awaiting a decision, so two admins clicking at once
// cannot both win. The file's row is returned either way.
func (s *Library) ResolveReport(ctx context.Context, attachmentID string, remove bool, note string) (*Material, bool, error) {
	status := MaterialApproved
	if remove {
		status = MaterialRejected
	}
	// A removed file loses its bucket copy's mark; one put back keeps it.
	res, err := s.db.ExecContext(ctx, `update materials
		set status = ?2, screen_note = ?3, screened_at = ?4,
		    backed_up_at = case when ?6 then null else backed_up_at end
		where attachment_id = ?1 and status = ?5`,
		attachmentID, status, nullStr(note), ms(time.Now()), MaterialReported, remove)
	if err != nil {
		return nil, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, false, err
	}
	m, err := s.MaterialByID(ctx, attachmentID)
	return m, n > 0, err
}
