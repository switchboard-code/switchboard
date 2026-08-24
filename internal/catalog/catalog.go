// Package catalog is the source of truth for what each route target can do,
// how its requests are priced, and where those facts came from.
//
// Entries are keyed by provider and serving surface as well as model, because
// the same nominal model can have different IDs, cache behavior, features,
// prices, and retention rules on its first-party API, Bedrock, Vertex, or a
// proxy (§4).
//
// Every entry carries provenance. Provider documentation is versioned by
// editing in place: the numbers here were true on the date recorded and will
// drift with no diff and no notice, so a cost or route decision is only
// reproducible against the revision that produced it.
package catalog

import (
	"fmt"
	"math"
	"math/bits"
	"strconv"
	"strings"
	"time"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/provider/anthropic"
)

// Money is an amount in micro-USD, a millionth of a dollar.
//
// Integer units rather than a float because prices are quoted per million
// tokens and the arithmetic runs over token counts: at $5.00 per MTok, one
// token costs exactly 5 micro-USD, and no rounding enters until display.
type Money int64

const (
	MicroUSD Money = 1
	USD            = 1_000_000 * MicroUSD
	MaxMoney Money = Money(math.MaxInt64)
)

// PerMTok converts a dollars-per-million-tokens quote to Money.
func PerMTok(dollars float64) Money {
	m, ok := moneyFromDollars(dollars)
	if !ok {
		return MaxMoney
	}
	return m
}

// Cost prices a token count against a per-million-token rate.
func (m Money) Cost(tokens int) Money {
	cost, ok := checkedTokenCost(m, tokens)
	if !ok {
		return MaxMoney
	}
	return cost
}

// checkedTokenCost multiplies before dividing without ever squeezing the
// intermediate product into an int64. Provider prices can be large enough for
// rate*tokens to overflow even though the final per-million-token cost fits.
func checkedTokenCost(rate Money, tokens int) (Money, bool) {
	if rate < 0 || tokens < 0 {
		return MaxMoney, false
	}

	hi, lo := bits.Mul64(uint64(rate), uint64(tokens))
	const perMillion = uint64(1_000_000)
	if hi >= perMillion {
		return MaxMoney, false
	}
	quotient, _ := bits.Div64(hi, lo, perMillion)
	if quotient > math.MaxInt64 {
		return MaxMoney, false
	}
	return Money(quotient), true
}

func checkedMoneyAdd(a, b Money) (Money, bool) {
	if a < 0 || b < 0 || b > MaxMoney-a {
		return MaxMoney, false
	}
	return a + b, true
}

func saturatingMoneyAdd(a, b Money) Money {
	total, ok := checkedMoneyAdd(a, b)
	if !ok {
		return MaxMoney
	}
	return total
}

func moneyFromDollars(dollars float64) (Money, bool) {
	if math.IsNaN(dollars) || math.IsInf(dollars, 0) || dollars < 0 {
		return 0, false
	}
	scaled := dollars * float64(USD)
	// float64(math.MaxInt64) rounds up to 2^63, so equality is already out of
	// range for a conversion to Money.
	if math.IsInf(scaled, 0) || scaled >= float64(math.MaxInt64) {
		return 0, false
	}
	return Money(math.Round(scaled)), true
}

// UnmarshalText reads a dollar amount written as a plain decimal string, so
// the catalog file reads "5.00" rather than 5000000.
func (m *Money) UnmarshalText(text []byte) error {
	s := strings.TrimPrefix(strings.TrimSpace(string(text)), "$")
	dollars, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("price %q is not a decimal amount: %w", text, err)
	}
	parsed, ok := moneyFromDollars(dollars)
	if !ok {
		return fmt.Errorf("price %q is negative or outside the supported range", text)
	}
	*m = parsed
	return nil
}

func (m Money) MarshalText() ([]byte, error) {
	return fmt.Appendf(nil, "%.6f", float64(m)/float64(USD)), nil
}

// String formats an amount for display. A single turn usually costs a fraction
// of a cent, so precision widens below a dollar rather than rounding a real
// charge away; a charge too small to show at all says so instead of printing
// as zero, because a user reading "$0.00" concludes they were not billed.
func (m Money) String() string {
	switch {
	case m == 0:
		return "$0.00"
	case m < 0:
		return "-" + (-m).String()
	case m < 100:
		return "<$0.0001"
	case m < USD:
		return fmt.Sprintf("$%.4f", float64(m)/float64(USD))
	default:
		return fmt.Sprintf("$%.2f", float64(m)/float64(USD))
	}
}

type ToolSupport string

const (
	ToolsNone     ToolSupport = "none"
	ToolsSerial   ToolSupport = "serial"
	ToolsParallel ToolSupport = "parallel"
)

type ReasoningSupport string

const (
	ReasoningNone     ReasoningSupport = "none"
	ReasoningBudget   ReasoningSupport = "budget"
	ReasoningAdaptive ReasoningSupport = "adaptive"
	ReasoningAlways   ReasoningSupport = "always"
)

type CacheMode string

const (
	CacheNone      CacheMode = "none"
	CacheAutomatic CacheMode = "automatic"
	CacheImplicit  CacheMode = "implicit"
	CacheExplicit  CacheMode = "explicit"
)

// UsageAccounting records how a surface reports cache activity, because a
// tracker that assumes one shape will silently believe in a cache it has never
// observed (§6.3).
type UsageAccounting string

const (
	// AccountingSeparate reports cache reads and writes in their own fields,
	// excluded from the uncached input count.
	AccountingSeparate UsageAccounting = "separate"

	// AccountingNone reports nothing about caching. A target like this cannot
	// exercise the §6 cache economics at all.
	AccountingNone UsageAccounting = "none"
)

// CachePolicy is an object rather than a set of flags because read and write
// accounting, defaults, TTL guarantees, and breakpoint semantics differ by
// surface, and flattening them loses exactly what the router needs.
type CachePolicy struct {
	Modes       []CacheMode `toml:"modes"`
	DefaultMode CacheMode   `toml:"default_mode"`

	// MinTokens is the shortest prefix the surface will cache. Below it a
	// breakpoint is accepted and silently does nothing, with no error either
	// way, which is why the number has to be data rather than a constant.
	MinTokens int `toml:"min_tokens"`

	TTLs           []string `toml:"ttls"`
	MaxBreakpoints int      `toml:"max_breakpoints"`

	// LookbackBlocks is how far back the surface searches for a reusable
	// prefix. A long tool-use turn can cross it.
	LookbackBlocks int `toml:"lookback_blocks"`

	RoutingKeySupport bool            `toml:"routing_key_support"`
	UsageAccounting   UsageAccounting `toml:"usage_accounting"`
}

// PriceBand is one price regime. Pricing is a list because it changes at
// context thresholds, by processing mode, region, and effective date, and
// flattening it into a single rate produces confidently wrong estimates.
type PriceBand struct {
	EffectiveAt time.Time `toml:"effective_at"`

	// MaxInputTokens bounds the band. Zero means no upper bound.
	MaxInputTokens int `toml:"max_input_tokens"`

	ProcessingMode string `toml:"processing_mode"`
	Region         string `toml:"region"`

	InputPerMTok     Money `toml:"input_per_mtok"`
	OutputPerMTok    Money `toml:"output_per_mtok"`
	CacheReadPerMTok Money `toml:"cache_read_per_mtok"`

	// CacheWritePerMTok is keyed by TTL because the write multiplier differs
	// per TTL and per provider. It is not a universal constant.
	CacheWritePerMTok map[string]Money `toml:"cache_write_per_mtok"`
}

type ModelInfo struct {
	Provider        string `toml:"provider"`
	Surface         string `toml:"surface"`
	ProviderModelID string `toml:"provider_model_id"`
	DisplayName     string `toml:"display_name"`
	Snapshot        string `toml:"snapshot"`

	ContextWindow int `toml:"context_window"`
	MaxOutput     int `toml:"max_output"`

	Pricing []PriceBand `toml:"pricing"`
	Cache   CachePolicy `toml:"cache"`

	// Metering says what a turn actually consumes. Three entries now price at
	// zero per token for three different reasons, and the difference decides
	// what a router is optimizing: a local model consumes nothing scarce, a
	// plan consumes quota, and a metered target consumes money. Reporting all
	// three as "free" would tell a router the wrong thing about two of them.
	Metering Metering `toml:"metering"`

	Tools         ToolSupport      `toml:"tools"`
	StrictSchema  bool             `toml:"strict_schema"`
	Vision        bool             `toml:"vision"`
	Reasoning     ReasoningSupport `toml:"reasoning"`
	EffortLevels  []string         `toml:"effort_levels"`
	StructuredOut bool             `toml:"structured_out"`

	// Provenance. Without these a historical cost cannot be reconstructed
	// against the data that actually produced it.
	VerifiedAt time.Time `toml:"verified_at"`
	SourceURLs []string  `toml:"source_urls"`
}

// ID is the catalog key, matching provider.RouteTarget.ID minus inference
// parameters: price and capability attach to the model on a surface, while
// effort changes cache identity without changing the price sheet.
func (m ModelInfo) ID() string {
	return fmt.Sprintf("%s/%s/%s", m.Provider, m.Surface, m.ProviderModelID)
}

// Band selects the price regime for a request of a given size.
//
// The largest applicable bound wins so that an entry can list a cheap band up
// to a threshold and a dearer one above it without ordering mattering.
func (m ModelInfo) Band(inputTokens int) (PriceBand, bool) {
	var best PriceBand
	found := false
	for _, b := range m.Pricing {
		if b.MaxInputTokens != 0 && inputTokens > b.MaxInputTokens {
			continue
		}
		if !found || bandNarrower(b, best) {
			best, found = b, true
		}
	}
	return best, found
}

// bandNarrower prefers the tightest bound that still admits the request, so a
// long-context premium applies only once the request actually crosses into it.
func bandNarrower(candidate, current PriceBand) bool {
	if current.MaxInputTokens == 0 {
		return candidate.MaxInputTokens != 0
	}
	if candidate.MaxInputTokens == 0 {
		return false
	}
	return candidate.MaxInputTokens < current.MaxInputTokens
}

// Cost prices observed usage. It reports which band produced the number,
// because an estimate that cannot name its band is not reproducible.
func (m ModelInfo) Cost(u provider.Usage) (Money, PriceBand, bool) {
	billable := saturatingTokenSum(u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	band, ok := m.Band(billable)
	if !ok {
		return 0, PriceBand{}, false
	}

	total := band.InputPerMTok.Cost(u.InputTokens)
	total = saturatingMoneyAdd(total, band.OutputPerMTok.Cost(u.OutputTokens))
	total = saturatingMoneyAdd(total, band.CacheReadPerMTok.Cost(u.CacheReadTokens))

	// Writes are billed at the TTL actually used. Without a recorded TTL the
	// shortest is the conservative assumption, since it is the cheapest and
	// under-reporting a charge is the failure that matters here.
	if u.CacheWriteTokens > 0 {
		total = saturatingMoneyAdd(total, cheapestWrite(band).Cost(u.CacheWriteTokens))
	}
	return total, band, true
}

func saturatingTokenSum(tokens ...int) int {
	total := 0
	for _, count := range tokens {
		if count < 0 || count > math.MaxInt-total {
			return math.MaxInt
		}
		total += count
	}
	return total
}

func cheapestWrite(b PriceBand) Money {
	var cheapest Money
	first := true
	for _, price := range b.CacheWritePerMTok {
		if first || price < cheapest {
			cheapest, first = price, false
		}
	}
	return cheapest
}

// Metering names what a turn draws down.
type Metering string

const (
	// PerToken is the ordinary case: a turn costs money, priced by the bands.
	PerToken Metering = "per-token"

	// Local means nothing meters it. Such a target cannot exercise the §6 cache
	// economics at all, whatever it reports.
	Local Metering = "local"

	// Plan means a flat subscription pays for it and quota is the scarce
	// resource. Nothing here models quota yet, so a cost estimate against a
	// plan target is correct at zero and silent about what actually runs out.
	Plan Metering = "plan"
)

func (m Metering) String() string {
	if m == "" {
		return string(PerToken)
	}
	return string(m)
}

// Free reports whether every band in the entry costs nothing. It is about
// money only: a plan target is free per token and still finite.
func (m ModelInfo) Free() bool {
	for _, b := range m.Pricing {
		if b.InputPerMTok != 0 || b.OutputPerMTok != 0 || b.CacheReadPerMTok != 0 {
			return false
		}
		for _, price := range b.CacheWritePerMTok {
			if price != 0 {
				return false
			}
		}
	}
	return true
}

// Confidence separates a verified entry from a guess. An unknown target is
// usable but must stay visibly low-confidence until evidence exists (§8.2).
type Confidence string

const (
	// Verified means the entry came from the catalog.
	Verified Confidence = "verified"

	// Prior means nothing in the catalog matched and a surface default was
	// substituted. Its numbers are shape, not fact.
	Prior Confidence = "prior"
)

// Catalog is a revision of the target data.
type Catalog struct {
	Revision string
	Source   string

	entries  map[string]ModelInfo
	defaults map[string]ModelInfo
}

func (c *Catalog) Lookup(target provider.RouteTarget) (ModelInfo, Confidence, bool) {
	key := fmt.Sprintf("%s/%s/%s", target.Provider, target.Surface, target.ModelID)
	if info, ok := c.entries[key]; ok {
		return info, Verified, true
	}
	if info, ok := c.snapshotLookup(target); ok {
		return info, Verified, true
	}

	// A surface default carries the mechanics that hold for everything served
	// there, which is what makes an unpulled or brand-new local model usable
	// without pretending it is known.
	if info, ok := c.defaults[target.Provider+"/"+target.Surface]; ok {
		info.ProviderModelID = target.ModelID
		info.DisplayName = target.ModelID
		return info, Prior, true
	}
	return ModelInfo{}, Prior, false
}

// snapshotLookup carries an alias's verified evidence onto the exact dated
// snapshot the provider listed. It never rewrites the target back to the alias:
// pricing and capabilities may be shared, while serving identity, cache keys,
// session records, and provider requests must continue to name the snapshot.
func (c *Catalog) snapshotLookup(target provider.RouteTarget) (ModelInfo, bool) {
	if c == nil {
		return ModelInfo{}, false
	}

	// An explicitly recorded snapshot is the strongest relation. Search in a
	// stable order so a malformed override containing duplicates cannot make a
	// lookup depend on Go's randomized map iteration.
	var explicit ModelInfo
	for _, info := range c.entries {
		if info.Provider != target.Provider || info.Surface != target.Surface || info.Snapshot != target.ModelID {
			continue
		}
		if explicit.Provider == "" || info.ID() < explicit.ID() {
			explicit = info
		}
	}
	if explicit.Provider != "" {
		return snapshotIdentity(explicit, target.ModelID), true
	}

	if target.Provider != anthropic.Name || target.Surface != anthropic.Surface {
		return ModelInfo{}, false
	}
	alias, ok := anthropic.AdaptiveAlias(target.ModelID)
	if !ok || alias == target.ModelID {
		return ModelInfo{}, false
	}
	key := fmt.Sprintf("%s/%s/%s", target.Provider, target.Surface, alias)
	info, ok := c.entries[key]
	if !ok {
		return ModelInfo{}, false
	}
	return snapshotIdentity(info, target.ModelID), true
}

func snapshotIdentity(info ModelInfo, modelID string) ModelInfo {
	info.ProviderModelID = modelID
	info.Snapshot = modelID
	return info
}

func (c *Catalog) Len() int { return len(c.entries) }

// Surfaces lists the explicit surface defaults. A concrete model is evidence
// about that model, not a surface floor: returning an arbitrary concrete entry
// here made per-model reasoning dialects and output limits depend on randomized
// map iteration. Callers that merely need to enumerate every known provider /
// surface pair can combine this list with Entries while keeping those concrete
// capabilities attached to their models.
func (c *Catalog) Surfaces() []ModelInfo {
	out := make([]ModelInfo, 0, len(c.defaults))
	for _, info := range c.defaults {
		out = append(out, info)
	}
	sortByID(out)
	return out
}

// Entries returns every verified entry, sorted by ID.
func (c *Catalog) Entries() []ModelInfo {
	out := make([]ModelInfo, 0, len(c.entries))
	for _, info := range c.entries {
		out = append(out, info)
	}
	sortByID(out)
	return out
}

func sortByID(entries []ModelInfo) {
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].ID() < entries[j-1].ID(); j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}

// Describe renders an entry's provenance for the UI, so a user can see how old
// the number behind a cost estimate is.
func (m ModelInfo) Describe() string {
	var b strings.Builder
	b.WriteString(m.ID())
	if !m.VerifiedAt.IsZero() {
		fmt.Fprintf(&b, " verified %s", m.VerifiedAt.Format("2006-01-02"))
	}
	if len(m.SourceURLs) > 0 {
		fmt.Fprintf(&b, " from %s", m.SourceURLs[0])
	}
	return b.String()
}
