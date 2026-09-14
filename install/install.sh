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
#   - an Ollama container (podman or docker) for brain embeddings (localhost only,
#     skipped gracefully when no container runtime is available)
#   - Gino, cloned from GitHub and built from source
#   - config.json with sandbox.mode=yolo and the brain enabled (advanced or basic)
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
# LLM provider presets live in section 6 (provider menu); each preset
# carries its own apiBase, default models, and reasoningLevels vocabulary.

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

# container runtime detection: prefer podman, fall back to docker, else none
# (skips the Ollama container gracefully rather than hard-faulting)
CONTAINER_RT=""
CRT_BIN=""
detect_container_runtime() {
    # Candidate binary locations beyond sudo's secure_path (snap, /usr/local,
    # docker-ce static installs) plus whatever `command -v` finds.
    local cands
    cands="$( { command -v podman docker 2>/dev/null || true; } ) /snap/bin/docker /snap/bin/podman /usr/local/bin/docker /usr/local/bin/podman /opt/docker/bin/docker /usr/bin/docker /usr/bin/podman"
    for c in $cands; do
        [ -x "$c" ] || continue
        if "${c}" info >/dev/null 2>&1 || "${c}" version >/dev/null 2>&1; then
            if basename "$(readlink -f "$c")" | grep -q '^podman'; then
                CONTAINER_RT="podman"
            else
                CONTAINER_RT="docker"
            fi
            CRT_BIN="$c"
            return 0
        fi
    done
    # daemon present but CLI missing/unusable: warn, caller decides
    if [ -e /var/run/docker.sock ]; then
        warn "docker daemon socket exists (/var/run/docker.sock) but no usable docker CLI was found — install the docker CLI or add its directory to PATH"
    fi
    return 1
}

# ── 1. base packages (deb-based assumption) ─────────────────────────────────
if [ "$TEST" != "1" ]; then
    command -v apt-get >/dev/null 2>&1 || die "apt-get not found — this installer targets Debian-based systems"
    export DEBIAN_FRONTEND=noninteractive
    log "updating package lists"
    apt-get update -y </dev/null >/dev/null
    # probe for an existing runtime FIRST: never install podman over an
    # existing docker (or vice versa) — respect what the operator already has
    RUNTIME_PKGS="git curl ca-certificates"
    if detect_container_runtime; then
        log "using existing container runtime: ${CONTAINER_RT} (${CRT_BIN})"
    elif apt-get install -y --no-install-recommends podman </dev/null >/dev/null 2>&1; then
        RUNTIME_PKGS="$RUNTIME_PKGS podman"
        log "installed podman as the container runtime"
    elif apt-get install -y --no-install-recommends docker.io </dev/null >/dev/null 2>&1; then
        RUNTIME_PKGS="$RUNTIME_PKGS docker.io"
        log "installed docker.io as the container runtime"
    else
        log "no container runtime installable — the Ollama brain container will be skipped if requested"
    fi
    log "installing base packages (${RUNTIME_PKGS})"
    apt-get install -y $RUNTIME_PKGS </dev/null >/dev/null
    detect_container_runtime || true
fi
detect_container_runtime || true

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

# ── 5. questions (all via /dev/tty) ─────────────────────────────────────────
printf '\n' >&3
printf '\033[1m── Provider ──────────────────────────────────────\033[0m\n' >&3

# provider presets: base URL / default model / default subagent model /
# reasoningLevels vocabulary (see docs/CONFIG.md "Reasoning models").
provider_menu() {
    printf 'Select LLM provider:\n' >&3
    printf '  1) z.ai GLM — Coding Plan      (api.z.ai/api/coding/paas/v4)\n' >&3
    printf '  2) z.ai GLM — Direct API       (api.z.ai/api/paas/v4)\n' >&3
    printf '  3) OpenAI                      (api.openai.com/v1)\n' >&3
    printf '  4) OpenRouter                  (openrouter.ai/api/v1)\n' >&3
    printf '  5) Ollama — local              (127.0.0.1:11434, no key needed)\n' >&3
    printf '  6) Custom — any OpenAI-compatible endpoint\n' >&3
}
provider_menu
API_BASE=""; MODEL=""; SUB_MODEL_DEFAULT=""; REASONING_LEVELS=""
API_KEY_DEFAULT=""
while :; do
    ask "Choice" "1"
    case "$REPLY" in
        1)  API_BASE="https://api.z.ai/api/coding/paas/v4"
            MODEL="glm-5.3-flash"; SUB_MODEL_DEFAULT="glm-5.3-flash"
            # coding endpoint accepts the broad vocabulary and remaps per model
            REASONING_LEVELS='["none", "minimal", "low", "medium", "high", "xhigh", "max"]'
            ;;
        2)  API_BASE="https://api.z.ai/api/paas/v4"
            MODEL="glm-5.3"; SUB_MODEL_DEFAULT="glm-5.3-flash"
            # direct API + GLM-5.3: only these three; anything else hard-errors
            REASONING_LEVELS='["low", "high", "max"]'
            ;;
        3)  API_BASE="https://api.openai.com/v1"
            MODEL="gpt-5"; SUB_MODEL_DEFAULT="gpt-5-mini"
            REASONING_LEVELS='["minimal", "low", "medium", "high"]'
            ;;
        4)  API_BASE="https://openrouter.ai/api/v1"
            MODEL="glm-5.2"; SUB_MODEL_DEFAULT="glm-4.5-air"
            REASONING_LEVELS='["none", "minimal", "low", "medium", "high", "xhigh", "max"]'
            ;;
        5)  API_BASE="http://127.0.0.1:11434/v1"
            MODEL="qwen3:8b"; SUB_MODEL_DEFAULT="qwen3:4b"
            REASONING_LEVELS='["none"]'
            API_KEY_DEFAULT="ollama"   # local endpoint ignores the key
            ;;
        6)  ;;
        *)  printf '  enter a number 1-6\n' >&3; continue ;;
    esac
    break
done
PROVIDER_CHOICE="$REPLY"

ask_required "LLM provider API base URL" "$API_BASE"
API_BASE="$(json_sanitize "$REPLY")"

if [ "$PROVIDER_CHOICE" = "5" ]; then
    ask_secret "API key (any value for local endpoints)" "ollama"
    API_KEY="$(json_sanitize "$REPLY")"
else
    while :; do
        ask_secret "API key"
        [ -n "$REPLY" ] && break
        printf '  a value is required\n' >&3
    done
    API_KEY="$(json_sanitize "$REPLY")"
fi

ask_required "Model name" "$MODEL"
MODEL="$(json_sanitize "$REPLY")"

# vision: if the chosen model accepts image input, offer it as the vision
# model (agents.defaults.visionModel — powers the vision analysis tool).
VISION_MODEL=""
printf '\n' >&3
ask "Does ${MODEL} support image input (vision)?" "N"
yes_no "$REPLY"
if [ "$REPLY" = "y" ]; then
    ask "Configure it as the vision model for image analysis?" "Y"
    yes_no "$REPLY"
    if [ "$REPLY" = "y" ]; then
        VISION_MODEL="$MODEL"
        log "vision model: ${MODEL}"
    fi
fi

# logging: default OFF (clean installs stay quiet — routine tool chatter,
# heartbeats and MCP logs hidden; fatal startup errors still print).
# GINO_LOG_LEVEL=info (or editing config.json) re-enables logging.
DEBUG_LOGS="off"
ask "Enable debug logging (verbose runtime logs)?" "N"
yes_no "$REPLY"
if [ "$REPLY" = "y" ]; then
    DEBUG_LOGS="info"
    log "debug logging enabled"
fi

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

    ask_required "Subagent model" "${SUB_MODEL_DEFAULT:-$MODEL}"
    SUB_MODEL="$(json_sanitize "$REPLY")"

    if [ "$SUB_API_BASE" != "$API_BASE" ] || [ "$SUB_API_KEY" != "$API_KEY" ]; then
        SUB_USES_PRESET="true"
    fi
fi

# memory brain (advanced vs basic)
BRAIN_ADVANCED="true"
printf '\n' >&3
printf '\033[1m── Memory brain ──────────────────────────────────\033[0m\n' >&3
# container-runtime + low-RAM heads-up before the brain question
detect_container_runtime || true
MEM_KB="$(awk '/^MemTotal:/{print $2; exit}' /proc/meminfo 2>/dev/null || true)"
if [ -n "$MEM_KB" ] && [ "$MEM_KB" -gt 0 ] && [ "$MEM_KB" -lt $((2 * 1024 * 1024)) ]; then
    warn "this device has ~$((MEM_KB / 1024)) MB RAM — local Ollama probably won't work suitably here; the basic brain is recommended"
fi
if [ -z "$CONTAINER_RT" ] && [ "$TEST" != "1" ]; then
    warn "no container runtime (podman/docker) found or installable — the advanced brain needs one; the basic brain will be used"
    BRAIN_ADVANCED="false"
    log "no container runtime available — basic brain selected, keyword search only"
else
    ask "Install local Ollama for the advanced memory brain? (yes = advanced brain, no = basic brain)" "Y"
    yes_no "$REPLY"
    BRAIN_ADVANCED="$REPLY"
    [ "$BRAIN_ADVANCED" = "y" ] && BRAIN_ADVANCED="true" || BRAIN_ADVANCED="false"
    if [ "$BRAIN_ADVANCED" = "false" ]; then
        log "basic brain selected — keyword search only, no Ollama container"
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

# ── 6. Ollama container (advanced brain / Ollama-LLM only) ─────────────────
ollama_up() { curl -sf "${OLLAMA_URL}/api/version" >/dev/null 2>&1; }

write_ollama_unit() {
    [ -n "$CRT_BIN" ] || CRT_BIN="$(command -v "$CONTAINER_RT" || true)"
    [ -n "$CRT_BIN" ] || die "cannot resolve container runtime binary for systemd unit"
    cat > "${UNIT_DIR}/gino-ollama.service" <<EOF
[Unit]
Description=Gino Ollama (brain embeddings)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=${CRT_BIN} start -a ${OLLAMA_NAME}
ExecStop=${CRT_BIN} stop -t 10 ${OLLAMA_NAME}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
}

OLLAMA_MODE="none"   # none | existing | installed
detect_container_runtime || true   # runtime may have been installed by section 1
if [ -n "$CONTAINER_RT" ]; then
    log "container runtime detected: ${CONTAINER_RT} (${CRT_BIN})"
fi
if [ "$TEST" != "1" ]; then
    if [ "$BRAIN_ADVANCED" = "false" ] && [ "$PROVIDER_CHOICE" != "5" ]; then
        if ollama_up; then
            log "note: an existing server answers on ${OLLAMA_URL} — the brain will use it automatically if reachable at runtime"
        fi
    elif ollama_up; then
        log "something already answers on ${OLLAMA_URL} — reusing it for the brain, skipping container setup"
        OLLAMA_MODE="existing"
    elif [ "$PROVIDER_CHOICE" = "5" ] && [ -z "$CONTAINER_RT" ]; then
        # Ollama-LLM without any container runtime: cannot proceed with local LLM
        die "Ollama selected as LLM provider but no container runtime (podman/docker) is available — install one or choose a different provider"
    elif [ "$PROVIDER_CHOICE" = "5" ]; then
        # Ollama selected as the LLM provider: the container is required
        log "Ollama LLM mode requires the local container — installing it"
        BRAIN_ADVANCED="true"
    fi
    if [ "$BRAIN_ADVANCED" = "true" ] && [ "$OLLAMA_MODE" != "existing" ]; then
        [ -n "$CONTAINER_RT" ] || die "advanced brain selected but no container runtime (podman/docker) available"
        mkdir -p "$OLLAMA_DATA"
        if ! "$CRT_BIN" container exists "$OLLAMA_NAME" 2>/dev/null \
           && ! "$CRT_BIN" ps -a --format '{{.Names}}' 2>/dev/null | grep -qx "$OLLAMA_NAME"; then
            log "creating Ollama container (localhost-only port) via ${CONTAINER_RT}"
            retry 3 "container create" "$CRT_BIN" create \
                --name "$OLLAMA_NAME" \
                -v "${OLLAMA_DATA}:/root/.ollama:Z" \
                -p 127.0.0.1:11434:11434 \
                "$OLLAMA_IMAGE" >&2
        else
            log "Ollama container '$OLLAMA_NAME' already exists — reusing"
        fi

        log "pulling Ollama image (may take a while)"
        retry 3 "image pull" "$CRT_BIN" pull "$OLLAMA_IMAGE" >&2

        if command -v systemctl >/dev/null 2>&1; then
            write_ollama_unit
            systemctl daemon-reload
            log "starting gino-ollama service"
            systemctl enable --now gino-ollama >/dev/null 2>&1 || systemctl restart gino-ollama
        else
            warn "systemd not available — starting container directly (no boot persistence)"
            "$CRT_BIN" start "$OLLAMA_NAME" >&2
        fi

        log "waiting for Ollama API"
        waited=0
        until ollama_up; do
            sleep 2; waited=$((waited + 2))
            [ "$waited" -ge 120 ] && die "Ollama did not come up within 120s — check: ${CRT_BIN} logs $OLLAMA_NAME"
        done

        log "pulling embedding model '${EMBED_MODEL}'"
        retry 3 "model pull" "$CRT_BIN" exec "$OLLAMA_NAME" ollama pull "$EMBED_MODEL" >&2
        OLLAMA_MODE="installed"
    fi
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

    # brain fragment: advanced mode points at local Ollama; basic mode omits
    # the URL so the runtime degrades to keyword search (and auto-upgrades
    # if Ollama appears later)
    BRAIN_CFG_EXTRA=""
    if [ "$BRAIN_ADVANCED" = "true" ]; then
        BRAIN_CFG_EXTRA=",
        \"ollamaBaseURL\": \"${OLLAMA_URL}\""
    fi

    # reasoning vocabulary fragment (empty = omit reasoningLevels)
    PROVIDER_EXTRA=""
    if [ -n "$REASONING_LEVELS" ]; then
        PROVIDER_EXTRA=",
            \"reasoningLevels\": ${REASONING_LEVELS}"
    fi

    # subagent JSON fragments
    VISION_EXTRA=""
    if [ -n "$VISION_MODEL" ]; then
        VISION_EXTRA='
            "visionModel": "'"${VISION_MODEL}"'",'
    fi
    LOG_EXTRA=""
    if [ -n "$DEBUG_LOGS" ]; then
        LOG_EXTRA='
            "logLevel": "'"${DEBUG_LOGS}"'",'
    fi
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
            "enableToolErrorMessages": true,${VISION_EXTRA}${LOG_EXTRA}
            "sandbox": {
                "mode": "yolo",
                "allowStringCommands": true
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
            "apiKey": "${API_KEY}"${PROVIDER_EXTRA}
        },
${PRESETS_JSON}        "fallbacks": []
    },
    "channels": {
${TELEGRAM_JSON}
    },
    "brain": {
        "enabled": true,
        "embeddingModel": "${EMBED_MODEL}",
        "embeddingDims": 768${BRAIN_CFG_EXTRA}
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

if [ "$PROVIDER_CHOICE" = "5" ] && [ "${CONFIG_ACTION:-new}" != "kept" ]; then
    warn "Ollama LLM mode: pull your model first — ${CONTAINER_RT:-podman} exec $OLLAMA_NAME ollama pull $MODEL"
fi

# ── 8. gateway service (Telegram mode) ──────────────────────────────────────
# The installer owns the gino-gateway unit file: refresh it whenever it
# already exists (a re-run that answers "N" to Telegram must still update
# the unit of an existing install), and only install it fresh when the
# user opts into Telegram this run.
GATEWAY_UNIT_INSTALLED=0
if [ -f "${UNIT_DIR}/gino-gateway.service" ] || { [ "$TEST" != "1" ] && [ "$TG_ENABLED" = "true" ]; }; then
    GATEWAY_UNIT_INSTALLED=1
    if [ "$TEST" != "1" ]; then
        log "writing gino-gateway systemd service"
    else
        log "TEST mode: gino-gateway unit would be written here (systemd skipped)"
    fi
    GATEWAY_OLLAMA_DEPS=""
    if [ "$OLLAMA_MODE" = "installed" ] || [ "$OLLAMA_MODE" = "existing" ]; then
        GATEWAY_OLLAMA_DEPS=" gino-ollama.service"
    fi
    # systemd services run with $HOME unset; pin it (plus -home on the
    # ExecStart) so the gateway never resolves a relative home dir.
    HOME_PARENT="${GINO_HOME%/*}"
    UNIT_HOME_ENV=""
    if [ -n "$HOME_PARENT" ] && [ "$HOME_PARENT" != "$GINO_HOME" ]; then
        UNIT_HOME_ENV="Environment=HOME=${HOME_PARENT}
"
    fi
    cat > "${UNIT_DIR}/gino-gateway.service" <<EOF
[Unit]
Description=Gino gateway (Telegram)
After=network-online.target
Wants=network-online.target${GATEWAY_OLLAMA_DEPS}

[Service]
Type=simple
${UNIT_HOME_ENV}ExecStart=${BIN_DIR}/gino gateway -home ${GINO_HOME}
WorkingDirectory=${GINO_HOME}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
    if [ "$TEST" != "1" ]; then
        systemctl daemon-reload
        # enable --now alone won't restart an already-running service onto a
        # changed unit — restart explicitly so the new unit takes effect.
        systemctl enable gino-gateway >/dev/null 2>&1 || true
        systemctl restart gino-gateway
        log "gateway (re)started with updated unit"
    fi
fi

# ── 9. summary ──────────────────────────────────────────────────────────────
printf '\n' >&3
printf '\033[1m── Install complete ───────────────────────────────\033[0m\n' >&3
{
    printf '  binary     : %s/gino\n' "$BIN_DIR"
    printf '  provider   : %s\n' "$API_BASE"
    printf '  model      : %s\n' "$MODEL"
    [ -n "$VISION_MODEL" ] && printf '  vision     : %s\n' "$VISION_MODEL"
    if [ "$DEBUG_LOGS" = "off" ]; then
        printf '  logging    : off (GINO_LOG_LEVEL=info to enable)\n'
    else
        printf '  logging    : info\n'
    fi
    printf '  repo       : %s\n' "$REPO_DIR"
    printf '  config     : %s (%s)\n' "$CONFIG" "${CONFIG_ACTION:-new}"
    printf '  sandbox    : yolo (string commands on)\n'
    if [ "$BRAIN_ADVANCED" = "true" ]; then
        printf '  brain      : advanced (%s @ %s)\n' "$EMBED_MODEL" "$OLLAMA_URL"
    else
        printf '  brain      : basic (keyword search, no Ollama)\n'
    fi
    if [ "$TG_ENABLED" = "true" ]; then
        printf '  channel    : Telegram (gateway service running)\n'
        printf '\n  logs       : journalctl -u gino-gateway -f\n'
    else
        printf '  channel    : TUI\n'
        printf '\n  start      : gino chat\n'
    fi
    printf '\n  verify     : gino doctor\n'
    if [ "$TEST" = "1" ]; then printf '  mode       : TEST (packages/podman/systemd skipped)\n'; fi
} >&3
printf '\n' >&3
