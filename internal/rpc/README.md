# rpc

The internal API through which the three services communicate over Railway's private
network. Services share no database, so this package is the only way for one service to
use another's data. The call graph is in the
[architecture](../../docs/ARCHITECTURE.md#service-to-service-calls).

## Wire format

- JSON over HTTP `POST` on port 8081 (`rpc.Port`). The port has no public route.
- Two headers, `X-Steki-Time` and `X-Steki-Signature`. The signature is an HMAC over the
  time and the body, using a key derived from `INTERNAL_SECRET`.
- A request older than one minute is refused, which limits replay.
- Bodies are limited to 1 MiB and a call times out after 15 seconds.
- `blastdoor` holds no secret and is not part of this API.

## Routes

| Constant | Path | Served by | Called by |
|---|---|---|---|
| `PathLinkPending` | `/internal/link/pending` | registration | web |
| `PathLinkAccept` | `/internal/link/accept` | registration | web |
| `PathEmailsBan` | `/internal/emails/ban` | registration | moderation |
| `PathEmailsUnban` | `/internal/emails/unban` | registration | moderation |
| `PathGuardActive` | `/internal/guard/active` | moderation | web |
| `PathGuardLift` | `/internal/guard/lift` | moderation | web |
| `PathRosterSync` | `/internal/roster/sync` | moderation | registration |
| `PathScreenTitle` | `/internal/screen-title` | moderation | web |
| `PathDropMessage` | `/internal/materials/drop-message` | web | moderation |
| `PathForget` | `/internal/forget` | all three | registration |
| `PathEvents` | `/internal/events` | all three | registration |
| `PathStats` | `/internal/stats` | all three | moderation |

## Design rule

The owner of the data decides and the caller asks. A call that would let one service
write another's tables is the wrong call.

## Adding a call

1. Add a `Path…` constant and typed request and answer structs in `rpc.go`. Use
   `rpc.None` when there is nothing to send.
2. In the owning service's `API(srv, session)` method, register the handler with
   `rpc.Handle(srv, rpc.PathX, func(ctx, in Req) (Resp, error) { … })`.
3. In the caller, use the `rpc` client for that peer: `Call(ctx, path, req, &resp)`.
4. Update the table above and the diagram in `docs/ARCHITECTURE.md`.
