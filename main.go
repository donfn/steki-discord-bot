// Command steki-bot is the Steki Discord registration and moderation bot.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/config"
	"steki-bot/events"
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

	var guard *verify.Server
	if cfg.MediaGuardEnabled() {
		guard = verify.NewServer(verify.NewTokens(cfg.JWTSecret), h.GuardLifter(sess), logger)
		logger.Info("media guard enabled", "public_url", cfg.PublicURL)
	} else {
		logger.Warn("media guard disabled: set JWT_SECRET and a public domain (or PUBLIC_URL) to enable it")
	}
	srv := startHTTPServer(cfg.Port, st, guard, logger)

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
// from a healthy one, and the media guard's verification page when enabled.
func startHTTPServer(port string, st *store.Store, guard *verify.Server, logger *slog.Logger) *http.Server {
	mux := http.NewServeMux()
	if guard != nil {
		guard.Register(mux)
	}
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := st.Ping(ctx); err != nil {
			http.Error(w, "database unreachable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// The verification page is public, so every phase of a request is
	// bounded: a client that trickles bytes cannot hold connections open.
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
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
