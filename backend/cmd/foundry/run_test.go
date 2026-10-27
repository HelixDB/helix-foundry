package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/helixdb/helix-foundry/backend/local"
	"github.com/helixdb/helix-foundry/backend/store"
)

// part records its start and stop; it runs until its context ends, or
// fails at once with fail.
type part struct {
	name    string
	fail    error
	started chan struct{}
	log     *events
}

type events struct {
	mu   sync.Mutex
	list []string
}

func (e *events) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, s)
}

func (e *events) get() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.list)
}

func (p *part) Name() string { return p.name }

func (p *part) Start(ctx context.Context) error {
	close(p.started)
	if p.fail != nil {
		return p.fail
	}
	<-ctx.Done()
	return ctx.Err()
}

func (p *part) Stop(context.Context) error {
	p.log.add("stop " + p.name)
	return nil
}

func newParts(log *events, fail error, names ...string) []component {
	var parts []component
	for i, n := range names {
		p := &part{name: n, started: make(chan struct{}), log: log}
		if i == len(names)-1 {
			p.fail = fail
		}
		parts = append(parts, p)
	}
	return parts
}

func get(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// serve runs every part and the health listener, reports ready, and on
// shutdown stops the workers before the front end.
func TestServe(t *testing.T) {
	var pingErr error
	var mu sync.Mutex
	probes := newProbes(func(context.Context) error { mu.Lock(); defer mu.Unlock(); return pingErr },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + ln.Addr().String()
	log := &events{}
	parts := newParts(log, nil, "foundry", "foundry-read", "foundry-sync-worker")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, slog.New(slog.NewTextHandler(io.Discard, nil)), probes, ln, parts) }()
	for _, p := range parts {
		<-p.(*part).started
	}
	if code := get(t, url+"/healthz"); code != http.StatusOK {
		t.Fatalf("healthz = %d", code)
	}
	if code := get(t, url+"/metrics"); code != http.StatusOK {
		t.Fatalf("metrics = %d", code)
	}
	for deadline := time.Now().Add(5 * time.Second); get(t, url+"/readyz") != http.StatusOK; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("never ready")
		}
	}
	// Not ready while the database does not answer.
	mu.Lock()
	pingErr = errors.New("db down")
	mu.Unlock()
	if code := get(t, url+"/readyz"); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz with the db down = %d", code)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := log.get(); !slices.Equal(got, []string{"stop foundry-sync-worker", "stop foundry-read", "stop foundry"}) {
		t.Fatalf("stops = %v", got)
	}
}

// A part that fails stops the install, with its error.
func TestServeStopsWhenAPartFails(t *testing.T) {
	probes := newProbes(func(context.Context) error { return nil }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	log := &events{}
	boom := errors.New("boom")
	err = serve(context.Background(), slog.New(slog.NewTextHandler(io.Discard, nil)), probes, ln,
		newParts(log, boom, "foundry", "foundry-ask-worker"))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if got := log.get(); !slices.Equal(got, []string{"stop foundry-ask-worker", "stop foundry"}) {
		t.Fatalf("stops = %v", got)
	}
}

// The image's configuration builds foundry-read and every worker an install
// runs, from only what an install provides them.
func TestBuildWorkers(t *testing.T) {
	secrets(t)
	cfg, err := load("foundry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	parts, err := buildWorkers(cfg.foundry(), logger, newProbes(nil, logger), store.Unimplemented{}, aws.Config{Region: "us-east-1"},
		local.Databases{HelixURL: cfg.Local.HelixURL})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range parts {
		names = append(names, p.Name())
	}
	slices.Sort(names)
	want := []string{"foundry-ask-worker", "foundry-document-worker", "foundry-graph-worker", "foundry-ontology-worker", "foundry-read", "foundry-sync-worker"}
	if !slices.Equal(names, want) {
		t.Fatalf("parts = %v, want %v", names, want)
	}
}
