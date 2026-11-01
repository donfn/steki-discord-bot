package store

import (
	"context"
	"errors"
	"time"
)

// StudyNotesQuota is the most a member's saved notes may weigh in all, in
// bytes. Each save is capped on its own; this keeps one account from filling
// the volume with many of them.
const StudyNotesQuota = 32 << 20

// ErrStudyQuota is returned by SetStudyNote when a save would take the member
// past StudyNotesQuota.
var ErrStudyQuota = errors.New("store: study notes quota reached")

// StudyNote is one member's saved state of one kind for one file.
type StudyNote struct {
	AttachmentID string
	Kind         string
	Data         string
}

// StudyNotes lists what a member saved, for every file.
func (s *Library) StudyNotes(ctx context.Context, userID string) ([]StudyNote, error) {
	rows, err := s.db.QueryContext(ctx,
		`select attachment_id, kind, data from study_notes where user_id = ?1`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StudyNote
	for rows.Next() {
		var n StudyNote
		if err := rows.Scan(&n.AttachmentID, &n.Kind, &n.Data); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ClearStudyNotes removes what a member saved, once it has moved to their own
// database in the bucket.
func (s *Library) ClearStudyNotes(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `delete from study_notes where user_id = ?1`, userID)
	return err
}

// SetStudyNote saves a member's state of one kind for one file, replacing
// what was there. Empty data deletes it.
func (s *Library) SetStudyNote(ctx context.Context, userID, attachmentID, kind, data string) error {
	if data == "" {
		_, err := s.db.ExecContext(ctx, `delete from study_notes
			where user_id = ?1 and attachment_id = ?2 and kind = ?3`, userID, attachmentID, kind)
		return err
	}
	// One statement, so parallel saves cannot both slip under the quota. What
	// the save replaces does not count against it.
	res, err := s.db.ExecContext(ctx, `
		insert into study_notes (user_id, attachment_id, kind, data, updated_at)
		select ?1, ?2, ?3, ?4, ?5
		where (select coalesce(sum(length(cast(data as blob))), 0) from study_notes
			where user_id = ?1 and not (attachment_id = ?2 and kind = ?3)) + length(cast(?4 as blob)) <= ?6
		on conflict (user_id, attachment_id, kind) do update set data = excluded.data, updated_at = excluded.updated_at`,
		userID, attachmentID, kind, data, ms(time.Now()), StudyNotesQuota)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrStudyQuota
	}
	return nil
}
