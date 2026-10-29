package syncer

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/helixdb/helix-foundry/backend/connector"
	"github.com/helixdb/helix-foundry/backend/store/model"
)

// googleDrive is a Workspace a test edits between syncs: one user, no
// calendars, and Drive's Docs by id, each with its modified time and
// Markdown, and whether it is in the trash. It counts every export by id.
type googleDrive struct {
	mu      sync.Mutex
	docs    map[string]googleTestDoc
	exports map[string]int
}

type googleTestDoc struct {
	modified, text string
	trashed        bool
}

func (d *googleDrive) serve(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		switch path := r.URL.Path; {
		case path == "/token":
			_, _ = w.Write([]byte(`{"access_token":"token","token_type":"Bearer","expires_in":3600}`))
		case path == "/admin/directory/v1/users":
			_, _ = w.Write([]byte(`{"users":[{"id":"1001","primaryEmail":"ada@acme.com","name":{"fullName":"Ada Lovelace"}}]}`))
		case path == "/calendar/v3/users/me/calendarList":
			_, _ = w.Write([]byte(`{"items":[]}`))
		case path == "/drive/v3/drives":
			_, _ = w.Write([]byte(`{"drives":[]}`))
		case path == "/drive/v3/files":
			// A Sheet is a row and never a document.
			files := []string{`{"id":"sheet1","name":"Budget","mimeType":"application/vnd.google-apps.spreadsheet","modifiedTime":"2026-10-01T09:00:00Z"}`}
			for _, id := range slices.Sorted(maps.Keys(d.docs)) {
				if doc := d.docs[id]; !doc.trashed || r.URL.Query().Get("q") != "trashed=false" {
					files = append(files, fmt.Sprintf(`{"id":%q,"name":"Doc %s","mimeType":"application/vnd.google-apps.document",`+
						`"owners":[{"emailAddress":"ada@acme.com"}],"modifiedTime":%q,"webViewLink":"https://docs.google.com/document/d/%s/edit","version":"1"}`,
						id, id, doc.modified, id))
				}
			}
			_, _ = fmt.Fprintf(w, `{"files":[%s]}`, strings.Join(files, ","))
		case strings.HasPrefix(path, "/drive/v3/files/") && strings.HasSuffix(path, "/export"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/drive/v3/files/"), "/export")
			d.exports[id]++
			doc, ok := d.docs[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write([]byte(doc.text))
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

// took are the exports since the last call, by id.
func (d *googleDrive) took() map[string]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	exports := d.exports
	d.exports = map[string]int{}
	return exports
}

// TestGoogleSyncDocuments runs Google Workspace through the syncer: a
// sync reads every table in one pass and fills the Docs set, an unchanged
// Doc is not exported again, an edited one is, and a Doc moved to the
// trash leaves the files table and its document goes with its row.
func TestGoogleSyncDocuments(t *testing.T) {
	drive := &googleDrive{exports: map[string]int{}, docs: map[string]googleTestDoc{
		"doc1": {modified: "2026-10-01T10:00:00Z", text: "## One\n\nFirst."},
		"doc2": {modified: "2026-10-02T10:00:00.500Z", text: "## Two\n\nSecond."},
	}}
	f := newFixture(t, "http://unused", "google", `{"subject":"ada@acme.com","event_days":90}`, plan{code: "pro"})
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := json.Marshal(map[string]string{
		"type": "service_account", "client_email": "foundry@acme.iam.gserviceaccount.com", "private_key_id": "5c1e0d9f1a2b",
		"private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
	})
	conn := f.store.connectors["conn-1"]
	if conn.SecretCiphertext, conn.SecretKeyID, err = f.syncer.Keys.Seal(key, connector.SecretContext("conn-1")); err != nil {
		t.Fatal(err)
	}
	endpoints := connector.DefaultEndpoints()
	endpoints.Google = drive.serve(t)
	endpoints.GoogleToken = endpoints.Google + "/token"
	f.syncer.Catalog = connector.NewCatalog(endpoints, connector.Guard{AllowPrivate: true}, connector.S3Options{}, connector.EventsOptions{})
	f.syncer.Now = func() time.Time { return time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC) }
	text := func(id, modified, body string) string {
		return fmt.Sprintf("# Doc %s\n\nOwner: ada@acme.com\nLast modified: %s\nFile id: %s\n\n%s\n", id, modified, id, body)
	}

	res := f.run(t, "sync-1")
	if res.Mode != model.ConnectorSyncFull || len(f.store.tables) != 5 {
		t.Fatalf("mode = %s, tables = %v", res.Mode, slices.Sorted(maps.Keys(f.store.tables)))
	}
	if exports := drive.took(); fmt.Sprint(exports) != "map[doc1:1 doc2:1]" {
		t.Fatalf("exports = %v; want each Doc once", exports)
	}
	docs := f.held(t, "docs")
	if len(docs) != 2 || docs["doc1"] != text("doc1", "2026-10-01 10:00 UTC", "## One\n\nFirst.") ||
		docs["doc2"] != text("doc2", "2026-10-02 10:00 UTC", "## Two\n\nSecond.") {
		t.Fatalf("docs = %q", docs)
	}
	if set := f.store.setOf("docs"); set.Name != "Docs" || set.Owner.ConnectorID != "conn-1" || !strings.Contains(set.Description, "Google Doc") {
		t.Fatalf("docs set = %+v", set)
	}

	// Nothing changed: the Docs are held at their versions, so none is exported.
	doc1 := f.liveID("docs", "doc1")
	f.run(t, "sync-2")
	if exports := drive.took(); len(exports) != 0 || f.liveID("docs", "doc1") != doc1 {
		t.Fatalf("exports = %v, doc1 = %s, was %s", exports, f.liveID("docs", "doc1"), doc1)
	}

	// An edited Doc is exported again and replaces its document; one moved
	// to the trash is no longer listed, so its row and document go.
	drive.mu.Lock()
	drive.docs["doc1"] = googleTestDoc{modified: "2026-10-01T10:00:00Z", text: "## One\n\nFirst.", trashed: true}
	drive.docs["doc2"] = googleTestDoc{modified: "2026-10-28T08:00:00Z", text: "## Two\n\nSecond, edited."}
	drive.mu.Unlock()
	f.run(t, "sync-3")
	if exports := drive.took(); fmt.Sprint(exports) != "map[doc2:1]" {
		t.Fatalf("exports = %v", exports)
	}
	if docs := f.held(t, "docs"); len(docs) != 1 || docs["doc2"] != text("doc2", "2026-10-28 08:00 UTC", "## Two\n\nSecond, edited.") {
		t.Fatalf("docs = %q", docs)
	}
	if f.store.docs[f.liveID("docs", "doc2")].Source.Version != "1|2026-10-28T08:00:00Z|1" {
		t.Fatalf("doc2 = %+v", f.store.docs[f.liveID("docs", "doc2")].Source)
	}
}
