package materials

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"steki-bot/blastdoor"
)

func header(data []byte) objectHeader {
	return objectHeader{size: int64(len(data)), sum: sha256.Sum256(data)}
}

func TestHeaderRoundTrip(t *testing.T) {
	h := header([]byte("hello"))
	got, ok := parseHeader(h.bytes())
	if !ok || got != h {
		t.Fatalf("%+v %v", got, ok)
	}
	for _, plain := range []string{"%PDF-1.7 and plenty more bytes after the magic, enough for a header", "\xff\xd8\xff", ""} {
		if _, ok := parseHeader([]byte(plain)); ok {
			t.Errorf("%q taken for compressed", plain)
		}
	}
}

func checked(stream, promised []byte) *checkedReader {
	h := header(promised)
	return &checkedReader{src: bytes.NewReader(stream), left: h.size, want: h.sum, h: sha256.New(), cancel: func() {}}
}

func TestCheckedReader(t *testing.T) {
	file := []byte("the real file")
	for name, c := range map[string]struct {
		stream, promised []byte
		ok               bool
	}{
		"good":      {file, file, true},
		"one byte":  {[]byte("x"), []byte("x"), true},
		"empty":     {nil, nil, true},
		"tampered":  {[]byte("the fake file"), file, false},
		"short":     {file[:5], file, false},
		"long":      {append(bytes.Clone(file), 'x'), file, false},
		"from none": {[]byte("x"), nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := io.ReadAll(iotestSmall{checked(c.stream, c.promised)})
			if c.ok {
				if err != nil || !bytes.Equal(got, c.promised) {
					t.Fatalf("%q %v", got, err)
				}
				return
			}
			if err == nil {
				t.Fatal("accepted")
			}
			// A client must never receive the complete length of a bad file.
			if len(c.promised) > 0 && len(got) >= len(c.promised) {
				t.Errorf("handed over %d of %d bytes", len(got), len(c.promised))
			}
		})
	}
}

// iotestSmall reads through a small buffer, to cross the last-byte boundary in
// several steps.
type iotestSmall struct{ r io.Reader }

func (s iotestSmall) Read(p []byte) (int, error) {
	if len(p) > 3 {
		p = p[:3]
	}
	return s.r.Read(p)
}

func TestCompressRejectsHostileReplies(t *testing.T) {
	file := []byte(strings.Repeat("course material ", 100))
	enc, _ := zstd.NewWriter(nil)
	other := enc.EncodeAll([]byte("something else entirely"), nil)
	good := enc.EncodeAll(file, nil)
	for name, c := range map[string]struct {
		reply []byte
		ok    bool
	}{
		"good":       {good, true},
		"other file": {other, false},
		"garbage":    {[]byte("not zstd at all"), false},
	} {
		t.Run(name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.Copy(io.Discard, r.Body)
				w.Write(c.reply)
			}))
			defer ts.Close()
			plain, _, sum, err := spool(bytes.NewReader(file))
			if err != nil {
				t.Fatal(err)
			}
			defer discard(plain)
			plain.Seek(0, io.SeekStart)
			out, _, err := NewConverter(ts.URL).compressOnce(context.Background(), plain, objectHeader{size: int64(len(file)), sum: sum})
			if (err == nil) != c.ok {
				t.Fatalf("err = %v", err)
			}
			if err == nil {
				discard(out)
			}
		})
	}
}

// TestBucketEndToEnd runs the real service against a real S3 server. Point
// STEKI_TEST_S3 at a MinIO (minioadmin credentials); it needs zstd installed.
func TestBucketEndToEnd(t *testing.T) {
	host := os.Getenv("STEKI_TEST_S3")
	if host == "" {
		t.Skip("STEKI_TEST_S3 not set")
	}
	if _, err := exec.LookPath("zstd"); err != nil {
		t.Skip("zstd not installed")
	}
	ctx := context.Background()
	ts := httptest.NewServer(blastdoor.Handler(slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer ts.Close()
	client, err := minio.New(host, &minio.Options{Creds: credentials.NewStaticV4("minioadmin", "minioadmin", "")})
	if err != nil {
		t.Fatal(err)
	}
	const name = "steki-test"
	if ok, _ := client.BucketExists(ctx, name); !ok {
		if err := client.MakeBucket(ctx, name, minio.MakeBucketOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	b := &Bucket{client: client, name: name, codec: NewConverter(ts.URL)}
	file := []byte(strings.Repeat("%PDF course material 0123456789\n", 20000))

	read := func(key string, want []byte) {
		t.Helper()
		r, size, err := b.Open(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		got, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(got, want) || size != int64(len(want)) {
			t.Fatalf("read %s: %d bytes, size %d, %v", key, len(got), size, err)
		}
	}
	stored := func(key string) int64 {
		t.Helper()
		i, err := client.StatObject(ctx, name, key, minio.StatObjectOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return i.Size
	}

	// A new upload is compressed under its own key.
	if err := b.Store(ctx, "μάθημα/1-σημειώσεις.pdf", bytes.NewReader(file), int64(len(file)), TypePDF); err != nil {
		t.Fatal(err)
	}
	if s := stored("μάθημα/1-σημειώσεις.pdf"); s >= int64(len(file))/10 {
		t.Errorf("stored %d of %d bytes", s, len(file))
	}
	read("μάθημα/1-σημειώσεις.pdf", file)

	// An old plain object is readable, then compressed in place, then skipped.
	if err := b.put(ctx, "old.pdf", bytes.NewReader(file), int64(len(file)), TypePDF); err != nil {
		t.Fatal(err)
	}
	read("old.pdf", file)
	before, after, err := b.compressObject(ctx, "old.pdf")
	if err != nil || before != int64(len(file)) || after != stored("old.pdf") || after >= before {
		t.Fatalf("%d %d %v", before, after, err)
	}
	read("old.pdf", file)
	if _, _, err := b.compressObject(ctx, "old.pdf"); !errors.Is(err, errObjectDone) {
		t.Errorf("second pass: %v", err)
	}
	if _, _, err := b.compressObject(ctx, "nothing.pdf"); !errors.Is(err, errObjectGone) {
		t.Errorf("missing: %v", err)
	}

	// The members' note databases go in as they are, and a missing one is no
	// error.
	var buf bytes.Buffer
	if found, err := b.Fetch(ctx, "user-notes/none.sqlite", &buf); found || err != nil {
		t.Fatalf("missing object: %v %v", found, err)
	}
	dbPath := t.TempDir() + "/n.db"
	if err := os.WriteFile(dbPath, []byte("SQLite format 3\x00 data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.PutFile(ctx, "user-notes/1.sqlite", dbPath, "application/vnd.sqlite3"); err != nil {
		t.Fatal(err)
	}
	if found, err := b.Fetch(ctx, "user-notes/1.sqlite", &buf); !found || err != nil || !strings.HasSuffix(buf.String(), " data") {
		t.Fatalf("fetch: %v %v %q", found, err, buf.String())
	}

	// Empty and tiny files, and a tiny plain one shorter than the header.
	for _, data := range [][]byte{{}, []byte("x")} {
		if err := b.Store(ctx, "small", bytes.NewReader(data), int64(len(data)), TypePDF); err != nil {
			t.Fatal(err)
		}
		read("small", data)
	}
	if err := b.put(ctx, "tiny", bytes.NewReader([]byte("abc")), 3, TypePDF); err != nil {
		t.Fatal(err)
	}
	read("tiny", []byte("abc"))
}
