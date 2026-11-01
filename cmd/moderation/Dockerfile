# syntax=docker/dockerfile:1
# Build with the repository root as the context.
FROM golang:1.26.10-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /moderation ./cmd/moderation

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 steki
COPY --from=builder /moderation /usr/local/bin/moderation
USER steki
EXPOSE 8080 8081
ENTRYPOINT ["moderation"]
