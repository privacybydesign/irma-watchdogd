FROM golang:1.21 AS builder

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o watchdogd

FROM alpine:latest

COPY --from=builder /app/watchdogd /app/watchdogd

EXPOSE 8080

WORKDIR /app

ENTRYPOINT ["/app/watchdogd" , "--config","/config/config.yaml"]