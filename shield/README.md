# shield

Watches the public HTTP surface address by address and blocks scanners and floods.
Linked only by `web`. The thresholds and the Railway side are in
[OPERATIONS.md](../docs/OPERATIONS.md#ip-challenges-and-bans); the state machine is
drawn in [ARCHITECTURE.md](../docs/ARCHITECTURE.md#ip-shield).

| File | Holds |
|---|---|
| `detector.go` | per-address counters (IPv6 as its /64), challenge and ban decisions |
| `edge.go` | writes the blocks to Railway **edge rules** so the edge stops them first |
| `railway.go` | turns Railway's Under Attack Mode on through the CLI |

Blocks are stored in `library.ip_bans`, so a redeploy does not lift them. The bot also
refuses banned addresses itself, which covers IPv6 (the edge matches IPv4 only) and the
seconds before the edge updates.

Every counter is bounded and swept; do not add per-address state without a cap.
`go test ./shield` needs no network: Railway is behind interfaces.
