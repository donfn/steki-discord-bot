# Contributing

Thanks for helping with the Steki bot. This guide gets you from clone to pull request.
Read [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) first if you want the picture of how
the pieces fit; you do not need it to fix a typo.

## Quick start

You need **Go 1.26** (see `go.mod`). Nothing else is needed to build and test.

```bash
git clone https://github.com/donfn/steki-discord-bot.git
cd steki-discord-bot
go build ./...
go vet ./...
go test ./...
```

`go test ./...` needs no Discord token, bucket or network: store tests open a throwaway
local SQLite file (`store/testing.go`), and handler tests use fake Discord sessions
(`services/*/fakediscord_test.go`). Two optional extras:

- `STEKI_TEST_S3="endpoint region key secret bucket" go test ./s3db` also runs the
  database engine against a real S3 server.
- `blastdoor`'s sandbox test only runs on Linux.

## Running a service locally

Most changes do not need this. When one does:

1. Create a **test** Discord application and bot, and a **test** server. Never use the
   production token or bucket: starting against the production bucket takes a shard
   over and fences the live service.
2. Start a local [MinIO](https://min.io) as the bucket.
3. `cp .env.example .env` and fill it in. Set `BUCKET_PATH_STYLE=1` for MinIO and
   `DB_CREATE_EMPTY=1` to start with empty databases.
4. Run one service: `go run ./cmd/registration` (or `moderation`, `web`).
   Every service's internal API listens on port 8081, so run them one at a time or in
   containers. `go run ./blastdoor/cmd` runs the helper (Linux only for the sandbox).

Channel, category and role IDs are constants for the real guild
(`config/config.go`, `bot/constants.go`); for a test server you will edit those locally.
Do not commit that edit.

## Where does my change go?

| I want to… | Look in |
|---|---|
| change what happens when someone joins or registers | `services/registration/` |
| change message filtering, the media guard, edit/delete logs | `services/moderation/` |
| change a public page | `verify/` (HTML is embedded next to the Go) |
| change course material, uploads, stickies | `services/web/`, `materials/` |
| change the MCP server | `mcp/` |
| add or change a slash command | `bot/commands.go` + the owning service, see [COMMANDS.md](docs/COMMANDS.md#adding-a-command) |
| add a service-to-service call | `rpc/rpc.go` + both services, see [ARCHITECTURE.md](docs/ARCHITECTURE.md#service-to-service-calls) |
| add a table or column | `store/migrations/<shard>/NNNN_name.sql` + `store/` |
| add an environment variable | `config/config.go`, `.env.example`, [OPERATIONS.md](docs/OPERATIONS.md#configuration) |

## The rules that are tested

These are architecture rules, and tests fail when you break them. They are there on
purpose; do not weaken a test to get green. Ask in the pull request if you think a rule
is wrong.

- **A service only links what it may.** `registration`, `moderation` and `web` have
  forbidden packages (`TestServicesStayInTheirLane`). Do not import `mail` from `web`,
  or `verify` from `moderation`.
- **Secrets stay with their service** (`TestSecretsStayWithTheirService`).
- **Every slash command has exactly one owner** (`TestEveryCommandHasOneOwner`).
- **Watch patterns are complete** (`TestWatchPatternsCoverEveryDependency`). If you
  add a package that a service links, add `"<package>/**"` to
  `cmd/<service>/settings.toml`.
- **SQL is safe.** `store/sqlsafety_test.go` checks queries; use `?` placeholders,
  never build SQL from strings.
- **Nothing shares a database.** Reach another service's data through `rpc/`.

## Code conventions

- Standard Go: `gofmt`, `go vet`. No other linter is required.
- Standard library first. Add a dependency only when it clearly pays for itself, and
  say why in the pull request. The binaries are small on purpose.
- Every exported `store` method takes a `context.Context`; there is no ORM. Timestamps
  are integer Unix milliseconds.
- Every Discord or database call from a handler runs under a timeout
  (`bot.DBTimeout`, `bot.FlowTimeout`).
- Every in-memory table has a size bound and a way out. Memory is tight: a 24 MiB soft
  heap limit per service.
- Anything that fails for a moderator-relevant reason is reported to the log channel,
  not only logged.
- Never log or send to analytics a user ID, username, email, IP address or message
  text. `analytics.Capture` drops anything that is not a number, a flag or a known label.
- Comments explain *why*. The existing code is the style guide; keep package doc
  comments accurate when you change what a package does.
- User-facing strings are in Greek, matching the server. Code, logs and comments are
  in English.

## Tests

Add or update tests with your change. Handler tests use the fake Discord sessions in
`services/*/fakediscord_test.go`; store tests use `store.OpenTest…`. Look at a
neighbouring `_test.go` for the pattern. Prefer testing behaviour (what Discord sees,
what the database holds) over internals.

## Pull requests

1. Branch from `main`; keep the change focused, one concern per pull request.
2. `go build ./... && go vet ./... && go test ./...` passes.
3. Update the docs that describe what you changed: the package README, the diagrams in
   `docs/ARCHITECTURE.md`, `docs/COMMANDS.md`, `.env.example`.
4. Describe *what* and *why* in the pull request; the template lists what reviewers
   look for.
5. Commit messages: a short imperative subject (`Fix reaper kicking a verified member`),
   then a body if the reason is not obvious.

Deployment is done by the maintainers; merging to `main` is what Railway builds from.
Changes to a public HTTP route also need the Railway edge-rule allowlist updated, so
mention it in the pull request ([OPERATIONS.md](docs/OPERATIONS.md#deployment)).

## Security

Do not open a public issue for a vulnerability. See [SECURITY.md](SECURITY.md).

## Licence

By contributing you agree your work is released under the [MIT licence](LICENSE).
