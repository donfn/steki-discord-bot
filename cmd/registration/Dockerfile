# syntax=docker/dockerfile:1
# Build with the repository root as the context.
FROM golang:1.26.10-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /registration ./cmd/registration

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 steki
COPY --from=builder /registration /usr/local/bin/registration
USER steki
EXPOSE 8080 8081
ENTRYPOINT ["registration"]
