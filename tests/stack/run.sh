#!/bin/sh
# Runs tests/stack against a fresh local stack on this machine: wipes the
# stack's volumes (its secrets too), starts it as a fresh clone does, with
# nothing set up, plus the sample database, waits for the site, then runs
# the suite in Playwright's image on the host network, its results in
# test-results/stack. It runs the site and foundry built from source,
# helix-foundry/{site,foundry}:local (it builds foundry's from backend/
# first; Compose builds the site's when it is missing), and the release's
# ontology agent, whose source is not here. FOUNDRY_SITE_IMAGE, FOUNDRY_IMAGE
# and FOUNDRY_AGENT_IMAGE run others as they are. Without models it bills
# nothing, and finds none on this machine (FOUNDRY_AI_PROVIDER=none):
#   FOUNDRY_STACK_MODELS=1  Claude is the install's provider, on the
#                           ANTHROPIC_API_KEY of this shell;
#   FOUNDRY_STACK_OLLAMA=1  Ollama in Docker (the ollama profile) with
#                           FOUNDRY_STACK_OLLAMA_MODEL (the install's default
#                           model unless set), on the GPUs with
#                           FOUNDRY_STACK_GPU=1, which the suite chooses in
#                           Settings › AI models. Its models stay in the volume
#                           helix-foundry-stack-ollama between runs.
set -eu
cd "$(dirname "$0")/../.."
# Its own project, so wiping its volumes never touches an installation. The
# two would share ports 3000 and 8333, so it refuses to run beside one.
if docker compose ls -q | grep -qx helix-foundry; then
  echo "Helix Foundry is running here (project helix-foundry); stop it first." >&2
  exit 1
fi
# The site and foundry from source unless FOUNDRY_*IMAGE names others; the
# agent is local/compose.yaml's unless FOUNDRY_AGENT_IMAGE names one.
export FOUNDRY_SITE_IMAGE="${FOUNDRY_SITE_IMAGE:-helix-foundry/site:local}"
if [ -z "${FOUNDRY_IMAGE:-}" ]; then
  export FOUNDRY_IMAGE=helix-foundry/foundry:local
  docker build -t "$FOUNDRY_IMAGE" backend
fi
compose="docker compose -p helix-foundry-stack -f local/compose.yaml -f tests/stack/compose.yaml"
# Ollama's profile, overriding local/.env's.
export COMPOSE_PROFILES=
if [ -n "${FOUNDRY_STACK_OLLAMA:-}" ]; then
  export COMPOSE_PROFILES=ollama \
    FOUNDRY_OLLAMA_MODEL="${FOUNDRY_STACK_OLLAMA_MODEL:-qwen3.5:4b}" \
    FOUNDRY_OLLAMA_DIGEST=
  compose="$compose -f tests/stack/ollama.yaml"
  if [ -n "${FOUNDRY_STACK_GPU:-}" ]; then
    compose="$compose -f local/compose.gpu.yaml"
  fi
  docker volume create helix-foundry-stack-ollama >/dev/null
fi
# The install's AI provider, overriding local/.env's if there is one: none
# without models, so drafts and uploads wait for one, nothing is billed, and
# nothing on this machine (an Ollama, a key in this shell) is found instead;
# with FOUNDRY_STACK_MODELS=1, Claude with the ANTHROPIC_API_KEY of this
# shell. Either way none of another kind's fields from local/.env, which
# Foundry refuses for Claude.
if [ -n "${FOUNDRY_STACK_MODELS:-}" ]; then
  export FOUNDRY_AI_PROVIDER=anthropic \
    FOUNDRY_AI_API_KEY="${ANTHROPIC_API_KEY:?FOUNDRY_STACK_MODELS needs ANTHROPIC_API_KEY}" \
    FOUNDRY_AI_DRAFT_MODEL=claude-opus-5-5 FOUNDRY_AI_ASK_MODEL=claude-sonnet-5-5 \
    FOUNDRY_AI_EXTRACT_MODEL=claude-haiku-5-5 FOUNDRY_AI_VERIFY_MODEL=claude-sonnet-5-5 \
    FOUNDRY_AI_BASE_URL= FOUNDRY_AI_REGION= FOUNDRY_AI_ENDPOINT= FOUNDRY_AI_API= \
    FOUNDRY_AI_CONTEXT_TOKENS=
else
  export FOUNDRY_AI_PROVIDER=none \
    FOUNDRY_AI_API_KEY= FOUNDRY_AI_MODEL= FOUNDRY_AI_DRAFT_MODEL= FOUNDRY_AI_ASK_MODEL= \
    FOUNDRY_AI_EXTRACT_MODEL= FOUNDRY_AI_VERIFY_MODEL= \
    FOUNDRY_AI_BASE_URL= FOUNDRY_AI_REGION= FOUNDRY_AI_ENDPOINT= FOUNDRY_AI_API= \
    FOUNDRY_AI_CONTEXT_TOKENS=
fi
# With the profile on, so a last run's Ollama goes too.
COMPOSE_PROFILES=ollama $compose down -v --remove-orphans
$compose up -d --wait
# The suite's Ollama needs its model pulled and loaded, which --wait doesn't
# wait for.
if [ -n "${FOUNDRY_STACK_OLLAMA:-}" ]; then
  $compose wait ollama-pull
fi
# The checkout is copied in, without its node_modules: the container installs
# its own (Linux binaries, its own store) and never writes to this one but for
# the results, as this user.
# Playwright clears its output directory first, which it can't do to a mount
# point it doesn't own the parent of: the mount is the parent.
mkdir -p test-results
docker run --rm --network host --user "$(id -u):$(id -g)" -e HOME=/tmp \
  -v "$PWD":/src:ro -v "$PWD/test-results":/out \
  -e FOUNDRY_URL=http://localhost:3000 -e FOUNDRY_STACK_MODELS="${FOUNDRY_STACK_MODELS:-}" \
  -e FOUNDRY_STACK_OWN_KEY="${FOUNDRY_STACK_MODELS:+${ANTHROPIC_API_KEY:-}}" \
  -e FOUNDRY_STACK_OLLAMA_MODEL="${FOUNDRY_STACK_OLLAMA:+$FOUNDRY_OLLAMA_MODEL}" \
  mcr.microsoft.com/playwright:v1.62.0-noble \
  sh -c 'mkdir /tmp/site && tar -C /src --exclude=./node_modules --exclude="./.next*" --exclude=./test-results -cf - . | tar -C /tmp/site -xf - &&
    cd /tmp/site && corepack pnpm install --frozen-lockfile --silent &&
    corepack pnpm exec playwright test -c playwright.stack.config.ts --output /out/stack'
