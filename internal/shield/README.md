# shield

Watches the public HTTP surface address by address and blocks scanners and floods. Only
the `web` service links this package. The thresholds and the Railway configuration are in
[OPERATIONS.md](../../docs/OPERATIONS.md#ip-challenges-and-bans), and the state machine is
drawn in the [architecture](../../docs/ARCHITECTURE.md#ip-shield).

- `detector.go` keeps per-address counters (an IPv6 address is counted as its /64) and
  decides on challenges and bans.
- `edge.go` writes the blocks to Railway edge rules, so that the edge stops the traffic
  before it reaches the service.
- `railway.go` switches Railway's Under Attack Mode on through the CLI.

Blocks are stored in `library.ip_bans`, so a redeploy does not lift them. The bot also
refuses banned addresses itself. This covers IPv6, which the edge rules do not match, and
the seconds before an edge update takes effect.

Every counter is bounded and swept. New per-address state needs a cap, because an
attacker controls how many addresses appear. `go test ./shield` needs no network, since
Railway sits behind interfaces.
