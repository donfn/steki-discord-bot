# syntax=docker/dockerfile:1
FROM golang:1.26.10-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o steki-bot .

FROM alpine:3.22
# The Railway CLI lets the bot switch on Under Attack Mode when it sees a flood.
ARG RAILWAY_CLI_VERSION=5.59.0
RUN apk add --no-cache ca-certificates \
 && wget -qO- "https://github.com/railwayapp/cli/releases/download/v${RAILWAY_CLI_VERSION}/railway-v${RAILWAY_CLI_VERSION}-x86_64-unknown-linux-musl.tar.gz" \
  | tar -xz -C /usr/local/bin railway \
 && adduser -D -u 10001 steki
WORKDIR /app
COPY --from=builder /app/steki-bot .
USER steki
EXPOSE 8080
CMD ["./steki-bot"]
