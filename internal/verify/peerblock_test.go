package verify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func testPeerBlock(addrs ...string) *PeerBlock {
	p := NewPeerBlock("http://blastdoor.railway.internal:8080", slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.lookup = func(context.Context, string) ([]netip.Addr, error) {
		var out []netip.Addr
		for _, a := range addrs {
			out = append(out, netip.MustParseAddr(a))
		}
		return out, nil
	}
	return p
}

func TestPeerBlockRefusesTheService(t *testing.T) {
	p := testPeerBlock("fd12::5", "10.0.0.7", "127.0.0.1")
	if err := p.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	reached := false
	h := p.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	for remote, want := range map[string]int{
		"[fd12::5]:4000":         http.StatusForbidden,
		"10.0.0.7:4000":          http.StatusForbidden,
		"[::ffff:10.0.0.7]:4000": http.StatusForbidden,
		"10.0.0.8:4000":          http.StatusOK,
		"127.0.0.1:4000":         http.StatusOK,
		"not an address":         http.StatusOK,
	} {
		reached = false
		r := httptest.NewRequest(http.MethodGet, "/verify", nil)
		r.RemoteAddr = remote
		r.Header.Set("X-Forwarded-For", "203.0.113.9")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want || reached != (want == http.StatusOK) {
			t.Errorf("%s: status %d, reached %v, want %d", remote, w.Code, reached, want)
		}
	}
}

func TestPeerBlockKeepsAddressesWhenLookupFails(t *testing.T) {
	p := testPeerBlock("10.0.0.7")
	if err := p.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	p.lookup = func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no dns") }
	if err := p.refresh(t.Context()); err == nil {
		t.Fatal("lookup error was not reported")
	}
	if !p.blocked("10.0.0.7:1") {
		t.Error("a failed lookup dropped the block")
	}
}

func TestPeerBlockFollowsARedeploy(t *testing.T) {
	p := testPeerBlock("10.0.0.7")
	if err := p.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	p.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("10.0.0.9")}, nil
	}
	if err := p.refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if p.blocked("10.0.0.7:1") || !p.blocked("10.0.0.9:1") {
		t.Error("addresses were not replaced")
	}
}

func TestPeerBlockWithoutAURLBlocksNothing(t *testing.T) {
	p := NewPeerBlock("", slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.Run(t.Context()) // returns at once
	if p.blocked("10.0.0.7:1") {
		t.Error("blocked with no service")
	}
}
