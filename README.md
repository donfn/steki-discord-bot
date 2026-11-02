# Steki Discord Bot

The bot of the Steki Discord server, as three services and one helper. Discord sees
a single bot: each service keeps its own gateway connection and ignores what is not
its own.

| Service | Does | Holds | Public |
|---------|------|-------|--------|
| `registration` | register channels, the email link, the reaper, `/reg`, `/email` | Resend key, members' addresses | no |
| `moderation` | screens messages, media guard, ghost role, edit and delete logs, `/guard`, `/member`, `/rules`, `/bot`; installs the slash commands | message cache, phishing list | no |
| `web` | public pages, MCP server, course material, IP shield, `/materials`, `/mcp` | `JWT_SECRET`, OAuth, Gemini and Railway tokens | yes |
| `blastdoor` | parses uploaded images, compresses files, in a sandbox | nothing | no |

New members get a private `register-<id>` channel and give their student email
there. The registration service emails a link (via Resend) to a consent page that
shows their Discord avatar beside the address and the university's logo; accepting
it, behind an invisible ALTCHA proof of work, binds the address and grants the role.
The page is served by `web` and answered by `registration`.

`TestServicesStayInTheirLane` fails if a service starts linking code outside its
scope, `TestSecretsStayWithTheirService` if it starts reading another's secret, and
`TestEveryCommandHasOneOwner` if a command has no owner or two.

## Talking between services

Nothing shares a database. A service reaches another's data only through its
internal API (`rpc/`): signed JSON on port 8081, which has no public route. Requests
are signed with a key derived from `INTERNAL_SECRET` and refused after a minute, so
nothing else on the private network can call them; blastdoor has no secret.

Links are signed with two keys. Email and captcha links use a key derived from
`INTERNAL_SECRET`, since `registration` and `moderation` issue them. Everything only
`web` issues uses `JWT_SECRET`, so a compromised `registration` or `moderation`
cannot forge a material link or an MCP token.

Screening is no longer a step before recording: `moderation` and `web` see each
message at the same moment. What `moderation` deletes is hidden by the delete event;
for what it only restricts the author over, it tells `web` to drop the message.

## The database

Each shard is a SQLite file on the container's scratch disk with one writing
process; the bucket holds the truth (`s3db/`). No volume.

| Shard | Owner | Holds | A write returns |
|-------|-------|-------|-----------------|
| `registry` | registration | addresses, members, registrations, bans | once in the bucket |
| `guard` | moderation | media guard | once in the bucket |
| `activity` | moderation | when members last spoke | at once; shipped every 30 s |
| `library` | web | materials, reports, stickies, IP blocks | once in the bucket |

```
db/<shard>/manifest.json   the commit point: which objects make the database
db/<shard>/snap/…          whole images, the current one and three before it
db/<shard>/seg/…           the pages changed since the current image
db/<shard>/daily/…         one image a day, kept for two weeks
material/…                 course material, blastdoor-compressed
user-notes/…               members' notes, one SQLite file each
```

Nothing under `db/` is compressed, neither by blastdoor nor by the engine: an image
opens as it is with any SQLite tool. `materials.object_key` does not include
`material/`; the bucket client adds it.

- **Commit.** SQLite commits to its write-ahead log as usual. One goroutine reads the
  committed frames out of the log, uploads them as a segment and adds it to the
  manifest. A wrapper around the SQL driver holds each write's result back until
  then, so the store's SQL knows nothing of this. Writes waiting together share an
  upload. Reads never wait, so they keep working while the bucket is down.
- **Fencing.** A starting service raises the manifest's epoch, and every manifest
  write is conditional on the etag it last saw. An older deployment's next write is
  refused and it shuts down; if restarted, it finds itself on the retired list and
  stays out. A service refuses to start on a bucket that does not refuse a stale
  conditional write (`s3db.Verify`).
- **Compaction.** Automatic checkpoints are off, so the main file only changes when
  the engine says so, which makes it a stable image to compress while writes carry
  on. If the log ever restarts with frames unshipped, no segment is written until a
  new image exists.
- **Failure.** With the bucket down a write returns an error saying it is not yet
  durable, and shipping keeps retrying. A missing or damaged object, or a shard that
  does not exist, stops the service: it never starts from less than the whole
  database, since an empty registry would let members reuse taken addresses.
- **Notes.** Members' page notes keep their own per-member object (`usernotes/`),
  uploaded every 15 s while open and on shutdown; a killed container can lose up to
  that much of one member's typing.

The bucket's credentials reach every object, so a compromised service could read
other shards from the bucket directly. Railway has no per-prefix credentials;
separate buckets would be the way to close that.

## Configuration

Each service reads only its own variables and refuses to start with a list of any
that are missing. See `.env.example`.

| Variable | registration | moderation | web |
|----------|:---:|:---:|:---:|
| `DISCORD_TOKEN`, `INTERNAL_SECRET`, `BUCKET_*` | ✓ | ✓ | ✓ |
| `PUBLIC_URL` | ✓ | ✓ | from its domain |
| `REGISTRATION_URL` | | ✓ | ✓ |
| `MODERATION_URL` | ✓ | | ✓ |
| `WEB_URL` | ✓ | ✓ | |
| `RESEND_API_KEY`, `EMAIL_FROM` | ✓ | | |
| `URL_BLACKLIST` | | ✓ | |
| `JWT_SECRET`, `BLASTDOOR_URL` | | | ✓ |
| `DISCORD_CLIENT_ID/SECRET`, `GOOGLE_AI_*`, `RAILWAY_TOKEN` | | | optional |
| `LOG_LEVEL`, `POSTHOG_*` | optional | optional | optional |

Discord channel, category and role IDs are compile-time constants in
`config/config.go` and `bot/constants.go`. The guild ID is learned from the gateway.

Each service's build and deploy settings are recorded in `cmd/<service>/settings.toml`
and set on the service in Railway, including watch patterns, so a change only
redeploys the services built from it.

## Course material

Every PDF and JPEG posted in a course channel
or one of its threads is copied to the `steki-backups` bucket, under a folder
per channel name (`<channel>/<thread>/<attachment id>-<file name>`). New posts
are picked up live. The history before the bot, and anything posted while it was
offline, is found by `/materials scan`, which walks every course channel and
thread back to its first message and resumes across restarts; it never runs by
itself. Deleting a message hides its files from the material page but never
removes them from the bucket.

Each channel in the course categories (`courseCategoryIDs` in
`services/web/materials.go`) keeps a bot message at its bottom saying how many files
members have shared, with two buttons:

- **Προβολή υλικού μαθήματος** answers, to that member alone, with one short
  line and an «Άνοιγμα χώρου μελέτης» button. The
  button holds a personal link to `/materials`, valid for 30 seconds and good
  for one visit. The reply counts down with a bar and is deleted when the
  link is opened or runs out. After a change to the card, `/materials stickies`
  edits every existing sticky in place. The page lists every file of the channel and
  its threads, searchable, with PDFs (pdf.js) and images viewed in the page.
  It downloads every file as soon as it opens. Studied marks, the member's
  own titles, notes and ink are saved per member (`study_notes`) and come back
  on any device; the latest save wins. The page sends them over a WebSocket on
  `/s`, opened with a one-time token that lasts 30 minutes; the server sends a
  fresh one every 5 minutes so the page can reconnect after a drop. If it
  cannot (the token ran out), the page turns read-only. The MCP announcement's «Οδηγίες
  σύνδεσης» button hands out links to `/mcp/setup` the same way.
- **Ανέβασμα υλικού** opens a form with a title and a file picker (PDF or JPG,
  up to 10 files). During an exam period the title starts as
  `Θέματα εξεταστικής <period> <year>`. The bot posts the files under the title,
  credits the member, pins the message and counts the files as theirs.

The sticky message is reposted when others post, debounced, and always as a
silent message (no push or desktop notifications). The bot checks Discord
kept the flag on every copy. If one comes back without it, the bot deletes
it, stops the stickies until the next restart and tells the moderators.

A file is listed only after it is backed up and approved by the screener
(`materials.Screener`). The current `BasicScreener` only checks that the content
really is a PDF or JPEG; a real screening pipeline plugs in by implementing that
interface and passing it to `materials.NewWorker` in `services/web/handler.go`. It may
return `pending` to decide asynchronously; rejected files stay in the bucket.

The bot needs Read Message History everywhere it should back up, and Manage
Messages (or Pin Messages) in course channels to pin uploads.

## IP challenges and bans

The bot counts requests per client address (an IPv6 address counts as its
whole /64). `/healthz`, successful material downloads and successful `/mcp`
calls are not counted, and neither are 404s for `/favicon.ico`, `/robots.txt`,
the Apple touch icons and `/.well-known/…`, which browsers and MCP clients ask
for on their own.

- **Challenge, 1 hour:** 3 `404` responses within an hour, or more than
  10 requests/s over 10 seconds.
- **Ban, 6 hours:** while challenged, 20 `404` responses within an hour, or
  10 requests/s or more held for over 20 minutes since the challenge began.

Every block is stored in the `ip_bans` table and restored on start-up. With
`RAILWAY_TOKEN` set, the bot writes them to the service's
[edge rules](https://docs.railway.com/networking/edge-rules), so Railway
challenges and blocks those addresses before their requests reach the bot.
It owns two rules, described `steki-shield: …` and placed first; it rewrites
them when the set changes and drops ended blocks within a minute. Any other
rule in the ruleset is kept, moved down only if its priority is 1 or 2. The
edge matches IPv4 only. Since Railway's challenge stops visitors who don't
clear it, only requests from visitors who did reach the bot to count towards a ban.

The bot also refuses banned addresses itself, so a ban holds without
`RAILWAY_TOKEN`, during the seconds before the edge is updated, and for IPv6.
Bans are posted silently to the log channel; a failed edge write is posted
loudly, at most once an hour, and retried every minute.

## Automatic Under Attack Mode

With `RAILWAY_TOKEN` set, the bot turns on Railway's Under Attack Mode for the
whole service for one hour when more than 50 requests/s, sustained over 10
seconds, come from addresses that are not already challenged or banned. A single
address is challenged long before it could get there alone, so this only fires
for floods spread over many addresses. It posts to the log channel when it
does, waits out the hour before it can fire again, and retries after 5 minutes
if Railway refuses. The Railway CLI is installed in the image for this.

## Running locally

```bash
cp .env.example .env   # fill in the values, with a local MinIO as the bucket
```

Set `BUCKET_PATH_STYLE=1` for MinIO and `DB_CREATE_EMPTY=1` to start with empty
databases. Run one service with `go run ./cmd/registration` (or `moderation`, `web`).
Every service's internal API uses port 8081, so run them one at a time or in
containers. Never point a local run at the production bucket: taking a shard over
fences the live service.

`go test ./...` needs nothing else. `STEKI_TEST_S3="endpoint region key secret bucket"`
also runs the engine against a real S3 server.

To read a shard's data outside a service, fetch its manifest and objects: an image
is a SQLite file, or take `daily/<date>.db` for a day's copy.

## Memory footprint

Three processes cost more than one: each holds a gateway connection and its own
state cache, under the same soft limits. The earlier 50 MB target for the whole
deployment no longer holds, which is the price of the separation.

- **A soft Go heap limit of 24 MiB and `GOGC=40`** per service (`bot.TuneRuntime`);
  `GOMEMLIMIT` and `GOGC` in the environment override them.
- **Each service subscribes only to the gateway intents it reads**, and only the
  discordgo state any of them reads is tracked: channels, roles and members.
- **A fixed ring of the last 1500 messages** (`services/moderation/msgcache.go`)
  backs the edit and delete logs, in the moderation service only.
- **The phishing feeds are streamed into a sorted slice of 64-bit hashes**, in the
  moderation service only; `web` asks it about upload titles.
- **Every in-memory table has a bound and a way out**: rate counters, replay sets and
  sessions are swept or capped, and PostHog's queue holds at most 1,000 events.

## How it stays stable

- **Category overflow** — Discord caps a category at 50 channels. When the
  register category fills, the bot provisions `register-overflow-N` categories
  automatically, so a join can never again fail with `CHANNEL_PARENT_MAX_CHANNELS`.
- **Abandonment reaper** — a registration idle for 1h gets one reminder *in its
  channel* (no DMs); idle for 6h, the member is kicked and the channel removed.
  Kicked members can rejoin and retry.
- **Reconcile on demand** — nothing repairs drift by itself, so a deploy or a
  reconnect never sets off a pass over the whole server. `/reg reconcile` diffs
  guild members against the database and channels, creating channels for joins
  that arrived while the bot was away (members who joined in the last 24h, at
  most 25 channels per run) and cleaning up orphans. Run it after the bot was
  down; `/bot stats` shows when it last ran.
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
- `/reg reconcile` — brings registrations and their channels back in line with
  who is on the server (see below)
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
- `/bot stats` — health, open registrations, when `/reg reconcile` last ran and
  whether any admin cards are waiting
- `/member ban|unban` · `/rules post` · `/mcp announce`

### Catch-up commands

These passes repair or redraw what is already in Discord. They touch many
members, channels or messages at once, so none of them runs at start-up, on a
reconnect or on a timer: a moderator runs them when needed, each can be run
again safely, and two cannot run at the same time.

| Command | When to run it |
|---------|----------------|
| `/reg reconcile` | After the bot was down or disconnected for a while, or when `/reg stuck` shows a member with no channel. Closes registrations of members who left or verified, makes missing register channels (also for recent joins the bot missed, at most 25 per run), deletes orphan and duplicate ones, refreshes the member index behind the ghost role |
| `/materials scan` | To pick up files posted before the bot, or while it was offline, and to place the stickies |
| `/materials stickies` | After a change to the sticky card: edits every existing sticky in place, silently |
| `/materials cards` | When `/bot stats` reports cards that did not reach the log channel (held images, reports) |
| `/materials rescan` | To send images uploaded before the content check existed through it |
| `/mcp refresh` | To give older MCP announcements the current button |

Every join, email attempt, link sent, retry, failure and role grant is recorded
in `registration_events`, which is what `/reg history` reads.
