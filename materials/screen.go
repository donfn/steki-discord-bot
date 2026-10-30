package materials

import (
	"bytes"
	"context"
	"io"

	"steki-bot/store"
)

// Verdict is a Screener's decision on one file.
type Verdict struct {
	// Status is store.MaterialApproved, store.MaterialRejected, or
	// store.MaterialPending to leave the file unlisted and ask again on the
	// next pass (for a screener that works asynchronously).
	Status string
	// Note says why, for moderators. Optional.
	Note string
}

// Opener reads the file being screened back from the bucket. Each call
// starts a new read; the caller closes it.
type Opener func(ctx context.Context) (io.ReadCloser, error)

// Screener decides whether a backed-up file may be listed on the material
// page. It runs after the backup, so it reads the bucket's copy, never
// Discord's, and a rejected file is still kept in the bucket.
//
// A Screener returning an error leaves the file pending and is retried on a
// later pass; it should only do so for transient failures.
type Screener interface {
	Screen(ctx context.Context, m *store.Material, open Opener) (Verdict, error)
}

// Content types a material can have.
const (
	TypePDF  = "application/pdf"
	TypeJPEG = "image/jpeg"
)

var (
	// pdfMagic is how every PDF file starts.
	pdfMagic = []byte("%PDF-")
	// jpegMagic is the start-of-image marker every JPEG begins with.
	jpegMagic = []byte{0xFF, 0xD8, 0xFF}
)

// BasicScreener is the screener in use until a real pipeline is plugged in:
// it approves anything that really is the type it claims to be and rejects
// the rest, so a file merely renamed to .pdf or .jpg never reaches the viewer.
type BasicScreener struct{}

// Screen implements Screener.
func (BasicScreener) Screen(ctx context.Context, m *store.Material, open Opener) (Verdict, error) {
	r, err := open(ctx)
	if err != nil {
		return Verdict{}, err
	}
	defer r.Close()
	head := make([]byte, 1024)
	n, err := io.ReadFull(r, head)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return Verdict{}, err
	}
	head = head[:n]
	var ok bool
	switch m.ContentType {
	case TypePDF:
		// The header may follow some junk; PDF readers accept it anywhere
		// in the first kilobyte.
		ok = bytes.Contains(head, pdfMagic)
	case TypeJPEG:
		ok = bytes.HasPrefix(head, jpegMagic)
	}
	if !ok {
		return Verdict{Status: store.MaterialRejected, Note: "content is not a valid " + m.ContentType}, nil
	}
	return Verdict{Status: store.MaterialApproved}, nil
}
