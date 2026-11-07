# services/registration

Lets members in. It alone holds the **Resend key** and the members' **email addresses**
(the `registry` shard). It serves no pages and handles no uploaded files.

Entry point: [`cmd/registration`](../../cmd/registration/). The registration sequence is
drawn in [ARCHITECTURE.md](../../docs/ARCHITECTURE.md#registration).

## What it does

- Gives each new member a private `register-<id>` channel with one **card** message
  showing the three steps. The member opens a Discord form from the card's button and
  types their student address.
- Classifies the address (`classifyEmail`) into the role it earns, or refuses it, then
  emails a signed link through Resend.
- Answers `web` over RPC when the member opens the consent page, then binds the address
  and grants the role when they accept.
- Runs the **reaper**: one reminder after 1 h idle, a kick after 6 h, never a verified
  member.
- Provides `/reg` and `/email` for moderators ([commands](../../docs/COMMANDS.md)).

## Files

| File | Holds |
|---|---|
| `handler.go` | `Handler`, gateway registration, RPC routes, intents |
| `member_add.go`, `member_remove.go`, `reaction_add.go` | join, leave, and the legacy restart reaction |
| `card.go`, `emailform.go`, `help.go` | the register channel's card, address form and help button |
| `email.go`, `linkconsent.go` | address classification, sending, what the consent page shows |
| `registration.go` | granting verification, and the errors callers turn into messages |
| `maintenance.go` | the reaper and `/reg reconcile` |
| `cmd_reg.go`, `cmd_email.go` | the slash commands |
| `constants.go` | the lifecycle timings |

## Invariants

- All registration work for one member is serialised (`bot.KeyedMutex`), and channel
  placement is serialised guild-wide; two events never make two channels.
- Before any kick the reaper re-reads the row and the member's roles.
- It must **not** import `verify`, `mcp`, `materials`, `usernotes`, `shield` or the
  other services (enforced).
- Nothing it logs or reports to analytics may contain an address or a user ID.
