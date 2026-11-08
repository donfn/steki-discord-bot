package rpc

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"steki-bot/internal/tokens"
)

const testSecret = "0123456789abcdef0123456789abcdef"

func testServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	s := NewServer(testSecret, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, srv
}

func TestCallReachesTheHandler(t *testing.T) {
	s, srv := testServer(t)
	Handle(s, PathForget, func(_ context.Context, in UserRequest) (StatsAnswer, error) {
		return StatsAnswer{Text: "forgot " + in.UserID}, nil
	})
	var out StatsAnswer
	if err := NewClient(srv.URL, testSecret).Call(context.Background(), PathForget, UserRequest{UserID: "u1"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.Text != "forgot u1" {
		t.Fatalf("got %q", out.Text)
	}
}

// Anything else on the private network must be refused, blastdoor above all.
func TestUnsignedAndForgedRequestsAreRefused(t *testing.T) {
	s, srv := testServer(t)
	called := false
	Handle(s, PathForget, func(context.Context, UserRequest) (None, error) { called = true; return None{}, nil })

	post := func(at, sig, body string) int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+PathForget, strings.NewReader(body))
		req.Header.Set(headerTime, at)
		req.Header.Set(headerSig, sig)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	now := strconv.FormatInt(time.Now().Unix(), 10)
	key := requestKey(testSecret)
	body := `{"user_id":"u1"}`
	old := strconv.FormatInt(time.Now().Add(-2*window).Unix(), 10)

	for name, code := range map[string]int{
		"no signature":   post(now, "", body),
		"wrong key":      post(now, sign(requestKey("another secret"), PathForget, now, []byte(body)), body),
		"other path":     post(now, sign(key, PathStats, now, []byte(body)), body),
		"changed body":   post(now, sign(key, PathForget, now, []byte(`{"user_id":"u2"}`)), body),
		"replayed later": post(old, sign(key, PathForget, old, []byte(body)), body),
		"no time at all": post("", sign(key, PathForget, "", []byte(body)), body),
	} {
		if code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, code)
		}
	}
	if called {
		t.Fatal("a refused request reached the handler")
	}
	if code := post(now, sign(key, PathForget, now, []byte(body)), body); code != http.StatusOK || !called {
		t.Fatalf("a correctly signed request got %d", code)
	}
}

// The link page tells these two apart, so they must survive the trip.
func TestLinkErrorsCrossTheWire(t *testing.T) {
	s, srv := testServer(t)
	Handle(s, PathLinkAccept, func(context.Context, ClaimsRequest) (None, error) { return None{}, tokens.ErrLinkUsed })
	Handle(s, PathLinkPending, func(context.Context, ClaimsRequest) (None, error) { return None{}, tokens.ErrEmailTaken })
	Handle(s, PathStats, func(context.Context, None) (None, error) { return None{}, errors.New("boom") })
	c := NewClient(srv.URL, testSecret)
	ctx := context.Background()
	if err := c.Call(ctx, PathLinkAccept, ClaimsRequest{}, nil); !errors.Is(err, tokens.ErrLinkUsed) {
		t.Errorf("got %v", err)
	}
	if err := c.Call(ctx, PathLinkPending, ClaimsRequest{}, nil); !errors.Is(err, tokens.ErrEmailTaken) {
		t.Errorf("got %v", err)
	}
	if err := c.Call(ctx, PathStats, None{}, nil); err == nil || errors.Is(err, tokens.ErrLinkUsed) {
		t.Errorf("got %v", err)
	}
}

func TestNilClientFailsCleanly(t *testing.T) {
	var c *Client
	if err := c.Call(context.Background(), PathStats, None{}, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	if NewClient("", testSecret) != nil {
		t.Fatal("an empty URL should give a nil client")
	}
}

func TestClaimsSurviveTheTrip(t *testing.T) {
	s, srv := testServer(t)
	issuer := tokens.NewIssuer(testSecret)
	web := tokens.New("j"+testSecret, testSecret)
	raw, err := issuer.IssueLink("u1", "g1", "link1", time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	claims, err := web.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	var got tokens.Claims
	Handle(s, PathLinkAccept, func(_ context.Context, in ClaimsRequest) (None, error) { got = in.Claims; return None{}, nil })
	if err := NewClient(srv.URL, testSecret).Call(context.Background(), PathLinkAccept, ClaimsRequest{Claims: *claims}, nil); err != nil {
		t.Fatal(err)
	}
	if got.Subject != "u1" || got.GuildID != "g1" || got.ID != "link1" || got.Reason != tokens.ReasonLink || got.ExpiresAt == nil {
		t.Fatalf("claims changed on the way: %+v", got)
	}
}
