FROM golang:1.21 AS builder

WORKDIR /app

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o watchdogd

FROM alpine:latest

RUN mkdir -p /tmp && chmod 777 /tmp

COPY --from=builder /app/watchdogd /app/watchdogd

EXPOSE 8079

WORKDIR /app

ENTRYPOINT ["/app/watchdogd"]