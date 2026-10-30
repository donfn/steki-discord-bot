package analytics

import (
	"context"
	"net/http"
	"strings"
	"time"
)

type tagKey struct{}

// tags is what a handler learned about its request, read back by Middleware
// once the response is written.
type tags struct {
	props map[string]any
}

// Tag attaches properties to the request in ctx as key, value pairs. Handlers
// call it as soon as they know. Outside a request served through Middleware it
// does nothing.
func Tag(ctx context.Context, kv ...any) {
	t, _ := ctx.Value(tagKey{}).(*tags)
	if t == nil {
		return
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			t.props[k] = kv[i+1]
		}
	}
}

// Middleware records every page and API request next: its route, status, size
// and duration, never its address, path, query string (which carries signed
// links) or user agent. Static assets, the health probe and requests no route
// matched (scanners) are left out.
func (c *Client) Middleware(next http.Handler) http.Handler {
	if c == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := &tags{props: map[string]any{}}
		r = r.WithContext(context.WithValue(r.Context(), tagKey{}, t))
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		defer func() {
			if r.Pattern == "" || skipped(r.URL.Path) {
				return
			}
			t.props["method"] = r.Method
			t.props["route"] = r.Pattern
			t.props["status"] = rec.status
			t.props["bytes"] = rec.bytes
			t.props["duration_ms"] = time.Since(start).Milliseconds()
			c.Capture("http_request", t.props)
		}()
		next.ServeHTTP(rec, r)
	})
}

func skipped(path string) bool {
	return path == "/healthz" || strings.HasPrefix(path, "/verify/uni/") || path == "/verify/logo.png"
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// recorder remembers the status and size of a response.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer, which the
// material download needs to extend its write deadline.
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
