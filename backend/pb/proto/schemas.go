// Package protoschemas embeds parts of the vendored ontology agent contract
// for code that hands them on to its own callers, such as the MCP's Foundry
// read guide, or checks a deployment against them. The files stay where they
// are vendored (see ontology_agent/v1/README.md).
package protoschemas

import _ "embed"

// OntologyAgentReadRequest is the ontology agent's read_request.json, whose
// ReadItem is what a Foundry graph read sends.
//
//go:embed ontology_agent/v1/schema/read_request.json
var OntologyAgentReadRequest []byte

// OntologyAgentCatalog is the agent's price catalog, catalog.json: the
// models it prices by table, and their rates in nano-USD per token.
//
//go:embed ontology_agent/v1/schema/catalog.json
var OntologyAgentCatalog []byte

// OntologyAgentREADME is the vendored contract's README, which names the
// agent commit the contract was copied from: the agent image a deployment
// runs must be built from that commit.
//
//go:embed ontology_agent/v1/README.md
var OntologyAgentREADME []byte
