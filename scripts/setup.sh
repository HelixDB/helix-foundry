#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
if [ ! -f .env ]; then
  umask 077
  key=$(openssl rand -hex 32)
  internal=$(openssl rand -hex 32)
  cat > .env <<ENV
ENCRYPTION_KEY=$key
INTERNAL_KEY=$internal
APP_ORIGIN=http://localhost:5173
HELIX_URL=http://127.0.0.1:6969
EXECUTOR_URL=http://127.0.0.1:3002
OLLAMA_URL=http://127.0.0.1:11434
DATA_DIR=.data
ENV
  echo 'Generated local secrets in .env'
fi
if [ "${1:-}" = "--compose" ]; then docker compose --profile local up -d --build; fi
