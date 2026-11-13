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

	"steki-bot/internal/bot"
	"steki-bot/internal/dc"
	"steki-bot/internal/moderation"
	"steki-bot/internal/tokens"
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

	// Repeats of one image or link. A second copy within the window is spam:
	// the media guard already restricts a member's second attachment or link in
	// an hour, so this only adds removing what they sent.
	repeatWindow   = 5 * time.Minute
	repeatMessages = 2
)

type dupPost struct {
	key       string
	channelID string
	messageID string
	at        time.Time
}

// dupTracker keeps each member's recent posts in memory. Nothing here needs to
// survive a restart: the windows are minutes long.
type dupTracker struct {
	window time.Duration
	// min distinct values of spread among posts of one key trip the tracker.
	min    int
	spread func(dupPost) string

	mu    sync.Mutex
	posts map[string][]dupPost
}

// newDupTracker trips on the same message in dupChannels channels.
func newDupTracker() *dupTracker {
	return &dupTracker{window: dupWindow, min: dupChannels,
		spread: func(p dupPost) string { return p.channelID }, posts: map[string][]dupPost{}}
}

// newRepeatTracker trips on the same image or link in repeatMessages messages,
// in one channel or several.
func newRepeatTracker() *dupTracker {
	return &dupTracker{window: repeatWindow, min: repeatMessages,
		spread: func(p dupPost) string { return p.messageID }, posts: map[string][]dupPost{}}
}

// add records a post and, when the same content has now reached the tracker's
// minimum spread within the window, returns every copy and forgets them so the
// same burst does not trip twice.
func (t *dupTracker) add(userID string, p dupPost) []dupPost {
	t.mu.Lock()
	defer t.mu.Unlock()

	kept := t.posts[userID][:0]
	for _, old := range t.posts[userID] {
		if p.at.Sub(old.at) < t.window {
			kept = append(kept, old)
		}
	}
	kept = append(kept, p)

	var same []dupPost
	spread := map[string]bool{}
	for _, q := range kept {
		if q.key == p.key {
			same = append(same, q)
			spread[t.spread(q)] = true
		}
	}
	if len(spread) < t.min {
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
		if len(posts) == 0 || now.Sub(posts[len(posts)-1].at) >= t.window {
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

	h.Log.Info("duplicate: cross-channel burst", "user_id", m.Author.ID, "copies", len(copies))
	h.punishSpam(s, m, copies, tokens.ReasonDuplicate)
	return true
}

// punishSpam deletes every copy of a spam message and restricts the member
// until they pass the captcha.
//
// Several copies often arrive at once. They queue here, and once one has
// restricted the member the rest are only deleted: one restriction, one DM.
func (h *Handler) punishSpam(s *discordgo.Session, m *discordgo.MessageCreate, copies []dupPost, reason string) {
	defer h.LockUser(m.Author.ID)()
	ctx, cancel := h.Ctx()
	defer cancel()
	for _, c := range copies {
		err := h.Retry(ctx, "message.delete", func() error {
			return s.ChannelMessageDelete(c.channelID, c.messageID, discordgo.WithContext(ctx))
		})
		if err != nil && !dc.IsMissingResource(err) {
			h.Log.Warn("spam: delete failed", "channel_id", c.channelID, "message_id", c.messageID, "error", err)
		}
	}

	if g, err := h.st.GuardStatus(ctx, m.Author.ID); err == nil && g.LinkPending &&
		g.RestrictedAt != nil && time.Since(*g.RestrictedAt) < guardTimeout {
		return
	}

	tokenID, err := bot.NewTokenID()
	if err != nil {
		h.Log.Error("spam: token id", "error", err)
		return
	}
	if err := h.st.ForceRestriction(ctx, m.Author.ID, m.GuildID, tokenID, time.Now()); err != nil {
		h.Log.Error("spam: record restriction", "user_id", m.Author.ID, "error", err)
		return
	}
	_, _ = h.restrict(s, m.Author.ID, m.GuildID, m.ChannelID, reason, tokenID, "")
}
