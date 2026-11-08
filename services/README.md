# services

The logic of the three Discord services. Each service is a separate binary, wired in
`cmd/<name>/main.go`, so that the process boundary also serves as the privilege boundary
described in the [architecture](../docs/ARCHITECTURE.md#why-three-services-and-a-blastdoor).

| Package | Responsibility | Holds |
|---|---|---|
| [`registration`](registration/) | admission of members | mail API key, member addresses |
| [`moderation`](moderation/) | screening of messages | message cache, phishing list |
| [`web`](web/) | public pages, MCP, course material, address shield | token-signing key, OAuth, Gemini and Railway tokens |

All three open a gateway connection with the same bot token and ignore events that
concern another service. Each subscribes only to the intents it reads (`Intents` in
`handler.go`).

## Interface of a service

Every service exposes the same three operations, which are all that `main.go` needs:

```go
h, err := web.NewHandler(cfg, store, analytics, logger) // arguments differ per service
h.Register(session)                                      // gateway handlers
h.API(rpcServer, session)                                // internal API routes
```

Each package also provides `Routes()`, the slash subcommands it answers. The tests below
read it.

## Tests that enforce the architecture

The tests in `services/*_test.go` check properties that a reviewer would otherwise have
to verify by hand. Each failure message names the package or command at fault.

| Test | Property |
|---|---|
| `TestServicesStayInTheirLane` | a service links none of the packages it is forbidden to link |
| `TestSecretsStayWithTheirService` | a secret is read only by the config loader of its service |
| `TestEveryCommandHasOneOwner` | each subcommand is answered by exactly one service |
| `TestWatchPatternsCoverEveryDependency` | each `settings.toml` watches every package its service links, so a change redeploys it |
| `TestCommandDefinitionsFitDiscordLimits`, `TestEveryCommandIsStaffOnly`, `TestEveryTextOptionIsBounded` | command definitions are valid, staff-only, and bounded in size |

## Testing a handler

Each service has a stand-in for Discord (`fakediscord_test.go` or `helpers_test.go`) and
uses `store.OpenTest…` for a throwaway database. A new test follows the neighbouring
ones: build a handler, deliver an event, and assert on what the fake Discord received and
on what the store holds.
