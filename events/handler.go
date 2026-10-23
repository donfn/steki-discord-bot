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

	"steki-bot/config"
	"steki-bot/dc"
	"steki-bot/mail"
	"steki-bot/moderation"
	"steki-bot/store"
)

// Handler carries the dependencies shared by every Discord event handler.
type Handler struct {
	cfg    *config.Config
	st     *store.Store
	mailer *mail.Mailer
	log    *slog.Logger

	linkChk *moderation.LinkChecker
	modlog  *moderation.Log
	placer  *dc.Placer

	// users serialises all registration work for one member, so concurrent
	// events (a join racing the reconciler, two quick messages) cannot
	// interleave and, for instance, create two channels.
	users keyedMutex

	bootTime    time.Time
	guild       atomic.Value // string, learned from the gateway
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
	return &Handler{
		cfg:      cfg,
		st:       st,
		mailer:   mail.New(cfg.ResendAPIKey, cfg.EmailFrom, logger),
		log:      logger,
		linkChk:  moderation.NewLinkChecker(cfg.URLBlacklist, modlog, logger),
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
	s.AddHandler(guard(h, "ready", h.onReady))
	s.AddHandler(guard(h, "guild_create", h.onGuildCreate))
	s.AddHandler(guard(h, "disconnect", h.onDisconnect))
	s.AddHandler(guard(h, "resumed", h.onResumed))
	s.AddHandler(guard(h, "message_create", h.onMessage))
	s.AddHandler(guard(h, "message_update", h.onMessageUpdate))
	s.AddHandler(guard(h, "message_delete", h.onMessageDelete))
	s.AddHandler(guard(h, "member_add", h.onMemberAdd))
	s.AddHandler(guard(h, "member_remove", h.onMemberRemove))
	s.AddHandler(guard(h, "reaction_add", h.onReactionAdd))
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

// guildID returns the guild the bot is operating in, as learned from the
// gateway. It is empty until the first Ready or Guild Create.
func (h *Handler) guildID() string {
	if v, ok := h.guild.Load().(string); ok {
		return v
	}
	return ""
}

func (h *Handler) setGuildID(id string) {
	if id != "" && h.guildID() == "" {
		h.guild.Store(id)
	}
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

// audit writes an audit event, logging rather than propagating failures: the
// trail must never break the flow it describes.
func (h *Handler) audit(userID, regID, kind string, detail map[string]any, actorID string) {
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
