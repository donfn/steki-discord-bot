# verify

The public HTTP pages and their middleware. Only the `web` service links this package.
See the [architecture](../../docs/ARCHITECTURE.md#code-layout-and-who-may-import-what).

## Routes

| Route | Page | Answered by |
|---|---|---|
| `GET /verify`, `/verify/challenge`, `POST /verify/solve` | media guard captcha | moderation, over RPC |
| `GET /link`, `/link/challenge`, `POST /link/accept` | email consent page (avatar, address, university) | registration, over RPC |
| `GET /materials` | exchanges a personal link for a session | web |
| `GET /s` | the material page, or its WebSocket | web |
| `GET /s/f/{id}` | a material file | web |
| `POST /s/report` | report a file | web |
| `GET /mcp/setup` | instructions for connecting an AI assistant | web |
| `GET /verify/logo.png`, `/verify/uni/{file}` | images | web |

The OAuth and `/mcp` routes are in [`mcp/`](../mcp/), and `/healthz` is in `cmd/web`.

Railway's edge rule returns 404 for any path not on its allowlist. Adding or renaming a
route therefore also requires a maintainer to update the rule; see
[OPERATIONS.md](../../docs/OPERATIONS.md#deployment).

## Design

- The package does not import any service. The pages reach their data through small
  interfaces (`Lifter`, `Consenter`, `MaterialSource`), which the `web` service
  implements.
- `middleware.go` limits the requests in flight to 128 and applies per-address rate
  limits.
- `session.go`, `socket.go` and `presence.go` implement the one-time tokens and the
  WebSocket that carries notes and ink for the material page. `peerblock.go` talks to
  `blastdoor`.
- The `*.html` files are embedded with `go:embed`. The pages use plain HTML and short
  inline scripts, with no build step and no third-party script in a visitor's browser.
- Links are verified with [`tokens/`](../tokens/). The captcha is an ALTCHA proof of work.

## Testing

`go test ./verify` runs against `httptest` servers and needs nothing else.
