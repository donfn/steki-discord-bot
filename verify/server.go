package verify

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"time"
)

// ErrLinkUsed is returned by a Lifter or Consenter when the link was already
// used or has been replaced by a newer one.
var ErrLinkUsed = errors.New("verify: link already used")

// ErrEmailTaken is returned by a Consenter when the address was bound to
// another account after the email was sent.
var ErrEmailTaken = errors.New("verify: address already in use")

// Lifter is the bot side of the media guard: it knows which links are still
// outstanding and how to lift a timeout on Discord.
type Lifter interface {
	TokenActive(ctx context.Context, c *Claims) (bool, error)
	Lift(ctx context.Context, c *Claims) error
}

const (
	requestTimeout = 15 * time.Second
	maxBodyBytes   = 8 << 10
)

var (
	//go:embed page.html link.html
	pagesFS embed.FS
	//go:embed logo.png
	logoPNG []byte
	//go:embed uni
	uniFS embed.FS

	guardTmpl = template.Must(template.ParseFS(pagesFS, "page.html"))
	linkTmpl  = template.Must(template.ParseFS(pagesFS, "link.html"))
)

// Page states rendered by both pages.
const (
	stateReady   = "ready"
	stateInvalid = "invalid"
	stateError   = "error"
)

// Server serves the verification pages and their API.
type Server struct {
	tokens  *Tokens
	lifter  Lifter
	consent Consenter
	log     *slog.Logger
}

// NewServer returns a Server.
func NewServer(tokens *Tokens, lifter Lifter, consent Consenter, logger *slog.Logger) *Server {
	return &Server{tokens: tokens, lifter: lifter, consent: consent, log: logger}
}

// Register mounts the routes on mux.
func (v *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /verify", v.page)
	mux.HandleFunc("GET /verify/challenge", v.challenge)
	mux.HandleFunc("POST /verify/solve", v.solve)
	mux.HandleFunc("GET /link", v.linkPage)
	mux.HandleFunc("GET /link/challenge", v.linkChallenge)
	mux.HandleFunc("POST /link/accept", v.linkAccept)

	mux.HandleFunc("GET /verify/logo.png", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(logoPNG)
	})
	mux.HandleFunc("GET /verify/uni/{file}", func(w http.ResponseWriter, r *http.Request) {
		b, err := uniFS.ReadFile("uni/" + r.PathValue("file"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(b)
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
	h.Set("Strict-Transport-Security", "max-age=31536000")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
}

// pageCSP allows exactly what the pages load: the pinned ALTCHA script and
// the page's own inline script (by nonce), Google Fonts, this origin's images
// and Discord avatars, and fetches to this origin. ALTCHA runs its proof of
// work in blob: workers.
const pageCSP = "default-src 'none'; script-src 'nonce-%s' https://cdn.jsdelivr.net; " +
	"style-src 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; " +
	"img-src 'self' data: https://cdn.discordapp.com; connect-src 'self'; worker-src blob:; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func newNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// render writes an HTML page under a fresh CSP nonce.
func (v *Server) render(w http.ResponseWriter, tmpl *template.Template, data map[string]any) {
	nonce := newNonce()
	data["Nonce"] = nonce
	w.Header().Set("Content-Security-Policy", fmt.Sprintf(pageCSP, nonce))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		v.log.Warn("verify: render page", "error", err)
	}
}

// guardClaims parses a media guard token; registration links are refused.
func (v *Server) guardClaims(raw string) (*Claims, bool) {
	c, err := v.tokens.Parse(raw)
	if err != nil || c.Reason == ReasonLink {
		return nil, false
	}
	return c, true
}

// active parses a guard token and checks it is still the member's
// outstanding link. It returns the page state to show when it is not.
func (v *Server) active(ctx context.Context, raw string) (*Claims, string) {
	c, ok := v.guardClaims(raw)
	if !ok {
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
	c, state := v.active(ctx, raw)
	reason := ""
	if state != stateReady {
		raw = ""
	} else {
		reason = c.Reason
	}
	v.render(w, guardTmpl, map[string]any{"State": state, "Token": raw, "Reason": reason})
}

func (v *Server) challenge(w http.ResponseWriter, r *http.Request) {
	secure(w)
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	c, state := v.active(ctx, r.URL.Query().Get("t"))
	if state != stateReady {
		writeJSON(w, statusFor(state), map[string]string{"error": state})
		return
	}
	v.writeChallenge(w, c.ID)
}

// writeChallenge answers the widget with a challenge bound to linkID.
func (v *Server) writeChallenge(w http.ResponseWriter, linkID string) {
	ch, err := v.tokens.newChallenge(linkID)
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

// readSolve decodes a solve request and checks its captcha against the
// token's link. It writes the error response itself when either fails.
func (v *Server) readSolve(w http.ResponseWriter, r *http.Request, parse func(string) (*Claims, bool)) (*Claims, bool) {
	var req solveRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request"})
		return nil, false
	}
	c, ok := parse(req.Token)
	if !ok {
		writeJSON(w, http.StatusGone, map[string]string{"error": stateInvalid})
		return nil, false
	}
	if !v.tokens.checkSolution(req.Payload, c.ID) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "captcha"})
		return nil, false
	}
	return c, true
}

func (v *Server) solve(w http.ResponseWriter, r *http.Request) {
	secure(w)
	ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
	defer cancel()

	c, ok := v.readSolve(w, r, v.guardClaims)
	if !ok {
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
