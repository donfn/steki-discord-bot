# mcp

Serves the course material to AI assistants over the
[Model Context Protocol](https://modelcontextprotocol.io) (Streamable HTTP, stateless).
Access is controlled by an OAuth 2.1 authorization server in this package, which logs
members in with Discord. Only the `web` service links the package, and it is disabled
unless `DISCORD_CLIENT_ID` and `DISCORD_CLIENT_SECRET` are set. The login sequence is
drawn in the [architecture](../../docs/ARCHITECTURE.md#mcp-server).

## Tools

The server cannot write: none of its tools modifies anything.

| Tool | Returns |
|---|---|
| list courses | the course channels the member may read |
| material index | each file in a channel with caption, uploader and date |
| download | the content of one file |

Every call is checked against the member's current standing in the server as well as
against the token, so a member who loses the role loses access at once. Names, captions
and file contents are written by students. They are sanitised before they are returned,
and clients are told to treat them as data.

## Routes

`POST /mcp`, `GET /.well-known/oauth-protected-resource`,
`GET /.well-known/oauth-authorization-server`, `GET /.well-known/jwks.json`,
`POST /oauth/register`, `GET /oauth/authorize`, `GET /oauth/callback`,
`GET /oauth/approve` and `POST /oauth/token`.

In the Discord developer portal, register the redirect `<PUBLIC_URL>/oauth/callback`.

## Contents

`server.go` holds the routes and the request loop, `oauth.go` the authorization server
(dynamic registration, PKCE, tokens), `signer.go` the JWT signing with `JWT_SECRET`, and
`tools.go` the three tools. `library.go` defines the `Library` interface, which the
`web` service implements to decide who may read what, so this package needs no knowledge
of the database. `consent.html` is the consent page.

## Testing

`go test ./mcp` exercises the whole flow with fakes. Against a running `web` service,
point an MCP client at `<PUBLIC_URL>/mcp`. The page `/mcp/setup` gives members the
instructions.
