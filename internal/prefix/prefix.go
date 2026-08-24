// Package prefix lays out a request so that the part a provider can cache stays
// byte-identical from one turn to the next.
//
// §6.1 describes four ordered zones and calls the layout "not clever, but
// discipline, and worth more than any of the optimizations below". Discipline
// that lives in a document gets violated by the next person in a hurry, so the
// zones are types here and the rules are the only operations they offer: the
// frozen zone has no setter, the stable zone refuses writes once a session is
// under way, history appends and nothing else, and only the tail can be
// rewritten.
//
// The failure this prevents is specific. Inserting a block anywhere above the
// tail shifts every block after it, so the provider's cached prefix stops
// matching at the insertion point and the whole remainder is re-read at full
// price. Most harnesses do this to themselves by appending retrieved context
// into the middle of the conversation.
package prefix

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"sort"
	"strings"

	"github.com/switchboard-code/switchboard/internal/provider"
)

// Zone names a region of the request. The order is the order they are sent in,
// which is also increasing order of how often they change.
type Zone int

const (
	// Frozen holds the system prompt, the tool definitions, and project
	// instructions. It is fixed for the life of a session.
	Frozen Zone = iota

	// Stable holds file contents and retrieved documents confirmed unchanged by
	// content hash. It is populated at session start or at a scheduled rebuild,
	// never in between.
	Stable

	// History holds conversation turns, tool calls, and tool results. It only
	// grows.
	History

	// Tail holds the current user message and any per-request state. It is the
	// only zone that may be rewritten.
	Tail
)

func (z Zone) String() string {
	switch z {
	case Frozen:
		return "frozen"
	case Stable:
		return "stable"
	case History:
		return "history"
	case Tail:
		return "tail"
	}
	return "unknown"
}

// ErrSealed reports an attempt to write to a zone that is closed for the
// session. It is a programming error rather than a user error: the caller
// should have scheduled a rebuild.
var ErrSealed = errors.New("the stable zone is sealed until the next rebuild")

// Document is a file or retrieved text placed in the stable zone.
//
// Hash is over the content, and it is the whole basis for the zone: a document
// belongs here only while it is known unchanged. When it changes, a new history
// block supersedes it rather than the stable entry being edited, because
// editing would move every block after it.
type Document struct {
	Path    string
	Hash    string
	Content string

	// Pinned documents survive eviction. AGENTS.md and anything the user named
	// explicitly are pinned, because evicting the instructions that shape the
	// session to make room for a file read once is the wrong trade.
	Pinned bool

	// lastReferenced orders eviction. It is a counter rather than a clock so
	// that layout decisions are reproducible from a session log.
	lastReferenced uint64
}

// Layout assembles a request from the four zones.
//
// It is not safe for concurrent use; the agent loop drives one turn at a time.
type Layout struct {
	system []provider.Block
	tools  []provider.ToolDefinition

	documents []*Document
	sealed    bool

	history []provider.Message
	tail    []provider.Block

	// budget caps the stable zone in tokens. Crossing it schedules a rebuild
	// rather than truncating: silently dropping a document the model was told
	// it had is worse than paying to rebuild the prefix deliberately.
	budget int

	clock uint64
}

// New builds a layout. The frozen zone is supplied once and has no setter,
// which is the mechanism §6.1 relies on: dynamic operator context goes in the
// tail, never by editing what has already been cached.
//
// Tool definitions are sorted by name. Two sessions that registered the same
// tools in a different order would otherwise produce different prefixes and
// share no cache, for no reason a user could see.
func New(system []provider.Block, tools []provider.ToolDefinition, stableBudget int) *Layout {
	sorted := append([]provider.ToolDefinition(nil), tools...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	return &Layout{
		system: append([]provider.Block(nil), system...),
		tools:  sorted,
		budget: stableBudget,
	}
}

// Add places a document in the stable zone. It is refused once the zone is
// sealed, which happens as soon as the session starts producing history.
func (l *Layout) Add(doc Document) error {
	if l.sealed {
		return fmt.Errorf("%w: %s must go in the history zone or wait for a rebuild", ErrSealed, doc.Path)
	}
	l.clock++
	doc.lastReferenced = l.clock

	for i, existing := range l.documents {
		if existing.Path != doc.Path {
			continue
		}
		if existing.Hash == doc.Hash {
			// Already present and unchanged. Touching it keeps eviction honest
			// without moving it, because moving it would shift everything after.
			l.documents[i].lastReferenced = doc.lastReferenced
			return nil
		}
		// Replacing in place before the session starts is safe: nothing has
		// been cached yet.
		l.documents[i] = &doc
		return nil
	}
	l.documents = append(l.documents, &doc)
	return nil
}

// Touch records that a document was referenced, which is what keeps a
// frequently used file from being evicted at the next rebuild. It does not move
// anything.
func (l *Layout) Touch(path string) {
	for _, doc := range l.documents {
		if doc.Path == path {
			l.clock++
			doc.lastReferenced = l.clock
			return
		}
	}
}

// Seal closes the stable zone. Everything after this point appends to history.
func (l *Layout) Seal() { l.sealed = true }

func (l *Layout) Sealed() bool { return l.sealed }

// AppendHistory adds a turn. Sealing on the first append is what makes the
// rule automatic rather than something a caller has to remember.
func (l *Layout) AppendHistory(msgs ...provider.Message) {
	if len(msgs) > 0 {
		l.sealed = true
	}
	l.history = append(l.history, msgs...)
}

func (l *Layout) History() []provider.Message {
	return append([]provider.Message(nil), l.history...)
}

// SetTail replaces the volatile tail. This is the only rewrite the layout
// allows, and it is where mode changes, remaining budget, and the current
// instruction belong.
func (l *Layout) SetTail(blocks ...provider.Block) {
	l.tail = append([]provider.Block(nil), blocks...)
}

// Documents returns the stable zone in send order.
func (l *Layout) Documents() []Document {
	out := make([]Document, 0, len(l.documents))
	for _, doc := range l.documents {
		out = append(out, *doc)
	}
	return out
}

// StableTokens estimates the stable zone's size. It is characters over four,
// the same crude estimate the local adapters use and with the same measured
// bias (docs/estimator.md), which is acceptable here because it decides when to
// schedule a rebuild rather than what anything costs.
func (l *Layout) StableTokens() int {
	total := 0
	for _, doc := range l.documents {
		total += tokensOf(doc)
	}
	return total
}

// NeedsRebuild reports whether the stable zone has outgrown its budget.
//
// The answer is advisory. Crossing the budget schedules a rebuild; it does not
// truncate, because dropping a document mid-session both invalidates the cache
// and leaves the model referring to content that is no longer there.
func (l *Layout) NeedsRebuild() bool {
	return l.budget > 0 && l.StableTokens() > l.budget
}

// Rebuild opens the stable zone again and evicts down to the budget.
//
// This is a cache-invalidating event by definition, which is why it is an
// explicit call rather than something that happens when a threshold is crossed.
// Eviction is least-recently-referenced, and pinned documents are exempt.
func (l *Layout) Rebuild() (evicted []string) {
	l.sealed = false
	if l.budget <= 0 {
		return nil
	}

	// Oldest reference first, so the survivors are what the session actually
	// keeps using.
	order := make([]*Document, len(l.documents))
	copy(order, l.documents)
	sort.SliceStable(order, func(i, j int) bool {
		return order[i].lastReferenced < order[j].lastReferenced
	})

	remaining := l.StableTokens()
	drop := map[string]bool{}
	for _, doc := range order {
		if remaining <= l.budget {
			break
		}
		if doc.Pinned {
			continue
		}
		drop[doc.Path] = true
		remaining -= tokensOf(doc)
		evicted = append(evicted, doc.Path)
	}
	if len(drop) == 0 {
		return nil
	}

	kept := l.documents[:0]
	for _, doc := range l.documents {
		if !drop[doc.Path] {
			kept = append(kept, doc)
		}
	}
	l.documents = kept
	return evicted
}

func tokensOf(doc *Document) int {
	return (len(doc.Path) + len(doc.Content)) / 4
}

// Request assembles the canonical request.
//
// The stable zone is rendered as a single user message followed by an assistant
// acknowledgement, rather than as system blocks, so that adding a document
// cannot alter the system prompt a target has already cached.
func (l *Layout) Request() provider.Request {
	messages := make([]provider.Message, 0, len(l.history)+3)

	if len(l.documents) > 0 {
		var b strings.Builder
		b.WriteString("Files already read for this session. " +
			"Each is current as of the hash shown; if one changes, a later message supersedes it.\n")
		for _, doc := range l.documents {
			fmt.Fprintf(&b, "\n<file path=%q sha256=%q>\n%s\n</file>\n", doc.Path, doc.Hash, doc.Content)
		}
		messages = append(messages,
			provider.Message{Role: provider.RoleUser, Content: []provider.Block{provider.Text{Text: b.String()}}},
			provider.Message{Role: provider.RoleAssistant, Content: []provider.Block{
				provider.Text{Text: "Noted. I will use those contents unless a later message supersedes them."},
			}},
		)
	}

	messages = append(messages, l.history...)
	if len(l.tail) > 0 {
		messages = append(messages, provider.Message{Role: provider.RoleUser, Content: l.tail})
	}

	return provider.Request{
		System:   append([]provider.Block(nil), l.system...),
		Tools:    append([]provider.ToolDefinition(nil), l.tools...),
		Messages: messages,
	}
}

// Boundary is the last position of a zone in the rendered request, with the
// prefix length it covers.
//
// The breakpoint manager needs both and can derive neither: only the layout
// knows how its zones become messages, and a marker placed from a guess at that
// caches a different prefix than the one that was scored.
type Boundary struct {
	Zone     Zone
	Position provider.CachePosition

	// TokensBefore estimates everything from the start of the request through
	// this position. It decides whether a target's minimum is met, so it counts
	// the prefix rather than the zone.
	TokensBefore int
}

// Boundaries reports zone ends in prefix order, which is the order a provider
// reads them: tools, then the system prompt, then the stable documents, then
// history. A boundary is omitted when its zone is empty, because a marker on
// nothing caches nothing.
func (l *Layout) Boundaries() []Boundary {
	req := l.Request()
	var out []Boundary
	running := 0

	for i, t := range l.tools {
		running += (len(t.Name) + len(t.Description) + len(t.Schema)) / 4
		if i == len(l.tools)-1 {
			out = append(out, Boundary{
				Zone:         Frozen,
				Position:     provider.CachePosition{MessageIndex: provider.ToolDefinitions, BlockIndex: i},
				TokensBefore: running,
			})
		}
	}
	for i, b := range l.system {
		running += blockTokens(b)
		if i == len(l.system)-1 {
			out = append(out, Boundary{
				Zone:         Frozen,
				Position:     provider.CachePosition{MessageIndex: provider.SystemBlocks, BlockIndex: i},
				TokensBefore: running,
			})
		}
	}

	// Messages carry the stable zone first when it has anything, then history,
	// then the tail. The tail is deliberately not a boundary: it is rewritten
	// every turn, so a marker on it would never be reused.
	stableMessages := 0
	if len(l.documents) > 0 {
		stableMessages = 2
	}
	historyEnd := stableMessages + len(l.history)

	for i, m := range req.Messages {
		if i >= historyEnd {
			break // the tail
		}
		for _, b := range m.Content {
			running += blockTokens(b)
		}
		last := len(m.Content) - 1
		if last < 0 {
			continue
		}
		switch {
		case stableMessages > 0 && i == stableMessages-1:
			out = append(out, Boundary{
				Zone:         Stable,
				Position:     provider.CachePosition{MessageIndex: i, BlockIndex: last},
				TokensBefore: running,
			})
		case i == historyEnd-1:
			out = append(out, Boundary{
				Zone:         History,
				Position:     provider.CachePosition{MessageIndex: i, BlockIndex: last},
				TokensBefore: running,
			})
		}
	}
	return out
}

// HistoryBlocks counts blocks in the history zone. A target searches back only
// so far for a reusable prefix, so this is what a growing turn is measured
// against.
func (l *Layout) HistoryBlocks() int {
	n := 0
	for _, m := range l.history {
		n += len(m.Content)
	}
	return n
}

// RequestTokens estimates what a whole request costs a target to read:
// system, tool definitions, and every message. Characters over four, the
// same crude estimate everything else here uses, with the same measured
// bias (docs/estimator.md): a floor, never an overcount. The budget check
// prices its preflight bound from this, which is why it is exported.
func RequestTokens(req provider.Request) int {
	req = provider.ReplayRequest(req)
	total := 0
	for _, b := range req.System {
		total += blockTokens(b)
	}
	for _, t := range req.Tools {
		total += (len(t.Name) + len(t.Description) + len(t.Schema)) / 4
	}
	for _, m := range req.Messages {
		for _, b := range m.Content {
			total += blockTokens(b)
		}
	}
	return total
}

// RequestTokenCeiling is the fail-safe count used for hard context and dollar
// limits. RequestTokens deliberately remains the comparable chars/4 estimate
// used for the displayed expected cost. A measured average error cannot be a
// hard bound: dense Unicode, code, and many tiny blocks can all tokenize much
// more densely than that sample. This count therefore allows one token for
// every payload byte and adds explicit request framing. Byte-fallback
// tokenizers cannot exceed that text allowance; media has a separate bound.
func RequestTokenCeiling(req provider.Request) int {
	req = provider.ReplayRequest(req)
	total := 16 // request envelope
	for _, block := range req.System {
		total = saturatingIntAdd(total, 8)
		total = saturatingIntAdd(total, blockTokenCeiling(block))
	}
	for _, message := range req.Messages {
		total = saturatingIntAdd(total, 16+len(message.Role))
		for _, block := range message.Content {
			total = saturatingIntAdd(total, 8)
			total = saturatingIntAdd(total, blockTokenCeiling(block))
		}
	}
	for _, tool := range req.Tools {
		total = saturatingIntAdd(total, 32)
		total = saturatingIntAdd(total, len(tool.Name))
		total = saturatingIntAdd(total, len(tool.Description))
		total = saturatingIntAdd(total, len(tool.Schema))
	}
	return total
}

func blockTokenCeiling(block provider.Block) int {
	switch value := block.(type) {
	case provider.Text:
		return len(value.Text)
	case provider.Thinking:
		return saturatingIntAdd(len(value.Text), len(value.Signature))
	case provider.ToolUse:
		total := saturatingIntAdd(len(value.ID), len(value.Name))
		return saturatingIntAdd(total, len(value.Input))
	case provider.ToolResult:
		total := saturatingIntAdd(len(value.ToolUseID), len(value.Name))
		return saturatingIntAdd(total, len(value.Content))
	case provider.Image:
		return saturatingIntAdd(len(value.MediaType), imageTokenCeiling(value.Data))
	case provider.Document:
		metadata := saturatingIntAdd(len(value.MediaType), len(value.Name))
		if strings.HasPrefix(strings.ToLower(value.MediaType), "text/") {
			return saturatingIntAdd(metadata, len(value.Data))
		}
		// Compressed PDFs and office documents can expand without a safe
		// byte ratio. Mark them unknown so a finite context window refuses
		// them instead of accepting a fabricated bound.
		return int(^uint(0) >> 1)
	default:
		// A new block type has no established hard bound. Fail closed until
		// its provider rendering and tokenization are accounted for here.
		return int(^uint(0) >> 1)
	}
}

func imageTokenCeiling(data []byte) int {
	// One token per encoded byte is already deliberately pessimistic. Geometry
	// is a separate lower bound because a highly compressible image may contain
	// very few bytes while providers tile a large decoded canvas.
	ceiling := saturatingIntAdd(len(data), 2_048)
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return max(ceiling, 8_192)
	}
	maxInt := int(^uint(0) >> 1)
	if config.Width > maxInt/config.Height {
		return maxInt
	}
	pixels := config.Width * config.Height
	geometry := saturatingIntAdd((pixels+255)/256, 2_048)
	return max(ceiling, geometry)
}

func saturatingIntAdd(a, b int) int {
	maxInt := int(^uint(0) >> 1)
	if a < 0 || b < 0 || a > maxInt-b {
		return maxInt
	}
	return a + b
}

func blockTokens(b provider.Block) int {
	switch v := b.(type) {
	case provider.Text:
		return len(v.Text) / 4
	case provider.Thinking:
		return (len(v.Text) + len(v.Signature)) / 4
	case provider.ToolUse:
		return (len(v.Name) + len(v.Input)) / 4
	case provider.ToolResult:
		return (len(v.Name) + len(v.Content)) / 4
	case provider.Image:
		// Providers price images by their own geometry, not by bytes; this is
		// the same crude floor the Ollama adapter's estimate uses, kept so an
		// attached image never counts as free.
		return len(v.Data) / 12
	case provider.Document:
		return len(v.Data) / 12
	default:
		return 0
	}
}

// PrefixHash fingerprints everything above the tail.
//
// The cache tracker keys on this: a prefix that hashes the same is one the
// provider could still be holding, and one that hashes differently cannot be,
// whatever either side believes. The tail is excluded because it is expected to
// change every turn and including it would make every prefix look new.
func (l *Layout) PrefixHash() string {
	h := sha256.New()

	writeBlocks(h, l.system)
	for _, t := range l.tools {
		fmt.Fprintf(h, "tool\x00%s\x00%s\x00%s\x00", t.Name, t.Description, t.Schema)
	}
	for _, doc := range l.documents {
		fmt.Fprintf(h, "doc\x00%s\x00%s\x00", doc.Path, doc.Hash)
	}
	for _, m := range l.history {
		fmt.Fprintf(h, "msg\x00%s\x00", m.Role)
		writeBlocks(h, m.Content)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeBlocks(h interface{ Write([]byte) (int, error) }, blocks []provider.Block) {
	for _, b := range blocks {
		switch v := b.(type) {
		case provider.Text:
			fmt.Fprintf(h, "text\x00%s\x00", v.Text)
		case provider.Thinking:
			// The signature is included: replacing a thinking block with an
			// unsigned copy changes what will be sent, so it changes the prefix.
			fmt.Fprintf(h, "thinking\x00%s\x00%s\x00", v.Text, v.Signature)
		case provider.ToolUse:
			fmt.Fprintf(h, "tool_use\x00%s\x00%s\x00%s\x00", v.ID, v.Name, v.Input)
		case provider.ToolResult:
			fmt.Fprintf(h, "tool_result\x00%s\x00%s\x00", v.ToolUseID, v.Content)
		default:
			fmt.Fprintf(h, "%s\x00", b.Kind())
		}
	}
}
