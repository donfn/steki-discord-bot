package blastdoor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	_ "image/png" // registers the PNG decoder
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	_ "golang.org/x/image/webp" // registers the WebP decoder
)

const (
	// jpegQuality is the highest the encoder offers. JPEG has no truly lossless
	// mode; at 100 the loss is not visible.
	jpegQuality = 100

	// maxPixels bounds the decoded size of a PNG or WebP: 4 bytes per pixel
	// are held in memory, and the bot runs under a small limit.
	maxPixels = 16_000_000

	// heicTimeout bounds one HEIC conversion.
	heicTimeout = time.Minute
)

// ErrUnsupportedImage is returned for content that is not a PNG, WebP or HEIC
// image the converter can read.
var ErrUnsupportedImage = errors.New("blastdoor: unsupported image")

// heifTools are the names libheif's converter goes by: heif-dec is the newer
// one and heif-convert is kept as an alias.
var heifTools = []string{"heif-convert", "heif-dec"}

// heicBrands are the "ftyp" brands of HEIC and HEIF images.
var heicBrands = [][]byte{
	[]byte("heic"), []byte("heix"), []byte("heim"), []byte("heis"),
	[]byte("hevc"), []byte("hevx"), []byte("hevm"), []byte("hevs"),
	[]byte("mif1"), []byte("msf1"),
}

type imageKind int

const (
	kindUnknown imageKind = iota
	kindPNG
	kindWebP
	kindHEIC
)

// sniffImage tells the format from the content, not the name, so a renamed
// file cannot take the wrong path.
func sniffImage(b []byte) imageKind {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return kindPNG
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return kindWebP
	case len(b) >= 12 && bytes.Equal(b[4:8], []byte("ftyp")) &&
		slices.ContainsFunc(heicBrands, func(br []byte) bool { return bytes.Equal(b[8:12], br) }):
		return kindHEIC
	}
	return kindUnknown
}

// ToJPEG converts a PNG, WebP or HEIC image to a JPEG at the highest quality.
// Transparency is flattened onto white. HEIC goes through libheif's command
// line tool, which applies the photo's rotation, so its memory is the
// subprocess's and is returned when it exits.
func ToJPEG(ctx context.Context, data []byte) ([]byte, error) {
	switch sniffImage(data) {
	case kindPNG, kindWebP:
		return rasterToJPEG(data)
	case kindHEIC:
		return heicToJPEG(ctx, data)
	}
	return nil, ErrUnsupportedImage
}

func rasterToJPEG(data []byte) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupportedImage, err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxPixels {
		return nil, fmt.Errorf("%w: %dx%d is too large", ErrUnsupportedImage, cfg.Width, cfg.Height)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnsupportedImage, err)
	}
	if o, ok := img.(interface{ Opaque() bool }); !ok || !o.Opaque() {
		flat := image.NewRGBA(img.Bounds())
		draw.Draw(flat, flat.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
		draw.Draw(flat, flat.Bounds(), img, img.Bounds().Min, draw.Over)
		img = flat
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// heifMemoryKiB caps the converter's address space. A HEIC file declares its
// own dimensions, so a small upload can ask libheif for gigabytes.
const heifMemoryKiB = 768 << 10

// heifCommand builds the converter's command line. The file it reads comes
// from a stranger, so the process gets what it needs and nothing else: no
// environment, a memory ceiling, and no network (see sandbox.go).
func heifCommand(ctx context.Context, tool, in, out string) *exec.Cmd {
	cmd := sandboxCommand(ctx, heifMemoryKiB, []string{"PATH=" + os.Getenv("PATH"), "TMPDIR=" + filepath.Dir(in)},
		tool, "-q", fmt.Sprint(jpegQuality), in, out)
	cmd.Dir = filepath.Dir(in)
	return cmd
}

func heicToJPEG(ctx context.Context, data []byte) ([]byte, error) {
	var tool string
	for _, name := range heifTools {
		if p, err := exec.LookPath(name); err == nil {
			tool = p
			break
		}
	}
	if tool == "" {
		return nil, errors.New("blastdoor: no libheif converter installed")
	}
	dir, err := os.MkdirTemp("", "heic-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	in, out := filepath.Join(dir, "in.heic"), filepath.Join(dir, "out.jpg")
	if err := os.WriteFile(in, data, 0o600); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, heicTimeout)
	defer cancel()
	if msg, err := heifCommand(ctx, tool, in, out).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrUnsupportedImage, bytes.TrimSpace(msg), err)
	}
	// A file holding several images is written as out-1.jpg, out-2.jpg, ...;
	// the first is the one to show.
	got, err := os.ReadFile(out)
	if errors.Is(err, os.ErrNotExist) {
		names, _ := filepath.Glob(filepath.Join(dir, "out-*.jpg"))
		slices.Sort(names)
		if len(names) > 0 {
			got, err = os.ReadFile(names[0])
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%w: converter wrote nothing", ErrUnsupportedImage)
	}
	return got, nil
}
