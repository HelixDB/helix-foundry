package companyresearch

import (
	"bytes"
	"context"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/net/html/charset"
	"golang.org/x/net/publicsuffix"

	"github.com/helixdb/helix-foundry/backend/connector"
)

// The bounds of a site's read.
const (
	// PageTimeout bounds one page, its redirects included; Timeout the
	// whole read.
	PageTimeout = 5 * time.Second
	Timeout     = 6 * time.Second
	// MaxPageBytes is the most of a page's body read.
	MaxPageBytes = 512 << 10
	// MaxPageChars and MaxSiteChars bound the text kept of a page and of
	// the whole site.
	MaxPageChars = 6000
	MaxSiteChars = 16000
	// MaxLinked is how many pages linked from the home page are read.
	MaxLinked = 3
	// minRoom is the least of MaxSiteChars a page is kept in.
	minRoom = 100
	// maxRedirects is how many redirects one page follows.
	maxRedirects = 3
	// UserAgent is what Foundry says it is.
	UserAgent = "HelixFoundry/1.0 (+https://foundry.helix-db.com)"
)

// aboutLink is a link whose path or text says it is about the company.
var aboutLink = regexp.MustCompile(`(?i)about|company|product|platform|solutions|pricing|customers`)

// pageTypes are the media types a page is read as, true for HTML.
var pageTypes = map[string]bool{"text/html": true, "application/xhtml+xml": true, "text/plain": false, "text/markdown": false}

// Page is what was read of one page: its address after redirects, and its
// text, at most MaxPageChars, one block (a title, a heading, a paragraph,
// an item) a line.
type Page struct {
	URL  string
	Text string
}

// Site is what was read of a domain's website: its home page first, then
// the pages it links about the company; none when nothing could be read.
type Site struct{ Pages []Page }

// Sources are the pages' addresses.
func (s Site) Sources() []string {
	out := []string{}
	for _, p := range s.Pages {
		out = append(out, p.URL)
	}
	return out
}

// Fetcher reads a domain's website over https: every connection through
// its transport, never a proxy or an automatic redirect.
type Fetcher struct {
	// Transport makes each request; connector.Guard's refuses every
	// non-public address.
	Transport http.RoundTripper
	// PageTimeout and Timeout bound a page and the whole read.
	PageTimeout, Timeout time.Duration
}

// NewFetcher reads through guard's transport, within PageTimeout a page and
// Timeout in all. Foundry reads a company's public website only, so guard
// never allows private addresses, locally too.
func NewFetcher() *Fetcher {
	return &Fetcher{Transport: connector.Guard{}.Transport(), PageTimeout: PageTimeout, Timeout: Timeout}
}

// Fetch reads domain's site: https://<domain>/, else https://www.<domain>/,
// then up to MaxLinked pages of the same registrable domain it links whose
// path or text says they are about the company (aboutLink), at once. A page
// is read only over https, within maxRedirects redirects that stay on https
// and on domain, each with a Location, as one of pageTypes, at most
// MaxPageBytes, decoded from the charset it names (else UTF-8 when it is,
// else Windows-1252: charset.DetermineEncoding); its text is cut to
// MaxPageChars, and the pages' together to MaxSiteChars, a page kept only
// while minRoom of it is left. A page that can't be read is left out: Fetch
// never fails, and a site nothing of which could be read has no pages.
func (f *Fetcher) Fetch(ctx context.Context, domain string) Site {
	ctx, cancel := context.WithTimeout(ctx, f.Timeout)
	defer cancel()
	client := &http.Client{Transport: f.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var home *read
	for _, start := range []string{"https://" + domain + "/", "https://www." + domain + "/"} {
		if home = f.page(ctx, client, domain, start); home != nil {
			break
		}
	}
	if home == nil {
		return Site{Pages: []Page{}}
	}
	links := home.links(domain)
	reads := make([]*read, len(links))
	var wg sync.WaitGroup
	for i, link := range links {
		wg.Go(func() { reads[i] = f.page(ctx, client, domain, link) })
	}
	wg.Wait()
	site, left := Site{}, MaxSiteChars
	for _, r := range append([]*read{home}, reads...) {
		if r == nil || left < minRoom {
			continue
		}
		text := cut(r.text, min(MaxPageChars, left))
		left -= utf8.RuneCountInString(text)
		site.Pages = append(site.Pages, Page{URL: r.url.String(), Text: text})
	}
	return site
}

// read is a page read: its address, text and, for HTML, its links.
type read struct {
	url  *url.URL
	text string
	doc  *html.Node
}

// page reads one page within PageTimeout, following redirects by hand; nil
// when it can't be read or holds no text.
func (f *Fetcher) page(ctx context.Context, client *http.Client, domain, address string) *read {
	ctx, cancel := context.WithTimeout(ctx, f.PageTimeout)
	defer cancel()
	for range maxRedirects + 1 {
		u, err := url.Parse(address)
		if err != nil || !onDomain(u, domain) {
			return nil
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil
		}
		req.Header.Set("User-Agent", UserAgent)
		req.Header.Set("Accept", "text/html, application/xhtml+xml, text/plain;q=0.9, text/markdown;q=0.9")
		resp, err := client.Do(req)
		if err != nil {
			return nil
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			location := resp.Header.Get("Location")
			resp.Body.Close()
			// Without a Location the redirect would name the page itself.
			if location == "" {
				return nil
			}
			next, err := u.Parse(location)
			if err != nil {
				return nil
			}
			address = next.String()
			continue
		}
		contentType := resp.Header.Get("Content-Type")
		mediaType, _, _ := mime.ParseMediaType(contentType)
		isHTML, readable := pageTypes[mediaType]
		var body []byte
		if resp.StatusCode == http.StatusOK && readable {
			// A body cut short by the page's time is read as far as it came.
			body, _ = io.ReadAll(io.LimitReader(resp.Body, MaxPageBytes))
		}
		resp.Body.Close()
		return readOf(u, isHTML, contentType, body)
	}
	return nil
}

// readOf is a page's read from its body, decoded from the charset
// contentType or the page names; nil when it holds no text.
func readOf(u *url.URL, isHTML bool, contentType string, raw []byte) *read {
	if len(raw) == 0 {
		return nil
	}
	decoder, err := charset.NewReader(bytes.NewReader(raw), contentType)
	if err != nil {
		return nil
	}
	body, err := io.ReadAll(decoder)
	if err != nil {
		return nil
	}
	u.Fragment = ""
	r := &read{url: u}
	if isHTML {
		doc, err := html.Parse(bytes.NewReader(body))
		if err != nil {
			return nil
		}
		r.doc, r.text = doc, pageText(doc)
	} else {
		r.text = plainText(string(body))
	}
	if r.text == "" {
		return nil
	}
	return r
}

// onDomain reports whether u is an https address of domain's registrable
// domain, on the default port, with no user.
func onDomain(u *url.URL, domain string) bool {
	if u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	registrable, err := publicsuffix.EffectiveTLDPlusOne(strings.TrimSuffix(strings.ToLower(u.Hostname()), "."))
	return err == nil && registrable == domain
}

// links are the page's first MaxLinked links about the company, on domain,
// each once, never the page itself.
func (r *read) links(domain string) []string {
	if r.doc == nil {
		return nil
	}
	seen := map[string]bool{strings.TrimSuffix(r.url.String(), "/"): true}
	out := []string{}
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if len(out) == MaxLinked {
			return
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.A {
			href := attr(n, "href")
			u, err := r.url.Parse(href)
			if err == nil && onDomain(u, domain) && (aboutLink.MatchString(u.Path) || aboutLink.MatchString(textOf(n))) {
				u.Fragment, u.RawQuery = "", ""
				if key := strings.TrimSuffix(u.String(), "/"); !seen[key] {
					seen[key] = true
					out = append(out, u.String())
				}
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(r.doc)
	return out
}

// skipped are the elements whose text is never read: code, drawings,
// navigation and footers.
var skipped = map[atom.Atom]bool{
	atom.Script: true, atom.Style: true, atom.Svg: true, atom.Nav: true, atom.Footer: true, atom.Noscript: true, atom.Template: true,
	atom.Iframe: true, atom.Form: true,
}

// blocks are the elements read as one line each.
var blocks = map[atom.Atom]bool{
	atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true, atom.P: true, atom.Li: true,
	atom.Blockquote: true, atom.Dt: true, atom.Dd: true,
}

// pageText is an HTML page's text: its title, its description (meta, then
// Open Graph's), its site's name, then each block's text, one a line, each
// line once.
func pageText(doc *html.Node) string {
	var head, body []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch {
			case skipped[n.DataAtom]:
				return
			case n.DataAtom == atom.Title:
				head = append([]string{textOf(n)}, head...)
				return
			case n.DataAtom == atom.Meta:
				name := strings.ToLower(attr(n, "name") + attr(n, "property"))
				if name == "description" || name == "og:description" || name == "og:site_name" || name == "og:title" {
					head = append(head, attr(n, "content"))
				}
				return
			case blocks[n.DataAtom]:
				body = append(body, textOf(n))
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return joinLines(append(head, body...))
}

// plainText is a text page's non-empty lines.
func plainText(s string) string { return joinLines(strings.Split(s, "\n")) }

// joinLines joins lines, each collapsed to one line, leaving out empty
// ones and repeats.
func joinLines(lines []string) string {
	seen := map[string]bool{}
	out := []string{}
	for _, l := range lines {
		if l = collapse(l); l != "" && !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// textOf is n's text, its skipped elements left out.
func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch {
		case n.Type == html.TextNode:
			b.WriteString(n.Data)
			b.WriteByte(' ')
		case n.Type == html.ElementNode && skipped[n.DataAtom]:
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return collapse(b.String())
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return a.Val
		}
	}
	return ""
}
