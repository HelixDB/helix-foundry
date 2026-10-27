package ontologyagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
)

// MaxMessageBytes is the agent's message limit, which the client matches: a
// result carries the state (up to 1 MiB), the draft and any approved bundle,
// past gRPC's 4 MiB default.
const MaxMessageBytes = 32 << 20

// MaxRequestBytes is the most JSON one call may send: MaxMessageBytes less
// the bytes of the field that carries it (a tag and a length, at most 6).
const MaxRequestBytes = MaxMessageBytes - 16

// ErrNoResult is a call whose stream ended without its result: the agent
// stopped or the connection dropped. Retrying the same request is safe.
var ErrNoResult = errors.New("ontology agent: the turn ended without a result")

// Client calls the agent sidecar over loopback, with no TLS: the sidecar binds
// 127.0.0.1, so only its pod can reach it. The calls that run models take
// the model key, which goes in the call's ModelKeyHeader metadata, never in
// its request; the client has no interceptors that could see it.
type Client struct {
	conn  *grpc.ClientConn
	agent agentv1.AgentClient
}

// Dial makes a client for the agent at addr. It connects lazily, so the
// sidecar may still be starting.
func Dial(addr string) (*Client, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(MaxMessageBytes), grpc.MaxCallSendMsgSize(MaxMessageBytes)))
	if err != nil {
		return nil, fmt.Errorf("ontology agent client for %s: %w", addr, err)
	}
	return &Client{conn: conn, agent: agentv1.NewAgentClient(conn)}, nil
}

func (c *Client) Close() error { return c.conn.Close() }

// Info is what the agent runs: its contract version, the provider kinds it
// calls, and its build. It waits, until ctx ends, for an agent that is still
// starting.
func (c *Client) Info(ctx context.Context) (*agentv1.GetInfoResponse, error) {
	return c.agent.GetInfo(ctx, &agentv1.GetInfoRequest{}, grpc.WaitForReady(true))
}

// withKey carries key in ctx's outgoing metadata, as the agent takes it;
// the zero key, none.
func withKey(ctx context.Context, key ModelKey) context.Context {
	if key.IsZero() {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, ModelKeyHeader, key.Reveal())
}

// Turn runs one turn on its request's models, key their endpoint's: it hands
// each event to onEvent as it arrives, on the calling goroutine, and returns
// the result that ends the stream. An error is the stream's gRPC status, or
// ErrNoResult. Cancelling ctx stops the turn before the agent's next model
// call.
func (c *Client) Turn(ctx context.Context, request []byte, key ModelKey, onEvent func(json.RawMessage)) (json.RawMessage, error) {
	ctx, cancel := context.WithCancel(withKey(ctx, key))
	defer cancel() // the result is the last message; this releases the stream
	stream, err := c.agent.Turn(ctx, &agentv1.TurnRequest{RequestJson: string(request)})
	if err != nil {
		return nil, err
	}
	return readStream(stream.Recv, func(e json.RawMessage) error { onEvent(e); return nil })
}

// Ingest loads tables into a database, as Turn runs a turn: each event goes
// to onEvent, and the result ends the stream. An error from onEvent ends the
// call, which cancels the load before its next chunk.
func (c *Client) Ingest(ctx context.Context, request []byte, onEvent func(json.RawMessage) error) (json.RawMessage, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.agent.Ingest(ctx, &agentv1.IngestRequest{RequestJson: string(request)})
	if err != nil {
		return nil, err
	}
	return readStream(stream.Recv, onEvent)
}

// Extract extracts documents into a database on its request's models, key
// their endpoint's, as Ingest loads tables. Cancelling stops it before the
// next document's commit.
func (c *Client) Extract(ctx context.Context, request []byte, key ModelKey, onEvent func(json.RawMessage) error) (json.RawMessage, error) {
	ctx, cancel := context.WithCancel(withKey(ctx, key))
	defer cancel()
	stream, err := c.agent.Extract(ctx, &agentv1.ExtractRequest{RequestJson: string(request)})
	if err != nil {
		return nil, err
	}
	return readStream(stream.Recv, onEvent)
}

// DeleteLoad removes a load from a database, as Ingest loads tables.
// Cancelling stops it between requests; the request in flight finishes.
func (c *Client) DeleteLoad(ctx context.Context, request []byte, onEvent func(json.RawMessage) error) (json.RawMessage, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.agent.DeleteLoad(ctx, &agentv1.DeleteLoadRequest{RequestJson: string(request)})
	if err != nil {
		return nil, err
	}
	return readStream(stream.Recv, onEvent)
}

// Read reads a load: one unary call, which returns the result or the
// agent's gRPC status (FAILED_PRECONDITION for a load the database doesn't
// hold for the version, RESOURCE_EXHAUSTED when it runs as many reads as it
// allows).
func (c *Client) Read(ctx context.Context, request []byte) (json.RawMessage, error) {
	resp, err := c.agent.Read(ctx, &agentv1.ReadRequest{RequestJson: string(request)})
	if err != nil {
		return nil, err
	}
	return json.RawMessage(resp.GetResultJson()), nil
}

// Ask answers one question over a load on its request's model, key its
// endpoint's, as Ingest loads tables: each event goes to onEvent, and the
// result ends the stream. Cancelling stops it before its next model call.
func (c *Client) Ask(ctx context.Context, request []byte, key ModelKey, onEvent func(json.RawMessage) error) (json.RawMessage, error) {
	ctx, cancel := context.WithCancel(withKey(ctx, key))
	defer cancel()
	stream, err := c.agent.Ask(ctx, &agentv1.AskRequest{RequestJson: string(request)})
	if err != nil {
		return nil, err
	}
	return readStream(stream.Recv, onEvent)
}

// Title writes a title (a TitleRequest) on its request's model, key its
// endpoint's: one unary call, which returns the TitleResult or the agent's
// gRPC status. A failed model call is a result, not an error.
func (c *Client) Title(ctx context.Context, request []byte, key ModelKey) (json.RawMessage, error) {
	resp, err := c.agent.Title(withKey(ctx, key), &agentv1.TitleRequest{RequestJson: string(request)})
	if err != nil {
		return nil, err
	}
	return json.RawMessage(resp.GetResultJson()), nil
}

// Probe checks an endpoint's models (a ProbeRequest), key the endpoint's:
// one unary call, which returns the ProbeResult or the agent's gRPC status.
// A check that failed is a result, not an error.
func (c *Client) Probe(ctx context.Context, request []byte, key ModelKey) (json.RawMessage, error) {
	resp, err := c.agent.Probe(withKey(ctx, key), &agentv1.ProbeRequest{RequestJson: string(request)})
	if err != nil {
		return nil, err
	}
	return json.RawMessage(resp.GetResultJson()), nil
}

// Clues streams the people a load could look up (clues.go): each event goes
// to onEvent until the one that ends the stream, which is returned, as a
// result ends the other streams. An error from onEvent ends the call.
// Cancelling stops it before its next chunk of rows; it writes nothing.
func (c *Client) Clues(ctx context.Context, request []byte, onEvent func(json.RawMessage) error) (json.RawMessage, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stream, err := c.agent.Clues(ctx, &agentv1.CluesRequest{RequestJson: string(request)})
	if err != nil {
		return nil, err
	}
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil, ErrNoResult
		}
		if err != nil {
			return nil, err
		}
		event := json.RawMessage(msg.GetEventJson())
		if name, _ := EventName(event); cluesEnding[name] {
			return event, nil
		}
		if err := onEvent(event); err != nil {
			return nil, err
		}
	}
}

// streamMessage is a message of one of the agent's streams: an event, or the
// result that ends it.
type streamMessage interface {
	GetEventJson() string
	GetResultJson() string
}

// readStream hands each event to onEvent until the result, which it returns.
func readStream[M streamMessage](recv func() (M, error), onEvent func(json.RawMessage) error) (json.RawMessage, error) {
	for {
		msg, err := recv()
		if errors.Is(err, io.EOF) {
			return nil, ErrNoResult
		}
		if err != nil {
			return nil, err
		}
		if result := msg.GetResultJson(); result != "" {
			return json.RawMessage(result), nil
		}
		if err := onEvent(json.RawMessage(msg.GetEventJson())); err != nil {
			return nil, err
		}
	}
}
