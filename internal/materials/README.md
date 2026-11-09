# materials

Copies the PDFs and JPEGs that members post into the `steki-backups` bucket and decides
which of them the material page may list. Only the `web` service links this package. The
flow is drawn in the
[architecture](../../docs/ARCHITECTURE.md#course-material-backup-screening-viewing).

Finding files (which channel, which message) is done by the Discord handlers in
[`services/web`](../../services/web/). This package starts from a row in the `materials`
table.

## Pipeline

1. The **worker** (`worker.go`) copies the file from Discord into the bucket. If the link
   has expired it asks a `Refresher` for a new one. If the message no longer exists
   (`ErrSourceGone`) it stops.
2. The **screener** (`screen.go`) returns a `Verdict`: approved, rejected or pending. It
   checks that the file is really a PDF or a JPEG. For JPEGs, `gemini.go` adds a content
   check by a Gemini model, whose answer is constrained to a JSON schema. The image is
   untrusted, so any text inside it is treated as data and never as an instruction. Held
   images become review cards for administrators. The same answer rates an approved image
   as sure, unsure or not final exam exercises (`Verdict.Exam`); the worker hands the first
   two to `services/web/exam.go`, which grants the exam role.
3. A file is **listed only when approved**. A rejected file stays in the bucket, as does
   every file whose message was later deleted.
4. **Compression** (`compress.go`) stores an object as a small header followed by a zstd
   stream, so a reader can distinguish compressed objects from older uncompressed ones.
   The work is done by [`blastdoor`](../../blastdoor/), which `convert.go` also uses for HEIC
   images.
5. **Reports** (`report.go`): a member can report a file. The check runs at once, and a
   report that stands removes the file from the page and posts a card.

## Adding a screener

Implement `materials.Screener` and pass it to `materials.NewWorker` in
`services/web/handler.go`. A screener may return `pending` to decide later, and
`RateLimitError` when its provider's quota is exhausted, which makes the worker back off.

## Testing

`go test ./materials` uses fakes and `httptest` servers, including a fake Gemini, and
needs neither network access nor credentials.
