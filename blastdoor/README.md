# blastdoor

A small HTTP service that parses and compresses **untrusted files** in a container that
holds nothing worth stealing. If a decoder has an exploit, the attacker lands somewhere
with no secrets, no bucket credentials and no bot process to reach.

It reads no configuration except `PORT` (default `8080`). **Never give it a secret.**

Part of the [architecture](../docs/ARCHITECTURE.md#why-three-services-and-a-blastdoor).

## API

| Route | Does |
|---|---|
| `POST /convert` | image in, JPEG out (HEIC and others via libheif). `422` means unreadable image |
| `POST /compress` | bytes in, zstd out (level 22) |
| `POST /decompress` | zstd in, bytes out |
| `GET /healthz` | `204` |
| `GET /ready` | `200`, only once the sandbox self-check has passed |

Inputs are capped at 100 MiB (`MaxInput`). Concurrency is bounded so a burst cannot run
the container out of memory: 2 conversions, 1 compression (level 22 can take 1.3 GiB),
4 decompressions.

Only `web` calls it, over plain HTTP; `web` must treat everything that comes back as
untrusted.

## The sandbox

On Linux (amd64/arm64) each conversion runs in a child process under a seccomp filter
that forbids creating sockets, directly or through io_uring (`sandbox_linux.go`). At
start-up `CheckSandbox` proves the filter works and the process refuses to listen if it
does not. On other platforms `sandbox_other.go` compiles a stub so the package builds
and tests run on a laptop, but it is **not** a sandbox.

## Rebuilds

The image installs `libheif-tools` and `zstd`, whose packages only change when the image
is rebuilt. `.github/workflows/rebuild-blastdoor.yml` commits a new `rebuild-stamp`
every Monday, which makes Railway re-run the install and pick up security fixes. No
other service watches that file.

## Running and testing

```bash
go test ./blastdoor
docker build -f blastdoor/Dockerfile -t blastdoor .   # needs the repo root as context
```

The service needs libheif's converter (`heif-convert` or `heif-dec`) and `zstd` on
`PATH`; the Docker image provides them, so tests that need them skip or fail on a bare
laptop.
