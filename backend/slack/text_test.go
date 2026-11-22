package slack_test

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/helixdb/helix-foundry/backend/slack"
)

const origin = "https://foundry.helix-db.com"

func site(t testing.TB) slack.Site {
	t.Helper()
	s, err := slack.NewSite(origin)
	require.NoError(t, err)
	return s
}

func TestNewSite(t *testing.T) {
	for _, ok := range []string{"https://foundry.helix-db.com", "https://foundry.helix-db.com/", "http://localhost:3000", "https://FOUNDRY.helix-db.com"} {
		s, err := slack.NewSite(ok)
		require.NoError(t, err, ok)
		require.Equal(t, strings.ToLower(strings.TrimSuffix(ok, "/")), s.Origin())
	}
	for _, bad := range []string{"", "foundry.helix-db.com", "ftp://foundry.helix-db.com", "https://", "https://a@foundry.helix-db.com",
		"https://foundry.helix-db.com/app", "https://foundry.helix-db.com?x=1", "https://foundry.helix-db.com#x", "https://%zz", "https:opaque"} {
		_, err := slack.NewSite(bad)
		require.Error(t, err, bad)
	}
}

func TestSitePageAndLink(t *testing.T) {
	s := site(t)
	page, err := s.Page("/slack/link?workspace=W1&x=a|b#t=hx_sl_abc")
	require.NoError(t, err)
	require.Equal(t, "https://foundry.helix-db.com/slack/link?workspace=W1&x=a|b#t=hx_sl_abc", page.String())
	require.Equal(t, "<https://foundry.helix-db.com/slack/link?workspace=W1&amp;x=a%7Cb#t=hx_sl_abc|Link &lt;you&gt;>", s.Link(page, "Link <you>").String())
	require.Equal(t, "<https://foundry.helix-db.com/slack/link?workspace=W1&amp;x=a%7Cb#t=hx_sl_abc|https://foundry.helix-db.com/slack/link?workspace=W1&amp;x=a|b#t=hx_sl_abc>",
		s.Link(page, "  ").String())
	for _, bad := range []string{"", "slack/link", "//evil.com/x", "/%zz"} {
		_, err := s.Page(bad)
		require.Error(t, err, bad)
	}
}

// Text, word by word: escaped, the site's URLs linked, every other URL and
// domain in code, code spans kept and closed.
func TestText(t *testing.T) {
	s := site(t)
	for in, want := range map[string]string{
		"":                                 "",
		"hello & <world>":                  "hello &amp; &lt;world&gt;",
		"<!here> <!channel> <@U123> <#C1>": "&lt;!here&gt; &lt;!channel&gt; &lt;@U123&gt; &lt;#C1&gt;",
		"@here, @channel":                  "@here, @channel",
		"*bold* and _it_ and ~gone~":       "*bold* and _it_ and ~gone~",
		"see https://foundry.helix-db.com/ontology/01J.": "see <https://foundry.helix-db.com/ontology/01J|https://foundry.helix-db.com/ontology/01J>.",
		"(https://foundry.helix-db.com/a?b=1&c=2)":       "(<https://foundry.helix-db.com/a?b=1&amp;c=2|https://foundry.helix-db.com/a?b=1&amp;c=2>)",
		"https://FOUNDRY.helix-db.com/x":                 "<https://FOUNDRY.helix-db.com/x|https://FOUNDRY.helix-db.com/x>",
		"go to https://evil.com/x now":                   "go to `https://evil.com/x` now",
		"http://foundry.helix-db.com/x":                  "`http://foundry.helix-db.com/x`",
		"https://foundry.helix-db.com@evil.com/":         "`https://foundry.helix-db.com@evil.com/`",
		"https://foundry.helix-db.com.evil.com/":         "`https://foundry.helix-db.com.evil.com/`",
		"https://foundry.helix-db.com:8443/":             "`https://foundry.helix-db.com:8443/`",
		"<https://evil.com|click here>":                  "&lt;`https://evil.com|click` here&gt;",
		"ftp://files and slack://open":                   "`ftp://files` and `slack://open`",
		"x:https://localhost":                            "`x:https://localhost`",
		"mail ada@acme.com.":                             "mail `ada@acme.com`.",
		"visit example.com/path?q=1&r=<2>, now":          "visit `example.com/path?q=1&amp;r=&lt;2`&gt;, now",
		"foundry.helix-db.com":                           "`foundry.helix-db.com`",
		"Node.js, e.g. 3.14 or i.e. U.S.A":               "`Node.js`, e.g. 3.14 or i.e. U.S.A",
		"10.0.0.1":                                       "`10.0.0.1`",
		"пример.рф":                                      "`пример.рф`",
		"`code example.com <x>` kept":                    "`code example.com &lt;x&gt;` kept",
		"it`s fine":                                      "it's fine",
		"`` empty":                                       "'' empty",
		"` spaced ` x":                                   "' spaced ' x",
		"`a\nb` x":                                       "'a\nb' x",
		"```\nx <y> example.com\n```":                    "```\nx &lt;y&gt; example.com\n```",
		"````x`` y```":                                   "```ˋxˋˋ y```",
		"`````` x":                                       "'''''' x",
		"```unclosed example.com":                        "'''unclosed `example.com`",
		"`a``b.com`":                                     "`a` `b.com`",
		"`a`b.com":                                       "`a` `b.com`",
		"tabs\tand\nnewlines  kept":                      "tabs\tand\nnewlines  kept",
		"*example.com*":                                  "*`example.com`*",
	} {
		got := s.Text(in)
		require.Equal(t, want, got.String(), "%q", in)
		check(t, got.String())
	}
	long := "https://foundry.helix-db.com/" + strings.Repeat("a", 1000)
	require.Equal(t, "`"+long+"`", s.Text(long).String())
	require.Equal(t, 9, s.Text("a & b").Len())
}

func TestCompose(t *testing.T) {
	s := site(t)
	require.Equal(t, "*a &amp; b*", slack.Bold(s.Text("a & b")).String())
	require.Equal(t, "_a_", slack.Italic(s.Text("a")).String())
	require.Equal(t, "", slack.Bold(slack.Mrkdwn{}).String())
	require.Equal(t, "", slack.Italic(slack.Mrkdwn{}).String())
	require.Equal(t, "<@U0ADA0001>", slack.UserMention("U0ADA0001").String())
	require.Equal(t, "@&lt;!here&gt;", slack.UserMention("<!here>").String())
	require.Equal(t, "Ran by <@U1>: `x.com` `y.com`", slack.Join(s.Text("Ran by "), slack.UserMention("U1"), s.Text(": x.com"), s.Text("y.com")).String())
	require.Equal(t, "", slack.Join().String())
}

var (
	checkDomainRE = regexp.MustCompile(`[\p{L}\p{N}][\p{L}\p{N}_-]*\.\p{L}{2,}|\d+\.\d+\.\d+\.\d+`)
	entityRE      = regexp.MustCompile(`^&(amp|lt|gt);`)
)

// check fails unless out is safe mrkdwn, read the way Slack reads it: no
// special mention, every link to the site, every entity one of the three,
// every code span closed (inline on one line), and nothing outside code
// that reads as a URL or a domain.
func check(t testing.TB, out string) {
	t.Helper()
	require.NotContains(t, out, "<!")
	require.NotContains(t, out, "<#")
	var plain strings.Builder
	words := func() {
		for _, w := range strings.FieldsFunc(plain.String(), unicode.IsSpace) {
			core := strings.TrimRight(strings.TrimLeft(w, `([{<"'*_~`), `.,;:!?)]}>"'*_~`)
			require.NotContains(t, core, "://", "%q in %q", w, out)
			require.False(t, checkDomainRE.MatchString(core), "domain %q outside code in %q", w, out)
		}
		plain.Reset()
	}
	for i := 0; i < len(out); {
		rest := out[i:]
		switch {
		case strings.HasPrefix(rest, "```"):
			end := strings.Index(rest[3:], "```")
			require.Greater(t, end, 0, "an unclosed or empty block in %q", out)
			require.NotContains(t, rest[3:3+end], "<", out)
			require.NotContains(t, rest[3:3+end], "`", out)
			words()
			i += end + 6
		case rest[0] == '`':
			end := strings.IndexAny(rest[1:], "`\n")
			require.Greater(t, end, 0, "an unclosed or empty span in %q", out)
			require.Equal(t, byte('`'), rest[1+end], "a span across lines in %q", out)
			require.NotContains(t, rest[1:1+end], "<", out)
			words()
			i += end + 2
		case rest[0] == '<':
			end := strings.IndexByte(rest, '>')
			require.Greater(t, end, 0, out)
			target, _, _ := strings.Cut(rest[1:end], "|")
			if !strings.HasPrefix(target, "@") {
				u, err := url.Parse(strings.ReplaceAll(target, "&amp;", "&"))
				require.NoError(t, err, out)
				require.Equal(t, "https", u.Scheme, out)
				require.Equal(t, "foundry.helix-db.com", strings.ToLower(u.Host), out)
				require.Nil(t, u.User, out)
			}
			words()
			i += end + 1
		case rest[0] == '&':
			require.Regexp(t, entityRE, rest, "a bare & in %q", out)
			plain.WriteString(rest[:strings.IndexByte(rest, ';')+1])
			i += strings.IndexByte(rest, ';') + 1
		default:
			require.NotEqual(t, byte('>'), rest[0], out)
			plain.WriteByte(rest[0])
			i++
		}
	}
	words()
}

func FuzzText(f *testing.F) {
	for _, seed := range []string{
		"see https://foundry.helix-db.com/x and https://evil.com", "ada@acme.com", "`a``b.com`", "```x```y.com", "<!here>",
		"it`s example.com", "a&b.com", "(evil.com)", "https://foundry.helix-db.com@evil.com", "``` ``` `` `", "x\n`y\nz` w.io",
		"*bold* _it_ ~s~ `c`", "&amp;lt;", "<https://foundry.helix-db.com/x|label>", "\xff.com", "e.g. 1.2.3.4",
	} {
		f.Add(seed)
	}
	s := site(f)
	f.Fuzz(func(t *testing.T, raw string) {
		out := s.Text(raw)
		check(t, out.String())
		require.Equal(t, out.String(), slack.Join(out).String())
		for _, max := range []int{17, 40, 3000} {
			parts := slack.Split(out, max)
			var joined strings.Builder
			for _, p := range parts {
				// Only a link longer than max is a part longer than max.
				if p.Len() > max {
					require.True(t, strings.HasPrefix(p.String(), "<") && strings.HasSuffix(p.String(), ">") &&
						strings.Count(p.String(), "<") == 1, "part %q of %d", p, max)
				}
				require.NotEmpty(t, p.String())
				check(t, p.String())
				joined.WriteString(p.String())
			}
			// The parts hold the text, but for the fences a cut adds.
			require.Equal(t, strings.ReplaceAll(out.String(), "`", ""), strings.ReplaceAll(joined.String(), "`", ""))
			cut := slack.Cut(out, max)
			require.LessOrEqual(t, cut.Len(), max+maxLinkOver(cut))
			check(t, strings.TrimSuffix(cut.String(), "…"))
		}
	})
}

// maxLinkOver is how far past its max a cut text may run: by the length of
// a link it starts with.
func maxLinkOver(m slack.Mrkdwn) int {
	if !strings.HasPrefix(m.String(), "<") {
		return 0
	}
	return utf8.RuneCountInString(m.String())
}

func TestSplit(t *testing.T) {
	s := site(t)
	short := s.Text("short")
	require.Equal(t, []slack.Mrkdwn{short}, slack.Split(short, 16))
	require.Panics(t, func() { slack.Split(short, 15) })
	require.Empty(t, slack.Split(slack.Mrkdwn{}, 16))

	// After the last newline in the part's second half, else the last space.
	parts := slack.Split(s.Text("aaaa bbbb\ncccc dddd eeee ffff"), 16)
	require.Equal(t, []string{"aaaa bbbb\n", "cccc dddd eeee ", "ffff"}, texts(parts))
	// An early newline isn't taken over a late space.
	parts = slack.Split(s.Text("a\nbbbbbbbb cccccccccccc"), 16)
	require.Equal(t, []string{"a\nbbbbbbbb ", "cccccccccccc"}, texts(parts))
	// No space: as late as fits, never inside an entity.
	parts = slack.Split(s.Text(strings.Repeat("a", 14)+"&&"), 16)
	require.Equal(t, []string{strings.Repeat("a", 14), "&amp;&amp;"}, texts(parts))
	// A block cut in two is closed and opened again.
	parts = slack.Split(s.Text("```"+strings.Repeat("x", 30)+"```"), 16)
	require.Equal(t, []string{"```" + strings.Repeat("x", 10) + "```", "```" + strings.Repeat("x", 10) + "```", "```" + strings.Repeat("x", 10) + "```"}, texts(parts))
	// An inline span too, cut late in it rather than early before it.
	parts = slack.Split(s.Text("hi `"+strings.Repeat("y", 20)+"`"), 16)
	require.Equal(t, []string{"hi `" + strings.Repeat("y", 11) + "`", "`" + strings.Repeat("y", 9) + "`"}, texts(parts))
	// Entities in code stay whole.
	parts = slack.Split(s.Text("```"+strings.Repeat("&", 10)+"```"), 16)
	require.Equal(t, slices.Repeat([]string{"```&amp;&amp;```"}, 5), texts(parts))
	// A span of spaces is cut beside a space, the last resort.
	parts = slack.Split(s.Text("`a"+strings.Repeat(" ", 30)+"b`"), 16)
	require.Equal(t, []string{"`a" + strings.Repeat(" ", 13) + "`", "`" + strings.Repeat(" ", 14) + "`", "`   b`"}, texts(parts))
	// A link fits whole, or is a part of its own.
	link := s.Text("https://foundry.helix-db.com/x")
	parts = slack.Split(slack.Join(s.Text("go "), link), 16)
	require.Equal(t, []string{"go ", link.String()}, texts(parts))

	require.Equal(t, "aaaa bbbb\n…", slack.Cut(s.Text("aaaa bbbb\ncccc dddd eeee ffff"), 17).String())
	require.Equal(t, short, slack.Cut(short, 16))
}

func texts(parts []slack.Mrkdwn) []string {
	out := make([]string, len(parts))
	for i, p := range parts {
		out[i] = p.String()
	}
	return out
}

func TestMentionedUsersAndInputText(t *testing.T) {
	raw := "<@U0BOT0001> ask <@U0ADA0001> and <@U0GRACE01|grace> in <#C0GEN0001|general> or <#C0X> " +
		"<!here> <!subteam^S1|@eng> <!date^1392734382^{date}|Feb 18> <https://acme.com|Acme> <https://acme.com> " +
		"<mailto:a@b.co|a@b.co> &lt;b&gt; &amp;amp; <@U0ADA0001>"
	require.Equal(t, []string{"U0BOT0001", "U0ADA0001", "U0GRACE01"}, slack.MentionedUsers(raw))
	names := map[string]string{"U0ADA0001": "Ada Lovelace"}
	got := slack.InputText(raw, "U0BOT0001", func(id string) string { return names[id] })
	require.Equal(t, "ask @Ada Lovelace and @grace in #general or #channel @here @eng Feb 18 Acme (https://acme.com) https://acme.com "+
		"a@b.co (mailto:a@b.co) <b> &amp; @Ada Lovelace", got)
	require.Equal(t, "@U0X1", slack.InputText("<@U0X1>", "", func(string) string { return "" }))
	require.Equal(t, "@channel", slack.InputText("<!channel>", "", func(string) string { return "" }))
	require.Empty(t, slack.InputText("  <@U0BOT0001>  ", "U0BOT0001", func(string) string { return "" }))
	require.Empty(t, slack.MentionedUsers("<@lower> <@> <#C1>"))
}

func ExampleSite_Text() {
	site, _ := slack.NewSite("https://foundry.helix-db.com")
	fmt.Println(site.Text("Renewals <!here>: see https://foundry.helix-db.com/ask/01J or email ops@acme.com"))
	// Output:
	// Renewals &lt;!here&gt;: see <https://foundry.helix-db.com/ask/01J|https://foundry.helix-db.com/ask/01J> or email `ops@acme.com`
}

func ExampleSplit() {
	site, _ := slack.NewSite("https://foundry.helix-db.com")
	for _, part := range slack.Split(site.Text("First paragraph.\nSecond one, longer."), 24) {
		fmt.Printf("%q\n", part)
	}
	// Output:
	// "First paragraph.\n"
	// "Second one, longer."
}

func ExampleInputText() {
	names := map[string]string{"U0ADA0001": "Ada"}
	fmt.Println(slack.InputText("<@U0BOT0001> what did <@U0ADA0001> sell in <#C0GEN0001|sales>? &lt;urgent&gt;",
		"U0BOT0001", func(id string) string { return names[id] }))
	// Output:
	// what did @Ada sell in #sales? <urgent>
}
