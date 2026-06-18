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

# Install Ollama (multi-arch, from ollama.com — same source as official install script)
RUN set -eux; \
    case "${TARGETARCH}" in \
        amd64) OLLAMA_ARCH="amd64";; \
        arm64) OLLAMA_ARCH="arm64";; \
        *) echo "Unsupported arch: ${TARGETARCH}"; exit 1;; \
    esac; \
    echo "Downloading Ollama for ${OLLAMA_ARCH}..."; \
    curl -fsSL "https://ollama.com/download/ollama-linux-${OLLAMA_ARCH}.tar.zst" \
        | tar --use-compress-program=unzstd -xf - -C /usr/local; \
    chmod +x /usr/local/bin/ollama; \
    /usr/local/bin/ollama --version || true

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
