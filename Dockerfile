# ── Stage 1: Build ──────────────────────────────────────────────────────────
FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.Version=1.0.0" -o /bin/firew2oai ./cmd/server/

# ── Stage 2: Runtime ───────────────────────────────────────────────────────
FROM scratch

# Copy CA certificates for HTTPS, timezone data
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

# Copy binary
COPY --from=builder /bin/firew2oai /firew2oai

EXPOSE 8000

ENTRYPOINT ["/firew2oai"]
