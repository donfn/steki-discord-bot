// Package registration is the service that lets members in: their register
// channels, the email link, the abandonment reaper, and /reg and /email. It alone
// holds the Resend key and the members' addresses.
package registration

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/analytics"
	"steki-bot/bot"
	"steki-bot/config"
	"steki-bot/dc"
	"steki-bot/mail"
	"steki-bot/rpc"
	"steki-bot/store"
	"steki-bot/tokens"
)

// Intents are the gateway events this service reads.
const Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMembers |
	discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent |
	discordgo.IntentsGuildMessageReactions

// Handler carries the service's dependencies.
type Handler struct {
	*bot.Core
	cfg    *config.Registration
	st     *store.Registry
	mailer *mail.Mailer
	placer *dc.Placer
	tokens *tokens.Tokens
	// moderation and web are the other services' internal APIs.
	moderation *rpc.Client
	web        *rpc.Client
	// lastReconcile is the Unix second of the last finished /reg reconcile.
	lastReconcile atomic.Int64
}

// NewHandler wires the handler's dependencies.
func NewHandler(cfg *config.Registration, st *store.Registry, an *analytics.Client, logger *slog.Logger) *Handler {
	return &Handler{
		Core:       bot.NewCore(logger, an, st),
		cfg:        cfg,
		st:         st,
		mailer:     mail.New(cfg.ResendAPIKey, cfg.EmailFrom, logger),
		placer:     dc.NewPlacer(config.RegisterCategoryID, logger),
		tokens:     tokens.NewIssuer(cfg.InternalSecret),
		moderation: rpc.NewClient(cfg.ModerationURL, cfg.InternalSecret),
		web:        rpc.NewClient(cfg.WebURL, cfg.InternalSecret),
	}
}

// Register subscribes the handler to the gateway events it serves.
func (h *Handler) Register(s *discordgo.Session) {
	h.Core.Register(s)
	h.OnHome = func(s *discordgo.Session) { h.startMaintenance(s) }
	s.AddHandler(bot.Guard(h.Core, "message_create", h.onMessage))
	s.AddHandler(bot.Guard(h.Core, "member_add", h.onMemberAdd))
	s.AddHandler(bot.Guard(h.Core, "member_remove", h.onMemberRemove))
	s.AddHandler(bot.Guard(h.Core, "reaction_add", h.onReactionAdd))
	s.AddHandler(bot.Guard(h.Core, "interaction_create", h.onInteractionCreate))
}

// onMessage drives the flow in register channels, for the ones opened before
// they became read-only. Screening messages is the moderation service's.
func (h *Handler) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot || !h.InHomeGuild(m.GuildID) {
		return
	}
	ctx, cancel := h.Ctx()
	ch, err := h.Channel(ctx, s, m.ChannelID)
	cancel()
	if err != nil || !bot.IsRegisterChannel(ch) {
		return
	}
	h.handleRegistration(s, m)
}

// commandFunc handles one subcommand. The reply has already been deferred.
type commandFunc func(h *Handler, s *discordgo.Session, i *discordgo.InteractionCreate,
	sub *discordgo.ApplicationCommandInteractionDataOption)

// commandRoutes maps "command subcommand" to its handler.
var commandRoutes = map[string]commandFunc{
	"reg status":     (*Handler).cmdRegStatus,
	"reg history":    (*Handler).cmdRegHistory,
	"reg stuck":      (*Handler).cmdRegStuck,
	"reg restart":    (*Handler).cmdRegRestart,
	"reg verify":     (*Handler).cmdRegVerify,
	"reg reregister": (*Handler).cmdRegReregister,
	"reg forget":     (*Handler).cmdRegForget,
	"reg reconcile":  (*Handler).cmdRegReconcile,
	"email whois":    (*Handler).cmdEmailWhois,
	"email list":     (*Handler).cmdEmailList,
	"email add":      (*Handler).cmdEmailAdd,
	"email update":   (*Handler).cmdEmailUpdate,
	"email remove":   (*Handler).cmdEmailRemove,
}

// Routes lists the command paths this service answers.
func Routes() []string { return routeKeys(commandRoutes) }

func routeKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// onInteractionCreate takes the commands, buttons and forms that are this
// service's. Every service sees every interaction, and answering one that belongs
// to another would make that service's answer fail.
func (h *Handler) onInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	switch i.Type {
	case discordgo.InteractionMessageComponent:
		if strings.HasPrefix(i.MessageComponentData().CustomID, "reg:") {
			h.Track("discord_interaction", bot.InteractionProps(i))
			h.onComponent(s, i)
		}
	case discordgo.InteractionModalSubmit:
		if d, ok := i.Data.(discordgo.ModalSubmitInteractionData); ok && strings.HasPrefix(d.CustomID, "reg:") {
			h.Track("discord_interaction", bot.InteractionProps(i))
			h.onEmailSubmit(s, i)
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
	consent := linkConsenter{h: h, s: s}
	rpc.Handle(srv, rpc.PathLinkPending, func(ctx context.Context, in rpc.ClaimsRequest) (rpc.ConsentAnswer, error) {
		c, err := consent.Pending(ctx, &in.Claims)
		if err != nil {
			return rpc.ConsentAnswer{}, err
		}
		return rpc.ConsentAnswer{Consent: *c}, nil
	})
	rpc.Handle(srv, rpc.PathLinkAccept, func(ctx context.Context, in rpc.ClaimsRequest) (rpc.None, error) {
		return rpc.None{}, consent.Accept(ctx, &in.Claims)
	})
	rpc.Handle(srv, rpc.PathEmailsBan, func(ctx context.Context, in rpc.BanRequest) (store.BanResult, error) {
		defer h.LockUser(in.UserID)()
		return h.st.BanEmails(ctx, in.UserID, in.Extra, in.Reason, in.By)
	})
	rpc.Handle(srv, rpc.PathEmailsUnban, func(ctx context.Context, in rpc.UnbanRequest) (rpc.UnbanAnswer, error) {
		n, err := h.st.UnbanEmails(ctx, in.UserID, in.Email)
		return rpc.UnbanAnswer{Lifted: n}, err
	})
	rpc.Handle(srv, rpc.PathEvents, func(ctx context.Context, in rpc.EventsRequest) (rpc.EventsAnswer, error) {
		ev, err := h.st.History(ctx, in.UserID, in.Limit)
		return rpc.EventsAnswer{Events: ev}, err
	})
	rpc.Handle(srv, rpc.PathStats, func(ctx context.Context, _ rpc.None) (rpc.StatsAnswer, error) {
		return rpc.StatsAnswer{Text: h.stats(ctx, s)}, nil
	})
}

// stats is this service's part of /bot stats.
func (h *Handler) stats(ctx context.Context, s *discordgo.Session) string {
	var b strings.Builder
	if err := h.st.Ping(ctx); err != nil {
		fmt.Fprintf(&b, "• 🔴 Βάση εγγραφών: %v\n", err)
	} else {
		fmt.Fprintf(&b, "• 🟢 Βάση εγγραφών: %.1f MB\n", float64(h.st.FileSize())/(1<<20))
	}
	if open, err := h.st.ListOpen(ctx); err == nil {
		fmt.Fprintf(&b, "• Ανοιχτές εγγραφές: %d\n", len(open))
	}
	// Nothing catches the bot up on its own, so say what is due.
	if last := h.lastReconcile.Load(); last > 0 {
		fmt.Fprintf(&b, "• Τελευταίος συγχρονισμός εγγραφών: <t:%d:R>\n", last)
	} else {
		b.WriteString("• Συγχρονισμός εγγραφών: δεν έχει τρέξει από την εκκίνηση (`/reg reconcile`)\n")
	}
	if es, err := h.st.EmailStats(ctx); err == nil {
		fmt.Fprintf(&b, "• Emails: %d (με κάτοχο: %d) · παλιά hashes: %d\n",
			es.Verified, es.VerifiedLinked, es.LegacyHashes)
	}
	if occ, err := h.placer.Occupancies(ctx, s, h.GuildID()); err == nil {
		b.WriteString("\n**Κατηγορίες εγγραφής**\n")
		for _, o := range occ {
			warn := ""
			if o.Used >= o.Capacity-5 {
				warn = " ⚠️"
			}
			fmt.Fprintf(&b, "• `%s`: %d/%d%s\n", o.Name, o.Used, o.Capacity, warn)
		}
	}
	if counts, err := h.st.CountEventsByKind(ctx, time.Now().Add(-24*time.Hour)); err == nil {
		b.WriteString(bot.FormatEventCounts(counts))
	}
	return b.String()
}
