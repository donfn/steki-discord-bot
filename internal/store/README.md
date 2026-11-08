# store

The persistence layer. It gives typed access to SQLite shards that [`s3db`](../s3db/)
keeps in the bucket. Each shard has one concern and one writing service. There is no ORM
and no query builder. See the
[architecture](../../docs/ARCHITECTURE.md#data-sqlite-shards-in-a-bucket).

## Shards

Each shard is a distinct Go type. A service that holds one cannot compile a query against
another's tables, so the ownership rule is checked by the compiler. Data crosses services
only through [`rpc/`](../rpc/).

| Type | Owner | Holds |
|---|---|---|
| `store.Registry` | registration | addresses, members, registrations, events, bans |
| `store.Guard` | moderation | media guard state |
| `store.Activity` | moderation | when members last spoke |
| `store.Library` | web | materials, reports, stickies, study notes, address bans, events |

## Conventions

- Every exported method takes a `context.Context` and runs one statement or one explicit
  transaction.
- Timestamps are integer Unix milliseconds.
- Queries use `?` placeholders only. `sqlsafety_test.go` scans for queries built from
  strings.
- Domain types are in `models.go`.
- A table that analytics events draw on must not contain a Discord ID, email address or
  username. Analytics receives counts and labels only.

## Migrations

SQL files under `migrations/<shard>/` are embedded and applied in numeric order when a
shard opens. To change a schema, add a new numbered file. A file that has been deployed
is never edited, because it has already run against live data. Migrations must be safe on
existing rows and cheap, since the `registry` and `library` shards are shipped to the
bucket page by page.

## Testing

`store.OpenTestRegistry(t)` and its counterparts (`testing.go`) return a store over a
temporary SQLite file with the real schema. They do not exercise the bucket, which the
tests of `s3db` cover. Every test that needs a database should use them.
