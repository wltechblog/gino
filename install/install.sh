#!/usr/bin/env bash
#
# install/install.sh — Gino installer (root + yolo + brain profile)
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/wltechblog/gino/main/install/install.sh | sudo bash
#
# For what this script does and why, see install/README.md.
#
# Assumes a Debian-based system with network access. Installs:
#   - Go toolchain (official tarball if system Go is too old)
#   - podman + an Ollama container for brain embeddings (localhost only)
#   - Gino, cloned from GitHub and built from source
#   - config.json with sandbox.mode=yolo and the brain enabled
#   - Telegram gateway systemd service if Telegram is chosen; else TUI mode
#
# All prompts read from /dev/tty so the script works through `curl | bash`
# (stdin carries the script itself and cannot be used for Q&A).
#
# Test hook (developers): GINO_INSTALL_TEST=1 skips package installation,
# podman/Ollama and systemd, and lets you override paths:
#   GINO_INSTALL_REPO_DIR / GINO_INSTALL_GINO_HOME / GINO_INSTALL_BIN_DIR

set -euo pipefail

# ── tunables ────────────────────────────────────────────────────────────────
TEST="${GINO_INSTALL_TEST:-0}"
REPO_URL="${GINO_INSTALL_REPO_URL:-https://github.com/wltechblog/gino.git}"
REPO_DIR="${GINO_INSTALL_REPO_DIR:-/opt/gino}"
BIN_DIR="${GINO_INSTALL_BIN_DIR:-/usr/local/bin}"
GINO_HOME="${GINO_INSTALL_GINO_HOME:-/root/.gino}"
UNIT_DIR="${GINO_INSTALL_UNIT_DIR:-/etc/systemd/system}"
OLLAMA_NAME="gino-ollama"
OLLAMA_DATA="${GINO_INSTALL_OLLAMA_DATA:-/opt/gino-ollama}"
OLLAMA_URL="http://127.0.0.1:11434"
OLLAMA_IMAGE="docker.io/ollama/ollama:latest"
EMBED_MODEL="nomic-embed-text"
GO_NEED="1.26.3"
GO_INSTALL="1.26.4"
DEFAULT_API_BASE="https://api.z.ai/api/coding/paas/v4"
DEFAULT_MODEL="glm-4.6"
DEFAULT_SUB_MODEL="glm-4.5-air"

# ── helpers ─────────────────────────────────────────────────────────────────
log()  { printf '\033[1;32m==>\033[0m %s\n' "$*" >&2; }
warn() { printf '\033[1;33mWARNING:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

# Prompts MUST come from /dev/tty: under `curl | sudo bash` stdin is the
# script text itself. fd 3 is opened read-write on the controlling terminal.
if [ "$TEST" != "1" ]; then
    [ "$(id -u)" -eq 0 ] || die "this installer must run as root (curl ... | sudo bash)"
fi
if [ ! -e /dev/tty ]; then
    die "no controlling terminal — run from an interactive shell"
fi
exec 3<>/dev/tty || die "cannot open /dev/tty for prompts"

ask() { # ask PROMPT [DEFAULT] -> sets REPLY
    local prompt="$1" def="${2:-}"
    if [ -n "$def" ]; then prompt="$prompt [$def]"; fi
    printf '%s: ' "$prompt" >&3
    if ! IFS= read -r REPLY <&3; then
        die "failed reading from /dev/tty (interactive terminal required)"
    fi
    REPLY="${REPLY%%$'\r'}"
    if [ -z "$REPLY" ] && [ -n "$def" ]; then REPLY="$def"; fi
}

ask_secret() { # ask_secret PROMPT [DEFAULT] -> sets REPLY (input hidden)
    local prompt="$1" def="${2:-}"
    if [ -n "$def" ]; then prompt="$prompt [$def]"; fi
    printf '%s: ' "$prompt" >&3
    if ! IFS= read -rs REPLY <&3; then
        die "failed reading from /dev/tty (interactive terminal required)"
    fi
    printf '\n' >&3
    REPLY="${REPLY%%$'\r'}"
    if [ -z "$REPLY" ] && [ -n "$def" ]; then REPLY="$def"; fi
}

ask_required() { # ask_required PROMPT [DEFAULT] -> sets REPLY (non-empty)
    local tries=0
    while :; do
        ask "$@"
        [ -n "$REPLY" ] && return 0
        tries=$((tries+1))
        [ "$tries" -ge 5 ] && die "no input after 5 attempts, aborting"
        printf '  a value is required\n' >&3
    done
}

json_sanitize() { # strip characters that would break JSON string literals
    printf '%s' "$1" | tr -d '"\\'
}

yes_no() { # sets REPLY to y/n
    case "$(printf '%s' "${1,,}")" in
        y|yes) REPLY=y ;;
        *)     REPLY=n ;;
    esac
}

retry() { # retry N label cmd...
    local n="$1" label="$2"; shift 2
    for i in $(seq 1 "$n"); do
        if "$@"; then return 0; fi
        warn "$label failed (attempt $i/$n)"
        [ "$i" -lt "$n" ] && sleep $((i * 3))
    done
    return 1
}

go_version_ok() { # go_version_ok MINIMUM
    local need="$1" have
    have="$(GOTOOLCHAIN=local go version 2>/dev/null | awk '{print $3}')" || return 1
    have="${have#go}"
    [ -n "$have" ] || return 1
    [ "$(printf '%s\n' "$need" "$have" | sort -V | head -n1)" = "$need" ]
}

# ════════════════════════════════════════════════════════════════════════════
log "Gino installer — root + yolo + brain profile"

# ── 1. base packages (deb-based assumption) ─────────────────────────────────
if [ "$TEST" != "1" ]; then
    command -v apt-get >/dev/null 2>&1 || die "apt-get not found — this installer targets Debian-based systems"
    export DEBIAN_FRONTEND=noninteractive
    log "updating package lists"
    apt-get update -y </dev/null >/dev/null
    log "installing base packages (git curl ca-certificates podman)"
    apt-get install -y git curl ca-certificates podman </dev/null >/dev/null
fi

# ── 2. Go toolchain ─────────────────────────────────────────────────────────
install_go() {
    local arch
    case "$(uname -m)" in
        x86_64)         arch=amd64 ;;
        aarch64|arm64)  arch=arm64 ;;
        *)              die "unsupported architecture: $(uname -m)" ;;
    esac
    local tarball="/tmp/go${GO_INSTALL}.linux-${arch}.tar.gz"
    log "downloading Go ${GO_INSTALL} (${arch})"
    retry 3 "go download" curl -fSL --retry 3 -o "$tarball" \
        "https://go.dev/dl/go${GO_INSTALL}.linux-${arch}.tar.gz" >&2
    log "installing Go to /usr/local/go"
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "$tarball"
    rm -f "$tarball"
    ln -sf /usr/local/go/bin/go /usr/local/bin/go
    ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
}

if command -v go >/dev/null 2>&1 && go_version_ok "$GO_NEED"; then
    log "using existing Go $(go version | awk '{print $3}')"
else
    if [ "$TEST" = "1" ]; then
        die "TEST mode expects a working Go >= ${GO_NEED} on PATH"
    fi
    install_go
fi
log "Go: $(go version | awk '{print $3}')"

# ── 3. clone / update the repository ────────────────────────────────────────
if [ "$TEST" != "1" ]; then
    if [ -d "$REPO_DIR/.git" ]; then
        log "updating existing clone at $REPO_DIR"
        git -C "$REPO_DIR" fetch --prune origin >&2 || warn "git fetch failed — building current checkout"
        git -C "$REPO_DIR" reset --hard origin/main >&2 || warn "could not reset to origin/main"
    else
        log "cloning $REPO_URL -> $REPO_DIR"
        retry 3 "git clone" git clone --depth 1 "$REPO_URL" "$REPO_DIR" >&2
    fi
elif [ ! -d "$REPO_DIR" ]; then
    die "TEST mode: GINO_INSTALL_REPO_DIR ($REPO_DIR) does not exist"
fi

# ── 4. build ────────────────────────────────────────────────────────────────
log "building gino (vendored deps, no cgo)"
mkdir -p "$BIN_DIR"
( cd "$REPO_DIR" && \
    GOTOOLCHAIN=local GOFLAGS=-mod=vendor CGO_ENABLED=0 \
    go build -trimpath -o "${BIN_DIR}/gino" ./cmd/gino ) >&2
[ -x "${BIN_DIR}/gino" ] || die "build produced no binary"
log "installed ${BIN_DIR}/gino"

# ── 5. Ollama container for the brain ───────────────────────────────────────
ollama_up() { curl -sf "${OLLAMA_URL}/api/version" >/dev/null 2>&1; }

write_ollama_unit() {
    cat > "${UNIT_DIR}/gino-ollama.service" <<EOF
[Unit]
Description=Gino Ollama (brain embeddings)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/bin/podman start -a ${OLLAMA_NAME}
ExecStop=/usr/bin/podman stop -t 10 ${OLLAMA_NAME}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
}

if [ "$TEST" != "1" ]; then
    if ollama_up; then
        warn "something already answers on ${OLLAMA_URL} — reusing it, skipping container setup"
    else
        mkdir -p "$OLLAMA_DATA"
        if ! podman container exists "$OLLAMA_NAME"; then
            log "creating Ollama container (localhost-only port)"
            retry 3 "podman create" podman create \
                --name "$OLLAMA_NAME" \
                -v "${OLLAMA_DATA}:/root/.ollama:Z" \
                -p 127.0.0.1:11434:11434 \
                "$OLLAMA_IMAGE" >&2
        else
            log "Ollama container '$OLLAMA_NAME' already exists — reusing"
        fi

        log "pulling Ollama image (may take a while)"
        retry 3 "podman pull" podman pull "$OLLAMA_IMAGE" >&2

        if command -v systemctl >/dev/null 2>&1; then
            write_ollama_unit
            systemctl daemon-reload
            log "starting gino-ollama service"
            systemctl enable --now gino-ollama >/dev/null 2>&1 || systemctl restart gino-ollama
        else
            warn "systemd not available — starting container directly (no boot persistence)"
            podman start "$OLLAMA_NAME" >&2
        fi

        log "waiting for Ollama API"
        waited=0
        until ollama_up; do
            sleep 2; waited=$((waited + 2))
            [ "$waited" -ge 120 ] && die "Ollama did not come up within 120s — check: podman logs $OLLAMA_NAME"
        done

        log "pulling embedding model '${EMBED_MODEL}'"
        retry 3 "model pull" podman exec "$OLLAMA_NAME" ollama pull "$EMBED_MODEL" >&2
    fi
fi

# ── 6. questions (all via /dev/tty) ─────────────────────────────────────────
printf '\n' >&3
printf '\033[1m── Provider ──────────────────────────────────────\033[0m\n' >&3

ask_required "LLM provider API base URL" "$DEFAULT_API_BASE"
API_BASE="$(json_sanitize "$REPLY")"

ask_required "API key"
API_KEY="$(json_sanitize "$REPLY")"

ask_required "Model name" "$DEFAULT_MODEL"
MODEL="$(json_sanitize "$REPLY")"

# optional subagent
SUB_ENABLED="false"; SUB_NAME=""; SUB_MODEL=""; SUB_USES_PRESET="false"
printf '\n' >&3
printf '\033[1m── Subagent (optional) ────────────────────────────\033[0m\n' >&3
ask "Configure a subagent (separate model for delegated tasks)?" "N"
yes_no "$REPLY"
if [ "$REPLY" = "y" ]; then
    SUB_ENABLED="true"
    ask_required "Subagent name" "researcher"
    SUB_NAME="$(json_sanitize "$REPLY")"
    # keep [a-zA-Z0-9_-]
    SUB_NAME="$(printf '%s' "$SUB_NAME" | tr -c 'a-zA-Z0-9_-' '-')"

    ask "Subagent API base URL" "$API_BASE"
    SUB_API_BASE="$(json_sanitize "$REPLY")"

    ask_secret "Subagent API key (empty = same as main)"
    SUB_API_KEY="$(json_sanitize "$REPLY")"
    [ -z "$SUB_API_KEY" ] && SUB_API_KEY="$API_KEY"

    ask_required "Subagent model" "$DEFAULT_SUB_MODEL"
    SUB_MODEL="$(json_sanitize "$REPLY")"

    if [ "$SUB_API_BASE" != "$API_BASE" ] || [ "$SUB_API_KEY" != "$API_KEY" ]; then
        SUB_USES_PRESET="true"
    fi
fi

# telegram
TG_ENABLED="false"; TG_TOKEN=""; TG_FROM=""
printf '\n' >&3
printf '\033[1m── Channel ────────────────────────────────────────\033[0m\n' >&3
ask "Use Telegram (otherwise TUI mode)?" "N"
yes_no "$REPLY"
if [ "$REPLY" = "y" ]; then
    TG_ENABLED="true"
    ask_required "Telegram bot token (from @BotFather)"
    TG_TOKEN="$(json_sanitize "$REPLY")"
    while :; do
        ask_required "Allowed-from Telegram user ID (numeric; @userinfobot shows yours)"
        TG_FROM="$(json_sanitize "$REPLY")"
        if printf '%s' "$TG_FROM" | grep -Eq '^-?[0-9]+$'; then break; fi
        printf '  must be numeric (e.g. 8113382039; groups may be negative)\n' >&3
    done
fi

# ── 7. config.json ──────────────────────────────────────────────────────────
mkdir -p "$GINO_HOME/workspace"
CONFIG="${GINO_HOME}/config.json"

if [ -f "$CONFIG" ]; then
    printf '\n' >&3
    ask "config.json already exists at $CONFIG — overwrite?" "N"
    yes_no "$REPLY"
    if [ "$REPLY" != "y" ]; then
        log "keeping existing config at $CONFIG"
        CONFIG_ACTION="kept"
    else
        cp "$CONFIG" "${CONFIG}.bak.$(date +%Y%m%d%H%M%S)"
        CONFIG_ACTION="replaced"
    fi
else
    CONFIG_ACTION="new"
fi

if [ "${CONFIG_ACTION:-new}" != "kept" ]; then
    log "writing $CONFIG"

    # subagent JSON fragments
    SPAWN_AGENTS_JSON="[]"
    PRESETS_JSON=""
    if [ "$SUB_ENABLED" = "true" ]; then
        PROVIDER_FIELD=""
        if [ "$SUB_USES_PRESET" = "true" ]; then
            PROVIDER_FIELD="\"provider\": \"subagent-provider\","
            PRESETS_JSON=$(cat <<EOF
        "presets": {
            "subagent-provider": {
                "apiBase": "${SUB_API_BASE}",
                "apiKey": "${SUB_API_KEY}",
                "model": "${SUB_MODEL}"
            }
        },
EOF
)
        fi
        SPAWN_AGENTS_JSON=$(cat <<EOF | sed '/^[[:space:]]*$/d'
[
                    {
                        "name": "${SUB_NAME}",
                        "description": "Delegated subagent for self-contained tasks (research, summaries, lookups). Use it to keep the main conversation small.",
                        ${PROVIDER_FIELD}
                        "model": "${SUB_MODEL}"
                    }
                ]
EOF
)
    fi

    TELEGRAM_JSON='        "telegram": { "enabled": false }'
    if [ "$TG_ENABLED" = "true" ]; then
        TELEGRAM_JSON=$(cat <<EOF
        "telegram": {
            "enabled": true,
            "token": "${TG_TOKEN}",
            "allowFrom": ["${TG_FROM}"]
        }
EOF
)
    fi

    cat > "$CONFIG" <<EOF
{
    "agents": {
        "defaults": {
            "workspace": "${GINO_HOME}/workspace",
            "model": "${MODEL}",
            "maxTokens": 16384,
            "temperature": 0.7,
            "maxToolIterations": 50,
            "heartbeatIntervalS": 30,
            "requestTimeoutS": 300,
            "enableToolActivityIndicator": true,
            "enableToolErrorMessages": true,
            "sandbox": {
                "mode": "yolo"
            },
            "spawn": {
                "enabled": ${SUB_ENABLED},
                "defaultTimeoutS": 300,
                "agents": ${SPAWN_AGENTS_JSON}
            }
        }
    },
    "providers": {
        "openai": {
            "apiBase": "${API_BASE}",
            "apiKey": "${API_KEY}"
        },
${PRESETS_JSON}        "fallbacks": []
    },
    "channels": {
${TELEGRAM_JSON}
    },
    "brain": {
        "enabled": true,
        "embeddingModel": "${EMBED_MODEL}",
        "embeddingDims": 768,
        "ollamaBaseURL": "${OLLAMA_URL}"
    }
}
EOF

    # best-effort JSON validation + format normalization when python3 is present
    if command -v python3 >/dev/null 2>&1; then
        if ! python3 -m json.tool "$CONFIG" > "${CONFIG}.tmp"; then
            rm -f "${CONFIG}.tmp"
            die "generated config failed JSON validation — inspect $CONFIG"
        fi
        mv "${CONFIG}.tmp" "$CONFIG"
        log "config JSON validated + normalized"
    fi
    chmod 600 "$CONFIG"
fi

# ── 8. gateway service (Telegram mode) ──────────────────────────────────────
if [ "$TEST" != "1" ] && [ "$TG_ENABLED" = "true" ]; then
    log "installing gino-gateway systemd service"
    cat > "${UNIT_DIR}/gino-gateway.service" <<EOF
[Unit]
Description=Gino gateway (Telegram)
After=network-online.target gino-ollama.service
Wants=network-online.target
Wants=gino-ollama.service

[Service]
ExecStart=${BIN_DIR}/gino gateway
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable --now gino-gateway >/dev/null 2>&1 || systemctl restart gino-gateway
    log "gateway started"
fi

# ── 9. summary ──────────────────────────────────────────────────────────────
printf '\n' >&3
printf '\033[1m── Install complete ───────────────────────────────\033[0m\n' >&3
{
    printf '  binary     : %s/gino\n' "$BIN_DIR"
    printf '  repo       : %s\n' "$REPO_DIR"
    printf '  config     : %s (%s)\n' "$CONFIG" "${CONFIG_ACTION:-new}"
    printf '  sandbox    : yolo\n'
    printf '  brain      : enabled (%s @ %s)\n' "$EMBED_MODEL" "$OLLAMA_URL"
    if [ "$TG_ENABLED" = "true" ]; then
        printf '  channel    : Telegram (gateway service running)\n'
        printf '\n  logs       : journalctl -u gino-gateway -f\n'
    else
        printf '  channel    : TUI\n'
        printf '\n  start      : gino chat\n'
    fi
    if [ "$TEST" = "1" ]; then printf '  mode       : TEST (packages/podman/systemd skipped)\n'; fi
} >&3
printf '\n' >&3
