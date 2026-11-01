package s3db

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/klauspost/compress/zstd"
)

// The write-ahead log's layout, from https://www.sqlite.org/fileformat.html.
const (
	walHeaderSize  = 32
	walFrameHeader = 24
	walMagicLE     = 0x377f0682
	walMagicBE     = 0x377f0683
)

// walCursor is how far into one incarnation of the log shipping has got. SQLite
// gives the log new salts each time it starts it over, so the salts tell a log
// that continues from one that replaced it.
type walCursor struct {
	offset int64
	salt1  uint32
	salt2  uint32
	// sum1 and sum2 are the running checksum at offset.
	sum1, sum2 uint32
}

// walBatch is the committed changes read from the log: for each page its newest
// image, and the database's size in pages after the last commit.
type walBatch struct {
	pageSize uint32
	dbPages  uint32
	pages    map[uint32][]byte
}

// errWALRestarted says the log is not the one the cursor was reading: SQLite
// began it again, so the frames the cursor had not reached are only in the main
// database file now.
var errWALRestarted = errors.New("s3db: the write-ahead log restarted")

// readWAL returns the transactions committed after cur and the cursor past
// them, with no batch when there are none. It stops at the first frame that
// fails its checksum or belongs to no commit: those are a writer still at work.
// Committed frames never change until the log restarts, which the salts reveal.
func readWAL(path string, cur walCursor) (*walBatch, walCursor, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		if cur.offset > 0 {
			return nil, cur, errWALRestarted
		}
		return nil, cur, nil
	}
	if err != nil {
		return nil, cur, err
	}
	defer f.Close()

	var hdr [walHeaderSize]byte
	if _, err := f.ReadAt(hdr[:], 0); err != nil {
		if errors.Is(err, io.EOF) { // empty, or the header is being written
			if cur.offset > 0 {
				return nil, cur, errWALRestarted
			}
			return nil, cur, nil
		}
		return nil, cur, err
	}
	magic := binary.BigEndian.Uint32(hdr[0:])
	var order binary.ByteOrder
	switch magic {
	case walMagicLE:
		order = binary.LittleEndian
	case walMagicBE:
		order = binary.BigEndian
	default:
		return nil, cur, fmt.Errorf("s3db: not a write-ahead log (magic %#x)", magic)
	}
	pageSize := binary.BigEndian.Uint32(hdr[8:])
	salt1, salt2 := binary.BigEndian.Uint32(hdr[16:]), binary.BigEndian.Uint32(hdr[20:])

	switch {
	case cur.offset == 0:
		s1, s2 := walChecksum(order, 0, 0, hdr[:24])
		if s1 != binary.BigEndian.Uint32(hdr[24:]) || s2 != binary.BigEndian.Uint32(hdr[28:]) {
			return nil, cur, nil // the header is being written
		}
		cur = walCursor{offset: walHeaderSize, salt1: salt1, salt2: salt2, sum1: s1, sum2: s2}
	case salt1 != cur.salt1 || salt2 != cur.salt2:
		return nil, cur, errWALRestarted
	}

	frame := make([]byte, walFrameHeader+int(pageSize))
	pos := cur
	next := cur
	pending := map[uint32][]byte{}
	batch := &walBatch{pageSize: pageSize, pages: map[uint32][]byte{}}
	for {
		if _, err := f.ReadAt(frame, pos.offset); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, cur, err
		}
		pgno := binary.BigEndian.Uint32(frame[0:])
		dbSize := binary.BigEndian.Uint32(frame[4:])
		if binary.BigEndian.Uint32(frame[8:]) != salt1 || binary.BigEndian.Uint32(frame[12:]) != salt2 {
			break
		}
		s1, s2 := walChecksum(order, pos.sum1, pos.sum2, frame[:8])
		s1, s2 = walChecksum(order, s1, s2, frame[walFrameHeader:])
		if s1 != binary.BigEndian.Uint32(frame[16:]) || s2 != binary.BigEndian.Uint32(frame[20:]) {
			break
		}
		pos.offset += int64(len(frame))
		pos.sum1, pos.sum2 = s1, s2
		pending[pgno] = bytes.Clone(frame[walFrameHeader:])
		if dbSize != 0 { // a commit: everything so far is real
			for k, v := range pending {
				batch.pages[k] = v
			}
			clear(pending)
			batch.dbPages = dbSize
			next = pos
		}
	}
	if next.offset == cur.offset {
		return nil, cur, nil
	}
	return batch, next, nil
}

// walChecksum is SQLite's checksum over b, whose length is a multiple of eight.
func walChecksum(order binary.ByteOrder, s1, s2 uint32, b []byte) (uint32, uint32) {
	for i := 0; i+8 <= len(b); i += 8 {
		s1 += order.Uint32(b[i:]) + s2
		s2 += order.Uint32(b[i+4:]) + s1
	}
	return s1, s2
}

// A segment is a zstd stream of: segmentMagic, then the page size, the
// database's size in pages and the page count as big-endian uint32, then each
// page as its uint32 number and its bytes.
var segmentMagic = []byte("STKSEG1\n")

var (
	segEncoder, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault),
		zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(1<<20))
	segDecoder, _ = zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true))
)

// encodeSegment serialises a batch, with pages in order so that the same batch
// always produces the same bytes.
func encodeSegment(b *walBatch) []byte {
	nums := make([]uint32, 0, len(b.pages))
	for n := range b.pages {
		nums = append(nums, n)
	}
	sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })

	raw := make([]byte, 0, len(segmentMagic)+12+len(nums)*(4+int(b.pageSize)))
	raw = append(raw, segmentMagic...)
	raw = binary.BigEndian.AppendUint32(raw, b.pageSize)
	raw = binary.BigEndian.AppendUint32(raw, b.dbPages)
	raw = binary.BigEndian.AppendUint32(raw, uint32(len(nums)))
	for _, n := range nums {
		raw = binary.BigEndian.AppendUint32(raw, n)
		raw = append(raw, b.pages[n]...)
	}
	return segEncoder.EncodeAll(raw, nil)
}

// applySegment writes a segment's pages into the database file at f and sets
// the file to the size the commit left it. Nothing has the file open while this
// runs: it is part of rebuilding the database before SQLite opens it.
func applySegment(f *os.File, encoded []byte) error {
	raw, err := segDecoder.DecodeAll(encoded, nil)
	if err != nil {
		return fmt.Errorf("decompress: %w", err)
	}
	if !bytes.HasPrefix(raw, segmentMagic) || len(raw) < len(segmentMagic)+12 {
		return errors.New("not a segment")
	}
	raw = raw[len(segmentMagic):]
	pageSize := int64(binary.BigEndian.Uint32(raw[0:]))
	dbPages := int64(binary.BigEndian.Uint32(raw[4:]))
	count := int(binary.BigEndian.Uint32(raw[8:]))
	raw = raw[12:]
	if int64(len(raw)) != int64(count)*(4+pageSize) {
		return fmt.Errorf("holds %d bytes, want %d pages of %d", len(raw), count, pageSize)
	}
	for range count {
		n := int64(binary.BigEndian.Uint32(raw))
		if n == 0 {
			return errors.New("holds page number zero")
		}
		if _, err := f.WriteAt(raw[4:4+pageSize], (n-1)*pageSize); err != nil {
			return err
		}
		raw = raw[4+pageSize:]
	}
	return f.Truncate(dbPages * pageSize)
}
