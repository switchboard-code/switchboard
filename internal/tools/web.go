package tools

// The web tools: websearch and webfetch. Both reach off the machine from
// this process, which is the external effect — no bounded mode auto-allows
// them, bypass included, because egress can carry the workspace anywhere and a
// sandbox cannot judge whether that was wanted. Yolo alone covers them: the
// everything-grant exempts nothing. The remember key carries
// the host, so one approval covers a host for the session rather than one
// byte-exact URL, and the URL the model composed passes the credential
// scan before anything leaves: a fetch is the classic exfiltration channel,
// and the gate that faces outward for prompts faces outward here too.
//
// The search backend is DuckDuckGo's HTML endpoint, parsed against the
// captured response in testdata/ddg.html rather than against documentation
// none exists for. Result links arrive as redirect URLs with the real
// destination in the uddg query parameter; the parser unwraps them, and a
// direct href is taken as it stands so a format change degrades to worse
// links rather than no results.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/html"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/permission"
)

const (
	webUserAgent = "switchboard (+https://github.com/switchboard-code/switchboard)"

	// webFetchLimit caps what a fetch reads off the wire; webTextLimit caps
	// what reaches the context after conversion. Both exist because a page
	// has no contract about its size and the context is the scarce thing.
	webFetchLimit = 2 << 20
	webTextLimit  = 40_000

	ddgEndpoint = "https://html.duckduckgo.com/html/"
)

var errWebResponseTooLarge = errors.New("response exceeded the 2097152-byte read limit; content withheld")

// readWebBody distinguishes a complete response at the wire cap from a
// prefix. Returning a prefix would let the cap remove the last byte that made
// a credential recognizable and hand the remaining fragment to the provider.
func readWebBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, webFetchLimit+1))
	if err != nil {
		return nil, err
	}
	if len(body) > webFetchLimit {
		return nil, errWebResponseTooLarge
	}
	return body, nil
}

func newWebClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}

// pinnedToHost derives a client that refuses redirects leaving the named
// host. The user approved a host, not wherever that host points: a server
// redirecting off itself would otherwise read from hosts nobody approved —
// an internal service included — under a grant that named someone else.
// The check is by hostname, the same grain as the remember key, and the
// refusal names the destination so the model can fetch it directly and
// route the new host through its own approval.
func pinnedToHost(base *http.Client, host string) *http.Client {
	pinned := *base
	pinned.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if req.URL.Hostname() != host {
			return fmt.Errorf("redirect leaves the approved host %s for %s; fetch the destination directly to approve its host", host, req.URL.Hostname())
		}
		return nil
	}
	return &pinned
}

// ProbeWeb answers whether the search backend is reachable from this
// machine, for doctor: the same endpoint, client posture, and identity a
// real search uses, reading nothing but the status. A server fault and an
// unreachable network are both "the next search will error", which is what
// the caller is really asking.
func ProbeWeb(ctx context.Context) error {
	return probeWeb(ctx, newWebClient(), ddgEndpoint)
}

func probeWeb(ctx context.Context, client *http.Client, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", webUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("the search backend answered %s", resp.Status)
	}
	return nil
}

// scanOutbound refuses to send a key-shaped string off the machine. The
// finding's own rendering is masked, so the refusal cannot leak what it
// held back.
func scanOutbound(what, text string) error {
	if leaks := credential.ScanPrompt(text); len(leaks) > 0 {
		return fmt.Errorf("the %s carries %s; refusing to send it off this machine", what, leaks[0].String())
	}
	return nil
}

// --- websearch ---------------------------------------------------------------

type websearchTool struct {
	client   *http.Client
	endpoint string
}

func (t *websearchTool) Name() string { return "websearch" }

func (t *websearchTool) Description() string {
	return "Search the web. Returns titles, links, and snippets from a general web " +
		"index. Use it for current documentation, an unfamiliar error message, or " +
		"anything the workspace cannot answer; follow a promising result with webfetch. " +
		"The first call of a session asks for approval, which then covers the session."
}

func (t *websearchTool) ParallelSafe() bool { return true }

func (t *websearchTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "The search query, as you would type it."},
    "count": {"type": "integer", "description": "How many results to return, 1 to 10. Defaults to 5."}
  },
  "required": ["query"]
}`)
}

type websearchInput struct {
	Query string `json:"query"`
	Count int    `json:"count"`
}

func (t *websearchTool) Plan(input json.RawMessage) (Plan, error) {
	var in websearchInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Plan{}, fmt.Errorf("websearch: %w", err)
	}
	if strings.TrimSpace(in.Query) == "" {
		return Plan{}, fmt.Errorf("websearch: query is required")
	}
	if err := scanOutbound("query", in.Query); err != nil {
		return Plan{}, fmt.Errorf("websearch: %w", err)
	}
	count := in.Count
	if count <= 0 {
		count = 5
	}
	if count > 10 {
		count = 10
	}
	host := "duckduckgo.com"
	if t.endpoint != ddgEndpoint {
		if u, err := url.Parse(t.endpoint); err == nil {
			host = u.Hostname()
		}
	}
	return Plan{
		Request: permission.Request{
			Tool:   t.Name(),
			Effect: permission.EffectExternal,
			Path:   host,
			Detail: in.Query,
		},
		Run: func(ctx context.Context) (Result, error) {
			return t.search(ctx, in.Query, count)
		},
	}, nil
}

func (t *websearchTool) search(ctx context.Context, query string, count int) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.endpoint+"?q="+url.QueryEscape(query), nil)
	if err != nil {
		return errorf("websearch: %v", err)
	}
	req.Header.Set("User-Agent", webUserAgent)
	resp, err := pinnedToHost(t.client, req.URL.Hostname()).Do(req)
	if err != nil {
		return errorf("websearch: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errorf("websearch: the search backend answered %s", resp.Status)
	}
	body, err := readWebBody(resp.Body)
	if err != nil {
		return errorf("websearch: %v", err)
	}
	results, err := parseDDG(bytes.NewReader(body))
	if err != nil {
		return errorf("websearch: %v", err)
	}
	if len(results) == 0 {
		return Result{Content: "no results"}, nil
	}
	if len(results) > count {
		results = results[:count]
	}
	var b strings.Builder
	for i, r := range results {
		fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, r.title, r.url)
		if r.snippet != "" {
			fmt.Fprintf(&b, "   %s\n", r.snippet)
		}
	}
	return Result{Content: strings.TrimRight(b.String(), "\n")}, nil
}

type searchResult struct {
	title   string
	url     string
	snippet string
}

// parseDDG walks the endpoint's HTML for result anchors and snippets. The
// shapes it matches are the ones in the captured response: an <a> classed
// result__a carrying the redirect link, and a result__snippet beside it.
func parseDDG(r io.Reader) ([]searchResult, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return nil, err
	}
	var results []searchResult
	var current *searchResult
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			class := attrOf(n, "class")
			switch {
			case n.Data == "a" && strings.Contains(class, "result__a"):
				if current != nil {
					results = append(results, *current)
				}
				current = &searchResult{
					title: strings.TrimSpace(textOf(n)),
					url:   unwrapDDG(attrOf(n, "href")),
				}
			case strings.Contains(class, "result__snippet") && current != nil && current.snippet == "":
				current.snippet = strings.TrimSpace(textOf(n))
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	if current != nil {
		results = append(results, *current)
	}
	kept := results[:0]
	for _, r := range results {
		if r.title != "" && r.url != "" {
			kept = append(kept, r)
		}
	}
	return kept, nil
}

// unwrapDDG resolves a result link to its destination: the endpoint hands
// out //duckduckgo.com/l/?uddg=<escaped url> redirects. Anything else is
// returned as it stands.
func unwrapDDG(href string) string {
	if href == "" {
		return ""
	}
	u, err := url.Parse(href)
	if err != nil {
		return href
	}
	if dest := u.Query().Get("uddg"); dest != "" {
		return dest
	}
	if strings.HasPrefix(href, "//") {
		return "https:" + href
	}
	return href
}

func attrOf(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

func textOf(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// --- webfetch ----------------------------------------------------------------

type webfetchTool struct {
	client *http.Client
}

func (t *webfetchTool) Name() string { return "webfetch" }

func (t *webfetchTool) Description() string {
	return "Fetch a page from the web by URL. HTML is reduced to its readable text; " +
		"JSON and plain text return as they are, truncated past a limit. The first " +
		"fetch to a new host asks for approval, and the approval covers that host " +
		"for the rest of the session. A redirect that leaves the approved host is " +
		"refused; fetch the destination directly to approve its host."
}

func (t *webfetchTool) ParallelSafe() bool { return true }

func (t *webfetchTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "url": {"type": "string", "description": "The http or https URL to fetch."}
  },
  "required": ["url"]
}`)
}

type webfetchInput struct {
	URL string `json:"url"`
}

func (t *webfetchTool) Plan(input json.RawMessage) (Plan, error) {
	var in webfetchInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Plan{}, fmt.Errorf("webfetch: %w", err)
	}
	u, err := url.Parse(strings.TrimSpace(in.URL))
	if err != nil {
		return Plan{}, fmt.Errorf("webfetch: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Plan{}, fmt.Errorf("webfetch: only http and https URLs are fetched, not %q", u.Scheme)
	}
	if u.Hostname() == "" {
		return Plan{}, fmt.Errorf("webfetch: the URL names no host")
	}
	if err := scanOutbound("url", in.URL); err != nil {
		return Plan{}, fmt.Errorf("webfetch: %w", err)
	}
	return Plan{
		Request: permission.Request{
			Tool:   t.Name(),
			Effect: permission.EffectExternal,
			Path:   u.Hostname(),
			Detail: u.String(),
		},
		Run: func(ctx context.Context) (Result, error) {
			return t.fetch(ctx, u)
		},
	}, nil
}

func (t *webfetchTool) fetch(ctx context.Context, u *url.URL) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return errorf("webfetch: %v", err)
	}
	req.Header.Set("User-Agent", webUserAgent)
	resp, err := pinnedToHost(t.client, u.Hostname()).Do(req)
	if err != nil {
		return errorf("webfetch: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return errorf("webfetch: %s answered %s", u.Hostname(), resp.Status)
	}

	ctype := resp.Header.Get("Content-Type")
	mediatype := strings.TrimSpace(strings.SplitN(ctype, ";", 2)[0])
	body, err := readWebBody(resp.Body)
	if err != nil {
		return errorf("webfetch: reading %s: %v", u.Hostname(), err)
	}

	var text string
	switch {
	case mediatype == "text/html" || mediatype == "application/xhtml+xml":
		text, err = htmlToText(strings.NewReader(string(body)))
		if err != nil {
			return errorf("webfetch: parsing %s: %v", u.Hostname(), err)
		}
	case strings.HasPrefix(mediatype, "text/"),
		mediatype == "application/json",
		mediatype == "application/xml",
		strings.HasSuffix(mediatype, "+json"),
		strings.HasSuffix(mediatype, "+xml"):
		text = string(body)
	default:
		return errorf("webfetch: %s is %s, which has no text to hand back", u.Hostname(), mediatype)
	}

	var notes []string
	// Same-host redirects are the server's own routing and were followed —
	// the pinned client already refused anything that left the host — and a
	// moved page is still worth saying.
	if final := resp.Request.URL; final.String() != u.String() {
		notes = append(notes, fmt.Sprintf("[fetched %s after redirect]", final))
	}
	// Scan the complete extracted component before the context cap. A cap can
	// otherwise remove the final byte that made a credential recognizable and
	// return the rest as apparently safe provider context.
	text = credential.Redact(text, credential.ScanPrompt(text))
	if len(text) > webTextLimit {
		text = truncateValidUTF8Bytes(text, webTextLimit)
		notes = append(notes, fmt.Sprintf("[truncated at %d characters]", webTextLimit))
	}
	if len(notes) > 0 {
		text = strings.Join(notes, " ") + "\n" + text
	}
	return Result{Content: text}, nil
}

// htmlToText reduces a page to what a reader would read: block elements
// break lines, scripts and styles vanish, and everything else is its text.
func htmlToText(r io.Reader) (string, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return "", err
	}
	blocks := map[string]bool{
		"p": true, "div": true, "br": true, "li": true, "tr": true,
		"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
		"article": true, "section": true, "header": true, "footer": true,
		"blockquote": true, "pre": true, "table": true, "ul": true, "ol": true,
	}
	skip := map[string]bool{
		"script": true, "style": true, "noscript": true, "template": true,
		"svg": true, "iframe": true,
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && skip[n.Data] {
			return
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		if n.Type == html.ElementNode && blocks[n.Data] {
			b.WriteString("\n")
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && blocks[n.Data] {
			b.WriteString("\n")
		}
	}
	walk(doc)

	// Collapse the whitespace HTML never meant: runs of spaces inside a
	// line, and runs of blank lines between blocks.
	var lines []string
	blank := true
	for _, line := range strings.Split(b.String(), "\n") {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			if !blank {
				lines = append(lines, "")
			}
			blank = true
			continue
		}
		blank = false
		lines = append(lines, line)
	}
	return strings.TrimSpace(strings.Join(lines, "\n")), nil
}
