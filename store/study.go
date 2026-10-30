package store

import (
	"context"
	"time"
)

// StudyNote is one member's saved state of one kind for one file.
type StudyNote struct {
	AttachmentID string
	Kind         string
	Data         string
}

// StudyNotes lists what a member saved, for every file.
func (s *Store) StudyNotes(ctx context.Context, userID string) ([]StudyNote, error) {
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

// SetStudyNote saves a member's state of one kind for one file, replacing
// what was there. Empty data deletes it.
func (s *Store) SetStudyNote(ctx context.Context, userID, attachmentID, kind, data string) error {
	if data == "" {
		_, err := s.db.ExecContext(ctx, `delete from study_notes
			where user_id = ?1 and attachment_id = ?2 and kind = ?3`, userID, attachmentID, kind)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
		insert into study_notes (user_id, attachment_id, kind, data, updated_at) values (?1, ?2, ?3, ?4, ?5)
		on conflict (user_id, attachment_id, kind) do update set data = excluded.data, updated_at = excluded.updated_at`,
		userID, attachmentID, kind, data, ms(time.Now()))
	return err
}
