#!/bin/bash
set -e

# ═══════════════════════════════════════════════════════
# entrypoint.sh — main Gino entrypoint (runs as gino user)
# Manages: Ollama (optional) + Gino agent
# ═══════════════════════════════════════════════════════

GINO_HOME="${GINO_HOME:-/home/gino/.gino}"
CONFIG="${GINO_HOME}/config.json"
OLLAMA_PID=""

# ── Signal handling ───────────────────────────────────
cleanup() {
    echo ""
    echo "Shutting down..."
    if [ -n "${OLLAMA_PID}" ] && kill -0 "${OLLAMA_PID}" 2>/dev/null; then
        kill "${OLLAMA_PID}" 2>/dev/null || true
        wait "${OLLAMA_PID}" 2>/dev/null || true
    fi
    exit 0
}
trap cleanup SIGTERM SIGINT

# ── Start Ollama (unless external URL provided) ───────
start_ollama() {
    # If user provided an external Ollama URL, skip bundled Ollama
    if [ -n "${OLLAMA_URL}" ]; then
        echo "ℹ️  Using external Ollama: ${OLLAMA_URL}"
        return 0
    fi

    if ! command -v ollama &>/dev/null; then
        echo "⚠️  Ollama not found in image — brain embeddings disabled unless OLLAMA_URL is set"
        return 0
    fi

    BRAIN_ENABLED="${GINO_BRAIN_ENABLED:-true}"
    if [ "$BRAIN_ENABLED" != "true" ] && [ "$BRAIN_ENABLED" != "1" ]; then
        echo "ℹ️  Brain disabled (GINO_BRAIN_ENABLED=false). Skipping Ollama."
        return 0
    fi

    echo "🦙 Starting bundled Ollama server..."
    export OLLAMA_MODELS="${GINO_HOME}/.ollama/models"
    export OLLAMA_HOST="127.0.0.1:11434"
    ollama serve &>/tmp/ollama.log &
    OLLAMA_PID=$!

    # Wait for Ollama to be ready (max 30s)
    READY=false
    for i in $(seq 1 30); do
        if curl -sf http://127.0.0.1:11434/api/tags >/dev/null 2>&1; then
            echo "✅ Ollama ready (pid ${OLLAMA_PID})"
            READY=true
            break
        fi
        if ! kill -0 "${OLLAMA_PID}" 2>/dev/null; then
            echo "❌ Ollama crashed. Log:"
            cat /tmp/ollama.log 2>/dev/null
            return 1
        fi
        sleep 1
    done

    if [ "$READY" = false ]; then
        echo "❌ Ollama did not start within 30s. Log:"
        cat /tmp/ollama.log 2>/dev/null
        return 1
    fi

    # Auto-pull embedding model
    MODEL="${GINO_BRAIN_EMBEDDING_MODEL:-nomic-embed-text}"
    if ! ollama list 2>/dev/null | grep -q "$MODEL"; then
        echo "📦 Pulling embedding model: ${MODEL}..."
        ollama pull "$MODEL" && echo "✅ Model ready" || echo "⚠️  Failed to pull model"
    fi
}

# ── Config bootstrap ──────────────────────────────────
init_config() {
    if [ ! -f "${CONFIG}" ]; then
        echo "🔧 First run — generating config..."
        gino onboard 2>/dev/null || true
    fi
}

# ── Apply env vars to config ──────────────────────────
apply_env() {
    [ ! -f "${CONFIG}" ] && return 0

    apply() {
        # $1 = jq filter, $2 = value type (str/json), $3 = value
        local filter="$1"; local vtype="$2"; local val="$3"
        local tmp
        tmp=$(mktemp)
        if [ "$vtype" = "json" ]; then
            jq "${filter} ${val}" "${CONFIG}" > "$tmp" || { rm -f "$tmp"; return 1; }
        else
            jq --arg v "${val}" "${filter}" "${CONFIG}" > "$tmp" || { rm -f "$tmp"; return 1; }
        fi
        mv "$tmp" "${CONFIG}"
    }

    # LLM Provider
    [ -n "${OPENAI_API_KEY}" ] && apply '.providers.openai.apiKey = $v' str "${OPENAI_API_KEY}"
    [ -n "${OPENAI_API_BASE}" ] && apply '.providers.openai.apiBase = $v' str "${OPENAI_API_BASE}"
    [ -n "${GINO_MODEL}" ] && apply '.agents.defaults.model = $v' str "${GINO_MODEL}"
    [ -n "${GINO_MAX_TOKENS}" ] && apply '.agents.defaults.maxTokens = $v' json "${GINO_MAX_TOKENS}"
    [ -n "${GINO_MAX_TOOL_ITERATIONS}" ] && apply '.agents.defaults.maxToolIterations = $v' json "${GINO_MAX_TOOL_ITERATIONS}"

    # Telegram
    if [ -n "${TELEGRAM_BOT_TOKEN}" ]; then
        apply '.channels.telegram.enabled = true' json "true"
        apply '.channels.telegram.token = $v' str "${TELEGRAM_BOT_TOKEN}"
    fi
    if [ -n "${TELEGRAM_ALLOW_FROM}" ]; then
        apply '.channels.telegram.allowFrom = $v' json "$(echo "${TELEGRAM_ALLOW_FROM}" | jq -R 'split(",")')"
    fi

    # Discord
    if [ -n "${DISCORD_BOT_TOKEN}" ]; then
        apply '.channels.discord.enabled = true' json "true"
        apply '.channels.discord.token = $v' str "${DISCORD_BOT_TOKEN}"
    fi
    if [ -n "${DISCORD_ALLOW_FROM}" ]; then
        apply '.channels.discord.allowFrom = $v' json "$(echo "${DISCORD_ALLOW_FROM}" | jq -R 'split(",")')"
    fi

    # Brain / Knowledge
    local brain_enabled="${GINO_BRAIN_ENABLED:-true}"
    if [ "$brain_enabled" = "true" ] || [ "$brain_enabled" = "1" ]; then
        apply '.brain.enabled = true' json "true"
    fi
    [ -n "${GINO_BRAIN_EMBEDDING_MODEL}" ] && apply '.brain.embeddingModel = $v' str "${GINO_BRAIN_EMBEDDING_MODEL}"

    # Ollama URL — use bundled if no external URL, unless OLLAMA_URL is set
    if [ -n "${OLLAMA_URL}" ]; then
        apply '.brain.ollamaUrl = $v' str "${OLLAMA_URL}"
    elif [ "$brain_enabled" = "true" ] || [ "$brain_enabled" = "1" ]; then
        apply '.brain.ollamaUrl = $v' str "http://127.0.0.1:11434"
    fi

    [ -n "${GINO_BRAIN_REMOTE_API_BASE}" ] && apply '.brain.remoteApiBase = $v' str "${GINO_BRAIN_REMOTE_API_BASE}"
    [ -n "${GINO_BRAIN_REMOTE_API_KEY}" ] && apply '.brain.remoteApiKey = $v' str "${GINO_BRAIN_REMOTE_API_KEY}"
    [ -n "${GINO_BRAIN_REMOTE_MODEL}" ] && apply '.brain.remoteModel = $v' str "${GINO_BRAIN_REMOTE_MODEL}"

    echo "✅ Config updated from environment variables"
}

# ── Main ──────────────────────────────────────────────
start_ollama
init_config
apply_env

echo ""
echo "🤖 Starting Gino: $@"
exec gino "$@"
