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
# The official installer runs without prompts (--yes) and installs the pinned version
# below. It does not verify a checksum; the previous download did, so restore that if
# the build must resist a tampered release.
ARG RAILWAY_CLI_VERSION=5.59.0
RUN apk add --no-cache ca-certificates bash curl ncurses \
 && curl -fsSL -o /tmp/install.sh https://railway.com/install.sh \
 && RAILWAY_VERSION="${RAILWAY_CLI_VERSION}" bash /tmp/install.sh --yes --bin-dir /usr/local/bin \
 && railway --version \
 && rm /tmp/install.sh \
 && apk del bash curl ncurses \
 && adduser -D -u 10001 steki
WORKDIR /app
COPY --from=builder /app/web .
USER steki
# 8080 is public; 8081 is the internal API, reachable only on the private network.
EXPOSE 8080 8081
ENTRYPOINT ["./web"]
