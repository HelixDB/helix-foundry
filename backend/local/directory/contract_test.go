package directory_test

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// swagger is the part of an OpenAPI 2 document the comparison reads.
type swagger struct {
	Paths       map[string]map[string]operation `json:"paths"`
	Definitions map[string]*schema              `json:"definitions"`
}

type operation struct {
	OperationID string      `json:"operationId"`
	Parameters  []parameter `json:"parameters"`
	Responses   map[string]struct {
		Schema *schema `json:"schema"`
	} `json:"responses"`
}

type parameter struct {
	Name   string   `json:"name"`
	In     string   `json:"in"`
	Type   string   `json:"type"`
	Format string   `json:"format"`
	Enum   []string `json:"enum"`
	Schema *schema  `json:"schema"`
}

type schema struct {
	Ref        string             `json:"$ref"`
	Type       string             `json:"type"`
	Format     string             `json:"format"`
	Enum       []string           `json:"enum"`
	Items      *schema            `json:"items"`
	Properties map[string]*schema `json:"properties"`
}

func readSwagger(t *testing.T, path string) swagger {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s swagger
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s swagger) resolve(sc *schema) *schema {
	for sc != nil && sc.Ref != "" {
		sc = s.Definitions[strings.TrimPrefix(sc.Ref, "#/definitions/")]
	}
	return sc
}

// sameShape reports where ours is not theirs: every property ours has, they
// have with the same JSON type, format and enum values, all the way down.
// An enum of ours may have fewer values than theirs.
func sameShape(path string, ours swagger, o *schema, theirs swagger, w *schema) []string {
	o, w = ours.resolve(o), theirs.resolve(w)
	if o == nil || w == nil {
		if o != w {
			return []string{path + ": missing"}
		}
		return nil
	}
	var diffs []string
	if o.Type != w.Type || o.Format != w.Format {
		diffs = append(diffs, path+": "+o.Type+"/"+o.Format+", the WFE's "+w.Type+"/"+w.Format)
	}
	for _, v := range o.Enum {
		if !slices.Contains(w.Enum, v) {
			diffs = append(diffs, path+": enum value "+v+" is not the WFE's")
		}
	}
	if o.Items != nil {
		diffs = append(diffs, sameShape(path+"[]", ours, o.Items, theirs, w.Items)...)
	}
	for name, p := range o.Properties {
		wp, ok := w.Properties[name]
		if !ok {
			diffs = append(diffs, path+"."+name+": not in the WFE's")
			continue
		}
		diffs = append(diffs, sameShape(path+"."+name, ours, p, theirs, wp)...)
	}
	return diffs
}

// The directory serves the WFE's routes the Foundry site calls, with their
// JSON: testdata/wfe.swagger.json is those routes of the site's
// contract/wfe.swagger.json. Each of the directory's operations is the WFE's
// path and verb, takes the WFE's parameters, and answers with a subset of the
// WFE's fields, each of the same shape.
func TestServesTheWFEsRoutes(t *testing.T) {
	ours := readSwagger(t, "../../pb/gen/openapiv2/helix/foundry/directory/v1/directory.swagger.json")
	theirs := readSwagger(t, "testdata/wfe.swagger.json")

	ops := 0
	for path, verbs := range ours.Paths {
		for verb, op := range verbs {
			ops++
			name := verb + " " + path
			wop, ok := theirs.Paths[path][verb]
			if !ok {
				t.Errorf("%s: the WFE has no such route", name)
				continue
			}
			if strings.TrimPrefix(op.OperationID, "DirectoryService_") != strings.TrimPrefix(wop.OperationID, "WfeService_") {
				t.Errorf("%s is %s, the WFE's %s", name, op.OperationID, wop.OperationID)
			}
			for _, p := range op.Parameters {
				i := slices.IndexFunc(wop.Parameters, func(w parameter) bool { return w.Name == p.Name && w.In == p.In })
				if i < 0 {
					t.Errorf("%s: parameter %s (%s) is not the WFE's", name, p.Name, p.In)
					continue
				}
				w := wop.Parameters[i]
				if p.Type != w.Type || p.Format != w.Format || !reflect.DeepEqual(p.Enum, w.Enum) && len(p.Enum) > 0 {
					t.Errorf("%s: parameter %s is %s/%s, the WFE's %s/%s", name, p.Name, p.Type, p.Format, w.Type, w.Format)
				}
				for _, d := range sameShape(name+" body", ours, p.Schema, theirs, w.Schema) {
					t.Error(d)
				}
			}
			for _, d := range sameShape(name+" response", ours, op.Responses["200"].Schema, theirs, wop.Responses["200"].Schema) {
				t.Error(d)
			}
		}
	}
	if ops != 9 {
		t.Errorf("the directory has %d operations, want 9", ops)
	}

	// And it has what the site reads of each answer.
	for route, fields := range map[[2]string][]string{
		{"/v1/whoami", "get"}:                {"user.id", "user.email", "user.displayName", "workspaces[].workspaceId", "workspaces[].role", "workspaces[].user.id"},
		{"/v1/workspaces", "get"}:            {"workspaces[].id", "workspaces[].slug", "workspaces[].displayName", "workspaces[].region", "workspaces[].status", "nextPageToken"},
		{"/v1/workspaces", "post"}:           {"id", "slug", "displayName", "status", "timestamps.createdAt"},
		{"/v1/workspaces/{id}", "get"}:       {"id", "slug", "displayName", "status"},
		{"/v1/projects", "get"}:              {"projects[].id", "projects[].workspaceId", "projects[].slug", "projects[].displayName", "nextPageToken"},
		{"/v1/projects", "post"}:             {"id", "workspaceId", "slug", "displayName"},
		{"/v1/tenants", "get"}:               {"tenants[].id", "tenants[].projectId", "tenants[].workspaceId", "tenants[].name", "tenants[].slug", "tenants[].status", "tenants[].queryState", "nextPageToken"},
		{"/v1/tenants", "post"}:              {"tenant.id", "tenant.name", "tenant.slug"},
		{"/v1/databases:query-read", "post"}: {"responseJson", "statusCode", "contentType"},
	} {
		sc := ours.Paths[route[0]][route[1]].Responses["200"].Schema
		for _, field := range fields {
			if !has(ours, sc, field) {
				t.Errorf("%s %s answers no %s", route[1], route[0], field)
			}
		}
	}
}

// has reports whether field, a dotted path with [] for an array's items, is
// in sc.
func has(s swagger, sc *schema, field string) bool {
	for _, name := range strings.Split(field, ".") {
		sc = s.resolve(sc)
		if sc == nil {
			return false
		}
		items := strings.HasSuffix(name, "[]")
		sc = sc.Properties[strings.TrimSuffix(name, "[]")]
		if items {
			if sc = s.resolve(sc); sc == nil || sc.Type != "array" {
				return false
			}
			sc = sc.Items
		}
	}
	return s.resolve(sc) != nil
}
