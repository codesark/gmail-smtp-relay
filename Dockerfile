# syntax=docker/dockerfile:1.7

FROM golang:1.25-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . ./
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/smtp-relay ./cmd/smtp-relay

FROM alpine:3.20
RUN addgroup -S relay && adduser -S relay -G relay && \
    apk add --no-cache ca-certificates tzdata wget

WORKDIR /app
COPY --from=builder /out/smtp-relay /app/smtp-relay

RUN mkdir -p /app/data /tmp && chown -R relay:relay /app /tmp

USER relay
EXPOSE 465 587 8080
ENTRYPOINT ["/app/smtp-relay"]
