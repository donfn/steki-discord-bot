# services/web

The public face of the bot: the web pages, the MCP server and the course material. It is
the only service reachable from the internet. It therefore holds the token-signing key
(`JWT_SECRET`), the OAuth application, and the Gemini and Railway tokens, and it is the
only service that can call `blastdoor`. It holds no mail key and links no other
service's logic. Entry point: [`cmd/web`](../../cmd/web/).

Two pages belong to other services: the email consent page and the media-guard captcha.
`web` serves them and their owners answer over RPC.

Related documents: the
[material pipeline](../../docs/ARCHITECTURE.md#course-material-backup-screening-viewing),
the [MCP login](../../docs/ARCHITECTURE.md#mcp-server), the
[address shield](../../docs/ARCHITECTURE.md#ip-shield), and
[OPERATIONS.md](../../docs/OPERATIONS.md) for stickies, uploads and the shield.

## Contents

`handler.go` holds the `Handler` and the adapters through which the pages reach the
service. The other files are `materials.go` (discovery of course files), `sticky.go` (the
silent card at the bottom of each course channel), `links.go` (the 30-second personal
links), `upload.go` (the upload form), `review.go` (administrator cards for held images),
`reports.go` (member reports) and `mcp.go` (which members may read which material).

## Invariants

- Everything signed here uses `JWT_SECRET`, so a compromised `registration` or
  `moderation` cannot forge a material link or an MCP token.
- Data returned by `blastdoor` or by Gemini is untrusted input.
- A deleted Discord message hides its files and never removes them from the bucket.
- Adding or renaming a public route requires an update to Railway's edge allowlist.
- The package must not import `mail` or another service.
  `TestServicesStayInTheirLane` checks this.
