# Stage 1: Build
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build (VERSION is set via --build-arg)
ARG VERSION=dev
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.Version=${VERSION}" -o /bin/firew2oai ./cmd/server/

# Stage 2: Runtime
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata wget && \
    adduser -D -H -u 1001 appuser

# Copy binary
COPY --from=builder /bin/firew2oai /firew2oai

USER appuser

EXPOSE 39527

ENTRYPOINT ["/firew2oai"]
