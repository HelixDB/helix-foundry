package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

type googleKind struct {
	base     string
	tokenURL string
	http     *http.Client
}

func (k *googleKind) Name() string        { return "google" }
func (k *googleKind) DisplayName() string { return "Google Workspace" }

// GoogleSettings are a Google Workspace connector's settings: the user the
// service account acts as, and how many days of past calendar events it keeps.
type GoogleSettings struct {
	Subject   string `json:"subject"`
	EventDays int    `json:"event_days"`
}

func (k *googleKind) Prepare(Source) (json.RawMessage, error) {
	return nil, errors.New("google: not implemented")
}

func (k *googleKind) Discover(context.Context, Source) (Discovery, error) {
	return Discovery{}, errors.New("google: not implemented")
}

func (k *googleKind) Hint(string) string { return "…" }

func (k *googleKind) Sync(context.Context, Source, Table, RowWriter) (Table, error) {
	return Table{}, errors.New("google: not implemented")
}
