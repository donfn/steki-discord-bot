# mcp

Serves the course material to AI assistants over the
[Model Context Protocol](https://modelcontextprotocol.io) (Streamable HTTP, stateless),
behind its own **OAuth 2.1** authorization server that logs members in with Discord.
Linked only by the `web` service. It is off unless `DISCORD_CLIENT_ID` and
`DISCORD_CLIENT_SECRET` are set.

The login sequence is drawn in
[ARCHITECTURE.md](../docs/ARCHITECTURE.md#mcp-server).

## Tools

Read-only by construction; there is no tool that writes.

| Tool | Returns |
|---|---|
| list courses | the course channels the member may read |
| material index | every file in a channel with caption, uploader and date |
| download | one file's content |

Every call is checked against the member's **current** standing in the server, not only
against their token: a member who loses their role loses access at once. Names, captions
and file contents are written by students, so they are sanitised before being returned
and clients are told to treat them as data.

## Routes

`POST /mcp`, `GET /.well-known/oauth-protected-resource`,
`GET /.well-known/oauth-authorization-server`, `GET /.well-known/jwks.json`,
`POST /oauth/register`, `GET /oauth/authorize`, `GET /oauth/callback`,
`GET /oauth/approve`, `POST /oauth/token`.

In the Discord developer portal, add the redirect `<PUBLIC_URL>/oauth/callback`.

## Files

| File | Holds |
|---|---|
| `server.go` | routes and the MCP request loop |
| `oauth.go` | the authorization server: dynamic registration, PKCE, tokens |
| `signer.go` | JWT signing with `JWT_SECRET` |
| `tools.go` | the three tools |
| `library.go` | the `Library` interface the web service implements (who may read what) |
| `consent.html` | the consent page |

## Trying it

`go test ./mcp` covers the flow end to end with fakes. Against a running `web` service,
point an MCP client (Claude, an inspector) at `<PUBLIC_URL>/mcp`; `/mcp/setup` has the
member-facing instructions.
