# services/web

The public face of the bot: the pages, the MCP server and the course material. It is the
only service on the internet, so it is the only one holding **`JWT_SECRET`**, the OAuth
application, the Gemini and Railway tokens, and the only one that can reach `blastdoor`.
It has no mail key and links no other service's logic.

Entry point: [`cmd/web`](../../cmd/web/). The two pages that belong to other services
(email consent, media-guard captcha) are served here and answered by their owners over
RPC.

Diagrams: [material pipeline](../../docs/ARCHITECTURE.md#course-material-backup-screening-viewing),
[MCP login](../../docs/ARCHITECTURE.md#mcp-server),
[IP shield](../../docs/ARCHITECTURE.md#ip-shield). Behaviour of stickies, uploads and
the shield is in [OPERATIONS.md](../../docs/OPERATIONS.md).

## Files

| File | Holds |
|---|---|
| `handler.go` | `Handler`, gateway registration, RPC routes, the adapters the pages call |
| `materials.go` | discovery of course files, `/materials scan`, course category IDs |
| `sticky.go` | the silent bottom-of-channel card per course channel |
| `links.go` | the personal 30-second links behind the buttons |
| `upload.go` | the upload form: title, files, post, pin, credit |
| `review.go` | admin cards for images the content check holds |
| `reports.go` | members reporting a file |
| `mcp.go` | the bot's side of MCP: who may read what |

## Invariants

- Everything signed here uses `JWT_SECRET`; a compromised `registration` or `moderation`
  must not be able to forge a material link or MCP token.
- Anything that comes back from `blastdoor` or from Gemini is untrusted input.
- Never remove a file from the bucket because a message was deleted; hide it.
- Adding or renaming a public route also means updating Railway's edge allowlist.
- Must not import `mail` or the other services (enforced).
