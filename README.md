# Steki Discord Bot

Registration and moderation bot for the Steki Discord server. New members are
given a private `register-<id>` channel and send their student email there. The
bot emails them a link (via Resend) to a consent page that shows their Discord
avatar beside the address and the university's logo; accepting it, behind an
invisible ALTCHA proof of work, binds the address and grants the role at once.
State lives in an embedded SQLite file, so there is no database server to run.

## Configuration

Three variables are required (plus a public URL); the rest have sane defaults.
See `.env.example`.

| Variable        | Required | Purpose                                            |
|-----------------|----------|----------------------------------------------------|
| `DISCORD_TOKEN` | yes      | Bot token                                          |
| `DATABASE_PATH` | no       | SQLite file. Defaults to `steki.db` inside the Railway volume (`RAILWAY_VOLUME_MOUNT_PATH`), or the working directory off Railway. On Railway the bot refuses to start without a volume, since the database would be erased on every deploy |
| `DATABASE_URL`  | no       | Old Postgres DSN, read only to copy its data across once (see [Moving off Postgres](#moving-off-postgres)) |
| `RESEND_API_KEY`| yes      | Resend API key                                     |
| `JWT_SECRET`    | yes      | Signs email and captcha links (`openssl rand -hex 32`) |
| `PUBLIC_URL`    | on Railway, no | Base URL of the pages; defaults to `RAILWAY_PUBLIC_DOMAIN` |
| `EMAIL_FROM`    | no       | Verified sender (default `Steki <steki@poiw.org>`) |
| `URL_BLACKLIST` | no       | Comma-separated link substrings to block           |
| `LOG_LEVEL`     | no       | `debug`/`info`/`warn`/`error` (default `info`)     |
| `PORT`          | no       | HTTP port for the pages and `/healthz` (default `8080`) |
| `RAILWAY_TOKEN` | no       | Railway project token; enables automatic Under Attack Mode (see below) |

Discord channel, category and role IDs are compile-time constants in
`config/config.go` and `events/constants.go`. The guild ID is learned from the
gateway at runtime.

## Automatic Under Attack Mode

With `RAILWAY_TOKEN` set (a project token), the bot turns on Railway's Under
Attack Mode for its own service for one hour when it sees either more than
5 requests/s sustained over 10 seconds, or more than 10 `404` responses in an
hour. `/healthz` is not counted. It posts to the log channel when it does, waits
out the hour before it can fire again, and retries after 5 minutes if Railway
refuses. The Railway CLI is installed in the image for this.

## Running locally

```bash
cp .env.example .env   # fill in the required values
go run .
```

The schema is created automatically on first boot (embedded migrations in
`store/migrations`). The database is the file `steki.db` next to where you run
it; delete it to start over. The store's tests run against throwaway SQLite
files, so `go test ./...` needs no database.

## Memory footprint

The whole deployment is the bot process and one SQLite file on a volume; there
is no database service. What keeps the process small:

- **SQLite instead of Postgres.** The database runs inside the bot with two
  connections and a 1 MiB page cache each, replacing a separate Postgres
  service that idled at 50-70 MB.
- **A soft Go heap limit of 24 MiB and `GOGC=40`** (`tuneRuntime` in `main.go`);
  `GOMEMLIMIT` and `GOGC` in the environment override them.
- **Only the discordgo state the bot reads is tracked**: channels, roles and
  members. Presences, voice, emojis, stickers and threads are switched off, and
  the unused emoji intent is not requested.
- **A fixed ring of the last 1500 messages** (`events/msgcache.go`) backs the
  edit and delete logs, instead of discordgo's cache of 200 messages for every
  channel.
- **The phishing feeds are streamed into a sorted slice of 64-bit hashes**, 8
  bytes a domain rather than a map of strings (about 0.7 MB instead of 7.5 MB
  for 90,000 domains), and never held as a full list while refreshing.
- **The hourly reconciler keeps a trimmed copy of each member** rather than the
  whole decoded payload.

## Moving off Postgres

Earlier versions kept their data in Railway Postgres. To move it:

1. Attach a volume to the bot service (any mount path, e.g. `/data`).
2. Keep `DATABASE_URL` pointing at the Postgres service (it already does, via
   `${{Postgres.DATABASE_URL}}`) and deploy this version.
3. On that first boot, with an empty SQLite file, the bot copies all seven tables
   in one consistent snapshot, checks the row counts, and records the import in
   `schema_migrations` so it never runs again. If the copy fails the bot does not
   start, because an empty database would let members reuse taken addresses.
4. Once the log shows `imported postgres data`, remove `DATABASE_URL` and delete
   the Postgres service. The import code in `store/pgimport.go` and the pgx
   dependency can then be deleted too.

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
- `/reg forget <user>` — erases a member who has left the server: their member
  row, registrations, history, guard and help state. Their addresses stay
  recorded and spent, but lose their owner
- `/guard status|restrict|ungrace <user>`
- `/email` — create, read, update and delete for the address tables. Wherever
  it asks for an email it also takes a 64-character SHA-256 hash, for the
  addresses imported from the old bot that were never recovered in plaintext:
  - `/email add <email|hash> [user]` · `/email whois <email|hash>` ·
    `/email list <user>`
  - `/email update <email|hash> [user | unlink] [new_email]` — move an address
    to another account, drop its owner, or correct a mistyped address
  - `/email remove <email|hash>` — frees the address in both forms
- `/bot stats`

Every join, email attempt, link sent, retry, failure and role grant is recorded
in `registration_events`, which is what `/reg history` reads.
