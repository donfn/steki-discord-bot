// Package events implements the bot's behaviour: the registration flow, the
// background reaper and reconciler, link moderation, and the moderator slash
// commands.
package events

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/analytics"
	"steki-bot/config"
	"steki-bot/dc"
	"steki-bot/mail"
	"steki-bot/materials"
	"steki-bot/moderation"
	"steki-bot/store"
	"steki-bot/verify"
)

// Handler carries the dependencies shared by every Discord event handler.
type Handler struct {
	cfg    *config.Config
	st     *store.Store
	mailer *mail.Mailer
	log    *slog.Logger
	// an reports interactions to PostHog; nil when analytics is off.
	an *analytics.Client

	linkChk *moderation.LinkChecker
	phish   *moderation.PhishFeed
	modlog  *moderation.Log
	placer  *dc.Placer

	// tokens signs registration and media guard links.
	tokens *verify.Tokens
	// dups tracks recent messages for cross-channel duplicate detection.
	dups *dupTracker
	// msgs remembers recent message text for the edit and delete logs.
	msgs *msgCache
	// invites caches whether an invite code points at the Steki guild.
	invitesMu sync.Mutex
	invites   map[string]bool

	// bucket is the material backup bucket and mat the worker copying into
	// it. Both are nil when no bucket is configured, which turns the backup,
	// the stickies and the material page off. mat is set by Register.
	bucket *materials.Bucket
	mat    *materials.Worker
	// judge checks the reports filed on the material page. It is set by
	// Register when the content check has an API key.
	judge  materials.ReportJudge
	sticky stickyQueue
	// matLive is set once the bucket answered and the worker runs; the
	// stickies and both buttons wait for it.
	matLive atomic.Bool
	// scanning is set while /materials scan runs, so two cannot overlap.
	scanning atomic.Bool
	// materialsOnce starts the backup, the scan and the stickies on the first
	// GUILD_CREATE of the Steki guild.
	materialsOnce sync.Once

	// users serialises all registration work for one member, so concurrent
	// events (a join racing the reconciler, two quick messages) cannot
	// interleave and, for instance, create two channels.
	users keyedMutex

	// gate rate-limits abuse-prone actions across registrations.
	gate rateGates

	bootTime    time.Time
	guild       atomic.Value // string: the Steki guild, see onGuildCreate
	processed   atomic.Int64
	blocked     atomic.Int64
	statusIndex atomic.Int64
	reconciling atomic.Bool

	// base is cancelled by Stop, which aborts in-flight work on shutdown.
	base      context.Context
	cancel    context.CancelFunc
	startOnce sync.Once
	wg        sync.WaitGroup
}

// NewHandler wires the handler's dependencies.
func NewHandler(cfg *config.Config, st *store.Store, logger *slog.Logger) *Handler {
	modlog := moderation.NewLog(config.LogChannelID, logger)
	base, cancel := context.WithCancel(context.Background())
	phish := moderation.NewPhishFeed(logger)
	var bucket *materials.Bucket
	if b := cfg.Bucket; b.Enabled() {
		var err error
		if bucket, err = materials.NewBucket(b.Endpoint, b.Region, b.AccessKeyID, b.SecretAccessKey, b.Name); err != nil {
			logger.Error("material backup off: bucket misconfigured", "error", err)
		}
	} else {
		logger.Info("BUCKET_* not set; material backup, stickies and the material page are off")
	}
	return &Handler{
		bucket:   bucket,
		tokens:   verify.NewTokens(cfg.JWTSecret),
		cfg:      cfg,
		st:       st,
		mailer:   mail.New(cfg.ResendAPIKey, cfg.EmailFrom, logger),
		log:      logger,
		linkChk:  moderation.NewLinkChecker(cfg.URLBlacklist, phish, modlog, logger),
		phish:    phish,
		dups:     newDupTracker(),
		msgs:     newMsgCache(msgCacheSize),
		invites:  map[string]bool{},
		modlog:   modlog,
		placer:   dc.NewPlacer(config.RegisterCategoryID, logger),
		bootTime: time.Now(),
		base:     base,
		cancel:   cancel,
	}
}

// Register subscribes the handler to every gateway event it serves.
//
// Each callback is wrapped in a panic guard: discordgo runs handlers on bare
// goroutines with no recovery of its own, so a single nil dereference in any
// handler would otherwise take the whole process down.
func (h *Handler) Register(s *discordgo.Session) {
	if h.bucket != nil {
		var screener materials.Screener = materials.BasicScreener{}
		if h.cfg.GoogleAIToken != "" {
			g := materials.NewGemini(h.cfg.GoogleAIToken, h.cfg.GoogleAIModel, h.log)
			screener, h.judge = g, g
			h.log.Info("image content check enabled")
		}
		h.mat = materials.NewWorker(h.st, h.bucket, screener, h.refreshMaterialSource(s),
			func(home string) { h.refreshSticky(s, home) },
			func() { go h.safely("review_cards", func() { h.announceReviews(s) }) },
			func(ctx context.Context) ([]string, error) { return h.courseChannelIDs(ctx, s) }, h.log)
	}
	s.AddHandler(guard(h, "ready", h.onReady))
	s.AddHandler(guard(h, "guild_create", h.onGuildCreate))
	s.AddHandler(guard(h, "disconnect", h.onDisconnect))
	s.AddHandler(guard(h, "resumed", h.onResumed))
	s.AddHandler(guard(h, "message_create", h.onMessage))
	s.AddHandler(guard(h, "message_update", h.onMessageUpdate))
	s.AddHandler(guard(h, "message_delete", h.onMessageDelete))
	s.AddHandler(guard(h, "message_delete_bulk", h.onMessageDeleteBulk))
	s.AddHandler(guard(h, "raw_event", h.onRawEvent))
	s.AddHandler(guard(h, "member_add", h.onMemberAdd))
	s.AddHandler(guard(h, "member_remove", h.onMemberRemove))
	s.AddHandler(guard(h, "reaction_add", h.onReactionAdd))
	s.AddHandler(guard(h, "reaction_remove", h.onReactionRemove))
	s.AddHandler(guard(h, "interaction_create", h.onInteractionCreate))
}

// guard adapts a handler method into a discordgo callback that cannot panic.
// The returned function keeps the concrete signature discordgo dispatches on.
func guard[E any](h *Handler, name string, fn func(*discordgo.Session, E)) func(*discordgo.Session, E) {
	return func(s *discordgo.Session, e E) {
		h.safely(name, func() { fn(s, e) })
	}
}

// Stop cancels in-flight work, stops the background loops and waits for them.
func (h *Handler) Stop() {
	h.cancel()
	h.wg.Wait()
}

// ctx returns a context for one short database or Discord exchange.
func (h *Handler) ctx() (context.Context, context.CancelFunc) {
	return h.ctxTimeout(dbTimeout)
}

// ctxTimeout returns a context bounded by d that is also cancelled on shutdown.
func (h *Handler) ctxTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(h.base, d)
}

// lockUser serialises registration work for one member.
func (h *Handler) lockUser(userID string) (unlock func()) {
	return h.users.Lock(userID)
}

// guildID returns the Steki guild, as learned from its GUILD_CREATE. It is
// empty until that event arrives.
func (h *Handler) guildID() string {
	if v, ok := h.guild.Load().(string); ok {
		return v
	}
	return ""
}

// inHomeGuild reports whether an event belongs to the Steki guild. The bot is
// a member of more than one server, and registration, the reaper and the
// reconciler must only ever act on the one that owns the register category.
func (h *Handler) inHomeGuild(guildID string) bool {
	return guildID != "" && guildID == h.guildID()
}

// goLoop runs fn on a ticker until shutdown, invoking it once immediately.
func (h *Handler) goLoop(name string, every time.Duration, fn func()) {
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()

		ticker := time.NewTicker(every)
		defer ticker.Stop()

		for {
			h.safely(name, fn)
			select {
			case <-h.base.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// goLoopAfter is goLoop for work whose first run is already done: fn first
// runs after one interval.
func (h *Handler) goLoopAfter(name string, every time.Duration, fn func()) {
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()

		ticker := time.NewTicker(every)
		defer ticker.Stop()

		for {
			select {
			case <-h.base.Done():
				return
			case <-ticker.C:
				h.safely(name, fn)
			}
		}
	}()
}

// safely runs fn, turning a panic into a log line rather than a dead process.
func (h *Handler) safely(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			h.log.Error("recovered from panic", "where", name, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	fn()
}

// alertMods posts to the moderator log channel. Used for every failure a
// moderator has to know about — most importantly a registration channel that
// could not be created, which previously vanished into stdout.
func (h *Handler) alertMods(s *discordgo.Session, format string, args ...any) {
	h.modlog.Post(s, fmt.Sprintf(format, args...))
}

// alertModsOnce is alertMods limited to one post per key per alertEvery, so a
// member repeating an action cannot flood the mod log.
func (h *Handler) alertModsOnce(s *discordgo.Session, key, format string, args ...any) {
	if h.gate.allow("alert:"+key, 1, alertEvery) {
		h.alertMods(s, format, args...)
	}
}

// audit writes an audit event, logging rather than propagating failures: the
// trail must never break the flow it describes.
func (h *Handler) audit(userID, regID, kind string, detail map[string]any, actorID string) {
	// Every registration step, moderator action and upload passes through
	// here, so this one call gives analytics the whole funnel.
	h.track(userID, "audit_"+kind, auditProps(regID, actorID, detail))
	ctx, cancel := h.ctx()
	defer cancel()
	if err := h.st.LogEvent(ctx, userID, regID, kind, detail, actorID); err != nil {
		h.log.Error("audit write failed", "kind", kind, "user_id", userID, "error", err)
	}
}

// retry is a convenience wrapper so handlers do not each import dc.
func (h *Handler) retry(ctx context.Context, op string, fn func() error) error {
	return dc.Do(ctx, h.log, op, fn)
}

// refreshPhishFeed reloads the phishing domain feeds, keeping the previous
// list when the download fails.
func (h *Handler) refreshPhishFeed() {
	ctx, cancel := h.ctxTimeout(2 * time.Minute)
	defer cancel()
	if err := h.phish.Refresh(ctx); err != nil {
		h.log.Warn("phishing feed refresh failed", "error", err)
	}
}
