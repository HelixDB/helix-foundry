// Package contracttest checks JSON against the ontology agent's vendored
// schemas (pb/proto/ontology_agent/v1/schema), for the tests of whatever
// builds the agent's requests or reads its events and results.
package contracttest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

// schemaDir is the schemas' directory below the backend module's root.
var schemaDir = filepath.Join("pb", "proto", "ontology_agent", "v1", "schema")

// Dir is the vendored schemas' directory: the first one found going up from
// the test's working directory, which is its package's directory under go
// test.
func Dir(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if info, err := os.Stat(filepath.Join(dir, schemaDir)); err == nil && info.IsDir() {
			return filepath.Join(dir, schemaDir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no %s above the test's directory", schemaDir)
		}
		dir = parent
	}
}

// Schema is the vendored schema name, such as "ask_request.json", resolved.
func Schema(t testing.TB, name string) *jsonschema.Resolved {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(Dir(t), name))
	if err != nil {
		t.Fatal(err)
	}
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	resolved, err := s.Resolve(nil)
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	return resolved
}

// Validate reports whether data, which must be JSON, matches schema.
func Validate(t testing.TB, schema *jsonschema.Resolved, data []byte) error {
	t.Helper()
	var instance any
	if err := json.Unmarshal(data, &instance); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
	return schema.Validate(instance)
}
