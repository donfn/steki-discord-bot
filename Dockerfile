FROM golang:alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o steki-bot .

FROM alpine:latest
WORKDIR /app
COPY --from=builder /app/steki-bot .
CMD ["./steki-bot"]
