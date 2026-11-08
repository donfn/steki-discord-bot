// Command web runs the web service: the public pages, the MCP server and the
// course material.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"

	"steki-bot/internal/analytics"
	"steki-bot/internal/bot"
	"steki-bot/internal/config"
	"steki-bot/internal/mcp"
	"steki-bot/internal/rpc"
	"steki-bot/internal/shield"
	"steki-bot/internal/store"
	"steki-bot/internal/verify"
	"steki-bot/services/web"
)

// gatewayDelay lets the other services identify with the gateway first.
const gatewayDelay = 12 * time.Second

func main() {
	bot.TuneRuntime()
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadWeb()
	if err != nil {
		return err
	}
	logger := bot.NewLogger(cfg.Common, "web")
	logger.Info("starting")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	objects, err := bot.OpenBucket(ctx, cfg.Common)
	if err != nil {
		return err
	}
	fence := bot.NewFence()
	st, shard, err := store.OpenLibrary(ctx, bot.ShardOptions(cfg.Common, objects, fence, logger))
	if err != nil {
		return err
	}
	defer shard.Close()
	logger.Info("database ready", "bytes", st.FileSize())

	sess, err := bot.NewSession(cfg.DiscordToken, web.Intents)
	if err != nil {
		return err
	}

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

	h, err := web.NewHandler(cfg, st, an, logger)
	if err != nil {
		return err
	}
	h.Register(sess)

	api := rpc.NewServer(cfg.InternalSecret, logger)
	h.API(api, sess)
	api.Start(ctx)

	pages := verify.NewServer(h.Tokens(), h.Lifter(), h.Consenter(), h.MaterialSource(sess), logger)
	pages.SetLinkWatcher(h)
	pages.SetProfiles(h.ProfileSource(sess))
	logger.Info("pages enabled", "public_url", cfg.PublicURL)

	var mcpServer *mcp.Server
	if lib := h.MCPLibrary(sess); lib != nil {
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
	} else {
		logger.Info("mcp server is off: DISCORD_CLIENT_ID and DISCORD_CLIENT_SECRET are not set")
	}
	srv := startHTTPServer(cfg.Port, st, pages, mcpServer, newShield(cfg, st, sess, logger),
		verify.NewPeerBlock(cfg.BlastdoorURL, logger), an, logger)

	bot.Stagger(ctx, gatewayDelay)
	if err := sess.Open(); err != nil {
		return err
	}
	bot.Running(logger)
	bot.WaitForStop(logger, fence)

	// Shut down outside-in: stop taking events, then let in-flight work
	// finish or abort, then stop answering health checks. The deferred
	// shard close ships what is left.
	if err := sess.Close(); err != nil {
		logger.Warn("gateway close", "error", err)
	}
	h.Stop()

	shutdownCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("http server shutdown", "error", err)
	}
	return nil
}

// startHTTPServer exposes /healthz, so the platform can tell a wedged process
// from a healthy one, and the registration and media guard pages.
func startHTTPServer(port string, st *store.Library, pages *verify.Server, mcpSrv *mcp.Server, sh *shield.Detector, peers *verify.PeerBlock, an *analytics.Client, logger *slog.Logger) *http.Server {
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
	// The peer block goes outside the shield: what it refuses has a forged
	// address, which must not count against anyone.
	handler := peers.Middleware(sh.Middleware(verify.Harden(an.Middleware(mux), logger)))

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
	go peers.Run(shieldCtx)
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
func newShield(cfg *config.Web, st *store.Library, sess *discordgo.Session, logger *slog.Logger) *shield.Detector {
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
