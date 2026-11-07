# services/moderation

Keeps the server clean. It reads every message, so it is deliberately given the least:
**no member's address, no files, no mail key, no web pages.** To close an address it asks
`registration` over RPC.

Entry point: [`cmd/moderation`](../../cmd/moderation/). It also installs the slash
commands with Discord on start-up. The screening order is drawn in
[ARCHITECTURE.md](../../docs/ARCHITECTURE.md#message-screening).

## Screening (`onMessage`)

Strongest signal first, so cheap and decisive checks win:

1. blacklisted links (`URL_BLACKLIST`) and the phishing feeds: delete, time out the
   sender
2. invites to other servers (`invites.go`)
3. cross-channel duplicates, the signature of a hijacked account (`duplicates.go`)
4. the **media guard** (`mediaguard.go`): holds a member's attachments or links until
   they pass a captcha; the captcha page is served by `web` and answered here
5. what it only restricts the author over, it tells `web` to drop (`materials/drop-message`)

Deleted and edited messages are logged from a fixed ring of the last 1500 messages
(`msgcache.go`).

## Other jobs

- **Ghost role** (`ghosts.go`): members silent for three months, from the `activity`
  shard.
- **Phishing feeds** streamed into a sorted slice of 64-bit hashes, refreshed
  periodically.
- **Join-rate alerts** and `/rules post`.
- **Bans** (`bans.go`): the Discord ban stops the account; asking `registration` to ban
  the address stops the person.
- `/bot stats` shows this service's health and each other service's.

## Files

| File | Holds |
|---|---|
| `handler.go` | `Handler`, registration, RPC routes, intents |
| `message.go`, `message_edits.go`, `msgcache.go` | screening, edit and delete logs |
| `mediaguard.go`, `guardcmds.go` | the media guard and `/guard` |
| `duplicates.go`, `invites.go`, `ghosts.go` | the individual checks |
| `bans.go`, `cmd_bot.go`, `rules.go` | `/member`, `/bot`, `/rules` |
| `constants.go` | feed refresh interval, join-rate thresholds |

Shared helpers (log channel, blacklist matching, feed streaming) are in
[`/moderation`](../../moderation/), not here, because other services use them too.
