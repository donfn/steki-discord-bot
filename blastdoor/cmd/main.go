// Command blastdoor converts untrusted images to JPEG in its own container, so
// that an exploit in a decoder has no secrets, volume or bot process to reach.
// It reads no configuration besides PORT. Never give this service a secret.
package main

import (
	"log/slog"
	"os"

	"steki-bot/blastdoor"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	// ":port" listens on IPv6 too, which Railway's private network requires.
	log.Info("blastdoor listening", "port", port)
	if err := blastdoor.NewServer(":"+port, log).ListenAndServe(); err != nil {
		log.Error("serve", "error", err)
		os.Exit(1)
	}
}
