# blastdoor

A small HTTP service that parses and compresses untrusted files in a container that holds
nothing worth stealing. If a decoder contains an exploitable flaw, the attacker reaches a
process with no secrets, no bucket credentials and no bot to attack. This is the reason
it is a separate unit; see the
[architecture](../docs/ARCHITECTURE.md#why-three-services-and-a-blastdoor).

It reads one setting, `PORT` (default `8080`). It must never be given a secret.

## API

| Route | Behaviour |
|---|---|
| `POST /convert` | image in, JPEG out (HEIC and other formats through libheif). Status `422` means the image is unreadable |
| `POST /compress` | bytes in, zstd out (level 22) |
| `POST /decompress` | zstd in, bytes out |
| `GET /healthz` | status `204` |
| `GET /ready` | status `200`, only after the sandbox self-check has passed |

Inputs are limited to 100 MiB (`MaxInput`). Concurrency is bounded so that a burst cannot
exhaust memory: two conversions, one compression (level 22 can need 1.3 GiB) and four
decompressions at a time.

Only `web` calls it, over plain HTTP. `web` treats everything returned as untrusted.

## Sandbox

On Linux (amd64 and arm64) each conversion runs in a child process under a seccomp filter
that forbids creating sockets, directly or through io_uring (`sandbox_linux.go`). At
start-up `CheckSandbox` verifies the filter, and the process refuses to listen if the
check fails. On other platforms `sandbox_other.go` supplies a stub so that the package
builds and its tests run on a laptop. The stub is not a sandbox.

## Rebuilds

The image installs `libheif-tools` and `zstd`, which change only when the image is
rebuilt. `.github/workflows/rebuild-blastdoor.yml` commits a new `rebuild-stamp` every
Monday, so that Railway repeats the installation and receives security fixes. No other
service watches that file.

## Running and testing

```bash
go test ./blastdoor
docker build -f blastdoor/Dockerfile -t blastdoor .   # the repository root is the build context
```

The service needs libheif's converter (`heif-convert` or `heif-dec`) and `zstd` on
`PATH`. The Docker image provides both, so tests that depend on them skip or fail on a
machine without them.
