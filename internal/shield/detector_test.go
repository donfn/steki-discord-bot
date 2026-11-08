package shield

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"steki-bot/internal/store"
)

func newTest(o Options) *Detector {
	o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if o.ClientIP == nil {
		o.ClientIP = func(r *http.Request) string { return r.RemoteAddr }
	}
	return New(o)
}

const ip = "198.51.100.7"

func TestThreeNotFoundsChallenge(t *testing.T) {
	d := newTest(Options{})
	now := time.Unix(100000, 0)
	for i := 0; i < challengeNotFound-1; i++ {
		if b, _ := d.observe(ip, 404, "/wp-login.php", now.Add(time.Duration(i)*time.Minute)); b != nil {
			t.Fatalf("404 #%d blocked: %+v", i+1, b)
		}
	}
	// Browsers ask for these on their own.
	for _, p := range append(browserProbes, "/.well-known/openid-configuration", "/.well-known/oauth-authorization-server/mcp") {
		if b, _ := d.observe(ip, 404, p, now.Add(5*time.Minute)); b != nil {
			t.Fatalf("404 for %s blocked", p)
		}
	}
	b, _ := d.observe(ip, 404, "/.env", now.Add(30*time.Minute))
	if b == nil || b.Kind != store.IPChallenge || !b.Until.Equal(now.Add(30*time.Minute+challengeFor)) {
		t.Fatalf("third 404 in an hour: %+v", b)
	}
}

func TestOldNotFoundsExpire(t *testing.T) {
	d := newTest(Options{})
	now := time.Unix(100000, 0)
	d.observe(ip, 404, "/a", now)
	d.observe(ip, 404, "/b", now)
	if b, _ := d.observe(ip, 404, "/c", now.Add(notFoundWindow+time.Minute)); b != nil {
		t.Fatalf("404s from over an hour ago still counted: %+v", b)
	}
}

func TestRateChallenge(t *testing.T) {
	d := newTest(Options{})
	now := time.Unix(100000, 0)
	// 10 req/s for 10s is at the limit, not over it.
	for i := 0; i < challengeRate*rateSeconds; i++ {
		if b, _ := d.observe(ip, 200, "/", now.Add(time.Duration(i)*100*time.Millisecond)); b != nil {
			t.Fatalf("request %d blocked at exactly %d/s", i+1, challengeRate)
		}
	}
	b, _ := d.observe(ip, 200, "/", now.Add(9*time.Second+950*time.Millisecond))
	if b == nil || b.Kind != store.IPChallenge {
		t.Fatalf("request over %d/s: %+v", challengeRate, b)
	}
	// Another address is not affected.
	if b, _ := d.observe("198.51.100.8", 404, "/x", now.Add(10*time.Second)); b != nil {
		t.Fatal("a different address was blocked")
	}
}

// challenge puts ip behind a challenge at now.
func challenge(t *testing.T, d *Detector, now time.Time) {
	t.Helper()
	for i := 0; i < challengeNotFound; i++ {
		d.observe(ip, 404, fmt.Sprintf("/probe%d", i), now)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ips[ip].challengeUntil.IsZero() {
		t.Fatal("not challenged")
	}
}

func TestNotFoundsAfterChallengeBan(t *testing.T) {
	d := newTest(Options{})
	now := time.Unix(100000, 0)
	challenge(t, d, now)
	for i := challengeNotFound; i < banNotFound-1; i++ {
		if b, _ := d.observe(ip, 404, "/x", now.Add(time.Minute)); b != nil {
			t.Fatalf("404 #%d banned", i+1)
		}
	}
	b, _ := d.observe(ip, 404, "/x", now.Add(2*time.Minute))
	if b == nil || b.Kind != store.IPBanned || !b.Until.Equal(now.Add(2*time.Minute+banFor)) {
		t.Fatalf("20th 404 in an hour after a challenge: %+v", b)
	}
	if _, ok := d.banned(ip, now.Add(banFor)); !ok {
		t.Fatal("ban over too early")
	}
	if _, ok := d.banned(ip, now.Add(banFor+3*time.Minute)); ok {
		t.Fatal("ban did not end")
	}
}

// flood sends rate requests a second from ip between from and to, and returns
// the first block it caused.
func flood(d *Detector, rate int, from, to time.Time) *store.IPBan {
	step := time.Second / time.Duration(rate)
	for at := from; at.Before(to); at = at.Add(step) {
		if b, _ := d.observe(ip, 200, "/", at); b != nil {
			return b
		}
	}
	return nil
}

func TestSustainedRateAfterChallengeBans(t *testing.T) {
	d := newTest(Options{})
	now := time.Unix(100000, 0)
	challenge(t, d, now)
	if b := flood(d, banRate, now, now.Add(banSustain-time.Second)); b != nil {
		t.Fatalf("banned before %s of %d/s: %+v", banSustain, banRate, b)
	}
	b := flood(d, banRate, now.Add(banSustain-time.Second), now.Add(banSustain+time.Minute))
	if b == nil || b.Kind != store.IPBanned {
		t.Fatalf("not banned after %s of %d/s", banSustain, banRate)
	}
}

func TestPauseResetsSustainedRate(t *testing.T) {
	d := newTest(Options{})
	now := time.Unix(100000, 0)
	challenge(t, d, now)
	if b := flood(d, banRate, now, now.Add(15*time.Minute)); b != nil {
		t.Fatal("banned early")
	}
	// A minute's pause starts the count again.
	resume := now.Add(16 * time.Minute)
	if b := flood(d, banRate, resume, resume.Add(15*time.Minute)); b != nil {
		t.Fatalf("banned though the rate was not held for %s", banSustain)
	}
}

func TestChallengeEnds(t *testing.T) {
	d := newTest(Options{})
	now := time.Unix(100000, 0)
	challenge(t, d, now)
	later := now.Add(challengeFor + 2*time.Hour)
	// After the hour, a flood is back to being challenged, not banned.
	b := flood(d, 2*challengeRate, later, later.Add(time.Minute))
	if b == nil || b.Kind != store.IPChallenge {
		t.Fatalf("after the challenge ended: %+v", b)
	}
}

func TestIPv6CountedPerNetwork(t *testing.T) {
	if a, b := key("2001:db8:1:2::1"), key("2001:db8:1:2:ffff::9"); a != b || a != "2001:db8:1:2::/64" {
		t.Fatalf("keys %q and %q", a, b)
	}
	if k := key(ip); k != ip {
		t.Fatalf("IPv4 key %q", k)
	}
}

type memStore struct {
	mu     sync.Mutex
	blocks []store.IPBan
}

func (m *memStore) RecordIPBan(_ context.Context, b store.IPBan) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.blocks = append(m.blocks, b)
	return nil
}

func (m *memStore) ActiveIPBans(_ context.Context, now time.Time) ([]store.IPBan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.IPBan
	for _, b := range m.blocks {
		if b.Until.After(now) {
			out = append(out, b)
		}
	}
	return out, nil
}

func (m *memStore) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.blocks)
}

func TestMiddlewareRefusesBannedAndStores(t *testing.T) {
	st := &memStore{}
	events := make(chan Event, 4)
	d := newTest(Options{Store: st, Notify: func(e Event) { events <- e }})
	h := d.Middleware(http.NotFoundHandler())
	get := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = ip
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for i := 0; i < banNotFound; i++ {
		get("/wp-admin")
	}
	select {
	case e := <-events:
		if e.Kind != EventBan || e.IP != ip {
			t.Fatalf("event %+v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ban not reported")
	}
	w := get("/")
	if w.Code != http.StatusForbidden || w.Header().Get("Retry-After") == "" {
		t.Fatalf("banned address got %d", w.Code)
	}
	deadline := time.Now().Add(2 * time.Second)
	for st.count() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := st.count(); n != 2 {
		t.Fatalf("%d blocks stored, want a challenge and a ban", n)
	}

	// A restart keeps the ban.
	d2 := newTest(Options{Store: st})
	if err := d2.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := d2.banned(ip, time.Now()); !ok {
		t.Fatal("ban not restored")
	}
}

func TestHealthzIsNotCounted(t *testing.T) {
	d := newTest(Options{})
	h := d.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }))
	for i := 0; i < 100; i++ {
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/healthz", nil))
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.ips) != 0 {
		t.Fatal("health probes were counted")
	}
}

func TestUnderAttackOnlyForSpreadFloods(t *testing.T) {
	d := newTest(Options{Enable: func(context.Context) error { return nil }})
	now := time.Unix(100000, 0)
	// One address flooding is challenged and stops counting towards the
	// whole service long before it could switch Under Attack Mode on.
	for i := 0; i < 5000; i++ {
		if _, r := d.observe(ip, 200, "/", now.Add(time.Duration(i)*time.Millisecond)); r != "" {
			t.Fatalf("one address switched on Under Attack Mode: %s", r)
		}
	}
	// Many addresses, each under the per-address limit, do.
	fired := false
	later := now.Add(time.Hour)
	for i := 0; i < 600 && !fired; i++ {
		addr := fmt.Sprintf("203.0.113.%d", i%60)
		_, r := d.observe(addr, 200, "/", later.Add(time.Duration(i)*10*time.Millisecond))
		fired = r != ""
	}
	if !fired {
		t.Fatal("a flood from 60 addresses did not switch on Under Attack Mode")
	}
}

type fakeEdge struct {
	mu    sync.Mutex
	calls [][2][]string
}

func (f *fakeEdge) Sync(_ context.Context, challenged, banned []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, [2][]string{challenged, banned})
	return nil
}

func TestEdgeSetsSkipIPv6AndEndedBlocks(t *testing.T) {
	d := newTest(Options{})
	now := time.Now()
	d.ips["192.0.2.1"] = &ipState{bannedUntil: now.Add(time.Hour)}
	d.ips["192.0.2.2"] = &ipState{challengeUntil: now.Add(time.Hour)}
	d.ips["192.0.2.3"] = &ipState{challengeUntil: now.Add(-time.Minute)}
	d.ips["2001:db8::/64"] = &ipState{bannedUntil: now.Add(time.Hour)}
	ch, bn := d.edgeSets(now)
	if fmt.Sprint(ch) != "[192.0.2.2]" || fmt.Sprint(bn) != "[192.0.2.1]" {
		t.Fatalf("challenged %v, banned %v", ch, bn)
	}
}

func TestMergeRules(t *testing.T) {
	old := json.RawMessage(`{"version":1,"rules":[
		{"id":"rul_own","description":"steki-shield: banned addresses","priority":1,"if":{"attr":"ipv4.src","op":"in","value":["192.0.2.9"]},"then":{"action":"block"}},
		{"id":"rul_mine","description":"Allow office","priority":2,"if":{"attr":"http.path","op":"eq","value":"/x"},"then":{"action":"allow"}},
		{"id":"rul_two","priority":30,"if":{"attr":"http.path","op":"eq","value":"/y"},"then":{"action":"block"}}]}`)
	many := make([]string, 70)
	for i := range many {
		many[i] = fmt.Sprintf("198.51.100.%d", i)
	}
	set, changed, err := mergeRules(old, many, []string{"192.0.2.1"})
	if err != nil || !changed {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	rules := set["rules"].([]any)
	if len(rules) != 4 {
		t.Fatalf("%d rules", len(rules))
	}
	ban, chal, office := rules[0].(map[string]any), rules[1].(map[string]any), rules[2].(map[string]any)
	if ban["priority"] != banPriority || ban["id"] != "rul_own" || ban["then"].(map[string]any)["action"] != "block" {
		t.Fatalf("ban rule %v", ban)
	}
	if or := chal["if"].(map[string]any)["or"].([]any); len(or) != 2 {
		t.Fatalf("70 addresses in %d clauses, want 2", len(or))
	}
	// The member's own rules keep their order, moved below the bot's.
	if p := office["priority"]; fmt.Sprint(p) != "3" || office["id"] != "rul_mine" {
		t.Fatalf("office rule %v", office)
	}
	if p := rules[3].(map[string]any)["priority"]; fmt.Sprint(p) != "31" {
		t.Fatalf("last rule priority %v", p)
	}

	// Written back unchanged, there is nothing to do.
	b, _ := json.Marshal(set)
	if _, changed, err := mergeRules(b, many, []string{"192.0.2.1"}); err != nil || changed {
		t.Fatalf("same set: changed %v, err %v", changed, err)
	}
	// With nothing to block, the bot's rules go.
	set, changed, _ = mergeRules(b, nil, nil)
	if !changed || len(set["rules"].([]any)) != 2 {
		t.Fatalf("empty sets: changed %v, rules %v", changed, set["rules"])
	}
	if _, changed, _ := mergeRules(nil, nil, nil); changed {
		t.Fatal("no rules and nothing to block: changed")
	}
}

func TestRailwayEdgeSync(t *testing.T) {
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Project-Access-Token") != "tok" {
			t.Errorf("token header %q", r.Header.Get("Project-Access-Token"))
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		got = append(got, req)
		if strings.HasPrefix(strings.TrimSpace(req["query"].(string)), "query") {
			_, _ = io.WriteString(w, `{"data":{"serviceInstance":{"edgeConfig":{"edgeRules":null}}}}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":{"updateServiceEdgeRules":{"id":"x"}}}`)
	}))
	defer srv.Close()
	e := NewRailwayEdge("tok", "svc", "env")
	e.api = srv.URL
	if err := e.Sync(context.Background(), []string{"192.0.2.2"}, nil); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("%d calls, want a read and a write", len(got))
	}
	in := got[1]["variables"].(map[string]any)["input"].(map[string]any)
	if in["serviceId"] != "svc" || in["environmentId"] != "env" {
		t.Fatalf("input %v", in)
	}
	rules := in["edgeRules"].(map[string]any)["rules"].([]any)
	if len(rules) != 1 || rules[0].(map[string]any)["then"].(map[string]any)["action"] != "challenge" {
		t.Fatalf("rules %v", rules)
	}
}

func TestRailwayEdgeReportsErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"errors":[{"message":"Not Authorized"}]}`)
	}))
	defer srv.Close()
	e := NewRailwayEdge("tok", "svc", "env")
	e.api = srv.URL
	if err := e.Sync(context.Background(), nil, []string{"192.0.2.1"}); err == nil || !strings.Contains(err.Error(), "Not Authorized") {
		t.Fatalf("err %v", err)
	}
}

func TestRailwayEnablerArgs(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "railway")
	script := "#!/bin/sh\necho \"$@\" > " + out + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := RailwayEnabler(bin, "svc-123")(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(out)
	if want := "waf under-attack enable --duration 1h --service svc-123"; strings.TrimSpace(string(got)) != want {
		t.Fatalf("args %q, want %q", got, want)
	}
	if err := RailwayEnabler(filepath.Join(dir, "missing"), "")(context.Background()); err == nil {
		t.Fatal("missing binary should fail")
	}
}
