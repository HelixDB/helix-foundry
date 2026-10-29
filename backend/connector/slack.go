package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

type slackKind struct {
	base string
	http *http.Client
}

func (k *slackKind) Name() string        { return "slack" }
func (k *slackKind) DisplayName() string { return "Slack" }

// SlackSettings are a Slack connector's settings: how many days of channel
// history it keeps.
type SlackSettings struct {
	HistoryDays int `json:"history_days"`
}

func (k *slackKind) Prepare(Source) (json.RawMessage, error) {
	return nil, errors.New("slack: not implemented")
}

func (k *slackKind) Discover(context.Context, Source) (Discovery, error) {
	return Discovery{}, errors.New("slack: not implemented")
}

func (k *slackKind) Hint(secret string) string { return keyHint(secret, 0) }

func (k *slackKind) Sync(context.Context, Source, Table, RowWriter) (Table, error) {
	return Table{}, errors.New("slack: not implemented")
}
