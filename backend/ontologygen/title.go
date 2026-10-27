package ontologygen

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode"

	"google.golang.org/grpc"

	foundryreadv1 "github.com/helixdb/helix-foundry/backend/pb/gen/go/helix/foundryread/v1"
)

const (
	// MaxTitleChars bounds a generation's title, in characters.
	MaxTitleChars = 60
	// TitleTimeout bounds the title call, so queueing a generation waits at most this long for it.
	TitleTimeout = 4 * time.Second
	// titleTrailing is cut from the end of a title: punctuation, dashes, underscores, single quotes and spaces.
	titleTrailing = ".,;:!?…-–—_'‘’ "
)

// sentenceEnd splits a goal into sentences: after '.', '!', '?' or '…' and
// any closing quotes or Markdown marks, followed by whitespace, and at every
// line break.
var sentenceEnd = regexp.MustCompile("[.!?…]+[\"'”’*_`]*\\s+|[\r\n]+")

var (
	// markdownLink is a Markdown link or image; a title keeps its text.
	markdownLink = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	// titleLeading is cut from the start of a title: heading, quote and list
	// markers ("1." and "1)" included), underscores, single quotes and spaces.
	titleLeading = regexp.MustCompile(`^(?:[#>+_'‘’ -]|\d+[.)] )+`)
)

// titleWriter writes titles; foundryreadv1.FoundryReadClient is one.
type titleWriter interface {
	Title(ctx context.Context, in *foundryreadv1.TitleRequest, opts ...grpc.CallOption) (*foundryreadv1.TitleResponse, error)
}

// Titler titles generations from their goals. foundry-read has the
// workspace's AI setting write the title, on its document-reading model
// without reasoning (Haiku, thinking disabled, for Helix's Claude), through
// the ontology agent; without it, and whenever its answer is unusable, the
// title is the goal's first sentence. Both are cleaned the same way.
type Titler struct {
	// Writer is foundry-read; nil titles every generation with its goal's
	// first sentence.
	Writer  titleWriter
	Timeout time.Duration
	Logger  *slog.Logger
}

// NewTitler titles through foundry-read's client, or only falls back when
// it is nil. Fallbacks are logged to logger, or slog.Default when it is nil.
func NewTitler(read foundryreadv1.FoundryReadClient, logger *slog.Logger) *Titler {
	if logger == nil {
		logger = slog.Default()
	}
	t := &Titler{Timeout: TitleTimeout, Logger: logger}
	if read != nil {
		t.Writer = read
	}
	return t
}

// Title is a one-line, plain-text title of generation's goal of at most
// MaxTitleChars characters, or "" when goal has no letters. It never
// fails, and waits for the model at most Timeout.
func (t *Titler) Title(ctx context.Context, workspaceID, generationID, goal string) string {
	fallback := firstTitle(sentenceEnd.Split(goal, -1))
	if t.Writer == nil || fallback == "" {
		return fallback
	}
	title, err := t.ask(ctx, workspaceID, generationID, goal)
	if err != nil {
		t.Logger.WarnContext(ctx, "titled an ontology generation with its goal's first sentence", "generation", generationID, "error", err)
		return fallback
	}
	return title
}

// ask has the workspace's model title goal within Timeout: the first line of
// its answer that cleans to a title.
func (t *Titler) ask(ctx context.Context, workspaceID, generationID, goal string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, t.Timeout)
	defer cancel()
	resp, err := t.Writer.Title(ctx, &foundryreadv1.TitleRequest{WorkspaceId: workspaceID, GenerationId: generationID, Goal: goal})
	if err != nil {
		return "", err
	}
	title := firstTitle(strings.Split(resp.GetText(), "\n"))
	if title == "" {
		return "", errors.New("the title model answered without a title")
	}
	return title, nil
}

// firstTitle is the first of parts that cleans to a title, or "".
func firstTitle(parts []string) string {
	for _, part := range parts {
		if title := cleanTitle(part); title != "" {
			return title
		}
	}
	return ""
}

// cleanTitle makes s one line of plain text of at most MaxTitleChars
// characters, or "" when it has no letters. Markdown links keep their text,
// Markdown emphasis and code marks and double quotes go, other whitespace and
// control characters become spaces and runs of spaces one, titleLeading and
// titleTrailing are cut, and a longer title ends at the last whole word that
// fits, or mid-word when its first word is longer than a title.
func cleanTitle(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case strings.ContainsRune("*`\"“”„«»", r):
			return -1
		case !unicode.IsPrint(r):
			return ' '
		}
		return r
	}, markdownLink.ReplaceAllString(s, "$1"))
	s = strings.TrimRight(titleLeading.ReplaceAllString(strings.Join(strings.Fields(s), " "), ""), titleTrailing)
	if runes := []rune(s); len(runes) > MaxTitleChars {
		// One past the limit, so a space there ends the last word that fits.
		head := string(runes[:MaxTitleChars+1])
		if i := strings.LastIndexByte(head, ' '); i > 0 {
			s = head[:i]
		} else {
			s = string(runes[:MaxTitleChars])
		}
		s = strings.TrimRight(s, titleTrailing)
	}
	if !strings.ContainsFunc(s, unicode.IsLetter) {
		return ""
	}
	return s
}
