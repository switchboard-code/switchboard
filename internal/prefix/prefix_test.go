package prefix

import (
	"bytes"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func sys(text string) []provider.Block { return []provider.Block{provider.Text{Text: text}} }

func tool(name string) provider.ToolDefinition {
	return provider.ToolDefinition{Name: name, Schema: json.RawMessage(`{"type":"object"}`)}
}

func doc(path, content string) Document {
	return Document{Path: path, Hash: "sha-" + path, Content: content}
}

// Two sessions that registered the same tools in a different order would
// otherwise build different prefixes and share no cache, for a reason no user
// could see or fix.
func TestToolOrderDoesNotChangeThePrefix(t *testing.T) {
	a := New(sys("you are terse"), []provider.ToolDefinition{tool("write"), tool("read"), tool("exec")}, 0)
	b := New(sys("you are terse"), []provider.ToolDefinition{tool("read"), tool("exec"), tool("write")}, 0)

	if a.PrefixHash() != b.PrefixHash() {
		t.Error("tool registration order changed the prefix hash")
	}

	names := []string{}
	for _, tl := range a.Request().Tools {
		names = append(names, tl.Name)
	}
	if strings.Join(names, ",") != "exec,read,write" {
		t.Errorf("tools = %v, want deterministic name order", names)
	}
}

// The rule §6.1 exists to enforce: nothing above the tail is rewritten once the
// session is under way. Inserting into the stable zone mid-session shifts every
// block after it and invalidates the cached prefix from that point.
func TestStableZoneSealsOnceHistoryStarts(t *testing.T) {
	l := New(sys("s"), nil, 0)

	if err := l.Add(doc("main.go", "package main")); err != nil {
		t.Fatalf("adding before the session starts must work: %v", err)
	}

	l.AppendHistory(provider.UserText("what does main.go do?"))

	err := l.Add(doc("other.go", "package other"))
	if !errors.Is(err, ErrSealed) {
		t.Fatalf("err = %v, want ErrSealed: a mid-session insert would move every block after it", err)
	}
	if !strings.Contains(err.Error(), "history zone") {
		t.Errorf("err = %v; the caller needs to be told where the content should go instead", err)
	}
}

// Appending to history must not disturb what came before it, which is the
// property that lets a provider keep serving the prefix from cache.
func TestAppendingHistoryLeavesThePrefixBelowItIntact(t *testing.T) {
	l := New(sys("s"), []provider.ToolDefinition{tool("read")}, 0)
	if err := l.Add(doc("main.go", "package main")); err != nil {
		t.Fatal(err)
	}

	before := l.Request()
	l.AppendHistory(provider.UserText("first question"))
	after := l.Request()

	// Everything the first request contained is still a prefix of the second.
	if len(after.Messages) <= len(before.Messages) {
		t.Fatal("appending history did not grow the message list")
	}
	for i := range before.Messages {
		if hashOf(before.Messages[i]) != hashOf(after.Messages[i]) {
			t.Errorf("message %d changed when history was appended, so the cached prefix stops matching here", i)
		}
	}
}

func hashOf(m provider.Message) string {
	l := &Layout{history: []provider.Message{m}}
	return l.PrefixHash()
}

// The tail is the one zone that may be rewritten, which is where dynamic
// operator context belongs. Rewriting it must not disturb the prefix hash, or
// every turn would look like a new prefix.
func TestTailRewriteDoesNotChangeThePrefixHash(t *testing.T) {
	l := New(sys("s"), nil, 0)
	l.AppendHistory(provider.UserText("earlier turn"))

	l.SetTail(provider.Text{Text: "mode: default, budget remaining: $4.10"})
	first := l.PrefixHash()

	l.SetTail(provider.Text{Text: "mode: plan, budget remaining: $3.02"})
	second := l.PrefixHash()

	if first != second {
		t.Error("rewriting the tail changed the prefix hash, so no turn would ever hit cache")
	}
	// It does have to reach the request, or the model never sees it.
	req := l.Request()
	last := req.Messages[len(req.Messages)-1]
	if got := last.Content[0].(provider.Text).Text; !strings.Contains(got, "plan") {
		t.Errorf("the tail did not reach the request: %q", got)
	}
}

// A prefix that hashes the same is one the provider could still be holding; one
// that hashes differently cannot be. Anything above the tail that changes has
// to move the hash, or the tracker will believe in a cache that is gone.
func TestEveryZoneAboveTheTailMovesTheHash(t *testing.T) {
	base := func() *Layout {
		l := New(sys("s"), []provider.ToolDefinition{tool("read")}, 0)
		l.Add(doc("main.go", "package main"))
		l.AppendHistory(provider.UserText("q"))
		return l
	}
	original := base().PrefixHash()

	changes := map[string]func() *Layout{
		"system prompt": func() *Layout {
			l := New(sys("different"), []provider.ToolDefinition{tool("read")}, 0)
			l.Add(doc("main.go", "package main"))
			l.AppendHistory(provider.UserText("q"))
			return l
		},
		"tool set": func() *Layout {
			l := New(sys("s"), []provider.ToolDefinition{tool("read"), tool("write")}, 0)
			l.Add(doc("main.go", "package main"))
			l.AppendHistory(provider.UserText("q"))
			return l
		},
		"document content": func() *Layout {
			l := New(sys("s"), []provider.ToolDefinition{tool("read")}, 0)
			l.Add(Document{Path: "main.go", Hash: "different-hash", Content: "package other"})
			l.AppendHistory(provider.UserText("q"))
			return l
		},
		"history": func() *Layout {
			l := base()
			l.AppendHistory(provider.UserText("another turn"))
			return l
		},
	}
	for name, build := range changes {
		if build().PrefixHash() == original {
			t.Errorf("changing the %s did not move the prefix hash", name)
		}
	}
}

// A thinking block replayed without its signature is a different request, and
// the provider rejects it. It has to count as a different prefix.
func TestThinkingSignatureIsPartOfThePrefix(t *testing.T) {
	signed := &Layout{history: []provider.Message{{
		Role:    provider.RoleAssistant,
		Content: []provider.Block{provider.Thinking{Text: "reasoning", Signature: "sig-abc"}},
	}}}
	unsigned := &Layout{history: []provider.Message{{
		Role:    provider.RoleAssistant,
		Content: []provider.Block{provider.Thinking{Text: "reasoning"}},
	}}}

	if signed.PrefixHash() == unsigned.PrefixHash() {
		t.Error("dropping a signature left the prefix hash unchanged, so the tracker would expect a hit on a request the server refuses")
	}
}

// Re-adding an unchanged document must not move it. Moving it would shift
// everything after, which is the exact defect the zone exists to avoid.
func TestReAddingAnUnchangedDocumentIsANoOp(t *testing.T) {
	l := New(sys("s"), nil, 0)
	l.Add(doc("a.go", "aaa"))
	l.Add(doc("b.go", "bbb"))
	before := l.PrefixHash()

	if err := l.Add(doc("a.go", "aaa")); err != nil {
		t.Fatal(err)
	}
	if l.PrefixHash() != before {
		t.Error("re-adding an unchanged document moved the prefix")
	}
	if got := l.Documents(); len(got) != 2 {
		t.Errorf("the document was duplicated: %d entries", len(got))
	}
}

// Crossing the budget schedules a rebuild rather than truncating. Dropping a
// document mid-session both breaks the cache and leaves the model referring to
// content that is no longer there.
func TestBudgetSchedulesARebuildRatherThanTruncating(t *testing.T) {
	l := New(sys("s"), nil, 10) // ten tokens, so roughly forty characters
	l.Add(doc("big.go", strings.Repeat("x", 400)))

	if !l.NeedsRebuild() {
		t.Fatal("an oversized stable zone did not ask for a rebuild")
	}
	if len(l.Documents()) != 1 {
		t.Error("the document was dropped without a rebuild being asked for")
	}
}

func TestRebuildEvictsLeastRecentlyReferenced(t *testing.T) {
	// Room for the pinned file plus one more, so exactly one has to go and the
	// test measures which one rather than how many.
	l := New(sys("s"), nil, 60)

	l.Add(Document{Path: "AGENTS.md", Hash: "h1", Content: strings.Repeat("a", 100), Pinned: true})
	l.Add(doc("old.go", strings.Repeat("b", 100)))
	l.Add(doc("new.go", strings.Repeat("c", 100)))

	// Referencing the older file makes it the most recently used, so the one
	// added last is the one that should go.
	l.Touch("old.go")

	evicted := l.Rebuild()
	if len(evicted) == 0 {
		t.Fatal("a rebuild over budget evicted nothing")
	}

	kept := map[string]bool{}
	for _, d := range l.Documents() {
		kept[d.Path] = true
	}
	if !kept["AGENTS.md"] {
		t.Error("a pinned document was evicted; project instructions outrank a file read once")
	}
	if !kept["old.go"] {
		t.Error("the most recently referenced document was evicted")
	}
	if kept["new.go"] {
		t.Error("the least recently referenced document survived")
	}
}

// A rebuild is the scheduled, cache-invalidating event that reopens the zone.
func TestRebuildReopensTheStableZone(t *testing.T) {
	l := New(sys("s"), nil, 0)
	l.AppendHistory(provider.UserText("q"))

	if !l.Sealed() {
		t.Fatal("the zone did not seal when history started")
	}
	l.Rebuild()
	if l.Sealed() {
		t.Error("a rebuild did not reopen the stable zone")
	}
	if err := l.Add(doc("now-allowed.go", "x")); err != nil {
		t.Errorf("adding after a rebuild was refused: %v", err)
	}
}

// The stable zone is rendered as a conversation turn rather than as system
// blocks, so adding a document cannot alter a system prompt the target has
// already cached.
func TestDocumentsDoNotEnterTheSystemPrompt(t *testing.T) {
	l := New(sys("you are terse"), nil, 0)
	l.Add(doc("secret.go", "package secret"))

	req := l.Request()
	for _, b := range req.System {
		if strings.Contains(b.(provider.Text).Text, "secret.go") {
			t.Error("a document reached the system prompt, so the frozen zone is not frozen")
		}
	}

	var found bool
	for _, m := range req.Messages {
		for _, b := range m.Content {
			if txt, ok := b.(provider.Text); ok && strings.Contains(txt.Text, "package secret") {
				found = true
			}
		}
	}
	if !found {
		t.Error("the document never reached the request")
	}
}

func TestEmptyLayoutProducesAUsableRequest(t *testing.T) {
	l := New(sys("s"), nil, 0)
	l.SetTail(provider.Text{Text: "hello"})

	req := l.Request()
	if len(req.Messages) != 1 || req.Messages[0].Role != provider.RoleUser {
		t.Fatalf("messages = %+v", req.Messages)
	}
	if len(req.System) != 1 {
		t.Errorf("system = %+v", req.System)
	}
}

func TestRequestTokensCountsEveryZone(t *testing.T) {
	req := provider.Request{
		System: []provider.Block{provider.Text{Text: strings.Repeat("s", 40)}},
		Tools: []provider.ToolDefinition{
			{Name: "read", Description: strings.Repeat("d", 36), Schema: []byte("{}")},
		},
		Messages: []provider.Message{
			{Role: provider.RoleUser, Content: []provider.Block{provider.Text{Text: strings.Repeat("u", 80)}}},
		},
	}
	// Characters over four: 10 for the system block, 10 for the tool
	// (4 + 36 + 2 over four), 20 for the message.
	if got := RequestTokens(req); got != 40 {
		t.Errorf("RequestTokens = %d, want 40", got)
	}
}

func TestRequestEstimatesExcludeIncompleteAssistantMessages(t *testing.T) {
	baseline := provider.Request{Messages: []provider.Message{
		provider.UserText("before"),
		provider.UserText("after"),
	}}
	withPartial := baseline
	withPartial.Messages = []provider.Message{
		baseline.Messages[0],
		{
			Role:       provider.RoleAssistant,
			Incomplete: true,
			Content:    []provider.Block{provider.Text{Text: strings.Repeat("must not be estimated", 10_000)}},
		},
		baseline.Messages[1],
	}

	if got, want := RequestTokens(withPartial), RequestTokens(baseline); got != want {
		t.Fatalf("estimated tokens = %d, want %d after replay projection", got, want)
	}
	if got, want := RequestTokenCeiling(withPartial), RequestTokenCeiling(baseline); got != want {
		t.Fatalf("token ceiling = %d, want %d after replay projection", got, want)
	}
	if len(withPartial.Messages) != 3 {
		t.Fatal("estimation mutated the durable request")
	}
}

func TestRequestTokenCeilingCountsShortBlockFraming(t *testing.T) {
	req := provider.Request{}
	for range 1_000 {
		req.Messages = append(req.Messages, provider.Message{
			Role: provider.RoleUser, Content: []provider.Block{provider.Text{Text: "x"}},
		})
	}
	if floor := RequestTokens(req); floor != 0 {
		t.Fatalf("fixture floor = %d, want zero to reproduce per-block truncation", floor)
	}
	if ceiling := RequestTokenCeiling(req); ceiling < 10_000 {
		t.Fatalf("hard context bound = %d, did not account for short-message framing", ceiling)
	}
}

func TestRequestTokenCeilingBoundsDenseTextByBytes(t *testing.T) {
	for name, content := range map[string]string{
		"dense unicode": strings.Repeat("🧪", 1_000),
		"base64":        strings.Repeat("/+/+", 1_000),
		"code":          strings.Repeat("};){=>", 1_000),
	} {
		t.Run(name, func(t *testing.T) {
			req := provider.Request{Messages: []provider.Message{provider.UserText(content)}}
			floor := RequestTokens(req)
			ceiling := RequestTokenCeiling(req)
			if ceiling < len(content) {
				t.Fatalf("hard bound = %d, want at least one token per payload byte (%d)", ceiling, len(content))
			}
			if ceiling <= floor {
				t.Fatalf("hard bound = %d, floor = %d; adversarial text was not widened", ceiling, floor)
			}
		})
	}
}

func TestRequestTokenCeilingAccountsForImageGeometry(t *testing.T) {
	encode := func(width, height int) []byte {
		var out bytes.Buffer
		if err := png.Encode(&out, image.NewGray(image.Rect(0, 0, width, height))); err != nil {
			t.Fatal(err)
		}
		return out.Bytes()
	}
	small := encode(16, 16)
	large := encode(2_048, 2_048)
	if len(small) < len(large) {
		small = append(small, make([]byte, len(large)-len(small))...)
	} else {
		large = append(large, make([]byte, len(small)-len(large))...)
	}
	request := func(data []byte) provider.Request {
		return provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: []provider.Block{
			provider.Image{MediaType: "image/png", Data: data},
		}}}}
	}
	smallBound := RequestTokenCeiling(request(small))
	largeBound := RequestTokenCeiling(request(large))
	if largeBound <= smallBound {
		t.Fatalf("equal-byte images ignored decoded geometry: small=%d large=%d bytes=%d", smallBound, largeBound, len(large))
	}
}

func TestRequestTokenCeilingRefusesCompressedDocumentGuess(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	req := provider.Request{Messages: []provider.Message{{Role: provider.RoleUser, Content: []provider.Block{
		provider.Document{MediaType: "application/pdf", Data: []byte("%PDF-compressed")},
	}}}}
	if got := RequestTokenCeiling(req); got != maxInt {
		t.Fatalf("opaque compressed document bound = %d, want unknown/max", got)
	}
}
