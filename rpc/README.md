# rpc

How the three services talk to each other over Railway's private network. See the
[call graph](../docs/ARCHITECTURE.md#service-to-service-calls).

## Wire format

- JSON over HTTP `POST`, port **8081** (`rpc.Port`). The port has no public route.
- Two headers: `X-Steki-Time` and `X-Steki-Signature`, an HMAC over the time and body
  with a key derived from `INTERNAL_SECRET`.
- Requests older than one minute are refused, which bounds a replay.
- Bodies are limited to 1 MiB; a call times out after 15 s.
- `blastdoor` has no secret and is **not** part of this API.

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

## Adding a call

1. Add a `Path…` constant and typed request and answer structs in `rpc.go`. Return
   `rpc.None` when there is nothing to send.
2. On the owning service, register it in that service's `API(srv, session)` method with
   `rpc.Handle(srv, rpc.PathX, func(ctx, in Req) (Resp, error) { … })`.
3. On the caller, call it through its `rpc` client for that peer (`Call(ctx, path, req,
   &resp)`).
4. Add it to the table above and to the diagram in `docs/ARCHITECTURE.md`.

Design rule: **the owner decides, the caller asks.** If the call would let a service
write another's tables, it is the wrong call.
