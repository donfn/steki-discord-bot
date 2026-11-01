# syntax=docker/dockerfile:1
FROM golang:1.26.10-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# -s -w drop the symbol table and DWARF data: a smaller image and fewer pages
# for the loader to map, with no effect on behaviour.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o web ./cmd/web

FROM alpine:3.22
# This is the web service's image. The Railway CLI lets it switch on Under Attack
# Mode when it sees a flood; no other service has the CLI or its token.
# su-exec lets the entrypoint drop root after preparing the data volume.
# The download is checked against a pinned SHA-256, so a changed release asset
# stops the build instead of landing in the image. Update both together.
ARG RAILWAY_CLI_VERSION=5.59.0
ARG RAILWAY_CLI_SHA256=d6dec28a178348b6feee350eae6d137e14e88948ea2d7289ee037991a33550b1
RUN apk add --no-cache ca-certificates su-exec \
 && wget -qO /tmp/railway.tgz "https://github.com/railwayapp/cli/releases/download/v${RAILWAY_CLI_VERSION}/railway-v${RAILWAY_CLI_VERSION}-x86_64-unknown-linux-musl.tar.gz" \
 && echo "${RAILWAY_CLI_SHA256}  /tmp/railway.tgz" | sha256sum -c - \
 && tar -xz -C /usr/local/bin -f /tmp/railway.tgz railway \
 && rm /tmp/railway.tgz \
 && adduser -D -u 10001 steki
WORKDIR /app
COPY --from=builder /app/web .
COPY docker-entrypoint.sh .
RUN chmod +x docker-entrypoint.sh
# The entrypoint runs as root to chown the old volume, then execs the service as
# the unprivileged "steki" user.
# 8080 is public; 8081 is the internal API, reachable only on the private network.
EXPOSE 8080 8081
ENTRYPOINT ["./docker-entrypoint.sh"]
