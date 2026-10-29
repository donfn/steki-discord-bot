# Steki Discord Bot

Registration and moderation bot for the Steki Discord server. New members are
given a private `register-<id>` channel and send their student email there. The
bot emails them a link (via Resend) to a consent page that shows their Discord
avatar beside the address and the university's logo; accepting it, behind an
invisible ALTCHA proof of work, binds the address and grants the role at once.
State lives in PostgreSQL.

## Configuration

Four variables are required; the rest have sane defaults. See `.env.example`.

| Variable        | Required | Purpose                                            |
|-----------------|----------|----------------------------------------------------|
| `DISCORD_TOKEN` | yes      | Bot token                                          |
| `DATABASE_URL`  | yes      | Postgres DSN (`${{Postgres.DATABASE_URL}}` on Railway) |
| `RESEND_API_KEY`| yes      | Resend API key                                     |
| `JWT_SECRET`    | yes      | Signs email and captcha links (`openssl rand -hex 32`) |
| `PUBLIC_URL`    | on Railway, no | Base URL of the pages; defaults to `RAILWAY_PUBLIC_DOMAIN` |
| `EMAIL_FROM`    | no       | Verified sender (default `Steki <steki@poiw.org>`) |
| `URL_BLACKLIST` | no       | Comma-separated link substrings to block           |
| `LOG_LEVEL`     | no       | `debug`/`info`/`warn`/`error` (default `info`)     |
| `PORT`          | no       | HTTP port for the pages and `/healthz` (default `8080`) |

Discord channel, category and role IDs are compile-time constants in
`config/config.go` and `events/constants.go`. The guild ID is learned from the
gateway at runtime.

## Running locally

```bash
cp .env.example .env   # fill in the required values
go run .
```

The schema is created automatically on first boot (embedded migrations in
`store/migrations`, applied under a Postgres advisory lock).

## How it stays stable

- **Category overflow** — Discord caps a category at 50 channels. When the
  register category fills, the bot provisions `register-overflow-N` categories
  automatically, so a join can never again fail with `CHANNEL_PARENT_MAX_CHANNELS`.
- **Abandonment reaper** — a registration idle for 1h gets one reminder *in its
  channel* (no DMs); idle for 6h, the member is kicked and the channel removed.
  Kicked members can rejoin and retry.
- **Reconciler** — on every gateway (re)connect and hourly, the bot diffs guild
  members against the database and channels, recreating channels for joins that
  arrived while it was disconnected (members who joined in the last 24h, at most
  25 channels per pass) and cleaning up orphans.
- **No races** — all registration work for one member is serialised, and
  channel placement is serialised guild-wide, so concurrent events can never
  produce two channels for a member or overfill a category.
- **Never kick the verified** — before a kick, the reaper re-checks the row and
  the member's roles; a member the database records as verified but who lost
  their role gets the role back instead.
- **Retries + alerts** — Discord calls retry with backoff on 429/5xx; anything
  that still fails (a channel that cannot be created, an email that cannot be
  sent) is posted to the moderator log channel rather than lost to stdout.

## Moderator commands

Slash commands, restricted to the staff/mod roles, replies are ephemeral:

- `/reg status <user>` · `/reg history <user>` · `/reg stuck`
- `/reg restart <user>` · `/reg verify <user> <email>`
- `/reg reregister <user> [release_email]` — takes the verified roles away and
  opens a fresh registration; the member keeps their address unless
  `release_email` frees it
- `/guard status|restrict|ungrace <user>`
- `/email whois <email>` · `/email add <email> [user]` · `/email remove <email>`
- `/bot stats`

Every join, email attempt, link sent, retry, failure and role grant is recorded
in `registration_events`, which is what `/reg history` reads.

## Email history reconciliation

The old bot stored only unsalted SHA-256 hashes of addresses, with no owner. Its
moderator log channel is the one record of who submitted which, so on every boot
the bot reads that channel in the background, page by page from newest to
oldest, saving each page's evidence and its position (`log_scan_state`,
`legacy_log_evidence`). A restart resumes where the last one stopped; once the
channel's start is reached, later boots only read what was posted since.

After each scan it binds every hash it can: the owner is the one member who
submitted the address, after setting aside the submissions an "already used"
notice refused right after; with two or more such members the hash stays
unowned rather than risk the wrong account. A plaintext address is taken only
from a notice that printed it and only if it hashes back to the same value.
Nothing reverses a hash, and an owner already recorded is never replaced.
