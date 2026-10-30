package verify

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

var dataBlock = regexp.MustCompile(`(?s)<script type="application/json" id="data">(.*?)</script>`)

// pageData reads the JSON the page was rendered with.
func pageData(t *testing.T, body string) (socket string, notes map[string]map[string]json.RawMessage) {
	t.Helper()
	m := dataBlock.FindStringSubmatch(body)
	if m == nil {
		t.Fatal("page has no data block")
	}
	var d struct {
		Socket string                                `json:"socket"`
		Exp    int64                                 `json:"socketExp"`
		Notes  map[string]map[string]json.RawMessage `json:"notes"`
	}
	if err := json.Unmarshal([]byte(m[1]), &d); err != nil {
		t.Fatal(err)
	}
	if d.Socket != "" && time.UnixMilli(d.Exp).Before(time.Now().Add(socketTokenTTL-time.Minute)) {
		t.Errorf("socket token expiry %v", time.UnixMilli(d.Exp))
	}
	return d.Socket, d.Notes
}

type testSocket struct {
	t    *testing.T
	conn *websocket.Conn
}

func (e *testEnv) dial(t *testing.T, token string) *testSocket {
	t.Helper()
	h := http.Header{"Origin": {e.srv.URL}}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(e.srv.URL, "http")+"/s", h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	s := &testSocket{t: t, conn: conn}
	s.send(map[string]any{"op": "auth", "t": token})
	return s
}

func (s *testSocket) send(v any) {
	s.t.Helper()
	if err := s.conn.WriteJSON(v); err != nil {
		s.t.Fatal(err)
	}
}

// read returns the next message, or the close code when the socket closed.
func (s *testSocket) read() (map[string]any, int) {
	s.t.Helper()
	_ = s.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var m map[string]any
	if err := s.conn.ReadJSON(&m); err != nil {
		if ce, ok := err.(*websocket.CloseError); ok {
			return nil, ce.Code
		}
		s.t.Fatal(err)
	}
	return m, 0
}

func TestSocketSavesAndReissues(t *testing.T) {
	e := newEnv(t)
	_, _, body := e.open(t, "100")
	first, _ := pageData(t, body)
	if first == "" {
		t.Fatal("the page carries no socket token")
	}

	ws := e.dial(t, first)
	m, _ := ws.read()
	next, _ := m["t"].(string)
	if m["op"] != "token" || next == "" || next == first {
		t.Fatalf("first message = %v, want a fresh token", m)
	}
	ws.send(map[string]any{"op": "save", "seq": 1, "items": []map[string]any{
		{"f": "11", "k": "study", "d": map[string]any{"d": 1, "n": "<b>κεφ. 1</b></script>"}},
		{"f": "11", "k": "ink", "d": map[string]any{"1": []any{}}},
		{"f": "21", "k": "study", "d": map[string]any{"d": 1}}, // another channel's file
		{"f": "12", "k": "x", "d": map[string]any{"d": 1}},
		{"f": "12", "k": "study", "d": "x"},
	}})
	m, _ = ws.read()
	if m["op"] != "ack" || m["seq"] != float64(1) {
		t.Fatalf("ack = %v", m)
	}
	if drops, _ := m["drop"].([]any); len(drops) != 3 {
		t.Errorf("dropped %v, want the three bad items", m["drop"])
	}
	ws.send(map[string]any{"op": "save", "seq": 2, "items": []map[string]any{
		{"f": "11", "k": "study", "d": map[string]any{"d": 1, "t": "Θέματα 2023"}},
		{"f": "11", "k": "ink", "d": nil},
	}})
	if m, _ := ws.read(); m["op"] != "ack" || m["drop"] != nil {
		t.Fatalf("second ack = %v", m)
	}

	// A used token does not open a second socket; the reissued one does.
	if _, code := e.dial(t, first).read(); code != socketExpired {
		t.Errorf("reused token: close %d, want %d", code, socketExpired)
	}
	if m, _ := e.dial(t, next).read(); m["op"] != "token" {
		t.Errorf("reissued token refused: %v", m)
	}
	// Other kinds of token do not open it either.
	link := e.materialToken(t, "100", time.Minute)
	if _, code := e.dial(t, link).read(); code != socketExpired {
		t.Errorf("material link opened the socket: close %d", code)
	}

	// A new visit, as on another device, finds the latest saves.
	_, _, body = e.open(t, "100")
	if _, notes := pageData(t, body); !strings.Contains(string(notes["11"]["study"]), "Θέματα 2023") ||
		notes["11"]["ink"] != nil {
		t.Errorf("notes on the next visit = %v", notes)
	}
	if strings.Contains(body, "</script><b>") {
		t.Error("saved notes were not escaped")
	}
}

func TestSocketNeedsAuthFirst(t *testing.T) {
	e := newEnv(t)
	h := http.Header{"Origin": {e.srv.URL}}
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(e.srv.URL, "http")+"/s", h)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.WriteJSON(map[string]any{"op": "save", "seq": 1})
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Error("a socket without a token stayed open")
	}

	// Another site cannot open it.
	bad := http.Header{"Origin": {"https://evil.example"}}
	if _, res, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(e.srv.URL, "http")+"/s", bad); err == nil ||
		res == nil || res.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site socket: %v", err)
	}
}
