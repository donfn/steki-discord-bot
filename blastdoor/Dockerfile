# syntax=docker/dockerfile:1
# Build with the repository root as the context.
FROM golang:1.26.10-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY blastdoor ./blastdoor
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /blastdoor-server ./blastdoor/cmd

FROM alpine:3.22
# libheif-tools converts HEIC photos. This is the only container that has it.
RUN apk add --no-cache libheif-tools && adduser -D -u 10001 blastdoor
COPY --from=builder /blastdoor-server /usr/local/bin/blastdoor
USER blastdoor
EXPOSE 8080
ENTRYPOINT ["blastdoor"]
