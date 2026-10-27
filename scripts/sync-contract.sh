#!/bin/sh
# Copies the REST contracts and the ontology agent's JSON Schemas into
# contract/:
# - contract/foundry.swagger.json: Foundry's own API (helix.foundry.v1), every
#   route under /v1/foundry and /v1/workspaces/{workspaceId}/foundry, from
#   backend/;
# - contract/ontology_agent: the agent's Read and Ask JSON Schemas, which
#   Foundry carries as JSON strings, its Probe result (the model failures and
#   probe details Settings › AI models names) and its price table
#   (catalog.json: the models it can price, per kind), from backend/;
# - contract/wfe.swagger.json: the hosted Helix WFE's, trimmed by
#   scripts/trim-wfe-contract.mjs to the Helix routes the hosted site calls
#   besides (whoami, workspaces, members, projects, tenants, billing), from
#   WFE_REPO, a checkout of hosted Helix's backend, which only Helix's
#   maintainers have, at WFE_REF (origin/dev); without WFE_REPO it stays as
#   it is. contract/wfe.swagger.ref records the commit it came from.
set -eu
cp backend/pb/gen/openapiv2/helix/foundry/v1/foundry.swagger.json contract/foundry.swagger.json
schemas=backend/pb/proto/ontology_agent/v1/schema
mkdir -p contract/ontology_agent
for f in read_request.json read_result.json ask_request.json ask_event.json ask_result.json probe_result.json catalog.json VERSION; do
  cp "$schemas/$f" "contract/ontology_agent/$f"
done
echo "contract/foundry.swagger.json and contract/ontology_agent ($(cat contract/ontology_agent/VERSION)) copied from backend/"
if [ -z "${WFE_REPO:-}" ]; then
  echo "contract/wfe.swagger.json kept: WFE_REPO is not set"
  exit 0
fi
ref=${WFE_REF:-origin/dev}
git -C "$WFE_REPO" show "$ref:backend/pb/gen/openapiv2/helix/wfe/v1/wfe.swagger.json" >contract/wfe.swagger.json
git -C "$WFE_REPO" rev-parse "$ref" >contract/wfe.swagger.ref
node scripts/trim-wfe-contract.mjs
echo "contract/wfe.swagger.json synced from $ref ($(cat contract/wfe.swagger.ref))"
