package model

import (
	"fmt"
	"slices"
	"testing"
)

func TestFoundryReadKind(t *testing.T) {
	// Each type is read one way only: loads, generations and the provider's
	// lists all derive what they read from this one table.
	t.Run("reads every stored type one way", func(t *testing.T) {
		want := map[FoundryMediaType]FoundryReadKind{
			FoundryMediaMarkdown: FoundryReadText, FoundryMediaMDX: FoundryReadText, FoundryMediaPlainText: FoundryReadText,
			FoundryMediaJSON: FoundryReadTable, FoundryMediaCSV: FoundryReadTable, FoundryMediaTSV: FoundryReadTable,
			FoundryMediaJSONL: FoundryReadTable, FoundryMediaParquet: FoundryReadTable, FoundryMediaPDF: FoundryReadPDF,
		}
		if len(FoundryMediaTypes()) != len(want) {
			t.Fatalf("%d stored types, want %d", len(FoundryMediaTypes()), len(want))
		}
		for _, mediaType := range FoundryMediaTypes() {
			if got := mediaType.ReadKind(); got != want[mediaType] {
				t.Errorf("%s reads as %q, want %q", mediaType, got, want[mediaType])
			}
			if mediaType.Format() == "" {
				t.Errorf("%s has no format name", mediaType)
			}
		}
	})
	// A type Foundry does not store has no kind and no name, so no list or
	// switch takes it for one it reads.
	t.Run("reads no type it does not store", func(t *testing.T) {
		for _, mediaType := range []FoundryMediaType{"text/html", "", "TEXT/MARKDOWN"} {
			if mediaType.ReadKind() != "" || mediaType.Format() != "" {
				t.Errorf("%q reads as %q, named %q", mediaType, mediaType.ReadKind(), mediaType.Format())
			}
		}
	})
	// The provider's SQL lists keep a fixed order, so queries are stable.
	t.Run("lists the types of kinds in a fixed order", func(t *testing.T) {
		if got := FoundryMediaTypesRead(FoundryReadText); !slices.Equal(got, []FoundryMediaType{FoundryMediaMarkdown, FoundryMediaMDX, FoundryMediaPlainText}) {
			t.Errorf("text types = %v", got)
		}
		if got := FoundryMediaTypesRead(FoundryReadText, FoundryReadPDF); len(got) != 4 || got[3] != FoundryMediaPDF {
			t.Errorf("text and PDF types = %v", got)
		}
		if got := FoundryMediaTypesRead(); got != nil {
			t.Errorf("no kind lists %v", got)
		}
	})
}

func ExampleFoundryMediaType_ReadKind() {
	for _, mediaType := range []FoundryMediaType{FoundryMediaMDX, FoundryMediaCSV, FoundryMediaPDF} {
		fmt.Println(mediaType.Format(), mediaType.ReadKind())
	}
	// Output:
	// MDX text
	// CSV table
	// PDF pdf
}

func TestFoundryReadState(t *testing.T) {
	// What the website shows for each stored type and read status: one
	// table, so a new status or type can't fall through unseen.
	t.Run("shows each read as the website words it", func(t *testing.T) {
		read := func(status FoundryFileReadStatus) *FoundryFileRead { return &FoundryFileRead{Status: status} }
		for _, tc := range []struct {
			mediaType FoundryMediaType
			read      *FoundryFileRead
			want      FoundryFileReadState
		}{
			{FoundryMediaMarkdown, nil, FoundryFileReadReady},
			{FoundryMediaMDX, nil, FoundryFileReadReady},
			{FoundryMediaPlainText, nil, FoundryFileReadReady},
			{FoundryMediaCSV, nil, FoundryFileReadReading},
			{FoundryMediaCSV, read(FoundryFileReadQueued), FoundryFileReadReading},
			{FoundryMediaTSV, read(FoundryFileReadRunning), FoundryFileReadReading},
			{FoundryMediaJSON, read(FoundryFileReadSucceeded), FoundryFileReadReady},
			{FoundryMediaJSONL, read(FoundryFileReadFailed), FoundryFileReadFailedState},
			{FoundryMediaParquet, read(FoundryFileReadSubmitted), FoundryFileReadReading},
			{FoundryMediaPDF, nil, FoundryFileReadReading},
			{FoundryMediaPDF, read(FoundryFileReadQueued), FoundryFileReadReading},
			{FoundryMediaPDF, read(FoundryFileReadRunning), FoundryFileReadReading},
			{FoundryMediaPDF, read(FoundryFileReadSubmitted), FoundryFileReadReading},
			{FoundryMediaPDF, read(FoundryFileReadSucceeded), FoundryFileReadReady},
			{FoundryMediaPDF, read(FoundryFileReadFailed), FoundryFileReadFailedState},
			{"text/html", nil, FoundryFileReadFailedState},
			{FoundryMediaCSV, read("lost"), FoundryFileReadFailedState},
		} {
			d := &FoundryDocument{ID: "d", MediaType: tc.mediaType, Read: tc.read}
			if got := d.ReadState(); got != tc.want {
				t.Errorf("%s with %+v reads as %q, want %q", tc.mediaType, tc.read, got, tc.want)
			}
		}
	})
	// A read waiting again after a failure says so; a fresh one doesn't.
	t.Run("tells a retry from a first try", func(t *testing.T) {
		message := "storage is unavailable"
		for _, tc := range []struct {
			read FoundryFileRead
			want bool
		}{
			{FoundryFileRead{Status: FoundryFileReadQueued}, false},
			{FoundryFileRead{Status: FoundryFileReadQueued, LastError: &message}, true},
			{FoundryFileRead{Status: FoundryFileReadFailed, LastError: &message}, false},
		} {
			if got := tc.read.Retrying(); got != tc.want {
				t.Errorf("%+v retrying = %t, want %t", tc.read, got, tc.want)
			}
		}
	})
}

func ExampleFoundryDocument_ReadState() {
	csv := &FoundryDocument{MediaType: FoundryMediaCSV, Read: &FoundryFileRead{Status: FoundryFileReadSucceeded}}
	pdf := &FoundryDocument{MediaType: FoundryMediaPDF, Read: &FoundryFileRead{Status: FoundryFileReadQueued}}
	fmt.Println(csv.ReadState(), pdf.ReadState())
	// Output: ready reading
}

func TestFoundryDocumentTextKey(t *testing.T) {
	// Loads, generations and the document page read a document's text from
	// one place: a text document's original, or the text a PDF's read
	// stored once it succeeded. Anything else has no text to read.
	t.Run("finds each document's text", func(t *testing.T) {
		text := "ws/w/documents/d/read/standard-abc.txt"
		read := func(status FoundryFileReadStatus, artifact *string) *FoundryFileRead {
			return &FoundryFileRead{Status: status, ArtifactKey: artifact}
		}
		for _, tc := range []struct {
			name      string
			mediaType FoundryMediaType
			read      *FoundryFileRead
			want      string
		}{
			{"Markdown is its original", FoundryMediaMarkdown, nil, "original"},
			{"MDX is its original", FoundryMediaMDX, nil, "original"},
			{"plain text is its original", FoundryMediaPlainText, nil, "original"},
			{"a read PDF is its text", FoundryMediaPDF, read(FoundryFileReadSucceeded, &text), text},
			{"a PDF being read has none", FoundryMediaPDF, read(FoundryFileReadRunning, nil), ""},
			{"a PDF not yet queued has none", FoundryMediaPDF, nil, ""},
			{"a PDF that failed has none", FoundryMediaPDF, read(FoundryFileReadFailed, nil), ""},
			{"a table file has none", FoundryMediaCSV, read(FoundryFileReadSucceeded, &text), ""},
			{"an unknown type has none", "text/html", nil, ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				d := &FoundryDocument{MediaType: tc.mediaType, S3Key: "original", Read: tc.read}
				key, ok := d.TextKey()
				if key != tc.want || ok != (tc.want != "") {
					t.Fatalf("TextKey() = %q, %t; want %q", key, ok, tc.want)
				}
			})
		}
	})

	// The provider's SQL names application/pdf as the one PDF type.
	t.Run("has one PDF type", func(t *testing.T) {
		if got := FoundryMediaTypesRead(FoundryReadPDF); len(got) != 1 || got[0] != FoundryMediaPDF {
			t.Fatalf("PDF types = %v", got)
		}
	})
}

// A connector's set is called as its connector's tables are, so two
// connections' Notes are told apart; a person's set is its name.
func ExampleFoundryDocumentSet_DisplayName() {
	contracts := &FoundryDocumentSet{Name: "Contracts"}
	notes := &FoundryDocumentSet{Name: "Notes", Owner: &FoundryDocumentSetOwner{ConnectorID: "c1", ConnectorName: "Team Granola", Stream: "notes"}}
	fmt.Println(contracts.DisplayName())
	fmt.Println(notes.DisplayName())
	fmt.Println(ConnectorPartName("Team Granola", "notes"))
	// Output:
	// Contracts
	// Team Granola / Notes
	// Team Granola / notes
}

// Prod's $50 month with a $10 job cap: a refresh or an extract no one asked
// for stops at $40, and a person's load may spend the last $10.
func ExampleFoundryGraphBudget_CapFor() {
	budget := FoundryGraphBudget{CapMicros: 50_000_000, ReserveMicros: 10_000_000}
	person := "user-1"
	fmt.Println(budget.CapFor(nil), budget.CapFor(&person))
	// Output:
	// 40000000 50000000
}

// Prod's API source allowance, $5 a source and $20 a workspace: a source
// that spent $1.50 of a workspace's $12 has $3.50 left; once the workspace
// is at $18 every source has $2 at most.
func ExampleFoundryDocumentSetSpend_Left() {
	allowance := FoundrySetAllowance{SetMicros: 5_000_000, WorkspaceMicros: 20_000_000}
	fmt.Println(FoundryDocumentSetSpend{USDMicros: 1_500_000, WorkspaceUSDMicros: 12_000_000}.Left(allowance))
	fmt.Println(FoundryDocumentSetSpend{USDMicros: 1_500_000, WorkspaceUSDMicros: 18_000_000}.Left(allowance))
	// Output:
	// 3500000
	// 2000000
}

func TestFoundryDocumentSetSpendLeft(t *testing.T) {
	// Whichever allowance is nearer its end bounds the set; at or past
	// either one nothing is left, however much the other has.
	allowance := FoundrySetAllowance{SetMicros: 5_000_000, WorkspaceMicros: 20_000_000}
	for _, tc := range []struct {
		name           string
		set, workspace int64
		want           int64
	}{
		{"nothing spent", 0, 0, 5_000_000},
		{"the set's allowance bounds it", 4_000_000, 10_000_000, 1_000_000},
		{"the workspace's allowance bounds it", 1_000_000, 19_500_000, 500_000},
		{"the set's spent", 5_000_000, 5_000_000, 0},
		{"the workspace's spent", 0, 20_000_000, 0},
		{"the set's overspent by what was in flight", 5_013_000, 6_000_000, -13_000},
		{"the workspace's overspent by what was in flight", 0, 20_004_500, -4_500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := FoundryDocumentSetSpend{USDMicros: tc.set, WorkspaceUSDMicros: tc.workspace}.Left(allowance)
			if got != tc.want {
				t.Fatalf("Left = %d, want %d", got, tc.want)
			}
		})
	}
}
