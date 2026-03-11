# ─── Stage 1: Build Go binary ─────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

# ARG GOPROXY=https://proxy.golang.org,direct
# ENV GOPROXY=${GOPROXY}

RUN apk add --no-cache git ca-certificates

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/llm_telebot .

# ─── Stage 2: Runtime ─────────────────────────────────────────────────────────
# Use Node.js base so npx/node-based MCP servers work out of the box.
# Python + uvx can be installed below if you need Python-based MCP servers.
FROM node:25-alpine3.22

RUN apk add --no-cache ca-certificates tzdata \
    # Install Python + pip + uvx for Python-based MCP servers (optional, remove to slim down)
    python3 py3-pip pipx \
    && pipx install uvx 2>/dev/null || true

# Ensure pipx bin dir is in PATH
ENV PATH="/root/.local/bin:${PATH}"

WORKDIR /app

# Copy compiled binary
COPY --from=builder /out/llm_telebot /app/llm_telebot

# Default data directory
RUN mkdir -p /app/data

# Expose nothing — the bot uses outbound connections only.
# Data volume for bbolt databases
VOLUME ["/app/data"]

ENTRYPOINT ["/app/llm_telebot"]
