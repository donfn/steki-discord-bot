package materials

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"steki-bot/blastdoor"
)

func TestConverterRoundTrip(t *testing.T) {
	ts := httptest.NewServer(blastdoor.Handler(slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer ts.Close()
	var in bytes.Buffer
	if err := png.Encode(&in, image.NewRGBA(image.Rect(0, 0, 8, 6))); err != nil {
		t.Fatal(err)
	}
	c := NewConverter(ts.URL)
	out, err := c.ToJPEG(context.Background(), in.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if cfg, err := jpeg.DecodeConfig(bytes.NewReader(out)); err != nil || cfg.Width != 8 {
		t.Errorf("result: %+v %v", cfg, err)
	}
	if _, err := c.ToJPEG(context.Background(), []byte("not an image")); !errors.Is(err, ErrUnsupportedImage) {
		t.Errorf("text: %v", err)
	}
}

func TestConverterRejectsHostileReplies(t *testing.T) {
	var hits atomic.Int32
	for name, h := range map[string]http.HandlerFunc{
		"not a jpeg": func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("<html>")) },
		"redirect": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/elsewhere" {
				hits.Add(1)
			}
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		},
	} {
		t.Run(name, func(t *testing.T) {
			ts := httptest.NewServer(h)
			defer ts.Close()
			c := NewConverter(ts.URL)
			if _, err := c.convert(context.Background(), []byte("x")); err == nil {
				t.Error("accepted")
			}
		})
	}
	if hits.Load() != 0 {
		t.Error("followed a redirect")
	}
}

func TestConverterUnconfigured(t *testing.T) {
	if _, err := NewConverter("").ToJPEG(context.Background(), []byte("x")); err == nil {
		t.Error("no URL should fail, never convert locally")
	}
}
