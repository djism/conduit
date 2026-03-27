# ── Build stage ───────────────────────────────────────────────────────────────
# Use the full Go image to compile — it has all build tools.
FROM golang:1.22-alpine AS builder

WORKDIR /app

# Copy dependency files first — Docker caches this layer.
# Only re-downloads modules when go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build.
# CGO_ENABLED=0: static binary, no C dependencies.
# GOOS=linux: target Linux even if building on Mac.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o conduit ./cmd/server

# ── Run stage ─────────────────────────────────────────────────────────────────
# Alpine is ~5MB — much smaller than the full Go image.
# Only copy the compiled binary — no source code in the final image.
FROM alpine:3.19

WORKDIR /app

# ca-certificates: needed for HTTPS calls (e.g. to external APIs).
RUN apk --no-cache add ca-certificates

COPY --from=builder /app/conduit .

EXPOSE 8080

CMD ["./conduit"]