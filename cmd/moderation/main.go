// Command moderation runs the moderation service.
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
	"steki-bot/services/moderation"
	"steki-bot/store"
)

// gatewayDelay lets the registration service identify with the gateway first.
const gatewayDelay = 6 * time.Second

func main() {
	bot.TuneRuntime()
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.LoadModeration()
	if err != nil {
		return err
	}
	logger := bot.NewLogger(cfg.Common, "moderation")
	logger.Info("starting")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	objects, err := bot.OpenBucket(ctx, cfg.Common)
	if err != nil {
		return err
	}
	fence := bot.NewFence()
	opts := bot.ShardOptions(cfg.Common, objects, fence, logger)
	type guardDB struct {
		st *store.Guard
		sh *s3db.Shard
	}
	guard, err := bot.WaitForShard(ctx, logger, store.ShardGuard, func(ctx context.Context) (guardDB, error) {
		st, sh, err := store.OpenGuard(ctx, opts)
		return guardDB{st, sh}, err
	})
	if err != nil {
		return err
	}
	defer guard.sh.Close()
	type activityDB struct {
		st *store.Activity
		sh *s3db.Shard
	}
	activity, err := bot.WaitForShard(ctx, logger, store.ShardActivity, func(ctx context.Context) (activityDB, error) {
		st, sh, err := store.OpenActivity(ctx, opts)
		return activityDB{st, sh}, err
	})
	if err != nil {
		return err
	}
	defer activity.sh.Close()

	an, err := analytics.New(cfg.PostHogKey, cfg.PostHogHost, logger)
	if err != nil {
		logger.Warn("analytics is off: PostHog client failed", "error", err)
	}
	defer an.Close()

	sess, err := bot.NewSession(cfg.DiscordToken, moderation.Intents)
	if err != nil {
		return err
	}
	h := moderation.NewHandler(cfg, guard.st, activity.st, an, logger)
	h.Register(sess)

	api := rpc.NewServer(cfg.InternalSecret, logger)
	h.API(api, sess)
	api.Start(ctx)
	health := bot.HealthServer(cfg.Port, guard.st.Ping, logger)

	bot.Stagger(ctx, gatewayDelay)
	if err := sess.Open(); err != nil {
		return err
	}
	logger.Info("running")
	bot.WaitForStop(logger, fence)

	if err := sess.Close(); err != nil {
		logger.Warn("gateway close", "error", err)
	}
	h.Stop()
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	return health.Shutdown(shutdown)
}
