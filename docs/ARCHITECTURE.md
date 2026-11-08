# Architecture

How the Steki bot is put together, and why. Diagrams are [Mermaid](https://mermaid.js.org/),
which GitHub renders in place. If you change behaviour a diagram shows, change the
diagram in the same pull request.

- [The big picture](#the-big-picture)
- [Why three services and a blastdoor](#why-three-services-and-a-blastdoor)
- [Code layout and who may import what](#code-layout-and-who-may-import-what)
- [Service-to-service calls](#service-to-service-calls)
- [Data: SQLite shards in a bucket](#data-sqlite-shards-in-a-bucket)
- [Flows](#flows)
  - [Registration](#registration)
  - [Message screening](#message-screening)
  - [Course material: backup, screening, viewing](#course-material-backup-screening-viewing)
  - [MCP server](#mcp-server)
  - [IP shield](#ip-shield)
- [Process lifecycle](#process-lifecycle)
- [Secrets and trust boundaries](#secrets-and-trust-boundaries)

## The big picture

Discord sees a single bot. Behind it are three Go services that each open their own
gateway connection with the same token and ignore events that are not theirs, plus
`blastdoor`, a secret-free helper that handles hostile files.

```mermaid
flowchart TB
    GW["Discord<br/>gateway and REST API"]
    BR["Members' browsers"]
    AI["AI assistants<br/>(MCP clients)"]

    subgraph Railway["Railway private network"]
        direction LR
        REG["registration<br/>cmd/registration"]
        MOD["moderation<br/>cmd/moderation"]
        WEB["web<br/>cmd/web"]
    end

    Resend["Resend<br/>email API"]
    DB[("S3 bucket<br/>db/ shards, material/, usernotes/")]
    BD["blastdoor<br/>blastdoor/cmd<br/>no secrets"]
    Gemini["Google Gemini<br/>image check"]
    RWAPI["Railway API<br/>edge rules"]

    GW <-->|"one gateway connection per service"| Railway
    BR -->|HTTPS| WEB
    AI -->|"HTTPS /mcp"| WEB
    Railway -->|"each service uses its own shards"| DB
    REG --> Resend
    WEB -->|"plain HTTP"| BD
    WEB --> Gemini
    WEB --> RWAPI
```

The three services call one another over signed RPC on port 8081, drawn in
[Service-to-service calls](#service-to-service-calls). All three also send anonymous
counts to PostHog, which is omitted here.

| Service | Role | Owns |
|---|---|---|
| [`registration`](../services/registration/) | register channels, email link, reaper, `/reg`, `/email` | `registry` shard, Resend key, member addresses |
| [`moderation`](../services/moderation/) | screens messages, media guard, ghost role, edit/delete logs, installs slash commands | `guard` and `activity` shards, message cache, phishing list |
| [`web`](../services/web/) | public pages, MCP server, course material, IP shield | `library` shard, `JWT_SECRET`, OAuth, Gemini and Railway tokens |
| [`blastdoor`](../blastdoor/) | converts images, compresses files, in a sandbox | nothing |

## Why three services and a blastdoor

The split is a security boundary first and a memory trade-off second.

- **Least privilege.** The service that reads every message (`moderation`) holds no
  mail key and no member's address. The service that faces the internet (`web`) holds
  no mail key either and cannot link the other services' logic. The service that holds
  the addresses (`registration`) serves no web pages and handles no uploaded files.
  A bug in one gives an attacker only what that service holds.
- **Hostile input is isolated.** Strangers' images are parsed by libheif, and files are
  compressed with zstd, inside `blastdoor`: a container with no secrets and no bucket
  credentials, running under a seccomp sandbox that forbids creating sockets. Treat it
  as already compromised.
- **The cost.** Each service keeps a gateway connection and a state cache, so three
  processes use more memory than one. Each is held under a soft 24 MiB Go heap
  (`bot.TuneRuntime`).

These rules are enforced by tests, not by convention. See
[Guardrail tests](#guardrail-tests).

## Code layout and who may import what

```mermaid
flowchart TB
    subgraph cmds["Entry points (cmd/)"]
        direction LR
        cR[registration]
        cM[moderation]
        cW[web]
        cB[blastdoor]
    end

    subgraph svcs["Service logic (services/)"]
        direction LR
        sR[registration]
        sM[moderation]
        sW[web]
    end

    mail["mail<br/>linked by registration only"]
    webpk["verify, mcp, materials, usernotes, shield<br/>linked by web only"]
    shared["Shared by all three services<br/>bot, config, dc, analytics, moderation,<br/>rpc, tokens, store, s3db, brand"]

    cR --> sR
    cM --> sM
    cW --> sW
    sR --> mail
    sW --> webpk
    svcs --> shared
```

Within the shared group, `bot` builds on `config`, `store` and `s3db`; `rpc` builds on
`tokens` and `store`; `store` builds on `s3db`. `mail` and `verify` use `brand`.

Directory guide:

| Path | What it is |
|---|---|
| `cmd/<service>/` | `main.go`, `Dockerfile` and `settings.toml` for one service |
| `services/<service>/` | event handlers, slash commands and RPC handlers of that service |
| `internal/bot/` | what every Discord service shares: `Core`, gateway session, panic guard, graceful shutdown, command definitions, runtime tuning |
| `internal/config/` | per-service environment loaders, plus the guild's channel, category and role IDs as constants |
| `internal/store/` | the persistence layer, one Go type per shard, plus embedded SQL migrations |
| `internal/s3db/` | the engine that keeps a SQLite file durable in a bucket |
| `internal/rpc/` | signed service-to-service JSON calls |
| `internal/tokens/` | signed links (email, captcha, material, MCP guide) and the ALTCHA captcha |
| `internal/verify/` | the public HTTP pages and middleware (`web` only) |
| `internal/mcp/` | MCP server and its OAuth 2.1 authorization server (`web` only) |
| `internal/materials/` | course-file backup worker, screening and compression (`web` only) |
| `internal/shield/` | per-address abuse detection and Railway edge rules (`web` only) |
| `blastdoor/` | the sandboxed image and compression service |
| `internal/mail/` | the registration email (`registration` only) |
| `internal/moderation/` | the mod log channel and the blacklisted-link filter, shared helpers |
| `internal/usernotes/` | per-member study notes, one SQLite object per member |
| `internal/dc/` | bounded retries, error classification and register-channel placement |
| `internal/analytics/` | anonymous PostHog events |
| `internal/brand/` | logos and university names |

### Guardrail tests

Four tests in [`services/`](../services/) keep the architecture honest. Run them before
you open a pull request; each fails with a message naming the offender.

| Test | Fails when |
|---|---|
| `TestServicesStayInTheirLane` | a service's binary links a package it is forbidden to (for example `moderation` linking `mail`) |
| `TestSecretsStayWithTheirService` | a secret variable appears in the wrong service's config loader |
| `TestEveryCommandHasOneOwner` | a slash command is answered by zero or by two services |
| `TestWatchPatternsCoverEveryDependency` | a package a service links is missing from its `settings.toml` watch patterns, so a change would not redeploy it |

## Service-to-service calls

Nothing shares a database. A service reaches another's data only through its internal
API in [`internal/rpc/`](../internal/rpc/): JSON over HTTP on port 8081, which has no public route.
Requests carry an HMAC signature and timestamp derived from `INTERNAL_SECRET` and are
refused after one minute.

```mermaid
flowchart TB
    MOD[moderation]
    REG[registration]
    WEB[web]

    WEB -->|"link/pending, link/accept"| REG
    MOD -->|"emails/ban, emails/unban, stats"| REG
    REG -->|"forget, events, roster/sync"| MOD
    WEB -->|"guard/active, guard/lift, screen-title"| MOD
    MOD -->|"materials/drop-message, stats"| WEB
    REG -->|"forget, events"| WEB
```

Each arrow points from the caller to the service that answers. The purpose of each
route is in the [route table](../internal/rpc/README.md#routes): `link/*` serves the email consent
page, `emails/*` serves `/member ban`, `forget` and `events` serve `/reg forget` and
`/reg history`, `guard/*` serves the captcha page, and `drop-message` hides the post of a
restricted author.

Rules of thumb:

- Add a route as a constant in `internal/rpc/rpc.go`, register it with `rpc.Handle` in the
  owner's `API` method, and call it from the caller. Typed request and response
  structs keep both ends in step.
- The owner decides; the caller asks. `moderation` never writes the address table, it
  asks `registration` to ban an email.
- `blastdoor` is **not** an RPC peer. It has no secret and is called over plain HTTP;
  `web` must assume its answers could be forged and check them.

## Data: SQLite shards in a bucket

There is no database server and no volume. Each shard is a SQLite file on the
container's scratch disk with exactly one writing process; the S3 bucket holds the
truth. Details are in [`internal/s3db/README.md`](../internal/s3db/README.md).

```mermaid
flowchart TB
    subgraph Container
        APP[service code] --> STORE["store.Registry / Guard /<br/>Activity / Library"]
        STORE --> DRV["s3db driver wrapper<br/>holds each write's result"]
        DRV --> SQLITE[(SQLite file<br/>+ WAL on scratch disk)]
        SHIP[shipper goroutine]
        SQLITE -. committed WAL frames .-> SHIP
    end

    subgraph Bucket
        MANI["db/&lt;shard&gt;/manifest.json<br/>(commit point, epoch, etag)"]
        SNAP["snap/… whole images"]
        SEG["seg/… changed pages"]
        DAILY["daily/… one a day, 2 weeks"]
    end

    SHIP -->|"(1) upload segment"| SEG
    SHIP -->|"(2) conditional write"| MANI
    SHIP -->|"(3) release waiting writers"| DRV
    MANI --- SNAP & SEG & DAILY
```

| Shard | Written by | Holds | A write returns |
|---|---|---|---|
| `registry` | registration | addresses, members, registrations, bans | once in the bucket |
| `guard` | moderation | media guard | once in the bucket |
| `activity` | moderation | when members last spoke | at once, shipped every 30 s |
| `library` | web | materials, reports, stickies, IP blocks, study notes | once in the bucket |

Each shard type is a distinct Go type, so a service that holds one cannot even compile
a query against another's tables.

## Flows

### Registration

A new member proves they own a university address. The email carries a link, not a
code; the member opens it, sees their Discord avatar beside the address, and accepts
behind an invisible ALTCHA proof of work.

```mermaid
sequenceDiagram
    autonumber
    actor M as Member
    participant D as Discord
    participant R as registration
    participant W as web
    participant E as Resend

    M->>D: joins the server
    D->>R: GuildMemberAdd
    R->>D: create a private register-ID channel
    M->>D: presses the card's button, fills in the email form
    R->>R: classify the address, check it is free (registry shard)
    R->>E: send email with signed consent link
    E-->>M: email
    M->>W: opens /link?token=…
    W->>R: RPC link/pending (who, which address?)
    R-->>W: avatar, address, university
    W-->>M: consent page + ALTCHA challenge
    M->>W: accepts (proof of work solved)
    W->>R: RPC link/accept
    R->>R: bind address to member (registry shard)
    R->>D: grant verified role, remove register channel
```

If the member goes quiet, the **reaper** sends one reminder after 1 h and kicks after
6 h (never a verified member). The full list of stability behaviours is in
[`OPERATIONS.md`](OPERATIONS.md#how-it-stays-stable).

### Message screening

`moderation` and `web` each see every message at the same moment. Screening is not a
step before recording.

```mermaid
flowchart TD
    MSG[MessageCreate event] --> MODQ{{moderation}}
    MSG --> WEBQ{{web}}

    MODQ --> CACHE["store in the 1500-message ring<br/>(for edit and delete logs)"]
    CACHE --> LINK{"link on the blacklist<br/>or phishing feed?"}
    LINK -- yes --> DEL["delete message,<br/>time out sender, log it"]
    LINK -- no --> INV{"invite to another<br/>server?"}
    INV -- yes --> DEL
    INV -- no --> DUP{"same text pasted<br/>across channels?"}
    DUP -- yes --> DEL
    DUP -- no --> MEDIA{"media guard: should this<br/>author's media be held?"}
    MEDIA -- restrict --> RESTRICT["restrict author,<br/>DM captcha link"]
    RESTRICT -. "RPC materials/drop-message" .-> WEBQ
    MEDIA -- ok --> OK[nothing to do]

    WEBQ --> ATT{"PDF or JPEG in a<br/>course channel?"}
    ATT -- yes --> MATQ[queue for backup]
    ATT -- no --> STICKY[maybe repost the sticky]
    DEL -. "delete event hides the files" .-> MATQ
```

### Course material: backup, screening, viewing

```mermaid
flowchart TD
    POST["PDF / JPEG posted in a course channel<br/>(or uploaded through the form)"] --> ROW[row in library.materials]
    ROW --> WORKER[materials.Worker]
    WORKER --> COPY["download from Discord,<br/>copy to bucket material/…"]
    COPY --> SCREEN{materials.Screener}
    SCREEN --> FMT["format check:<br/>really a PDF or JPEG?"]
    SCREEN --> GEM["JPEGs: Gemini content check<br/>(held images get an admin review card)"]
    FMT --> VERDICT{approved?}
    GEM --> VERDICT
    VERDICT -- yes --> LIST[listed on /materials]
    VERDICT -- no --> STAY[stays in bucket, not listed]
    COPY -.->|"HEIC conversion,<br/>zstd compression"| BD[blastdoor]

    LIST --> BTN["Sticky button → personal link<br/>valid 30 s, one visit"]
    BTN --> PAGE["/materials page<br/>pdf.js, notes, ink, studied marks"]
    PAGE <-->|"WebSocket /s,<br/>one-time token"| NOTES[("usernotes/ object<br/>per member")]
    LIST --> MCPQ[also served to MCP clients]
```

Deleting a Discord message hides its files from the page; it never removes them from
the bucket. Only a moderator's `/materials remove` does that.

### MCP server

`web` serves course material to AI assistants over MCP (Streamable HTTP, stateless)
behind its own OAuth 2.1 authorization server that logs members in with Discord.

```mermaid
sequenceDiagram
    autonumber
    participant C as MCP client
    participant W as web (/mcp, /oauth/*)
    participant D as Discord OAuth
    participant L as library shard + Discord roles

    C->>W: POST /mcp (no token)
    W-->>C: 401 + /.well-known/oauth-protected-resource
    C->>W: POST /oauth/register (dynamic client)
    C->>W: GET /oauth/authorize (PKCE)
    W->>D: redirect to Discord login
    D-->>W: GET /oauth/callback (code)
    W->>L: is this user a verified member?
    W-->>C: consent page, then authorization code
    C->>W: POST /oauth/token
    W-->>C: JWT signed with JWT_SECRET
    C->>W: POST /mcp tools/call (courses, index, download)
    W->>L: re-check the member's current standing on every call
    W-->>C: result
```

The endpoint is read-only by construction: the only tools list the course channels,
read a channel's index and download a file.

### IP shield

```mermaid
stateDiagram-v2
    [*] --> Normal
    Normal --> Challenged: 3 x 404 in 1 h, or over 10 req/s for 10 s
    Challenged --> Banned: 20 x 404 in 1 h, or 10 req/s held for 20 min
    Challenged --> Normal: 1 h passes
    Banned --> Normal: 6 h pass
    Normal --> UnderAttack: over 50 req/s for 10 s from unchallenged addresses
    UnderAttack --> Normal: 1 h passes
```

Blocks are stored in `library.ip_bans`, restored on start-up, and (with
`RAILWAY_TOKEN`) pushed to Railway edge rules so the edge stops them first. The bot
also refuses banned addresses itself.

## Process lifecycle

Every service follows the same shape in `main.go`:

```mermaid
flowchart TB
    A[bot.TuneRuntime] --> B["config.Load…<br/>fail on missing vars"]
    B --> C["bot.OpenBucket<br/>prove stale writes are refused"]
    C --> D["store.Open… shard<br/>download image, replay segments"]
    D --> E[NewHandler + rpc server + HTTP]
    E --> F["bot.Stagger:<br/>services identify in turn"]
    F --> G["sess.Open<br/>(gateway)"]
    G --> H["run until SIGTERM<br/>or the Fence trips"]
    H --> I["close gateway, stop handlers,<br/>drain HTTP, close shard (ships the rest)"]
```

The **fence** is how a deploy hands a shard over safely: a starting service raises the
manifest's epoch; the old deployment's next write is refused, it shuts itself down, and
Railway is told not to restart it into its successor's way
(`restartPolicyType = "ON_FAILURE"`).

## Secrets and trust boundaries

```mermaid
flowchart LR
    subgraph input["Untrusted input"]
        I["Internet, uploaded files,<br/>message content"]
    end

    subgraph trusted["Trusted: holds secrets"]
        R["registration<br/>RESEND_API_KEY<br/>INTERNAL_SECRET"]
        M["moderation<br/>INTERNAL_SECRET<br/>URL_BLACKLIST"]
        W["web<br/>JWT_SECRET, OAuth secret<br/>GOOGLE_AI_TOKEN, RAILWAY_TOKEN<br/>INTERNAL_SECRET"]
    end

    subgraph untrusted["Assume compromised"]
        B["blastdoor<br/>PORT only"]
    end

    I --> M
    I --> W
    W -->|bytes in, bytes out| B
```

- Email and captcha links are signed with a key derived from `INTERNAL_SECRET`, because
  `registration` and `moderation` issue them.
- Everything only `web` issues (material links, sockets, MCP tokens) is signed with
  `JWT_SECRET`, so a compromised `registration` or `moderation` cannot forge them.
- Known limit: the bucket's credentials reach every object, so a compromised service
  could read other shards directly. Railway has no per-prefix credentials; separate
  buckets would close this.
