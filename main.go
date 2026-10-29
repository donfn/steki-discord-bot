// Command steki-bot is the Steki Discord registration and moderation bot.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/config"
	"steki-bot/events"
	"steki-bot/shield"
	"steki-bot/store"
	"steki-bot/verify"
)

// messageCacheSize is how many recent messages per channel discordgo keeps,
// which bounds how far back an edit or deletion can still be reported.
const messageCacheSize = 200

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
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

	// Give the database a generous window: on Railway the bot and Postgres
	// may start together, and Open retries until it answers.
	dbCtx, dbCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	st, err := store.Open(dbCtx, cfg.DatabaseURL, logger)
	dbCancel()
	if err != nil {
		return err
	}
	defer st.Close()
	logger.Info("database ready")

	sess, err := discordgo.New("Bot " + cfg.DiscordToken)
	if err != nil {
		return err
	}
	sess.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildMessages |
		discordgo.IntentsMessageContent |
		discordgo.IntentsDirectMessages |
		discordgo.IntentsGuildEmojis |
		discordgo.IntentsGuildMembers |
		discordgo.IntentsGuildMessageReactions
	// The edit and delete logs need the previous message content, which
	// discordgo only has if it caches messages; it caches none by default.
	sess.State.MaxMessageCount = messageCacheSize

	h := events.NewHandler(cfg, st, logger)
	h.Register(sess)

	if err := sess.Open(); err != nil {
		return err
	}

	pages := verify.NewServer(verify.NewTokens(cfg.JWTSecret), h.GuardLifter(sess), h.LinkConsenter(sess), logger)
	if len(cfg.JWTSecret) < 32 {
		logger.Warn("JWT_SECRET is shorter than 32 characters; use a long random value")
	}
	logger.Info("verification pages enabled", "public_url", cfg.PublicURL)
	srv := startHTTPServer(cfg.Port, st, pages, newShield(cfg, sess, logger), logger)

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
func startHTTPServer(port string, st *store.Store, pages *verify.Server, sh *shield.Detector, logger *slog.Logger) *http.Server {
	mux := http.NewServeMux()
	pages.Register(mux)
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

	var handler http.Handler = verify.Harden(mux, logger)
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
		msg := "🛡️ Under Attack Mode on για 1 ώρα. Λόγος: " + reason
		if err != nil {
			msg = "⚠️ Δεν μπόρεσα να ενεργοποιήσω το Under Attack Mode (" + reason + "). Ξαναπροσπαθώ σε λίγα λεπτά."
		}
		if _, sendErr := sess.ChannelMessageSend(config.LogChannelID, msg); sendErr != nil {
			logger.Warn("shield: log channel message", "error", sendErr)
		}
	}
	logger.Info("automatic Under Attack Mode enabled")
	return shield.New(shield.RailwayEnabler("railway", cfg.RailwayServiceID), notify, logger)
}
