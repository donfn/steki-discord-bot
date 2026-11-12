package moderation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/internal/bot"
	"steki-bot/internal/moderation"
	"steki-bot/internal/store"
	"steki-bot/internal/tokens"
)

// The intelligence feed. A hijacked account sends the same image or link over
// and over, and the next hijacked account sends the same ones again. An item
// sent twice by one member is taken for spam, removed, and remembered in the
// feed; after that, sending it from any account removes the message and
// restricts the sender at once. An admin can take an entry out from the card
// that announced it.
const (
	// intelMaxFile bounds what is downloaded to fingerprint an attachment.
	intelMaxFile = 8 << 20
	// intelFetchers bounds how many attachments are downloaded at once.
	intelFetchers      = 3
	intelFetchTimeout  = 15 * time.Second
	intelScreenTimeout = time.Minute

	intelPrefix       = "intel:"
	intelRemovePrefix = "intel:remove:"
	intelColor        = 0xED4245
	intelRemovedColor = 0x2B2D31
)

// intelSafeHosts are never learned: they are shared by everyone, so one member
// repeating a link to them is not a sign of an attack.
var intelSafeHosts = []string{
	"discord.com", "discord.gg", "discordapp.com", "discordapp.net",
	"youtube.com", "youtu.be", "github.com", "google.com", "wikipedia.org",
	"tenor.com", "giphy.com",
}

// cdnHosts are where Discord keeps attachments. Only these are downloaded, so
// a crafted attachment URL cannot make the bot fetch anything else.
var cdnHosts = []string{"cdn.discordapp.com", "media.discordapp.net"}

// spamItem is one image or link of a message.
type spamItem struct {
	kind  string // store.IntelMedia or store.IntelLink
	key   string // the file's SHA-256, or the normalised link
	label string
	image string // where an admin can see it, for images
}

func (it spamItem) trackKey() string { return it.kind + ":" + it.key }

// linkKey normalises a link so the same page written two ways is one item. It
// reports false for links that must not be learned.
func linkKey(raw, ownHost string) (string, bool) {
	raw = strings.TrimRight(raw, ".,;:!?)]}")
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	if host == "" || host == ownHost {
		return "", false
	}
	for _, safe := range intelSafeHosts {
		if host == safe || strings.HasSuffix(host, "."+safe) {
			return "", false
		}
	}
	key := host + strings.TrimRight(u.EscapedPath(), "/")
	if u.RawQuery != "" {
		key += "?" + u.RawQuery
	}
	return key, true
}

// fromDiscordCDN reports whether raw is an attachment URL worth downloading.
func fromDiscordCDN(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && slices.Contains(cdnHosts, u.Hostname())
}

// hashBody fingerprints a file, or reports false when it is too large.
func hashBody(r io.Reader) (string, bool) {
	sum := sha256.New()
	n, err := io.Copy(sum, io.LimitReader(r, intelMaxFile+1))
	if err != nil || n > intelMaxFile {
		return "", false
	}
	return hex.EncodeToString(sum.Sum(nil)), true
}

// fingerprint downloads an attachment and hashes it. An attachment that
// cannot be fetched is simply not tracked, never guessed at.
func (h *Handler) fingerprint(ctx context.Context, a *discordgo.MessageAttachment) (string, bool) {
	if a.Size > intelMaxFile || !fromDiscordCDN(a.URL) {
		return "", false
	}
	select {
	case h.fetch <- struct{}{}:
		defer func() { <-h.fetch }()
	case <-ctx.Done():
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, intelFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return "", false
	}
	resp, err := h.cdn.Do(req)
	if err != nil {
		h.Log.Debug("intel: download failed", "error", err)
		return "", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	return hashBody(resp.Body)
}

// itemsOf lists a message's distinct images and links.
func (h *Handler) itemsOf(ctx context.Context, m *discordgo.Message) []spamItem {
	var own string
	if u, err := url.Parse(h.cfg.PublicURL); err == nil {
		own = strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	}
	var items []spamItem
	add := func(it spamItem) {
		if !slices.ContainsFunc(items, func(o spamItem) bool { return o.trackKey() == it.trackKey() }) {
			items = append(items, it)
		}
	}
	for _, raw := range moderation.URLs(moderation.Text(m)) {
		if key, ok := linkKey(raw, own); ok {
			add(spamItem{kind: store.IntelLink, key: key, label: key})
		}
	}
	for _, a := range moderation.Attachments(m) {
		if key, ok := h.fingerprint(ctx, a); ok {
			it := spamItem{kind: store.IntelMedia, key: key, label: a.Filename}
			if strings.HasPrefix(a.ContentType, "image/") {
				it.image = a.URL
			}
			add(it)
		}
	}
	return items
}

// screenIntel removes a message that carries an item from the feed, or that
// repeats an image or link its author just sent, and restricts the author. A
// repeat is added to the feed. It reports whether it removed anything.
func (h *Handler) screenIntel(s *discordgo.Session, m *discordgo.MessageCreate) bool {
	if !flaggedPost(m.Message) || h.isModerator(s, m.Author.ID, m.GuildID, m.ChannelID) {
		return false
	}
	ctx, cancel := h.CtxTimeout(intelScreenTimeout)
	defer cancel()
	items := h.itemsOf(ctx, m.Message)
	if len(items) == 0 {
		return false
	}
	self := dupPost{channelID: m.ChannelID, messageID: m.ID}

	for _, it := range items {
		e, err := h.st.MatchIntel(ctx, it.kind, it.key)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			h.Log.Error("intel: match", "error", err)
			continue
		}
		if err := h.st.CountIntelHit(ctx, e.ID); err != nil {
			h.Log.Warn("intel: count hit", "id", e.ID, "error", err)
		}
		h.Log.Info("intel: known item", "user_id", m.Author.ID, "intel_id", e.ID)
		h.punishSpam(s, m, []dupPost{self}, tokens.ReasonSpam)
		return true
	}

	var copies []dupPost
	for _, it := range items {
		p := dupPost{key: it.trackKey(), channelID: m.ChannelID, messageID: m.ID, at: time.Now()}
		repeated := h.repeats.add(m.Author.ID, p)
		if repeated == nil {
			continue
		}
		copies = append(copies, repeated...)
		id, added, err := h.st.AddIntel(ctx, it.kind, it.key, it.label, m.Author.ID)
		if err != nil {
			h.Log.Error("intel: add", "error", err)
			continue
		}
		if added {
			h.Log.Info("intel: added", "intel_id", id, "kind", it.kind, "user_id", m.Author.ID)
			// The card goes out before the copies are deleted, so Discord has
			// already rendered the image it shows.
			embed, components := intelCard(id, it, m.Author.ID)
			h.Modlog.PostCard(s, embed, components)
		}
	}
	if copies == nil {
		return false
	}
	seen := map[string]bool{}
	copies = slices.DeleteFunc(copies, func(c dupPost) bool {
		dup := seen[c.messageID]
		seen[c.messageID] = true
		return dup
	})
	h.punishSpam(s, m, copies, tokens.ReasonSpam)
	return true
}

// intelCard announces a new entry, with the button that takes it out again.
func intelCard(id int64, it spamItem, userID string) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
	what := "Σύνδεσμος"
	if it.kind == store.IntelMedia {
		what = "Αρχείο"
	}
	embed := &discordgo.MessageEmbed{
		Title: "🕵️ Νέα καταχώριση στο intel feed",
		Color: intelColor,
		Description: fmt.Sprintf("Ο χρήστης <@%s> έστειλε το ίδιο περιεχόμενο ξανά και ξανά, οπότε τα μηνύματα διαγράφηκαν "+
			"και ο χρήστης μπήκε σε περιορισμό. Από εδώ και πέρα, κάθε μήνυμα που το περιέχει διαγράφεται "+
			"και βάζει τον αποστολέα σε περιορισμό.", userID),
		Fields: []*discordgo.MessageEmbedField{
			{Name: what, Value: "`" + moderation.Quote(clip(it.label, 200)) + "`"},
		},
	}
	if it.image != "" {
		embed.Image = &discordgo.MessageEmbedImage{URL: it.image}
	}
	return embed, intelButtons(id, false)
}

func intelButtons(id int64, disabled bool) []discordgo.MessageComponent {
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{
			Label:    "Αφαίρεση από το intel feed",
			Style:    discordgo.DangerButton,
			CustomID: intelRemovePrefix + strconv.FormatInt(id, 10),
			Disabled: disabled,
		},
	}}}
}

// onIntelComponent takes an entry out of the feed when staff press the button
// on its card.
func (h *Handler) onIntelComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if !h.InHomeGuild(i.GuildID) {
		return
	}
	if i.Member == nil || !bot.HasAnyRole(i.Member, []string{bot.StaffRoleID, bot.ModRoleID}) {
		h.RespondNow(s, i, "⛔ Η ενέργεια αυτή είναι μόνο για την Ομάδα Διαχείρισης.")
		return
	}
	idText, ok := strings.CutPrefix(i.MessageComponentData().CustomID, intelRemovePrefix)
	id, err := strconv.ParseInt(idText, 10, 64)
	if !ok || err != nil {
		return
	}

	ctx, cancel := h.Ctx()
	defer cancel()
	by := bot.ActorID(i)
	e, err := h.st.RemoveIntel(ctx, id)
	switch {
	case err == nil:
		h.Audit(e.UserID, "", store.EventModAction,
			map[string]any{"action": "intel_remove", "kind": e.Kind, "label": e.Label}, by)
		h.Log.Info("intel: removed", "intel_id", id, "by", by)
	case !errors.Is(err, store.ErrNotFound):
		h.Log.Error("intel: remove", "intel_id", id, "error", err)
		h.RespondNow(s, i, fmt.Sprintf("Σφάλμα βάσης: %v", err))
		return
	}

	if i.Message == nil || len(i.Message.Embeds) == 0 {
		h.RespondNow(s, i, "✅ Αφαιρέθηκε από το intel feed.")
		return
	}
	embed := *i.Message.Embeds[0]
	embed.Color = intelRemovedColor
	embed.Fields = append(slices.Clone(embed.Fields),
		&discordgo.MessageEmbedField{Name: "Αφαιρέθηκε", Value: "από τον/την <@" + by + ">"})
	err = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Embeds:     []*discordgo.MessageEmbed{&embed},
			Components: intelButtons(id, true),
		},
	})
	if err != nil {
		h.Log.Error("intel: update card", "error", err)
	}
}
