// Package rpc is how the services talk to each other over the private network.
//
// A service's data is only ever reached through its owner: nothing shares a
// database. Requests are JSON, and signed with a key derived from INTERNAL_SECRET
// so that nothing else on the private network (blastdoor parses strangers' files,
// and must be assumed taken over) can call them. The API listens on its own port,
// which has no public route, so it never meets the public pages' middleware.
package rpc

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"steki-bot/brand"
	"steki-bot/store"
	"steki-bot/tokens"
)

// Port is where every service's internal API listens.
const Port = "8081"

const (
	headerTime = "X-Steki-Time"
	headerSig  = "X-Steki-Signature"
	// window is how old a signed request may be. Addresses on the private
	// network are not secret, so this bounds a replay.
	window       = time.Minute
	maxBody      = 1 << 20
	callTimeout  = 15 * time.Second
	errCodeUsed  = "link_used"
	errCodeTaken = "email_taken"
)

// The API's paths.
const (
	PathLinkPending = "/internal/link/pending"
	PathLinkAccept  = "/internal/link/accept"
	PathEmailsBan   = "/internal/emails/ban"
	PathEmailsUnban = "/internal/emails/unban"
	PathGuardActive = "/internal/guard/active"
	PathGuardLift   = "/internal/guard/lift"
	PathRosterSync  = "/internal/roster/sync"
	PathDropMessage = "/internal/materials/drop-message"
	PathScreenTitle = "/internal/screen-title"
	PathForget      = "/internal/forget"
	PathEvents      = "/internal/events"
	PathStats       = "/internal/stats"
)

func requestKey(secret string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte("steki internal requests v1"))
	return mac.Sum(nil)
}

func sign(key []byte, path, at string, body []byte) string {
	sum := sha256.Sum256(body)
	mac := hmac.New(sha256.New, key)
	fmt.Fprintf(mac, "%s\n%s\n%x", path, at, sum)
	return hex.EncodeToString(mac.Sum(nil))
}

// Client calls one service. A nil Client (no URL configured) fails every call.
type Client struct {
	base string
	key  []byte
	http *http.Client
}

// NewClient returns a Client for the service at base, or nil when base is empty.
func NewClient(base, secret string) *Client {
	if base == "" {
		return nil
	}
	return &Client{base: strings.TrimRight(base, "/"), key: requestKey(secret), http: &http.Client{Timeout: callTimeout}}
}

// ErrUnavailable wraps failures to reach the service, so a caller can tell them
// from a refusal.
var ErrUnavailable = errors.New("rpc: service unavailable")

type errorBody struct {
	Code  string `json:"code,omitempty"`
	Error string `json:"error"`
}

// Call posts in to path and decodes the answer into out, which may be nil.
func (c *Client) Call(ctx context.Context, path string, in, out any) error {
	if c == nil {
		return fmt.Errorf("%w: no address configured", ErrUnavailable)
	}
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	at := strconv.FormatInt(time.Now().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(headerTime, at)
	req.Header.Set(headerSig, sign(c.key, path, at, body))
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	if resp.StatusCode != http.StatusOK {
		var e errorBody
		_ = json.Unmarshal(data, &e)
		switch e.Code {
		case errCodeUsed:
			return tokens.ErrLinkUsed
		case errCodeTaken:
			return tokens.ErrEmailTaken
		}
		return fmt.Errorf("rpc: %s answered %d: %s", path, resp.StatusCode, e.Error)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

// Server is one service's internal API.
type Server struct {
	mux *http.ServeMux
	key []byte
	log *slog.Logger
}

// NewServer returns an API with no routes yet.
func NewServer(secret string, log *slog.Logger) *Server {
	return &Server{mux: http.NewServeMux(), key: requestKey(secret), log: log}
}

// Handler is the API as an http.Handler.
func (s *Server) Handler() http.Handler { return s.mux }

// Handle adds a route. fn gets the decoded request and returns the answer.
func Handle[In, Out any](s *Server, path string, fn func(ctx context.Context, in In) (Out, error)) {
	s.mux.HandleFunc("POST "+path, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
		if err != nil || len(body) > maxBody {
			writeError(w, http.StatusBadRequest, "", "bad body")
			return
		}
		if !s.authentic(r, path, body) {
			s.log.Warn("rpc: refused an unsigned request", "path", path, "remote", r.RemoteAddr)
			writeError(w, http.StatusUnauthorized, "", "unauthorized")
			return
		}
		var in In
		if err := json.Unmarshal(body, &in); err != nil {
			writeError(w, http.StatusBadRequest, "", "bad json")
			return
		}
		out, err := fn(r.Context(), in)
		switch {
		case errors.Is(err, tokens.ErrLinkUsed):
			writeError(w, http.StatusConflict, errCodeUsed, err.Error())
		case errors.Is(err, tokens.ErrEmailTaken):
			writeError(w, http.StatusConflict, errCodeTaken, err.Error())
		case err != nil:
			s.log.Error("rpc: handler failed", "path", path, "error", err)
			writeError(w, http.StatusInternalServerError, "", err.Error())
		default:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
		}
	})
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Code: code, Error: msg})
}

func (s *Server) authentic(r *http.Request, path string, body []byte) bool {
	at := r.Header.Get(headerTime)
	secs, err := strconv.ParseInt(at, 10, 64)
	if err != nil {
		return false
	}
	if d := time.Since(time.Unix(secs, 0)); d > window || d < -window {
		return false
	}
	return hmac.Equal([]byte(sign(s.key, path, at, body)), []byte(r.Header.Get(headerSig)))
}

// Start listens on Port until the context ends. A failure to start is logged:
// the service's public side keeps going without it.
func (s *Server) Start(ctx context.Context) {
	srv := &http.Server{
		Addr:              ":" + Port,
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      45 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    8 << 10,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Error("rpc: server failed", "error", err)
		}
	}()
}

// The requests and answers.
type (
	// None is an empty request or answer.
	None struct{}

	// ClaimsRequest carries a link the web service has already verified.
	ClaimsRequest struct {
		Claims tokens.Claims `json:"claims"`
	}
	// ConsentAnswer is what the link page shows.
	ConsentAnswer struct {
		Consent brand.Consent `json:"consent"`
	}
	// ActiveAnswer says whether a guard link is still outstanding.
	ActiveAnswer struct {
		Active bool `json:"active"`
	}
	// BanRequest closes a member's addresses.
	BanRequest struct {
		UserID string `json:"user_id"`
		Extra  string `json:"extra,omitempty"`
		Reason string `json:"reason,omitempty"`
		By     string `json:"by,omitempty"`
	}
	// UnbanRequest reopens a member's addresses.
	UnbanRequest struct {
		UserID string `json:"user_id"`
		Email  string `json:"email,omitempty"`
	}
	// UnbanAnswer counts what was reopened.
	UnbanAnswer struct {
		Lifted int `json:"lifted"`
	}
	// DropMessageRequest keeps a message's attachments off the material pages.
	DropMessageRequest struct {
		MessageID string `json:"message_id"`
		GuildID   string `json:"guild_id"`
		ChannelID string `json:"channel_id"`
	}
	// ScreenTitleRequest asks whether an upload's title may be posted.
	ScreenTitleRequest struct {
		Title string `json:"title"`
		Staff bool   `json:"staff"`
	}
	// ScreenTitleAnswer is the verdict.
	ScreenTitleAnswer struct {
		Flagged       bool   `json:"flagged"`
		Matched       string `json:"matched,omitempty"`
		ForeignInvite bool   `json:"foreign_invite"`
	}
	// UserRequest names a member.
	UserRequest struct {
		UserID string `json:"user_id"`
	}
	// EventsRequest asks for a member's audit trail on one service.
	EventsRequest struct {
		UserID string `json:"user_id"`
		Limit  int    `json:"limit"`
	}
	// EventsAnswer is that trail, newest first.
	EventsAnswer struct {
		Events []*store.Event `json:"events"`
	}
	// StatsAnswer is one service's part of /bot stats, ready to show.
	StatsAnswer struct {
		Text string `json:"text"`
	}
)
