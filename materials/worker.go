package materials

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"steki-bot/store"
)

const (
	// MaxBackupAttempts is how often a copy is tried before the file is left
	// for a moderator to look at.
	MaxBackupAttempts = 8

	// retryAfter is how long the worker waits before trying again what failed
	// in a pass. With nothing failed it sleeps until woken: it never polls.
	retryAfter = 10 * time.Minute

	// batch is how many rows one database read takes.
	batch = 25

	// copyTimeout bounds one file's download and upload.
	copyTimeout = 45 * time.Minute

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
	st       *store.Library
	bucket   *Bucket
	screener Screener
	refresh  Refresher
	// changed is called with a home channel whenever its listed files change,
	// so its sticky message can show the new count.
	changed func(homeChannelID string)
	// held is called when a file is held for review, so its admin card can be
	// posted.
	held func()
	http *http.Client
	log  *slog.Logger
	wake chan struct{}

	// imagesFirst is set when the screener looks at image content: images in
	// course channels are then screened from Discord's copy and reach the
	// bucket only if approved. Other channels are not content-checked.
	imagesFirst bool
	// courses lists the course channels the content check covers.
	courses func(ctx context.Context) ([]string, error)
	// scope is the result of courses for the current pass.
	scope    []string
	scopeSet map[string]bool
	// rescan makes the screening pass visit listed images the content check
	// has never seen. It is set by the rescan command.
	rescan atomic.Bool
	// retryNeeded is set when a pass left work behind because something failed,
	// so the worker comes back on its own. Only its goroutine touches it.
	retryNeeded bool
	// pausedUntil is when screening resumes after the screener ran out of
	// quota. Only the worker's own goroutine touches it.
	pausedUntil time.Time
}

// NewWorker returns a Worker. Call Run to start it.
func NewWorker(st *store.Library, bucket *Bucket, screener Screener, refresh Refresher,
	changed func(homeChannelID string), held func(), courses func(ctx context.Context) ([]string, error),
	logger *slog.Logger) *Worker {
	ps, _ := screener.(PreScreener)
	return &Worker{
		held:        held,
		courses:     courses,
		imagesFirst: ps != nil && ps.PreScreens(TypeJPEG),
		st:          st,
		bucket:      bucket,
		screener:    screener,
		refresh:     refresh,
		changed:     changed,
		http:        &http.Client{Timeout: copyTimeout},
		log:         logger,
		wake:        make(chan struct{}, 1),
	}
}

// Bucket is the bucket the worker copies into.
func (w *Worker) Bucket() *Bucket { return w.bucket }

// Vision reports whether images are checked by content.
func (w *Worker) Vision() bool { return w.imagesFirst }

// Unchecked counts the listed images in course channels the content check has
// not seen yet.
func (w *Worker) Unchecked(ctx context.Context) (int, error) {
	ids, err := w.courses(ctx)
	if err != nil {
		return 0, err
	}
	return w.st.CountUnchecked(ctx, ids)
}

// Rescan queues every listed image in a course channel the content check has
// not seen yet. The queue is the database, so a restart only needs the
// command run again.
func (w *Worker) Rescan() {
	w.rescan.Store(true)
	w.Wake()
}

// Wake asks the worker to look for new work now. It never blocks.
func (w *Worker) Wake() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run processes the backlog until ctx is cancelled. It works through what
// the database already holds, then sleeps until Wake. It comes back by itself
// only for work that failed, or when a screening quota is expected back.
func (w *Worker) Run(ctx context.Context) {
	for {
		w.retryNeeded = false
		w.pass(ctx)

		var timeout <-chan time.Time
		wait := time.Duration(0)
		if w.retryNeeded {
			wait = retryAfter
		}
		if d := time.Until(w.pausedUntil); d > 0 && (wait == 0 || d < wait) {
			wait = d
		}
		var timer *time.Timer
		if wait > 0 {
			timer = time.NewTimer(wait)
			timeout = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-w.wake:
		case <-timeout:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// pass works through everything currently waiting. Images are screened
// first when the screener reads their content, since a flagged one must not
// be copied; then backups, since a PDF can only be screened once it is in the
// bucket; then whatever is left to screen.
func (w *Worker) pass(ctx context.Context) {
	w.scope, w.scopeSet = nil, nil
	if w.imagesFirst {
		// Without the list of course channels nothing can be told apart, and an
		// image would be copied unchecked, so the pass waits for the next one.
		ids, err := w.courses(ctx)
		if err != nil {
			w.log.Error("materials: list course channels", "error", err)
			return
		}
		w.scope, w.scopeSet = ids, make(map[string]bool, len(ids))
		for _, id := range ids {
			w.scopeSet[id] = true
		}
	}
	w.screenPass(ctx)

	// Failures are retried on later passes, not within this one, so each
	// sweep walks the backlog once with a cursor.
	var cur store.Cursor
	for ctx.Err() == nil {
		rows, err := w.st.MaterialBacklog(ctx, MaxBackupAttempts, cur, batch, w.scope)
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

	w.screenPass(ctx)
}

// screenPass screens the waiting files, oldest first. When the screener runs
// out of quota the pass stops and the files stay queued until it is back.
func (w *Worker) screenPass(ctx context.Context) {
	var cur store.Cursor
	for ctx.Err() == nil && !w.paused() {
		rows, err := w.st.ScreeningBacklog(ctx, cur, batch, w.scope, w.rescan.Load())
		if err != nil {
			w.log.Error("materials: read screening backlog", "error", err)
			return
		}
		for _, m := range rows {
			if w.paused() || ctx.Err() != nil {
				return
			}
			w.screen(ctx, m)
			cur = cur.Next(m)
		}
		if len(rows) < batch {
			if w.rescan.Load() && !w.paused() {
				w.rescan.Store(false) // everything has been seen
				w.log.Info("materials: rescan finished")
			}
			return
		}
	}
}

func (w *Worker) paused() bool { return time.Now().Before(w.pausedUntil) }

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
		w.retryNeeded = true
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
	// An image approved before its backup is listed only now.
	if m.Status == store.MaterialApproved && w.changed != nil {
		w.changed(m.HomeChannelID)
	}
}

type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("download: HTTP %d", e.code) }

// copy compresses the file at url into the bucket without holding it in memory.
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
	if err := w.bucket.Store(ctx, m.ObjectKey, io.LimitReader(resp.Body, size), size, m.ContentType); err != nil {
		return fmt.Errorf("upload: %w", err)
	}
	return nil
}

// screen asks the screener about one file and records the verdict. A file
// not in the bucket yet is read from Discord.
func (w *Worker) screen(ctx context.Context, m *store.Material) {
	log := w.log.With("attachment_id", m.AttachmentID, "channel_id", m.ChannelID)
	ctx, cancel := context.WithTimeout(ctx, screenTimeout)
	defer cancel()

	open := func(ctx context.Context) (io.ReadCloser, error) {
		r, _, err := w.bucket.Open(ctx, m.ObjectKey)
		return r, err
	}
	if m.BackedUpAt == nil {
		open = func(ctx context.Context) (io.ReadCloser, error) { return w.openSource(ctx, m) }
	}
	screener := w.screener
	if w.imagesFirst && !w.scopeSet[m.HomeChannelID] {
		screener = BasicScreener{} // outside the course channels: format check only
	}
	v, err := screener.Screen(ctx, m, open)
	var limit *RateLimitError
	switch {
	case errors.As(err, &limit):
		w.pausedUntil = time.Now().Add(limit.RetryAfter)
		return
	case errors.Is(err, ErrSourceGone):
		// Deleted before it could be looked at: there is nothing to list.
		v, err = Verdict{Status: store.MaterialRejected, Note: "deleted before it could be screened"}, nil
	}
	if err != nil {
		log.Warn("materials: screening failed; will retry", "error", err)
		w.retryNeeded = true
		return
	}
	switch v.Status {
	case store.MaterialPending:
		return
	case store.MaterialApproved, store.MaterialRejected, store.MaterialReview:
	default:
		log.Error("materials: screener returned an unknown status", "status", v.Status)
		return
	}
	if v.Status == store.MaterialReview {
		w.hold(ctx, m, v.Note)
		return
	}
	if m.Status != v.Status { // a rescan of a listed file that passes changes nothing
		if err := w.st.SetScreening(ctx, m.AttachmentID, v.Status, v.Note); err != nil {
			log.Error("materials: record screening", "error", err)
			return
		}
	}
	if v.Checked {
		if err := w.st.MarkVisionChecked(ctx, m.AttachmentID); err != nil {
			log.Error("materials: record content check", "error", err)
		}
	}
	if v.Status == store.MaterialRejected {
		log.Warn("materials: file rejected by screening", "note", v.Note, "filename", m.Filename)
		return
	}
	// A file still to be copied is listed once the backup is done.
	if m.BackedUpAt != nil && m.Status != store.MaterialApproved && w.changed != nil {
		w.changed(m.HomeChannelID)
	}
	if m.BackedUpAt == nil {
		w.Wake() // the backup is next
	}
}

// hold takes a flagged file off the page and out of the bucket and asks for an
// admin's decision.
func (w *Worker) hold(ctx context.Context, m *store.Material, note string) {
	log := w.log.With("attachment_id", m.AttachmentID, "channel_id", m.ChannelID)
	if m.BackedUpAt != nil {
		if err := w.bucket.Delete(ctx, m.ObjectKey); err != nil {
			log.Error("materials: remove flagged file from the bucket; will retry", "error", err)
			w.retryNeeded = true
			return
		}
	}
	if err := w.st.HoldMaterial(ctx, m.AttachmentID, note); err != nil {
		log.Error("materials: hold flagged file", "error", err)
		return
	}
	log.Warn("materials: file held for review", "note", note, "filename", m.Filename)
	if m.BackedUpAt != nil && m.Status == store.MaterialApproved && w.changed != nil {
		w.changed(m.HomeChannelID)
	}
	if w.held != nil {
		w.held()
	}
}

// openSource reads a file from Discord, refreshing its signed link once if it
// has expired.
func (w *Worker) openSource(ctx context.Context, m *store.Material) (io.ReadCloser, error) {
	r, err := w.get(ctx, m.SourceURL)
	var expired *httpStatusError
	if errors.As(err, &expired) && (expired.code == http.StatusForbidden || expired.code == http.StatusNotFound) {
		url, rerr := w.refresh(ctx, m)
		if rerr != nil {
			return nil, rerr // includes ErrSourceGone
		}
		if serr := w.st.SetMaterialSource(ctx, m.AttachmentID, url); serr != nil {
			w.log.Warn("materials: save refreshed link", "error", serr)
		}
		return w.get(ctx, url)
	}
	return r, err
}

// get starts a download; the caller closes the body.
func (w *Worker) get(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := w.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, &httpStatusError{code: resp.StatusCode}
	}
	return resp.Body, nil
}
