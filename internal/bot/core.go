// Package bot is what the three services share as Discord bots: the session, the
// panic guard, the home guild, the moderators' log channel, and the reply and
// lookup helpers. Each service embeds a Core and adds only its own handlers.
package bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/internal/analytics"
	"steki-bot/internal/config"
	"steki-bot/internal/dc"
	"steki-bot/internal/moderation"
)

// AuditLog is where a service keeps its part of the audit trail.
type AuditLog interface {
	LogEvent(ctx context.Context, userID, regID, kind string, detail map[string]any, actorID string) error
}

// Core carries the dependencies every event handler shares.
type Core struct {
	Log *slog.Logger
	// An reports interactions to PostHog; nil when analytics is off.
	An     *analytics.Client
	Modlog *moderation.Log
	// Audits receives Audit calls.
	Audits AuditLog
	// Gate rate-limits abuse-prone actions.
	Gate RateGates
	// Jobs stops an on-demand command from running twice at once.
	Jobs Jobs
	// OnHome runs once, on the first GUILD_CREATE of the Steki guild.
	OnHome func(*discordgo.Session)

	// users serialises work on one member, so concurrent events cannot
	// interleave and, for instance, create two channels. It holds within a
	// service only; across services each owner's database decides.
	users    KeyedMutex
	homeOnce sync.Once
	guild    atomic.Value // string: the Steki guild, see onGuildCreate

	BootTime time.Time

	// Base is cancelled by Stop, which aborts in-flight work on shutdown.
	Base   context.Context
	cancel context.CancelFunc
	WG     sync.WaitGroup
}

// NewCore returns a Core logging to the moderators' channel through logger.
func NewCore(logger *slog.Logger, an *analytics.Client, audits AuditLog) *Core {
	base, cancel := context.WithCancel(context.Background())
	return &Core{
		Log: logger, An: an, Audits: audits, Modlog: moderation.NewLog(config.LogChannelID, logger),
		BootTime: time.Now(), Base: base, cancel: cancel,
	}
}

// Register subscribes to the events every service needs.
func (c *Core) Register(s *discordgo.Session) {
	s.AddHandler(Guard(c, "ready", c.onReady))
	s.AddHandler(Guard(c, "guild_create", c.onGuildCreate))
	s.AddHandler(Guard(c, "disconnect", c.onDisconnect))
	s.AddHandler(Guard(c, "resumed", c.onResumed))
}

// Guard adapts a handler method into a discordgo callback that cannot panic:
// discordgo runs handlers on bare goroutines with no recovery of its own. The
// returned function keeps the concrete signature discordgo dispatches on.
func Guard[E any](c *Core, name string, fn func(*discordgo.Session, E)) func(*discordgo.Session, E) {
	return func(s *discordgo.Session, e E) {
		c.Safely(name, func() { fn(s, e) })
	}
}

func (c *Core) onReady(_ *discordgo.Session, r *discordgo.Ready) {
	c.Log.Info("gateway ready", "user", r.User.Username, "guilds", len(r.Guilds), "session", r.SessionID)
}

// onGuildCreate fires for every guild after each new gateway session. The Steki
// guild is recognised by owning the register category: the bot is in more than
// one server, so the ID cannot simply be taken from the first event.
func (c *Core) onGuildCreate(s *discordgo.Session, g *discordgo.GuildCreate) {
	if !IsHomeGuild(g.Guild) {
		return
	}
	c.guild.Store(g.ID)
	c.Log.Info("home guild available", "guild_id", g.ID, "name", g.Name)
	if c.OnHome != nil {
		c.homeOnce.Do(func() { c.OnHome(s) })
	}
}

// IsHomeGuild reports whether g is the Steki guild.
func IsHomeGuild(g *discordgo.Guild) bool {
	return g != nil && slices.ContainsFunc(g.Channels, func(c *discordgo.Channel) bool {
		return c.ID == config.RegisterCategoryID
	})
}

// SetGuild sets the home guild, for tests.
func (c *Core) SetGuild(id string) { c.guild.Store(id) }

// onDisconnect and onResumed make gateway health visible in the logs, since
// silent reconnects were masking dropped joins.
func (c *Core) onDisconnect(_ *discordgo.Session, _ *discordgo.Disconnect) {
	c.Log.Warn("gateway disconnected")
}

// A resume replays the events missed while the connection was down, so there
// is nothing to catch up on.
func (c *Core) onResumed(_ *discordgo.Session, _ *discordgo.Resumed) {
	c.Log.Info("gateway resumed")
}

// Stop cancels in-flight work, stops the background loops and waits for them.
func (c *Core) Stop() {
	c.cancel()
	c.WG.Wait()
}

// Ctx returns a context for one short database or Discord exchange.
func (c *Core) Ctx() (context.Context, context.CancelFunc) {
	return c.CtxTimeout(DBTimeout)
}

// CtxTimeout returns a context bounded by d that is also cancelled on shutdown.
func (c *Core) CtxTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(c.Base, d)
}

// LockUser serialises work on one member.
func (c *Core) LockUser(userID string) (unlock func()) {
	return c.users.Lock(userID)
}

// GuildID returns the Steki guild, as learned from its GUILD_CREATE. It is
// empty until that event arrives.
func (c *Core) GuildID() string {
	if v, ok := c.guild.Load().(string); ok {
		return v
	}
	return ""
}

// InHomeGuild reports whether an event belongs to the Steki guild. The bot is
// a member of more than one server.
func (c *Core) InHomeGuild(guildID string) bool {
	return guildID != "" && guildID == c.GuildID()
}

// GoLoop runs fn on a ticker until shutdown, invoking it once immediately.
func (c *Core) GoLoop(name string, every time.Duration, fn func()) {
	c.WG.Add(1)
	go func() {
		defer c.WG.Done()

		ticker := time.NewTicker(every)
		defer ticker.Stop()

		for {
			c.Safely(name, fn)
			select {
			case <-c.Base.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// Safely runs fn, turning a panic into a log line rather than a dead process.
func (c *Core) Safely(name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			c.Log.Error("recovered from panic", "where", name, "panic", r, "stack", string(debug.Stack()))
		}
	}()
	fn()
}

// AlertMods posts to the moderator log channel. Used for every failure a
// moderator has to know about. The post is silent; UrgentMods is for the ones an
// admin has to act on.
func (c *Core) AlertMods(s *discordgo.Session, format string, args ...any) {
	c.Modlog.Post(s, fmt.Sprintf(format, args...))
}

// UrgentMods posts to the moderator log channel with a notification.
func (c *Core) UrgentMods(s *discordgo.Session, format string, args ...any) {
	c.Modlog.Alert(s, fmt.Sprintf(format, args...))
}

// AlertModsOnce is AlertMods limited to one post per key per AlertEvery, so a
// member repeating an action cannot flood the mod log.
func (c *Core) AlertModsOnce(s *discordgo.Session, key, format string, args ...any) {
	if c.Gate.Allow("alert:"+key, 1, AlertEvery) {
		c.AlertMods(s, format, args...)
	}
}

// UrgentModsOnce is UrgentMods limited the same way.
func (c *Core) UrgentModsOnce(s *discordgo.Session, key, format string, args ...any) {
	if c.Gate.Allow("alert:"+key, 1, AlertEvery) {
		c.UrgentMods(s, format, args...)
	}
}

// Audit writes an audit event, logging rather than propagating failures: the
// trail must never break the flow it describes.
func (c *Core) Audit(userID, regID, kind string, detail map[string]any, actorID string) {
	// Every registration step, moderator action and upload passes through
	// here, so this one call gives analytics the whole funnel.
	c.Track("audit_"+kind, detail)
	ctx, cancel := c.Ctx()
	defer cancel()
	if err := c.Audits.LogEvent(ctx, userID, regID, kind, detail, actorID); err != nil {
		c.Log.Error("audit write failed", "kind", kind, "user_id", userID, "error", err)
	}
}

// Retry is a convenience wrapper so handlers do not each import dc.
func (c *Core) Retry(ctx context.Context, op string, fn func() error) error {
	return dc.Do(ctx, c.Log, op, fn)
}

// NewTokenID returns a random ID for a signed link.
func NewTokenID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// NewSession opens no connection yet: it returns a session asking for intents
// and tracking only what the bot reads. Presences and voice states are not even
// subscribed to, and emojis, stickers and threads are never looked at, so tracking
// them would only fill the heap. MaxMessageCount stays 0.
func NewSession(token string, intents discordgo.Intent) (*discordgo.Session, error) {
	sess, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}
	sess.Identify.Intents = intents
	sess.State.TrackPresences = false
	sess.State.TrackVoice = false
	sess.State.TrackEmojis = false
	sess.State.TrackStickers = false
	sess.State.TrackThreads = false
	sess.State.TrackThreadMembers = false
	return sess, nil
}

// SkipMemberCache keeps the guild's member list out of the gateway state. The
// state would hold every member for the life of the process, and it never leaves
// room for the lookups the services make (they fall back to the API). A member
// whose roles change is dropped too, so what a later lookup caches is current.
func (c *Core) SkipMemberCache(s *discordgo.Session) {
	s.State.TrackMembers = false
	s.AddHandler(Guard(c, "member_cache_fill", func(s *discordgo.Session, g *discordgo.GuildCreate) {
		for _, m := range slices.Clone(g.Members) {
			_ = s.State.MemberRemove(m)
		}
	}))
	s.AddHandler(Guard(c, "member_cache_update", func(s *discordgo.Session, u *discordgo.GuildMemberUpdate) {
		_ = s.State.MemberRemove(u.Member)
	}))
}

// SlimMember keeps only what reconcile reads: who the member is, their
// roles and when they joined.
func SlimMember(m *discordgo.Member) *discordgo.Member {
	s := &discordgo.Member{GuildID: m.GuildID, Roles: m.Roles, JoinedAt: m.JoinedAt}
	if u := m.User; u != nil {
		s.User = &discordgo.User{
			ID: u.ID, Username: u.Username, GlobalName: u.GlobalName,
			Discriminator: u.Discriminator, Avatar: u.Avatar, Bot: u.Bot,
		}
	}
	return s
}

// AllMembers pages through the guild's member list.
func (c *Core) AllMembers(ctx context.Context, s *discordgo.Session, guildID string) ([]*discordgo.Member, error) {
	var all []*discordgo.Member
	after := ""

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		var page []*discordgo.Member
		err := c.Retry(ctx, "guild.members", func() error {
			p, perr := s.GuildMembers(guildID, after, 1000, discordgo.WithContext(ctx))
			if perr != nil {
				return perr
			}
			page = p
			return nil
		})
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			return all, nil
		}
		// Keep a trimmed copy so each decoded page, with its avatar hashes,
		// flags and banners, can be collected as soon as the next one loads.
		for _, m := range page {
			all = append(all, SlimMember(m))
		}

		last := page[len(page)-1]
		if last.User == nil || len(page) < 1000 {
			return all, nil
		}
		after = last.User.ID
	}
}
