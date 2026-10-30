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

// Every course channel keeps one bot message at its bottom, with the buttons
// to the material page and to the upload form and the number of files members
// have shared there. "Sticky" is done the only way Discord allows: when
// others post, the bot sends a fresh copy and deletes the old one.
//
// Each copy is sent as a silent message (SUPPRESS_NOTIFICATIONS), so it never
// raises a push or desktop notification. That is the whole reason the feature
// is acceptable in busy channels, so it is checked rather than assumed: if
// Discord ever returns a copy without the flag, the bot deletes it, turns the
// sticky off for the rest of the process and tells the moderators.
//
// Reposts are debounced. A conversation produces one repost once it pauses
// for stickyQuiet, and at most every stickyMaxWait while it does not. A
// change of the count alone only edits the message, which notifies nobody.

const (
	uploadButtonID = "materials:upload"

	stickyQuiet   = 20 * time.Second
	stickyMaxWait = 2 * time.Minute
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

// stickyContent is the sticky's text for a count of member files.
func stickyContent(count int) string {
	if count == 1 {
		return "📚 **1 διαθέσιμο αρχείο από μέλη του server**"
	}
	return fmt.Sprintf("📚 **%d διαθέσιμα αρχεία από μέλη του server**", count)
}

func stickyComponents() []discordgo.MessageComponent {
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{
			Label:    "Προβολή υλικού",
			Style:    discordgo.PrimaryButton,
			CustomID: materialsButtonID,
			Emoji:    &discordgo.ComponentEmoji{Name: "📚"},
		},
		discordgo.Button{
			Label:    "Ανέβασμα υλικού",
			Style:    discordgo.SecondaryButton,
			CustomID: uploadButtonID,
			Emoji:    &discordgo.ComponentEmoji{Name: "📤"},
		},
	}}}
}

// silentSend posts a sticky as a silent message and confirms Discord kept it
// silent. Mentions are disabled too, though the text has none.
func silentSend(ctx context.Context, s *discordgo.Session, channelID, content string) (*discordgo.Message, error) {
	return s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content:         content,
		Components:      stickyComponents(),
		Flags:           discordgo.MessageFlagsSuppressNotifications,
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
	}, discordgo.WithContext(ctx))
}

// isSilent reports whether a sent message carries the no-notification flag.
func isSilent(m *discordgo.Message) bool {
	return m != nil && m.Flags&discordgo.MessageFlagsSuppressNotifications != 0
}

// syncSticky brings a channel's sticky up to date. With repost it makes sure
// the sticky is the newest message, sending a new copy if it is not; without,
// it only edits the count.
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

	count, err := h.st.CountMemberMaterials(ctx, channelID)
	if err != nil {
		return err
	}
	content := stickyContent(count)
	old, err := h.st.Sticky(ctx, channelID)
	if err != nil && err != store.ErrNotFound {
		return err
	}

	if old != "" {
		current := ""
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
				repost, current = false, last[0].Content
			}
		}
		if !repost {
			if current == content {
				return nil
			}
			_, err := s.ChannelMessageEditComplex(&discordgo.MessageEdit{
				ID:         old,
				Channel:    channelID,
				Content:    &content,
				Components: ptr(stickyComponents()),
			}, discordgo.WithContext(ctx))
			if err == nil {
				return nil
			}
			if !dc.IsMissingResource(err) && dc.APICode(err) != discordgo.ErrCodeUnknownMessage {
				return err
			}
			// Someone deleted it; fall through to a new one.
		}
	}

	msg, err := silentSend(ctx, s, channelID, content)
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
		if err := s.ChannelMessageDelete(channelID, old, discordgo.WithContext(ctx)); err != nil &&
			!dc.IsMissingResource(err) && dc.APICode(err) != discordgo.ErrCodeUnknownMessage {
			h.log.Warn("sticky: delete previous copy", "channel_id", channelID, "message_id", old, "error", err)
		}
	}
	return nil
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
	h.alertMods(s, "⚠️ Το Discord δεν κράτησε σιωπηλό το μήνυμα «Προβολή υλικού» στο <#%s>, οπότε το διέγραψα "+
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

func ptr[T any](v T) *T { return &v }
