# verify

The public HTTP pages and their middleware. Linked **only** by the `web` service.

Part of the [architecture](../docs/ARCHITECTURE.md#code-layout-and-who-may-import-what).

## Routes

| Route | Page | Answered by |
|---|---|---|
| `GET /verify`, `/verify/challenge`, `POST /verify/solve` | media guard captcha | moderation, over RPC |
| `GET /link`, `/link/challenge`, `POST /link/accept` | email consent page (avatar, address, university) | registration, over RPC |
| `GET /materials` | exchanges a personal link for a session | web |
| `GET /s` | the material page, or its WebSocket | web |
| `GET /s/f/{id}` | a material file | web |
| `POST /s/report` | report a file | web |
| `GET /mcp/setup` | how to connect an AI assistant | web |
| `GET /verify/logo.png`, `/verify/uni/{file}` | images | web |

The OAuth and `/mcp` routes are in [`mcp/`](../mcp/); `/healthz` is in `cmd/web`.

> Adding or renaming a route? Railway's edge rule 404s anything not on its allowlist,
> so a maintainer must update it, see
> [OPERATIONS.md](../docs/OPERATIONS.md#deployment).

## What is here

- `server.go` registers routes. The pages call back through small interfaces
  (`Lifter`, `Consenter`, `MaterialSource`, …) so this package never imports a service.
- `middleware.go` caps requests in flight (128) and applies per-address rate limits.
- `session.go`, `socket.go`, `presence.go` run the material page's one-time tokens and
  WebSocket that carries notes and ink.
- `peerblock.go` talks to `blastdoor`.
- `*.html` are embedded with `go:embed`; the pages are plain HTML and small inline
  scripts, with no build step and no third-party scripts loaded into visitors' browsers.
- Links are verified with [`tokens/`](../tokens/); the captcha is ALTCHA proof of work.

## Tests

`go test ./verify` runs against `httptest` servers and needs nothing else.
