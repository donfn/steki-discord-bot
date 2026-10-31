package blastdoor

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"
)

const (
	// MaxStream bounds one file passing through either stream operation.
	MaxStream = 2 << 30

	// streamTimeout bounds one operation. The highest zstd level manages about
	// a megabyte a second, so a large file takes minutes.
	streamTimeout = 30 * time.Minute

	// compressMemoryKiB allows level 22's tables (about 1.3 GiB on a large
	// file) and decompressMemoryKiB its 128 MiB window with room to spare. Both
	// sit under the service's memory limit, so zstd fails before the container
	// is killed.
	compressMemoryKiB   = 1800 << 10
	decompressMemoryKiB = 512 << 10
)

// zstdCommand runs zstd over stdin and stdout with no environment, a memory
// ceiling and no network (see sandbox.go).
func zstdCommand(ctx context.Context, memoryKiB int, args ...string) *exec.Cmd {
	return sandboxCommand(ctx, memoryKiB, []string{"PATH=" + os.Getenv("PATH")}, "zstd", args...)
}

// handleCompress serves POST /compress?size=N. The size tells zstd how large
// its tables need to be; without it even a small file would cost 1.3 GiB.
func handleCompress(sem chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		size, err := strconv.ParseInt(r.URL.Query().Get("size"), 10, 64)
		if err != nil || size < 0 || size > MaxStream {
			http.Error(w, "bad size", http.StatusBadRequest)
			return
		}
		stream(w, r, sem, compressMemoryKiB,
			"--ultra", "-22", "-q", "-c", "--stream-size="+strconv.FormatInt(size, 10))
	}
}

// handleDecompress serves POST /decompress. Its window limit matches level 22,
// so a stream asking for more is refused rather than allocated.
func handleDecompress(sem chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stream(w, r, sem, decompressMemoryKiB, "-d", "-q", "-c", "--memory=128MB")
	}
}

// stream pipes the request through zstd into the response as both arrive. Any
// failure aborts the connection, so a partial result can never look complete.
func stream(w http.ResponseWriter, r *http.Request, sem chan struct{}, memoryKiB int, args ...string) {
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-r.Context().Done():
		return
	}
	// By default the server stops reading the request on the first write.
	if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
		panic(http.ErrAbortHandler)
	}
	ctx, cancel := context.WithTimeout(r.Context(), streamTimeout)
	defer cancel()
	cmd := zstdCommand(ctx, memoryKiB, args...)
	cmd.Stdin = http.MaxBytesReader(w, r.Body, MaxStream)
	cmd.Stdout = w
	w.Header().Set("Content-Type", "application/octet-stream")
	if err := cmd.Run(); err != nil {
		panic(http.ErrAbortHandler)
	}
}
