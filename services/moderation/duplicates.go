package moderation

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/bot"
	"steki-bot/dc"
	"steki-bot/moderation"
	"steki-bot/tokens"
)

// Cross-channel duplicate detection. A hijacked account typically pastes the
// same scam message into channel after channel within seconds, text-only
// messages included, which the media guard alone would miss.
const (
	dupWindow   = time.Minute
	dupChannels = 3
	// dupMinLen ignores short text ("ok", "χαχα") that people legitimately
	// say in several channels; attachments always count.
	dupMinLen = 12
)

type dupPost struct {
	key       string
	channelID string
	messageID string
	at        time.Time
}

// dupTracker keeps each member's posts from the last dupWindow in memory.
// Nothing here needs to survive a restart: the window is a minute long.
type dupTracker struct {
	mu    sync.Mutex
	posts map[string][]dupPost
}

func newDupTracker() *dupTracker {
	return &dupTracker{posts: map[string][]dupPost{}}
}

// add records a post and, when the same content has now appeared in
// dupChannels distinct channels within the window, returns every copy and
// forgets them so the same burst does not trip twice.
func (t *dupTracker) add(userID string, p dupPost) []dupPost {
	t.mu.Lock()
	defer t.mu.Unlock()

	kept := t.posts[userID][:0]
	for _, old := range t.posts[userID] {
		if p.at.Sub(old.at) < dupWindow {
			kept = append(kept, old)
		}
	}
	kept = append(kept, p)

	var same []dupPost
	channels := map[string]bool{}
	for _, q := range kept {
		if q.key == p.key {
			same = append(same, q)
			channels[q.channelID] = true
		}
	}
	if len(channels) < dupChannels {
		t.posts[userID] = kept
		return nil
	}
	t.posts[userID] = slices.DeleteFunc(kept, func(q dupPost) bool { return q.key == p.key })
	return same
}

// sweep drops members whose posts have all aged out.
func (t *dupTracker) sweep(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for user, posts := range t.posts {
		if len(posts) == 0 || now.Sub(posts[len(posts)-1].at) >= dupWindow {
			delete(t.posts, user)
		}
	}
}

// dupKey fingerprints a message by its normalised text and its attachments'
// names and sizes. It returns "" for messages too short to judge.
func dupKey(m *discordgo.Message) string {
	text := strings.Join(strings.Fields(strings.ToLower(moderation.Text(m))), " ")
	attachments := moderation.Attachments(m)
	if len([]rune(text)) < dupMinLen && len(attachments) == 0 {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(text))
	for _, a := range attachments {
		fmt.Fprintf(h, "\x00%s:%d", strings.ToLower(a.Filename), a.Size)
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

// screenDuplicates records the message and, on a cross-channel burst, deletes
// every copy and restricts the member. It reports whether it did.
func (h *Handler) screenDuplicates(s *discordgo.Session, m *discordgo.MessageCreate) bool {
	key := dupKey(m.Message)
	if key == "" {
		return false
	}
	copies := h.dups.add(m.Author.ID, dupPost{key: key, channelID: m.ChannelID, messageID: m.ID, at: time.Now()})
	if copies == nil || h.isModerator(s, m.Author.ID, m.GuildID, m.ChannelID) {
		return false
	}

	ctx, cancel := h.Ctx()
	defer cancel()
	for _, c := range copies {
		err := h.Retry(ctx, "message.delete", func() error {
			return s.ChannelMessageDelete(c.channelID, c.messageID, discordgo.WithContext(ctx))
		})
		if err != nil && !dc.IsMissingResource(err) {
			h.Log.Warn("duplicate: delete failed", "channel_id", c.channelID, "message_id", c.messageID, "error", err)
		}
	}
	h.Log.Info("duplicate: cross-channel burst", "user_id", m.Author.ID, "copies", len(copies))

	tokenID, err := bot.NewTokenID()
	if err != nil {
		h.Log.Error("duplicate: token id", "error", err)
		return true
	}
	if err := h.st.ForceRestriction(ctx, m.Author.ID, m.GuildID, tokenID, time.Now()); err != nil {
		h.Log.Error("duplicate: record restriction", "user_id", m.Author.ID, "error", err)
		return true
	}
	_, _ = h.restrict(s, m.Author.ID, m.GuildID, m.ChannelID, tokens.ReasonDuplicate, tokenID, "")
	return true
}
