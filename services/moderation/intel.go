package moderation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
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
// restricts the sender at once. Recognising a repeat costs no download: a file
// is told by its name and size, and only a message that matches a feed entry,
// or repeats, is downloaded, to hash it. An admin can take an entry out from the card
// that announced it.
const (
	// intelMaxFile bounds what is downloaded to hash an attachment.
	intelMaxFile       = 8 << 20
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
	key   string // the normalised link, or a file's lower-case name and size
	label string
	file  *discordgo.MessageAttachment // for media
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

// fileKey names a file by what the message already says about it, so nothing
// has to be downloaded to recognise a repeat. Two files with the same name and
// byte size sent by one member within minutes are the same file.
func fileKey(a *discordgo.MessageAttachment) string {
	return fmt.Sprintf("%s:%d", strings.ToLower(a.Filename), a.Size)
}

// fromDiscordCDN reports whether raw is an attachment URL worth downloading.
func fromDiscordCDN(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && slices.Contains(cdnHosts, u.Hostname())
}

// readFile reads a file, or reports false when it is too large.
func readFile(r io.Reader) ([]byte, bool) {
	data, err := io.ReadAll(io.LimitReader(r, intelMaxFile+1))
	return data, err == nil && len(data) <= intelMaxFile
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// download fetches an attachment from Discord's CDN. It is for the rare
// message already suspected, never for every message.
func (h *Handler) download(ctx context.Context, a *discordgo.MessageAttachment) ([]byte, bool) {
	if a.Size > intelMaxFile || !fromDiscordCDN(a.URL) {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, intelFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return nil, false
	}
	resp, err := h.cdn.Do(req)
	if err != nil {
		h.Log.Debug("intel: download failed", "error", err)
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	return readFile(resp.Body)
}

// itemsOf lists a message's distinct images and links. It makes no request.
func (h *Handler) itemsOf(m *discordgo.Message) []spamItem {
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
		add(spamItem{kind: store.IntelMedia, key: fileKey(a), label: a.Filename, file: a})
	}
	return items
}

// known finds the feed's entry for an item. A file is a candidate by name and
// size; only then is it downloaded, to compare its hash with the entry's.
func (h *Handler) known(ctx context.Context, it spamItem) (store.IntelEntry, bool) {
	if it.kind == store.IntelLink {
		e, err := h.st.MatchIntel(ctx, it.kind, it.key)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			h.Log.Error("intel: match", "error", err)
		}
		return e, err == nil
	}
	cands, err := h.st.IntelByQuick(ctx, it.key)
	if err != nil {
		h.Log.Error("intel: match", "error", err)
		return store.IntelEntry{}, false
	}
	if len(cands) == 0 {
		return store.IntelEntry{}, false
	}
	data, ok := h.download(ctx, it.file)
	if !ok {
		return store.IntelEntry{}, false
	}
	hash := sha(data)
	for _, c := range cands {
		if c.Key == hash {
			return c, true
		}
	}
	return store.IntelEntry{}, false
}

// learn adds a repeated item to the feed and announces it. A file is
// downloaded now, while its message still exists, so the card can show it.
func (h *Handler) learn(ctx context.Context, s *discordgo.Session, it spamItem, userID string) {
	key, quick := it.key, ""
	var shown *discordgo.File
	if it.kind == store.IntelMedia {
		data, ok := h.download(ctx, it.file)
		if !ok {
			return
		}
		key, quick = sha(data), it.key
		if strings.HasPrefix(it.file.ContentType, "image/") {
			shown = &discordgo.File{Name: "intel" + fileExt(it.file.Filename), Reader: bytes.NewReader(data)}
		}
	}
	id, added, err := h.st.AddIntel(ctx, it.kind, key, quick, it.label, userID)
	if err != nil {
		h.Log.Error("intel: add", "error", err)
		return
	}
	if !added {
		return
	}
	h.Log.Info("intel: added", "intel_id", id, "kind", it.kind, "user_id", userID)
	embed, components := intelCard(id, it, userID, shown != nil)
	if shown != nil {
		embed.Image = &discordgo.MessageEmbedImage{URL: "attachment://" + shown.Name}
		h.Modlog.PostCard(s, embed, components, shown)
		return
	}
	h.Modlog.PostCard(s, embed, components)
}

// fileExt is a file name's extension if it is a plain one, so the name the
// card's image is attached under is never member-controlled.
func fileExt(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if len(ext) < 2 || len(ext) > 6 || strings.Trim(ext[1:], "abcdefghijklmnopqrstuvwxyz0123456789") != "" {
		return ".png"
	}
	return ext
}

// screenIntel removes a message that carries an item from the feed, or that
// repeats an image or link its author just sent, and restricts the author. A
// repeat is added to the feed. It reports whether it removed anything.
func (h *Handler) screenIntel(s *discordgo.Session, m *discordgo.MessageCreate) bool {
	if !flaggedPost(m.Message) || h.isModerator(s, m.Author.ID, m.GuildID, m.ChannelID) {
		return false
	}
	items := h.itemsOf(m.Message)
	if len(items) == 0 {
		return false
	}
	ctx, cancel := h.CtxTimeout(intelScreenTimeout)
	defer cancel()
	self := dupPost{channelID: m.ChannelID, messageID: m.ID}

	for _, it := range items {
		e, ok := h.known(ctx, it)
		if !ok {
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
	var repeated []spamItem
	for _, it := range items {
		p := dupPost{key: it.trackKey(), channelID: m.ChannelID, messageID: m.ID, at: time.Now()}
		if c := h.repeats.add(m.Author.ID, p); c != nil {
			copies = append(copies, c...)
			repeated = append(repeated, it)
		}
	}
	if copies == nil {
		return false
	}
	// Files are downloaded before anything is deleted, since Discord stops
	// serving a file once its message is gone.
	for _, it := range repeated {
		h.learn(ctx, s, it, m.Author.ID)
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
func intelCard(id int64, it spamItem, userID string, hasImage bool) (*discordgo.MessageEmbed, []discordgo.MessageComponent) {
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
