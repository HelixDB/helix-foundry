package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	_ "github.com/go-sql-driver/mysql"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/helixdb/helix-foundry/backend/local"
	"github.com/helixdb/helix-foundry/backend/local/directory"
	"github.com/helixdb/helix-foundry/backend/store"
)

// freeAddr is a loopback address no one listens on, for a listener the test
// starts next.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// startFront migrates a fresh database on the test MySQL server
// (HELIX_TEST_ROOT_DSN, a root DSN ending in "/"), seeds the owner and
// serves the front end on loopback with the image's configuration, as run
// does. Without a server the test skips; there is no default.
func startFront(t *testing.T, helixURL string) (rest string, grpcAddr string, probes *probes, db *sql.DB) {
	t.Helper()
	rootDSN := os.Getenv("HELIX_TEST_ROOT_DSN")
	if rootDSN == "" {
		t.Skip("no test MySQL: set HELIX_TEST_ROOT_DSN")
	}
	const schema = "foundry_cmd_test"
	root, err := sql.Open("mysql", rootDSN)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{"DROP DATABASE IF EXISTS " + schema, "CREATE DATABASE " + schema} {
		if _, err := root.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	db, err = openDB(ctx, rootDSN+schema)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	dir := directory.NewStore(db)
	if err := dir.SeedOwner(ctx); err != nil {
		t.Fatal(err)
	}
	// The image's configuration, on loopback listeners of the test's own.
	secrets(t)
	cfg, err := load("foundry.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Local.HelixURL = helixURL
	cfg.Local.Listen.REST, cfg.Local.Listen.GRPC = freeAddr(t), freeAddr(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	probes = newProbes(db.PingContext, logger)
	f, err := newFront(ctx, cfg, logger, probes, dir, store.NewMySQL(db, local.Workspaces{}), aws.Config{Region: "us-east-1"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = f.Start(ctx); close(done) }()
	t.Cleanup(func() {
		_ = f.Stop(context.Background())
		cancel()
		<-done
		_ = db.Close()
		_, _ = root.Exec("DROP DATABASE IF EXISTS " + schema)
		_ = root.Close()
	})
	return "http://" + cfg.Local.Listen.REST, cfg.Local.Listen.GRPC, probes, db
}

// call sends a JSON request to the REST gateway, as the site's /papi does,
// and decodes the answer into out.
func call(t *testing.T, method, url, token string, body any, out any) int {
	t.Helper()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	var resp *http.Response
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if resp, err = http.DefaultClient.Do(req); err == nil || time.Now().After(deadline) {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: %d %s: %v", method, url, resp.StatusCode, raw, err)
		}
	}
	return resp.StatusCode
}

// The site's calls of the directory, over REST with the site token, answer
// with the WFE's JSON: camelCase names, enum names, int64s and bytes as
// strings.
func TestFrontServesTheDirectory(t *testing.T) {
	helix := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tenant":"` + r.Header.Get("X-Helix-Tenant-ID") + `"}`))
	}))
	defer helix.Close()
	rest, _, _, _ := startFront(t, helix.URL)

	var refused map[string]any
	if code := call(t, "GET", rest+"/v1/whoami", "", nil, &refused); code != http.StatusUnauthorized || refused["code"] != float64(16) {
		t.Fatalf("no token: %d %v", code, refused)
	}
	if code := call(t, "GET", rest+"/v1/whoami", "wrong", nil, &refused); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d %v", code, refused)
	}

	var me struct {
		User struct {
			ID, Email, DisplayName string
		}
		Workspaces []map[string]any
	}
	if code := call(t, "GET", rest+"/v1/whoami", siteToken, nil, &me); code != http.StatusOK ||
		me.User.Email != directory.OwnerEmail || me.User.DisplayName != "Owner" || me.User.ID == "" {
		t.Fatalf("whoami: %d %+v", code, me)
	}

	var ws map[string]any
	if code := call(t, "POST", rest+"/v1/workspaces", siteToken, map[string]any{"slug": "acme", "displayName": "Acme", "region": "local"}, &ws); code != http.StatusOK {
		t.Fatalf("create workspace: %d %v", code, ws)
	}
	if ws["slug"] != "acme" || ws["displayName"] != "Acme" || ws["status"] != "RESOURCE_STATUS_ACTIVE" || ws["region"] != "local" {
		t.Fatalf("workspace = %v", ws)
	}
	if created, _ := ws["timestamps"].(map[string]any)["createdAt"].(string); !strings.HasSuffix(created, "Z") {
		t.Fatalf("createdAt = %v", ws["timestamps"])
	}
	wsID := ws["id"].(string)
	var taken map[string]any
	if code := call(t, "POST", rest+"/v1/workspaces", siteToken, map[string]any{"slug": "acme", "region": "local"}, &taken); code != http.StatusConflict || taken["code"] != float64(6) {
		t.Fatalf("taken slug: %d %v", code, taken)
	}

	if code := call(t, "GET", rest+"/v1/whoami", siteToken, nil, &me); code != http.StatusOK || len(me.Workspaces) != 1 ||
		me.Workspaces[0]["workspaceId"] != wsID || me.Workspaces[0]["role"] != "WORKSPACE_ROLE_OWNER" {
		t.Fatalf("memberships: %d %+v", code, me.Workspaces)
	}
	var list struct {
		Workspaces    []map[string]any
		NextPageToken *string
	}
	if code := call(t, "GET", rest+"/v1/workspaces?page.size=200", siteToken, nil, &list); code != http.StatusOK ||
		len(list.Workspaces) != 1 || list.NextPageToken == nil || *list.NextPageToken != "" {
		t.Fatalf("list workspaces: %d %+v", code, list)
	}
	var got map[string]any
	if code := call(t, "GET", rest+"/v1/workspaces/"+wsID, siteToken, nil, &got); code != http.StatusOK || got["id"] != wsID {
		t.Fatalf("get workspace: %d %v", code, got)
	}

	var project map[string]any
	if code := call(t, "POST", rest+"/v1/projects", siteToken, map[string]any{"workspaceId": wsID, "slug": "crm", "displayName": "CRM"}, &project); code != http.StatusOK ||
		project["workspaceId"] != wsID || project["displayName"] != "CRM" {
		t.Fatalf("create project: %d %v", code, project)
	}
	var elsewhere map[string]any
	if code := call(t, "POST", rest+"/v1/projects", siteToken, map[string]any{"workspaceId": "01J00000000000000000000000", "slug": "crm"}, &elsewhere); code != http.StatusForbidden {
		t.Fatalf("another workspace's project: %d %v", code, elsewhere)
	}
	var projects struct{ Projects []map[string]any }
	if code := call(t, "GET", rest+"/v1/projects?workspaceId="+wsID+"&page.size=100", siteToken, nil, &projects); code != http.StatusOK || len(projects.Projects) != 1 {
		t.Fatalf("list projects: %d %+v", code, projects)
	}

	var made struct{ Tenant map[string]any }
	if code := call(t, "POST", rest+"/v1/tenants", siteToken, map[string]any{"projectId": project["id"], "name": "Shop", "slug": "shop"}, &made); code != http.StatusOK ||
		made.Tenant["queryState"] != "TENANT_QUERY_STATE_ENABLED" || made.Tenant["status"] != "RESOURCE_STATUS_ACTIVE" {
		t.Fatalf("create tenant: %d %+v", code, made)
	}
	if code := call(t, "POST", rest+"/v1/tenants", siteToken, map[string]any{"projectId": project["id"], "name": "Shop", "slug": "shop"}, &taken); code != http.StatusConflict || taken["code"] != float64(6) {
		t.Fatalf("taken database slug: %d %v", code, taken)
	}
	var tenants struct{ Tenants []map[string]any }
	if code := call(t, "GET", rest+"/v1/tenants?projectId="+project["id"].(string)+"&page.size=100", siteToken, nil, &tenants); code != http.StatusOK || len(tenants.Tenants) != 1 {
		t.Fatalf("list tenants: %d %+v", code, tenants)
	}

	tenantID := made.Tenant["id"].(string)
	var answer struct {
		ResponseJSON string `json:"responseJson"`
		StatusCode   int    `json:"statusCode"`
		ContentType  string `json:"contentType"`
	}
	query := base64.StdEncoding.EncodeToString([]byte(`{"request_type":"read","query":{}}`))
	if code := call(t, "POST", rest+"/v1/databases:query-read", siteToken, map[string]any{"database": map[string]any{"tenantId": tenantID}, "queryJson": query}, &answer); code != http.StatusOK {
		t.Fatalf("query: %d %+v", code, answer)
	}
	body, _ := base64.StdEncoding.DecodeString(answer.ResponseJSON)
	if answer.StatusCode != 200 || answer.ContentType != "application/json" || string(body) != `{"tenant":"`+tenantID+`"}` {
		t.Fatalf("answer = %+v %s", answer, body)
	}
}

// The site's calls of Foundry's API go to FoundryService, behind the same
// site token, and a method an install does not serve is refused before it
// runs.
func TestFrontServesFoundry(t *testing.T) {
	rest, _, _, _ := startFront(t, "http://helix:8080")
	var ws map[string]any
	if code := call(t, "POST", rest+"/v1/workspaces", siteToken, map[string]any{"slug": "acme", "displayName": "Acme", "region": "local"}, &ws); code != http.StatusOK {
		t.Fatalf("create workspace: %d %v", code, ws)
	}
	kindsURL := rest + "/v1/workspaces/" + ws["id"].(string) + "/foundry/connector-kinds"
	var refused map[string]any
	if code := call(t, "GET", kindsURL, "", nil, &refused); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d %v", code, refused)
	}
	var kinds struct{ Kinds []map[string]any }
	if code := call(t, "GET", kindsURL, siteToken, nil, &kinds); code != http.StatusOK || len(kinds.Kinds) == 0 {
		t.Fatalf("connector kinds: %d %+v", code, kinds)
	}
	for _, k := range kinds.Kinds {
		// The install's one plan has every kind.
		if k["available"] != true || k["requiresPro"] == true {
			t.Fatalf("kind %v is not available", k)
		}
	}
	var other map[string]any
	if code := call(t, "GET", rest+"/v1/workspaces/01J00000000000000000000000/foundry/connector-kinds", siteToken, nil, &other); code != http.StatusForbidden {
		t.Fatalf("another workspace: %d %v", code, other)
	}
	var plans map[string]any
	if code := call(t, "GET", rest+"/v1/foundry/plans", siteToken, nil, &plans); code != http.StatusBadRequest ||
		!strings.Contains(fmt.Sprint(plans["details"]), "LOCAL_MODE_UNSUPPORTED") {
		t.Fatalf("plans: %d %v", code, plans)
	}
}

// The health service answers without the token, on gRPC and on the REST
// listener's /readyz, which is ready once everything has started.
func TestFrontHealth(t *testing.T) {
	rest, grpcAddr, probes, _ := startFront(t, "http://helix:8080")
	if code := call(t, "GET", rest+"/readyz", "", nil, nil); code != http.StatusServiceUnavailable {
		t.Fatalf("readyz before start: %d", code)
	}
	probes.markReady()
	if code := call(t, "GET", rest+"/readyz", "", nil, nil); code != http.StatusOK {
		t.Fatalf("readyz: %d", code)
	}
	conn, err := grpc.NewClient(grpcAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health = %v, %v", resp, err)
	}
}
