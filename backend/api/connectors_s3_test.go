package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/filesource"
	foundryv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundry/v1"
)

// s3Stub stands in for the s3 kind: it keeps settings as given and finds
// one table, so the service's own handling of S3 configs is under test.
type s3Stub struct {
	connector.Kind
	roleARN string
}

func (s3Stub) Name() string        { return "s3" }
func (s3Stub) DisplayName() string { return "Amazon S3" }

func (s3Stub) Prepare(src connector.Source) (json.RawMessage, error) { return src.Settings, nil }

func (s3Stub) Discover(context.Context, connector.Source) (connector.Discovery, error) {
	return connector.Discovery{Tables: []connector.Table{{Name: "orders"}}, MaxBytes: 20 << 30}, nil
}

func (s3Stub) Hint(secret string) string {
	if secret == "" {
		return "IAM role"
	}
	return "secret …" + secret[len(secret)-4:]
}

func (k s3Stub) Setup(workspaceID string) (string, string, bool) {
	return k.roleARN, connector.S3ExternalID(workspaceID), k.roleARN != ""
}

func s3Service(t *testing.T, roleARN string) (*Service, *connectorStore) {
	t.Helper()
	s, store, keyring := connectorService(t, "http://unused", proPlan)
	var kinds []connector.Kind
	for _, k := range s.deps.ConnectorCatalog.Kinds() {
		if k.Name() == "s3" {
			k = s3Stub{roleARN: roleARN}
		}
		kinds = append(kinds, k)
	}
	s.deps.ConnectorCatalog, s.deps.ConnectorKeys = connector.NewCatalogOf(kinds...), keyring
	return s, store
}

func s3RoleConfig(arn string) *foundryv1.ConnectorConfig {
	return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_S3{S3: &foundryv1.S3ConnectorConfig{
		Bucket: "acme-exports", Prefix: "foundry/", Region: "eu-west-2", Tables: []string{"orders"},
		Auth: &foundryv1.S3ConnectorConfig_Role{Role: &foundryv1.S3RoleAuth{RoleArn: arn}},
	}}}
}

func s3KeyConfig(secret string) *foundryv1.ConnectorConfig {
	return &foundryv1.ConnectorConfig{Kind: &foundryv1.ConnectorConfig_S3{S3: &foundryv1.S3ConnectorConfig{
		Bucket: "acme-exports", Auth: &foundryv1.S3ConnectorConfig_Keys{Keys: &foundryv1.S3KeyAuth{
			AccessKeyId: "AKIAEXAMPLE", SecretAccessKey: secret, Endpoint: "https://store.example.com",
		}},
	}}}
}

func TestS3ConnectorKinds(t *testing.T) {
	ctx := context.Background()
	t.Run("offers role access with the workspace's external ID", func(t *testing.T) {
		s, _ := s3Service(t, "arn:aws:iam::111122223333:role/example-foundry-connector")
		resp, err := s.ListConnectorKinds(ctx, &foundryv1.ListConnectorKindsRequest{WorkspaceId: "ws-1"})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		var s3 *foundryv1.ConnectorKind
		for _, k := range resp.GetKinds() {
			names = append(names, k.GetKind())
			if k.GetKind() == "s3" {
				s3 = k
			} else if k.GetSetup() != nil {
				t.Fatalf("%s has a setup", k.GetKind())
			}
		}
		if strings.Join(names, ",") != "stripe,workos,posthog,github,linear,calcom,granola,slack,notion,google,postgres,mysql,s3,events" || !s3.GetAvailable() || !s3.GetRequiresPro() ||
			s3.GetS3().GetPrincipalArn() != "arn:aws:iam::111122223333:role/example-foundry-connector" ||
			s3.GetS3().GetExternalId() != "helix-foundry-ws-1" {
			t.Fatalf("kinds = %v, s3 = %+v", names, s3)
		}
	})
	t.Run("offers no setup when role access is off", func(t *testing.T) {
		s, _ := s3Service(t, "")
		resp, err := s.ListConnectorKinds(ctx, &foundryv1.ListConnectorKindsRequest{WorkspaceId: "ws-1"})
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range resp.GetKinds() {
			if k.GetSetup() != nil {
				t.Fatalf("%s has a setup", k.GetKind())
			}
		}
	})
}

func TestS3ConnectorConfigs(t *testing.T) {
	ctx := context.Background()
	// The response rebuilds the config from stored settings, so it can never carry the key.
	t.Run("stores settings without the secret", func(t *testing.T) {
		s, store := s3Service(t, "")
		resp, err := s.CreateConnector(ctx, &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Exports", Config: s3KeyConfig("  wJalrXUtnFEMI/K7MDENG  ")})
		if err != nil {
			t.Fatal(err)
		}
		c := resp.GetConnector()
		keys := c.GetConfig().GetS3().GetKeys()
		if keys.GetAccessKeyId() != "AKIAEXAMPLE" || keys.GetEndpoint() != "https://store.example.com" || keys.GetSecretAccessKey() != "" ||
			c.GetSecretHint() != "secret …DENG" || resp.GetDiscovery().GetMaxBytes() != 20<<30 {
			t.Fatalf("connector = %+v, discovery = %+v", c, resp.GetDiscovery())
		}
		stored := store.rows[c.GetId()]
		if strings.Contains(string(stored.Config), "wJalr") ||
			string(stored.Config) != `{"bucket":"acme-exports","prefix":"","keys":{"access_key_id":"AKIAEXAMPLE","endpoint":"https://store.example.com"}}` {
			t.Fatalf("stored settings = %s", stored.Config)
		}
	})
	t.Run("rebuilds a role config", func(t *testing.T) {
		s, _ := s3Service(t, "")
		resp, err := s.TestConnector(ctx, &foundryv1.TestConnectorRequest{WorkspaceId: "ws-1", Config: s3RoleConfig("arn:aws:iam::123456789012:role/helix")})
		if err != nil {
			t.Fatal(err)
		}
		got := resp.GetConfig().GetS3()
		if got.GetBucket() != "acme-exports" || got.GetPrefix() != "foundry/" || got.GetRegion() != "eu-west-2" ||
			got.GetRole().GetRoleArn() != "arn:aws:iam::123456789012:role/helix" || strings.Join(got.GetTables(), ",") != "orders" {
			t.Fatalf("config = %+v", got)
		}
	})
	// Moving to role access must not leave the old access key sealed in the row.
	t.Run("clears the key when moving to role access", func(t *testing.T) {
		s, store := s3Service(t, "")
		created, err := s.CreateConnector(ctx, &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Exports", Config: s3KeyConfig("secret-abcd")})
		if err != nil {
			t.Fatal(err)
		}
		id := created.GetConnector().GetId()
		resp, err := s.UpdateConnector(ctx, &foundryv1.UpdateConnectorRequest{WorkspaceId: "ws-1", Id: id, Config: s3RoleConfig("arn:aws:iam::123456789012:role/helix")})
		if err != nil {
			t.Fatal(err)
		}
		row := store.rows[id]
		secret, err := s.deps.ConnectorKeys.Open(row.SecretCiphertext, row.SecretKeyID, connector.SecretContext(id))
		if err != nil || len(secret) != 0 || resp.GetConnector().GetSecretHint() != "IAM role" {
			t.Fatalf("secret = (%q, %v), hint = %q", secret, err, resp.GetConnector().GetSecretHint())
		}
	})
	t.Run("keeps the stored key when an edit leaves it blank", func(t *testing.T) {
		s, store := s3Service(t, "")
		created, err := s.CreateConnector(ctx, &foundryv1.CreateConnectorRequest{WorkspaceId: "ws-1", Name: "Exports", Config: s3KeyConfig("secret-abcd")})
		if err != nil {
			t.Fatal(err)
		}
		id := created.GetConnector().GetId()
		resp, err := s.UpdateConnector(ctx, &foundryv1.UpdateConnectorRequest{WorkspaceId: "ws-1", Id: id, Config: s3KeyConfig("")})
		if err != nil {
			t.Fatal(err)
		}
		row := store.rows[id]
		secret, err := s.deps.ConnectorKeys.Open(row.SecretCiphertext, row.SecretKeyID, connector.SecretContext(id))
		if err != nil || string(secret) != "secret-abcd" || resp.GetConnector().GetSecretHint() != "secret …abcd" {
			t.Fatalf("secret = (%q, %v), hint = %q", secret, err, resp.GetConnector().GetSecretHint())
		}
	})
}

func TestS3Discovery(t *testing.T) {
	out := discoveryToProto(connector.Discovery{MaxBytes: 20 << 30, Tables: []connector.Table{{
		Name: "orders", PrimaryKey: []string{"id"}, Problem: "orders/p.json record 2: not a JSON object",
		Columns: []connector.Column{{Name: "id", Type: "integer"}},
		Files: []connector.Object{
			{Key: "e2e/orders/a.json", Size: 100, Format: filesource.FormatJSON},
			{Key: "e2e/orders/b.json", Size: 50, Format: filesource.FormatJSON},
		},
		Preview: []json.RawMessage{json.RawMessage(`{"id":1}`), json.RawMessage(`{"id":2}`)},
	}}})
	table := out.GetTables()[0]
	if out.GetMaxBytes() != 20<<30 || table.GetFormat() != "json" || table.GetFileCount() != 2 || table.GetBytes() != 150 ||
		strings.Join(table.GetPreview(), ",") != `{"id":1},{"id":2}` || table.GetProblem() != "orders/p.json record 2: not a JSON object" {
		t.Fatalf("discovery = %+v", out)
	}
	api := discoveryToProto(connector.Discovery{Tables: []connector.Table{{Name: "customers"}}}).GetTables()[0]
	if api.GetFormat() != "" || api.GetFileCount() != 0 || api.GetBytes() != 0 || api.GetPreview() != nil {
		t.Fatalf("an API table = %+v", api)
	}
}

// The s3 kind holds a test's columns to 1 MiB and its previews to 256 KiB
// (connector's sampling budget); with every other field at its largest,
// the answer stays well inside gRPC's 4 MiB default between wfe and backend.
func TestS3DiscoveryWorstCaseSize(t *testing.T) {
	const columnBudget, previewBudget = 1 << 20, 256 << 10
	d := connector.Discovery{MaxBytes: 20 << 30}
	columnBytes, previewBytes := 0, 0
	row := json.RawMessage(`{"v":"` + strings.Repeat("x", 4<<10-8) + `"}`)
	for i := range 200 {
		table := connector.Table{Name: strings.Repeat("t", 250) + fmt.Sprintf("%03d", i), PrimaryKey: []string{"id"},
			Problem: strings.Repeat("p", 300), Files: make([]connector.Object, 50)}
		for c := 0; c < 1000; c++ {
			col := connector.Column{Name: fmt.Sprintf("column_%04d_%s", c, strings.Repeat("n", 40)), Type: "timestamptz", Nullable: true}
			if size := len(col.Name) + len(col.Type) + 16; columnBytes+size <= columnBudget {
				columnBytes += size
				table.Columns = append(table.Columns, col)
			}
		}
		for len(table.Preview) < 5 && previewBytes+len(row) <= previewBudget {
			previewBytes += len(row)
			table.Preview = append(table.Preview, row)
		}
		d.Tables = append(d.Tables, table)
	}
	for i := range 51 {
		d.Unavailable = append(d.Unavailable, connector.Unavailable{Name: strings.Repeat("u", 1024) + fmt.Sprint(i), Reason: "is 3.1 GB, over the 2 GB limit"})
	}
	if size := proto.Size(discoveryToProto(d)); size >= 2<<20 {
		t.Fatalf("the largest test answer is %d bytes", size)
	}
}
