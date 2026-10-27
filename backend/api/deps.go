// Package api serves Foundry's API, helix.foundry.v1.FoundryService and the
// internal helix.foundry.v1.FoundryEventsService, over Foundry's store and
// the ports its host implements. It trusts the ports.Actor on each call's
// context: a front end authenticates and authorizes the caller first.
package api

import (
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/helixdb/helix-foundry/backend/ai/detect"
	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/document"
	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/store"
)

// Deps is what the API needs from its host. The API builds the rest itself
// from Cfg: the connector catalog and keyring, the AI deployment, the plan
// catalog, and its foundry-read client (Cfg.Read).
type Deps struct {
	Cfg    *config.Config
	Logger *slog.Logger
	DB     store.Store
	// AWS is the identity Foundry's own buckets are reached with (event
	// sends' snapshots); connectors' buckets build their own clients.
	AWS aws.Config
	// Documents keeps uploaded documents; nil without a data bucket, and the
	// document routes are then Unavailable.
	Documents *document.Store
	// Plans is nil where no plan is sold: every workspace reads as the
	// catalog's free plan, and checkout, cancel and resume are refused.
	Plans     ports.Plans
	Databases ports.Databases
	Graph     ports.GraphReader
	People    ports.People
	// LocalAI is a local install's look at the model servers on its machine:
	// it finds the install's default again, and lists an Ollama server's
	// models. nil hosted, where DetectFoundryAIDefault and
	// ListFoundryOllamaModels are refused.
	LocalAI *detect.Detector
}
