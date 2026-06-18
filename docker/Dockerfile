# syntax=docker/dockerfile:1.7

# ---- Build Stage: Gino ----
FROM golang:1.26-alpine AS gino-builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /gino ./cmd/gino

# ---- Runtime Stage ----
FROM debian:13-slim

ARG TARGETARCH
ARG OLLAMA_VERSION=latest

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates tzdata bash curl jq libstdc++6 gosu zstd && \
    rm -rf /var/lib/apt/lists/*

# Create non-root user
RUN groupadd -r gino && useradd -r -g gino -m gino

# Install Gino
COPY --from=gino-builder /gino /usr/local/bin/gino
RUN chmod +x /usr/local/bin/gino

# Install Ollama (multi-arch, downloaded from GitHub releases during build)
RUN set -eux; \
    case "${TARGETARCH}" in \
        amd64) OLLAMA_ARCH="amd64";; \
        arm64) OLLAMA_ARCH="arm64";; \
        *) echo "Unsupported arch: ${TARGETARCH}"; exit 1;; \
    esac; \
    echo "Downloading Ollama for ${OLLAMA_ARCH}..."; \
    OLLAMA_VER=$(curl -fsSL https://api.github.com/repos/ollama/ollama/releases/latest | jq -r '.tag_name'); \
    curl -fsSL "https://github.com/ollama/ollama/releases/download/${OLLAMA_VER}/ollama-linux-${OLLAMA_ARCH}.tar.zst" -o /tmp/ollama.tar.zst; \
    mkdir -p /tmp/ollama-extract; \
    tar --use-compress-program=unzstd -xf /tmp/ollama.tar.zst -C /tmp/ollama-extract; \
    cp /tmp/ollama-extract/ollama /usr/bin/ollama; \
    chmod +x /usr/bin/ollama; \
    rm -rf /tmp/ollama.tar.zst /tmp/ollama-extract; \
    ollama --version || true

# Gino home directory
ENV GINO_HOME=/home/gino/.gino
ENV GINO_BRAIN_ENABLED=false
RUN mkdir -p ${GINO_HOME}/.ollama/models \
             ${GINO_HOME}/workspace/memory \
             ${GINO_HOME}/workspace/skills && \
    chown -R gino:gino /home/gino

COPY docker/entrypoint.sh /entrypoint.sh
COPY docker/init.sh /init.sh
RUN chmod +x /entrypoint.sh /init.sh

WORKDIR /home/gino/.gino

# Init script runs as root to fix volume permissions, then drops to gino
ENTRYPOINT ["/init.sh"]
CMD ["gateway"]
