## What and why

<!-- What does this change, and why is it needed? Link an issue if there is one. -->

## How to check it

<!-- Tests you added, or how to try it in a test server. -->

## Checklist

- [ ] `go build ./... && go vet ./... && go test ./...` passes
- [ ] Tests added or updated for the behaviour I changed
- [ ] Docs updated: package README, `docs/ARCHITECTURE.md` diagrams, `docs/COMMANDS.md`, `.env.example` (whichever apply)
- [ ] No user ID, username, email, IP or message text is logged or sent to analytics
- [ ] New package linked by a service? Added to its `cmd/<service>/settings.toml` watch patterns
- [ ] Public HTTP route added, renamed or removed? Maintainers: update the Railway edge-rule allowlist
- [ ] New secret? Only in the one service's config loader, and in `TestSecretsStayWithTheirService`
