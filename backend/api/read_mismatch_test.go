package api

import (
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Only the engine's load_mismatch refusal names a version to reconcile to.
func TestLoadMismatch(t *testing.T) {
	for name, c := range map[string]struct {
		err     error
		version string
		ok      bool
	}{
		"the engine's refusal": {status.Error(codes.FailedPrecondition,
			"load_mismatch: the load was created with ontology version foundry-v8"), "foundry-v8", true},
		"none":                {nil, "", false},
		"another refusal":     {status.Error(codes.FailedPrecondition, "load_unknown: the graph holds no such load"), "", false},
		"no version":          {status.Error(codes.FailedPrecondition, "load_mismatch: the load was created with ontology version "), "", false},
		"not a status at all": {errors.New("load_mismatch: the load was created with ontology version foundry-v8"), "", false},
	} {
		t.Run(name, func(t *testing.T) {
			version, ok := loadMismatch(c.err)
			if version != c.version || ok != c.ok {
				t.Fatalf("loadMismatch = %q, %v; want %q, %v", version, ok, c.version, c.ok)
			}
		})
	}
}
