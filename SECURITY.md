# Security policy

The bot holds students' university email addresses and moderates a community server, so
please report security problems privately.

## Reporting a vulnerability

Use GitHub's **private vulnerability reporting** (Security tab → *Report a
vulnerability*) on this repository. If that is unavailable, contact the repository
maintainers directly and avoid putting details in a public issue or pull request.

Include what you found, how to reproduce it, and what an attacker gains. We will
acknowledge the report, fix it, and credit you if you wish.

## What is in scope

- Anything that exposes a member's email address, Discord identity or notes.
- Forging a signed link, a material link or an MCP token; bypassing the captcha or the
  consent page.
- Reaching one service's secrets or data from another, or from `blastdoor`.
- Escaping `blastdoor`'s sandbox, or abusing the image and file handling.
- Bypassing the IP shield, the media guard or the screening of uploads.
- Reading or writing the bucket's databases without the owning service.

## Design choices that are not vulnerabilities

These are known and documented in [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md):

- The bucket's credentials reach every object, so a compromised service could read the
  other services' shards. Railway has no per-prefix credentials.
- `blastdoor` is unauthenticated on the private network by design; it holds nothing.

## For contributors

- Never commit tokens, keys or `.env` files (`.env` is git-ignored).
- Never point a local run at the production bucket or token.
- Keep secrets in the loader of the one service that needs them; a test enforces it.
