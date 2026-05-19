# syntax=docker/dockerfile:1.7

# ---- Build Stage: Picobot ----
FROM golang:1.26-alpine AS picobot-builder

ARG TARGETOS=linux
ARG TARGETARCH=arm64
ARG TARGETVARIANT

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN set -eux; \
    GOOS="${TARGETOS:-linux}"; \
    GOARCH="${TARGETARCH:-arm64}"; \
    if [ "$GOARCH" = "arm" ] && [ -n "${TARGETVARIANT}" ]; then \
        GOARM="${TARGETVARIANT#v}"; \
    fi; \
    CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" ${GOARM:+GOARM=$GOARM} \
    go build -ldflags="-s -w" -o /picobot ./cmd/picobot

# ---- Runtime Stage ----
FROM debian:13-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates tzdata bash curl jq libstdc++6 gosu && \
    rm -rf /var/lib/apt/lists/*

# Create non-root user
RUN groupadd -r picobot && useradd -r -g picobot -m picobot

# Install Picobot
COPY --from=picobot-builder /picobot /usr/local/bin/picobot
RUN chmod +x /usr/local/bin/picobot

# Install Ollama (copy from local build if present, otherwise skip)
# To include: place ollama-bin/ollama and ollama-bin/lib/*.so* in build context
COPY ollama-bin/ /tmp/ollama-install/
RUN if [ -f /tmp/ollama-install/ollama ]; then \
        echo "Installing Ollama from local build..."; \
        cp /tmp/ollama-install/ollama /usr/bin/ollama && \
        chmod +x /usr/bin/ollama && \
        mkdir -p /lib/ollama && \
        cp /tmp/ollama-install/lib/*.so* /lib/ollama/ 2>/dev/null || true; \
        ldconfig /lib/ollama 2>/dev/null || true; \
        echo "Ollama installed."; \
    else \
        echo "No Ollama binary found — brain will use external Ollama or remote API."; \
    fi && \
    rm -rf /tmp/ollama-install

# Picobot home directory
ENV PICOBOT_HOME=/home/picobot/.picobot
RUN mkdir -p ${PICOBOT_HOME}/.ollama/models \
             ${PICOBOT_HOME}/workspace/memory \
             ${PICOBOT_HOME}/workspace/skills && \
    chown -R picobot:picobot /home/picobot

COPY docker/entrypoint.sh /entrypoint.sh
COPY docker/docker-entrypoint-init.sh /docker-entrypoint-init.sh
RUN chmod +x /entrypoint.sh /docker-entrypoint-init.sh

# Init script runs as root to fix volume permissions, then drops to picobot
ENTRYPOINT ["/docker-entrypoint-init.sh"]
CMD ["gateway"]
