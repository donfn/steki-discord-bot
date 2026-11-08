# Moderator commands

Slash commands, restricted to the staff and mod roles; replies are ephemeral. Each
command is answered by exactly one service, which is why the list below names it
(`TestEveryCommandHasOneOwner` enforces this). Definitions live in `internal/bot/commands.go`;
`moderation` installs them with Discord on start-up.

| Command | Service | What it does |
|---|---|---|
| `/reg status <user>` | registration | where a member's registration stands |
| `/reg history <user>` | registration | every join, email attempt, link sent, retry, failure and role grant, from `registration_events` |
| `/reg stuck` | registration | registrations that are not moving |
| `/reg restart <user>` | registration | start a member's registration over |
| `/reg verify <user> <email>` | registration | verify a member by hand |
| `/reg reconcile` | registration | bring registrations and their channels back in line with who is on the server |
| `/reg reregister <user> [release_email]` | registration | take the verified roles away and open a fresh registration; the member keeps their address unless `release_email` frees it |
| `/reg forget <user>` | registration | erase a member who has left: member row, registrations, history, guard and help state. Their addresses stay recorded and spent, but lose their owner |
| `/email add <email\|hash> [user]` | registration | record an address |
| `/email whois <email\|hash>` | registration | who owns an address |
| `/email list <user>` | registration | a member's addresses |
| `/email update <email\|hash> [user \| unlink] [new_email]` | registration | move an address to another account, drop its owner, or correct a typo |
| `/email remove <email\|hash>` | registration | free the address in both forms |
| `/guard status\|restrict\|ungrace <user>` | moderation | the media guard |
| `/member ban\|unban` | moderation | ban or unban (asks `registration` to close the address) |
| `/rules post` | moderation | post the server rules |
| `/bot stats` | moderation | health, open registrations, when `/reg reconcile` last ran, admin cards waiting |
| `/materials scan\|stickies\|cards\|rescan` | web | catch-up passes, see below |
| `/materials view-as <channel> <user>` | web | opens a course's study page as a member sees it (read-only) |
| `/materials remove <message> <file>` | web | permanently removes a file from the page **and the bucket** |
| `/mcp announce`, `/mcp refresh` | web | post or refresh the MCP announcement |

Wherever `/email` asks for an email it also takes a 64-character SHA-256 hash, for the
addresses imported from the old bot that were never recovered in plaintext.

## Catch-up commands

These passes repair or redraw what is already in Discord. They touch many members,
channels or messages at once, so none of them runs at start-up, on a reconnect or on a
timer: a moderator runs them when needed, each can be run again safely, and two cannot
run at the same time.

| Command | When to run it |
|---------|----------------|
| `/reg reconcile` | After the bot was down or disconnected for a while, or when `/reg stuck` shows a member with no channel. Closes registrations of members who left or verified, makes missing register channels (also for recent joins the bot missed, at most 25 per run), deletes orphan and duplicate ones, refreshes the member index behind the ghost role |
| `/materials scan` | To pick up files posted before the bot, or while it was offline, and to place the stickies |
| `/materials stickies` | After a change to the sticky card: edits every existing sticky in place, silently |
| `/materials cards` | When `/bot stats` reports cards that did not reach the log channel (held images, reports) |
| `/materials rescan` | To send images uploaded before the content check existed through it |
| `/mcp refresh` | To give older MCP announcements the current button |

## Adding a command

1. Define it in `internal/bot/commands.go` (descriptions must be 1–100 characters; required
   options come before optional ones — `TestCommandDefinitionsFitDiscordLimits`).
2. Route it in exactly one service's `Routes()` and handler.
3. Keep it staff-only (`TestEveryCommandIsStaffOnly`).
4. Add it to the table above.
