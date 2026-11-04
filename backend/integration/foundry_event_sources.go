package integration

import (
	"cmp"
	"context"
	"net/url"
	"slices"
	"strconv"

	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
	foundrymodel "github.com/helixdb/helix-foundry/backend/store/model"

	"github.com/helixdb/helix-foundry/backend/events"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Foundry's event sources (connector kind events; people call them API sources) take pieces of
// data, JSON objects with an id, that people and their agents send. The MCP makes one with the
// confirmed create_event_source operation, and sends to one with helix_foundry_send_pieces, which
// is not confirmed: it only writes data to a source a person made. A sender is a user who may
// manage the Workspace's connectors, or the source's own send token (hx_ev_), which a person makes
// in the browser: no tool ever returns one. On a stamp with the HTTP door (the foundry-events role),
// an app without an MCP client POSTs the same pieces there with a send token; list_connectors and
// create_event_source give each source's URL as send_url, and the send tool says where it is.
const toolFoundrySendPieces = "helix_foundry_send_pieces"

// eventSourceTable is an event source's one table: the newest piece of each id.
const eventSourceTable = "events"

// eventSourceModes are how a generation reads an event source, as generate_ontology names them.
var eventSourceModes = map[string]foundryv1.FoundryEventSourceMode{
	"structured": foundryv1.FoundryEventSourceMode_FOUNDRY_EVENT_SOURCE_MODE_STRUCTURED,
	"extraction": foundryv1.FoundryEventSourceMode_FOUNDRY_EVENT_SOURCE_MODE_EXTRACTION,
}

type SendPiecesInput struct {
	WorkspaceID   string           `json:"workspace_id,omitempty" jsonschema:"The API source's workspace. With a send token, optional: the token's own."`
	EventSourceID string           `json:"event_source_id,omitempty" jsonschema:"The API source: a connector of kind events. With a send token, optional: the token's own."`
	ClientToken   string           `json:"client_token,omitempty" jsonschema:"A new random value for each call, 16 to 64 printable ASCII characters (a UUID). Reuse it only to retry the same call with the same pieces, such as after backend_unavailable: that stores nothing new and returns the first send. With other pieces it is refused."`
	Pieces        []SendPieceInput `json:"pieces" jsonschema:"1 to 500 pieces, at most 1 MiB of data together (keep a call under about 800 KB: data_json is escaped inside the call)."`
}

type SendPieceInput struct {
	ID         string `json:"id" jsonschema:"1 to 128 printable ASCII characters, no spaces, unique in the source: a later piece with the same id replaces it, whoever sent it."`
	OccurredAt string `json:"occurred_at,omitempty" jsonschema:"When it happened, RFC 3339. Optional."`
	DataJSON   string `json:"data_json" jsonschema:"The piece: one JSON object as text, at most 256 KiB, nested at most 64 deep. Its top-level keys are the source's fields; a field keeps the JSON type it first had (null is always allowed). It may not have top-level id, received_at or occurred_at. Write it from the original text; integers above 2^53 go as strings if your client may have parsed them."`
}

type EventSourceFieldOutput struct {
	Name string `json:"name"`
	// Type is string, number, boolean, object or array; null while the field has only been null.
	Type string `json:"type"`
}

// EventSourceCapacityOutput is how full a source's table will be once its waiting pieces are
// synced, against the caps every table has. Rows count a resent id again, so both counts are
// upper bounds.
type EventSourceCapacityOutput struct {
	Rows        string `json:"rows"`
	MaxRows     string `json:"max_rows"`
	Bytes       string `json:"bytes"`
	MaxBytes    string `json:"max_bytes"`
	PercentFull int    `json:"percent_full"`
}

type SendPiecesOutput struct {
	ContentTrust  string                    `json:"content_trust"`
	EventSourceID string                    `json:"event_source_id"`
	Seq           string                    `json:"seq" jsonschema:"The send's number in the source, as a string of digits."`
	Pieces        int32                     `json:"pieces"`
	ReceivedAt    string                    `json:"received_at"`
	Replayed      bool                      `json:"replayed" jsonschema:"True when this client_token's send was already stored: nothing new was stored."`
	NewFields     []EventSourceFieldOutput  `json:"new_fields"`
	Capacity      EventSourceCapacityOutput `json:"capacity" jsonschema:"rows, max_rows, bytes and max_bytes (strings of digits) and percent_full of the source's table."`
	Warning       string                    `json:"warning,omitempty" jsonschema:"Set from 80% full; tell the user."`
	Next          string                    `json:"next" jsonschema:"Pieces reach the source's table at its next sync, about a minute away; ontologies that map it see them at their next refresh."`
}

// FoundryEventSourceOutput is what list_connectors says of an API source beside its table. Its
// description and field names come from people and senders, so they are data. PiecesSetID and
// Extraction are both set, from the source's own document set, or neither is: a source without a
// text field has no documents, and one with a text field has its set once a sync has made a
// document of a piece.
type FoundryEventSourceOutput struct {
	Description    string                       `json:"description"`
	TextField      string                       `json:"text_field,omitempty" jsonschema:"The top-level field holding each piece's text, locked as a string: the body of each piece's document, which entity extraction reads. Absent when the source has none."`
	Fields         []EventSourceFieldOutput     `json:"fields"`
	LastReceivedAt string                       `json:"last_received_at,omitempty" jsonschema:"When its newest send arrived; absent before any."`
	PendingPieces  int64                        `json:"pending_pieces" jsonschema:"Pieces no sync has read yet."`
	Capacity       EventSourceCapacityOutput    `json:"capacity"`
	TokenURL       string                       `json:"token_url,omitempty" jsonschema:"The source's page in Foundry, where a person makes a send token (hx_ev_) for a sender that isn't them; no tool returns one."`
	SendURL        string                       `json:"send_url,omitempty" jsonschema:"An app without an MCP client can POST the same pieces here, data as a JSON object, with a send token as its bearer and a client_token on every send; absent on a stamp without the HTTP endpoint."`
	PiecesSetID    string                       `json:"pieces_set_id,omitempty" jsonschema:"The source's own document set: its newest pieces as Markdown documents, which generate_ontology reads with event_sources mode extraction (document_set_ids never names it). Absent without a text field, and until a sync has made a document of a piece."`
	Extraction     *EventSourceExtractionOutput `json:"extraction,omitempty" jsonschema:"The source's set as entity extraction reads it; present exactly when pieces_set_id is."`
}

// EventSourceExtractionOutput is an API source's own set as entity extraction reads it: its
// documents against the most it holds, and what work no person started spent extracting them
// this UTC month against the source's allowance.
type EventSourceExtractionOutput struct {
	Documents    int32   `json:"documents" jsonschema:"Pieces the set holds as documents; generate_ontology's mode extraction needs at least one."`
	MaxDocuments int32   `json:"max_documents" jsonschema:"The most it holds: the source's newest pieces by arrival, at every sync."`
	MonthUSD     float64 `json:"month_usd" jsonschema:"US dollars that work no person started (refreshes, and extracts no one asked for) spent extracting its documents this UTC month, for every ontology that reads it."`
	MaxMonthUSD  float64 `json:"max_month_usd" jsonschema:"The most that work spends on the source in a UTC month."`
	Paused       bool    `json:"paused" jsonschema:"True while its syncs write no documents: the set holds some, and work no person started can spend no more on it this UTC month (month_usd reached max_month_usd, or the workspace's share for all its API sources, or for all such work, is spent). The graph keeps the pieces it has, and new pieces wait for next month or a load a person starts."`
}

// FoundryEventSourceReadOutput is an API source a generation drafts from, and how its ontology
// reads it.
type FoundryEventSourceReadOutput struct {
	EventSourceID string `json:"event_source_id"`
	Mode          string `json:"mode" jsonschema:"structured: its table, through mappings; pieces reach the graph when the ontology refreshes. extraction: its own set's documents, its newest pieces' text, extracted as they arrive."`
}

// AddFoundrySendTool registers the send tool for a send token, and for a user who may manage some
// Workspace's connectors. A service key or an agent registration never gets it.
func (s *Service) AddFoundrySendTool(server *mcp.Server, identity *Identity) {
	if !s.Events {
		return
	}
	if identity.EventSource == nil && (!identity.User || !identity.HasPermission(PermissionWorkspaceConnectorsManage)) {
		return
	}
	destructive, no := true, false
	description := "Send pieces of data, JSON objects with an id, to a Foundry API source (a connector of kind events). " +
		"It adds pieces, and replaces any piece already in the source with the same id. Not confirmed: it only writes data to a source a person made. " +
		"Anyone who can send to the source can replace any of its pieces by id, or blank its fields by sending {}."
	// A send token sees this tool alone, so the sentence stands on its own and names no other tool.
	if s.EventsURL != nil {
		description += " Apps without an MCP client can POST the same pieces to " + s.EventsURL.String() +
			"/v1/sources/<event_source_id>/pieces with a send token as the bearer."
	}
	addFoundryTool(s, server, identity, &mcp.Tool{Name: toolFoundrySendPieces, Description: description,
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &destructive, OpenWorldHint: &no},
	}, s.SendFoundryPieces)
}

// SendFoundryPieces sends one batch. A send token sends to its own source: an empty Workspace or
// source is the token's, and any other is not found. A user is authorized as for SyncConnector,
// connector management in the Workspace. The backend checks the pieces and the token again;
// each piece's data_json goes to it byte for byte.
func (s *Service) SendFoundryPieces(
	ctx context.Context, identity *Identity, call *Call, input SendPiecesInput,
) (SendPiecesOutput, int, error) {
	sender := identity.EventSource
	if sender != nil {
		input.WorkspaceID = cmp.Or(input.WorkspaceID, sender.WorkspaceID)
		input.EventSourceID = cmp.Or(input.EventSourceID, sender.ConnectorID)
	}
	call.ResourceKind, call.ResourceID = "connector", input.EventSourceID
	if sender != nil && (input.WorkspaceID != sender.WorkspaceID || input.EventSourceID != sender.ConnectorID) {
		return SendPiecesOutput{}, 0, errCodeNotFound
	}
	if !ValidIDs([]string{input.WorkspaceID, input.EventSourceID}) {
		return SendPiecesOutput{}, 0, errCodeInvalidArguments
	}
	if sender == nil {
		if err := s.AuthorizeFoundry(ctx, identity, &foundryv1.SendFoundryEventsRequest{WorkspaceId: input.WorkspaceID}, foundryUserOnly, "SyncConnector"); err != nil {
			return SendPiecesOutput{}, 0, err
		}
	}
	req := &foundryv1.SendFoundryEventsRequest{
		WorkspaceId: input.WorkspaceID, ConnectorId: input.EventSourceID, ClientToken: input.ClientToken,
		Pieces: make([]*foundryv1.FoundryEventPiece, len(input.Pieces)),
	}
	for i, p := range input.Pieces {
		req.Pieces[i] = &foundryv1.FoundryEventPiece{Id: p.ID, DataJson: p.DataJSON}
		if p.OccurredAt == "" {
			continue
		}
		at, err := events.ParseOccurredAt(i, p.ID, p.OccurredAt)
		if err != nil {
			return SendPiecesOutput{}, 0, detailedToolError{code: errCodeInvalidArguments, message: err.Error()}
		}
		req.Pieces[i].OccurredAt = timestamppb.New(at)
	}
	response, err := s.Actions.SendFoundryEvents(ctx, req)
	if err != nil {
		return SendPiecesOutput{}, 0, FoundryError(err)
	}
	batch := response.GetBatch()
	out := SendPiecesOutput{
		ContentTrust: contentTrustUntrusted, EventSourceID: input.EventSourceID, Seq: strconv.FormatInt(batch.GetSeq(), 10),
		Pieces: batch.GetPieces(), ReceivedAt: protoTimestamp(batch.GetCreatedAt()), Replayed: response.GetReplayed(),
		NewFields: EventSourceFields(response.GetNewFields()), Capacity: EventSourceCapacity(response.GetCapacity()), Warning: response.GetWarning(),
		Next: "Pieces reach the source's table at its next sync, about a minute away; ontologies that map it see them at their next refresh.",
	}
	return out, int(batch.GetPieces()), nil
}

func EventSourceFields(fields []*foundryv1.EventSourceField) []EventSourceFieldOutput {
	out := make([]EventSourceFieldOutput, len(fields))
	for i, f := range fields {
		out[i] = EventSourceFieldOutput{Name: f.GetName(), Type: f.GetType()}
	}
	return out
}

// EventSourceCapacity is the backend's capacity with how full it is, as the backend's warning
// counts it; 0 for an answer without caps.
func EventSourceCapacity(c *foundryv1.EventSourceCapacity) EventSourceCapacityOutput {
	out := EventSourceCapacityOutput{
		Rows: strconv.FormatInt(c.GetRows(), 10), MaxRows: strconv.FormatInt(c.GetMaxRows(), 10),
		Bytes: strconv.FormatInt(c.GetBytes(), 10), MaxBytes: strconv.FormatInt(c.GetMaxBytes(), 10),
	}
	if c.GetMaxRows() > 0 && c.GetMaxBytes() > 0 {
		out.PercentFull = events.Capacity{Rows: c.GetRows(), Bytes: c.GetBytes(), Caps: events.Caps{MaxRows: c.GetMaxRows(), MaxBytes: c.GetMaxBytes()}}.Percent()
	}
	return out
}

// FoundryEventSource is list_connectors' block of an API source, its last four from its newest
// batch: the backend reads them in one consistent read. sets are the Workspace's document sets,
// read when some source has a text field; the one this source owns, if any, gives its extraction.
func (s *Service) FoundryEventSource(ctx context.Context, workspaceID string, c *foundryv1.Connector, sets []*foundryv1.FoundryDocumentSet) (*FoundryEventSourceOutput, error) {
	batches, err := s.Foundry.ListFoundryEventBatches(ctx, &foundryv1.ListFoundryEventBatchesRequest{WorkspaceId: workspaceID, ConnectorId: c.GetId(), Limit: 1})
	if err != nil {
		return nil, err
	}
	config := c.GetConfig().GetEvents()
	out := &FoundryEventSourceOutput{
		Description: config.GetDescription(), TextField: config.GetTextField(), Fields: EventSourceFields(config.GetFields()),
		PendingPieces: batches.GetPendingPieces(), Capacity: EventSourceCapacity(batches.GetCapacity()), TokenURL: s.EventSourceTokenURL(workspaceID, c.GetId()),
		SendURL: s.EventSourceSendURL(c.GetId()),
	}
	if newest := batches.GetBatches(); len(newest) > 0 {
		out.LastReceivedAt = protoTimestamp(newest[0].GetCreatedAt())
	}
	// A source whose text field was cleared keeps its emptied set, which is no longer read.
	if out.TextField == "" {
		return out, nil
	}
	i := slices.IndexFunc(sets, func(set *foundryv1.FoundryDocumentSet) bool {
		owner := set.GetConnector()
		return owner.GetConnectorKind() == foundrymodel.ConnectorKindEvents && owner.GetConnectorId() == c.GetId()
	})
	if i < 0 {
		return out, nil
	}
	owner := sets[i].GetConnector()
	out.PiecesSetID = sets[i].GetId()
	out.Extraction = &EventSourceExtractionOutput{
		Documents: sets[i].GetDocuments(), MaxDocuments: owner.GetMaxDocuments(),
		MonthUSD: float64(owner.GetMonthSystemUsdMicros()) / 1e6, MaxMonthUSD: float64(owner.GetMaxMonthSystemUsdMicros()) / 1e6,
		Paused: owner.GetPaused(),
	}
	return out, nil
}

// EventSourceSendURL is where an app without an MCP client POSTs the source's pieces, the HTTP
// door's POST /v1/sources/{id}/pieces with the id as one segment, or empty on a stamp without the
// door.
func (s *Service) EventSourceSendURL(connectorID string) string {
	if s.EventsURL == nil {
		return ""
	}
	return s.EventsURL.JoinPath("v1", "sources", url.PathEscape(connectorID), "pieces").String()
}

// EventSourceTokenURL opens the source's page in Foundry on its new-token dialog, or is empty on a
// stamp that doesn't name the website.
func (s *Service) EventSourceTokenURL(workspaceID, connectorID string) string {
	page := s.FoundryLink(workspaceID, "data", "connections", connectorID)
	if page == "" {
		return ""
	}
	link, err := url.Parse(page)
	if err != nil {
		return ""
	}
	query := link.Query()
	query.Set("token", "new")
	link.RawQuery = query.Encode()
	return link.String()
}

// EventSourceRefs are the Workspace's API sources as a generation's stored input names them,
// which holds tables and sets only: an events connector's table is read through mappings, and an
// API source's own set (its connector's kind events) by extraction.
type EventSourceRefs struct {
	// connectors are the Workspace's event sources by id.
	connectors map[string]bool
	// sets maps each event source's own set, by id, to the source.
	sets map[string]string
}

// EventSourceReads are the API sources a generation drafts from, with how its ontology reads
// each: its tables' first, then its sets'.
func EventSourceReads(gen *foundryv1.OntologyGeneration, sources EventSourceRefs) []FoundryEventSourceReadOutput {
	out := []FoundryEventSourceReadOutput{}
	for _, table := range gen.GetTables() {
		if sources.connectors[table.GetConnectorId()] {
			out = append(out, FoundryEventSourceReadOutput{EventSourceID: table.GetConnectorId(), Mode: "structured"})
		}
	}
	for _, setID := range gen.GetDocumentSetIds() {
		if source, ok := sources.sets[setID]; ok {
			out = append(out, FoundryEventSourceReadOutput{EventSourceID: source, Mode: "extraction"})
		}
	}
	return out
}

// EventSourceIDs are the Workspace's event sources and their own sets, to tell which of the
// generations' tables and sets are an API source's. They are none for a service key, which sees
// no connectors. Each is read only when it could matter: the connectors (listing them reads each
// one's tables and the Workspace's plan) when a generation has a table named events, as an event
// source's is, and the sets when a generation reads one. listed is the Workspace's sets when the
// caller has read them already, and nil otherwise. A host without API sources has none to tell.
func (s *Service) EventSourceIDs(
	ctx context.Context, identity *Identity, workspaceID string, listed *foundryv1.ListFoundryDocumentSetsResponse, generations ...*foundryv1.OntologyGeneration,
) (EventSourceRefs, error) {
	sources := EventSourceRefs{connectors: map[string]bool{}, sets: map[string]string{}}
	if identity.ServiceKey || !s.Events {
		return sources, nil
	}
	if slices.ContainsFunc(generations, func(gen *foundryv1.OntologyGeneration) bool {
		return slices.ContainsFunc(gen.GetTables(), func(t *foundryv1.OntologyGenerationTable) bool { return t.GetTable() == eventSourceTable })
	}) {
		connectors, err := s.Foundry.ListConnectors(ctx, &foundryv1.ListConnectorsRequest{WorkspaceId: workspaceID})
		if err != nil {
			return EventSourceRefs{}, err
		}
		for _, c := range connectors.GetConnectors() {
			if c.GetKind() == foundrymodel.ConnectorKindEvents {
				sources.connectors[c.GetId()] = true
			}
		}
	}
	if listed == nil && slices.ContainsFunc(generations, func(gen *foundryv1.OntologyGeneration) bool { return len(gen.GetDocumentSetIds()) > 0 }) {
		var err error
		if listed, err = s.Foundry.ListFoundryDocumentSets(ctx, &foundryv1.ListFoundryDocumentSetsRequest{WorkspaceId: workspaceID}); err != nil {
			return EventSourceRefs{}, err
		}
	}
	for _, set := range listed.GetSets() {
		if owner := set.GetConnector(); owner.GetConnectorKind() == foundrymodel.ConnectorKindEvents {
			sources.sets[set.GetId()] = owner.GetConnectorId()
		}
	}
	return sources, nil
}

// CreateEventSourcePayload's TextField is a pointer: absent, the source has no text field.
type CreateEventSourcePayload struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	TextField   *string `json:"text_field,omitempty"`
}

type CreateEventSourceOperation struct {
	payload CreateEventSourcePayload
	req     *foundryv1.CreateConnectorRequest
}

type EventSourceOutput struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// TextField is the field holding each piece's text, locked as a string; empty for none.
	TextField string                   `json:"text_field,omitempty"`
	Fields    []EventSourceFieldOutput `json:"fields"`
}

type CreateEventSourceResult struct {
	EventSource EventSourceOutput `json:"event_source"`
	// TokenURL is where a person makes the source a send token; empty on a stamp that doesn't name
	// the website.
	TokenURL string `json:"token_url"`
	SendURL  string `json:"send_url,omitempty" jsonschema:"An app without an MCP client can POST the same pieces here, data as a JSON object, with a send token as its bearer and a client_token on every send; absent on a stamp without the HTTP endpoint."`
	SendTool string `json:"send_tool"`
}

func (o CreateEventSourceOperation) CanonicalPayload() []byte { return mustCanonical(o.payload) }

// Execute creates the source once. The connector RPC takes no client token, and an Execute is
// never retried: the agent finds a source it may have made with list_connectors, and creating a
// name a live source has is already_exists, naming that source.
func (o CreateEventSourceOperation) Execute(ctx context.Context, s *Service, _ *Identity, _ string) (any, error) {
	response, err := s.Actions.CreateConnector(ctx, o.req)
	if err != nil {
		return nil, err
	}
	c := response.GetConnector()
	config := c.GetConfig().GetEvents()
	return CreateEventSourceResult{
		EventSource: EventSourceOutput{
			ID: c.GetId(), Name: c.GetName(), Description: config.GetDescription(), TextField: config.GetTextField(), Fields: EventSourceFields(config.GetFields()),
		},
		TokenURL: s.EventSourceTokenURL(o.req.GetWorkspaceId(), c.GetId()), SendURL: s.EventSourceSendURL(c.GetId()), SendTool: toolFoundrySendPieces,
	}, nil
}
