# syntax=docker/dockerfile:1
FROM golang:1.26.10-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# -s -w drop the symbol table and DWARF data: a smaller image and fewer pages
# for the loader to map, with no effect on behaviour.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o steki-bot .

FROM alpine:3.22
# The Railway CLI lets the bot switch on Under Attack Mode when it sees a flood.
# su-exec lets the entrypoint drop root after preparing the data volume.
ARG RAILWAY_CLI_VERSION=5.59.0
RUN apk add --no-cache ca-certificates su-exec \
 && wget -qO- "https://github.com/railwayapp/cli/releases/download/v${RAILWAY_CLI_VERSION}/railway-v${RAILWAY_CLI_VERSION}-x86_64-unknown-linux-musl.tar.gz" \
  | tar -xz -C /usr/local/bin railway \
 && adduser -D -u 10001 steki
WORKDIR /app
COPY --from=builder /app/steki-bot .
COPY docker-entrypoint.sh .
RUN chmod +x docker-entrypoint.sh
# The entrypoint runs as root to chown the volume, then execs the bot as the
# unprivileged "steki" user.
EXPOSE 8080
ENTRYPOINT ["./docker-entrypoint.sh"]
