package slack

import (
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Mrkdwn is text safe to send Slack as mrkdwn: every &, < and > escaped but
// in the links it makes, which go only to Foundry's site; every other URL,
// and anything that reads as a domain, in inline code; every code span
// closed. Site.Text makes it from any string; Bold, Italic, Join,
// UserMention and ChannelMention compose it. Its zero value is empty text.
type Mrkdwn struct{ s string }

// String is the mrkdwn as Slack is sent it.
func (m Mrkdwn) String() string { return m.s }

// Len is its length in characters, as Slack counts its limits.
func (m Mrkdwn) Len() int { return utf8.RuneCountInString(m.s) }

// Site is Foundry's site: the one origin Mrkdwn links to.
type Site struct {
	scheme, host string
}

// NewSite reads the site's origin: http or https, a host, and nothing after
// it but an optional "/".
func NewSite(origin string) (Site, error) {
	u, err := url.Parse(origin)
	switch {
	case err != nil:
		return Site{}, err
	case (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "":
		return Site{}, errors.New("slack: the site must be an http or https origin")
	}
	return Site{scheme: u.Scheme, host: strings.ToLower(u.Host)}, nil
}

// Origin is the site's origin, as "https://host".
func (s Site) Origin() string { return s.scheme + "://" + s.host }

// Link is an absolute URL on the site (Site.Page).
type Link struct{ u string }

// String is the URL.
func (l Link) String() string { return l.u }

// Page is the URL of a path on the site, which may carry a query and a
// fragment: "/slack/link?workspace=…#t=…".
func (s Site) Page(path string) (Link, error) {
	if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
		return Link{}, errors.New("slack: a page's path starts with one /")
	}
	u, err := url.Parse(s.Origin() + path)
	if err != nil || !s.owns(u) {
		return Link{}, errors.New("slack: not a page on the site")
	}
	return Link{u: u.String()}, nil
}

// owns reports whether u is on the site.
func (s Site) owns(u *url.URL) bool {
	return u.Scheme == s.scheme && strings.ToLower(u.Host) == s.host && u.User == nil && u.Opaque == ""
}

// Link is mrkdwn linking label to a page of the site.
func (s Site) Link(l Link, label string) Mrkdwn {
	if label = strings.TrimSpace(label); label == "" {
		label = l.u
	}
	return Mrkdwn{s: "<" + escape(strings.ReplaceAll(l.u, "|", "%7C")) + "|" + escape(label) + ">"}
}

// UserMention mentions a Slack user (never a group, a channel or everyone);
// an id that isn't one shows as written.
func UserMention(userID string) Mrkdwn {
	if !idRE.MatchString(userID) {
		return Mrkdwn{s: "@" + escape(userID)}
	}
	return Mrkdwn{s: "<@" + userID + ">"}
}

// ChannelMention names a Slack channel as Slack shows it, by its id; an id
// that isn't one shows as written.
func ChannelMention(channelID string) Mrkdwn {
	if !idRE.MatchString(channelID) {
		return Mrkdwn{s: "#" + escape(channelID)}
	}
	return Mrkdwn{s: "<#" + channelID + ">"}
}

// Bold makes m bold.
func Bold(m Mrkdwn) Mrkdwn {
	if m.s == "" {
		return m
	}
	return Mrkdwn{s: "*" + m.s + "*"}
}

// Italic makes m italic.
func Italic(m Mrkdwn) Mrkdwn {
	if m.s == "" {
		return m
	}
	return Mrkdwn{s: "_" + m.s + "_"}
}

// Join puts parts one after another, a space between two code spans that
// would otherwise touch.
func Join(parts ...Mrkdwn) Mrkdwn {
	var b strings.Builder
	for _, p := range parts {
		appendPiece(&b, p.s)
	}
	return Mrkdwn{s: b.String()}
}

// appendPiece appends piece, keeping a backtick that ends what is written
// from touching one that starts piece.
func appendPiece(b *strings.Builder, piece string) {
	if strings.HasPrefix(piece, "`") && strings.HasSuffix(b.String(), "`") {
		b.WriteByte(' ')
	}
	b.WriteString(piece)
}

// escape escapes the three characters Slack's text formatting reads.
func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

var (
	// schemeRE starts a URL of any scheme.
	schemeRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)
	// domainRE is anything that reads as a domain or an IP address: a label,
	// a dot and two letters or more, or four dotted numbers.
	domainRE = regexp.MustCompile(`[\p{L}\p{N}][\p{L}\p{N}_-]*\.\p{L}{2,}|\d+\.\d+\.\d+\.\d+`)
)

// leading and trailing are the punctuation a word may start and end with
// that isn't part of a URL or domain it holds.
const (
	leading  = `([{<"'*_~`
	trailing = `.,;:!?)]}>"'*_~`
)

// maxLink is the longest URL Text links; a longer one is shown in code, so
// Split always finds room around it.
const maxLink = 1000

// Text makes any string (a model's words, a document's, a tool's) safe
// mrkdwn. It keeps the code spans it holds (a closed `span` on one line, a
// closed ```block```) and escapes their insides; an unclosed backtick
// becomes an apostrophe. Outside code, a word is escaped, unless it is a URL
// on the site, which becomes a link, or another URL, or holds anything that
// reads as a domain (an email address, "example.com/path") or holds "://",
// which goes in inline code so Slack never links it.
func (s Site) Text(raw string) Mrkdwn {
	var b strings.Builder
	for _, seg := range segments(raw) {
		if seg.code {
			appendPiece(&b, escape(seg.text))
			continue
		}
		text := seg.text
		for text != "" {
			space := strings.IndexFunc(text, unicode.IsSpace)
			if space == 0 {
				_, size := utf8.DecodeRuneInString(text)
				b.WriteString(text[:size])
				text = text[size:]
				continue
			}
			word := text
			if space > 0 {
				word = text[:space]
			}
			text = text[len(word):]
			s.word(&b, word)
		}
	}
	return Mrkdwn{s: b.String()}
}

// word writes one whitespace-free word of plain text.
func (s Site) word(b *strings.Builder, word string) {
	core := strings.TrimLeft(word, leading)
	pre := word[:len(word)-len(core)]
	core = strings.TrimRight(core, trailing)
	post := word[len(pre)+len(core):]
	b.WriteString(escape(pre))
	switch {
	case core == "":
	case strings.Contains(core, "://"):
		if u, err := url.Parse(core); err == nil && schemeRE.MatchString(core) && len(core) <= maxLink && s.owns(u) {
			b.WriteString(s.Link(Link{u: u.String()}, core).s)
		} else {
			appendPiece(b, "`"+escape(core)+"`")
		}
	case domainRE.MatchString(core):
		appendPiece(b, "`"+escape(core)+"`")
	default:
		b.WriteString(escape(core))
	}
	b.WriteString(escape(post))
}

// segment is a run of text, in a code span or not.
type segment struct {
	text string
	code bool
}

// segments cuts raw into code spans and plain text, turning a backtick that
// opens no span into an apostrophe, so the plain text holds no backtick. An
// inline span is closed on its line and holds something with no space at
// either end; a block is closed by the next ``` and holds something, its
// own backticks turned into "ˋ", so no fence is ever ambiguous.
func segments(raw string) []segment {
	var (
		out   []segment
		plain strings.Builder
	)
	flush := func() {
		if plain.Len() > 0 {
			out = append(out, segment{text: plain.String()})
			plain.Reset()
		}
	}
	for i := 0; i < len(raw); {
		rest := raw[i:]
		if strings.HasPrefix(rest, "```") {
			if end := strings.Index(rest[3:], "```"); end > 0 {
				flush()
				out = append(out, segment{text: "```" + strings.ReplaceAll(rest[3:3+end], "`", "ˋ") + "```", code: true})
				i += end + 6
				continue
			}
			plain.WriteString("'''")
			i += 3
			continue
		}
		if rest[0] == '`' {
			end := strings.IndexAny(rest[1:], "`\n")
			if end > 0 && rest[1+end] == '`' {
				inside := rest[1 : 1+end]
				first, _ := utf8.DecodeRuneInString(inside)
				last, _ := utf8.DecodeLastRuneInString(inside)
				if !unicode.IsSpace(first) && !unicode.IsSpace(last) {
					flush()
					out = append(out, segment{text: rest[:end+2], code: true})
					i += end + 2
					continue
				}
			}
			plain.WriteByte('\'')
			i++
			continue
		}
		plain.WriteByte(raw[i])
		i++
	}
	flush()
	return out
}

// unitKind is what a unit is to a code span.
type unitKind uint8

const (
	outside unitKind = iota // not in code
	opening                 // a span's opening fence
	content                 // inside a span
	closing                 // a span's closing fence
)

// unit is one piece Split may not cut inside: a character, an entity
// (&amp;), a link, or a code fence. fence is the fence of the span it is
// in or is ("`" or "```"), "" outside one.
type unit struct {
	text  string
	kind  unitKind
	fence string
}

// units reads Mrkdwn into units.
func units(s string) []unit {
	var out []unit
	span := func(fence string) {
		end := strings.Index(s[len(fence):], fence) + len(fence)
		out = append(out, unit{text: fence, kind: opening, fence: fence})
		for inside := s[len(fence):end]; inside != ""; {
			_, n := utf8.DecodeRuneInString(inside)
			if inside[0] == '&' {
				n = strings.IndexByte(inside, ';') + 1
			}
			out = append(out, unit{text: inside[:n], kind: content, fence: fence})
			inside = inside[n:]
		}
		out = append(out, unit{text: fence, kind: closing, fence: fence})
		s = s[end+len(fence):]
	}
	for s != "" {
		n := 0
		switch {
		case strings.HasPrefix(s, "```"):
			span("```")
			continue
		case s[0] == '`':
			span("`")
			continue
		case s[0] == '<':
			n = strings.IndexByte(s, '>') + 1
		case s[0] == '&':
			n = strings.IndexByte(s, ';') + 1
		default:
			_, n = utf8.DecodeRuneInString(s)
		}
		out = append(out, unit{text: s[:n]})
		s = s[n:]
	}
	return out
}

// Split cuts m into parts of at most max characters (max is at least 16),
// each safe on its own: a cut never falls inside a link or an entity, and a
// code span cut in two is closed and opened again. It cuts after the last
// newline that fits in the part's second half, else the last space there,
// else as late as it can, outside code where it can. A link longer than
// max is a part of its own. Empty text has no parts.
func Split(m Mrkdwn, max int) []Mrkdwn {
	if max < 16 {
		panic("slack: Split needs room for code fences around an entity")
	}
	switch {
	case m.s == "":
		return nil
	case m.Len() <= max:
		return []Mrkdwn{m}
	}
	all := units(m.s)
	var parts []Mrkdwn
	open := "" // the fence the next part opens with: the last cut was in code
	for len(all) > 0 {
		// best is the latest cut of each rank: 0 after a newline outside
		// code, 1 after a space outside code, 2 elsewhere outside code, 3 in
		// code, 4 in code beside a space (which may undo an inline span);
		// late counts only cuts in the part's second half.
		best, late := [5]int{}, [5]int{}
		size := utf8.RuneCountInString(open)
		for n := 1; n <= len(all); n++ {
			size += utf8.RuneCountInString(all[n-1].text)
			if size > max {
				break
			}
			rank := 2
			switch {
			case n == len(all):
				rank = 0
			case all[n].kind == closing || all[n-1].kind == opening:
				continue // a span with nothing in it
			case all[n].kind == content:
				if size+len(all[n].fence) > max {
					continue
				}
				rank = 3
				if strings.TrimSpace(all[n-1].text) == "" || strings.TrimSpace(all[n].text) == "" {
					rank = 4
				}
			case all[n-1].text == "\n":
				rank = 0
			case all[n-1].text == " ":
				rank = 1
			}
			best[rank] = n
			if size > max/2 {
				late[rank] = n
			}
		}
		cut := 0
		for _, ranks := range [][5]int{late, best} {
			for _, at := range ranks {
				if cut == 0 && at > 0 {
					cut = at
				}
			}
		}
		if cut == 0 {
			cut = 1 // a link longer than max
		}
		var b strings.Builder
		b.WriteString(open)
		for _, u := range all[:cut] {
			b.WriteString(u.text)
		}
		open = ""
		if cut < len(all) && all[cut].kind == content {
			open = all[cut].fence
			b.WriteString(open)
		}
		parts = append(parts, Mrkdwn{s: b.String()})
		all = all[cut:]
	}
	return parts
}

// Cut is m cut to at most max characters (max is at least 17), ending "…"
// when cut, at a place Split would cut.
func Cut(m Mrkdwn, max int) Mrkdwn {
	if m.Len() <= max {
		return m
	}
	return Mrkdwn{s: Split(m, max-1)[0].s + "…"} // m isn't empty: it is longer than max
}

// slackFormatRE is a piece of Slack's formatting in text Slack sends:
// <@U…>, <#C…|name>, <!here>, <url|label>.
var slackFormatRE = regexp.MustCompile(`<([^<>]*)>`)

// MentionedUsers are the users text Slack sent mentions (<@U…>), each once,
// in order.
func MentionedUsers(raw string) []string {
	var out []string
	for _, m := range slackFormatRE.FindAllStringSubmatch(raw, -1) {
		id, _, _ := strings.Cut(strings.TrimPrefix(m[1], "@"), "|")
		if strings.HasPrefix(m[1], "@") && idRE.MatchString(id) && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// InputText reads text Slack sent as plain words for Founder: the bot's own
// mention dropped, another user's as "@" and their name (names, else the
// label Slack gave, else the id), a channel as "#name", a special mention as
// what it shows ("@here"), a link as "label (url)" or the URL, and Slack's
// escapes undone. It trims the result.
func InputText(raw, botUserID string, names func(userID string) string) string {
	out := slackFormatRE.ReplaceAllStringFunc(raw, func(m string) string {
		inner := m[1 : len(m)-1]
		target, label, labelled := strings.Cut(inner, "|")
		switch {
		case strings.HasPrefix(target, "@"):
			id := target[1:]
			if id == botUserID {
				return ""
			}
			if name := names(id); name != "" {
				return "@" + name
			}
			if labelled && label != "" {
				return "@" + strings.TrimPrefix(label, "@")
			}
			return "@" + id
		case strings.HasPrefix(target, "#"):
			if labelled && label != "" {
				return "#" + label
			}
			return "#channel"
		case strings.HasPrefix(target, "!"):
			if labelled && label != "" {
				return label
			}
			special, _, _ := strings.Cut(target[1:], "^")
			return "@" + special
		case labelled && label != "" && label != target:
			return label + " (" + target + ")"
		}
		return target
	})
	out = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(out)
	return strings.TrimSpace(out)
}
