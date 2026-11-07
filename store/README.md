# store

The persistence layer: SQLite databases kept in the bucket by [`s3db`](../s3db/), one
per concern, each written by **a single service**. No ORM, no query builder.

Part of the [architecture](../docs/ARCHITECTURE.md#data-sqlite-shards-in-a-bucket).

## Shards

Each shard has its own Go type, so a service holding one cannot even compile a query
against another's tables. Across services, data moves only through [`rpc/`](../rpc/).

| Type | Opened by | Owner | Holds |
|---|---|---|---|
| `store.Registry` | `OpenRegistry` | registration | addresses, members, registrations, events, bans |
| `store.Guard` | `OpenGuard` | moderation | media guard state |
| `store.Activity` | `OpenActivity` | moderation | when members last spoke |
| `store.Library` | `OpenLibrary` | web | materials, reports, stickies, study notes, IP bans, events |

## Conventions

- Every exported method takes a `context.Context` and runs a single statement or an
  explicit transaction.
- Timestamps are integer Unix milliseconds.
- SQL uses `?` placeholders only. `sqlsafety_test.go` scans for string-built queries.
- Domain types live in `models.go`.
- Never store a Discord ID, email or username in a table the analytics events draw on;
  analytics receives counts and labels only.

## Migrations

SQL files are embedded and applied in order on open:

```
migrations/registry/0001_init.sql
migrations/guard/0001_init.sql
migrations/activity/0001_init.sql
migrations/library/0001_materials.sql … 0006_events.sql
```

To change a schema, **add a new numbered file**; never edit one that has shipped.
Migrations run against a live database that members depend on, so they must be safe on
existing data and cheap (the `registry` and `library` shards are shipped to the bucket
page by page).

## Testing

`store.OpenTestRegistry(t)` and its siblings (`testing.go`) return a store over a
throwaway local SQLite file with the shard's real schema, so statements behave as on a
shard. It does not exercise the bucket; `s3db`'s own tests cover that. Use them for
every test that needs a database.
