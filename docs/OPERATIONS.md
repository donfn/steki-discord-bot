# Operations

How the bot is configured, deployed and kept healthy. For how the code is organised
see [ARCHITECTURE.md](ARCHITECTURE.md); for moderator commands see
[COMMANDS.md](COMMANDS.md).

- [Configuration](#configuration)
- [Deployment](#deployment)
- [Course material](#course-material)
- [IP challenges and bans](#ip-challenges-and-bans)
- [Automatic Under Attack Mode](#automatic-under-attack-mode)
- [Memory footprint](#memory-footprint)
- [How it stays stable](#how-it-stays-stable)

## Configuration

Each service reads only its own variables and refuses to start with a list of any that
are missing. [`.env.example`](../.env.example) is the annotated template, and
`internal/config/config.go` is the source of truth.

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
`internal/config/config.go` and `internal/bot/constants.go`. The guild ID is learned from the gateway.

Adding a variable? Put it only in the loader of the service that needs it. If it is a
secret, add it to the `onlyIn` map in `TestSecretsStayWithTheirService`
(`services/scope_test.go`), and to `.env.example` and the table above.

## Deployment

The bot runs on [Railway](https://railway.com), one service per binary, all in one
project and environment, sharing one S3-compatible bucket.

- Each service's build and deploy settings are recorded in
  `cmd/<service>/settings.toml` and set on the service in Railway, because Railway no
  longer reads config files. Keep them in step by hand.
- The `watchPatterns` in each file make a change redeploy only the services built from
  it. `TestWatchPatternsCoverEveryDependency` fails if you add a package a service
  links without adding it to that list.
- The shared packages live in `internal/`, so the watch patterns name
  `internal/<package>/**`. The patterns set on the services in Railway must be changed
  to match these files whenever the files change.
- `web` builds from the root `Dockerfile` (it also installs the pinned
  Railway CLI for Under Attack Mode); `registration` and `moderation` from
  `cmd/<service>/Dockerfile`; `blastdoor` from `blastdoor/Dockerfile`.
- A service stops cleanly when a newer deployment takes its databases over
  (see the *fence* in [ARCHITECTURE.md](ARCHITECTURE.md#process-lifecycle)), so
  `restartPolicyType` is `ON_FAILURE`, never `ALWAYS`.
- Health check: `GET /healthz` on every service.
- `blastdoor` is rebuilt weekly by `.github/workflows/rebuild-blastdoor.yml`, which
  commits a new `blastdoor/rebuild-stamp` so Railway re-runs the package install and
  picks up security fixes. No other service watches that file.
- Railway's edge rule 404s any path the bot does not serve. **If you add, rename or
  remove a public HTTP route, update the edge rule's allowlist too.**
- On Railway the `web` service holds the original variables and the others reference
  them, for example `DISCORD_TOKEN=${{web.DISCORD_TOKEN}}`.

## Course material

Every PDF and JPEG posted in a course channel or one of its threads is copied to the
`steki-backups` bucket, under a folder per channel name
(`<channel>/<thread>/<attachment id>-<file name>`). New posts are picked up live. The
history before the bot, and anything posted while it was offline, is found by
`/materials scan`, which walks every course channel and thread back to its first
message and resumes across restarts; it never runs by itself. Deleting a message hides
its files from the material page but never removes them from the bucket.

Each channel in the course categories (`courseCategoryIDs` in
`services/web/materials.go`) keeps a bot message at its bottom saying how many files
members have shared, with two buttons:

- **Προβολή υλικού μαθήματος** answers, to that member alone, with one short line and
  an «Άνοιγμα χώρου μελέτης» button. The button holds a personal link to `/materials`,
  valid for 30 seconds and good for one visit. The reply counts down with a bar and is
  deleted when the link is opened or runs out. After a change to the card,
  `/materials stickies` edits every existing sticky in place. The page lists every file
  of the channel and its threads, searchable, with PDFs (pdf.js) and images viewed in
  the page. It downloads every file as soon as it opens. Studied marks, the member's own
  titles, notes and ink are saved per member (`study_notes`) and come back on any
  device; the latest save wins. The page sends them over a WebSocket on `/s`, opened
  with a one-time token that lasts 30 minutes; the server sends a fresh one every 5
  minutes so the page can reconnect after a drop. If it cannot (the token ran out), the
  page turns read-only. The MCP announcement's «Οδηγίες σύνδεσης» button hands out
  links to `/mcp/setup` the same way.
- **Ανέβασμα υλικού** opens a form with a title and a file picker (PDF or JPG, up to 10
  files). During an exam period the title starts as
  `Θέματα εξεταστικής <period> <year>`. The bot posts the files under the title, credits
  the member, pins the message and counts the files as theirs.

The sticky message is reposted when others post, debounced, and always as a silent
message (no push or desktop notifications). The bot checks Discord kept the flag on
every copy. If one comes back without it, the bot deletes it, stops the stickies until
the next restart and tells the moderators.

A file is listed only after it is backed up and approved by the screener
(`materials.Screener`). Format is checked for every file; JPEGs also go through a
Gemini content check when `GOOGLE_AI_TOKEN` is set, and held images arrive as admin
review cards in the log channel. A screener may return `pending` to decide
asynchronously; rejected files stay in the bucket. Compressed objects are written and
read through `blastdoor`.

The bot needs Read Message History everywhere it should back up, and Manage Messages
(or Pin Messages) in course channels to pin uploads.

## IP challenges and bans

The bot counts requests per client address (an IPv6 address counts as its whole /64).
`/healthz`, successful material downloads and successful `/mcp` calls are not counted,
and neither are 404s for `/favicon.ico`, `/robots.txt`, the Apple touch icons and
`/.well-known/…`, which browsers and MCP clients ask for on their own.

- **Challenge, 1 hour:** 3 `404` responses within an hour, or more than 10 requests/s
  over 10 seconds.
- **Ban, 6 hours:** while challenged, 20 `404` responses within an hour, or 10
  requests/s or more held for over 20 minutes since the challenge began.

Every block is stored in the `ip_bans` table and restored on start-up. With
`RAILWAY_TOKEN` set, the bot writes them to the service's
[edge rules](https://docs.railway.com/networking/edge-rules), so Railway challenges and
blocks those addresses before their requests reach the bot. It owns two rules,
described `steki-shield: …` and placed first; it rewrites them when the set changes and
drops ended blocks within a minute. Any other rule in the ruleset is kept, moved down
only if its priority is 1 or 2. The edge matches IPv4 only. Since Railway's challenge
stops visitors who don't clear it, only requests from visitors who did reach the bot
count towards a ban.

The bot also refuses banned addresses itself, so a ban holds without `RAILWAY_TOKEN`,
during the seconds before the edge is updated, and for IPv6. Bans are posted silently
to the log channel; a failed edge write is posted loudly, at most once an hour, and
retried every minute.

## Automatic Under Attack Mode

With `RAILWAY_TOKEN` set, the bot turns on Railway's Under Attack Mode for the whole
service for one hour when more than 50 requests/s, sustained over 10 seconds, come from
addresses that are not already challenged or banned. A single address is challenged
long before it could get there alone, so this only fires for floods spread over many
addresses. It posts to the log channel when it does, waits out the hour before it can
fire again, and retries after 5 minutes if Railway refuses. The Railway CLI is
installed in the `web` image for this.

## Memory footprint

Three processes cost more than one: each holds a gateway connection and its own state
cache, under the same soft limits. The earlier 50 MB target for the whole deployment no
longer holds, which is the price of the separation.

- **A soft Go heap limit of 24 MiB and `GOGC=40`** per service (`bot.TuneRuntime`);
  `GOMEMLIMIT` and `GOGC` in the environment override them.
- **Each service subscribes only to the gateway intents it reads**, and only the
  discordgo state any of them reads is tracked: channels, roles and members.
- **A fixed ring of the last 1500 messages** (`services/moderation/msgcache.go`) backs
  the edit and delete logs, in the moderation service only.
- **The phishing feeds are streamed into a sorted slice of 64-bit hashes**, in the
  moderation service only; `web` asks it about upload titles.
- **Every in-memory table has a bound and a way out**: rate counters, replay sets and
  sessions are swept or capped, and PostHog's queue holds at most 1,000 events.

When you add a cache, give it a size bound and an eviction path.

## How it stays stable

- **Category overflow:** Discord caps a category at 50 channels. When the register
  category fills, the bot provisions `register-overflow-N` categories automatically, so
  a join can never again fail with `CHANNEL_PARENT_MAX_CHANNELS`.
- **Abandonment reaper:** a registration idle for 1 h gets one reminder *in its channel*
  (no DMs); idle for 6 h, the member is kicked and the channel removed. Kicked members
  can rejoin and retry.
- **Reconcile on demand:** nothing repairs drift by itself, so a deploy or a reconnect
  never sets off a pass over the whole server. `/reg reconcile` diffs guild members
  against the database and channels, creating channels for joins that arrived while the
  bot was away (members who joined in the last 24 h, at most 25 channels per run) and
  cleaning up orphans. Run it after the bot was down; `/bot stats` shows when it last
  ran.
- **No races:** all registration work for one member is serialised, and channel
  placement is serialised guild-wide, so concurrent events can never produce two
  channels for a member or overfill a category.
- **Never kick the verified:** before a kick, the reaper re-checks the row and the
  member's roles; a member the database records as verified but who lost their role gets
  the role back instead.
- **Retries and alerts:** Discord calls retry with backoff on 429/5xx; anything that
  still fails (a channel that cannot be created, an email that cannot be sent) is posted
  to the moderator log channel rather than lost to stdout.
- **Time budgets:** every database and Discord call from a handler runs under a timeout
  from `internal/bot/constants.go` (`DBTimeout`, `FlowTimeout`, …), so nothing wedges.
- **Databases never start from less than the whole:** see
  [`internal/s3db/README.md`](../internal/s3db/README.md#how-a-write-becomes-durable).
