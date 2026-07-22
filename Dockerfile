FROM golang:1.26 AS builder

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o watchdogd

FROM alpine:latest

# Run as an unprivileged user to limit the blast radius of a container compromise.
RUN addgroup -S watchdog && adduser -S -G watchdog watchdog

COPY --from=builder /app/watchdogd /app/watchdogd

EXPOSE 8080

WORKDIR /app

USER watchdog

ENTRYPOINT ["/app/watchdogd" , "--config","/config/config.yaml"]