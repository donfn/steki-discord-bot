package blastdoor

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const (
	// MaxInput bounds one upload. Discord's own attachment limits sit below it.
	MaxInput = 100 << 20

	// slots is how many conversions run at once; the rest wait, so a burst
	// cannot run the container out of memory.
	slots = 2

	// compressSlots is one because level 22 can take 1.3 GiB; a decompression
	// takes at most about 130 MiB.
	compressSlots   = 1
	decompressSlots = 4
)

// Handler serves POST /convert (image in, JPEG out), POST /compress and POST
// /decompress (see stream.go) and GET /healthz. A 422 from /convert means the
// image cannot be read; anything else is the service's own failure.
func Handler(log *slog.Logger) http.Handler {
	sem := make(chan struct{}, slots)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /compress", handleCompress(make(chan struct{}, compressSlots)))
	mux.HandleFunc("POST /decompress", handleDecompress(make(chan struct{}, decompressSlots)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /convert", func(w http.ResponseWriter, r *http.Request) {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-r.Context().Done():
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxInput))
		if err != nil {
			http.Error(w, "cannot read body", http.StatusRequestEntityTooLarge)
			return
		}
		out, err := ToJPEG(r.Context(), data)
		if errors.Is(err, ErrUnsupportedImage) {
			log.Warn("convert: unreadable image", "bytes", len(data), "error", err)
			http.Error(w, "unsupported image", http.StatusUnprocessableEntity)
			return
		}
		if err != nil {
			log.Error("convert", "bytes", len(data), "error", err)
			http.Error(w, "conversion failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(out)
	})
	return mux
}

// NewServer wraps Handler with the timeouts of a server fed untrusted input.
func NewServer(addr string, log *slog.Logger) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           Handler(log),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       streamTimeout,
		WriteTimeout:      streamTimeout,
	}
}
