#!/bin/bash
set -e

# Runs as root to fix volume permissions, then execs the real entrypoint as picobot.

PICOBOT_HOME="${PICOBOT_HOME:-/home/picobot/.picobot}"

# Ensure required directories exist and are owned by picobot
mkdir -p "${PICOBOT_HOME}/.ollama/models"
mkdir -p "${PICOBOT_HOME}/workspace/memory"
mkdir -p "${PICOBOT_HOME}/workspace/skills"
chown -R picobot:picobot "${PICOBOT_HOME}"

# Drop to picobot user and run the real entrypoint
exec gosu picobot /entrypoint.sh "$@"
