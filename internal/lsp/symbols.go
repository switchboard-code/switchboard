package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

const (
	defaultDocumentSymbolLimit  = 1_000
	maxDocumentSymbolLimit      = 2_000
	defaultWorkspaceSymbolLimit = 50
	maxWorkspaceSymbolLimit     = 200
)

// Range is a zero-based LSP wire range. User-facing renderers convert it to
// the product's 1-based file:line:column convention at their boundary.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end"`
}

// SymbolKind uses the numeric values fixed by the LSP specification.
type SymbolKind int

const (
	SymbolFile SymbolKind = iota + 1
	SymbolModule
	SymbolNamespace
	SymbolPackage
	SymbolClass
	SymbolMethod
	SymbolProperty
	SymbolField
	SymbolConstructor
	SymbolEnum
	SymbolInterface
	SymbolFunction
	SymbolVariable
	SymbolConstant
	SymbolString
	SymbolNumber
	SymbolBoolean
	SymbolArray
	SymbolObject
	SymbolKey
	SymbolNull
	SymbolEnumMember
	SymbolStruct
	SymbolEvent
	SymbolOperator
	SymbolTypeParameter
)

func (kind SymbolKind) String() string {
	names := [...]string{
		"", "file", "module", "namespace", "package", "class", "method",
		"property", "field", "constructor", "enum", "interface", "function",
		"variable", "constant", "string", "number", "boolean", "array",
		"object", "key", "null", "enum member", "struct", "event", "operator",
		"type parameter",
	}
	if kind > 0 && int(kind) < len(names) {
		return names[kind]
	}
	return fmt.Sprintf("unknown(%d)", kind)
}

// Symbol is the common, bounded representation returned for both document
// and workspace symbol requests. Depth is meaningful only for hierarchical
// document symbols. Flat SymbolInformation container names remain labels;
// they are never used to infer a hierarchy.
type Symbol struct {
	Name           string
	Detail         string
	Container      string
	Path           string
	Kind           SymbolKind
	Range          Range
	SelectionRange Range
	Depth          int
	Deprecated     bool
}

type wireDocumentSymbol struct {
	Name           string               `json:"name"`
	Detail         string               `json:"detail"`
	Kind           SymbolKind           `json:"kind"`
	Tags           []int                `json:"tags"`
	Deprecated     bool                 `json:"deprecated"`
	Range          *Range               `json:"range"`
	SelectionRange *Range               `json:"selectionRange"`
	Children       []wireDocumentSymbol `json:"children"`
}

type wireSymbolInformation struct {
	Name       string     `json:"name"`
	Kind       SymbolKind `json:"kind"`
	Tags       []int      `json:"tags"`
	Deprecated bool       `json:"deprecated"`
	Location   struct {
		URI   string `json:"uri"`
		Range *Range `json:"range"`
	} `json:"location"`
	ContainerName string `json:"containerName"`
}

// DocumentSymbols returns a deterministic, bounded flattened outline while
// preserving hierarchy through Depth.
func (c *Client) DocumentSymbols(ctx context.Context, path string) ([]Symbol, error) {
	symbols, _, err := c.DocumentSymbolsBounded(ctx, path, defaultDocumentSymbolLimit)
	return symbols, err
}

// DocumentSymbolsBounded is the explicit-limit form used by tools and UIs.
func (c *Client) DocumentSymbolsBounded(ctx context.Context, path string, limit int) ([]Symbol, bool, error) {
	limit = boundedLimit(limit, defaultDocumentSymbolLimit, maxDocumentSymbolLimit)
	var raw json.RawMessage
	err := c.documentCall(ctx, FeatureDocumentSymbols, "textDocument/documentSymbol", path,
		func([]byte) (map[string]any, error) {
			return map[string]any{"textDocument": map[string]any{"uri": pathToURI(path)}}, nil
		}, &raw)
	if err != nil {
		return nil, false, err
	}
	return decodeDocumentSymbols(raw, filepath.Clean(path), limit)
}

// WorkspaceSymbols searches project-wide symbols. Before the request it
// reconciles every document this client already owns; unopened files remain
// the server's saved-workspace/file-watcher responsibility.
func (c *Client) WorkspaceSymbols(ctx context.Context, query string, limit int) ([]Symbol, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if err := c.require(FeatureWorkspaceSymbols); err != nil {
		return nil, false, err
	}
	limit = boundedLimit(limit, defaultWorkspaceSymbolLimit, maxWorkspaceSymbolLimit)
	capabilities := c.Capabilities()

	c.documentsMu.Lock()
	if err := c.ensureRunning(); err != nil {
		c.documentsMu.Unlock()
		return nil, false, err
	}
	paths := make([]string, 0, len(c.documents))
	for path := range c.documents {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		data, err := c.readDocumentSnapshot(ctx, path)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				if closeErr := c.closeDocumentLocked(path, capabilities); closeErr != nil {
					c.documentsMu.Unlock()
					return nil, false, errors.Join(err, closeErr)
				}
				continue
			}
			if documentAuthorityChanged(err) {
				// A retained path is not authority. Remove both the client-side
				// snapshot and (when supported) the server-side open document so
				// a later workspace query cannot keep using bytes from a pathname
				// whose ancestor escaped or whose root identity changed.
				closeErr := c.closeDocumentLocked(path, capabilities)
				c.documentsMu.Unlock()
				return nil, false, errors.Join(err, closeErr)
			}
			c.documentsMu.Unlock()
			return nil, false, err
		}
		if err := c.syncDocumentLocked(path, data, capabilities); err != nil {
			c.documentsMu.Unlock()
			return nil, false, err
		}
	}
	handle, err := c.beginCall("workspace/symbol", map[string]any{"query": query})
	c.documentsMu.Unlock()
	if err != nil {
		return nil, false, err
	}

	var raw json.RawMessage
	if err := c.awaitCall(ctx, handle, &raw); err != nil {
		return nil, false, err
	}
	return decodeWorkspaceSymbols(raw, limit)
}

func decodeDocumentSymbols(raw json.RawMessage, path string, limit int) ([]Symbol, bool, error) {
	if len(raw) == 0 || isNull(raw) {
		return nil, false, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, false, fmt.Errorf("unreadable document-symbol answer: %w", err)
	}
	if len(items) == 0 {
		return nil, false, nil
	}
	limit = boundedLimit(limit, defaultDocumentSymbolLimit, maxDocumentSymbolLimit)

	var shape map[string]json.RawMessage
	if err := json.Unmarshal(items[0], &shape); err != nil {
		return nil, false, fmt.Errorf("unreadable document-symbol item: %w", err)
	}
	_, flat := shape["location"]
	if flat {
		var wire []wireSymbolInformation
		if err := json.Unmarshal(raw, &wire); err != nil {
			return nil, false, fmt.Errorf("unreadable flat document-symbol answer: %w", err)
		}
		symbols := make([]Symbol, 0, len(wire))
		for i, item := range wire {
			symbol, err := symbolFromInformation(item)
			if err != nil {
				return nil, false, fmt.Errorf("document symbol %d: %w", i, err)
			}
			symbols = append(symbols, symbol)
		}
		symbols = deterministicSymbols(symbols)
		return truncateSymbols(symbols, limit)
	}

	var wire []wireDocumentSymbol
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, false, fmt.Errorf("unreadable hierarchical document-symbol answer: %w", err)
	}
	sortDocumentSiblings(wire)
	symbols := make([]Symbol, 0, min(len(items), limit))
	for i := range wire {
		if err := flattenDocumentSymbol(&symbols, wire[i], filepath.Clean(path), 0); err != nil {
			return nil, false, fmt.Errorf("document symbol %d: %w", i, err)
		}
	}
	symbols = dedupeSymbols(symbols)
	return truncateSymbols(symbols, limit)
}

func decodeWorkspaceSymbols(raw json.RawMessage, limit int) ([]Symbol, bool, error) {
	if len(raw) == 0 || isNull(raw) {
		return nil, false, nil
	}
	var wire []wireSymbolInformation
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, false, fmt.Errorf("unreadable workspace-symbol answer: %w", err)
	}
	symbols := make([]Symbol, 0, len(wire))
	for i, item := range wire {
		symbol, err := symbolFromInformation(item)
		if err != nil {
			return nil, false, fmt.Errorf("workspace symbol %d: %w", i, err)
		}
		symbols = append(symbols, symbol)
	}
	symbols = deterministicSymbols(symbols)
	limit = boundedLimit(limit, defaultWorkspaceSymbolLimit, maxWorkspaceSymbolLimit)
	return truncateSymbols(symbols, limit)
}

func flattenDocumentSymbol(out *[]Symbol, wire wireDocumentSymbol, path string, depth int) error {
	if strings.TrimSpace(wire.Name) == "" {
		return fmt.Errorf("name is empty")
	}
	if wire.Range == nil || wire.SelectionRange == nil {
		return fmt.Errorf("%q omitted range or selectionRange", wire.Name)
	}
	if err := validateRange(*wire.Range); err != nil {
		return fmt.Errorf("%q range: %w", wire.Name, err)
	}
	if err := validateRange(*wire.SelectionRange); err != nil {
		return fmt.Errorf("%q selectionRange: %w", wire.Name, err)
	}
	if !rangeContains(*wire.Range, *wire.SelectionRange) {
		return fmt.Errorf("%q selectionRange is outside its range", wire.Name)
	}
	*out = append(*out, Symbol{
		Name: wire.Name, Detail: wire.Detail, Path: path, Kind: wire.Kind,
		Range: *wire.Range, SelectionRange: *wire.SelectionRange, Depth: depth,
		Deprecated: wire.Deprecated || hasDeprecatedTag(wire.Tags),
	})
	sortDocumentSiblings(wire.Children)
	for _, child := range wire.Children {
		if err := flattenDocumentSymbol(out, child, path, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func symbolFromInformation(wire wireSymbolInformation) (Symbol, error) {
	if strings.TrimSpace(wire.Name) == "" {
		return Symbol{}, fmt.Errorf("name is empty")
	}
	if wire.Location.URI == "" || wire.Location.Range == nil {
		return Symbol{}, fmt.Errorf("%q has no concrete location range", wire.Name)
	}
	if err := validateRange(*wire.Location.Range); err != nil {
		return Symbol{}, fmt.Errorf("%q range: %w", wire.Name, err)
	}
	path, err := filePath(wire.Location.URI)
	if err != nil {
		return Symbol{}, fmt.Errorf("%q URI: %w", wire.Name, err)
	}
	return Symbol{
		Name: wire.Name, Container: wire.ContainerName, Path: filepath.Clean(path), Kind: wire.Kind,
		Range: *wire.Location.Range, SelectionRange: *wire.Location.Range,
		Deprecated: wire.Deprecated || hasDeprecatedTag(wire.Tags),
	}, nil
}

func sortDocumentSiblings(symbols []wireDocumentSymbol) {
	sort.SliceStable(symbols, func(i, j int) bool {
		a, b := symbols[i], symbols[j]
		if a.SelectionRange != nil && b.SelectionRange != nil {
			if lessPosition(a.SelectionRange.Start, b.SelectionRange.Start) {
				return true
			}
			if lessPosition(b.SelectionRange.Start, a.SelectionRange.Start) {
				return false
			}
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Kind < b.Kind
	})
}

func deterministicSymbols(symbols []Symbol) []Symbol {
	sort.SliceStable(symbols, func(i, j int) bool {
		a, b := symbols[i], symbols[j]
		switch {
		case a.Path != b.Path:
			return filepath.ToSlash(a.Path) < filepath.ToSlash(b.Path)
		case lessPosition(a.SelectionRange.Start, b.SelectionRange.Start):
			return true
		case lessPosition(b.SelectionRange.Start, a.SelectionRange.Start):
			return false
		case a.Name != b.Name:
			return a.Name < b.Name
		case a.Kind != b.Kind:
			return a.Kind < b.Kind
		default:
			return a.Container < b.Container
		}
	})
	return dedupeSymbols(symbols)
}

func dedupeSymbols(symbols []Symbol) []Symbol {
	if len(symbols) < 2 {
		return symbols
	}
	out := symbols[:0]
	seen := make(map[string]bool, len(symbols))
	for _, symbol := range symbols {
		key := fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%d\x00%d\x00%d\x00%s\x00%d",
			symbol.Path, symbol.Name, symbol.Kind, symbol.SelectionRange.Start.Line,
			symbol.SelectionRange.Start.Character, symbol.SelectionRange.End.Line,
			symbol.SelectionRange.End.Character, symbol.Container, symbol.Depth)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, symbol)
	}
	return out
}

func truncateSymbols(symbols []Symbol, limit int) ([]Symbol, bool, error) {
	if len(symbols) <= limit {
		return symbols, false, nil
	}
	return append([]Symbol(nil), symbols[:limit]...), true, nil
}

func boundedLimit(limit, fallback, maximum int) int {
	if limit <= 0 {
		return fallback
	}
	if limit > maximum {
		return maximum
	}
	return limit
}

func validateRange(value Range) error {
	if value.Start.Line < 0 || value.Start.Character < 0 || value.End.Line < 0 || value.End.Character < 0 {
		return fmt.Errorf("positions must be non-negative")
	}
	if lessPosition(value.End, value.Start) {
		return fmt.Errorf("end precedes start")
	}
	return nil
}

func rangeContains(outer, inner Range) bool {
	return !lessPosition(inner.Start, outer.Start) && !lessPosition(outer.End, inner.End)
}

func lessPosition(a, b Position) bool {
	return a.Line < b.Line || (a.Line == b.Line && a.Character < b.Character)
}

func hasDeprecatedTag(tags []int) bool {
	for _, tag := range tags {
		if tag == 1 {
			return true
		}
	}
	return false
}
