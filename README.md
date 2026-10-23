# Steki Discord Bot

Registration and moderation bot for the Steki Discord server. New members are
given a private `register-<id>` channel, verify a student email via a code sent
with Resend, and receive their role. State lives in PostgreSQL.

## Configuration

Three variables are required; the rest have sane defaults. See `.env.example`.

| Variable        | Required | Purpose                                            |
|-----------------|----------|----------------------------------------------------|
| `DISCORD_TOKEN` | yes      | Bot token                                          |
| `DATABASE_URL`  | yes      | Postgres DSN (`${{Postgres.DATABASE_URL}}` on Railway) |
| `RESEND_API_KEY`| yes      | Resend API key                                     |
| `EMAIL_FROM`    | no       | Verified sender (default `Steki <steki@poiw.org>`) |
| `URL_BLACKLIST` | no       | Comma-separated link substrings to block           |
| `LOG_LEVEL`     | no       | `debug`/`info`/`warn`/`error` (default `info`)     |
| `PORT`          | no       | `/healthz` port (default `8080`)                   |

Discord channel, category and role IDs are compile-time constants in
`config/config.go` and `events/constants.go`. The guild ID is learned from the
gateway at runtime.

## Running locally

```bash
cp .env.example .env   # fill in the three required values
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
- `/email whois <email>` · `/email add <email> [user]` · `/email remove <email>`
- `/bot stats`

Every join, email attempt, code send, retry, failure and role grant is recorded
in `registration_events`, which is what `/reg history` reads.

## One-shot migration

`cmd/migrate` reconstructs the historical email map from the bot's own records —
the moderator log channel (which pairs submitters with email hashes, and
plaintext addresses with those hashes in the "already used" notices) joined with
the old Mongo `usedEmails` hash set. It does not reverse any hash.

Only hashes confirmed as verified (in Mongo, or named in an "already used"
notice) are imported; a bare submission may have been abandoned. An owner is
recorded only when exactly one member ever submitted the hash, and a recovered
plaintext is kept only if it hashes back to the same value.

```bash
go run ./cmd/migrate                # dry run: prints a reconciliation report
go run ./cmd/migrate --commit       # writes verified_emails + legacy_email_hashes
```

Requires `DISCORD_TOKEN`, `DATABASE_URL`, and (optionally) `MONGO_URL`.
