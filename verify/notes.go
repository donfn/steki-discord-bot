package verify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
)

// What a member keeps for themselves on the material page (studied marks,
// their own titles, notes, ink) is saved per member and file, so it is there
// on any device the next time they open the page. The page sends its saves
// over a socket (socket.go). There is no syncing: every save replaces what was
// stored, and the latest one stays.

// Kinds of state the page saves per file, and the most each may weigh.
const (
	NoteStudy = "study"
	NoteInk   = "ink"

	maxStudyBytes = 64 << 10
	maxInkBytes   = 1 << 20
)

// ErrNoteQuota is returned by a Notebook when a save would take the member past
// the most notes one member may keep.
var ErrNoteQuota = errors.New("verify: note storage full")

// Notebook is implemented by a MaterialSource that keeps members' notes.
type Notebook interface {
	// Notes returns what the member saved, by file ID and kind.
	Notes(ctx context.Context, userID string) (map[string]map[string]json.RawMessage, error)
	// SaveNote replaces what the member saved of one kind for one file. Nil
	// data deletes it. It returns ErrNoteQuota when the member's notes would
	// outgrow their allowance.
	SaveNote(ctx context.Context, userID, fileID, kind string, data json.RawMessage) error
}

// notesFor returns the member's saved state for the files on the page. A
// failure leaves the page without it rather than without the page.
func (v *Server) notesFor(ctx context.Context, userID string, fileIDs []string) map[string]map[string]json.RawMessage {
	out := map[string]map[string]json.RawMessage{}
	nb, ok := v.materials.(Notebook)
	if !ok || len(fileIDs) == 0 {
		return out
	}
	all, err := nb.Notes(ctx, userID)
	if err != nil {
		v.log.Error("verify: load notes", "user_id", userID, "error", err)
		return out
	}
	for _, id := range fileIDs {
		if n := all[id]; n != nil {
			out[id] = n
		}
	}
	return out
}

// cleanNote checks one save of the page: data must be a JSON object within
// the kind's size, and an empty one deletes. It returns the data to store, or
// an error code for the page.
func cleanNote(kind string, raw json.RawMessage) (json.RawMessage, string) {
	data := bytes.TrimSpace(raw)
	switch {
	case kind != NoteStudy && kind != NoteInk:
		return nil, "bad_request"
	case len(data) == 0 || bytes.Equal(data, []byte("null")) || bytes.Equal(data, []byte("{}")):
		return nil, ""
	case data[0] != '{' || !json.Valid(data):
		return nil, "bad_request"
	case kind == NoteStudy && len(data) > maxStudyBytes, kind == NoteInk && len(data) > maxInkBytes:
		return nil, "too_large"
	}
	return data, ""
}
