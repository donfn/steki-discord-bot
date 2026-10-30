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
	if len(cfg.JWTSecret) < 32 {
		logger.Warn("JWT_SECRET is shorter than 32 characters; use a long random value")
	}
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
	srv := startHTTPServer(cfg.Port, st, pages, mcpServer, newShield(cfg, sess, logger), an, logger)

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
	go func() {
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			dbHealthy.Store(st.Ping(ctx) == nil)
			cancel()
			time.Sleep(5 * time.Second)
		}
	}()
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
	var handler http.Handler = verify.Harden(an.Middleware(mux), logger)
	if sh != nil {
		// Outside Harden, so requests it turns away still count as load.
		handler = sh.Middleware(handler)
	}

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
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
		}
	}()
	return srv
}

// newShield returns the flood detector, or nil when no Railway token is set:
// without one there is nothing it could switch on.
func newShield(cfg *config.Config, sess *discordgo.Session, logger *slog.Logger) *shield.Detector {
	if cfg.RailwayToken == "" {
		logger.Info("RAILWAY_TOKEN not set; automatic Under Attack Mode is off")
		return nil
	}
	if cfg.RailwayServiceID == "" {
		logger.Warn("RAILWAY_SERVICE_ID not set; the Railway CLI will have to infer the service")
	}
	notify := func(reason string, err error) {
		msg := "🛡️ Ενεργοποιήθηκε το Under Attack Mode για 1 ώρα. Λόγος: " + reason
		if err != nil {
			msg = "⚠️ Δεν μπόρεσα να ενεργοποιήσω το Under Attack Mode (" + reason + "). Ξαναπροσπαθώ σε λίγα λεπτά."
		}
		// Only a failure needs an admin; the switch itself is posted silently.
		send := &discordgo.MessageSend{Content: msg}
		if err == nil {
			send.Flags = discordgo.MessageFlagsSuppressNotifications
		}
		if _, sendErr := sess.ChannelMessageSendComplex(config.LogChannelID, send); sendErr != nil {
			logger.Warn("shield: log channel message", "error", sendErr)
		}
	}
	logger.Info("automatic Under Attack Mode enabled")
	return shield.New(shield.RailwayEnabler("railway", cfg.RailwayServiceID), notify, logger)
}
