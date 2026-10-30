package events

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/verify"
)

// The buttons on the stickies and the MCP announcements carry no link. A click
// answers, to that member alone, with a personal link: it names the member,
// works for personalLinkTTL and opens once. Nothing is reissued: while the
// link lives, a bar at the top of the reply shrinks every countdownStep, and the reply
// is deleted as soon as the link is followed (the page tells the bot through
// LinkUsed) or runs out, so no stale link stays on screen.

const (
	// personalLinkTTL is how long a personal link works: long enough to click
	// it. Following it opens a session of its own, which carries the visit.
	personalLinkTTL = 30 * time.Second

	// personalLinksPerMinute is how many personal links one member may ask for
	// in a minute, so a script cannot press the button in a loop.
	personalLinksPerMinute = 6

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
const studySpaceIntro = "Πάτησε το κουμπί για να δεις το υλικό στον χώρο μελέτης:"

// mcpGuideIntro is the reply that carries a link to the MCP setup page.
const mcpGuideIntro = "Πάτησε το κουμπί για να δεις τις οδηγίες σύνδεσης του AI σου:"

// countdown is the bar at the top of the reply, showing how much of the
// link's time is left.
func countdown(left time.Duration) string {
	full := int((left*countdownCells + personalLinkTTL - 1) / personalLinkTTL)
	full = min(max(full, 0), countdownCells)
	return "`" + strings.Repeat("█", full) + strings.Repeat("░", countdownCells-full) + "`\n"
}

// onViewMaterials answers a sticky's "Προβολή υλικού μαθήματος" button.
func (h *Handler) onViewMaterials(s *discordgo.Session, i *discordgo.InteractionCreate) {
	userID := actorID(i)
	if userID == "" || !h.stickyEnabled() {
		h.respondNow(s, i, "Ο χώρος μελέτης δεν είναι διαθέσιμος αυτή τη στιγμή. Δοκίμασε ξανά σε λίγο.")
		return
	}
	if !h.gate.allow("link:"+userID, personalLinksPerMinute, time.Minute) {
		h.respondNow(s, i, "⏳ Πολλά αιτήματα. Δοκίμασε ξανά σε λίγο.")
		return
	}
	ctx, cancel := h.ctx()
	ch, err := h.channel(ctx, s, i.ChannelID)
	cancel()
	if err != nil || !isCourseChannel(ch) {
		h.respondNow(s, i, msgGenericError)
		return
	}
	// The button is on a card anyone who sees the channel can press, but the
	// study space is for members who finished registration. The page checks
	// this again; refusing here keeps a link from being issued at all.
	ctx, cancel = h.ctx()
	err = (&materialSource{h: h, s: s}).CanRead(ctx, userID, ch.ID)
	cancel()
	switch {
	case errors.Is(err, verify.ErrNoAccess):
		h.respondNow(s, i, "Ο χώρος μελέτης είναι διαθέσιμος μόνο σε επιβεβαιωμένα μέλη.")
		return
	case err != nil:
		h.log.Error("materials: check access for personal link", "user_id", userID, "error", err)
		h.respondNow(s, i, msgGenericError)
		return
	}
	link, tokenID, err := h.materialLink(userID, ch.ID, "")
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
	if !h.gate.allow("link:"+userID, personalLinksPerMinute, time.Minute) {
		h.respondNow(s, i, "⏳ Πολλά αιτήματα. Δοκίμασε ξανά σε λίγο.")
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
	h.sendLinkReply(s, i, false, tokenID, text, label, emoji, link)
}

// sendLinkReply is sendPersonalLink for an interaction answered either now or,
// with deferred, by filling in a reply acknowledged earlier.
func (h *Handler) sendLinkReply(s *discordgo.Session, i *discordgo.InteractionCreate, deferred bool, tokenID, text, label, emoji, link string) {
	expires := time.Now().Add(personalLinkTTL)
	content := countdown(personalLinkTTL) + text
	components := []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: label, Style: discordgo.LinkButton, URL: link, Emoji: &discordgo.ComponentEmoji{Name: emoji}},
	}}}
	mentions := &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}
	var err error
	if deferred {
		_, err = s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content: &content, Components: &components, AllowedMentions: mentions,
		})
	} else {
		err = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: content, Flags: discordgo.MessageFlagsEphemeral,
				Components: components, AllowedMentions: mentions,
			},
		})
	}
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
			content := countdown(left) + text
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

// profileOf is how the material page shows a member.
func profileOf(m *discordgo.Member, guildID string) verify.Profile {
	if m == nil || m.User == nil {
		return verify.Profile{}
	}
	mem := *m
	mem.GuildID = guildID
	name := mem.Nick
	if name == "" {
		name = displayName(mem.User)
	}
	return verify.Profile{Name: name, Username: mem.User.Username, Avatar: mem.AvatarURL("64")}
}

// ProfileSource returns the verify.ProfileSource that finds members through s.
func (h *Handler) ProfileSource(s *discordgo.Session) verify.ProfileSource {
	return profileSource{h: h, s: s}
}

type profileSource struct {
	h *Handler
	s *discordgo.Session
}

// MemberProfile returns how the material page shows a member.
func (p profileSource) MemberProfile(ctx context.Context, guildID, userID string) (verify.Profile, error) {
	m, err := p.h.guildMember(ctx, p.s, guildID, userID)
	if err != nil {
		return verify.Profile{}, err
	}
	return profileOf(m, guildID), nil
}

// cmdMaterialsViewAs gives a staff member a personal link to a course
// channel's study space as another member sees it: their titles, notes and
// ink, read-only. The page saves nothing, files no reports and is not counted
// as present, and every use is logged for the moderators.
func (h *Handler) cmdMaterialsViewAs(s *discordgo.Session, i *discordgo.InteractionCreate, sub *discordgo.ApplicationCommandInteractionDataOption) {
	if !h.stickyEnabled() {
		h.reply(s, i, "Ο χώρος μελέτης δεν είναι διαθέσιμος αυτή τη στιγμή.")
		return
	}
	target := optUser(i, sub.Options, "user")
	opt := findOpt(sub.Options, "channel")
	if target == nil || opt == nil {
		h.reply(s, i, "Διάλεξε κανάλι μαθήματος και μέλος.")
		return
	}
	ctx, cancel := h.ctxTimeout(flowTimeout)
	defer cancel()
	ch, err := h.channel(ctx, s, opt.ChannelValue(nil).ID)
	if err != nil || !isCourseChannel(ch) {
		h.reply(s, i, "Διάλεξε ένα κανάλι μαθήματος.")
		return
	}
	lib := &mcpLibrary{h: h, s: s, src: &materialSource{h: h, s: s}}
	mem, err := lib.member(ctx, i.GuildID, target.ID)
	if err != nil {
		h.reply(s, i, fmt.Sprintf("Το <@%s> δεν είναι μέλος του server.", target.ID))
		return
	}
	switch err := lib.src.CanRead(ctx, target.ID, ch.ID); {
	case errors.Is(err, verify.ErrNoAccess):
		h.reply(s, i, fmt.Sprintf("Το <@%s> δεν έχει πρόσβαση στο <#%s>, άρα δεν βλέπει τον χώρο μελέτης του.", target.ID, ch.ID))
		return
	case err != nil:
		h.reply(s, i, fmt.Sprintf("Ο έλεγχος πρόσβασης απέτυχε: %v", err))
		return
	}
	link, tokenID, err := h.materialLink(target.ID, ch.ID, actorID(i))
	if err != nil {
		h.reply(s, i, fmt.Sprintf("Δεν μπόρεσα να φτιάξω τον σύνδεσμο: %v", err))
		return
	}
	h.log.Info("materials: view as member", "user_id", actorID(i), "target_id", target.ID, "channel_id", ch.ID)
	h.alertMods(s, "👁️ Ο/Η <@%s> άνοιξε τον χώρο μελέτης του <#%s> ως <@%s> (μόνο ανάγνωση).", actorID(i), ch.ID, target.ID)
	h.sendLinkReply(s, i, true, tokenID,
		fmt.Sprintf("Προβολή ως **%s**, μόνο ανάγνωση:", profileOf(mem, i.GuildID).Name), "Άνοιγμα χώρου μελέτης", "👁️", link)
}
