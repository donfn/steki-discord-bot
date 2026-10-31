// Command blastdoor converts untrusted images to JPEG in its own container, so
// that an exploit in a decoder has no secrets, volume or bot process to reach.
// It reads no configuration besides PORT. Never give this service a secret.
package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"steki-bot/blastdoor"
)

const sandboxCheckTimeout = 30 * time.Second

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := blastdoor.HardenProcess(); err != nil {
		log.Error("harden process", "error", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), sandboxCheckTimeout)
	defer cancel()
	if err := blastdoor.CheckSandbox(ctx); err != nil {
		log.Error("sandbox check failed, refusing to start", "error", err)
		os.Exit(1)
	}
	// ":port" listens on IPv6 too, which Railway's private network requires.
	log.Info("blastdoor listening", "port", port)
	if err := blastdoor.NewServer(":"+port, log).ListenAndServe(); err != nil {
		log.Error("serve", "error", err)
		os.Exit(1)
	}
}
