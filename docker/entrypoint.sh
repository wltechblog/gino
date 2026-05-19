#!/bin/bash
set -e

PICOBOT_HOME="${PICOBOT_HOME:-/home/picobot/.picobot}"
CONFIG="${PICOBOT_HOME}/config.json"

# Start Ollama in the background if binary is present and brain is enabled
if command -v ollama &>/dev/null; then
    BRAIN_ENABLED="${PICOBOT_BRAIN_ENABLED:-false}"
    if [ "$BRAIN_ENABLED" = "true" ] || [ "$BRAIN_ENABLED" = "1" ]; then
        echo "Starting Ollama server..."
        export LD_LIBRARY_PATH="/lib/ollama${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
        OLLAMA_MODELS="${PICOBOT_HOME}/.ollama/models" ollama serve &>/tmp/ollama.log &
        OLLAMA_PID=$!

        # Wait for Ollama to be ready
        READY=false
        for i in $(seq 1 30); do
            if curl -s http://localhost:11434/api/tags >/dev/null 2>&1; then
                echo "Ollama ready (pid $OLLAMA_PID)."
                READY=true
                break
            fi
            # Check if ollama died
            if ! kill -0 $OLLAMA_PID 2>/dev/null; then
                echo "ERROR: Ollama crashed. Last log lines:"
                cat /tmp/ollama.log 2>/dev/null
                exit 1
            fi
            sleep 1
        done

        if [ "$READY" = "false" ]; then
            echo "ERROR: Ollama did not start within 30s. Log:"
            cat /tmp/ollama.log 2>/dev/null
            exit 1
        fi

        # Auto-pull embedding model if not present
        MODEL="${PICOBOT_BRAIN_EMBEDDING_MODEL:-nomic-embed-text}"
        if ! ollama list 2>/dev/null | grep -q "$MODEL"; then
            echo "Pulling embedding model: $MODEL"
            ollama pull "$MODEL"
            echo "Model ready."
        fi
    fi
fi

# Auto-onboard if config doesn't exist yet
if [ ! -f "${CONFIG}" ]; then
  echo "First run detected — running onboard..."
  picobot onboard
  echo "✅ Onboard complete. Config at ${CONFIG}"
  echo ""
  echo "⚠️  You need to configure your API key and model."
  echo "   Mount a config file or set environment variables."
  echo ""
fi

# Allow overriding config values via environment variables
if [ -n "${OPENAI_API_KEY}" ]; then
  echo "Applying OPENAI_API_KEY from environment..."
  TMP=$(mktemp)
  jq --arg key "${OPENAI_API_KEY}" '.providers.openai.apiKey = $key' "${CONFIG}" > "$TMP" && mv "$TMP" "${CONFIG}"
fi

if [ -n "${OPENAI_API_BASE}" ]; then
  echo "Applying OPENAI_API_BASE from environment..."
  TMP=$(mktemp)
  jq --arg base "${OPENAI_API_BASE}" '.providers.openai.apiBase = $base' "${CONFIG}" > "$TMP" && mv "$TMP" "${CONFIG}"
fi

if [ -n "${TELEGRAM_BOT_TOKEN}" ]; then
  echo "Applying TELEGRAM_BOT_TOKEN from environment..."
  TMP=$(mktemp)
  jq --arg token "${TELEGRAM_BOT_TOKEN}" '.channels.telegram.enabled = true | .channels.telegram.token = $token' "${CONFIG}" > "$TMP" && mv "$TMP" "${CONFIG}"
fi

if [ -n "${TELEGRAM_ALLOW_FROM}" ]; then
  echo "Applying TELEGRAM_ALLOW_FROM from environment..."
  ALLOW_JSON=$(echo "${TELEGRAM_ALLOW_FROM}" | jq -R 'split(",")')
  TMP=$(mktemp)
  jq --argjson allow "${ALLOW_JSON}" '.channels.telegram.allowFrom = $allow' "${CONFIG}" > "$TMP" && mv "$TMP" "${CONFIG}"
fi

if [ -n "${DISCORD_BOT_TOKEN}" ]; then
  echo "Applying DISCORD_BOT_TOKEN from environment..."
  TMP=$(mktemp)
  jq --arg token "${DISCORD_BOT_TOKEN}" '.channels.discord.enabled = true | .channels.discord.token = $token' "${CONFIG}" > "$TMP" && mv "$TMP" "${CONFIG}"
fi

if [ -n "${DISCORD_ALLOW_FROM}" ]; then
  echo "Applying DISCORD_ALLOW_FROM from environment..."
  ALLOW_JSON=$(echo "${DISCORD_ALLOW_FROM}" | jq -R 'split(",")')
  TMP=$(mktemp)
  jq --argjson allow "${ALLOW_JSON}" '.channels.discord.allowFrom = $allow' "${CONFIG}" > "$TMP" && mv "$TMP" "${CONFIG}"
fi

if [ -n "${SLACK_APP_TOKEN}" ]; then
  echo "Applying SLACK_APP_TOKEN from environment..."
  TMP=$(mktemp)
  jq --arg token "${SLACK_APP_TOKEN}" '.channels.slack.enabled = true | .channels.slack.appToken = $token' "${CONFIG}" >"$TMP" && mv "$TMP" "${CONFIG}"
fi

if [ -n "${SLACK_BOT_TOKEN}" ]; then
  echo "Applying SLACK_BOT_TOKEN from environment..."
  TMP=$(mktemp)
  jq --arg token "${SLACK_BOT_TOKEN}" '.channels.slack.enabled = true | .channels.slack.botToken = $token' "${CONFIG}" >"$TMP" && mv "$TMP" "${CONFIG}"
fi

if [ -n "${SLACK_ALLOW_USERS}" ]; then
  echo "Applying SLACK_ALLOW_USERS from environment..."
  ALLOW_JSON=$(echo "${SLACK_ALLOW_USERS}" | jq -R 'split(",")')
  TMP=$(mktemp)
  jq --argjson allow "${ALLOW_JSON}" '.channels.slack.allowUsers = $allow' "${CONFIG}" >"$TMP" && mv "$TMP" "${CONFIG}"
fi

if [ -n "${SLACK_ALLOW_CHANNELS}" ]; then
  echo "Applying SLACK_ALLOW_CHANNELS from environment..."
  ALLOW_JSON=$(echo "${SLACK_ALLOW_CHANNELS}" | jq -R 'split(",")')
  TMP=$(mktemp)
  jq --argjson allow "${ALLOW_JSON}" '.channels.slack.allowChannels = $allow' "${CONFIG}" >"$TMP" && mv "$TMP" "${CONFIG}"
fi

if [ -n "${PICOBOT_MODEL}" ]; then
  echo "Applying PICOBOT_MODEL from environment..."
  TMP=$(mktemp)
  jq --arg model "${PICOBOT_MODEL}" '.agents.defaults.model = $model' "${CONFIG}" > "$TMP" && mv "$TMP" "${CONFIG}"
fi

if [ -n "${PICOBOT_MAX_TOKENS}" ]; then
  echo "Applying PICOBOT_MAX_TOKENS from environment..."
  TMP=$(mktemp)
  jq --argjson tokens "${PICOBOT_MAX_TOKENS}" '.agents.defaults.maxTokens = $tokens' "${CONFIG}" > "$TMP" && mv "$TMP" "${CONFIG}"
fi

if [ -n "${PICOBOT_MAX_TOOL_ITERATIONS}" ]; then
  echo "Applying PICOBOT_MAX_TOOL_ITERATIONS from environment..."
  TMP=$(mktemp)
  jq --argjson iter "${PICOBOT_MAX_TOOL_ITERATIONS}" '.agents.defaults.maxToolIterations = $iter' "${CONFIG}" > "$TMP" && mv "$TMP" "${CONFIG}"
fi

if [ -n "${PICOBOT_ENABLE_TOOL_ACTIVITY_INDICATOR}" ]; then
  echo "Applying PICOBOT_ENABLE_TOOL_ACTIVITY_INDICATOR from environment..."
  TMP=$(mktemp)
  VAL=$(echo "${PICOBOT_ENABLE_TOOL_ACTIVITY_INDICATOR}" | tr '[:upper:]' '[:lower:]')
  if [ "$VAL" = "false" ] || [ "$VAL" = "0" ]; then
    jq '.agents.defaults.enableToolActivityIndicator = false' "${CONFIG}" > "$TMP" && mv "$TMP" "${CONFIG}"
  else
    jq '.agents.defaults.enableToolActivityIndicator = true' "${CONFIG}" > "$TMP" && mv "$TMP" "${CONFIG}"
  fi
fi

# Brain / knowledge system
if [ -n "${PICOBOT_BRAIN_ENABLED}" ]; then
  echo "Applying PICOBOT_BRAIN_ENABLED from environment..."
  TMP=$(mktemp)
  VAL=$(echo "${PICOBOT_BRAIN_ENABLED}" | tr '[:upper:]' '[:lower:]')
  if [ "$VAL" = "true" ] || [ "$VAL" = "1" ]; then
    jq '.brain.enabled = true' "${CONFIG}" > "$TMP" && mv "$TMP" "${CONFIG}"
  fi
fi

if [ -n "${PICOBOT_BRAIN_EMBEDDING_MODEL}" ]; then
  echo "Applying PICOBOT_BRAIN_EMBEDDING_MODEL from environment..."
  TMP=$(mktemp)
  jq --arg model "${PICOBOT_BRAIN_EMBEDDING_MODEL}" '.brain.embeddingModel = $model' "${CONFIG}" > "$TMP" && mv "$TMP" "${CONFIG}"
fi

if [ -n "${PICOBOT_BRAIN_OLLAMA_URL}" ]; then
  echo "Applying PICOBOT_BRAIN_OLLAMA_URL from environment..."
  TMP=$(mktemp)
  jq --arg url "${PICOBOT_BRAIN_OLLAMA_URL}" '.brain.ollamaUrl = $url' "${CONFIG}" > "$TMP" && mv "$TMP" "${CONFIG}"
fi

if [ -n "${PICOBOT_BRAIN_REMOTE_API_BASE}" ]; then
  echo "Applying PICOBOT_BRAIN_REMOTE_API_BASE from environment..."
  TMP=$(mktemp)
  jq --arg base "${PICOBOT_BRAIN_REMOTE_API_BASE}" '.brain.remoteApiBase = $base' "${CONFIG}" > "$TMP" && mv "$TMP" "${CONFIG}"
fi

if [ -n "${PICOBOT_BRAIN_REMOTE_API_KEY}" ]; then
  echo "Applying PICOBOT_BRAIN_REMOTE_API_KEY from environment..."
  TMP=$(mktemp)
  jq --arg key "${PICOBOT_BRAIN_REMOTE_API_KEY}" '.brain.remoteApiKey = $key' "${CONFIG}" > "$TMP" && mv "$TMP" "${CONFIG}"
fi

if [ -n "${PICOBOT_BRAIN_REMOTE_MODEL}" ]; then
  echo "Applying PICOBOT_BRAIN_REMOTE_MODEL from environment..."
  TMP=$(mktemp)
  jq --arg model "${PICOBOT_BRAIN_REMOTE_MODEL}" '.brain.remoteModel = $model' "${CONFIG}" > "$TMP" && mv "$TMP" "${CONFIG}"
fi

echo "Starting picobot $@..."
exec picobot "$@"
