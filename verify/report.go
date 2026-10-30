package verify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"steki-bot/analytics"
)

// A member can report a file on the material page. The report is checked
// straight away, by the same pipeline that screens uploads: if the reason
// holds and matches the file, the file is pulled from the page on the spot and
// the admins get a card to decide. See events/reports.go.

const (
	// reportTimeout bounds one report, mostly the model reading the file.
	reportTimeout = 90 * time.Second

	reasonMin = 10
	reasonMax = 500
)

// Errors a Reporter returns for the cases the page tells the member about.
var (
	// ErrReportAlready: this member already reported this file.
	ErrReportAlready = errors.New("verify: file already reported")
	// ErrReportUnavailable: the check could not run. The report was not
	// counted, so the member can try again.
	ErrReportUnavailable = errors.New("verify: report check unavailable")
)

// ReportLimitError is returned when the member used up the week's reports.
type ReportLimitError struct{ ResetsAt time.Time }

func (e *ReportLimitError) Error() string { return "verify: weekly report limit reached" }

// ReportResult is the outcome of a checked report.
type ReportResult struct {
	// Pulled is true when the report held and the file left the page.
	Pulled bool
	// Note says why, in one Greek sentence.
	Note string
	// Left is how many reports the member has left this week.
	Left int
}

// Reporter is implemented by a MaterialSource that takes reports. userID is
// the member the session belongs to.
type Reporter interface {
	Report(ctx context.Context, userID, channelID, fileID, reason string) (*ReportResult, error)
}

// cleanReason trims a reason and drops control characters.
func cleanReason(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(s)
}

// reportFile checks one report about a file of the session's page.
func (v *Server) reportFile(w http.ResponseWriter, r *http.Request) {
	secure(w)
	rep, ok := v.materials.(Reporter)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "gone"})
		return
	}
	var in struct {
		File   string `json:"file"`
		Reason string `json:"reason"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !isSnowflake(in.File) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
		return
	}
	reason := cleanReason(in.Reason)
	if n := utf8.RuneCountInString(reason); n < reasonMin || n > reasonMax {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "reason"})
		return
	}
	sid := sessionID(r)
	channelID, subject, ok := v.sessions.authorizeOnPage(sid, in.File, time.Now())
	if _, who := v.sessions.owner(sid); who.Viewer != "" {
		// A staff member viewing as the member cannot report in their name.
		ok = false
	}
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": stateInvalid})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), reportTimeout)
	defer cancel()
	if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(reportTimeout + 5*time.Second)); err != nil {
		v.log.Debug("verify: extend write deadline", "error", err)
	}
	analytics.Tag(ctx, subject, "channel_id", channelID, "file_id", in.File)

	res, err := rep.Report(ctx, subject, channelID, in.File, reason)
	var limit *ReportLimitError
	switch {
	case errors.Is(err, ErrNoMaterial):
		writeJSON(w, http.StatusGone, map[string]string{"error": "gone"})
	case errors.Is(err, ErrReportAlready):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "already"})
	case errors.As(err, &limit):
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "limit", "resets": limit.ResetsAt.UnixMilli()})
	case errors.Is(err, ErrReportUnavailable):
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
	case err != nil:
		v.log.Error("verify: report failed", "user_id", subject, "file_id", in.File, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": stateError})
	default:
		// The member is only told the file is under review: the check's
		// verdict and note stay with the admins.
		outcome := "report_declined"
		if res.Pulled {
			outcome = "report_pulled"
		}
		analytics.Tag(ctx, subject, "outcome", outcome)
		writeJSON(w, http.StatusOK, map[string]any{"status": "received", "left": res.Left})
	}
}
