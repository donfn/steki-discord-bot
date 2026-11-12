// Package moderation is the service that keeps the server clean: it screens every
// message, runs the media guard and the ghost role, logs edits and deletions, and
// answers /guard, /member, /rules and /bot. It holds no member's address and no
// file: closing an address goes through the registration service.
package moderation

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/internal/analytics"
	"steki-bot/internal/bot"
	"steki-bot/internal/config"
	"steki-bot/internal/moderation"
	"steki-bot/internal/rpc"
	"steki-bot/internal/store"
	"steki-bot/internal/tokens"
)

// Intents are the gateway events this service reads.
const Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMembers |
	discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent |
	discordgo.IntentsGuildMessageReactions

// Handler carries the service's dependencies.
type Handler struct {
	*bot.Core
	cfg      *config.Moderation
	st       *store.Guard
	activity *store.Activity
	tokens   *tokens.Tokens

	linkChk *moderation.LinkChecker
	phish   *moderation.PhishFeed
	// dups tracks recent messages for cross-channel duplicate detection.
	dups *dupTracker
	// repeats tracks images and links a member sends more than once.
	repeats *dupTracker
	// cdn downloads attachments to fingerprint them; fetch bounds how many at once.
	cdn   *http.Client
	fetch chan struct{}
	// msgs remembers recent message text for the edit and delete logs.
	msgs *msgCache
	// invites caches whether an invite code points at the Steki guild.
	invitesMu sync.Mutex
	invites   map[string]bool

	registration *rpc.Client
	web          *rpc.Client

	processed   atomic.Int64
	blocked     atomic.Int64
	statusIndex atomic.Int64
}

// NewHandler wires the handler's dependencies.
func NewHandler(cfg *config.Moderation, st *store.Guard, activity *store.Activity, an *analytics.Client, logger *slog.Logger) *Handler {
	core := bot.NewCore(logger, an, st)
	phish := moderation.NewPhishFeed(logger)
	return &Handler{
		Core:         core,
		cfg:          cfg,
		st:           st,
		activity:     activity,
		tokens:       tokens.NewIssuer(cfg.InternalSecret),
		linkChk:      moderation.NewLinkChecker(cfg.URLBlacklist, phish, core.Modlog, logger),
		phish:        phish,
		dups:         newDupTracker(),
		repeats:      newRepeatTracker(),
		cdn:          &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		fetch:        make(chan struct{}, intelFetchers),
		msgs:         newMsgCache(msgCacheSize),
		invites:      map[string]bool{},
		registration: rpc.NewClient(cfg.RegistrationURL, cfg.InternalSecret),
		web:          rpc.NewClient(cfg.WebURL, cfg.InternalSecret),
	}
}

// Register subscribes the handler to the gateway events it serves.
func (h *Handler) Register(s *discordgo.Session) {
	h.Core.Register(s)
	h.SkipMemberCache(s)
	h.OnHome = func(s *discordgo.Session) {
		h.GoLoop("status", 30*time.Second, func() { h.updateStatus(s) })
		h.GoLoop("phishing_feed", phishRefreshEvery, h.refreshPhishFeed)
		h.GoLoop("duplicate_sweep", dupWindow, func() {
			now := time.Now()
			h.dups.sweep(now)
			h.repeats.sweep(now)
		})
		h.GoLoop("ghost_sweep", ghostSweepEvery, func() { h.sweepGhosts(s) })
		h.WG.Add(1)
		go func() {
			defer h.WG.Done()
			h.installCommands(s)
		}()
	}
	s.AddHandler(bot.Guard(h.Core, "message_create", h.onMessage))
	s.AddHandler(bot.Guard(h.Core, "message_update", h.onMessageUpdate))
	s.AddHandler(bot.Guard(h.Core, "message_delete", h.onMessageDelete))
	s.AddHandler(bot.Guard(h.Core, "message_delete_bulk", h.onMessageDeleteBulk))
	s.AddHandler(bot.Guard(h.Core, "member_add", h.onMemberAdd))
	s.AddHandler(bot.Guard(h.Core, "member_remove", h.onMemberRemove))
	s.AddHandler(bot.Guard(h.Core, "reaction_add", h.onReactionAdd))
	s.AddHandler(bot.Guard(h.Core, "reaction_remove", h.onReactionRemove))
	s.AddHandler(bot.Guard(h.Core, "interaction_create", h.onInteractionCreate))
}

// installCommands keeps trying until the commands are installed: without them the
// moderators have no way to operate any of the services.
func (h *Handler) installCommands(s *discordgo.Session) {
	for {
		var err error
		h.Safely("register_commands", func() { err = h.registerCommands(s) })
		if err == nil {
			return
		}
		h.Log.Error("register slash commands failed", "error", err)
		select {
		case <-h.Base.Done():
			return
		case <-time.After(time.Minute):
		}
	}
}

// registerCommands installs every service's commands on the guild. Guild commands
// update instantly, unlike global ones. Discord replaces the whole set at once, so
// this service does it for all three.
func (h *Handler) registerCommands(s *discordgo.Session) error {
	guildID := h.GuildID()
	if guildID == "" || s.State.User == nil {
		return nil
	}
	ctx, cancel := h.Ctx()
	defer cancel()
	return h.Retry(ctx, "commands.overwrite", func() error {
		_, err := s.ApplicationCommandBulkOverwrite(
			s.State.User.ID, guildID, bot.Commands(), discordgo.WithContext(ctx))
		return err
	})
}

// onMessageDeleteBulk is a moderator purging many messages at once.
func (h *Handler) onMessageDeleteBulk(_ *discordgo.Session, m *discordgo.MessageDeleteBulk) {
	for _, id := range m.Messages {
		h.msgs.take(id)
	}
	if h.InHomeGuild(m.GuildID) {
		h.Track("discord_messages_bulk_deleted", map[string]any{
			"guild_id": m.GuildID, "channel_id": m.ChannelID, "count": len(m.Messages),
		})
	}
}

// onMemberAdd keeps the raid alarm and the activity index. The new member's
// register channel is the registration service's.
func (h *Handler) onMemberAdd(s *discordgo.Session, m *discordgo.GuildMemberAdd) {
	if m.User == nil || m.User.Bot || !h.InHomeGuild(m.GuildID) {
		return
	}
	if !h.Gate.Allow("joins", joinsPerWindow, joinWindow) {
		h.UrgentModsOnce(s, "raid", "🚨 Ασυνήθιστα πολλές είσοδοι (πάνω από %d σε %d λεπτά). Ίσως πρόκειται για raid· ελέγξτε τον server.",
			joinsPerWindow, int(joinWindow.Minutes()))
	}
	h.noteJoin(m.Member)
}

func (h *Handler) onMemberRemove(_ *discordgo.Session, m *discordgo.GuildMemberRemove) {
	if m.User == nil || !h.InHomeGuild(m.GuildID) {
		return
	}
	h.Track("discord_member_left", map[string]any{"guild_id": m.GuildID})
	h.noteLeave(m.User.ID)
}

// reactionProps describes a reaction by its emoji and where it was left.
func reactionProps(r *discordgo.MessageReaction) map[string]any {
	return map[string]any{
		"guild_id": r.GuildID, "channel_id": r.ChannelID,
		"emoji": r.Emoji.Name, "emoji_custom": r.Emoji.ID != "",
	}
}

func (h *Handler) onReactionAdd(s *discordgo.Session, r *discordgo.MessageReactionAdd) {
	if h.InHomeGuild(r.GuildID) && (s.State.User == nil || r.UserID != s.State.User.ID) {
		h.Track("discord_reaction_added", reactionProps(r.MessageReaction))
	}
}

func (h *Handler) onReactionRemove(s *discordgo.Session, r *discordgo.MessageReactionRemove) {
	if h.InHomeGuild(r.GuildID) && (s.State.User == nil || r.UserID != s.State.User.ID) {
		h.Track("discord_reaction_removed", reactionProps(r.MessageReaction))
	}
}

// updateStatus rotates the presence text. Only this service sets one: every
// session shows the same bot, and the counters are here.
//
// The previous implementation ticked every ten seconds regardless of
// connection state, producing a steady stream of
// "rotateStatus: no websocket connection exists" errors while reconnecting.
func (h *Handler) updateStatus(s *discordgo.Session) {
	s.RLock()
	ready := s.DataReady
	s.RUnlock()
	if !ready {
		return
	}

	var status string
	switch h.statusIndex.Add(1) % 3 {
	case 0:
		status = "Uptime: " + formatUptime(time.Since(h.BootTime))
	case 1:
		status = fmt.Sprintf("Κακόβουλα μηνύματα: %d", h.blocked.Load())
	case 2:
		status = fmt.Sprintf("Επεξεργασμένα μηνύματα: %d", h.processed.Load())
	}

	if err := s.UpdateGameStatus(0, status); err != nil {
		h.Log.Debug("update status failed", "error", err)
	}
}

// formatUptime renders a duration as "Nd Nh Nm".
func formatUptime(d time.Duration) string {
	return fmt.Sprintf("%dd %dh %dm", int(d.Hours())/24, int(d.Hours())%24, int(d.Minutes())%60)
}

// refreshPhishFeed reloads the phishing domain feeds, keeping the previous
// list when the download fails.
func (h *Handler) refreshPhishFeed() {
	ctx, cancel := h.CtxTimeout(2 * time.Minute)
	defer cancel()
	if err := h.phish.Refresh(ctx); err != nil {
		h.Log.Warn("phishing feed refresh failed", "error", err)
	}
}

// commandFunc handles one subcommand. The reply has already been deferred.
type commandFunc func(h *Handler, s *discordgo.Session, i *discordgo.InteractionCreate,
	sub *discordgo.ApplicationCommandInteractionDataOption)

// commandRoutes maps "command subcommand" to its handler.
var commandRoutes = map[string]commandFunc{
	"bot stats":      (*Handler).cmdBotStats,
	"member ban":     (*Handler).cmdMemberBan,
	"member unban":   (*Handler).cmdMemberUnban,
	"rules post":     (*Handler).cmdRulesPost,
	"guard status":   (*Handler).cmdGuardStatus,
	"guard restrict": (*Handler).cmdGuardRestrict,
	"guard ungrace":  (*Handler).cmdGuardUngrace,
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
		// The web service already counts every button press as an interaction.
		if strings.HasPrefix(i.MessageComponentData().CustomID, intelPrefix) {
			h.onIntelComponent(s, i)
		}
	case discordgo.InteractionApplicationCommand:
		sub, path, ok := h.BeginCommand(s, i, func(p string) bool { _, ok := commandRoutes[p]; return ok })
		if ok {
			commandRoutes[path](h, s, i, sub)
		}
	}
}

// API is the service's internal API.
func (h *Handler) API(srv *rpc.Server, s *discordgo.Session) {
	lift := guardLifter{h: h, s: s}
	rpc.Handle(srv, rpc.PathGuardActive, func(ctx context.Context, in rpc.ClaimsRequest) (rpc.ActiveAnswer, error) {
		ok, err := lift.TokenActive(ctx, &in.Claims)
		return rpc.ActiveAnswer{Active: ok}, err
	})
	rpc.Handle(srv, rpc.PathGuardLift, func(ctx context.Context, in rpc.ClaimsRequest) (rpc.None, error) {
		return rpc.None{}, lift.Lift(ctx, &in.Claims)
	})
	rpc.Handle(srv, rpc.PathScreenTitle, func(_ context.Context, in rpc.ScreenTitleRequest) (rpc.ScreenTitleAnswer, error) {
		return h.screenTitle(s, in), nil
	})
	rpc.Handle(srv, rpc.PathRosterSync, func(_ context.Context, _ rpc.None) (rpc.None, error) {
		go h.Safely("roster_sync", func() { h.syncRoster(s) })
		return rpc.None{}, nil
	})
	rpc.Handle(srv, rpc.PathForget, func(ctx context.Context, in rpc.UserRequest) (rpc.None, error) {
		return rpc.None{}, h.st.ForgetMember(ctx, in.UserID)
	})
	rpc.Handle(srv, rpc.PathEvents, func(ctx context.Context, in rpc.EventsRequest) (rpc.EventsAnswer, error) {
		ev, err := h.st.History(ctx, in.UserID, in.Limit)
		return rpc.EventsAnswer{Events: ev}, err
	})
}

// screenTitle checks an upload's title the way onMessage checks a message, for
// the web service, which keeps no phishing list of its own.
func (h *Handler) screenTitle(s *discordgo.Session, in rpc.ScreenTitleRequest) rpc.ScreenTitleAnswer {
	var ans rpc.ScreenTitleAnswer
	if matched, ok := h.linkChk.Flag(in.Title); ok {
		ans.Flagged, ans.Matched = true, matched
	}
	if !in.Staff {
		for _, m := range inviteRegex.FindAllStringSubmatch(in.Title, -1) {
			if !h.isHomeInvite(s, strings.ToLower(m[1]), m[2]) {
				ans.ForeignInvite = true
				break
			}
		}
	}
	return ans
}

// syncRoster refreshes the activity index from a full member list.
func (h *Handler) syncRoster(s *discordgo.Session) {
	guildID := h.GuildID()
	if guildID == "" {
		return
	}
	end, ok := h.Jobs.Begin("roster")
	if !ok {
		return
	}
	defer end()
	ctx, cancel := h.CtxTimeout(bot.ReconcileTimeout)
	defer cancel()
	listedAt := time.Now()
	members, err := h.AllMembers(ctx, s, guildID)
	if err != nil {
		h.Log.Error("ghosts: list members", "error", err)
		return
	}
	h.syncActivityRoster(ctx, members, listedAt)
}

// statsOf asks one service for its part of /bot stats.
func (h *Handler) statsOf(ctx context.Context, name string, c *rpc.Client) string {
	var ans rpc.StatsAnswer
	if err := c.Call(ctx, rpc.PathStats, rpc.None{}, &ans); err != nil {
		h.Log.Warn("stats: service failed", "service", name, "error", err)
		return fmt.Sprintf("\n**%s**\n• 🔴 Δεν απαντά: %v\n", name, err)
	}
	return fmt.Sprintf("\n**%s**\n%s", name, ans.Text)
}
