# bot

The code that the three Discord services share. Each service embeds a `bot.Core` and adds
its own handlers, so behaviour that every service needs, such as panic recovery,
graceful shutdown and runtime tuning, exists once. See the
[architecture](../../docs/ARCHITECTURE.md#code-layout-and-who-may-import-what).

## Contents

- `core.go`: `Core` holds the session, the panic guard, the home guild, the moderators'
  log channel, and helpers for replies and lookups.
- `service.go`: runtime tuning (`TuneRuntime`: a 24 MiB soft heap limit, `GOGC=40`, two
  processors), the logger, opening of the bucket and of shards, the `Fence`, the
  staggered start of gateway connections, and graceful stop.
- `commands.go`: the slash-command definitions. The answers are in each service.
- `constants.go`: role IDs and time budgets (`DBTimeout`, `FlowTimeout`).
- `jobs.go`, `locks.go`, `throttle.go`: `Jobs` prevents an on-demand job from running
  twice at once, `KeyedMutex` provides one mutex per key and releases it when unused, and
  `RateGates` keeps sliding-window counters per key.
- `track.go`: analytics hooks, which do nothing when analytics is off.
- `helpers.go`: small Discord helpers.

## Rules

The package must not import any `services/*` package. A helper used by only one service
belongs in that service.
