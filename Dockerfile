# syntax=docker/dockerfile:1
FROM golang:1.26.10-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o steki-bot .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 steki
WORKDIR /app
COPY --from=builder /app/steki-bot .
USER steki
EXPOSE 8080
CMD ["./steki-bot"]
