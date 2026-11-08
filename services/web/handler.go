// Package web is the public face of the bot: the pages, the MCP server and the
// course material. It alone holds JWT_SECRET, the OAuth application and the Gemini
// and Railway tokens, and it alone can reach blastdoor. The two pages that belong
// to other services (the email link and the captcha) are served here and answered
// by their owners over the internal API.
package web

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/internal/analytics"
	"steki-bot/internal/bot"
	"steki-bot/internal/config"
	"steki-bot/internal/materials"
	"steki-bot/internal/rpc"
	"steki-bot/internal/store"
	"steki-bot/internal/tokens"
	"steki-bot/internal/usernotes"
	"steki-bot/internal/verify"
)

// Intents are the gateway events this service reads.
const Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMembers |
	discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent

// rpcTimeout bounds an answer from another service while a member waits.
const rpcTimeout = 3 * time.Second

// Handler carries the service's dependencies.
type Handler struct {
	*bot.Core
	cfg    *config.Web
	st     *store.Library
	tokens *tokens.Tokens
	// converter decodes uploaded images in the blastdoor service.
	converter *materials.Converter

	// bucket is the material backup bucket and mat the worker copying into
	// it. mat is set by Register.
	bucket *materials.Bucket
	mat    *materials.Worker
	// judge checks the reports filed on the material page. It is set by
	// Register when the content check has an API key.
	judge  materials.ReportJudge
	sticky stickyQueue
	// links are the personal links handed out and not yet followed.
	links personalLinks
	// matLive is set once the bucket answered and the worker runs; the
	// stickies and both buttons wait for it.
	matLive atomic.Bool
	// notes keeps members' page notes in their own databases in the bucket.
	notes *usernotes.Manager
	// dropped remembers messages the moderation service restricted their author
	// for, in case its word arrives before the gateway's copy of the message.
	dropped droppedMessages

	moderation   *rpc.Client
	registration *rpc.Client
}

// NewHandler wires the handler's dependencies.
func NewHandler(cfg *config.Web, st *store.Library, an *analytics.Client, logger *slog.Logger) (*Handler, error) {
	b := cfg.Bucket
	bucket, err := materials.NewBucket(b.Endpoint, b.Region, b.AccessKeyID, b.SecretAccessKey, b.Name)
	if err != nil {
		return nil, fmt.Errorf("material bucket: %w", err)
	}
	bucket.SetCodec(materials.NewConverter(cfg.BlastdoorURL))
	return &Handler{
		Core:         bot.NewCore(logger, an, st),
		cfg:          cfg,
		st:           st,
		bucket:       bucket,
		tokens:       tokens.New(cfg.JWTSecret, cfg.InternalSecret),
		converter:    materials.NewConverter(cfg.BlastdoorURL),
		moderation:   rpc.NewClient(cfg.ModerationURL, cfg.InternalSecret),
		registration: rpc.NewClient(cfg.RegistrationURL, cfg.InternalSecret),
	}, nil
}

// Tokens is the service's token checker, which the pages share.
func (h *Handler) Tokens() *tokens.Tokens { return h.tokens }

// Register subscribes the handler to the gateway events it serves.
func (h *Handler) Register(s *discordgo.Session) {
	h.Core.Register(s)
	var screener materials.Screener = materials.BasicScreener{}
	if h.cfg.GoogleAIToken != "" {
		g := materials.NewGemini(h.cfg.GoogleAIToken, h.cfg.GoogleAIModel, h.Log)
		screener, h.judge = g, g
		h.Log.Info("image content check enabled")
	}
	h.mat = materials.NewWorker(h.st, h.bucket, screener, h.refreshMaterialSource(s),
		func(home string) { h.refreshSticky(s, home) },
		func() { go h.Safely("review_cards", func() { h.announceReviews(s) }) },
		func(ctx context.Context) ([]string, error) { return h.courseChannelIDs(ctx, s) }, h.Log)
	notes, err := usernotes.New(h.bucket, filepath.Join(h.cfg.DataDir, "user-notes"), h.Log)
	if err != nil {
		h.Log.Error("material page notes off", "error", err)
	} else {
		h.notes = notes
		h.WG.Add(1)
		go func() {
			defer h.WG.Done()
			h.Safely("user_notes", func() { notes.Run(h.Base) })
		}()
	}
	h.OnHome = func(s *discordgo.Session) { h.startMaterials(s) }
	s.AddHandler(bot.Guard(h.Core, "message_create", h.onMessage))
	s.AddHandler(bot.Guard(h.Core, "message_update", h.onMessageUpdate))
	s.AddHandler(bot.Guard(h.Core, "message_delete", h.onMessageDelete))
	s.AddHandler(bot.Guard(h.Core, "message_delete_bulk", h.onMessageDeleteBulk))
	s.AddHandler(bot.Guard(h.Core, "raw_event", h.onRawEvent))
	s.AddHandler(bot.Guard(h.Core, "interaction_create", h.onInteractionCreate))
}

// startMaterials starts the material worker once the bucket has been
// confirmed reachable. Nothing here reads channel history or touches a message
// already in Discord: the stickies, the cards for held files and reports, and
// old history are brought up to date by /materials stickies, /materials cards
// and /materials scan. The worker only picks up the work the database holds.
func (h *Handler) startMaterials(s *discordgo.Session) {
	ctx, cancel := h.Ctx()
	err := h.bucket.Check(ctx)
	cancel()
	if err != nil {
		h.Log.Error("material backup off: bucket unreachable", "error", err)
		h.UrgentMods(s, "⚠️ Δεν έχω πρόσβαση στο bucket των αντιγράφων (%v). Η αποθήκευση υλικού είναι απενεργοποιημένη μέχρι την επόμενη επανεκκίνηση.", err)
		return
	}
	h.Log.Info("material backup enabled")
	h.matLive.Store(true)
	h.WG.Add(1)
	go func() {
		defer h.WG.Done()
		h.Safely("materials_worker", func() { h.mat.Run(h.Base) })
	}()
}

// onMessage records the files of messages in course channels. The moderation
// service screens the same messages on its own connection: what it deletes is
// hidden again by onMessageDelete, and what it restricts the author for by the
// internal API.
func (h *Handler) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || !h.InHomeGuild(m.GuildID) {
		return
	}
	// The bot's own messages never move a sticky, or each would chase itself;
	// its uploads move it explicitly. Other bots' count like anyone's.
	if s.State.User != nil && m.Author.ID == s.State.User.ID {
		return
	}
	ctx, cancel := h.Ctx()
	ch, err := h.Channel(ctx, s, m.ChannelID)
	cancel()
	if err != nil || bot.IsRegisterChannel(ch) {
		return
	}
	h.onMaterialMessage(s, ch, m.Message)
}

func (h *Handler) onMessageUpdate(s *discordgo.Session, m *discordgo.MessageUpdate) {
	if m.Message == nil || m.Author == nil || m.Author.Bot || m.GuildID == "" {
		return
	}
	h.pruneMaterials(s, m.Message)
}

// onMessageDelete takes a deleted message's files off the material page; their
// backups stay in the bucket.
func (h *Handler) onMessageDelete(s *discordgo.Session, m *discordgo.MessageDelete) {
	h.forgetMaterials(s, m.GuildID, m.ChannelID, m.ID)
}

// droppedTTL is how long a dropped message is remembered. Gateway events reach
// the services seconds apart at worst.
const droppedTTL = 10 * time.Minute

// droppedMessages is a small expiring set. The zero value is ready to use.
type droppedMessages struct {
	mu sync.Mutex
	at map[string]time.Time
}

func (d *droppedMessages) add(id string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.at == nil {
		d.at = map[string]time.Time{}
	}
	now := time.Now()
	for k, t := range d.at {
		if now.Sub(t) > droppedTTL {
			delete(d.at, k)
		}
	}
	d.at[id] = now
}

func (d *droppedMessages) has(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, ok := d.at[id]
	return ok
}

// commandFunc handles one subcommand. The reply has already been deferred.
type commandFunc func(h *Handler, s *discordgo.Session, i *discordgo.InteractionCreate,
	sub *discordgo.ApplicationCommandInteractionDataOption)

// commandRoutes maps "command subcommand" to its handler.
var commandRoutes = map[string]commandFunc{
	"mcp announce":       (*Handler).cmdMCPAnnounce,
	"mcp refresh":        (*Handler).cmdMCPRefresh,
	"materials rescan":   (*Handler).cmdMaterialsRescan,
	"materials remove":   (*Handler).cmdMaterialsRemove,
	"materials scan":     (*Handler).cmdMaterialsScan,
	"materials stickies": (*Handler).cmdMaterialsStickies,
	"materials cards":    (*Handler).cmdMaterialsCards,
	"materials view-as":  (*Handler).cmdMaterialsViewAs,
}

// Routes lists the command paths this service answers.
func Routes() []string {
	out := make([]string, 0, len(commandRoutes))
	for k := range commandRoutes {
		out = append(out, k)
	}
	return out
}

func (h *Handler) onInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionMessageComponent:
		if !strings.HasPrefix(i.MessageComponentData().CustomID, "reg:") {
			h.Track("discord_interaction", bot.InteractionProps(i))
			h.onComponent(s, i)
		}
	case discordgo.InteractionApplicationCommand:
		sub, path, ok := h.BeginCommand(s, i, func(p string) bool { _, ok := commandRoutes[p]; return ok })
		if ok {
			commandRoutes[path](h, s, i, sub)
		}
	}
	// A form discordgo cannot decode (the upload form) arrives without data;
	// onRawEvent handles it.
}

// onComponent handles the material buttons.
func (h *Handler) onComponent(s *discordgo.Session, i *discordgo.InteractionCreate) {
	id := i.MessageComponentData().CustomID
	switch {
	case strings.HasPrefix(id, reviewApprovePrefix) || strings.HasPrefix(id, reviewRejectPrefix):
		h.onReviewButton(s, i)
	case strings.HasPrefix(id, reportRemovePrefix) || strings.HasPrefix(id, reportRestorePrefix):
		h.onReportButton(s, i)
	case id == uploadButtonID:
		h.openUploadForm(s, i)
	case id == viewButtonID:
		h.onViewMaterials(s, i)
	case id == mcpGuideButtonID:
		h.onMCPGuide(s, i)
	}
}

// API is the service's internal API.
func (h *Handler) API(srv *rpc.Server, s *discordgo.Session) {
	rpc.Handle(srv, rpc.PathDropMessage, func(_ context.Context, in rpc.DropMessageRequest) (rpc.None, error) {
		h.dropped.add(in.MessageID)
		h.forgetMaterials(s, in.GuildID, in.ChannelID, in.MessageID)
		return rpc.None{}, nil
	})
	rpc.Handle(srv, rpc.PathForget, func(ctx context.Context, in rpc.UserRequest) (rpc.None, error) {
		_, err := h.st.ForgetEvents(ctx, in.UserID)
		return rpc.None{}, err
	})
	rpc.Handle(srv, rpc.PathEvents, func(ctx context.Context, in rpc.EventsRequest) (rpc.EventsAnswer, error) {
		ev, err := h.st.History(ctx, in.UserID, in.Limit)
		return rpc.EventsAnswer{Events: ev}, err
	})
	rpc.Handle(srv, rpc.PathStats, func(ctx context.Context, _ rpc.None) (rpc.StatsAnswer, error) {
		return rpc.StatsAnswer{Text: h.stats(ctx)}, nil
	})
}

// stats is this service's part of /bot stats.
func (h *Handler) stats(ctx context.Context) string {
	var b strings.Builder
	if err := h.st.Ping(ctx); err != nil {
		fmt.Fprintf(&b, "• 🔴 Βάση υλικού: %v\n", err)
	} else {
		fmt.Fprintf(&b, "• 🟢 Βάση υλικού: %.1f MB\n", float64(h.st.FileSize())/(1<<20))
	}
	reviews, _ := h.st.ReviewsToAnnounce(ctx, cardsShown)
	reports, _ := h.st.ReportsToAnnounce(ctx, cardsShown)
	if n := len(reviews) + len(reports); n > 0 {
		fmt.Fprintf(&b, "• ⚠️ Κάρτες που δεν έφτασαν στα logs: %d (`/materials cards`)\n", n)
	}
	if counts, err := h.st.CountEventsByKind(ctx, time.Now().Add(-24*time.Hour)); err == nil {
		b.WriteString(bot.FormatEventCounts(counts))
	}
	return b.String()
}

// cardsShown caps how many waiting cards /bot stats counts; more than that is
// "plenty" for its purpose, which is to say a catch-up is due.
const cardsShown = 50

// Consenter answers the email link's page through the registration service.
func (h *Handler) Consenter() verify.Consenter { return peerConsenter{h.registration} }

// Lifter answers the captcha page through the moderation service.
func (h *Handler) Lifter() verify.Lifter { return peerLifter{h.moderation} }

type peerConsenter struct{ c *rpc.Client }

func (p peerConsenter) Pending(ctx context.Context, c *verify.Claims) (*verify.Consent, error) {
	var ans rpc.ConsentAnswer
	if err := p.c.Call(ctx, rpc.PathLinkPending, rpc.ClaimsRequest{Claims: *c}, &ans); err != nil {
		return nil, err
	}
	return &ans.Consent, nil
}

func (p peerConsenter) Accept(ctx context.Context, c *verify.Claims) error {
	return p.c.Call(ctx, rpc.PathLinkAccept, rpc.ClaimsRequest{Claims: *c}, nil)
}

type peerLifter struct{ c *rpc.Client }

func (p peerLifter) TokenActive(ctx context.Context, c *verify.Claims) (bool, error) {
	var ans rpc.ActiveAnswer
	err := p.c.Call(ctx, rpc.PathGuardActive, rpc.ClaimsRequest{Claims: *c}, &ans)
	return ans.Active, err
}

func (p peerLifter) Lift(ctx context.Context, c *verify.Claims) error {
	return p.c.Call(ctx, rpc.PathGuardLift, rpc.ClaimsRequest{Claims: *c}, nil)
}
