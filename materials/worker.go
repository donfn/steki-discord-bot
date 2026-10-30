package materials

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"steki-bot/store"
)

const (
	// MaxBackupAttempts is how often a copy is tried before the file is left
	// for a moderator to look at.
	MaxBackupAttempts = 8

	// passEvery is how often the worker looks for work nobody woke it for:
	// retries, and anything left by a restart.
	passEvery = 10 * time.Minute

	// batch is how many rows one database read takes.
	batch = 25

	// copyTimeout bounds one file's download and upload.
	copyTimeout = 10 * time.Minute

	// screenTimeout bounds one screening.
	screenTimeout = 2 * time.Minute
)

// ErrSourceGone is returned by a Refresher when the message or attachment no
// longer exists on Discord, so there is no fresh link to retry with.
var ErrSourceGone = errors.New("materials: attachment no longer on discord")

// Refresher returns a fresh download URL for an attachment. Discord's CDN
// links are signed and stop working after about a day.
type Refresher func(ctx context.Context, m *store.Material) (string, error)

// Worker copies discovered materials into the bucket and screens them.
type Worker struct {
	st       *store.Store
	bucket   *Bucket
	screener Screener
	refresh  Refresher
	// changed is called with a home channel whenever its listed files change,
	// so its sticky message can show the new count.
	changed func(homeChannelID string)
	http    *http.Client
	log     *slog.Logger
	wake    chan struct{}
}

// NewWorker returns a Worker. Call Run to start it.
func NewWorker(st *store.Store, bucket *Bucket, screener Screener, refresh Refresher,
	changed func(homeChannelID string), logger *slog.Logger) *Worker {
	return &Worker{
		st:       st,
		bucket:   bucket,
		screener: screener,
		refresh:  refresh,
		changed:  changed,
		http:     &http.Client{Timeout: copyTimeout},
		log:      logger,
		wake:     make(chan struct{}, 1),
	}
}

// Bucket is the bucket the worker copies into.
func (w *Worker) Bucket() *Bucket { return w.bucket }

// Wake asks the worker to look for new work now. It never blocks.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run processes the backlog until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(passEvery)
	defer ticker.Stop()
	for {
		w.pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-ticker.C:
		}
	}
}

// pass works through everything currently waiting: backups first, since a
// file can only be screened once it is in the bucket.
func (w *Worker) pass(ctx context.Context) {
	// Failures are retried on later passes, not within this one, so each
	// sweep walks the backlog once with a cursor.
	var cur store.Cursor
	for ctx.Err() == nil {
		rows, err := w.st.MaterialBacklog(ctx, MaxBackupAttempts, cur, batch)
		if err != nil {
			w.log.Error("materials: read backup backlog", "error", err)
			return
		}
		for _, m := range rows {
			w.backup(ctx, m)
			cur = cur.Next(m)
		}
		if len(rows) < batch {
			break
		}
	}

	cur = store.Cursor{}
	for ctx.Err() == nil {
		rows, err := w.st.ScreeningBacklog(ctx, cur, batch)
		if err != nil {
			w.log.Error("materials: read screening backlog", "error", err)
			return
		}
		for _, m := range rows {
			w.screen(ctx, m)
			cur = cur.Next(m)
		}
		if len(rows) < batch {
			break
		}
	}
}

// backup copies one attachment from Discord into the bucket.
func (w *Worker) backup(ctx context.Context, m *store.Material) {
	log := w.log.With("attachment_id", m.AttachmentID, "channel_id", m.ChannelID, "key", m.ObjectKey)
	ctx, cancel := context.WithTimeout(ctx, copyTimeout)
	defer cancel()

	err := w.copy(ctx, m, m.SourceURL)
	var expired *httpStatusError
	if errors.As(err, &expired) && (expired.code == http.StatusForbidden || expired.code == http.StatusNotFound) {
		// The signed link has most likely expired; ask Discord for a new one.
		url, rerr := w.refresh(ctx, m)
		switch {
		case rerr == nil:
			if serr := w.st.SetMaterialSource(ctx, m.AttachmentID, url); serr != nil {
				log.Warn("materials: save refreshed link", "error", serr)
			}
			err = w.copy(ctx, m, url)
		case errors.Is(rerr, ErrSourceGone):
			err = fmt.Errorf("%w (deleted before it could be backed up)", err)
		default:
			err = fmt.Errorf("%w; refresh link: %v", err, rerr)
		}
	}
	if ctx.Err() != nil && errors.Is(err, context.Canceled) {
		return // shutting down; not the file's fault
	}
	if err != nil {
		log.Warn("materials: backup failed", "attempt", m.BackupAttempts+1, "error", err)
		if ferr := w.st.MarkBackupFailed(context.WithoutCancel(ctx), m.AttachmentID, err.Error()); ferr != nil {
			log.Error("materials: record backup failure", "error", ferr)
		}
		return
	}
	if err := w.st.MarkBackedUp(ctx, m.AttachmentID, time.Now()); err != nil {
		log.Error("materials: record backup", "error", err)
		return
	}
	log.Info("materials: backed up", "bytes", m.Size)
}

type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("download: HTTP %d", e.code) }

// copy streams the file at url into the bucket without holding it in memory.
func (w *Worker) copy(ctx context.Context, m *store.Material, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := w.http.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &httpStatusError{code: resp.StatusCode}
	}
	size := resp.ContentLength
	if size < 0 {
		size = m.Size
	}
	if err := w.bucket.Put(ctx, m.ObjectKey, io.LimitReader(resp.Body, size), size, m.ContentType); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	return nil
}

// screen asks the screener about one backed-up file and records the verdict.
func (w *Worker) screen(ctx context.Context, m *store.Material) {
	log := w.log.With("attachment_id", m.AttachmentID, "channel_id", m.ChannelID)
	ctx, cancel := context.WithTimeout(ctx, screenTimeout)
	defer cancel()

	open := func(ctx context.Context) (io.ReadCloser, error) {
		r, _, err := w.bucket.Open(ctx, m.ObjectKey)
		return r, err
	}
	v, err := w.screener.Screen(ctx, m, open)
	if err != nil {
		log.Warn("materials: screening failed; will retry", "error", err)
		return
	}
	switch v.Status {
	case store.MaterialPending:
		return
	case store.MaterialApproved, store.MaterialRejected:
	default:
		log.Error("materials: screener returned an unknown status", "status", v.Status)
		return
	}
	if err := w.st.SetScreening(ctx, m.AttachmentID, v.Status, v.Note); err != nil {
		log.Error("materials: record screening", "error", err)
		return
	}
	if v.Status == store.MaterialRejected {
		log.Warn("materials: file rejected by screening", "note", v.Note, "filename", m.Filename)
		return
	}
	if w.changed != nil {
		w.changed(m.HomeChannelID)
	}
}
