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
	user  string
	props map[string]any
}

// Tag attaches facts to the request in ctx: the member it was for, and any
// further properties as key, value pairs. Handlers call it as soon as they
// know; an empty user leaves the member as it was. Outside a request served
// through Middleware it does nothing.
func Tag(ctx context.Context, user string, kv ...any) {
	t, _ := ctx.Value(tagKey{}).(*tags)
	if t == nil {
		return
	}
	if user != "" {
		t.user = user
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if k, ok := kv[i].(string); ok {
			t.props[k] = kv[i+1]
		}
	}
}

// Middleware records every page and API request next. Static assets, the
// health probe and requests no route matched (scanners) are left out. The
// query string is never recorded: it carries the signed links.
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
			t.props["path"] = r.URL.Path
			t.props["status"] = rec.status
			t.props["bytes"] = rec.bytes
			t.props["duration_ms"] = time.Since(start).Milliseconds()
			t.props["user_agent"] = clip(r.UserAgent(), 200)
			c.Capture(t.user, "http_request", t.props)
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
