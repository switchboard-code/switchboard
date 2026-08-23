package provider

import (
	"encoding/base64"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
)

// RouteTargetID identifies a target for cache scoping, cost attribution, and
// session records. Two tiers bound to the same model with different inference
// parameters are different targets, so the ID must include those parameters
// wherever they change capability, price, context rendering, or cache identity
// (§3.1).
type RouteTargetID string

// RouteTarget is the unit of execution: provider, serving surface, model
// snapshot, and inference configuration. Cache state, price, and capability
// attach here rather than to a bare model name.
type RouteTarget struct {
	Provider string
	Surface  string
	ModelID  string
	Params   Params
}

// Params holds inference configuration. A nil pointer means "provider default",
// which is distinct from an explicit zero.
type Params struct {
	MaxOutputTokens int
	Temperature     *float64
	Reasoning       *Reasoning
}

// Reasoning requests thinking output. Effort is an unvalidated provider-level
// string here; adapters map it against what the target actually supports and
// return a CapabilityError when it does not, rather than quietly ignoring it.
type Reasoning struct {
	Enabled bool
	Effort  string
}

const routeTargetIDV2Prefix = "rt2:"

func (t RouteTarget) ID() RouteTargetID {
	if !targetIDNeedsV2(t) {
		// The default spelling remains byte-for-byte compatible, including model
		// names containing '/', '+', or '%'. Parameterized identities use the
		// disjoint slash-free v2 namespace below, so those literal bytes cannot
		// impersonate a parameter suffix.
		return RouteTargetID(fmt.Sprintf("%s/%s/%s", t.Provider, t.Surface, t.ModelID))
	}
	encode := base64.RawURLEncoding.EncodeToString
	temperature := "n"
	if t.Params.Temperature != nil {
		temperature = fmt.Sprintf("%016x", math.Float64bits(*t.Params.Temperature))
	}
	thinking := "n"
	effort := ""
	if t.Params.Reasoning != nil {
		thinking = "0"
		if t.Params.Reasoning.Enabled {
			thinking = "1"
		}
		effort = t.Params.Reasoning.Effort
	}
	return RouteTargetID(fmt.Sprintf(
		"%sp=%s&s=%s&m=%s&max=%d&temp=%s&think=%s&effort=%s",
		routeTargetIDV2Prefix, encode([]byte(t.Provider)), encode([]byte(t.Surface)), encode([]byte(t.ModelID)),
		t.Params.MaxOutputTokens, temperature, thinking, encode([]byte(effort))))
}

func targetIDNeedsV2(t RouteTarget) bool {
	return t.Params.MaxOutputTokens != 0 || t.Params.Temperature != nil || t.Params.Reasoning != nil ||
		strings.Contains(t.Provider, "/") || strings.Contains(t.Surface, "/") || legacyReasoningSuffix(t.ModelID)
}

// LegacyID is the pre-escaping identity spelling. It exists only to match
// already-recorded sessions against a configured target during migration;
// new cache, probe, and accounting keys must use ID.
func (t RouteTarget) LegacyID() RouteTargetID {
	id := fmt.Sprintf("%s/%s/%s", t.Provider, t.Surface, t.ModelID)
	if r := t.Params.Reasoning; r != nil && r.Enabled {
		if r.Effort != "" {
			id += "+think:" + r.Effort
		} else {
			id += "+think"
		}
	}
	return RouteTargetID(id)
}

// ParseRouteTargetID reverses ID. Keeping the parser next to the encoder
// prevents resume, catalog lookup, and evaluation from each inventing a
// different interpretation of '+' in a legitimate model name.
func ParseRouteTargetID(id RouteTargetID) (RouteTarget, error) {
	raw := string(id)
	if strings.HasPrefix(raw, routeTargetIDV2Prefix) && !strings.Contains(raw, "/") {
		return parseRouteTargetIDV2(id)
	}
	parts := strings.SplitN(raw, "/", 3)
	if len(parts) != 3 {
		return RouteTarget{}, fmt.Errorf("unreadable route target ID %q", raw)
	}
	if legacyReasoningSuffix(parts[2]) {
		return RouteTarget{}, fmt.Errorf(
			"unreadable route target ID %q: legacy +think spelling is ambiguous without the configured target", raw)
	}
	return RouteTarget{Provider: parts[0], Surface: parts[1], ModelID: parts[2]}, nil
}

func legacyReasoningSuffix(model string) bool {
	index := strings.LastIndex(model, "+think")
	if index < 0 {
		return false
	}
	suffix := model[index+len("+think"):]
	return suffix == "" || strings.HasPrefix(suffix, ":")
}

func parseRouteTargetIDV2(id RouteTargetID) (RouteTarget, error) {
	raw := strings.TrimPrefix(string(id), routeTargetIDV2Prefix)
	fields := strings.Split(raw, "&")
	wantKeys := []string{"p", "s", "m", "max", "temp", "think", "effort"}
	if len(fields) != len(wantKeys) {
		return RouteTarget{}, fmt.Errorf("unreadable route target ID %q: v2 field count", id)
	}
	values := make([]string, len(fields))
	for index, field := range fields {
		key, value, ok := strings.Cut(field, "=")
		if !ok || key != wantKeys[index] {
			return RouteTarget{}, fmt.Errorf("unreadable route target ID %q: expected v2 field %q", id, wantKeys[index])
		}
		values[index] = value
	}
	decode := func(name, value string) (string, error) {
		decoded, err := base64.RawURLEncoding.DecodeString(value)
		if err != nil {
			return "", fmt.Errorf("route target %s: %w", name, err)
		}
		return string(decoded), nil
	}
	providerName, err := decode("provider", values[0])
	if err != nil {
		return RouteTarget{}, err
	}
	surface, err := decode("surface", values[1])
	if err != nil {
		return RouteTarget{}, err
	}
	model, err := decode("model", values[2])
	if err != nil {
		return RouteTarget{}, err
	}
	maxOutput, err := strconv.ParseInt(values[3], 10, strconv.IntSize)
	if err != nil {
		return RouteTarget{}, fmt.Errorf("route target max output: %w", err)
	}
	target := RouteTarget{Provider: providerName, Surface: surface, ModelID: model}
	target.Params.MaxOutputTokens = int(maxOutput)
	if values[4] != "n" {
		bits, parseErr := strconv.ParseUint(values[4], 16, 64)
		if parseErr != nil || len(values[4]) != 16 {
			return RouteTarget{}, fmt.Errorf("route target temperature has invalid bits %q", values[4])
		}
		temperature := math.Float64frombits(bits)
		target.Params.Temperature = &temperature
	}
	effort, err := decode("reasoning effort", values[6])
	if err != nil {
		return RouteTarget{}, err
	}
	switch values[5] {
	case "n":
		if effort != "" {
			return RouteTarget{}, fmt.Errorf("route target has reasoning effort without explicit reasoning")
		}
	case "0", "1":
		target.Params.Reasoning = &Reasoning{Enabled: values[5] == "1", Effort: effort}
	default:
		return RouteTarget{}, fmt.Errorf("route target has invalid thinking state %q", values[5])
	}
	if target.ID() != id {
		return RouteTarget{}, fmt.Errorf("unreadable route target ID %q: non-canonical v2 encoding", id)
	}
	return target, nil
}

// Display is the human-readable target label. ID is intentionally an opaque,
// versioned machine key when parameters are explicit; user surfaces should
// render this label so routing remains inspectable.
func (t RouteTarget) Display() string {
	label := fmt.Sprintf("%s/%s/%s", displayTargetComponent(t.Provider), displayTargetComponent(t.Surface), displayModelComponent(t.ModelID))
	var params []string
	if reasoning := t.Params.Reasoning; reasoning != nil {
		thinking := "think"
		if !reasoning.Enabled {
			thinking = "think-off"
		}
		if reasoning.Effort != "" {
			thinking += ":" + url.QueryEscape(reasoning.Effort)
		}
		params = append(params, thinking)
	}
	if t.Params.MaxOutputTokens != 0 {
		params = append(params, "max:"+strconv.Itoa(t.Params.MaxOutputTokens))
	}
	if t.Params.Temperature != nil {
		temperature := strconv.FormatFloat(*t.Params.Temperature, 'g', -1, 64)
		// The shortest float spelling round-trips every finite value, including
		// signed zero. NaN payloads are the sole exception, so retain their bits.
		if math.IsNaN(*t.Params.Temperature) {
			temperature += fmt.Sprintf("@%016x", math.Float64bits(*t.Params.Temperature))
		}
		params = append(params, "temp:"+temperature)
	}
	if len(params) > 0 {
		label += " [" + strings.Join(params, ", ") + "]"
	}
	return label
}

// displayModelComponent renders the model, which is the last component and
// therefore absorbs everything after the second separator. A slash inside it
// is not ambiguous for a reader, and namespaced ids are the ordinary case on
// a compatible endpoint, so quoting every one of them adds noise to the row a
// user reads most often. Anything else a bare component may not contain is
// still quoted.
func displayModelComponent(value string) string {
	return displayComponent(value, "/")
}

func displayTargetComponent(value string) string {
	return displayComponent(value, "")
}

func displayComponent(value, alsoBare string) string {
	if value != "" && strings.IndexFunc(value, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_' || r == '.' || r == ':' || r == '@' ||
			strings.ContainsRune(alsoBare, r))
	}) < 0 {
		return value
	}
	return strconv.Quote(value)
}

func (t RouteTarget) String() string { return t.Display() }

// DisplayRouteTargetID renders an opaque recorded key for people. Legacy IDs
// that cannot be parsed without configuration are already human-readable, so
// they pass through unchanged instead of being guessed at.
func DisplayRouteTargetID(id RouteTargetID) string {
	target, err := ParseRouteTargetID(id)
	if err != nil {
		return string(id)
	}
	return target.Display()
}

// EffectiveOutputTokenReserve returns the maximum output allowance the
// concrete adapter will put on a request. A context window covers input and
// output together, so routing and the last pre-stream check must share these
// semantics rather than each guessing from catalog MaxOutput.
func EffectiveOutputTokenReserve(target RouteTarget, catalogMax int) int {
	requested := target.Params.MaxOutputTokens
	switch target.Provider {
	case "anthropic", "kimi":
		const messagesDefault = 8_192
		if requested <= 0 {
			requested = messagesDefault
		}
		if reasoning := target.Params.Reasoning; reasoning != nil && reasoning.Enabled {
			budget := 0
			switch reasoning.Effort {
			case "":
				budget = 4_096
			case "low":
				budget = 1_024
			case "medium":
				budget = 4_096
			case "high":
				budget = 16_384
			case "max":
				budget = 32_768
			default:
				return maxOutputReserve(requested, catalogMax)
			}
			// The Messages API requires max_tokens to clear the thinking budget;
			// the adapter raises a too-small/default request by this exact amount.
			if requested <= budget {
				requested = budget + messagesDefault
			}
		}
		return requested
	default:
		if requested > 0 {
			return requested
		}
		if catalogMax > 0 {
			return catalogMax
		}
		// An omitted adapter limit delegates to a server/model default. With no
		// catalog maximum, that is not bounded evidence and must fail closed for
		// any finite context window.
		return math.MaxInt
	}
}

func maxOutputReserve(a, b int) int {
	if a < 0 || b < 0 {
		return math.MaxInt
	}
	if a > b {
		return a
	}
	return b
}

// ToolSupport records how reliably a target handles tool calls. Serial versus
// parallel is a wire-format question; Unreliable is a measured judgment a probe
// can only hint at (§4).
type ToolSupport string

const (
	ToolsNone       ToolSupport = "none"
	ToolsSerial     ToolSupport = "serial"
	ToolsParallel   ToolSupport = "parallel"
	ToolsUnreliable ToolSupport = "unreliable"
)

// ProbeResult establishes API compatibility. It does not establish tool-calling
// quality, which needs evaluation rather than a single successful call (§4).
type ProbeResult struct {
	Reachable    bool
	ModelPresent bool
	Tools        ToolSupport
	Vision       bool

	// ContextWindow is what the server said it will accept for this model,
	// in tokens, or zero where the protocol offers no way to ask. A live
	// answer outranks a catalog default for the same reason a probed
	// capability does: the catalog describes a surface, the server knows the
	// model that is loaded on it.
	ContextWindow int

	// WindowEnforced marks the difference between a window the server will
	// hold a request to — an allocation or a per-request limit — and one
	// inferred from metadata fields that can contradict each other on the
	// same response. Only an enforced window outranks the number the user
	// declared for the surface: a heuristic that disagrees with the person
	// who configured the server is the heuristic's loss.
	WindowEnforced bool

	// EffortLevels are the reasoning efforts the server states this model
	// accepts, in the server's own order, or nil where discovery says
	// nothing about them. The same live-answer rule as ContextWindow: the
	// catalog's list describes a surface's floor, and the levels vary per
	// model on the surfaces that report them.
	EffortLevels []string

	Detail string
}

// TokenEstimate reports a count. Exact is false when the number came from a
// local approximation rather than the provider, so callers can widen budget
// margins accordingly.
type TokenEstimate struct {
	InputTokens int
	Exact       bool
}
