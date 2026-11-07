# bot

What the three Discord services share. Each service embeds a `bot.Core` and adds only
its own handlers.

Part of the [architecture](../docs/ARCHITECTURE.md#code-layout-and-who-may-import-what).

| File | Holds |
|---|---|
| `core.go` | `Core`: the session, panic guard, home guild, moderators' log channel, reply and lookup helpers |
| `service.go` | `TuneRuntime` (24 MiB soft heap, `GOGC=40`, 2 procs), logger, bucket and shard opening, the `Fence`, staggered gateway start, graceful stop |
| `commands.go` | the slash-command definitions (the *answers* live in each service) |
| `constants.go` | role IDs, time budgets (`DBTimeout`, `FlowTimeout`, …) |
| `jobs.go` | `Jobs`: keeps an on-demand job from running twice at once |
| `locks.go` | `KeyedMutex`: one mutex per key, dropped when unused |
| `throttle.go` | `RateGates`: sliding-window counters per key |
| `track.go` | analytics hooks (no-ops when analytics is off) |
| `helpers.go` | small Discord helpers, e.g. the member ID in a `register-<id>` channel name |

Rules for this package: it must not import a `services/*` package, and a helper that only
one service uses belongs in that service.
