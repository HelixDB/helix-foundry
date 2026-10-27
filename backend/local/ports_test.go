package local

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/helixdb/helix-foundry/backend/ports"
	"github.com/helixdb/helix-foundry/backend/store"
)

func TestDatabases(t *testing.T) {
	ctx := context.Background()
	dbs := Databases{Directory: testDirectory(), HelixURL: "http://helix:8080/"}

	got, err := dbs.Get(ctx, "t")
	want := ports.Database{TenantID: "t", WorkspaceID: "ws", ProjectID: "p", Name: "Shop"}
	if err != nil || got != want {
		t.Fatalf("get = %+v, %v", got, err)
	}
	// Get reads a database as it is: deleted, or with its lifecycle retired.
	if got, err := dbs.Get(ctx, "deleted"); err != nil || !got.Deleted || got.Retired {
		t.Fatalf("deleted = %+v, %v", got, err)
	}
	if got, err := dbs.Get(ctx, "retired"); err != nil || got.Deleted || !got.Retired {
		t.Fatalf("retired = %+v, %v", got, err)
	}
	if _, err := dbs.Get(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("absent: %v", err)
	}

	if got, err := dbs.Live(ctx, "t"); err != nil || got != want {
		t.Fatalf("live = %+v, %v", got, err)
	}
	for id, code := range map[string]codes.Code{
		"nope": codes.NotFound, "deleted": codes.NotFound, "retired": codes.NotFound, "orphan": codes.FailedPrecondition,
	} {
		if _, err := dbs.Live(ctx, id); status.Code(err) != code {
			t.Errorf("live %s: %v, want %s", id, err, code)
		}
	}

	// Every database is queried at the install's one HelixDB.
	if url, err := dbs.Endpoint(ctx, "t", "/v2/query"); err != nil || url != "http://helix:8080/v2/query" {
		t.Fatalf("endpoint = %q, %v", url, err)
	}
	for id, code := range map[string]codes.Code{"nope": codes.NotFound, "deleted": codes.NotFound, "retired": codes.NotFound} {
		if _, err := dbs.Endpoint(ctx, id, "/v2/query"); status.Code(err) != code {
			t.Errorf("endpoint %s: %v, want %s", id, err, code)
		}
	}
	if _, err := dbs.Endpoint(ctx, "t", "v2/query"); status.Code(err) != codes.Internal {
		t.Errorf("a relative path: %v", err)
	}

	broken := testDirectory()
	broken.err = errors.New("db down")
	dbs.Directory = broken
	if _, err := dbs.Live(ctx, "t"); status.Code(err) != codes.Internal {
		t.Errorf("live, db down: %v", err)
	}
	if _, err := dbs.Endpoint(ctx, "t", "/v2/query"); status.Code(err) != codes.Unavailable {
		t.Errorf("endpoint, db down: %v", err)
	}
}

func TestPeople(t *testing.T) {
	ctx := context.Background()
	people := People{Directory: testDirectory()}
	for _, tc := range []struct {
		workspace, user string
		want            bool
	}{
		{"ws", "owner", true},
		{"deleting", "owner", false},
		{"gone", "owner", false},
		{"nope", "owner", false},
		{"ws", "nameless", false},
		{"ws", "", false},
	} {
		if got := people.MayManageAI(ctx, tc.workspace, tc.user); got != tc.want {
			t.Errorf("MayManageAI(%s, %s) = %v", tc.workspace, tc.user, got)
		}
	}
	if name, err := people.DisplayName(ctx, "owner"); err != nil || name != "Owner" {
		t.Fatalf("owner = %q, %v", name, err)
	}
	if name, err := people.DisplayName(ctx, "nameless"); err != nil || name != "n@localhost" {
		t.Fatalf("nameless = %q, %v", name, err)
	}
	if _, err := people.DisplayName(ctx, "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("absent: %v", err)
	}
	// Any doubt is false.
	broken := testDirectory()
	broken.err = errors.New("db down")
	if (People{Directory: broken}).MayManageAI(ctx, "ws", "owner") {
		t.Fatal("may manage with the directory down")
	}
}

func TestAWSConfig(t *testing.T) {
	cfg := AWSConfig("us-east-1", S3{Endpoint: "http://localhost:8333", AccessKeyID: "foundry", SecretAccessKey: "secret"})
	o := s3.NewFromConfig(cfg).Options()
	if aws.ToString(o.BaseEndpoint) != "http://localhost:8333" || !o.UsePathStyle || o.Region != "us-east-1" {
		t.Fatalf("options = %s %v %s", aws.ToString(o.BaseEndpoint), o.UsePathStyle, o.Region)
	}
	creds, err := cfg.Credentials.Retrieve(context.Background())
	if err != nil || creds.AccessKeyID != "foundry" || creds.SecretAccessKey != "secret" {
		t.Fatalf("credentials = %+v, %v", creds, err)
	}
	if cfg.RequestChecksumCalculation != aws.RequestChecksumCalculationWhenRequired {
		t.Fatal("checksums are calculated when not required")
	}
}

type bucketer struct{ err error }

func (b bucketer) CreateBucket(context.Context, *s3.CreateBucketInput, ...func(*s3.Options)) (*s3.CreateBucketOutput, error) {
	return &s3.CreateBucketOutput{}, b.err
}

func TestCreateBucket(t *testing.T) {
	ctx := context.Background()
	for name, err := range map[string]error{
		"new":    nil,
		"owned":  &types.BucketAlreadyOwnedByYou{},
		"exists": &types.BucketAlreadyExists{},
	} {
		if err := CreateBucket(ctx, bucketer{err: err}, "foundry-data"); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := CreateBucket(ctx, bucketer{err: errors.New("denied")}, "foundry-data"); err == nil {
		t.Fatal("a failed creation passed")
	}
}
