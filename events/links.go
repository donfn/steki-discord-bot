package events

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// The buttons on the stickies and the MCP announcements carry no link. A click
// answers, to that member alone, with a personal link: it names the member,
// works for personalLinkTTL and opens once. Nothing is reissued: while the
// link lives, the reply counts down with a bar the bot edits every
// countdownStep, next to a Discord timestamp that counts on its own, and it is
// deleted as soon as the link is followed (the page tells the bot through
// LinkUsed) or runs out, so no stale link stays on screen.

const (
	// personalLinkTTL is how long a personal link works: long enough to click
	// it. Following it opens a session of its own, which carries the visit.
	personalLinkTTL = 30 * time.Second

	// countdownStep is how often the reply's bar shrinks, and countdownCells
	// how many cells it has.
	countdownStep  = 3 * time.Second
	countdownCells = 10
)

// personalLinks are the replies carrying links not yet followed, by link ID.
type personalLinks struct {
	mu sync.Mutex
	m  map[string]*personalLink
}

type personalLink struct {
	s    *discordgo.Session
	i    *discordgo.Interaction
	stop chan struct{}
}

// studySpaceIntro is the reply that carries a link to the material page.
const studySpaceIntro = "### 📚 Ο χώρος μελέτης του μαθήματος\n" +
	"Όλο το υλικό του καναλιού σε μία σελίδα. Διάβασε τα PDF και τις φωτογραφίες, τσέκαρε ό,τι έχεις μελετήσει, " +
	"δώσε στα αρχεία δικούς σου τίτλους, κράτα σημειώσεις δίπλα σε κάθε αρχείο και γράψε ή υπογράμμισε πάνω στις σελίδες.\n" +
	"Ό,τι κάνεις αποθηκεύεται στον λογαριασμό σου και το βρίσκεις κάθε φορά που επιστρέφεις, από όποια συσκευή κι αν μπεις. " +
	"Τα βλέπεις μόνο εσύ."

// mcpGuideIntro is the reply that carries a link to the MCP setup page.
const mcpGuideIntro = "Οδηγίες βήμα προς βήμα για να συνδέσεις το **Claude** ή το **ChatGPT** με το υλικό του Steki."

// countdown is the reply's last lines: a bar of the time left, and when the
// link runs out as a Discord timestamp, which every client counts down live.
func countdown(left time.Duration, expires time.Time) string {
	full := int((left*countdownCells + personalLinkTTL - 1) / personalLinkTTL)
	full = min(max(full, 0), countdownCells)
	return fmt.Sprintf("\n\n`%s%s`  λήγει <t:%d:R>\n"+
		"-# Ο σύνδεσμος είναι προσωπικός και ανοίγει μία φορά. Το μήνυμα θα σβηστεί μόλις τον ανοίξεις ή λήξει.",
		strings.Repeat("█", full), strings.Repeat("░", countdownCells-full), expires.Unix())
}

// onViewMaterials answers a sticky's "Προβολή υλικού μαθήματος" button.
func (h *Handler) onViewMaterials(s *discordgo.Session, i *discordgo.InteractionCreate) {
	userID := actorID(i)
	if userID == "" || !h.stickyEnabled() {
		h.respondNow(s, i, "Ο χώρος μελέτης δεν είναι διαθέσιμος αυτή τη στιγμή. Δοκίμασε ξανά σε λίγο.")
		return
	}
	ctx, cancel := h.ctx()
	ch, err := h.channel(ctx, s, i.ChannelID)
	cancel()
	if err != nil || !isCourseChannel(ch) {
		h.respondNow(s, i, msgGenericError)
		return
	}
	link, tokenID, err := h.materialLink(userID, ch.ID)
	if err != nil {
		h.log.Error("materials: issue personal link", "user_id", userID, "error", err)
		h.respondNow(s, i, msgGenericError)
		return
	}
	h.sendPersonalLink(s, i, tokenID, studySpaceIntro, "Άνοιγμα χώρου μελέτης", "📖", link)
}

// onMCPGuide answers the MCP announcement's "Οδηγίες σύνδεσης" button.
func (h *Handler) onMCPGuide(s *discordgo.Session, i *discordgo.InteractionCreate) {
	userID := actorID(i)
	if userID == "" || !h.mcpEnabled() {
		h.respondNow(s, i, "Οι οδηγίες δεν είναι διαθέσιμες αυτή τη στιγμή. Δοκίμασε ξανά σε λίγο.")
		return
	}
	link, tokenID, err := h.mcpGuideLink(userID)
	if err != nil {
		h.log.Error("mcp: issue personal link", "user_id", userID, "error", err)
		h.respondNow(s, i, msgGenericError)
		return
	}
	h.sendPersonalLink(s, i, tokenID, mcpGuideIntro, "Οδηγίες σύνδεσης", "🔌", link)
}

// sendPersonalLink replies to the member alone with the link, and deletes the
// reply once the link is followed or has run out.
func (h *Handler) sendPersonalLink(s *discordgo.Session, i *discordgo.InteractionCreate, tokenID, text, label, emoji, link string) {
	expires := time.Now().Add(personalLinkTTL)
	err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: text + countdown(personalLinkTTL, expires),
			Flags:   discordgo.MessageFlagsEphemeral,
			Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
				discordgo.Button{Label: label, Style: discordgo.LinkButton, URL: link, Emoji: &discordgo.ComponentEmoji{Name: emoji}},
			}}},
			AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
		},
	})
	if err != nil {
		h.log.Error("personal link: reply failed", "error", err)
		return
	}
	l := &personalLink{s: s, i: i.Interaction, stop: make(chan struct{})}
	q := &h.links
	q.mu.Lock()
	if q.m == nil {
		q.m = map[string]*personalLink{}
	}
	q.m[tokenID] = l
	q.mu.Unlock()
	go h.safely("personal_link_countdown", func() { h.countDown(l, tokenID, text, expires) })
}

// countDown shrinks the reply's bar until the link runs out, then deletes the
// reply. It stops early when the link is followed.
func (h *Handler) countDown(l *personalLink, tokenID, text string, expires time.Time) {
	tick := time.NewTicker(countdownStep)
	defer tick.Stop()
	end := time.NewTimer(time.Until(expires))
	defer end.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-h.base.Done():
			return
		case <-end.C:
			h.dropPersonalLink(tokenID)
			return
		case <-tick.C:
			left := time.Until(expires)
			if left <= 0 {
				continue
			}
			content := text + countdown(left, expires)
			ctx, cancel := h.ctx()
			_, err := l.s.InteractionResponseEdit(l.i, &discordgo.WebhookEdit{Content: &content}, discordgo.WithContext(ctx))
			cancel()
			if err != nil {
				// Most likely dismissed by the member; nothing left to update.
				h.log.Debug("personal link: update countdown", "error", err)
			}
		}
	}
}

// LinkUsed implements verify.LinkWatcher: the link was followed, so the reply
// that carried it goes.
func (h *Handler) LinkUsed(tokenID string) {
	go h.dropPersonalLink(tokenID)
}

// dropPersonalLink deletes the reply carrying a link, once.
func (h *Handler) dropPersonalLink(tokenID string) {
	q := &h.links
	q.mu.Lock()
	l := q.m[tokenID]
	delete(q.m, tokenID)
	q.mu.Unlock()
	if l == nil {
		return
	}
	close(l.stop)
	h.safely("personal_link_delete", func() {
		ctx, cancel := h.ctx()
		defer cancel()
		if err := l.s.InteractionResponseDelete(l.i, discordgo.WithContext(ctx)); err != nil {
			h.log.Debug("personal link: delete reply", "error", err)
		}
	})
}
