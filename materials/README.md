# materials

Backs up the PDFs and JPEGs members post to the `steki-backups` bucket and decides which
of them the material page may list. Linked only by `web`.

Discovery (which channel, which message) lives with the Discord handlers in
[`services/web`](../services/web/); this package starts from a row in the `materials`
table. The flow is drawn in
[ARCHITECTURE.md](../docs/ARCHITECTURE.md#course-material-backup-screening-viewing).

## Pipeline

1. **Worker** (`worker.go`) copies the file from Discord into the bucket. If the link
   has expired it asks a `Refresher` for a fresh one; if the message is gone
   (`ErrSourceGone`) it gives up.
2. **Screener** (`screen.go`) returns a `Verdict`: approved, rejected or pending.
   - format check: is it really a PDF or JPEG?
   - `gemini.go`: JPEGs go through a Gemini content check. The model's answer is forced
     into a JSON schema and the image is untrusted, so text inside it is never an order.
     Held images become admin review cards.
3. A file is **listed only when approved**. Rejected files stay in the bucket; so does
   everything after its message is deleted.
4. **Compression** (`compress.go`): an object is a small header followed by a zstd
   stream, so reads can tell compressed objects from older plain ones. Compression runs
   in [`blastdoor`](../blastdoor/); `convert.go` also uses it for HEIC.
5. **Reports** (`report.go`): a member can report a file; the judge runs at once and a
   report that holds pulls the file off the page and posts a card.

## Adding a screener

Implement `materials.Screener` and pass it to `materials.NewWorker` in
`services/web/handler.go`. It may return `pending` to decide asynchronously, and
`RateLimitError` when its service is out of quota so the worker backs off.

## Tests

`go test ./materials` uses fakes and `httptest` servers (including a fake Gemini); it
needs no network or credentials.
