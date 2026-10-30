package blastdoor

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestToJPEGPNG(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 8, 6))
	for x := 0; x < 8; x++ {
		for y := 0; y < 6; y++ {
			img.Set(x, y, color.NRGBA{R: 200, A: 0}) // fully transparent
		}
	}
	var in bytes.Buffer
	if err := png.Encode(&in, img); err != nil {
		t.Fatal(err)
	}
	out, err := ToJPEG(context.Background(), in.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	got, err := jpeg.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := got.At(4, 3).RGBA()
	if r>>8 < 250 || g>>8 < 250 || b>>8 < 250 {
		t.Errorf("transparency not flattened onto white: %d %d %d", r>>8, g>>8, b>>8)
	}
}

func TestToJPEGWebP(t *testing.T) {
	// A 1x1 lossless WebP.
	data, _ := base64.StdEncoding.DecodeString("UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA==")
	out, err := ToJPEG(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := jpeg.Decode(bytes.NewReader(out)); err != nil {
		t.Fatal(err)
	}
}

func TestToJPEGRejects(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty": nil, "text": []byte("hello world, not an image"), "gif": []byte("GIF89a......"),
	} {
		if _, err := ToJPEG(context.Background(), data); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestToJPEGHEIC(t *testing.T) {
	enc, err := exec.LookPath("heif-enc")
	if err != nil {
		t.Skip("libheif tools not installed")
	}
	dir := t.TempDir()
	var src bytes.Buffer
	if err := jpeg.Encode(&src, image.NewRGBA(image.Rect(0, 0, 64, 48)), nil); err != nil {
		t.Fatal(err)
	}
	jp, hp := filepath.Join(dir, "a.jpg"), filepath.Join(dir, "a.heic")
	if err := os.WriteFile(jp, src.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if msg, err := exec.Command(enc, jp, "-o", hp).CombinedOutput(); err != nil {
		t.Skipf("cannot make a HEIC: %v %s", err, msg)
	}
	data, _ := os.ReadFile(hp)
	out, err := ToJPEG(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(out))
	if err != nil || cfg.Width != 64 || cfg.Height != 48 {
		t.Errorf("heic result: %+v %v", cfg, err)
	}
}
