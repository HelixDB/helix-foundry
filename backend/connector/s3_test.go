package connector

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"github.com/aws/aws-sdk-go-v2/aws"

	"github.com/helixdb/helix-foundry/backend/config"
	"github.com/helixdb/helix-foundry/backend/filesource"
	"github.com/helixdb/helix-foundry/backend/snapshot"
)

func invalidMessage(t *testing.T, err error) string {
	t.Helper()
	var e *InvalidError
	if !errors.As(err, &e) {
		t.Fatalf("error = %v, want an InvalidError", err)
	}
	return e.Message
}

func refusedMessage(t *testing.T, err error) string {
	t.Helper()
	var e *RefusedError
	if !errors.As(err, &e) {
		t.Fatalf("error = %v, want a RefusedError", err)
	}
	return e.Message
}

func TestS3Prepare(t *testing.T) {
	h := newS3Harness(t, nil)
	role := `"role":{"arn":"` + customerRole + `"}`
	keys := `"keys":{"access_key_id":"AKIAEXAMPLE"}`
	for name, tc := range map[string]struct {
		settings, secret string
		want             string // the prepared settings, or the error message
	}{
		"role":                 {`{"bucket":" acme-exports ","prefix":"/exports/daily","region":"eu-west-2",` + role + `}`, "", `{"bucket":"acme-exports","prefix":"exports/daily/","region":"eu-west-2","role":{"arn":"` + customerRole + `"}}`},
		"whole bucket":         {`{"bucket":"acme.exports",` + role + `}`, "", `{"bucket":"acme.exports","prefix":"","role":{"arn":"` + customerRole + `"}}`},
		"keys":                 {`{"bucket":"acme-exports",` + keys + `,"tables":[" a ","","a","b"]}`, "secret", `{"bucket":"acme-exports","prefix":"","keys":{"access_key_id":"AKIAEXAMPLE"},"tables":["a","b"]}`},
		"endpoint":             {`{"bucket":"acme-exports","keys":{"access_key_id":"AKIA","endpoint":" https://R2.example.com:8443/ "}}`, "s", `{"bucket":"acme-exports","prefix":"","keys":{"access_key_id":"AKIA","endpoint":"https://R2.example.com:8443"}}`},
		"temporary key at R2":  {`{"bucket":"acme-exports","keys":{"access_key_id":"ASIAX","endpoint":"https://r2.example.com"}}`, "s", `{"bucket":"acme-exports","prefix":"","keys":{"access_key_id":"ASIAX","endpoint":"https://r2.example.com"}}`},
		"malformed":            {`[`, "", "S3 settings are malformed"},
		"no bucket":            {`{` + role + `}`, "", msgBucket},
		"short bucket":         {`{"bucket":"ab",` + role + `}`, "", msgBucket},
		"capitals":             {`{"bucket":"Acme",` + role + `}`, "", msgBucket},
		"double dot":           {`{"bucket":"acme..exports",` + role + `}`, "", msgBucket},
		"ip bucket":            {`{"bucket":"192.168.10.1",` + role + `}`, "", msgBucket},
		"helix bucket":         {`{"bucket":"example-foundry-data",` + role + `}`, "", "example-foundry-data is a Helix bucket"},
		"long prefix":          {`{"bucket":"acme-exports","prefix":"` + strings.Repeat("a", 1024) + `",` + role + `}`, "", "the folder must be at most 1024 bytes"},
		"bad region":           {`{"bucket":"acme-exports","region":"europe",` + role + `}`, "", msgRegion},
		"china":                {`{"bucket":"acme-exports","region":"cn-north-1",` + role + `}`, "", msgRegion},
		"govcloud":             {`{"bucket":"acme-exports","region":"us-gov-west-1",` + role + `}`, "", msgRegion},
		"iso":                  {`{"bucket":"acme-exports","region":"us-isob-east-1",` + role + `}`, "", msgRegion},
		"neither":              {`{"bucket":"acme-exports"}`, "", msgAccess},
		"both":                 {`{"bucket":"acme-exports",` + role + `,` + keys + `}`, "", msgAccess},
		"role arn":             {`{"bucket":"acme-exports","role":{"arn":"arn:aws:iam::123:role/x"}}`, "", msgRoleARN},
		"user arn":             {`{"bucket":"acme-exports","role":{"arn":"arn:aws:iam::222222222222:user/x"}}`, "", msgRoleARN},
		"own account role":     {`{"bucket":"acme-exports","role":{"arn":"arn:aws:iam::111111111111:role/x"}}`, "", msgRoleARN},
		"role with a secret":   {`{"bucket":"acme-exports",` + role + `}`, "secret", msgAccess},
		"key id":               {`{"bucket":"acme-exports","keys":{"access_key_id":"AK"}}`, "s", msgKeyID},
		"key id space":         {`{"bucket":"acme-exports","keys":{"access_key_id":"AKIA EXAMPLE"}}`, "s", msgKeyID},
		"no secret":            {`{"bucket":"acme-exports",` + keys + `}`, "", msgSecret},
		"temporary key":        {`{"bucket":"acme-exports","keys":{"access_key_id":"ASIAEXAMPLE"}}`, "s", msgTemporary},
		"http endpoint":        {`{"bucket":"acme-exports","keys":{"access_key_id":"AKIA","endpoint":"http://r2.example.com"}}`, "s", msgEndpoint},
		"endpoint path":        {`{"bucket":"acme-exports","keys":{"access_key_id":"AKIA","endpoint":"https://r2.example.com/bucket"}}`, "s", msgEndpoint},
		"endpoint query":       {`{"bucket":"acme-exports","keys":{"access_key_id":"AKIA","endpoint":"https://r2.example.com?x=1"}}`, "s", msgEndpoint},
		"endpoint userinfo":    {`{"bucket":"acme-exports","keys":{"access_key_id":"AKIA","endpoint":"https://u:p@r2.example.com"}}`, "s", msgEndpoint},
		"endpoint fragment":    {`{"bucket":"acme-exports","keys":{"access_key_id":"AKIA","endpoint":"https://r2.example.com#x"}}`, "s", msgEndpoint},
		"endpoint unparseable": {`{"bucket":"acme-exports","keys":{"access_key_id":"AKIA","endpoint":"https://%zz"}}`, "s", msgEndpoint},
		"aws endpoint":         {`{"bucket":"acme-exports","keys":{"access_key_id":"AKIA","endpoint":"https://s3.eu-west-2.amazonaws.com"}}`, "s", msgAWSEndpoint},
		"aws china endpoint":   {`{"bucket":"acme-exports","keys":{"access_key_id":"AKIA","endpoint":"https://s3.cn-north-1.amazonaws.com.cn"}}`, "s", msgAWSEndpoint},
		"helix endpoint":       {`{"bucket":"acme-exports","keys":{"access_key_id":"AKIA","endpoint":"https://x.helix-db.com"}}`, "s", msgPrivate},
		"helix apex":           {`{"bucket":"acme-exports","keys":{"access_key_id":"AKIA","endpoint":"https://helix-db.com"}}`, "s", msgPrivate},
		"helix trailing dot":   {`{"bucket":"acme-exports","keys":{"access_key_id":"AKIA","endpoint":"https://api.helix-db.com."}}`, "s", msgEndpoint},
		"helix apex dot":       {`{"bucket":"acme-exports","keys":{"access_key_id":"AKIA","endpoint":"https://HELIX-DB.COM.:443"}}`, "s", msgEndpoint},
		"aws trailing dot":     {`{"bucket":"acme-exports","keys":{"access_key_id":"ASIAX","endpoint":"https://s3.amazonaws.com."}}`, "s", msgEndpoint},
		"trailing dots":        {`{"bucket":"acme-exports","keys":{"access_key_id":"AKIA","endpoint":"https://r2.example.com.."}}`, "s", msgEndpoint},
		"escaped helix":        {`{"bucket":"acme-exports","keys":{"access_key_id":"AKIA","endpoint":"https://x.helix-db%2Ecom"}}`, "s", msgEndpoint},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := h.kind.Prepare(Source{Settings: []byte(tc.settings), Secret: tc.secret})
			if err != nil {
				if got := invalidMessage(t, err); got != tc.want {
					t.Fatalf("message = %q, want %q", got, tc.want)
				}
				return
			}
			if string(raw) != tc.want {
				t.Fatalf("settings = %s, want %s", raw, tc.want)
			}
		})
	}
	t.Run("role access off", func(t *testing.T) {
		off := newS3Harness(t, func(o *S3Options) { o.RoleARN = "" })
		_, err := off.kind.Prepare(Source{Settings: []byte(`{"bucket":"acme-exports",` + role + `}`)})
		if got := invalidMessage(t, err); got != msgNoRole {
			t.Fatalf("message = %q", got)
		}
		if _, _, ok := off.kind.Setup("ws-1"); ok {
			t.Fatal("setup offered without a role")
		}
	})
	t.Run("hint", func(t *testing.T) {
		for secret, want := range map[string]string{"": "IAM role", "short": "secret …", "wJalrXUtnFEMI/K7MDENG": "secret …DENG"} {
			if got := h.kind.Hint(secret); got != want {
				t.Fatalf("Hint(%q) = %q", secret, got)
			}
		}
		if principal, external, ok := h.kind.Setup("ws-9"); !ok || principal != testConnectorRole || external != "helix-foundry-ws-9" {
			t.Fatalf("setup = %s %s %v", principal, external, ok)
		}
	})
}

// putE2E fills the bucket with the verification's layout under e2e/.
func putE2E(f *fakeS3) {
	f.put("e2e/customers.csv", "\xef\xbb\xbfid;name\n1;Ada\n2;Grace\n")
	f.put("e2e/events/2026-10-01.jsonl", "{\"id\":1,\"kind\":\"view\"}\n")
	f.put("e2e/events/2026-10-02.jsonl", "{\"id\":2,\"page\":\"/p\"}\n")
	f.put("e2e/products.json", `[{"id":"p1","price":1.5}]`)
	f.put("e2e/api/invoices.json", `{"object":"list","data":[{"id":"in_1"}]}`)
	f.put("e2e/notes.xlsx", "PK")
	f.put("e2e/_SUCCESS", "x")
	f.put("e2e/archive/old.csv", "id\n1\n")
	f.mu.Lock()
	o := f.objects["e2e/archive/old.csv"]
	o.storageClass = "GLACIER"
	f.objects["e2e/archive/old.csv"] = o
	f.mu.Unlock()
}

func tableNamesOf(d Discovery) []string {
	var out []string
	for _, t := range d.Tables {
		out = append(out, t.Name)
	}
	return out
}

func TestS3Discover(t *testing.T) {
	ctx := context.Background()
	t.Run("finds tables, previews and the region", func(t *testing.T) {
		h := newS3Harness(t, nil)
		putE2E(h.s3)
		d, err := h.kind.Discover(ctx, h.roleSource(t, ""))
		if err != nil {
			t.Fatal(err)
		}
		if got := tableNamesOf(d); !slices.Equal(got, []string{"api", "customers", "events", "products"}) {
			t.Fatalf("tables = %v", got)
		}
		if !slices.Equal(d.Unavailable, []Unavailable{{"archive/old.csv", "archived in Glacier; restore it first"}, {"notes.xlsx", "unsupported file type"}}) {
			t.Fatalf("unavailable = %v", d.Unavailable)
		}
		if string(d.Settings) != `{"bucket":"acme-exports","prefix":"e2e/","region":"eu-west-2","role":{"arn":"`+customerRole+`"}}` || d.MaxBytes != 4<<20 {
			t.Fatalf("settings = %s, max = %d", d.Settings, d.MaxBytes)
		}
		customers := d.Tables[1]
		if len(customers.Files) != 1 || customers.Files[0].Format != filesource.FormatCSV || customers.Files[0].ETag == "" ||
			!slices.Equal(customers.PrimaryKey, []string{"id"}) || len(customers.Columns) != 2 || customers.Columns[0].Type != "integer" ||
			len(customers.Preview) != 2 || string(customers.Preview[1]) != `{"id":2,"name":"Grace"}` {
			t.Fatalf("customers = %+v", customers)
		}
		events := d.Tables[2]
		if len(events.Columns) != 3 || len(events.Preview) != 2 || events.Problem != "" {
			t.Fatalf("events = %+v", events)
		}
	})
	t.Run("keeps a region it was given", func(t *testing.T) {
		h := newS3Harness(t, nil)
		putE2E(h.s3)
		d, err := h.kind.Discover(ctx, h.roleSource(t, `,"region":"eu-west-2"`))
		if err != nil || d.Settings != nil {
			t.Fatalf("settings = %s, err = %v", d.Settings, err)
		}
	})
	// A typed region the bucket is not in is corrected: stored by a test,
	// used in memory by a sync.
	t.Run("corrects a wrong region", func(t *testing.T) {
		for _, forSync := range []bool{false, true} {
			h := newS3Harness(t, nil)
			putE2E(h.s3)
			h.s3.listRegion = "eu-west-2"
			src := h.keySource(t, `,"region":"us-west-1"`)
			src.ForSync = forSync
			d, err := h.kind.Discover(ctx, src)
			if err != nil || !strings.Contains(string(d.Settings), `"region":"eu-west-2"`) {
				t.Fatalf("for sync %v: settings = %s, err = %v", forSync, d.Settings, err)
			}
		}
	})
	t.Run("asks for a region it cannot find", func(t *testing.T) {
		h := newS3Harness(t, nil)
		h.s3.fail = func(r *http.Request, _ string) (int, string) {
			if r.Method == http.MethodHead {
				return http.StatusForbidden, "AccessDenied"
			}
			return 0, ""
		}
		_, err := h.kind.Discover(ctx, h.keySource(t, ""))
		if got := invalidMessage(t, err); got != msgRegion {
			t.Fatalf("message = %q", got)
		}
	})
	t.Run("a custom endpoint needs no region", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.Header.Get("Authorization"), "/us-east-1/s3/") {
				t.Errorf("signed for %s", r.Header.Get("Authorization"))
			}
			w.Header().Set("Content-Type", "application/xml")
			fmt.Fprint(w, `<ListBucketResult><Name>acme-exports</Name><Contents><Key>a.csv</Key><Size>4</Size><ETag>"1"</ETag></Contents></ListBucketResult>`)
		}))
		t.Cleanup(server.Close)
		h := newS3Harness(t, nil)
		h.kind.tlsConfig = server.Client().Transport.(*http.Transport).TLSClientConfig
		src := h.prepared(t, `{"bucket":"acme-exports","keys":{"access_key_id":"AKIAR2","endpoint":"`+server.URL+`"}}`, "secret")
		src.ForSync = true
		d, err := h.kind.Discover(ctx, src)
		if err != nil || !slices.Equal(tableNamesOf(d), []string{"a"}) || d.Settings != nil {
			t.Fatalf("discovery = %+v, err = %v", d, err)
		}
	})
	t.Run("limits", func(t *testing.T) {
		for name, tc := range map[string]struct {
			fill func(f *fakeS3)
			want string
		}{
			"too many keys": {func(f *fakeS3) {
				for i := range 10_001 {
					f.objects[fmt.Sprintf("e2e/t/%05d.csv", i)] = fakeObject{body: []byte("a\n"), etag: `"1"`}
				}
			}, msgTooManyKeys},
			"too many tables": {func(f *fakeS3) {
				for i := range 201 {
					f.put(fmt.Sprintf("e2e/t%03d/x.csv", i), "a\n")
				}
			}, msgTooManyTbls},
			"nothing readable": {func(f *fakeS3) { f.put("e2e/notes.xlsx", "x") },
				"no files Helix can read under s3://acme-exports/e2e/; Helix reads Parquet, JSON, JSON Lines and CSV"},
		} {
			t.Run(name, func(t *testing.T) {
				h := newS3Harness(t, nil)
				tc.fill(h.s3)
				_, err := h.kind.Discover(ctx, h.keySource(t, `,"region":"eu-west-2"`))
				if got := invalidMessage(t, err); got != tc.want {
					t.Fatalf("message = %q", got)
				}
			})
		}
	})
	t.Run("stops a repeated continuation token", func(t *testing.T) {
		h := newS3Harness(t, nil)
		putE2E(h.s3)
		h.s3.repeatToken = true
		_, err := h.kind.Discover(ctx, h.keySource(t, `,"region":"eu-west-2"`))
		if got := refusedMessage(t, err); got != msgUnreadable {
			t.Fatalf("message = %q", got)
		}
	})
	t.Run("fails a chosen table that is gone", func(t *testing.T) {
		h := newS3Harness(t, nil)
		putE2E(h.s3)
		_, err := h.kind.Discover(ctx, h.keySource(t, `,"region":"eu-west-2","tables":["customers","orders"]`))
		if got := invalidMessage(t, err); got != `table "orders" is not in this folder any more` {
			t.Fatalf("message = %q", got)
		}
		if _, err := h.kind.Discover(ctx, h.keySource(t, `,"region":"eu-west-2","tables":["customers","notes.xlsx"]`)); err != nil {
			t.Fatalf("an unavailable chosen table is not gone: %v", err)
		}
	})
	// The listing alone decides availability, so a test and a sync agree.
	t.Run("marks a table over the source limit unavailable", func(t *testing.T) {
		h := newS3Harness(t, func(o *S3Options) { o.MaxSourceBytes = 30 })
		putE2E(h.s3)
		h.s3.put("e2e/big.csv", "id\n"+strings.Repeat("1\n", 20))
		test, err := h.kind.Discover(ctx, h.keySource(t, `,"region":"eu-west-2"`))
		if err != nil {
			t.Fatal(err)
		}
		src := h.keySource(t, `,"region":"eu-west-2"`)
		src.ForSync = true
		sync, err := h.kind.Discover(ctx, src)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(test.Unavailable, sync.Unavailable) || !slices.Contains(test.Unavailable, Unavailable{Name: "big", Reason: "is 43 bytes, over the 30 bytes limit"}) {
			t.Fatalf("test %v, sync %v", test.Unavailable, sync.Unavailable)
		}
	})
	t.Run("bounds a sync's bytes", func(t *testing.T) {
		h := newS3Harness(t, func(o *S3Options) { o.MaxConnectorBytes = 60 })
		putE2E(h.s3)
		if _, err := h.kind.Discover(ctx, h.keySource(t, `,"region":"eu-west-2"`)); err != nil {
			t.Fatalf("a test is not bounded by the sync's bytes: %v", err)
		}
		src := h.keySource(t, `,"region":"eu-west-2"`)
		src.ForSync = true
		_, err := h.kind.Discover(ctx, src)
		if got := invalidMessage(t, err); got != "the tables chosen hold 134 bytes, over the 60 bytes limit for one connection; choose fewer tables or a narrower folder" {
			t.Fatalf("message = %q", got)
		}
		src = h.keySource(t, `,"region":"eu-west-2","tables":["customers"]`)
		src.ForSync = true
		if _, err := h.kind.Discover(ctx, src); err != nil {
			t.Fatalf("a narrower choice fits: %v", err)
		}
	})
	t.Run("caps the unavailable list for a test only", func(t *testing.T) {
		h := newS3Harness(t, nil)
		h.s3.put("e2e/a.csv", "x\n")
		for i := range 60 {
			h.s3.put(fmt.Sprintf("e2e/n%02d.xlsx", i), "x")
		}
		test, err := h.kind.Discover(ctx, h.keySource(t, `,"region":"eu-west-2"`))
		if err != nil || len(test.Unavailable) != 51 || test.Unavailable[50].Name != "…and 10 more" {
			t.Fatalf("unavailable = %d, err = %v", len(test.Unavailable), err)
		}
		src := h.keySource(t, `,"region":"eu-west-2"`)
		src.ForSync = true
		sync, err := h.kind.Discover(ctx, src)
		if err != nil || len(sync.Unavailable) != 60 {
			t.Fatalf("unavailable = %d, err = %v", len(sync.Unavailable), err)
		}
	})
	t.Run("a sync's discovery reads no object", func(t *testing.T) {
		h := newS3Harness(t, nil)
		putE2E(h.s3)
		src := h.roleSource(t, "")
		src.ForSync = true
		d, err := h.kind.Discover(ctx, src)
		if err != nil || h.s3.gets() != 0 || d.Tables[0].Columns != nil {
			t.Fatalf("gets = %d, err = %v", h.s3.gets(), err)
		}
	})
	t.Run("runs one test per workspace", func(t *testing.T) {
		h := newS3Harness(t, nil)
		putE2E(h.s3)
		release, _ := h.kind.claimDiscover("ws-1")
		_, err := h.kind.Discover(ctx, h.roleSource(t, ""))
		if got := refusedMessage(t, err); got != msgBusy {
			t.Fatalf("message = %q", got)
		}
		other := h.roleSource(t, "")
		other.WorkspaceID = "ws-2"
		h.sts.allow[customerRole] = append(h.sts.allow[customerRole], "ws-2")
		if _, err := h.kind.Discover(ctx, other); err != nil {
			t.Fatalf("another workspace waits for nothing: %v", err)
		}
		release()
		if _, err := h.kind.Discover(ctx, h.roleSource(t, "")); err != nil {
			t.Fatal(err)
		}
	})
}

func TestS3Sampling(t *testing.T) {
	ctx := context.Background()
	t.Run("reports unreadable samples as problems", func(t *testing.T) {
		h := newS3Harness(t, nil)
		h.s3.put("e2e/bad/a.jsonl", "{\"a\":1}\n7\n")
		h.s3.put("e2e/denied/a.csv", "a\n1\n")
		h.s3.put("e2e/fine/a.csv", "a\n1\n")
		h.s3.put("e2e/pq/a.parquet", "PAR1"+strings.Repeat("x", 20)+"\x0a\x00\x00\x00PAR1")
		h.s3.fail = func(r *http.Request, key string) (int, string) {
			if strings.HasPrefix(key, "e2e/denied/") {
				return http.StatusForbidden, "AccessDenied"
			}
			return 0, ""
		}
		d, err := h.kind.Discover(ctx, h.keySource(t, `,"region":"eu-west-2"`))
		if err != nil {
			t.Fatal(err)
		}
		problems := map[string]string{}
		for _, tbl := range d.Tables {
			problems[tbl.Name] = tbl.Problem
		}
		if len(d.Unavailable) != 0 || problems["bad"] != "e2e/bad/a.jsonl record 2: not a JSON object" ||
			problems["denied"] != msgSampleDeny || problems["fine"] != "" || !strings.HasPrefix(problems["pq"], "e2e/pq/a.parquet: ") {
			t.Fatalf("problems = %v, unavailable = %v", problems, d.Unavailable)
		}
	})
	// A body that breaks off says nothing about the data: the table comes
	// back without columns, not with a problem naming the connection.
	t.Run("keeps a broken-off read out of problems", func(t *testing.T) {
		h := newS3Harness(t, nil)
		h.s3.put("e2e/cut/a.jsonl", strings.Repeat("{\"a\":1}\n", 200))
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		_, _ = zw.Write([]byte(strings.Repeat("{\"a\":1}\n", 2000)))
		_ = zw.Close()
		h.s3.put("e2e/zipped/a.jsonl.gz", gz.String())
		h.s3.put("e2e/fine/a.csv", "a\n1\n")
		h.s3.cut["e2e/cut/a.jsonl"], h.s3.cut["e2e/zipped/a.jsonl.gz"] = 1, 1
		d, err := h.kind.Discover(ctx, h.keySource(t, `,"region":"eu-west-2"`))
		if err != nil {
			t.Fatal(err)
		}
		for _, tbl := range d.Tables {
			if tbl.Problem != "" || (tbl.Name == "fine") != (tbl.Columns != nil) {
				t.Fatalf("table %s: problem %q, columns %v", tbl.Name, tbl.Problem, tbl.Columns)
			}
		}
	})
	t.Run("reads a Parquet footer only", func(t *testing.T) {
		h := newS3Harness(t, nil)
		h.s3.put("e2e/orders/p.parquet", string(parquetFixture(t)))
		d, err := h.kind.Discover(ctx, h.keySource(t, `,"region":"eu-west-2"`))
		if err != nil {
			t.Fatal(err)
		}
		orders := d.Tables[0]
		ranges := 0
		for _, r := range h.s3.seen() {
			if r.method == http.MethodGet && !r.list {
				if r.rangeHeader == "" || r.ifMatch == "" {
					t.Fatalf("an unranged read: %+v", r)
				}
				ranges++
			}
		}
		if ranges != 2 || len(orders.Columns) != 2 || orders.Columns[0].Type != "integer" || orders.Preview != nil {
			t.Fatalf("ranges = %d, orders = %+v", ranges, orders)
		}
	})
	t.Run("keeps short previews", func(t *testing.T) {
		h := newS3Harness(t, nil)
		var rows strings.Builder
		rows.WriteString(`{"id":0,"big":"` + strings.Repeat("x", 5000) + "\"}\n")
		for i := 1; i < 20; i++ {
			fmt.Fprintf(&rows, "{\"id\":%d}\n", i)
		}
		h.s3.put("e2e/t/a.jsonl", rows.String())
		d, err := h.kind.Discover(ctx, h.keySource(t, `,"region":"eu-west-2"`))
		if err != nil {
			t.Fatal(err)
		}
		preview := d.Tables[0].Preview
		if len(preview) != 5 || string(preview[0]) != `{"id":1,"big":null}` {
			t.Fatalf("preview = %s", preview)
		}
		for _, row := range preview {
			if len(row) > 4<<10 {
				t.Fatalf("a %d byte preview row", len(row))
			}
		}
	})
	// Sampling bounds: 100 GETs, fetches and decodes in parallel but bounded.
	t.Run("bounds a test's reads", func(t *testing.T) {
		h := newS3Harness(t, nil)
		for i := range 60 {
			for f := range 3 {
				h.s3.put(fmt.Sprintf("e2e/t%02d/%d.csv", i, f), "a\n1\n")
			}
		}
		h.s3.slow = time.Millisecond
		d, err := h.kind.Discover(ctx, h.keySource(t, `,"region":"eu-west-2"`))
		if err != nil {
			t.Fatal(err)
		}
		if gets := h.s3.gets(); gets > maxSampleGets || gets < 60 {
			t.Fatalf("%d GETs", gets)
		}
		if h.s3.maxInFlight > decodesAtOnce {
			t.Fatalf("%d decodes at once", h.s3.maxInFlight)
		}
		if len(d.Tables[0].Columns) == 0 {
			t.Fatal("first table not sampled")
		}
	})
	t.Run("samples the newest file first, then the oldest", func(t *testing.T) {
		jobs := sampleSchedule([][]Object{{{Key: "old"}, {Key: "mid"}, {Key: "new"}}, {{Key: "only"}}})
		var keys []string
		for _, j := range jobs {
			keys = append(keys, j.o.Key)
		}
		if strings.Join(keys, ",") != "new,only,old,mid" {
			t.Fatalf("order = %v", keys)
		}
	})
	t.Run("stops sampling at the soft deadline", func(t *testing.T) {
		h := newS3Harness(t, nil)
		putE2E(h.s3)
		h.s3.slow = 200 * time.Millisecond
		h.kind.timeouts.sample = 300 * time.Millisecond
		h.kind.timeouts.idle = time.Minute
		start := time.Now()
		d, err := h.kind.Discover(ctx, h.keySource(t, `,"region":"eu-west-2"`))
		if err != nil || time.Since(start) > 2*time.Second {
			t.Fatalf("took %s, err = %v", time.Since(start), err)
		}
		for _, tbl := range d.Tables {
			if tbl.Columns != nil || tbl.Problem != "" {
				t.Fatalf("%s sampled after the deadline: %+v", tbl.Name, tbl)
			}
		}
	})
	// Columns and previews stay within the response budget, so the largest
	// test answer fits gRPC's 4 MiB default between the services.
	t.Run("keeps the response within budget", func(t *testing.T) {
		h := newS3Harness(t, func(o *S3Options) { o.MaxSourceBytes = 1 << 30 })
		header := make([]string, 1000)
		row := make([]string, 1000)
		for i := range header {
			header[i] = fmt.Sprintf("column_with_a_long_name_%04d", i)
			row[i] = "v"
		}
		body := strings.Join(header, ",") + "\n" + strings.Repeat(strings.Join(row, ",")+"\n", 5)
		for i := range 40 {
			h.s3.put(fmt.Sprintf("e2e/t%03d/a.csv", i), body)
		}
		h.kind.timeouts.sample = 20 * time.Second
		d, err := h.kind.Discover(ctx, h.keySource(t, `,"region":"eu-west-2"`))
		if err != nil {
			t.Fatal(err)
		}
		columnBytes, previewBytes, bare := 0, 0, 0
		for _, tbl := range d.Tables {
			for _, c := range tbl.Columns {
				columnBytes += len(c.Name) + len(c.Type) + 16
			}
			for _, r := range tbl.Preview {
				previewBytes += len(r)
			}
			if tbl.Columns == nil {
				bare++
			}
		}
		if columnBytes > maxColumnBytes || previewBytes > maxPreviewBytes || bare == 0 {
			t.Fatalf("columns %d bytes, previews %d bytes, %d bare tables", columnBytes, previewBytes, bare)
		}
	})
	// A zstd frame declaring a 256 MiB window must not allocate it.
	t.Run("eight long-window samples stay small", func(t *testing.T) {
		h := newS3Harness(t, nil)
		// The frame header declares a 256 MiB window (zstd --long=28) before one raw byte.
		frame := []byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, (28 - 10) << 3, 0x09, 0x00, 0x00, '{'}
		for i := range 8 {
			h.s3.put(fmt.Sprintf("e2e/t%d/a.jsonl.zst", i), string(frame))
		}
		var before, peak runtime.MemStats
		runtime.GC()
		runtime.ReadMemStats(&before)
		stop := make(chan struct{})
		var wg sync.WaitGroup
		var max atomic.Uint64
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				case <-time.After(time.Millisecond):
					runtime.ReadMemStats(&peak)
					if peak.HeapAlloc > before.HeapAlloc && peak.HeapAlloc-before.HeapAlloc > max.Load() {
						max.Store(peak.HeapAlloc - before.HeapAlloc)
					}
				}
			}
		}()
		d, err := h.kind.Discover(ctx, h.keySource(t, `,"region":"eu-west-2"`))
		close(stop)
		wg.Wait()
		if err != nil {
			t.Fatal(err)
		}
		if max.Load() > 64<<20 {
			t.Fatalf("heap grew %d MiB", max.Load()>>20)
		}
		for _, tbl := range d.Tables {
			if tbl.Columns != nil || tbl.Problem != "" {
				t.Fatalf("a sample over the window budget gave %+v", tbl)
			}
		}
	})
}

// memWriter is a snapshot writer over a temp dir.
func newSnapshot(t *testing.T, maxRows, maxBytes int64) *snapshot.Writer {
	t.Helper()
	w, err := snapshot.NewWriter(t.TempDir(), maxRows, maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// discovered is the table a sync's discovery hands Sync.
func (h *s3Harness) discovered(t *testing.T, src Source, name string) (Source, Table) {
	t.Helper()
	src.ForSync = true
	d, err := h.kind.Discover(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if d.Settings != nil {
		src.Settings = d.Settings
	}
	i := slices.IndexFunc(d.Tables, func(t Table) bool { return t.Name == name })
	if i < 0 {
		t.Fatalf("no table %s in %v", name, tableNamesOf(d))
	}
	return src, d.Tables[i]
}

func TestS3Sync(t *testing.T) {
	ctx := context.Background()
	t.Run("writes every row with every column", func(t *testing.T) {
		h := newS3Harness(t, nil)
		putE2E(h.s3)
		h.s3.put("e2e/orders/dt=2026-10-01/part-0.jsonl", "{\"id\":1,\"total\":5}\n")
		h.s3.put("e2e/orders/dt=2026-10-02/part-0.jsonl", "{\"id\":2,\"note\":\"x\"}\n")
		src, table := h.discovered(t, h.roleSource(t, ""), "orders")
		sink := &rowSink{}
		written, err := h.kind.Sync(ctx, src, table, sink)
		if err != nil {
			t.Fatal(err)
		}
		want := []Column{{Name: "id", Type: "integer"}, {Name: "total", Type: "integer", Nullable: true},
			{Name: "note", Type: "string", Nullable: true}, {Name: "dt", Type: "string"}}
		if !slices.Equal(written.Columns, want) || !slices.Equal(written.PrimaryKey, []string{"id"}) || written.Name != "orders" {
			t.Fatalf("written = %+v", written)
		}
		if len(sink.rows) != 2 || len(sink.rows[1]) != 4 || sink.rows[1]["total"] != nil || sink.rows[1]["dt"] != "2026-10-02" {
			t.Fatalf("rows = %v", sink.rows)
		}
		for _, r := range h.s3.seen() {
			if r.method == http.MethodGet && !r.list && r.ifMatch == "" {
				t.Fatalf("a read without If-Match: %+v", r)
			}
		}
		if entries, _ := os.ReadDir(h.kind.opts.TempDir); len(entries) != 0 {
			t.Fatalf("left %v", entries)
		}
	})
	t.Run("writes through the snapshot writer", func(t *testing.T) {
		h := newS3Harness(t, nil)
		putE2E(h.s3)
		src, table := h.discovered(t, h.keySource(t, ""), "customers")
		w := newSnapshot(t, 10, 1<<20)
		if _, err := h.kind.Sync(ctx, src, table, w); err != nil || w.Rows() != 2 {
			t.Fatalf("rows = %d, err = %v", w.Rows(), err)
		}
		_, err := h.kind.Sync(ctx, src, table, newSnapshot(t, 1, 1<<20))
		if !errors.Is(err, snapshot.ErrRowLimit) {
			t.Fatalf("row limit = %v", err)
		}
		_, err = h.kind.Sync(ctx, src, table, newSnapshot(t, 10, 10))
		if !errors.Is(err, snapshot.ErrByteLimit) {
			t.Fatalf("byte limit = %v", err)
		}
	})
	t.Run("fails for good on malformed data", func(t *testing.T) {
		h := newS3Harness(t, nil)
		h.s3.put("e2e/events/a.jsonl", "{\"id\":1}\n{\"id\":\n")
		src, table := h.discovered(t, h.keySource(t, `,"region":"eu-west-2"`), "events")
		_, err := h.kind.Sync(ctx, src, table, &rowSink{})
		if got := invalidMessage(t, err); got != "e2e/events/a.jsonl record 2: the file ends inside a record" {
			t.Fatalf("message = %q", got)
		}
		if entries, _ := os.ReadDir(h.kind.opts.TempDir); len(entries) != 0 {
			t.Fatalf("left %v", entries)
		}
	})
	t.Run("retries an object that changed", func(t *testing.T) {
		for name, edit := range map[string]func(f *fakeS3){
			"rewritten": func(f *fakeS3) { f.put("e2e/customers.csv", "id;name\n1;Ada\n") },
			"deleted": func(f *fakeS3) {
				f.mu.Lock()
				defer f.mu.Unlock()
				delete(f.objects, "e2e/customers.csv")
			},
			"archived": func(f *fakeS3) {
				f.mu.Lock()
				defer f.mu.Unlock()
				f.fail = func(_ *http.Request, key string) (int, string) {
					if key == "e2e/customers.csv" {
						return http.StatusForbidden, "InvalidObjectState"
					}
					return 0, ""
				}
			},
			// A store that ignores If-Match sends more than was listed.
			"grown": func(f *fakeS3) {
				f.mu.Lock()
				defer f.mu.Unlock()
				o := f.objects["e2e/customers.csv"]
				o.body = append(o.body, "3;Alan\n"...)
				f.objects["e2e/customers.csv"] = o
			},
		} {
			t.Run(name, func(t *testing.T) {
				h := newS3Harness(t, nil)
				putE2E(h.s3)
				src, table := h.discovered(t, h.keySource(t, ""), "customers")
				edit(h.s3)
				_, err := h.kind.Sync(ctx, src, table, &rowSink{})
				if got := refusedMessage(t, err); got != "e2e/customers.csv changed while Helix read it; Helix will retry" {
					t.Fatalf("message = %q", got)
				}
			})
		}
	})
	t.Run("stops at the source byte cap", func(t *testing.T) {
		h := newS3Harness(t, nil)
		putE2E(h.s3)
		src, table := h.discovered(t, h.keySource(t, ""), "events")
		h.kind.opts.MaxSourceBytes = 30
		_, err := h.kind.Sync(ctx, src, table, &rowSink{})
		if got := refusedMessage(t, err); !strings.HasSuffix(got, "changed while Helix read it; Helix will retry") {
			t.Fatalf("message = %q", got)
		}
	})
	t.Run("reads a broken-off body again", func(t *testing.T) {
		h := newS3Harness(t, nil)
		putE2E(h.s3)
		src, table := h.discovered(t, h.keySource(t, ""), "customers")
		h.s3.cut["e2e/customers.csv"] = 2
		sink := &rowSink{}
		if _, err := h.kind.Sync(ctx, src, table, sink); err != nil || len(sink.rows) != 2 {
			t.Fatalf("rows = %d, err = %v", len(sink.rows), err)
		}
		h.s3.cut["e2e/customers.csv"] = 3
		_, err := h.kind.Sync(ctx, src, table, &rowSink{})
		if got := refusedMessage(t, err); got != msgUnreadable {
			t.Fatalf("message = %q", got)
		}
		// A table at the source cap still fits: a broken-off try's bytes
		// are given back rather than counted again.
		h.kind.opts.MaxSourceBytes = table.Files[0].Size
		h.s3.cut["e2e/customers.csv"] = 2
		if _, err := h.kind.Sync(ctx, src, table, &rowSink{}); err != nil {
			t.Fatalf("at the cap: %v", err)
		}
	})
	t.Run("fails a stalled store within its timeouts", func(t *testing.T) {
		for name, edit := range map[string]func(h *s3Harness){
			"headers never arrive": func(h *s3Harness) { h.s3.stall = time.Minute },
			"one byte a second":    func(h *s3Harness) { h.s3.slow = time.Second },
		} {
			t.Run(name, func(t *testing.T) {
				h := newS3Harness(t, nil)
				putE2E(h.s3)
				src, table := h.discovered(t, h.keySource(t, ""), "customers")
				edit(h)
				start := time.Now()
				_, err := h.kind.Sync(ctx, src, table, &rowSink{})
				if got := refusedMessage(t, err); got != msgUnreadable && got != msgThrottled {
					t.Fatalf("message = %q", got)
				}
				if time.Since(start) > 10*time.Second {
					t.Fatalf("took %s", time.Since(start))
				}
			})
		}
	})
	// The SDK's shared retry budget would end a sync after about a hundred
	// retries; each client has its own, unbounded one.
	t.Run("retries every throttled read", func(t *testing.T) {
		h := newS3Harness(t, func(o *S3Options) { o.MaxSourceBytes = 1 << 30 })
		for i := range 150 {
			h.s3.put(fmt.Sprintf("e2e/t/%03d.csv", i), "a\n1\n")
		}
		src, table := h.discovered(t, h.keySource(t, `,"region":"eu-west-2"`), "t")
		var mu sync.Mutex
		throttled := map[string]bool{}
		h.s3.fail = func(r *http.Request, key string) (int, string) {
			mu.Lock()
			defer mu.Unlock()
			if r.Method == http.MethodGet && key != "" && !throttled[key] {
				throttled[key] = true
				return http.StatusServiceUnavailable, "SlowDown"
			}
			return 0, ""
		}
		sink := &rowSink{}
		if _, err := h.kind.Sync(ctx, src, table, sink); err != nil || len(sink.rows) != 150 {
			t.Fatalf("rows = %d, err = %v", len(sink.rows), err)
		}
	})
	t.Run("gives up on a store that keeps throttling", func(t *testing.T) {
		h := newS3Harness(t, nil)
		putE2E(h.s3)
		src, table := h.discovered(t, h.keySource(t, ""), "customers")
		h.s3.fail = func(r *http.Request, key string) (int, string) {
			if key != "" {
				return http.StatusServiceUnavailable, "SlowDown"
			}
			return 0, ""
		}
		_, err := h.kind.Sync(ctx, src, table, &rowSink{})
		if got := refusedMessage(t, err); got != msgThrottled {
			t.Fatalf("message = %q", got)
		}
	})
	t.Run("refuses a read it may not make", func(t *testing.T) {
		h := newS3Harness(t, nil)
		putE2E(h.s3)
		src, table := h.discovered(t, h.keySource(t, ""), "customers")
		h.s3.fail = func(r *http.Request, key string) (int, string) {
			if key != "" {
				return http.StatusForbidden, "AccessDenied"
			}
			return 0, ""
		}
		_, err := h.kind.Sync(ctx, src, table, &rowSink{})
		if got := refusedMessage(t, err); got != "Helix cannot read e2e/customers.csv; allow s3:GetObject on it, and kms:Decrypt if the bucket uses a KMS key" {
			t.Fatalf("message = %q", got)
		}
	})
	t.Run("stops when the attempt ends", func(t *testing.T) {
		h := newS3Harness(t, nil)
		putE2E(h.s3)
		src, table := h.discovered(t, h.keySource(t, ""), "customers")
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := h.kind.Sync(cancelled, src, table, &rowSink{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
	// A chained session lasts an hour at most, so a long sync renews it.
	t.Run("renews the role's session during a long sync", func(t *testing.T) {
		for _, tc := range []struct {
			expiry, window time.Duration
			sessions       int
		}{{3 * time.Second, time.Second, 2}, {time.Hour, 5 * time.Minute, 1}} {
			h := newS3Harness(t, nil)
			for i := range 12 {
				h.s3.put(fmt.Sprintf("e2e/t/%02d.csv", i), "a\n1\n")
			}
			src, table := h.discovered(t, h.roleSource(t, `,"region":"eu-west-2"`), "t")
			h.sts.mu.Lock()
			h.sts.calls, h.sts.expiry = nil, tc.expiry
			h.sts.mu.Unlock()
			h.kind.timeouts.expiry, h.kind.timeouts.header = tc.window, 5*time.Second
			h.s3.fail = func(*http.Request, string) (int, string) {
				time.Sleep(1500 * time.Millisecond)
				return 0, ""
			}
			if _, err := h.kind.Sync(ctx, src, table, &rowSink{}); err != nil {
				t.Fatal(err)
			}
			if got := len(h.sts.sessions()); got != tc.sessions {
				t.Fatalf("expiry %s: %d sessions, want %d", tc.expiry, got, tc.sessions)
			}
		}
	})
}

// parquetFixture is a Parquet file of an int64 id and a string name.
func parquetFixture(t *testing.T) []byte {
	t.Helper()
	sc := arrow.NewSchema([]arrow.Field{{Name: "id", Type: arrow.PrimitiveTypes.Int64}, {Name: "name", Type: arrow.BinaryTypes.String, Nullable: true}}, nil)
	rec, _, err := array.RecordFromJSON(memory.DefaultAllocator, sc, strings.NewReader(`[{"id":1,"name":"a"},{"id":2,"name":"b"}]`))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	w, err := pqarrow.NewFileWriter(sc, &buf, parquet.NewWriterProperties(), pqarrow.DefaultWriterProps())
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestS3Catalog(t *testing.T) {
	cfg := config.Defaults()
	cfg.Foundry.Sync.DataBucket = "example-foundry-data"
	cfg.Foundry.Connectors.S3RoleARN = testConnectorRole
	opts := NewS3Options(&cfg, aws.Config{Region: "us-east-2"})
	if !slices.Equal(opts.Reserved, []string{"example-foundry-data"}) || opts.RoleARN != testConnectorRole ||
		opts.MaxSourceBytes != 2<<30 || opts.MaxConnectorBytes != 20<<30 || opts.MaxTableBytes != 2<<30 || opts.Base.Region != "us-east-2" {
		t.Fatalf("options = %+v", opts)
	}
	cfg.Foundry.Sync.DataBucket = ""
	if NewS3Options(&cfg, aws.Config{}).Reserved != nil {
		t.Fatal("an empty bucket is reserved")
	}
	s3, err := NewCatalog(DefaultEndpoints(), Guard{}, opts, EventsOptions{}).Kind("s3")
	if err != nil || s3.DisplayName() != "Amazon S3" {
		t.Fatalf("s3 kind = (%v, %v)", s3, err)
	}
	if _, ok := s3.(RoleSetup); !ok {
		t.Fatal("the s3 kind offers no role setup")
	}
}
