// Command registration runs the registration service.
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"steki-bot/analytics"
	"steki-bot/bot"
	"steki-bot/config"
	"steki-bot/rpc"
	"steki-bot/s3db"
	"steki-bot/services/registration"
	"steki-bot/store"
)

func main() {
	bot.TuneRuntime()
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadRegistration()
	if err != nil {
		return err
	}
	logger := bot.NewLogger(cfg.Common, "registration")
	logger.Info("starting")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	objects, err := bot.OpenBucket(ctx, cfg.Common)
	if err != nil {
		return err
	}
	fence := bot.NewFence()
	type opened struct {
		st *store.Registry
		sh *s3db.Shard
	}
	db, err := bot.WaitForShard(ctx, logger, store.ShardRegistry, func(ctx context.Context) (opened, error) {
		st, sh, err := store.OpenRegistry(ctx, bot.ShardOptions(cfg.Common, objects, fence, logger))
		return opened{st, sh}, err
	})
	if err != nil {
		return err
	}
	defer db.sh.Close()

	an, err := analytics.New(cfg.PostHogKey, cfg.PostHogHost, logger)
	if err != nil {
		logger.Warn("analytics is off: PostHog client failed", "error", err)
	}
	defer an.Close()

	sess, err := bot.NewSession(cfg.DiscordToken, registration.Intents)
	if err != nil {
		return err
	}
	h := registration.NewHandler(cfg, db.st, an, logger)
	h.Register(sess)

	api := rpc.NewServer(cfg.InternalSecret, logger)
	h.API(api, sess)
	api.Start(ctx)
	health := bot.HealthServer(cfg.Port, db.st.Ping, logger)

	if err := sess.Open(); err != nil {
		return err
	}
	logger.Info("running")
	bot.WaitForStop(logger, fence)

	// Shut down outside-in: stop taking events, then let in-flight work finish
	// or abort. The deferred shard close ships what is left.
	if err := sess.Close(); err != nil {
		logger.Warn("gateway close", "error", err)
	}
	h.Stop()
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	return health.Shutdown(shutdown)
}
