package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/tools"
)

// nameCharset is what every provider accepts in a tool name. MCP servers may
// use anything; a character outside this set is mapped to an underscore
// before the name crosses a wire.
var nameCharset = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// maxToolName is the tightest documented provider limit.
const maxToolName = 64

func sanitize(s string) string {
	return nameCharset.ReplaceAllString(s, "_")
}

// redactMetadata scans each complete value before it is embedded in a tool
// name, description, or schema. Scanning only the finished representation is
// not enough: the fixed MCP prefixes can remove the word boundary that makes a
// credential recognizable, and any future cap must not be able to split one
// into an unrecognizable prefix before the scan sees it.
func redactMetadata(s string) string {
	if leaks := credential.ScanPrompt(s); len(leaks) > 0 {
		return credential.Redact(s, leaks)
	}
	return s
}

// Namespaced renders the registry name for one server's tool. The mcp__
// prefix keeps the external suite visibly separate from the built-ins and
// unable to collide with them.
func Namespaced(server, tool string) string {
	return "mcp__" + sanitize(redactMetadata(server)) + "__" + sanitize(redactMetadata(tool))
}

// BridgedTools wraps every discovered tool for the registry. A tool whose
// namespaced name would not survive the providers' constraints is skipped
// with a notice rather than silently renamed into a collision.
func (c *Client) BridgedTools() []tools.Tool {
	var out []tools.Tool
	seen := map[string]bool{}
	for _, info := range c.tools {
		name := Namespaced(c.spec.Name, info.Name)
		if len(name) > maxToolName {
			c.logf("warn", fmt.Sprintf("mcp %s: tool %s skipped: namespaced name exceeds %d characters", redactMetadata(c.spec.Name), redactMetadata(info.Name), maxToolName))
			continue
		}
		if seen[name] {
			c.logf("warn", fmt.Sprintf("mcp %s: tool %s skipped: name collides after sanitizing", redactMetadata(c.spec.Name), redactMetadata(info.Name)))
			continue
		}
		seen[name] = true
		out = append(out, &bridgedTool{client: c, info: info, name: name})
	}
	return out
}

// AllowRules translates the spec's allow list into permission rules, so a
// tool the user named in config runs without a prompt. The rule names the
// namespaced form: what the user allowed is this server's tool, not any
// tool that happens to share the short name.
func (c *Client) AllowRules() []permission.Rule {
	var rules []permission.Rule
	for _, tool := range c.spec.Allow {
		rules = append(rules, permission.Rule{
			Decision: permission.Allow,
			Tool:     Namespaced(c.spec.Name, tool),
			Effect:   permission.EffectExternal,
		})
	}
	return rules
}

type bridgedTool struct {
	client *Client
	info   ToolInfo
	name   string

	// registry is set when the tool is registered, and is where a returned
	// picture goes. Nil means nobody is collecting: the images are dropped
	// and the result says so, which is the closed state.
	registry *tools.Registry
}

func (t *bridgedTool) setImageSink(r *tools.Registry) { t.registry = r }

func (t *bridgedTool) Name() string { return t.name }

func (t *bridgedTool) Description() string {
	server := strings.TrimSpace(redactMetadata(t.client.spec.Name))
	desc := strings.TrimSpace(redactMetadata(t.info.Description))
	if desc == "" {
		desc = "No description provided."
	}
	// The component scans above are the boundary guarantee. This final scan is
	// defense in depth if punctuation around them ever changes.
	return redactMetadata(fmt.Sprintf("[%s MCP] %s", server, desc))
}

// ParallelSafe is false for every bridged tool: this client cannot know what
// a server-side tool touches, and two opaque effects in flight at once is a
// race nobody can reason about afterward.
func (t *bridgedTool) ParallelSafe() bool { return false }

func (t *bridgedTool) Schema() json.RawMessage {
	if len(t.info.InputSchema) > 0 && json.Valid(t.info.InputSchema) {
		return redactSchemaStrings(t.info.InputSchema)
	}
	return emptyObjectSchema()
}

func emptyObjectSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

// redactSchemaStrings sanitizes the semantic value of every JSON string,
// including property names and strings written with JSON escapes. It preserves
// every non-secret token byte-for-byte, so large integers, schema extensions,
// and formatting cannot be changed by a decode into interface{} and re-encode.
// The input is known-valid JSON and is never mutated.
func redactSchemaStrings(raw json.RawMessage) json.RawMessage {
	out := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); {
		if raw[i] != '"' {
			out = append(out, raw[i])
			i++
			continue
		}

		start := i
		i++
		for i < len(raw) {
			switch raw[i] {
			case '\\':
				i += 2
			case '"':
				i++
				goto stringEnd
			default:
				i++
			}
		}

	stringEnd:
		token := raw[start:i]
		var value string
		if err := json.Unmarshal(token, &value); err != nil {
			// Schema checked json.Valid, so this is unreachable unless the token
			// walk and the parser disagree. Do not let that disagreement become a
			// raw-metadata escape hatch.
			return emptyObjectSchema()
		}
		redacted := redactMetadata(value)
		if redacted == value {
			out = append(out, token...)
			continue
		}
		encoded, _ := json.Marshal(redacted) // encoding a string cannot fail
		out = append(out, encoded...)
	}
	return json.RawMessage(out)
}

func (t *bridgedTool) Plan(input json.RawMessage) (tools.Plan, error) {
	if len(input) > 0 && !json.Valid(input) {
		return tools.Plan{}, fmt.Errorf("%s: arguments are not valid JSON", t.name)
	}
	if semanticJSONCarriesCredential(input) {
		return tools.Plan{}, fmt.Errorf("%s: arguments contain credential-shaped data; refusing to send them to an external MCP server", t.name)
	}

	// The external effect is the honest classification: whatever this tool
	// does happens outside the workspace boundary and outside any sandbox
	// this host verified, in a process the permission engine cannot see into.
	// Detail is display only — the dialog shows the arguments, while the
	// remembered answer covers the tool, because a user approving an MCP tool
	// is approving the tool, not one byte-exact invocation.
	return tools.Plan{
		Request: permission.Request{
			Tool:   t.name,
			Effect: permission.EffectExternal,
			Detail: redactMetadata(fmt.Sprintf("%s (%s server)", compactJSON(input), redactMetadata(t.client.spec.Name))),
		},
		Run: func(ctx context.Context) (tools.Result, error) {
			res, err := t.client.Call(ctx, t.info.Name, input)
			if err != nil {
				if ctx.Err() != nil {
					return tools.Result{}, ctx.Err()
				}
				return tools.Result{Content: redactMetadata(err.Error()), IsError: true}, nil
			}
			// The pictures go to the registry, which decides whether the
			// bound rung can see one and returns the sentence saying what
			// happened. A dropped image the model is not told about is a
			// model reasoning about a screenshot it never saw.
			content := res.Content
			if t.registry != nil {
				content += t.registry.AcceptToolImages(res.Images)
			} else if len(res.Images) > 0 {
				content += fmt.Sprintf("\n\n[%d image blocks were returned and there is nowhere to deliver them.]", len(res.Images))
			}
			return tools.Result{Content: content, IsError: res.IsError}, nil
		},
	}, nil
}

// semanticJSONCarriesCredential checks each decoded JSON string independently,
// including object keys and strings containing JSON escapes. The bridge does
// not concatenate sibling values and guess that they form a credential, but a
// recognized credential wholly contained in any one semantic value must never
// reach an unconfined MCP server. A decoder disagreement after json.Valid is a
// closed failure rather than an egress bypass.
func semanticJSONCarriesCredential(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	for {
		token, err := dec.Token()
		if err == io.EOF {
			return false
		}
		if err != nil {
			return true
		}
		if text, ok := token.(string); ok && len(credential.ScanPrompt(text)) > 0 {
			return true
		}
	}
}

// compactJSON renders arguments for the permission dialog: one line, bounded.
func compactJSON(raw json.RawMessage) string {
	compact := json.RawMessage(raw)
	if len(compact) == 0 {
		return "{}"
	}
	var tmp any
	if err := json.Unmarshal(compact, &tmp); err == nil {
		if b, err := json.Marshal(tmp); err == nil {
			compact = b
		}
	}
	if json.Valid(compact) {
		compact = redactSchemaStrings(compact)
	}
	s := redactMetadata(strings.ToValidUTF8(string(compact), "�"))
	const maxDetailBytes = 120
	if len(s) > maxDetailBytes {
		const marker = "…"
		keep := maxDetailBytes - len(marker)
		for keep > 0 && !utf8.RuneStart(s[keep]) {
			keep--
		}
		s = s[:keep] + marker
	}
	return s
}

// SortTools orders bridged tools by name so the registry's frozen-zone
// ordering does not depend on server enumeration order.
func SortTools(ts []tools.Tool) {
	slices.SortFunc(ts, func(a, b tools.Tool) int { return strings.Compare(a.Name(), b.Name()) })
}
