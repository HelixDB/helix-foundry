package main

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
	agentv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/ontology_agent/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type infoAgent struct {
	agentv1.UnimplementedAgentServer
	version string
}

func (a infoAgent) GetInfo(context.Context, *agentv1.GetInfoRequest) (*agentv1.GetInfoResponse, error) {
	if a.version == "unavailable" {
		return nil, status.Error(codes.Unavailable, "not serving")
	}
	return &agentv1.GetInfoResponse{ContractVersion: a.version}, nil
}

func TestCheckAgent(t *testing.T) {
	for _, tc := range []struct {
		name, version, want string
	}{
		{"matching", ontologyagent.ContractVersion, ""},
		{"mismatch", "0.0.0", "does not match"},
		{"empty version", "", "does not match"},
		{"unavailable", "unavailable", "agent unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server := grpc.NewServer()
			agentv1.RegisterAgentServer(server, infoAgent{version: tc.version})
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(server.Stop)
			var out bytes.Buffer
			err = runCheckAgent([]string{"--addr", listener.Addr().String()}, &out)
			if tc.want == "" {
				if err != nil || !strings.Contains(out.String(), ontologyagent.ContractVersion) {
					t.Fatalf("out %q, error %v", out.String(), err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
		})
	}
}

func TestCheckAgentRejectsNonLoopback(t *testing.T) {
	for _, addr := range []string{"example.com:50051", "192.0.2.1:50051", "not-a-hostport"} {
		if err := runCheckAgent([]string{"--addr", addr}, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted %q", addr)
		}
	}
}
