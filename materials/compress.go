package materials

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/minio/minio-go/v7"
)

// An object in the bucket is a header followed by a zstd stream. The header
// lets a read tell compressed objects from older plain ones (only PDFs and
// JPEGs were ever stored, so the magic cannot occur at the start of one), and
// gives the size and checksum the blastdoor service's output is held to.
const (
	objectMagic   = "STKZ\x01"
	headerSize    = len(objectMagic) + 8 + sha256.Size
	maxZstdWindow = 1 << 28

	// wakeAttempts covers the service waking from sleep or restarting after a
	// crash. Its first request after sleeping may get a 502.
	wakeAttempts = 8
	wakeBackoff  = 2 * time.Second
)

type objectHeader struct {
	size int64
	sum  [sha256.Size]byte
}

func (h objectHeader) bytes() []byte {
	b := append(make([]byte, 0, headerSize), objectMagic...)
	b = binary.BigEndian.AppendUint64(b, uint64(h.size))
	return append(b, h.sum[:]...)
}

func parseHeader(b []byte) (objectHeader, bool) {
	var h objectHeader
	if len(b) < headerSize || !bytes.HasPrefix(b, []byte(objectMagic)) {
		return h, false
	}
	h.size = int64(binary.BigEndian.Uint64(b[len(objectMagic):]))
	copy(h.sum[:], b[len(objectMagic)+8:])
	return h, h.size >= 0
}

// ready waits for the service to answer, so that a stream which cannot be
// replayed is only sent once it is awake.
func (c *Converter) ready(ctx context.Context) error {
	if c == nil || c.url == "/convert" {
		return errors.New("materials: BLASTDOOR_URL is not set")
	}
	var last error
	for attempt := range wakeAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wakeBackoff):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base("/healthz"), nil)
		if err != nil {
			return err
		}
		resp, err := c.stream.Do(req)
		if err != nil {
			last = err
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusNoContent {
			return nil
		}
		last = fmt.Errorf("materials: blastdoor answered %d", resp.StatusCode)
	}
	return fmt.Errorf("materials: blastdoor is not answering: %w", last)
}

func (c *Converter) base(path string) string { return strings.TrimSuffix(c.url, "/convert") + path }

// post starts a stream through the service. The caller closes the reply.
func (c *Converter) post(ctx context.Context, path string, body io.Reader) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base(path), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.stream.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("materials: blastdoor answered %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// spool holds a file on disk while it is worked on. The bot's memory limit is
// far smaller than the largest upload.
func spool(r io.Reader) (*os.File, int64, [sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	f, err := os.CreateTemp("", "steki-object-")
	if err != nil {
		return nil, 0, sum, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), r)
	if err != nil {
		discard(f)
		return nil, 0, sum, err
	}
	h.Sum(sum[:0])
	return f, n, sum, nil
}

func discard(f *os.File) {
	f.Close()
	os.Remove(f.Name())
}

// compress returns the compressed form of the size bytes in plain. The result
// is checked here, not by the service: it is decompressed in this process by a
// memory-safe decoder and must match the original exactly, because the caller
// may be about to replace the only copy of the file with it.
func (c *Converter) compress(ctx context.Context, plain *os.File, hdr objectHeader) (*os.File, int64, error) {
	var last error
	for attempt := range convertAttempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, 0, ctx.Err()
			case <-time.After(convertBackoff):
			}
		}
		if err := c.ready(ctx); err != nil {
			return nil, 0, err
		}
		if _, err := plain.Seek(0, io.SeekStart); err != nil {
			return nil, 0, err
		}
		out, n, err := c.compressOnce(ctx, plain, hdr)
		if err == nil {
			return out, n, nil
		}
		last = err
	}
	return nil, 0, last
}

func (c *Converter) compressOnce(ctx context.Context, plain *os.File, hdr objectHeader) (*os.File, int64, error) {
	body, err := c.post(ctx, fmt.Sprintf("/compress?size=%d", hdr.size), io.NopCloser(plain))
	if err != nil {
		return nil, 0, err
	}
	defer body.Close()
	out, err := os.CreateTemp("", "steki-zstd-")
	if err != nil {
		return nil, 0, err
	}
	// A reply much larger than the file is not a compression of it.
	n, err := io.Copy(out, io.LimitReader(body, hdr.size+hdr.size/8+1<<16))
	if err == nil {
		err = verifyZstd(out, hdr)
	}
	if err != nil {
		discard(out)
		return nil, 0, err
	}
	return out, n, nil
}

func verifyZstd(f *os.File, hdr objectHeader) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	dec, err := zstd.NewReader(f, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxWindow(maxZstdWindow))
	if err != nil {
		return err
	}
	defer dec.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(dec, hdr.size+1))
	if err != nil {
		return fmt.Errorf("materials: compressed reply does not decode: %w", err)
	}
	if n != hdr.size || !bytes.Equal(h.Sum(nil), hdr.sum[:]) {
		return errors.New("materials: compressed reply does not match the file")
	}
	_, err = f.Seek(0, io.SeekStart)
	return err
}

// Store compresses size bytes from r into the object at key.
func (b *Bucket) Store(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	plain, n, sum, err := spool(r)
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	defer discard(plain)
	if size >= 0 && n != size {
		return fmt.Errorf("read: got %d of %d bytes", n, size)
	}
	hdr := objectHeader{size: n, sum: sum}
	out, zn, err := b.codec.compress(ctx, plain, hdr)
	if err != nil {
		return fmt.Errorf("compress: %w", err)
	}
	defer discard(out)
	b.writes.Lock()
	defer b.writes.Unlock()
	return b.put(ctx, key, io.MultiReader(bytes.NewReader(hdr.bytes()), out), int64(headerSize)+zn, contentType)
}

// Open returns the file at key, decompressed as it is read, and its original
// size. The caller closes it. Plain objects from before compression are read
// as they are.
func (b *Bucket) Open(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	obj, err := b.client.GetObject(ctx, b.name, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, err
	}
	// GetObject is lazy; Stat makes the request, so a missing object fails
	// here rather than on the first read.
	info, err := obj.Stat()
	if err != nil {
		_ = obj.Close()
		return nil, 0, err
	}
	head := make([]byte, headerSize)
	n, err := io.ReadFull(obj, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		_ = obj.Close()
		return nil, 0, err
	}
	head = head[:n]
	hdr, packed := parseHeader(head)
	if !packed {
		return &plainObject{Reader: io.MultiReader(bytes.NewReader(head), obj), obj: obj}, info.Size, nil
	}
	r, err := b.codec.decompress(ctx, obj, hdr)
	if err != nil {
		_ = obj.Close()
		return nil, 0, err
	}
	return r, hdr.size, nil
}

type plainObject struct {
	io.Reader
	obj io.Closer
}

func (p *plainObject) Close() error { return p.obj.Close() }

// decompress streams src through the service. Closing the result ends the
// stream and closes src.
func (c *Converter) decompress(ctx context.Context, src io.ReadCloser, hdr objectHeader) (io.ReadCloser, error) {
	if err := c.ready(ctx); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	body, err := c.post(ctx, "/decompress", src)
	if err != nil {
		cancel()
		return nil, err
	}
	return &checkedReader{src: body, left: hdr.size, want: hdr.sum, h: sha256.New(), closers: []io.Closer{body, src}, cancel: cancel}, nil
}

// checkedReader holds the service's output to the header's size and checksum.
// The last byte is only handed over once the checksum matched, so a tampered
// file ends as an error, one byte short, and a client sees an interrupted
// download rather than a complete file.
type checkedReader struct {
	src     io.Reader
	left    int64
	want    [sha256.Size]byte
	h       hash.Hash
	checked bool
	closers []io.Closer
	cancel  context.CancelFunc
}

var errBadStream = errors.New("materials: decompressed file does not match its checksum")

func (r *checkedReader) Read(p []byte) (int, error) {
	if r.left == 0 && r.checked {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.left > 1 {
		if int64(len(p)) > r.left-1 {
			p = p[:r.left-1]
		}
		n, err := r.src.Read(p)
		r.h.Write(p[:n])
		r.left -= int64(n)
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return n, err
	}
	// Whatever follows the last byte means the stream is longer than promised.
	var last [2]byte
	n, err := io.ReadFull(r.src, last[:])
	if !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		if err == nil {
			err = errBadStream
		}
		return 0, err
	}
	if int64(n) != r.left {
		return 0, errBadStream
	}
	r.h.Write(last[:n])
	if !bytes.Equal(r.h.Sum(nil), r.want[:]) {
		return 0, errBadStream
	}
	r.checked = true
	r.left = 0
	if n == 0 {
		return 0, io.EOF
	}
	p[0] = last[0]
	return 1, nil
}

func (r *checkedReader) Close() error {
	r.cancel()
	for _, c := range r.closers {
		_ = c.Close()
	}
	return nil
}

// compressObject compresses one stored object in place and returns its sizes.
// It reports errObjectDone for one already compressed and errObjectGone for
// one no longer there.
func (b *Bucket) compressObject(ctx context.Context, key string) (before, after int64, err error) {
	obj, err := b.client.GetObject(ctx, b.name, key, minio.GetObjectOptions{})
	if err != nil {
		return 0, 0, err
	}
	defer obj.Close()
	info, err := obj.Stat()
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return 0, 0, errObjectGone
		}
		return 0, 0, err
	}
	head := make([]byte, headerSize)
	n, err := io.ReadFull(obj, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return 0, 0, err
	}
	head = head[:n]
	if _, packed := parseHeader(head); packed {
		return 0, 0, errObjectDone
	}
	plain, size, sum, err := spool(io.MultiReader(bytes.NewReader(head), obj))
	if err != nil {
		return 0, 0, err
	}
	defer discard(plain)
	if size != info.Size {
		return 0, 0, fmt.Errorf("read %d of %d bytes", size, info.Size)
	}
	hdr := objectHeader{size: size, sum: sum}
	out, zn, err := b.codec.compress(ctx, plain, hdr)
	if err != nil {
		return 0, 0, err
	}
	defer discard(out)

	b.writes.Lock()
	defer b.writes.Unlock()
	// The object may have been deleted or replaced during the compression.
	now, err := b.client.StatObject(ctx, b.name, key, minio.StatObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return 0, 0, errObjectGone
		}
		return 0, 0, err
	}
	if now.ETag != info.ETag {
		return 0, 0, errors.New("object changed during compression")
	}
	total := int64(headerSize) + zn
	if err := b.put(ctx, key, io.MultiReader(bytes.NewReader(hdr.bytes()), out), total, info.ContentType); err != nil {
		return 0, 0, err
	}
	return size, total, nil
}

var (
	errObjectDone = errors.New("already compressed")
	errObjectGone = errors.New("not in the bucket")
)

// CompressResult is what one CompressAll pass did.
type CompressResult struct {
	Compressed, Already, Missing, Failed int
	// Before and After are the bytes of the files compressed in this pass.
	Before, After int64
}

// CompressAll compresses every stored object not compressed yet, one at a
// time. It is safe to rerun: progress is in the objects themselves.
func (w *Worker) CompressAll(ctx context.Context) CompressResult {
	var res CompressResult
	after := ""
	for ctx.Err() == nil {
		keys, next, err := w.st.BackedUpKeys(ctx, after, batch)
		if err != nil {
			w.log.Error("materials: list stored files", "error", err)
			res.Failed++
			return res
		}
		if len(keys) == 0 {
			return res
		}
		after = next
		for _, key := range keys {
			fileCtx, cancel := context.WithTimeout(ctx, copyTimeout)
			before, packed, err := w.bucket.compressObject(fileCtx, key)
			cancel()
			switch {
			case err == nil:
				res.Compressed++
				res.Before += before
				res.After += packed
				w.log.Info("materials: compressed", "key", key, "before", before, "after", packed)
			case errors.Is(err, errObjectDone):
				res.Already++
			case errors.Is(err, errObjectGone):
				res.Missing++
			case ctx.Err() != nil:
				return res
			default:
				res.Failed++
				w.log.Warn("materials: compress stored file", "key", key, "error", err)
			}
		}
	}
	return res
}
