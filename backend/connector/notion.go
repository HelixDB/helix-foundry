package connector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
)

type notionKind struct {
	base string
	http *http.Client
}

func (k *notionKind) Name() string        { return "notion" }
func (k *notionKind) DisplayName() string { return "Notion" }

func (k *notionKind) Prepare(Source) (json.RawMessage, error) {
	return nil, errors.New("notion: not implemented")
}

func (k *notionKind) Discover(context.Context, Source) (Discovery, error) {
	return Discovery{}, errors.New("notion: not implemented")
}

func (k *notionKind) Hint(secret string) string { return keyHint(secret, 1) }

func (k *notionKind) Sync(context.Context, Source, Table, RowWriter) (Table, error) {
	return Table{}, errors.New("notion: not implemented")
}
