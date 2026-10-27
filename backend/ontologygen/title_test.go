package ontologygen

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	foundryreadv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundryread/v1"
)

// fakeWriter answers each title request with text, or err; requests are
// what it got. A nil text with no err waits for the call to end.
type fakeWriter struct {
	mu       sync.Mutex
	text     *string
	err      error
	requests []*foundryreadv1.TitleRequest
}

func (f *fakeWriter) Title(ctx context.Context, in *foundryreadv1.TitleRequest, _ ...grpc.CallOption) (*foundryreadv1.TitleResponse, error) {
	f.mu.Lock()
	f.requests = append(f.requests, in)
	text, err := f.text, f.err
	f.mu.Unlock()
	switch {
	case err != nil:
		return nil, err
	case text == nil:
		<-ctx.Done()
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	return &foundryreadv1.TitleResponse{Text: *text}, nil
}

func answering(text string) *fakeWriter { return &fakeWriter{text: &text} }

func titler(w titleWriter) *Titler {
	return &Titler{Writer: w, Timeout: TitleTimeout, Logger: slog.New(slog.DiscardHandler)}
}

// Without foundry-read, a title is the goal's first sentence, cleaned.
func ExampleTitler_Title() {
	titler := NewTitler(nil, nil)
	fmt.Println(titler.Title(context.Background(), "ws-1", "gen-1", "**Model our retail orders, stock and returns.** Include suppliers."))
	// Output: Model our retail orders, stock and returns
}

func TestTitlerTitle(t *testing.T) {
	ctx := context.Background()
	const goal = "We sell furniture online and ship from three warehouses. Model orders, stock and returns."
	const fallback = "We sell furniture online and ship from three warehouses"

	// foundry-read writes it on the workspace's setting, given the
	// workspace, the generation and the goal as its author wrote it.
	t.Run("uses the model's title", func(t *testing.T) {
		w := answering("Retail order fulfilment and inventory")
		if got := titler(w).Title(ctx, "ws-1", "gen-1", goal); got != "Retail order fulfilment and inventory" {
			t.Fatalf("title = %q", got)
		}
		if len(w.requests) != 1 || w.requests[0].GetWorkspaceId() != "ws-1" || w.requests[0].GetGenerationId() != "gen-1" ||
			w.requests[0].GetGoal() != goal {
			t.Fatalf("requests = %v", w.requests)
		}
	})
	t.Run("cleans the model's title", func(t *testing.T) {
		w := answering("\n## \"**Retail   orders** and `stock`.\"\nThis title covers orders.")
		if got := titler(w).Title(ctx, "ws-1", "gen-1", goal); got != "Retail orders and stock" {
			t.Fatalf("title = %q", got)
		}
	})

	// Every unusable answer, and every failure foundry-read reports, falls
	// back to the goal's first sentence.
	for name, w := range map[string]*fakeWriter{
		"a setting that can't run": {err: status.Error(codes.FailedPrecondition, "No AI provider is set up.")},
		"a model that failed":      {err: status.Error(codes.FailedPrecondition, "the title model failed: refused")},
		"foundry-read down":        {err: status.Error(codes.Unavailable, "connection refused")},
		"another error":            {err: errors.New("boom")},
		"an empty answer":          answering(""),
		"an answer without words":  answering("**\n> ...\n``"),
		"a list number":            answering("1."),
	} {
		t.Run("falls back on "+name, func(t *testing.T) {
			if got := titler(w).Title(ctx, "ws-1", "gen-1", goal); got != fallback || len(w.requests) != 1 {
				t.Fatalf("title = %q after %d requests, want %q", got, len(w.requests), fallback)
			}
		})
	}
	t.Run("falls back within its timeout", func(t *testing.T) {
		tt := titler(&fakeWriter{})
		tt.Timeout = 50 * time.Millisecond
		start := time.Now()
		if got := tt.Title(ctx, "ws-1", "gen-1", goal); got != fallback {
			t.Fatalf("title = %q", got)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("Title took %s with a %s timeout", elapsed, tt.Timeout)
		}
	})
	// No foundry-read client, as when backend.foundry_read.address is unset.
	t.Run("only falls back without foundry-read", func(t *testing.T) {
		var none foundryreadv1.FoundryReadClient
		tt := NewTitler(none, nil)
		if tt.Writer != nil || tt.Timeout != TitleTimeout {
			t.Fatalf("titler = %+v, want no writer", tt)
		}
		if got := tt.Title(ctx, "ws-1", "gen-1", goal); got != fallback {
			t.Fatalf("title = %q", got)
		}
	})
	t.Run("asks nothing of a goal without words", func(t *testing.T) {
		w := answering("Anything")
		if got := titler(w).Title(ctx, "ws-1", "gen-1", " ...\n**\n"); got != "" || len(w.requests) != 0 {
			t.Fatalf("title = %q after %d requests", got, len(w.requests))
		}
	})
}

func TestFallbackTitle(t *testing.T) {
	for _, tc := range []struct{ goal, want string }{
		{"Model our retail orders. Also returns!", "Model our retail orders"},
		{"What drives churn? Look at invoices.", "What drives churn"},
		{`Model "late payers." Then refunds`, "Model late payers"},
		{"\n\n  Map suppliers\nand parts", "Map suppliers"},
		{"...\nRetail stock levels", "Retail stock levels"},
		{"Version 3.5 rollout of e-commerce pricing", "Version 3.5 rollout of e-commerce pricing"},
		{"Track every customer order from checkout through payment, picking, packing, shipping and returns",
			"Track every customer order from checkout through payment"},
		{"1. Map suppliers to parts\n2. Track lead times", "Map suppliers to parts"},
		{"1) Map suppliers\n2) Track lead times", "Map suppliers"},
		{"2024: 3.5%\nRetail stock levels", "Retail stock levels"},
		{"_Retail orders_. Then refunds", "Retail orders"},
		{"[Retail orders](https://example.com/a) and stock. More", "Retail orders and stock"},
	} {
		if got := firstTitle(sentenceEnd.Split(tc.goal, -1)); got != tc.want {
			t.Errorf("fallback for %q = %q, want %q", tc.goal, got, tc.want)
		}
	}
}

func TestCleanTitle(t *testing.T) {
	sixty := strings.Repeat("abcd ", 11) + "abcde" // 60 characters, ending a word
	for _, tc := range []struct{ name, in, want string }{
		{"keeps a plain title", "Retail order fulfilment and inventory", "Retail order fulfilment and inventory"},
		{"drops double quotes", `"Retail “orders” «and» stock"`, "Retail orders and stock"},
		{"drops surrounding single quotes", "'Retail orders'", "Retail orders"},
		{"keeps apostrophes", "Customer's orders", "Customer's orders"},
		{"drops Markdown marks", "# **Retail** `orders`", "Retail orders"},
		{"drops list and quote markers", "> - + Retail orders", "Retail orders"},
		{"drops ordered list markers", "> 1. 2) Retail orders", "Retail orders"},
		{"keeps leading numbers", "3 warehouses and 1.5 million orders", "3 warehouses and 1.5 million orders"},
		{"drops wrapping underscores", "__Retail orders_", "Retail orders"},
		{"keeps underscores inside words", "order_items by sku_", "order_items by sku"},
		{"keeps a link's text", "[Retail orders](http://x) and ![stock](s.png)", "Retail orders and stock"},
		{"collapses whitespace and control characters", "Retail\t orders\u0007 and​ stock", "Retail orders and stock"},
		{"cuts trailing punctuation", "Retail orders...!?: —", "Retail orders"},
		{"keeps closing brackets and percentages", "Churn above 10% (beta)", "Churn above 10% (beta)"},
		{"keeps a title of exactly the limit", sixty, sixty},
		{"ends a longer title at a word boundary", sixty + " more", sixty},
		{"ends at the last word that fits", strings.Repeat("abcd ", 11) + "abcdefgh", strings.Repeat("abcd ", 10) + "abcd"},
		{"cuts punctuation left by the limit", strings.Repeat("abcd ", 10) + "abc, defghijk", strings.Repeat("abcd ", 10) + "abc"},
		{"cuts a single overlong word", strings.Repeat("x", 80), strings.Repeat("x", 60)},
		{"counts characters, not bytes", strings.Repeat("é", 59) + " ü", strings.Repeat("é", 59)},
		{"is empty without words", "  ** `` ... ", ""},
		{"is empty without letters", "1. 2024 (3.5%)", ""},
		{"is empty without letters within the limit", strings.Repeat("1", 70) + " abc", ""},
	} {
		got := cleanTitle(tc.in)
		if got != tc.want {
			t.Errorf("%s: cleanTitle(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
		if n := utf8.RuneCountInString(got); n > MaxTitleChars || strings.ContainsAny(got, "\n\r\"`*") || (got != "" && !strings.ContainsFunc(got, unicode.IsLetter)) {
			t.Errorf("%s: cleanTitle(%q) = %q (%d characters) is not one plain line", tc.name, tc.in, got, n)
		}
	}
}
