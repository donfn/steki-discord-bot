# services

The three Discord services. Each is a package of event handlers, slash-command handlers
and an internal API (`rpc`); the `main.go` that wires them up is in `cmd/<name>/`.

| Package | Does | Holds | README |
|---|---|---|---|
| `registration` | register channels, the email link, the reaper, `/reg`, `/email` | Resend key, members' addresses | [registration](registration/) |
| `moderation` | screens messages, media guard, ghost role, edit/delete logs, `/guard`, `/member`, `/rules`, `/bot` | message cache, phishing list | [moderation](moderation/) |
| `web` | pages, MCP server, course material, IP shield, `/materials`, `/mcp` | `JWT_SECRET`, OAuth, Gemini and Railway tokens | [web](web/) |

All three open a gateway connection with the same bot token and ignore events that are
not theirs. Each subscribes only to the intents it reads (`Intents` in `handler.go`).

## Shape of a service

Every service exposes the same surface, which is all `cmd/<name>/main.go` needs:

```go
h, err := web.NewHandler(cfg, store, analytics, logger) // build (arguments differ per service)
h.Register(session)                                      // gateway handlers
h.API(rpcServer, session)                                // internal API routes
```

plus a package-level `Routes()`, the list of slash subcommands it answers, which
`TestEveryCommandHasOneOwner` checks.

## Cross-cutting tests (`services/*_test.go`)

| Test | Keeps true |
|---|---|
| `TestServicesStayInTheirLane` | a service links none of the packages it is forbidden to |
| `TestSecretsStayWithTheirService` | a secret is read only by its service's config loader |
| `TestEveryCommandHasOneOwner` | each subcommand is answered by exactly one service |
| `TestWatchPatternsCoverEveryDependency` | each service's `settings.toml` watches everything it links |
| `TestCommandDefinitionsFitDiscordLimits`, `TestEveryCommandIsStaffOnly` | command definitions are valid and staff-only |

If one of these fails, the message names the package or command to fix. The
[architecture doc](../docs/ARCHITECTURE.md#guardrail-tests) explains why they exist.

## Testing a handler

Each service has a `fakediscord_test.go` (registration, web) or helper
(`helpers_test.go`) that stands in for Discord, and uses `store.OpenTest…` for a
throwaway database. Copy a neighbouring test: build a handler, feed it an event, assert
on what the fake Discord received and on what the store holds.
