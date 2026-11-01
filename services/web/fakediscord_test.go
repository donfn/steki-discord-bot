package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/config"
	"steki-bot/store"
)

// A fake Discord for the tests that run a whole pass (reconcile, the commands)
// against a real database. discordgo sends its REST calls through
// Session.Client, so pointing that at this transport is all it takes: no
// network, and the pass sees the guild the test describes.
//
// It knows only the calls the passes make. Anything else fails the test, so a
// pass that starts calling Discord somewhere new says so.

const testGuild = "g1"

type fakeDiscord struct {
	t  *testing.T
	mu sync.Mutex

	members  []map[string]any
	channels map[string]map[string]any   // by ID, categories included
	messages map[string][]map[string]any // by channel ID, newest first
	calls    []string                    // "METHOD /path", in order
	bodies   map[string][]string         // "METHOD /path" -> request bodies
	status   map[string]int              // "METHOD /path" -> forced status
	counter  int64
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	f := &fakeDiscord{
		t: t, channels: map[string]map[string]any{}, messages: map[string][]map[string]any{},
		bodies: map[string][]string{}, status: map[string]int{},
	}
	// The category register channels are made in.
	f.channels[config.RegisterCategoryID] = map[string]any{
		"id": config.RegisterCategoryID, "type": int(discordgo.ChannelTypeGuildCategory),
		"guild_id": testGuild, "name": "REGISTER",
	}
	return f
}

// oldID is a channel ID from before any pass in the test began.
func (f *fakeDiscord) oldID() string {
	f.counter++
	return fmt.Sprint(mustInt(snowflakeAt(time.Now().Add(-48*time.Hour))) + f.counter)
}

// newID is an ID Discord would hand out now.
func (f *fakeDiscord) newID() string {
	f.counter++
	return fmt.Sprint(mustInt(snowflakeAt(time.Now())) + f.counter)
}

func mustInt(s string) int64 {
	var n int64
	if _, err := fmt.Sscan(s, &n); err != nil {
		panic(err)
	}
	return n
}

// addMember puts a member in the guild.
func (f *fakeDiscord) addMember(id string, joined time.Time, roles ...string) {
	if roles == nil {
		roles = []string{}
	}
	f.members = append(f.members, map[string]any{
		"user":      map[string]any{"id": id, "username": "u" + id, "bot": false},
		"roles":     roles,
		"joined_at": joined.UTC().Format(time.RFC3339),
	})
}

// addBot puts a bot in the guild.
func (f *fakeDiscord) addBot(id string) {
	f.members = append(f.members, map[string]any{
		"user": map[string]any{"id": id, "username": "bot" + id, "bot": true}, "roles": []string{},
		"joined_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	})
}

// addChannel puts an existing text channel under the register category.
func (f *fakeDiscord) addChannel(name string) string {
	id := f.oldID()
	f.channels[id] = map[string]any{
		"id": id, "type": int(discordgo.ChannelTypeGuildText), "guild_id": testGuild,
		"name": name, "parent_id": config.RegisterCategoryID,
	}
	return id
}

// count is how many calls matched "METHOD /path"; a * in the pattern stands
// for one path segment.
func (f *fakeDiscord) count(pattern string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if ok, _ := path.Match(pattern, c); ok {
			n++
		}
	}
	return n
}

// created lists the names of the channels the pass made.
func (f *fakeDiscord) created() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for _, b := range f.bodies["POST /guilds/"+testGuild+"/channels"] {
		var in struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal([]byte(b), &in)
		names = append(names, in.Name)
	}
	return names
}

var apiPrefix = regexp.MustCompile(`^/api/v\d+`)

// RoundTrip implements http.RoundTripper.
func (f *fakeDiscord) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	route := apiPrefix.ReplaceAllString(r.URL.Path, "")
	key := r.Method + " " + route
	f.calls = append(f.calls, key)
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		f.bodies[key] = append(f.bodies[key], string(b))
	}

	reply := func(code int, v any) (*http.Response, error) {
		var body []byte
		if v != nil {
			body, _ = json.Marshal(v)
		}
		return &http.Response{
			StatusCode: code, Status: fmt.Sprint(code), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
			Header:  http.Header{"Content-Type": []string{"application/json"}},
			Body:    io.NopCloser(bytes.NewReader(body)),
			Request: r,
		}, nil
	}
	if code, ok := f.status[key]; ok {
		return reply(code, map[string]any{"code": 50013, "message": "forced by the test"})
	}

	parts := strings.Split(strings.Trim(route, "/"), "/")
	switch {
	case key == "GET /guilds/"+testGuild+"/members":
		if r.URL.Query().Get("after") != "" {
			return reply(200, []any{}) // one page holds every member in these tests
		}
		return reply(200, f.members)

	case key == "GET /guilds/"+testGuild+"/channels":
		out := make([]map[string]any, 0, len(f.channels))
		for _, ch := range f.channels {
			out = append(out, ch)
		}
		return reply(200, out)

	case key == "POST /guilds/"+testGuild+"/channels":
		var in map[string]any
		_ = json.Unmarshal([]byte(f.bodies[key][len(f.bodies[key])-1]), &in)
		id := f.newID()
		ch := map[string]any{"id": id, "type": in["type"], "guild_id": testGuild, "name": in["name"], "parent_id": in["parent_id"]}
		f.channels[id] = ch
		return reply(201, ch)

	case len(parts) == 2 && parts[0] == "channels" && r.Method == http.MethodGet:
		if ch, ok := f.channels[parts[1]]; ok {
			return reply(200, ch)
		}
		return reply(404, map[string]any{"code": 10003, "message": "Unknown Channel"})

	case len(parts) == 2 && parts[0] == "channels" && r.Method == http.MethodDelete:
		ch, ok := f.channels[parts[1]]
		if !ok {
			return reply(404, map[string]any{"code": 10003, "message": "Unknown Channel"})
		}
		delete(f.channels, parts[1])
		return reply(200, ch)

	case len(parts) == 3 && parts[0] == "channels" && parts[2] == "messages" && r.Method == http.MethodGet:
		return reply(200, append([]map[string]any{}, f.messages[parts[1]]...))

	case len(parts) == 4 && parts[0] == "channels" && parts[2] == "messages" && r.Method == http.MethodPatch:
		return reply(200, map[string]any{"id": parts[3], "channel_id": parts[1]})

	case len(parts) == 3 && parts[0] == "channels" && parts[2] == "messages" && r.Method == http.MethodPost:
		return reply(200, map[string]any{"id": f.newID(), "channel_id": parts[1]})

	case key == "POST /users/@me/channels":
		return reply(200, map[string]any{"id": f.newID(), "type": int(discordgo.ChannelTypeDM)})

	case len(parts) == 5 && parts[0] == "webhooks" && parts[3] == "messages" && r.Method == http.MethodPatch:
		return reply(200, map[string]any{"id": "1", "channel_id": "1"})

	case len(parts) == 4 && parts[0] == "channels" && parts[2] == "permissions":
		return reply(204, nil)

	case len(parts) == 5 && parts[0] == "applications" && parts[2] == "guilds" && parts[4] == "commands" && r.Method == http.MethodPut:
		return reply(200, []any{})
	}
	f.t.Errorf("the fake Discord has no answer for %s", key)
	return reply(500, nil)
}

// harness is a handler wired to a real store and the fake Discord.
type harness struct {
	t  *testing.T
	h  *Handler
	s  *discordgo.Session
	st *store.Library
	fd *fakeDiscord
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	st := store.OpenTestLibrary(t)
	fd := newFakeDiscord(t)
	s, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	s.Client = &http.Client{Transport: fd}
	s.MaxRestRetries = 0

	cfg := &config.Web{Common: config.Common{InternalSecret: strings.Repeat("k", 32)}, JWTSecret: strings.Repeat("j", 32),
		PublicURL: "https://steki.example"}
	cfg.Bucket = config.Bucket{Endpoint: "http://127.0.0.1:1", Name: "b", AccessKeyID: "k", SecretAccessKey: "s"}
	h, err := NewHandler(cfg, st, nil, discardLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Stop)
	h.SetGuild(testGuild)
	return &harness{t: t, h: h, s: s, st: st, fd: fd}
}

// say puts a message from the bot, or from a member, in a channel.
func (f *fakeDiscord) say(channelID, id, authorID string, embedTitle string, linkButton bool) {
	msg := map[string]any{
		"id": id, "channel_id": channelID,
		"author": map[string]any{"id": authorID, "username": "a" + authorID},
	}
	if embedTitle != "" {
		msg["embeds"] = []map[string]any{{"title": embedTitle}}
	}
	if linkButton {
		msg["components"] = []map[string]any{{"type": 1, "components": []map[string]any{
			{"type": 2, "style": 5, "label": "old", "url": "https://old.example/link"},
		}}}
	} else if embedTitle != "" {
		msg["components"] = []map[string]any{{"type": 1, "components": []map[string]any{
			{"type": 2, "style": 1, "label": "new", "custom_id": mcpGuideButtonID},
		}}}
	}
	f.messages[channelID] = append(f.messages[channelID], msg)
}
