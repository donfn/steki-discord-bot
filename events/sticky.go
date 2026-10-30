package events

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/dc"
	"steki-bot/store"
)

// Every course channel (bar the statistics channels, where members post exam
// results) keeps one bot card at its bottom, with the buttons to the material
// page and to the upload form. The first carries the number of files members
// have shared there; a click answers, to that member alone, with a personal
// link to the page (links.go), so the card itself holds no link and never
// needs refreshing for one. "Sticky" is done the only way Discord allows: when
// others post, the bot sends a fresh copy and deletes the old one.
//
// Each copy is sent as a silent message (SUPPRESS_NOTIFICATIONS), so it never
// raises a push or desktop notification. That is the whole reason the feature
// is acceptable in busy channels, so it is checked rather than assumed: if
// Discord ever returns a copy without the flag, the bot deletes it, turns the
// sticky off for the rest of the process and tells the moderators.
//
// Reposts are debounced only enough to coalesce a burst. A message brings the
// sticky back to the bottom once the channel has been quiet for stickyQuiet,
// and at most stickyMaxWait later while it keeps going. A change of the count
// alone only edits the message, which notifies nobody.

const (
	uploadButtonID = "materials:upload"
	// viewButtonID asks for a personal link to the material page.
	viewButtonID = "materials:view"

	stickyQuiet   = 2 * time.Second
	stickyMaxWait = 10 * time.Second
	stickyEditLag = 15 * time.Second
)

// stickyQueue is the pending sticky work per channel.
type stickyQueue struct {
	mu      sync.Mutex
	pending map[string]*stickyJob
	locks   keyedMutex
	// off is set when silent sending turned out not to work.
	off bool
}

type stickyJob struct {
	timer  *time.Timer
	first  time.Time
	repost bool
}

// stickyEnabled reports whether stickies are in use.
func (h *Handler) stickyEnabled() bool {
	if h.mat == nil || !h.matLive.Load() {
		return false
	}
	h.sticky.mu.Lock()
	defer h.sticky.mu.Unlock()
	return !h.sticky.off
}

// bumpSticky notes new activity in a course channel: the sticky moves back
// to the bottom once the channel goes quiet.
func (h *Handler) bumpSticky(s *discordgo.Session, channelID string) {
	h.scheduleSticky(s, channelID, true, stickyQuiet)
}

// refreshSticky updates the count on a channel's sticky in place.
func (h *Handler) refreshSticky(s *discordgo.Session, channelID string) {
	h.scheduleSticky(s, channelID, false, stickyEditLag)
}

func (h *Handler) scheduleSticky(s *discordgo.Session, channelID string, repost bool, delay time.Duration) {
	if !h.stickyEnabled() {
		return
	}
	q := &h.sticky
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.pending == nil {
		q.pending = map[string]*stickyJob{}
	}
	now := time.Now()
	job, ok := q.pending[channelID]
	if !ok {
		job = &stickyJob{first: now}
		job.timer = time.AfterFunc(delay, func() { h.runSticky(s, channelID) })
		q.pending[channelID] = job
	} else if wait := job.first.Add(stickyMaxWait).Sub(now); wait < delay {
		// Busy channel: stop pushing the repost back once it has waited long
		// enough.
		job.timer.Reset(max(wait, 0))
	} else {
		job.timer.Reset(delay)
	}
	job.repost = job.repost || repost
}

// runSticky does a channel's pending sticky work.
func (h *Handler) runSticky(s *discordgo.Session, channelID string) {
	q := &h.sticky
	q.mu.Lock()
	job := q.pending[channelID]
	delete(q.pending, channelID)
	q.mu.Unlock()
	if job == nil || h.base.Err() != nil {
		return
	}
	h.safely("sticky", func() {
		ctx, cancel := h.ctxTimeout(flowTimeout)
		defer cancel()
		if err := h.syncSticky(ctx, s, channelID, job.repost); err != nil {
			h.log.Warn("sticky: update failed", "channel_id", channelID, "error", err)
		}
	})
}

// stickyColor is the brand yellow of the card's edge.
const stickyColor = 0xFFC900

// stickyFlags mark the sticky as a components-only message, the one kind
// Discord lets carry buttons with no text or embed, and as silent.
const stickyFlags = discordgo.MessageFlagsIsComponentsV2 | discordgo.MessageFlagsSuppressNotifications

// stickyCard is the sticky's buttons for a count of member files. It is only
// buttons: the card sits at the bottom of a busy channel, so it takes as
// little room as it can.
func stickyCard(count int) []discordgo.MessageComponent {
	components := []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{
			Label:    fmt.Sprintf("Προβολή υλικού μαθήματος (%d)", count),
			Style:    discordgo.PrimaryButton,
			CustomID: viewButtonID,
			Emoji:    &discordgo.ComponentEmoji{Name: "📚"},
		},
		discordgo.Button{
			Label:    "Ανέβασμα υλικού",
			Style:    discordgo.SecondaryButton,
			CustomID: uploadButtonID,
			Emoji:    &discordgo.ComponentEmoji{Name: "📤"},
		},
	}}}
	return components
}

// silentSend posts a sticky as a silent message and confirms Discord kept it
// silent. Mentions are disabled too, though the text has none.
func silentSend(ctx context.Context, s *discordgo.Session, channelID string, count int) (*discordgo.Message, error) {
	return s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Components:      stickyCard(count),
		Flags:           stickyFlags,
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	}, discordgo.WithContext(ctx))
}

// isSilent reports whether a sent message carries the no-notification flag.
func isSilent(m *discordgo.Message) bool {
	return m != nil && m.Flags&discordgo.MessageFlagsSuppressNotifications != 0
}

// syncSticky brings a channel's sticky up to date. With repost it makes sure
// the sticky is the newest message, sending a new copy if it is not; without,
// it only edits the card in place.
func (h *Handler) syncSticky(ctx context.Context, s *discordgo.Session, channelID string, repost bool) error {
	unlock := h.sticky.locks.Lock(channelID)
	defer unlock()

	// Callers pass any channel whose files changed; only course channels
	// have a sticky.
	ch, err := h.channel(ctx, s, channelID)
	if err != nil {
		return err
	}
	if !isCourseChannel(ch) {
		return nil
	}
	old, err := h.st.Sticky(ctx, channelID)
	if err != nil && err != store.ErrNotFound {
		return err
	}
	if !hasSticky(ch) {
		return h.removeSticky(ctx, s, channelID, old)
	}

	count, err := h.st.CountMemberMaterials(ctx, channelID)
	if err != nil {
		return err
	}

	if old != "" {
		if repost {
			var last []*discordgo.Message
			err := h.retry(ctx, "sticky.last", func() error {
				var err error
				last, err = s.ChannelMessages(channelID, 1, "", "", "", discordgo.WithContext(ctx))
				return err
			})
			if err != nil {
				return err
			}
			if len(last) == 1 && last[0].ID == old {
				repost = false
			}
		}
		if !repost {
			components := stickyCard(count)
			// Empty content and embeds clear what older stickies carried.
			_, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
				ID:         old,
				Channel:    channelID,
				Content:    ptr(""),
				Embeds:     &[]*discordgo.MessageEmbed{},
				Components: &components,
				Flags:      stickyFlags,
			}, discordgo.WithContext(ctx))
			if err == nil {
				return nil
			}
			if !dc.IsMissingResource(err) && dc.APICode(err) != discordgo.ErrCodeUnknownMessage {
				// Most likely an older sticky Discord will not turn into a
				// buttons-only message; a new one replaces it.
				h.log.Warn("sticky: edit failed; posting a new one", "channel_id", channelID, "error", err)
			}
			// Deleted or not editable: fall through to a new one.
		}
	}

	msg, err := silentSend(ctx, s, channelID, count)
	if err != nil {
		return err
	}
	if !isSilent(msg) {
		h.disableStickies(s, channelID, msg.ID)
		return fmt.Errorf("sticky: discord did not keep the message silent (flags %d)", msg.Flags)
	}
	if err := h.st.SetSticky(ctx, channelID, msg.ID); err != nil {
		return err
	}
	if old != "" {
		h.deleteStickyMessage(ctx, s, channelID, old)
	}
	return nil
}

// removeSticky takes down the sticky of a channel that should not have one.
func (h *Handler) removeSticky(ctx context.Context, s *discordgo.Session, channelID, messageID string) error {
	if messageID == "" {
		return nil
	}
	h.deleteStickyMessage(ctx, s, channelID, messageID)
	return h.st.DeleteSticky(ctx, channelID)
}

func (h *Handler) deleteStickyMessage(ctx context.Context, s *discordgo.Session, channelID, messageID string) {
	if err := s.ChannelMessageDelete(channelID, messageID, discordgo.WithContext(ctx)); err != nil &&
		!dc.IsMissingResource(err) && dc.APICode(err) != discordgo.ErrCodeUnknownMessage {
		h.log.Warn("sticky: delete message", "channel_id", channelID, "message_id", messageID, "error", err)
	}
}

// disableStickies is the safety stop for a sticky that went out without the
// silent flag: it is removed at once and no more are sent.
func (h *Handler) disableStickies(s *discordgo.Session, channelID, messageID string) {
	h.sticky.mu.Lock()
	h.sticky.off = true
	for _, job := range h.sticky.pending {
		job.timer.Stop()
	}
	h.sticky.pending = nil
	h.sticky.mu.Unlock()

	ctx, cancel := h.ctx()
	defer cancel()
	if err := s.ChannelMessageDelete(channelID, messageID, discordgo.WithContext(ctx)); err != nil {
		h.log.Error("sticky: delete non-silent copy", "channel_id", channelID, "error", err)
	}
	h.log.Error("sticky: silent sending does not work; stickies are off until restart", "channel_id", channelID)
	h.urgentMods(s, "⚠️ Το Discord έστειλε το μήνυμα «Υλικό μαθήματος» στο <#%s> με ειδοποίηση αντί για σιωπηλά. Το διέγραψα "+
		"και σταμάτησα τα sticky μηνύματα μέχρι την επόμενη επανεκκίνηση, για να μην ενοχλούνται τα μέλη.", channelID)
}

// placeStickies makes sure every course channel has an up-to-date sticky at
// its bottom, one channel at a time. It runs when the bot connects.
func (h *Handler) placeStickies(s *discordgo.Session) {
	guildID := h.guildID()
	if guildID == "" || !h.stickyEnabled() {
		return
	}
	ctx, cancel := h.ctx()
	channels, err := dc.GuildChannels(ctx, s, h.log, guildID)
	cancel()
	if err != nil {
		h.log.Error("sticky: list channels", "error", err)
		return
	}
	placed := 0
	for _, ch := range channels {
		if !isCourseChannel(ch) || !h.stickyEnabled() || h.base.Err() != nil {
			continue
		}
		ctx, cancel := h.ctxTimeout(flowTimeout)
		err := h.syncSticky(ctx, s, ch.ID, true)
		if err == nil {
			err = h.sweepStickies(ctx, s, ch.ID)
		}
		cancel()
		if err != nil {
			h.log.Warn("sticky: place", "channel_id", ch.ID, "name", ch.Name, "error", err)
			continue
		}
		placed++
		time.Sleep(time.Second)
	}
	h.log.Info("sticky: course channels checked", "channels", placed)
}

// legacyMaterialsButtonID was the custom ID of the first design's
// "Προβολή υλικού" button, which sat on every old sticky.
const legacyMaterialsButtonID = "materials:open"

// sweepStickies deletes every sticky copy the bot left in a channel other than
// the one it tracks: old-design messages whose replacement or deletion failed,
// and all of them in channels that no longer have a sticky. It looks at the
// latest 100 messages.
func (h *Handler) sweepStickies(ctx context.Context, s *discordgo.Session, channelID string) error {
	keep, err := h.st.Sticky(ctx, channelID)
	if err != nil && err != store.ErrNotFound {
		return err
	}
	var msgs []*discordgo.Message
	if err := h.retry(ctx, "sticky.sweep", func() error {
		var err error
		msgs, err = s.ChannelMessages(channelID, 100, "", "", "", discordgo.WithContext(ctx))
		return err
	}); err != nil {
		return err
	}
	for _, m := range msgs {
		if m.ID == keep || m.Author == nil || s.State.User == nil || m.Author.ID != s.State.User.ID || !isStickyMessage(m) {
			continue
		}
		h.deleteStickyMessage(ctx, s, channelID, m.ID)
		h.log.Info("sticky: removed leftover copy", "channel_id", channelID, "message_id", m.ID)
	}
	return nil
}

// isStickyMessage reports whether m carries a sticky's buttons, old or new.
func isStickyMessage(m *discordgo.Message) bool {
	for _, c := range m.Components {
		row, ok := c.(*discordgo.ActionsRow)
		if !ok {
			continue
		}
		for _, b := range row.Components {
			if btn, ok := b.(*discordgo.Button); ok && (btn.CustomID == legacyMaterialsButtonID ||
				btn.CustomID == viewButtonID || btn.CustomID == uploadButtonID) {
				return true
			}
		}
	}
	return false
}

func ptr[T any](v T) *T { return &v }
