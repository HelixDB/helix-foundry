#!/bin/sh
# Writes .env next to compose.yaml with this installation's secrets, once,
# and asks once how Foundry's AI should run. Keep .env: FOUNDRY_CONNECTOR_KEY
# seals every stored database password and API key, so losing it makes them
# unreadable. Re-running never overwrites it. .env.example names every
# variable.
set -eu
cd "$(dirname "$0")"
if [ -e .env ]; then
  echo ".env exists; leaving it as it is."
  exit 0
fi
hex() { openssl rand -hex 32; }

# Ollama's model for a new install, and its digest when this was written: a
# candidate, until a real draft on a GPU picks the smallest that finishes one
# (README.md). About 5.7 GB of memory with drafting's 65536-token window.
OLLAMA_MODEL=qwen3.5:4b
OLLAMA_DIGEST=sha256:d8b0f5e9760cd1682034f292d7ef72ec46f432149be0df7574bf2d6e92e38c04
# Docker's memory below which Ollama in Docker and the rest of Foundry don't
# fit together; Docker Desktop reports a little over 10e9 for its "10 GB".
OLLAMA_MEMORY=10000000000
# Where Ollama on a Linux host listens for Docker and nothing else: Docker's
# bridge address, which host.docker.internal is there.
DOCKER_BRIDGE=172.17.0.1

# Asks for an API key without echoing it; $1 names it, $2 is one this shell
# has, offered first.
ask_key() {
  key=""
  if [ -n "$2" ]; then
    printf "Use the %s in this shell? [Y/n]: " "$1"
    read -r reuse
    case "${reuse:-y}" in [Yy]*) key=$2 ;; esac
  fi
  if [ -z "$key" ]; then
    printf "%s (not shown): " "$1"
    trap 'stty echo 2>/dev/null' EXIT
    stty -echo
    read -r key
    stty echo
    echo
  fi
  case $key in
    "" | *[!A-Za-z0-9_-]*)
      echo "That isn't an API key; nothing was written." >&2
      exit 1
      ;;
  esac
}

# The install's AI provider, written to .env below: Ollama in Docker (the
# compose file's ollama profile), Ollama on this computer, Claude or OpenAI
# with an API key (the ones in this shell, if any, are offered), an
# OpenAI-compatible server (LM Studio, vLLM, Ollama's /v1, a gateway) with
# its key if it takes one, Amazon Bedrock with a Bedrock API key (this
# shell's AWS_BEARER_TOKEN_BEDROCK and AWS_REGION, if any, are offered), or
# none until an owner chooses one in Settings › AI models. Without a
# terminal to ask on, none.
ai=""
if [ -t 0 ]; then
  default=1
  [ -n "${ANTHROPIC_API_KEY:-}" ] && default=3
  echo "How should Foundry's AI (drafting ontologies, reading documents, answering questions) run?"
  echo "  1) Ollama in Docker: a local model, downloaded on the first start ($OLLAMA_MODEL, 3.3 GB; Docker needs about 10 GB of memory)"
  echo "  2) Ollama on this computer: the Ollama you run yourself"
  echo "  3) Claude, with your Anthropic API key"
  echo "  4) OpenAI, with your OpenAI API key"
  echo "  5) An OpenAI-compatible server: LM Studio, vLLM, Ollama's /v1 or a gateway"
  echo "  6) Amazon Bedrock, with a Bedrock API key"
  echo "  7) Choose later in Settings › AI models"
  printf "Choice [%s]: " "$default"
  read -r choice
  case "${choice:-$default}" in
    1)
      ai=ollama-docker
      memory=$(docker info --format '{{.MemTotal}}' 2>/dev/null || echo 0)
      if [ "${memory:-0}" -gt 0 ] && [ "$memory" -lt "$OLLAMA_MEMORY" ]; then
        echo "Warning: Docker has $((memory / 1000000000)) GB of memory. Ollama with $OLLAMA_MODEL and the rest of Foundry need about 10 GB: when memory runs out, drafts and questions fail. Give Docker more memory, or run Ollama on this computer instead." >&2
      fi
      gpu=""
      if docker info --format '{{range $k, $v := .Runtimes}}{{$k}} {{end}}' 2>/dev/null | grep -qw nvidia; then
        printf "Docker can use NVIDIA GPUs here. Run Ollama on them? [Y/n]: "
        read -r answer
        case "${answer:-y}" in [Yy]*) gpu=1 ;; esac
      fi
      ;;
    2)
      ai=ollama-host
      printf "Ollama model [%s]: " "$OLLAMA_MODEL"
      read -r model
      model=${model:-$OLLAMA_MODEL}
      case $model in
        *[!A-Za-z0-9._:/-]*)
          echo "That isn't an Ollama model's name; nothing was written." >&2
          exit 1
          ;;
      esac
      # Ollama lists a model without a tag as :latest.
      case $model in *:*) listed=$model ;; *) listed=$model:latest ;; esac
      # On Linux, where it may already listen for Docker alone.
      hosts=127.0.0.1
      [ "$(uname -s)" = Linux ] && hosts="$hosts $DOCKER_BRIDGE"
      if command -v curl >/dev/null 2>&1; then
        tags=""
        for host in $hosts; do
          tags=$(curl -fsS --max-time 3 "http://$host:11434/api/tags" 2>/dev/null) && break
        done
        if [ -z "$tags" ]; then
          echo "Ollama isn't answering on this computer yet: install it from ollama.com and start it." >&2
        elif ! printf '%s' "$tags" | grep -qF "\"name\":\"$listed\""; then
          echo "Ollama here has no $model yet: run ollama pull $model." >&2
        fi
      fi
      if [ "$(uname -s)" = Linux ]; then
        echo "On Linux, start Ollama with OLLAMA_HOST=$DOCKER_BRIDGE, Docker's bridge address, so Docker can reach it and your network can't."
      fi
      ;;
    3)
      ai=anthropic
      ask_key "ANTHROPIC_API_KEY" "${ANTHROPIC_API_KEY:-}"
      ;;
    4)
      ai=openai
      ask_key "OPENAI_API_KEY" "${OPENAI_API_KEY:-}"
      ;;
    5)
      ai=openai_compatible
      # Foundry runs in Docker: a server on this computer is
      # host.docker.internal, not localhost.
      echo "The server's base URL, including /v1. On this computer, use host.docker.internal:"
      echo "LM Studio http://host.docker.internal:1234/v1, vLLM :8000/v1, Ollama :11434/v1."
      printf "Base URL [http://host.docker.internal:1234/v1]: "
      read -r base_url
      base_url=${base_url:-http://host.docker.internal:1234/v1}
      case $base_url in
        http://?* | https://?*) ;;
        *)
          echo "That isn't an http:// or https:// URL; nothing was written." >&2
          exit 1
          ;;
      esac
      case $base_url in
        *[!A-Za-z0-9._~:/%+-]*)
          echo "Use a base URL with no spaces, user, query or fragment; nothing was written." >&2
          exit 1
          ;;
      esac
      printf "API format: 1) OpenAI Chat Completions (most servers) 2) OpenAI Responses [1]: "
      read -r format
      case "${format:-1}" in
        1) api=chat_completions ;;
        2) api=responses ;;
        *)
          echo "Choose 1 or 2; nothing was written." >&2
          exit 1
          ;;
      esac
      printf "Model, as the server names it (its ID or alias): "
      read -r model
      case $model in
        "" | *[!A-Za-z0-9._:/@+-]*)
          echo "That isn't a model name; nothing was written." >&2
          exit 1
          ;;
      esac
      printf "The server's API key, if it takes one (not shown; Enter for none): "
      trap 'stty echo 2>/dev/null' EXIT
      stty -echo
      read -r key
      stty echo
      echo
      case $key in
        *[!A-Za-z0-9._~+/=:-]*)
          echo "That isn't an API key .env can hold; nothing was written." >&2
          exit 1
          ;;
      esac
      ;;
    6)
      ai=bedrock
      printf "AWS region [%s]: " "${AWS_REGION:-us-east-1}"
      read -r region
      region=${region:-${AWS_REGION:-us-east-1}}
      if ! printf %s "$region" | grep -Eqx '[a-z]{2}(-[a-z]+)+-[0-9]+'; then
        echo "That isn't an AWS region, like us-east-1; nothing was written." >&2
        exit 1
      fi
      key=""
      if [ -n "${AWS_BEARER_TOKEN_BEDROCK:-}" ]; then
        printf "Use the AWS_BEARER_TOKEN_BEDROCK in this shell? [Y/n]: "
        read -r reuse
        case "${reuse:-y}" in [Yy]*) key=$AWS_BEARER_TOKEN_BEDROCK ;; esac
      fi
      if [ -z "$key" ]; then
        printf "Bedrock API key (not shown): "
        trap 'stty echo 2>/dev/null' EXIT
        stty -echo
        read -r key
        stty echo
        echo
      fi
      case $key in
        "" | *[!A-Za-z0-9+/=_-]*)
          echo "That isn't a Bedrock API key; nothing was written." >&2
          exit 1
          ;;
      esac
      echo "A model or inference-profile ID for every task (it must support tool use),"
      printf "or Enter for Claude's models through Bedrock's global inference profiles, one per task: "
      read -r model
      case $model in
        *[!A-Za-z0-9._:/-]*)
          echo "That isn't a Bedrock model ID; nothing was written." >&2
          exit 1
          ;;
      esac
      # Global profiles run in AWS's commercial regions only.
      case $model:$region in
        :us-gov-* | :cn-*)
          echo "Bedrock has no global inference profiles in $region: enter a model ID it offers there; nothing was written." >&2
          exit 1
          ;;
      esac
      ;;
    7) ;;
    *)
      echo "Choose 1 to 7; nothing was written." >&2
      exit 1
      ;;
  esac
fi

umask 077
cat > .env <<ENV
FOUNDRY_LOCAL_TOKEN=$(hex)
FOUNDRY_S3_SECRET=$(hex)
FOUNDRY_MYSQL_PASSWORD=$(hex)
FOUNDRY_CONNECTOR_KEY=$(openssl rand -base64 32)
ENV
next="Next: docker compose up -d --wait"
case $ai in
  ollama-docker)
    cat >> .env <<ENV
# The install's AI provider: Ollama in Docker (the ollama profile), which
# pulls FOUNDRY_OLLAMA_MODEL once and checks it is FOUNDRY_OLLAMA_DIGEST. An
# owner can choose another in Settings › AI models.
COMPOSE_PROFILES=ollama
FOUNDRY_OLLAMA_MODEL=$OLLAMA_MODEL
FOUNDRY_OLLAMA_DIGEST=$OLLAMA_DIGEST
FOUNDRY_AI_PROVIDER=ollama
FOUNDRY_AI_BASE_URL=http://127.0.0.1:11434
FOUNDRY_AI_MODEL=$OLLAMA_MODEL
ENV
    if [ -n "$gpu" ]; then
      cat >> .env <<ENV
# Ollama on the NVIDIA GPUs (compose.gpu.yaml).
COMPOSE_FILE=compose.yaml:compose.gpu.yaml
ENV
    fi
    echo "Wrote .env, with Ollama in Docker as the install's AI. $next; the first start downloads $OLLAMA_MODEL (docker compose logs -f ollama-pull shows it)."
    ;;
  ollama-host)
    cat >> .env <<ENV
# The install's AI provider: Ollama on this computer. An owner can choose
# another in Settings › AI models.
FOUNDRY_AI_PROVIDER=ollama
FOUNDRY_AI_BASE_URL=http://host.docker.internal:11434
FOUNDRY_AI_MODEL=$model
ENV
    echo "Wrote .env, with Ollama on this computer as the install's AI. $next"
    ;;
  anthropic)
    # Single quotes: Compose reads the key as written.
    cat >> .env <<ENV
# The install's AI provider; an owner can choose another in Settings › AI models.
FOUNDRY_AI_PROVIDER=anthropic
FOUNDRY_AI_API_KEY='$key'
# Each task's model, which wins over FOUNDRY_AI_MODEL: switching provider,
# set these to its models or delete them.
FOUNDRY_AI_DRAFT_MODEL=claude-opus-5-5
FOUNDRY_AI_ASK_MODEL=claude-sonnet-5-5
FOUNDRY_AI_EXTRACT_MODEL=claude-haiku-5-5
FOUNDRY_AI_VERIFY_MODEL=claude-sonnet-5-5
ENV
    echo "Wrote .env, with Claude as the install's AI. $next"
    ;;
  openai)
    cat >> .env <<ENV
# The install's AI provider; an owner can choose another in Settings › AI models.
FOUNDRY_AI_PROVIDER=openai
FOUNDRY_AI_API_KEY='$key'
# Each task's model, which wins over FOUNDRY_AI_MODEL: switching provider,
# set these to its models or delete them.
FOUNDRY_AI_DRAFT_MODEL=gpt-6.1-sol
FOUNDRY_AI_ASK_MODEL=gpt-6.1-sol
FOUNDRY_AI_EXTRACT_MODEL=gpt-6-luna
FOUNDRY_AI_VERIFY_MODEL=gpt-6.1-sol
ENV
    echo "Wrote .env, with OpenAI as the install's AI. $next"
    ;;
  openai_compatible)
    # Every task runs on the model, with no cost counted. Single quotes:
    # Compose reads each value as written.
    cat >> .env <<ENV
# The install's AI provider; an owner can choose another in Settings › AI models.
FOUNDRY_AI_PROVIDER=openai_compatible
FOUNDRY_AI_BASE_URL='$base_url'
FOUNDRY_AI_API=$api
FOUNDRY_AI_MODEL='$model'
ENV
    if [ -n "$key" ]; then
      echo "FOUNDRY_AI_API_KEY='$key'" >> .env
    fi
    echo "Wrote .env, with the server at $base_url as the install's AI. $next"
    ;;
  bedrock)
    cat >> .env <<ENV
# The install's AI provider; an owner can choose another in Settings › AI models.
FOUNDRY_AI_PROVIDER=bedrock
FOUNDRY_AI_REGION=$region
FOUNDRY_AI_API_KEY='$key'
ENV
    if [ -n "$model" ]; then
      echo "FOUNDRY_AI_MODEL=$model" >> .env
      echo "Wrote .env, with Amazon Bedrock in $region as the install's AI. $next"
    else
      cat >> .env <<ENV
# Claude through Bedrock's global inference profiles, which may run requests
# in any AWS commercial region; a geography's (eu.anthropic.claude-…) stay
# within it. Each task's model wins over FOUNDRY_AI_MODEL: switching
# provider, set these to its models or delete them.
FOUNDRY_AI_DRAFT_MODEL=global.anthropic.claude-opus-5-5
FOUNDRY_AI_ASK_MODEL=global.anthropic.claude-sonnet-5-5
FOUNDRY_AI_EXTRACT_MODEL=global.anthropic.claude-haiku-5-5
FOUNDRY_AI_VERIFY_MODEL=global.anthropic.claude-sonnet-5-5
ENV
      echo "Wrote .env, with Amazon Bedrock in $region as the install's AI, through global inference profiles, which may run requests in any AWS commercial region. $next"
    fi
    ;;
  *)
    cat >> .env <<ENV
# No AI provider: choose one in Settings › AI models, or set the FOUNDRY_AI_*
# variables of .env.example here and run docker compose up -d again.
ENV
    echo "Wrote .env. $next, then choose an AI provider in Settings › AI models."
    ;;
esac
