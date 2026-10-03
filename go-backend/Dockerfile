# syntax=docker/dockerfile:1
# -----------------------------------------------------------------------------
# Stage 1: build the Go backend (replaces "pip install -r requirements.txt")
# -----------------------------------------------------------------------------
FROM golang:1.23-alpine AS builder

WORKDIR /src

# Cache-friendly dependency layer
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/super-clipboard .

# -----------------------------------------------------------------------------
# Stage 2: runtime (replaces python:3.11-slim + uvicorn)
# -----------------------------------------------------------------------------
FROM alpine:3.20 AS runtime

# ca-certificates is required for the captcha siteverify calls, tzdata keeps the
# direct-link HTML timestamps readable.
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /out/super-clipboard /usr/local/bin/super-clipboard

# Same storage layout as the Python image so existing volumes keep working.
RUN mkdir -p /app/backend/storage/files

# Optional: copy a prebuilt frontend bundle so the Go server can serve it.
# RUN --mount=type=bind,source=dist,target=/dist cp -r /dist /app/dist

ENV SUPER_CLIPBOARD_APP_HOST=0.0.0.0 \
    SUPER_CLIPBOARD_APP_PORT=5173 \
    SUPER_CLIPBOARD_DATABASE_PATH=/app/backend/storage/clipboard.db \
    SUPER_CLIPBOARD_FILE_STORAGE_DIR=/app/backend/storage/files \
    SUPER_CLIPBOARD_STATIC_ROOT=/app/dist

EXPOSE 5173
VOLUME ["/app/backend/storage"]

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
    CMD wget -qO- http://127.0.0.1:5173/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/super-clipboard"]
