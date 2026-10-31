# syntax=docker/dockerfile:1
# Build with the repository root as the context.
FROM golang:1.26.10-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY blastdoor ./blastdoor
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /blastdoor-server ./blastdoor/cmd

FROM alpine:3.22
# libheif-tools converts HEIC photos and zstd does the compression. This is the
# only container that has them. The stamp comes first so that changing it, which
# .github/workflows/rebuild-blastdoor.yml does weekly, reruns the install past the
# build cache and picks up security fixes.
COPY blastdoor/rebuild-stamp /etc/blastdoor-rebuild-stamp
RUN apk upgrade --no-cache && apk add --no-cache libheif-tools zstd && adduser -D -u 10001 blastdoor
COPY --from=builder /blastdoor-server /usr/local/bin/blastdoor
USER blastdoor
EXPOSE 8080
ENTRYPOINT ["blastdoor"]
