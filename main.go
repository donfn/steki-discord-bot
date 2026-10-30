// Command steki-bot is the Steki Discord registration and moderation bot.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/analytics"
	"steki-bot/config"
	"steki-bot/events"
	"steki-bot/mcp"
	"steki-bot/shield"
	"steki-bot/store"
	"steki-bot/verify"
)

const (
	// memoryLimit is the soft ceiling the Go runtime holds the heap under. The
	// bot's live data is a few megabytes; without a limit the collector lets
	// the heap grow to twice that between cycles and the scavenger returns
	// the slack slowly. GOMEMLIMIT in the environment overrides it.
	memoryLimit = 24 << 20
	// gcPercent collects sooner than the default 100, trading a little CPU
	// (the bot idles near zero) for a smaller resident set.
	gcPercent = 40
)

func main() {
	tuneRuntime()
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

// tuneRuntime sets the garbage collector's targets, unless the operator has
// set GOGC or GOMEMLIMIT, which then win.
func tuneRuntime() {
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(gcPercent)
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(memoryLimit)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	slog.SetDefault(logger)
	logger.Info("starting steki-bot")

	dbCtx, dbCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	st, err := store.Open(dbCtx, cfg.DatabasePath, logger)
	if err != nil {
		dbCancel()
		return err
	}
	defer st.Close()
	// The data of the Postgres-era bot is copied across exactly once. If the
	// copy fails the bot does not start: an empty database would let members
	// reuse addresses that were already taken.
	if cfg.ImportDatabaseURL != "" {
		res, err := st.ImportPostgres(dbCtx, cfg.ImportDatabaseURL)
		if err != nil {
			dbCancel()
			return fmt.Errorf("import from postgres: %w", err)
		}
		if res.Skipped != "" {
			logger.Info("postgres import skipped", "reason", res.Skipped)
		} else {
			logger.Warn("imported postgres data; DATABASE_URL and the Postgres service can now be removed",
				"rows", res.Rows)
		}
	}
	dbCancel()
	logger.Info("database ready", "path", cfg.DatabasePath, "bytes", st.FileSize())

	sess, err := discordgo.New("Bot " + cfg.DiscordToken)
	if err != nil {
		return err
	}
	sess.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsMessageContent |
		discordgo.IntentsDirectMessages |
		discordgo.IntentsGuildMembers |
		discordgo.IntentsGuildMessageReactions
	// The bot reads channels, roles and members from the state cache and
	// nothing else. Presences and voice states are not even subscribed to,
	// and emojis, stickers and threads are never looked at, so tracking them
	// would only fill the heap. Message text for the edit and delete logs is
	// kept by events.msgCache, a fixed ring, instead of discordgo's
	// per-channel cache (MaxMessageCount stays 0).
	sess.State.TrackPresences = false
	sess.State.TrackVoice = false
	sess.State.TrackEmojis = false
	sess.State.TrackStickers = false
	sess.State.TrackThreads = false
	sess.State.TrackThreadMembers = false

	// A nil client is valid and drops every event, so analytics being off or
	// misconfigured never affects the bot.
	an, err := analytics.New(cfg.PostHogKey, cfg.PostHogHost, logger)
	switch {
	case err != nil:
		logger.Warn("analytics is off: PostHog client failed", "error", err)
	case an == nil:
		logger.Info("POSTHOG_API_KEY not set; analytics is off")
	default:
		logger.Info("posthog analytics enabled", "host", cfg.PostHogHost)
	}
	defer an.Close()

	h := events.NewHandler(cfg, st, logger)
	h.SetAnalytics(an)
	h.Register(sess)

	if err := sess.Open(); err != nil {
		return err
	}

	pages := verify.NewServer(verify.NewTokens(cfg.JWTSecret), h.GuardLifter(sess), h.LinkConsenter(sess),
		h.MaterialSource(sess), logger)
	pages.SetLinkWatcher(h)
	pages.SetProfiles(h.ProfileSource(sess))
	logger.Info("verification pages enabled", "public_url", cfg.PublicURL)

	var mcpServer *mcp.Server
	switch lib := h.MCPLibrary(sess); {
	case lib != nil:
		mcpServer = mcp.New(mcp.Options{
			PublicURL:           cfg.PublicURL,
			Secret:              cfg.JWTSecret,
			DiscordClientID:     cfg.DiscordClientID,
			DiscordClientSecret: cfg.DiscordClientSecret,
			Library:             lib,
			Logger:              logger,
		})
		pages.EnableMCPGuide(cfg.PublicURL + mcp.PathMCP)
		logger.Info("mcp server enabled", "endpoint", cfg.PublicURL+mcp.PathMCP,
			"discord_redirect_uri", cfg.PublicURL+"/oauth/callback")
	case cfg.MCPEnabled():
		logger.Warn("mcp server is off: it needs the material bucket (BUCKET_* variables)")
	default:
		logger.Info("mcp server is off: DISCORD_CLIENT_ID and DISCORD_CLIENT_SECRET are not set")
	}
	srv := startHTTPServer(cfg.Port, st, pages, mcpServer, newShield(cfg, st, sess, logger), an, logger)

	logger.Info("bot is running")
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	// Shut down outside-in: stop taking events, then let in-flight work
	// finish or abort, then stop answering health checks. The deferred
	// st.Close runs last.
	logger.Info("shutting down")
	if err := sess.Close(); err != nil {
		logger.Warn("gateway close", "error", err)
	}
	h.Stop()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http server shutdown", "error", err)
	}
	return nil
}

// startHTTPServer exposes /healthz, so the platform can tell a wedged process
// from a healthy one, and the registration and media guard pages.
func startHTTPServer(port string, st *store.Store, pages *verify.Server, mcpSrv *mcp.Server, sh *shield.Detector, an *analytics.Client, logger *slog.Logger) *http.Server {
	mux := http.NewServeMux()
	pages.Register(mux)
	if mcpSrv != nil {
		mcpSrv.Register(mux)
	}
	// Health is probed in the background and cached, so a flood of public
	// requests never reaches the database pool.
	var dbHealthy atomic.Bool
	dbHealthy.Store(true)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if !dbHealthy.Load() {
			http.Error(w, "database unreachable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// Inside Harden, so requests it turns away (rate limit, busy) are not
	// reported: a flood must not become a flood of events.
	// The shield goes outside Harden, so requests Harden turns away still
	// count towards a block.
	handler := sh.Middleware(verify.Harden(an.Middleware(mux), logger))

	// The verification page is public, so every phase of a request is
	// bounded: a client that trickles bytes cannot hold connections open.
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	// The probe runs until the server is shut down.
	stopProbe := make(chan struct{})
	srv.RegisterOnShutdown(func() { close(stopProbe) })
	// The shield writes its blocks to the edge until the server is shut down.
	shieldCtx, stopShield := context.WithCancel(context.Background())
	srv.RegisterOnShutdown(stopShield)
	go sh.Run(shieldCtx)
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			dbHealthy.Store(st.Ping(ctx) == nil)
			cancel()
			select {
			case <-stopProbe:
				return
			case <-tick.C:
			}
		}
	}()
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
		}
	}()
	return srv
}

// newShield returns the HTTP shield. It always counts requests and refuses
// banned addresses itself; with a Railway token it also writes its blocks to
// the service's edge rules and can switch on Under Attack Mode.
func newShield(cfg *config.Config, st *store.Store, sess *discordgo.Session, logger *slog.Logger) *shield.Detector {
	o := shield.Options{ClientIP: verify.ClientIP, Store: st, Logger: logger}
	o.Notify = func(e shield.Event) {
		var msg string
		urgent := false
		switch e.Kind {
		case shield.EventBan:
			msg = fmt.Sprintf("⛔ Η διεύθυνση `%s` μπλοκαρίστηκε μέχρι <t:%d:t>. Λόγος: %s", e.IP, e.Until.Unix(), e.Reason)
		case shield.EventEdgeFailed:
			msg = fmt.Sprintf("⚠️ Δεν μπόρεσα να γράψω τους κανόνες IP στο Railway (edge rules), οπότε οι έλεγχοι και τα μπλοκ ισχύουν μόνο μέσα στο bot. Ξαναπροσπαθώ κάθε λεπτό: ```%v```", e.Err)
			urgent = true
		case shield.EventUnderAttack:
			msg = "🛡️ Ενεργοποιήθηκε το Under Attack Mode για 1 ώρα. Λόγος: " + e.Reason
			if e.Err != nil {
				msg = "⚠️ Δεν μπόρεσα να ενεργοποιήσω το Under Attack Mode (" + e.Reason + "). Ξαναπροσπαθώ σε λίγα λεπτά."
				urgent = true
			}
		default:
			return
		}
		// Only a failure needs an admin; the rest is posted silently.
		send := &discordgo.MessageSend{Content: msg, AllowedMentions: &discordgo.MessageAllowedMentions{}}
		if !urgent {
			send.Flags = discordgo.MessageFlagsSuppressNotifications
		}
		if _, err := sess.ChannelMessageSendComplex(config.LogChannelID, send); err != nil {
			logger.Warn("shield: log channel message", "error", err)
		}
	}

	switch {
	case cfg.RailwayToken == "":
		logger.Info("RAILWAY_TOKEN not set; IP blocks are enforced by the bot only, and automatic Under Attack Mode is off")
	case cfg.RailwayServiceID == "" || cfg.RailwayEnvironmentID == "":
		logger.Warn("RAILWAY_SERVICE_ID or RAILWAY_ENVIRONMENT_ID not set; IP blocks are enforced by the bot only")
		o.Enable = shield.RailwayEnabler("railway", cfg.RailwayServiceID)
	default:
		o.Edge = shield.NewRailwayEdge(cfg.RailwayToken, cfg.RailwayServiceID, cfg.RailwayEnvironmentID)
		o.Enable = shield.RailwayEnabler("railway", cfg.RailwayServiceID)
		logger.Info("IP blocks are written to the service's edge rules; automatic Under Attack Mode enabled")
	}

	sh := shield.New(o)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sh.Load(ctx); err != nil {
		logger.Error("shield: restore blocks", "error", err)
	}
	return sh
}
