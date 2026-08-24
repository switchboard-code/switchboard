package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/permission"
)

// The search parser is written against a captured response, not the
// endpoint's documentation, because none exists. testdata/ddg.html is that
// capture: ten results, each link a redirect carrying the destination in
// its uddg parameter.
func TestWebsearchParsesTheCapturedResponse(t *testing.T) {
	fixture, err := os.ReadFile("testdata/ddg.html")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("q"); got != "go process group reaping" {
			t.Errorf("query arrived as %q", got)
		}
		w.Write(fixture)
	}))
	defer srv.Close()

	tool := &websearchTool{client: srv.Client(), endpoint: srv.URL + "/html/"}
	plan, err := tool.Plan(json.RawMessage(`{"query": "go process group reaping", "count": 3}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := plan.Run(context.Background())
	if err != nil || res.IsError {
		t.Fatalf("search failed: %v %s", err, res.Content)
	}
	if !strings.Contains(res.Content, "1. ") || !strings.Contains(res.Content, "3. ") {
		t.Fatalf("expected three numbered results:\n%s", res.Content)
	}
	if strings.Contains(res.Content, "4. ") {
		t.Fatalf("count=3 returned a fourth result:\n%s", res.Content)
	}
	// The redirect link is unwrapped to its destination.
	if !strings.Contains(res.Content, "https://github.com/hashicorp/go-reap") {
		t.Fatalf("the uddg redirect was not unwrapped:\n%s", res.Content)
	}
	if strings.Contains(res.Content, "duckduckgo.com/l/") {
		t.Fatalf("a raw redirect link leaked into the results:\n%s", res.Content)
	}
}

// Egress is the external effect: no bounded mode auto-allows it, and the
// remember key carries the host so one approval covers a host, not one URL.
func TestWebToolsCarryTheExternalEffectAndTheHost(t *testing.T) {
	search := &websearchTool{client: newWebClient(), endpoint: ddgEndpoint}
	plan, err := search.Plan(json.RawMessage(`{"query": "anything"}`))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Request.Effect != permission.EffectExternal {
		t.Errorf("websearch effect = %s, want external", plan.Request.Effect)
	}
	if plan.Request.Path != "duckduckgo.com" {
		t.Errorf("websearch path = %q, want the backend host", plan.Request.Path)
	}

	fetch := &webfetchTool{client: newWebClient()}
	plan, err = fetch.Plan(json.RawMessage(`{"url": "https://pkg.go.dev/net/http#Client"}`))
	if err != nil {
		t.Fatal(err)
	}
	if plan.Request.Effect != permission.EffectExternal {
		t.Errorf("webfetch effect = %s, want external", plan.Request.Effect)
	}
	if plan.Request.Path != "pkg.go.dev" {
		t.Errorf("webfetch path = %q, want the host", plan.Request.Path)
	}
	if plan.Request.Detail == "" {
		t.Error("webfetch shows no URL at the prompt")
	}
}

func TestWebfetchRefusesNonHTTPSchemes(t *testing.T) {
	fetch := &webfetchTool{client: newWebClient()}
	for _, bad := range []string{
		`{"url": "file:///etc/passwd"}`,
		`{"url": "ftp://example.com/x"}`,
		`{"url": "not a url at all://"}`,
		`{"url": "/relative/path"}`,
	} {
		if _, err := fetch.Plan(json.RawMessage(bad)); err == nil {
			t.Errorf("Plan(%s) validated", bad)
		}
	}
}

// A key-shaped string in an outbound URL or query is refused before
// anything leaves, and the refusal itself is masked: the test that greps
// the error for the token is the guarantee.
func TestWebToolsRefuseToSendAKeyShapedString(t *testing.T) {
	token := "sk-ant-api03-" + strings.Repeat("a", 80)
	fetch := &webfetchTool{client: newWebClient()}
	_, err := fetch.Plan(json.RawMessage(fmt.Sprintf(`{"url": "https://example.com/?k=%s"}`, token)))
	if err == nil {
		t.Fatal("a key-shaped URL validated")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("the refusal carries the token it refused: %v", err)
	}

	search := &websearchTool{client: newWebClient(), endpoint: ddgEndpoint}
	_, err = search.Plan(json.RawMessage(fmt.Sprintf(`{"query": "what is %s"}`, token)))
	if err == nil {
		t.Fatal("a key-shaped query validated")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("the refusal carries the token it refused: %v", err)
	}
}

func TestWebfetchReducesHTMLToItsText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<html><head><title>The Page</title><style>body{color:red}</style></head>
<body><script>alert("nope")</script><h1>A Heading</h1>
<p>First paragraph   with    spread    spacing.</p>
<ul><li>one</li><li>two</li></ul></body></html>`)
	}))
	defer srv.Close()

	tool := &webfetchTool{client: srv.Client()}
	plan, err := tool.Plan(json.RawMessage(fmt.Sprintf(`{"url": %q}`, srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	res, err := plan.Run(context.Background())
	if err != nil || res.IsError {
		t.Fatalf("fetch failed: %v %s", err, res.Content)
	}
	for _, want := range []string{"A Heading", "First paragraph with spread spacing.", "one", "two"} {
		if !strings.Contains(res.Content, want) {
			t.Errorf("text lost %q:\n%s", want, res.Content)
		}
	}
	for _, gone := range []string{"alert", "color:red", "<h1>"} {
		if strings.Contains(res.Content, gone) {
			t.Errorf("markup or code leaked into the text (%q):\n%s", gone, res.Content)
		}
	}
}

func TestWebfetchRedactsCompleteTextBeforeContextCap(t *testing.T) {
	body := strings.Repeat("x", webTextLimit-len(truncationBoundaryToken)+1) + truncationBoundaryToken
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()

	tool := &webfetchTool{client: srv.Client()}
	plan, err := tool.Plan(json.RawMessage(fmt.Sprintf(`{"url":%q}`, srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	res, err := plan.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Content, truncationBoundaryToken) || strings.Contains(res.Content, "ghp_") {
		t.Fatalf("web text cap exposed a credential fragment: %q", res.Content)
	}
	if !strings.Contains(res.Content, "[redacted: a GitHub token]") {
		t.Fatalf("web text was not redacted before its cap: %q", res.Content)
	}
}

func TestWebToolsWithholdResponsesThatCrossTheWireCap(t *testing.T) {
	token := truncationBoundaryToken
	prefix := strings.Repeat("x", webFetchLimit-len(token)+1)
	body := prefix + token

	for _, test := range []struct {
		name        string
		contentType string
		endpoint    string
		plan        func(*http.Client, string) (Plan, error)
	}{
		{
			name:        "fetch",
			contentType: "text/plain",
			plan: func(client *http.Client, endpoint string) (Plan, error) {
				return (&webfetchTool{client: client}).Plan(json.RawMessage(fmt.Sprintf(`{"url":%q}`, endpoint)))
			},
		},
		{
			name:        "search",
			contentType: "text/html",
			endpoint:    "/html/",
			plan: func(client *http.Client, endpoint string) (Plan, error) {
				return (&websearchTool{client: client, endpoint: endpoint}).Plan(json.RawMessage(`{"query":"bounded"}`))
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				_, _ = io.WriteString(w, body)
			}))
			defer srv.Close()

			endpoint := srv.URL + test.endpoint
			plan, err := test.plan(srv.Client(), endpoint)
			if err != nil {
				t.Fatal(err)
			}
			result, err := plan.Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !result.IsError || !strings.Contains(result.Content, "content withheld") {
				t.Fatalf("oversized response was not withheld: %#v", result)
			}
			if strings.Contains(result.Content, token) || strings.Contains(result.Content, "ghp_") {
				t.Fatalf("wire cap exposed a credential fragment: %q", result.Content)
			}
		})
	}
}

func TestWebfetchHandsBackPlainTypesAndRefusesTheRest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/data.json":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"ok": true}`)
		case "/image.png":
			w.Header().Set("Content-Type", "image/png")
			w.Write([]byte{0x89, 'P', 'N', 'G'})
		}
	}))
	defer srv.Close()

	tool := &webfetchTool{client: srv.Client()}
	plan, _ := tool.Plan(json.RawMessage(fmt.Sprintf(`{"url": "%s/data.json"}`, srv.URL)))
	res, err := plan.Run(context.Background())
	if err != nil || res.IsError || res.Content != `{"ok": true}` {
		t.Fatalf("json fetch: %v %q", err, res.Content)
	}

	plan, _ = tool.Plan(json.RawMessage(fmt.Sprintf(`{"url": "%s/image.png"}`, srv.URL)))
	res, err = plan.Run(context.Background())
	if err != nil || !res.IsError {
		t.Fatalf("a png should refuse as a tool error: %v %q", err, res.Content)
	}
	if !strings.Contains(res.Content, "image/png") {
		t.Fatalf("the refusal does not name the type: %q", res.Content)
	}
}

func TestWebfetchTruncatesAndSaysSo(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(strings.Repeat("x", webTextLimit+1000)))
	}))
	defer srv.Close()

	tool := &webfetchTool{client: srv.Client()}
	plan, _ := tool.Plan(json.RawMessage(fmt.Sprintf(`{"url": %q}`, srv.URL)))
	res, err := plan.Run(context.Background())
	if err != nil || res.IsError {
		t.Fatalf("fetch failed: %v", err)
	}
	if len(res.Content) > webTextLimit+200 {
		t.Fatalf("content is %d chars, cap is %d", len(res.Content), webTextLimit)
	}
	if !strings.Contains(res.Content, "[truncated") {
		t.Fatal("a truncated fetch did not say so")
	}
}

// The approval covers a hostname, so redirects are held to the same grain:
// one that stays on the host is the server's own routing and follows; one
// that leaves it is refused before anything is dialed, because a grant
// naming host X must not read from host Y — an internal service included —
// on X's say-so.
func TestWebfetchRefusesARedirectOffTheApprovedHost(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "landed")
	}))
	defer final.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/same":
			// httptest servers share 127.0.0.1, so this is a same-hostname
			// redirect — the grain the remember key uses — and must follow.
			http.Redirect(w, r, final.URL, http.StatusFound)
		case "/away":
			http.Redirect(w, r, "http://sb-unapproved-host.invalid/x", http.StatusFound)
		}
	}))
	defer first.Close()

	tool := &webfetchTool{client: &http.Client{}}
	plan, _ := tool.Plan(json.RawMessage(fmt.Sprintf(`{"url": "%s/same"}`, first.URL)))
	res, err := plan.Run(context.Background())
	if err != nil || res.IsError {
		t.Fatalf("a same-host redirect failed the fetch: %v %s", err, res.Content)
	}
	if !strings.Contains(res.Content, "landed") {
		t.Fatalf("the same-host redirect was not followed: %q", res.Content)
	}

	plan, _ = tool.Plan(json.RawMessage(fmt.Sprintf(`{"url": "%s/away"}`, first.URL)))
	res, err = plan.Run(context.Background())
	if err != nil || !res.IsError {
		t.Fatalf("a cross-host redirect should refuse as a tool error: %v %q", err, res.Content)
	}
	if !strings.Contains(res.Content, "sb-unapproved-host.invalid") {
		t.Fatalf("the refusal does not name the destination: %q", res.Content)
	}
	if !strings.Contains(res.Content, "fetch the destination directly") {
		t.Fatalf("the refusal does not say the way through: %q", res.Content)
	}
}

// The live path, guarded the way every network test is.
func TestWebsearchLive(t *testing.T) {
	if os.Getenv("SB_LIVE") == "" {
		t.Skip("SB_LIVE not set")
	}
	tool := &websearchTool{client: newWebClient(), endpoint: ddgEndpoint}
	plan, err := tool.Plan(json.RawMessage(`{"query": "golang bubbletea textarea", "count": 3}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := plan.Run(context.Background())
	if err != nil || res.IsError {
		t.Fatalf("live search failed: %v %s", err, res.Content)
	}
	if !strings.Contains(res.Content, "1. ") {
		t.Fatalf("no results:\n%s", res.Content)
	}
}

// Fetch has its own live path, and it is a different one: search talks to a
// single known backend, while fetch talks to whatever the model names and has
// to survive a real redirect, a real content type, and a real page.
func TestWebfetchLive(t *testing.T) {
	if os.Getenv("SB_LIVE") == "" {
		t.Skip("SB_LIVE not set")
	}
	tool := &webfetchTool{client: newWebClient()}
	plan, err := tool.Plan(json.RawMessage(`{"url": "https://example.com"}`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := plan.Run(context.Background())
	if err != nil || res.IsError {
		t.Fatalf("live fetch failed: %v %s", err, res.Content)
	}
	if !strings.Contains(strings.ToLower(res.Content), "example domain") {
		t.Fatalf("the page came back without its content:\n%s", res.Content)
	}
}

// The doctor probe asks the one question doctor needs answered: will the
// next search error. A healthy backend and a client-side status both pass;
// a server fault and a dead network both fail with the reason.
func TestProbeWebReportsReachability(t *testing.T) {
	healthy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != webUserAgent {
			t.Errorf("the probe did not identify itself: %q", r.Header.Get("User-Agent"))
		}
	}))
	defer healthy.Close()
	if err := probeWeb(context.Background(), healthy.Client(), healthy.URL); err != nil {
		t.Fatalf("a healthy backend failed the probe: %v", err)
	}

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer broken.Close()
	if err := probeWeb(context.Background(), broken.Client(), broken.URL); err == nil {
		t.Fatal("a 502 backend passed the probe")
	}

	dead := httptest.NewServer(nil)
	deadURL := dead.URL
	dead.Close()
	if err := probeWeb(context.Background(), &http.Client{}, deadURL); err == nil {
		t.Fatal("a dead network passed the probe")
	}
}
