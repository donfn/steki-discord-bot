package verify

import (
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// maxInFlight caps concurrent requests across the whole public surface,
	// so a flood cannot exhaust the database pool or memory.
	maxInFlight = 128

	// rateWindow and rateLimit bound requests per client address. A page
	// load costs about four requests, so this is generous for a real person.
	rateWindow = time.Minute
	rateLimit  = 60

	// fileRateLimit is the per-address budget for material downloads. One
	// page fetches every file of its channel at once, and a computer lab
	// shares one address, so this is far above the page budget; the
	// downloads also need a valid link.
	fileRateLimit = 1200

	// mcpPath and tokenPath are the MCP endpoint and its OAuth token endpoint.
	// They must match mcp.PathMCP and the token path in package mcp.
	mcpPath   = "/mcp"
	tokenPath = "/oauth/token"
)

// limiter is a fixed-window request counter per client address.
type limiter struct {
	limit   int
	mu      sync.Mutex
	windows map[string]*window
	swept   time.Time
}

type window struct {
	start time.Time
	count int
}

func newLimiter(limit int) *limiter {
	return &limiter{windows: make(map[string]*window), limit: limit}
}

func (l *limiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.swept) > rateWindow {
		for k, w := range l.windows {
			if now.Sub(w.start) > rateWindow {
				delete(l.windows, k)
			}
		}
		l.swept = now
	}
	w, ok := l.windows[key]
	if !ok || now.Sub(w.start) > rateWindow {
		l.windows[key] = &window{start: now, count: 1}
		return true
	}
	w.count++
	return w.count <= l.limit
}

// clientIP is the address the platform proxy saw. The proxy appends the
// caller to X-Forwarded-For, so the last entry is the only one a client
// cannot forge; anything to its left is attacker-controlled.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if ip := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(ip) != nil {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Harden wraps h with the protections every response needs, whichever route
// it reaches: panic recovery, security headers (including on 404s), a global
// concurrency cap, per-address rate limiting, and cross-site and content-type
// checks on writes.
func Harden(h http.Handler, logger *slog.Logger) http.Handler {
	sem := make(chan struct{}, maxInFlight)
	lim := newLimiter(rateLimit)
	files := newLimiter(fileRateLimit)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secure(w)
		defer func() {
			if rec := recover(); rec != nil && rec != http.ErrAbortHandler {
				logger.Error("http: handler panic", "path", r.URL.Path, "panic", rec)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()

		budget := lim
		// Assistants call the MCP endpoint in bursts, and the hosted ones share
		// addresses between many members; every call needs a valid token.
		if strings.HasPrefix(r.URL.Path, filesPath) || r.URL.Path == mcpPath {
			budget = files
		}
		if r.URL.Path != "/healthz" && !budget.allow(clientIP(r), time.Now()) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		// A page socket stays open for as long as the page does, so it would
		// hold a slot for hours; sockets have a cap of their own (maxSockets).
		if !isWebSocket(r) {
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			default:
				w.Header().Set("Retry-After", "5")
				http.Error(w, "busy", http.StatusServiceUnavailable)
				return
			}
		}

		if r.Method == http.MethodPost {
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" && origin != "https://"+r.Host && origin != "http://"+r.Host {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			want := "application/json"
			if r.URL.Path == tokenPath {
				want = "application/x-www-form-urlencoded" // RFC 6749
			}
			if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(strings.ToLower(ct), want) {
				http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}
