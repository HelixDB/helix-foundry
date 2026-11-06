package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/helixdb/helix-foundry/backend/ontologyagent"
)

// runCheckAgent asks the sidecar for its contract, without starting Foundry or
// calling a model. A loopback-only address keeps this plaintext call local.
func runCheckAgent(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("check-agent", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	addr := flags.String("addr", "127.0.0.1:50051", "agent loopback address")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments")
	}
	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return fmt.Errorf("invalid agent address: %w", err)
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("agent address must be loopback")
	}
	client, err := ontologyagent.Dial(*addr)
	if err != nil {
		return err
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := client.Info(ctx)
	if err != nil {
		return fmt.Errorf("agent unavailable at %s: %w", *addr, err)
	}
	if info.GetContractVersion() != ontologyagent.ContractVersion {
		return fmt.Errorf("agent contract %q does not match backend contract %q; use a matching agent image", info.GetContractVersion(), ontologyagent.ContractVersion)
	}
	_, err = fmt.Fprintf(out, "ontology agent contract %s matches backend\n", info.GetContractVersion())
	return err
}
