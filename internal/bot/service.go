package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"steki-bot/internal/config"
	"steki-bot/internal/s3db"
	"steki-bot/internal/store"
)

const (
	// memoryLimit is the soft ceiling the Go runtime holds the heap under. A
	// service's live data is a few megabytes; without a limit the collector lets
	// the heap grow to twice that between cycles and the scavenger returns the
	// slack slowly. GOMEMLIMIT in the environment overrides it.
	memoryLimit = 24 << 20
	// maxProcs caps the scheduler: the services idle and wait on the network,
	// and every P keeps a cache of spans for each size class, which a container
	// reporting the host's cores would multiply. GOMAXPROCS overrides it.
	maxProcs = 2
	// gcPercent collects sooner than the default 100, trading a little CPU
	// (the bot idles near zero) for a smaller resident set.
	gcPercent = 40
)

// TuneRuntime sets the garbage collector's targets and the scheduler's width,
// unless the operator has set GOGC, GOMEMLIMIT or GOMAXPROCS, which then win.
func TuneRuntime() {
	if os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(min(maxProcs, runtime.NumCPU()))
	}
	if os.Getenv("GOGC") == "" {
		debug.SetGCPercent(gcPercent)
	}
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(memoryLimit)
	}
}

// Running logs that the service is up and hands the memory booting used back to
// the OS: it allocates a good deal that is dead for good, and the scavenger would
// otherwise return it slowly.
func Running(logger *slog.Logger) {
	logger.Info("running")
	debug.FreeOSMemory()
}

// NewLogger returns the service's JSON logger and makes it the default.
func NewLogger(c config.Common, service string) *slog.Logger {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: c.LogLevel})).With("service", service)
	slog.SetDefault(logger)
	return logger
}

// Fence is closed when a newer deployment takes one of the service's shards.
type Fence struct {
	once sync.Once
	C    chan struct{}
}

// NewFence returns an open Fence.
func NewFence() *Fence { return &Fence{C: make(chan struct{})} }

// Trip closes the Fence.
func (f *Fence) Trip() { f.once.Do(func() { close(f.C) }) }

// OpenBucket connects to the databases' bucket and proves it refuses stale
// conditional writes, which everything about the databases' safety rests on.
func OpenBucket(ctx context.Context, c config.Common) (s3db.Objects, error) {
	b := c.Bucket
	objects, err := s3db.NewBucket(b.Endpoint, b.Region, b.AccessKeyID, b.SecretAccessKey, b.Name, b.PathStyle)
	if err != nil {
		return nil, err
	}
	verifyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s3db.Verify(verifyCtx, objects); err != nil {
		return nil, fmt.Errorf("the bucket cannot hold the databases: %w", err)
	}
	return objects, nil
}

// ShardOptions fills in what every shard shares.
func ShardOptions(c config.Common, o s3db.Objects, fence *Fence, logger *slog.Logger) store.ShardOptions {
	return store.ShardOptions{
		Objects: o, Dir: c.DataDir, Owner: c.Deployment, CreateEmpty: c.CreateEmptyDB,
		OnFenced: fence.Trip, Log: logger,
	}
}

// Stagger keeps the services from identifying with the gateway at once: Discord
// allows a bot one every five seconds, and a push redeploys all three together.
func Stagger(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// WaitForStop blocks until the platform asks the service to stop or a newer
// deployment fenced it.
func WaitForStop(logger *slog.Logger, fence *Fence) {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-stop:
		logger.Info("shutting down")
	case <-fence.C:
		logger.Warn("a newer deployment took over; shutting down")
	}
}

// HealthServer serves /healthz on the platform's port, for the services with no
// public pages.
func HealthServer(port string, ping func(context.Context) error, logger *slog.Logger) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if err := ping(ctx); err != nil {
			http.Error(w, "database unreachable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("health server failed", "error", err)
		}
	}()
	return srv
}
