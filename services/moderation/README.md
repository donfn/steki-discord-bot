# services/moderation

Screens messages. It reads every message on the server, so it holds the least: no member
address, no files, no mail key and no web pages. To close an address it asks
`registration` over RPC. The order of the checks is drawn in the
[architecture](../../docs/ARCHITECTURE.md#message-screening). Entry point:
[`cmd/moderation`](../../cmd/moderation/); the service also installs the slash commands
with Discord on start-up.

## Screening

`onMessage` applies the checks in order of decisiveness, so that cheap and conclusive
tests run first:

1. Links on the blacklist (`URL_BLACKLIST`) or in the phishing feeds: the message is
   deleted and the sender is timed out.
2. The intelligence feed (`intel.go`): an image or link that is in the feed, or that the author
   sent twice within five minutes. Either way the messages are deleted and the author is
   restricted once. A file is recognised by name and size with no download; only a message that
   matches a feed entry, or repeats, is downloaded and hashed (SHA-256, the entry's key). A
   repeat adds the item to the feed and posts a card to the log channel, with the image
   attached and a button that removes the entry again (staff only). Links to widely shared
   hosts are never learned.
3. Invitations to other servers (`invites.go`).
4. The same text posted in several channels, the usual sign of a hijacked account
   (`duplicates.go`).
5. The media guard (`mediaguard.go`), which holds a member's attachments and links until
   the member passes a captcha. `web` serves the captcha page and this service answers it.
6. Messages from an author who has just been restricted are removed from the material
   page by asking `web` (`internal/materials/drop-message`).

Edits and deletions are logged from a ring of the last 1500 messages (`msgcache.go`).

## Other duties

- The ghost role (`ghosts.go`) marks members silent for three months, using the
  `activity` shard.
- The phishing feeds are read as a stream into a sorted slice of 64-bit hashes and
  refreshed periodically.
- Join-rate alerts and `/rules post`.
- Bans (`bans.go`): the Discord ban stops the account, and the request to `registration`
  stops the person.
- `/bot stats` reports the health of this service and of the other two.

Helpers shared with other services (log channel, blacklist matching, feed streaming) are
in [`internal/moderation`](../../internal/moderation/), outside this package.
