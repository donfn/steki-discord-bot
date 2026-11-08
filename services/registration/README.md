# services/registration

Admits members. It is the only service that holds the mail API key and the members'
email addresses (the `registry` shard), and for that reason it serves no web pages and
handles no uploaded files. The registration sequence is drawn in the
[architecture](../../docs/ARCHITECTURE.md#registration). Entry point:
[`cmd/registration`](../../cmd/registration/).

## Behaviour

- A new member receives a private `register-<id>` channel containing one message, the
  card, that lists the three steps. The button on the card opens a Discord form in which
  the member enters a student address.
- `classifyEmail` maps the address to the role it earns, or rejects it. The service then
  sends a signed link through the mail provider.
- When the member opens the consent page, `web` asks this service over RPC what to show.
  On acceptance this service binds the address to the member and grants the role.
- A reaper sends one reminder after one hour of inactivity and removes the member after
  six hours. A verified member is never removed.
- Moderators use `/reg` and `/email` ([commands](../../docs/COMMANDS.md)).

## Invariants

- Work for one member is serialised with `bot.KeyedMutex`, and channel placement is
  serialised across the guild, so two events cannot create two channels.
- Before any removal the reaper reads the member's row and roles again.
- The package must not import `verify`, `mcp`, `materials`, `usernotes`, `shield` or
  another service. `TestServicesStayInTheirLane` checks this.
- No address or user ID may appear in a log line or in an analytics event.
