package verify

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/altcha-org/altcha-lib-go"
)

// ErrLinkUsed is returned by a Lifter when the link was already used or has
// been replaced by a newer one.
var ErrLinkUsed = errors.New("verify: link already used")

// Lifter is the bot side of verification: it knows which links are still
// outstanding and how to lift a timeout on Discord.
type Lifter interface {
	TokenActive(ctx context.Context, c *Claims) (bool, error)
	Lift(ctx context.Context, c *Claims) error
}

const (
	// challengeMaxNumber sets the proof-of-work difficulty: about a second
	// of work on a phone, which is nothing to a person and real cost to a
	// script working through many hijacked accounts.
	challengeMaxNumber = 200_000
	challengeTTL       = 10 * time.Minute
	requestTimeout     = 15 * time.Second
	maxBodyBytes       = 8 << 10
)

var (
	//go:embed page.html
	pageHTML string
	//go:embed logo.png
	logoPNG []byte

	pageTmpl = template.Must(template.New("page").Parse(pageHTML))
)

// Page states rendered by page.html.
const (
	stateReady   = "ready"
	stateInvalid = "invalid"
	stateError   = "error"
)

// Server serves the verification page and its API.
type Server struct {
	tokens *Tokens
	lifter Lifter
	log    *slog.Logger
}

// NewServer returns a Server.
func NewServer(tokens *Tokens, lifter Lifter, logger *slog.Logger) *Server {
	return &Server{tokens: tokens, lifter: lifter, log: logger}
}

// Register mounts the verification routes on mux.
func (v *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /verify", v.page)
	mux.HandleFunc("GET /verify/challenge", v.challenge)
	mux.HandleFunc("POST /verify/solve", v.solve)
	mux.HandleFunc("GET /verify/logo.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(logoPNG)
	})
}

// secure sets the headers every response carrying a token needs. The link
// is a bearer credential, so it must not leak through the Referer header to
// the CDN, sit in a cache, or be framed.
func secure(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
}

// active parses a token and checks it is still the member's outstanding link.
// It returns the page state to show when it is not.
func (v *Server) active(ctx context.Context, raw string) (*Claims, string) {
	c, err := v.tokens.Parse(raw)
	if err != nil {
		return nil, stateInvalid
	}
	ok, err := v.lifter.TokenActive(ctx, c)
	if err != nil {
		v.log.Error("verify: token lookup failed", "user_id", c.Subject, "error", err)
		return nil, stateError
	}
	if !ok {
		return nil, stateInvalid
	}
	return c, stateReady
}

func (v *Server) page(w http.ResponseWriter, r *http.Request) {
	secure(w)
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	raw := r.URL.Query().Get("t")
	_, state := v.active(ctx, raw)
	if state != stateReady {
		raw = ""
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTmpl.Execute(w, map[string]string{"State": state, "Token": raw}); err != nil {
		v.log.Warn("verify: render page", "error", err)
	}
}

func (v *Server) challenge(w http.ResponseWriter, r *http.Request) {
	secure(w)
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	if _, state := v.active(ctx, r.URL.Query().Get("t")); state != stateReady {
		writeJSON(w, statusFor(state), map[string]string{"error": state})
		return
	}
	expires := time.Now().Add(challengeTTL)
	ch, err := altcha.CreateChallenge(altcha.ChallengeOptions{
		HMACKey:   v.tokens.altchaKey(),
		MaxNumber: challengeMaxNumber,
		Expires:   &expires,
	})
	if err != nil {
		v.log.Error("verify: create challenge", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": stateError})
		return
	}
	writeJSON(w, http.StatusOK, ch)
}

type solveRequest struct {
	Token   string `json:"token"`
	Payload string `json:"payload"`
}

func (v *Server) solve(w http.ResponseWriter, r *http.Request) {
	secure(w)
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	var req solveRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
		return
	}
	c, err := v.tokens.Parse(req.Token)
	if err != nil {
		writeJSON(w, http.StatusGone, map[string]string{"error": stateInvalid})
		return
	}
	ok, err := altcha.VerifySolutionSafe(req.Payload, v.tokens.altchaKey(), true)
	if err != nil || !ok {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "captcha"})
		return
	}

	switch err := v.lifter.Lift(ctx, c); {
	case errors.Is(err, ErrLinkUsed):
		writeJSON(w, http.StatusGone, map[string]string{"error": stateInvalid})
	case err != nil:
		v.log.Error("verify: lift failed", "user_id", c.Subject, "error", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": stateError})
	default:
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

func statusFor(state string) int {
	if state == stateInvalid {
		return http.StatusGone
	}
	return http.StatusServiceUnavailable
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
