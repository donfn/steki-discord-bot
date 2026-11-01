package moderation

import (
	"sync"

	"github.com/bwmarrin/discordgo"
)

const (
	// msgCacheSize is how many recent messages the edit and delete logs can
	// look back over, across all channels. It bounds memory by message count
	// rather than channel count, which is what discordgo's own per-channel
	// cache does not.
	msgCacheSize = 1500
	// msgCacheMaxContent caps the text kept per message. A longer message is
	// still reported, cut off.
	msgCacheMaxContent = 1500
)

// cachedMsg is the little of a message the edit and delete logs need.
type cachedMsg struct {
	id, authorID string
	bot          bool
	content      string
}

// msgCache remembers the most recent messages in a fixed ring, so an edit or
// a deletion can be reported with what the message said before.
type msgCache struct {
	mu   sync.Mutex
	ring []cachedMsg
	at   map[string]int // message ID -> slot in ring
	next int
}

func newMsgCache(size int) *msgCache {
	return &msgCache{ring: make([]cachedMsg, size), at: make(map[string]int, size)}
}

// put records a new message, evicting the oldest once the ring is full.
func (c *msgCache) put(m *discordgo.Message) {
	if m == nil || m.Author == nil || m.ID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old := c.ring[c.next]; old.id != "" {
		delete(c.at, old.id)
	}
	c.ring[c.next] = cachedMsg{
		id:       m.ID,
		authorID: m.Author.ID,
		bot:      m.Author.Bot,
		content:  clip(m.Content, msgCacheMaxContent),
	}
	c.at[m.ID] = c.next
	c.next = (c.next + 1) % len(c.ring)
}

// edit swaps in a message's new content and returns what it held before.
func (c *msgCache) edit(id, content string) (cachedMsg, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	slot, ok := c.at[id]
	if !ok {
		return cachedMsg{}, false
	}
	before := c.ring[slot]
	c.ring[slot].content = clip(content, msgCacheMaxContent)
	return before, true
}

// take removes and returns a message, for a deletion.
func (c *msgCache) take(id string) (cachedMsg, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	slot, ok := c.at[id]
	if !ok {
		return cachedMsg{}, false
	}
	m := c.ring[slot]
	c.ring[slot] = cachedMsg{}
	delete(c.at, id)
	return m, true
}

// clip cuts s to at most n bytes without splitting a UTF-8 sequence.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
