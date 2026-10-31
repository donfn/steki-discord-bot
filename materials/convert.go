package materials

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// maxConvertedPixels rejects a reply far larger than any real conversion.
	maxConvertedPixels = 200_000_000

	convertTimeout = 3 * time.Minute
	// convertAttempts covers the converter waking from sleep or restarting
	// after a crash.
	convertAttempts = 4
	convertBackoff  = 3 * time.Second
)

// ErrUnsupportedImage is returned for content the converter cannot read.
var ErrUnsupportedImage = errors.New("materials: unsupported image")

// ErrConvertedTooLarge is returned for a result over the caller's limit.
var ErrConvertedTooLarge = errors.New("materials: converted image is too large")

// Converter sends images to the blastdoor service, the only place they are
// decoded. There is deliberately no local fallback: without the service,
// conversion fails rather than parsing untrusted files inside the bot.
type Converter struct {
	url    string
	client *http.Client
	// stream has no overall timeout: those requests are bounded by their
	// context, as a large file takes minutes.
	stream *http.Client
}

// NewConverter returns a client for the blastdoor service at baseURL.
func NewConverter(baseURL string) *Converter {
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Converter{
		stream: &http.Client{CheckRedirect: noRedirect},
		url:    strings.TrimRight(baseURL, "/") + "/convert",
		client: &http.Client{
			Timeout: convertTimeout,
			// The service is untrusted: it must not steer the bot elsewhere.
			CheckRedirect: noRedirect,
		},
	}
}

// ToJPEG converts a PNG, WebP or HEIC image to a JPEG of at most maxBytes. The
// reply is treated as hostile too: it must have a valid JPEG header of a sane
// size, and no more than maxBytes of it are ever held in memory.
func (c *Converter) ToJPEG(ctx context.Context, data []byte, maxBytes int64) ([]byte, error) {
	if c == nil || c.url == "/convert" {
		return nil, errors.New("materials: BLASTDOOR_URL is not set")
	}
	var lastErr error
	for attempt := range convertAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * convertBackoff):
			}
		}
		out, err := c.convert(ctx, data, maxBytes)
		if err == nil || errors.Is(err, ErrUnsupportedImage) || errors.Is(err, ErrConvertedTooLarge) {
			return out, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func (c *Converter) convert(ctx context.Context, data []byte, maxBytes int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnprocessableEntity {
		return nil, ErrUnsupportedImage
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("materials: converter answered %d", resp.StatusCode)
	}
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(out)) > maxBytes {
		return nil, ErrConvertedTooLarge
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		return nil, fmt.Errorf("materials: converter reply is not a JPEG: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxConvertedPixels {
		return nil, fmt.Errorf("materials: converter reply is %dx%d", cfg.Width, cfg.Height)
	}
	return out, nil
}
