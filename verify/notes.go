package verify

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// What a member keeps for themselves on the material page (studied marks,
// their own titles, notes, ink) is saved per member and file, so it is there
// on any device the next time they open the page. There is no syncing: every
// save replaces what was stored, and the latest one stays.

// Kinds of state the page saves per file, and the most each may weigh.
const (
	NoteStudy = "study"
	NoteInk   = "ink"

	maxStudyBytes = 64 << 10
	maxInkBytes   = 1 << 20
)

// Notebook is implemented by a MaterialSource that keeps members' notes.
type Notebook interface {
	// Notes returns what the member saved, by file ID and kind.
	Notes(ctx context.Context, userID string) (map[string]map[string]json.RawMessage, error)
	// SaveNote replaces what the member saved of one kind for one file. Nil
	// data deletes it.
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

// saveNote stores one kind of the member's state for one file of the page.
func (v *Server) saveNote(w http.ResponseWriter, r *http.Request) {
	secure(w)
	nb, ok := v.materials.(Notebook)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "gone"})
		return
	}
	var in struct {
		File string          `json:"file"`
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxInkBytes+4<<10)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !isSnowflake(in.File) ||
		(in.Kind != NoteStudy && in.Kind != NoteInk) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
		return
	}
	data := bytes.TrimSpace(in.Data)
	switch {
	case len(data) == 0 || bytes.Equal(data, []byte("null")) || bytes.Equal(data, []byte("{}")):
		data = nil
	case data[0] != '{':
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
		return
	case in.Kind == NoteStudy && len(data) > maxStudyBytes, in.Kind == NoteInk && len(data) > maxInkBytes:
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "too_large"})
		return
	}
	channelID, subject, ok := v.sessions.authorizeOnPage(sessionID(r), in.File, time.Now())
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": stateInvalid})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()
	if err := nb.SaveNote(ctx, subject, in.File, in.Kind, data); err != nil {
		v.log.Error("verify: save note", "user_id", subject, "channel_id", channelID, "file_id", in.File, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": stateError})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
