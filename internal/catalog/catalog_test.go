package catalog

import (
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func load(t *testing.T) *Catalog {
	t.Helper()
	c, err := LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestBundledCatalogLoads(t *testing.T) {
	c := load(t)
	if c.Len() == 0 {
		t.Fatal("the bundled catalog is empty")
	}
	// A revision that does not change with the content cannot pin a recorded
	// cost to the data that produced it.
	if !strings.Contains(c.Revision, "+") {
		t.Errorf("revision %q carries no content fingerprint", c.Revision)
	}
	for _, m := range c.Entries() {
		if m.VerifiedAt.IsZero() {
			t.Errorf("%s has no verification date", m.ID())
		}
		if len(m.SourceURLs) == 0 {
			t.Errorf("%s cites no source", m.ID())
		}
	}
}

func TestLookupKnownTarget(t *testing.T) {
	c := load(t)
	target := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"}

	info, confidence, ok := c.Lookup(target)
	if !ok {
		t.Fatal("claude-opus-5 is not in the bundled catalog")
	}
	if confidence != Verified {
		t.Errorf("confidence = %s, want verified", confidence)
	}
	if info.ContextWindow != 1_000_000 {
		t.Errorf("context window = %d", info.ContextWindow)
	}
	if info.Free() {
		t.Error("a paid model reported itself free")
	}
}

func TestLookupDatedSnapshotCarriesAliasEvidenceWithoutCollapsingIdentity(t *testing.T) {
	c := load(t)
	for _, tc := range []struct {
		alias    string
		snapshot string
	}{
		{alias: "claude-opus-5", snapshot: "claude-opus-5-20260824"},
		{alias: "claude-sonnet-5", snapshot: "claude-sonnet-5-20261231"},
		{alias: "claude-haiku-4-5", snapshot: "claude-haiku-4-5-20251001"},
	} {
		t.Run(tc.snapshot, func(t *testing.T) {
			aliasTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: tc.alias}
			snapshotTarget := provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: tc.snapshot}
			aliasInfo, _, ok := c.Lookup(aliasTarget)
			if !ok {
				t.Fatalf("alias %q is absent", tc.alias)
			}
			info, confidence, ok := c.Lookup(snapshotTarget)
			if !ok || confidence != Verified {
				t.Fatalf("snapshot lookup = present %v confidence %q, want verified alias evidence", ok, confidence)
			}
			if info.ProviderModelID != tc.snapshot || info.ID() != string(snapshotTarget.ID()) {
				t.Fatalf("snapshot identity collapsed to alias: model=%q id=%q want %q", info.ProviderModelID, info.ID(), snapshotTarget.ID())
			}
			if info.Snapshot != tc.snapshot {
				t.Fatalf("resolved snapshot marker = %q, want %q", info.Snapshot, tc.snapshot)
			}
			if info.MaxOutput != aliasInfo.MaxOutput || info.Reasoning != aliasInfo.Reasoning ||
				strings.Join(info.EffortLevels, ",") != strings.Join(aliasInfo.EffortLevels, ",") {
				t.Fatalf("snapshot lost alias evidence:\n snapshot=%+v\n alias=%+v", info, aliasInfo)
			}
		})
	}
}

func TestSnapshotLookupRejectsNearPrefixesAndMalformedDates(t *testing.T) {
	c := load(t)
	for _, model := range []string{
		"claude-opus-20260824",
		"claude-opus-5x-20260824",
		"claude-opus-5-2026082",
		"claude-opus-5-20260230",
		"claude-opus-5-20260824-preview",
		// Haiku is a verified budget-dialect alias, not one of the four aliases
		// whose arbitrary canonical snapshots were live-verified. Only its exact
		// Snapshot field value above may inherit catalog evidence.
		"claude-haiku-4-5-20260824",
	} {
		t.Run(model, func(t *testing.T) {
			_, _, ok := c.Lookup(provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: model})
			if ok {
				t.Fatalf("malformed or unrelated snapshot %q inherited verified alias evidence", model)
			}
		})
	}
}

func TestDatedSnapshotInferenceIsNotGenericCatalogPrefixMatching(t *testing.T) {
	entry := ModelInfo{Provider: "acme", Surface: "cloud", ProviderModelID: "model-pro"}
	c := &Catalog{entries: map[string]ModelInfo{entry.ID(): entry}}
	for _, model := range []string{"model-pro-20260824", "model-pro-preview-20260824"} {
		if _, _, ok := c.Lookup(provider.RouteTarget{Provider: "acme", Surface: "cloud", ModelID: model}); ok {
			t.Fatalf("unrelated provider snapshot %q inherited an alias by spelling convention alone", model)
		}
	}
}

func TestSurfacesAreOnlyExplicitDefaultsAndDeterministic(t *testing.T) {
	defaults := []ModelInfo{
		{Provider: "zeta", Surface: "two", DisplayName: "zeta default"},
		{Provider: "alpha", Surface: "one", DisplayName: "alpha default"},
		{Provider: "middle", Surface: "three", DisplayName: "middle default"},
	}
	entries := []ModelInfo{
		{Provider: "anthropic", Surface: "first-party", ProviderModelID: "claude-opus-5", EffortLevels: []string{"xhigh"}, MaxOutput: 128000},
		{Provider: "entry-only", Surface: "cloud", ProviderModelID: "model", EffortLevels: []string{"wrong-floor"}, MaxOutput: 99},
	}
	want := "alpha/one/,middle/three/,zeta/two/"

	random := rand.New(rand.NewSource(42))
	for run := 0; run < 100; run++ {
		c := &Catalog{defaults: map[string]ModelInfo{}, entries: map[string]ModelInfo{}}
		for _, index := range random.Perm(len(defaults)) {
			info := defaults[index]
			c.defaults[info.Provider+"/"+info.Surface] = info
		}
		for _, index := range random.Perm(len(entries)) {
			info := entries[index]
			c.entries[info.ID()] = info
		}
		got := c.Surfaces()
		ids := make([]string, len(got))
		for i, info := range got {
			ids[i] = info.ID()
			if info.Provider == "anthropic" || info.Provider == "entry-only" {
				t.Fatalf("run %d promoted concrete model evidence to a surface default: %+v", run, info)
			}
		}
		if joined := strings.Join(ids, ","); joined != want {
			t.Fatalf("run %d surfaces = %q, want stable %q", run, joined, want)
		}
	}
}

// A model the catalog has never seen still has to be usable, and has to say
// that its numbers are shape rather than fact (§8.2).
func TestUnknownModelFallsBackToSurfacePrior(t *testing.T) {
	c := load(t)
	target := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "something-pulled-yesterday"}

	info, confidence, ok := c.Lookup(target)
	if !ok {
		t.Fatal("the ollama surface has no default entry")
	}
	if confidence != Prior {
		t.Errorf("confidence = %s, want prior", confidence)
	}
	if info.ProviderModelID != "something-pulled-yesterday" {
		t.Errorf("the prior did not adopt the requested model id: %q", info.ProviderModelID)
	}
	if !info.Free() {
		t.Error("a local model should cost nothing to run")
	}
	if info.Cache.UsageAccounting != AccountingNone {
		t.Error("Ollama reports no cache accounting; claiming otherwise would let the estimator believe in a cache it cannot see")
	}
}

// One model served two ways is two targets. They price the same today because
// both are free, but they are reached by different adapters with different
// capability evidence, so collapsing them would attach one surface's catalog
// entry to the other's traffic.
func TestCompatibilityEndpointIsItsOwnTarget(t *testing.T) {
	c := load(t)
	const model = "qwen3.5:9b-mlx"
	native := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: model}
	compat := provider.RouteTarget{Provider: "openaicompat", Surface: "ollama", ModelID: model}

	if native.ID() == compat.ID() {
		t.Fatal("the same model through two adapters produced one target id")
	}

	info, confidence, ok := c.Lookup(compat)
	if !ok {
		t.Fatal("the openaicompat/ollama surface has no default entry, so a tier binding it cannot be priced")
	}
	if confidence != Prior {
		t.Errorf("confidence = %s, want prior", confidence)
	}
	if !info.Free() {
		t.Error("a local model costs nothing however it is reached")
	}
	if info.Cache.UsageAccounting != AccountingNone {
		t.Error("this endpoint never populates prompt_tokens_details, so no cache accounting can be claimed")
	}
}

func TestUnknownProviderIsNotInvented(t *testing.T) {
	c := load(t)
	_, _, ok := c.Lookup(provider.RouteTarget{Provider: "acme", Surface: "cloud", ModelID: "x"})
	if ok {
		t.Error("a provider with no catalog entry and no surface default must not resolve")
	}
}

// Reproduces the worked example in §6.4, which is the whole reason the cache
// fields exist: the cheaper model can cost more for the turn.
func TestCacheInversionFromTheDesign(t *testing.T) {
	c := load(t)

	opus, _, _ := c.Lookup(provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"})
	haiku, _, _ := c.Lookup(provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"})

	const prefix = 80_000

	warmRead, _, ok := opus.Cost(provider.Usage{CacheReadTokens: prefix})
	if !ok {
		t.Fatal("no price band matched an 80k read")
	}
	coldWrite, _, ok := haiku.Cost(provider.Usage{CacheWriteTokens: prefix})
	if !ok {
		t.Fatal("no price band matched an 80k write")
	}

	if got, want := warmRead, Money(40_000); got != want {
		t.Errorf("warm Opus read = %s (%d), want %s", got, got, want)
	}
	if got, want := coldWrite, Money(100_000); got != want {
		t.Errorf("cold Haiku write = %s (%d), want %s", got, got, want)
	}
	if coldWrite <= warmRead {
		t.Errorf("the inversion did not reproduce: reading a warm prefix on the expensive model (%s) "+
			"should cost less than warming the cheap one (%s)", warmRead, coldWrite)
	}
}

// The design calls this out specifically: the minimum is not monotonic across
// generations, so a 3,000 token prefix caches on one model and silently does
// not on another, with no error in either direction.
func TestCacheMinimumsAreNotMonotonic(t *testing.T) {
	c := load(t)
	minimum := func(model string) int {
		info, _, ok := c.Lookup(provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: model})
		if !ok {
			t.Fatalf("%s missing from the catalog", model)
		}
		return info.Cache.MinTokens
	}

	opus5 := minimum("claude-opus-5")
	haiku := minimum("claude-haiku-4-5")

	if opus5 >= haiku {
		t.Errorf("expected the newer, larger model to have the smaller minimum: opus-5=%d haiku-4.5=%d", opus5, haiku)
	}

	const prefix = 3000
	if !(prefix > opus5 && prefix < haiku) {
		t.Errorf("a %d token prefix should cache on opus-5 (min %d) and not on haiku (min %d)", prefix, opus5, haiku)
	}
}

func TestCostArithmetic(t *testing.T) {
	c := load(t)
	info, _, _ := c.Lookup(provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"})

	got, band, ok := info.Cost(provider.Usage{InputTokens: 1000, OutputTokens: 500})
	if !ok {
		t.Fatal("no band matched")
	}
	// 1000 in at $5/MTok = $0.005; 500 out at $25/MTok = $0.0125.
	if want := Money(17_500); got != want {
		t.Errorf("cost = %d micro-USD, want %d", got, want)
	}
	if band.InputPerMTok != PerMTok(5) {
		t.Errorf("band input rate = %s", band.InputPerMTok)
	}
	if got.String() != "$0.0175" {
		t.Errorf("display = %s, want $0.0175", got.String())
	}
}

func TestCostArithmeticDoesNotOverflowBeforeDivision(t *testing.T) {
	const hugeRate = Money(5_000_000_000_000_000_000)
	for tokens, want := range map[int]Money{
		2: 10_000_000_000_000,
		4: 20_000_000_000_000,
	} {
		if got := hugeRate.Cost(tokens); got != want {
			t.Errorf("rate %d for %d tokens = %d, want %d", hugeRate, tokens, got, want)
		}
	}
}

func TestCostArithmeticSaturatesInsteadOfWrapping(t *testing.T) {
	info := ModelInfo{Pricing: []PriceBand{{
		InputPerMTok:  Money(5_000_000_000_000_000_000),
		OutputPerMTok: Money(5_000_000_000_000_000_000),
	}}}

	got, _, ok := info.Cost(provider.Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000})
	if !ok {
		t.Fatal("no band matched")
	}
	if got != MaxMoney {
		t.Fatalf("overflowing cumulative cost = %d, want saturation at %d", got, MaxMoney)
	}

	if got := Money(math.MaxInt64).Cost(math.MaxInt); got != MaxMoney {
		t.Fatalf("overflowing multiplication = %d, want saturation at %d", got, MaxMoney)
	}
}

func TestLocalModelCostsNothing(t *testing.T) {
	c := load(t)
	info, _, _ := c.Lookup(provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "qwen3.5:9b-mlx"})

	got, _, ok := info.Cost(provider.Usage{InputTokens: 500_000, OutputTokens: 100_000})
	if !ok {
		t.Fatal("no band matched")
	}
	if got != 0 {
		t.Errorf("cost = %s, want nothing for a local model", got)
	}
}

func TestPriceBandSelection(t *testing.T) {
	info := ModelInfo{
		Pricing: []PriceBand{
			{MaxInputTokens: 200_000, InputPerMTok: PerMTok(3)},
			{MaxInputTokens: 0, InputPerMTok: PerMTok(6)}, // long-context premium
		},
	}

	short, ok := info.Band(100_000)
	if !ok || short.InputPerMTok != PerMTok(3) {
		t.Errorf("a short request took the %s band, want the $3 band", short.InputPerMTok)
	}
	long, ok := info.Band(500_000)
	if !ok || long.InputPerMTok != PerMTok(6) {
		t.Errorf("a long request took the %s band, want the $6 premium band", long.InputPerMTok)
	}
}

func TestMoneyParsing(t *testing.T) {
	for in, want := range map[string]Money{
		"5.00": 5_000_000,
		"$3":   3_000_000,
		"0.50": 500_000,
		"0.10": 100_000,
		"1.25": 1_250_000,
		"0":    0,
	} {
		var m Money
		if err := m.UnmarshalText([]byte(in)); err != nil {
			t.Errorf("parsing %q: %v", in, err)
			continue
		}
		if m != want {
			t.Errorf("%q = %d micro-USD, want %d", in, m, want)
		}
	}

	var m Money
	if err := m.UnmarshalText([]byte("free")); err == nil {
		t.Error("a price that is not a number must be an error, not zero")
	}
	for _, invalid := range []string{"-1", "NaN", "+Inf", "10000000000000"} {
		if err := m.UnmarshalText([]byte(invalid)); err == nil {
			t.Errorf("out-of-range price %q was accepted", invalid)
		}
	}
}

// An entry with no price silently bills every request at nothing, which is
// worse than a missing entry.
func TestEntryWithoutPricingIsRejected(t *testing.T) {
	err := validate(ModelInfo{Provider: "p", Surface: "s", ProviderModelID: "m"})
	if err == nil || !strings.Contains(err.Error(), "pricing") {
		t.Errorf("err = %v, want a complaint about missing pricing", err)
	}
}

func TestEntryWithoutVerificationDateIsRejected(t *testing.T) {
	err := validate(ModelInfo{
		Provider: "p", Surface: "s", ProviderModelID: "m",
		Pricing: []PriceBand{{InputPerMTok: PerMTok(1)}},
	})
	if err == nil || !strings.Contains(err.Error(), "verified_at") {
		t.Errorf("err = %v, want a complaint about the missing verification date", err)
	}
}

func validPaidEntry() ModelInfo {
	return ModelInfo{
		Provider: "p", Surface: "s", ProviderModelID: "m",
		ContextWindow: 1_000_000,
		MaxOutput:     1,
		Pricing:       []PriceBand{{InputPerMTok: PerMTok(1), OutputPerMTok: PerMTok(1)}},
		VerifiedAt:    time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC),
	}
}

func TestNegativeCatalogRangesAreRejected(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ModelInfo)
	}{
		{"context window", func(m *ModelInfo) { m.ContextWindow = -1 }},
		{"max output", func(m *ModelInfo) { m.MaxOutput = -1 }},
		{"band limit", func(m *ModelInfo) { m.Pricing[0].MaxInputTokens = -1 }},
		{"input price", func(m *ModelInfo) { m.Pricing[0].InputPerMTok = -1 }},
		{"output price", func(m *ModelInfo) { m.Pricing[0].OutputPerMTok = -1 }},
		{"cache read price", func(m *ModelInfo) { m.Pricing[0].CacheReadPerMTok = -1 }},
		{"cache write price", func(m *ModelInfo) { m.Pricing[0].CacheWritePerMTok = map[string]Money{"5m": -1} }},
		{"cache minimum", func(m *ModelInfo) { m.Cache.MinTokens = -1 }},
		{"cache breakpoints", func(m *ModelInfo) { m.Cache.MaxBreakpoints = -1 }},
		{"cache lookback", func(m *ModelInfo) { m.Cache.LookbackBlocks = -1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry := validPaidEntry()
			tc.mutate(&entry)
			if err := validate(entry); err == nil || !strings.Contains(err.Error(), "negative") {
				t.Fatalf("validate() = %v, want a negative-range error", err)
			}
		})
	}
}

func TestPaidPerTokenEntryNeedsPositiveMaxOutput(t *testing.T) {
	entry := validPaidEntry()
	entry.MaxOutput = 0
	if err := validate(entry); err == nil || !strings.Contains(err.Error(), "max_output") {
		t.Fatalf("validate() = %v, want a max_output error", err)
	}
}

func TestUnrepresentableCatalogCostIsRejected(t *testing.T) {
	entry := validPaidEntry()
	entry.ContextWindow = 1_000_000
	entry.MaxOutput = 1_000_000
	entry.Pricing[0].InputPerMTok = Money(5_000_000_000_000_000_000)
	entry.Pricing[0].OutputPerMTok = Money(5_000_000_000_000_000_000)
	if err := validate(entry); err == nil || !strings.Contains(err.Error(), "representable") {
		t.Fatalf("validate() = %v, want a representable-range error", err)
	}
}

func TestLargeIntermediateWithRepresentableCostIsAccepted(t *testing.T) {
	entry := validPaidEntry()
	entry.ContextWindow = 0
	entry.MaxOutput = 2
	entry.Pricing[0].InputPerMTok = 0
	entry.Pricing[0].OutputPerMTok = Money(5_000_000_000_000_000_000)
	if err := validate(entry); err != nil {
		t.Fatalf("representable price was rejected: %v", err)
	}
}

// Local edits must be visible in the revision, or a cost reconstructed from a
// session record would be checked against data that never produced it.
func TestUserOverrideChangesTheRevision(t *testing.T) {
	c := load(t)
	before := c.Revision

	dir := t.TempDir()
	path := filepath.Join(dir, UserOverrideFile)
	override := `
[[model]]
provider = "anthropic"
surface = "first-party"
provider_model_id = "claude-opus-5"
display_name = "Claude Opus 5 (negotiated rate)"
context_window = 1000000
max_output = 128000
verified_at = 2026-08-13T00:00:00Z
source_urls = ["local override"]

  [[model.pricing]]
  effective_at = 2026-08-13T00:00:00Z
  input_per_mtok = "2.50"
  output_per_mtok = "12.50"
`
	if err := os.WriteFile(path, []byte(override), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.applyOverrides(path); err != nil {
		t.Fatal(err)
	}

	if c.Revision == before {
		t.Error("a local override left the revision unchanged")
	}
	if !strings.Contains(c.Revision, "user.") {
		t.Errorf("revision %q does not record that local edits are in play", c.Revision)
	}
	if c.Source != "bundled+user" {
		t.Errorf("source = %q", c.Source)
	}

	info, _, _ := c.Lookup(provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-opus-5"})
	band, _ := info.Band(1000)
	if band.InputPerMTok != PerMTok(2.5) {
		t.Errorf("override did not take effect: input rate = %s", band.InputPerMTok)
	}
}

func TestMalformedOverrideIsAnError(t *testing.T) {
	c := load(t)
	dir := t.TempDir()
	path := filepath.Join(dir, UserOverrideFile)
	if err := os.WriteFile(path, []byte("this is not toml ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.applyOverrides(path); err == nil {
		t.Error("a malformed override must fail loudly rather than be skipped")
	}
}

func TestMissingOverrideFileIsFine(t *testing.T) {
	c := load(t)
	if err := c.applyOverrides(filepath.Join(t.TempDir(), "absent.toml")); err != nil {
		t.Errorf("an absent override file is the normal case: %v", err)
	}
}

func TestMoneyDisplay(t *testing.T) {
	for amount, want := range map[Money]string{
		0:          "$0.00",
		50:         "<$0.0001", // a real charge too small to render
		17_500:     "$0.0175",  // one turn
		1_000_000:  "$1.00",
		12_345_678: "$12.35",
	} {
		if got := amount.String(); got != want {
			t.Errorf("%d micro-USD = %q, want %q", amount, got, want)
		}
	}
}

// A subscription is not a free target, even though both price at zero per
// token. A local model is free because nothing meters it; a plan is metered by
// quota rather than dollars, and it is the only target here that accepts a
// cache routing key.
func TestSubscriptionSurfaceIsMeteredDifferently(t *testing.T) {
	c := load(t)
	target := provider.RouteTarget{Provider: "openai", Surface: "subscription", ModelID: "gpt-5.4-mini"}

	info, confidence, ok := c.Lookup(target)
	if !ok {
		t.Fatal("the subscription surface has no catalog entry, so a turn on it reports nothing at all")
	}
	if confidence != Prior {
		t.Errorf("confidence = %s, want prior", confidence)
	}
	if !info.Cache.RoutingKeySupport {
		t.Error("routing key support is the one thing this surface offers that no other does")
	}
	if info.Cache.UsageAccounting != AccountingSeparate {
		t.Errorf("usage accounting = %q; this endpoint reports cached and written tokens separately",
			info.Cache.UsageAccounting)
	}

	// The developer API on the same provider is a different target with real
	// per-token rates, so the two must not collapse.
	api := provider.RouteTarget{Provider: "openai", Surface: "first-party", ModelID: "gpt-5.4-mini"}
	if api.ID() == target.ID() {
		t.Error("the two openai surfaces produced one target id")
	}
}

// Three surfaces price at zero per token for three different reasons, and a
// router that treats them alike is optimizing the wrong thing: a local model
// consumes nothing scarce, a plan consumes quota, and a metered target consumes
// money.
func TestMeteringDistinguishesZeroCostForDifferentReasons(t *testing.T) {
	c := load(t)

	for _, tc := range []struct {
		target provider.RouteTarget
		want   Metering
	}{
		{provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "any"}, Local},
		{provider.RouteTarget{Provider: "openaicompat", Surface: "ollama", ModelID: "any"}, Local},
		{provider.RouteTarget{Provider: "openai", Surface: "subscription", ModelID: "any"}, Plan},
		{provider.RouteTarget{Provider: "kimi", Surface: "coding", ModelID: "any"}, Plan},
		{provider.RouteTarget{Provider: "anthropic", Surface: "first-party", ModelID: "claude-haiku-4-5"}, PerToken},
	} {
		info, _, ok := c.Lookup(tc.target)
		if !ok {
			t.Errorf("%s has no catalog entry", tc.target.ID())
			continue
		}
		if got := Metering(info.Metering.String()); got != tc.want {
			t.Errorf("%s metering = %q, want %q", tc.target.ID(), got, tc.want)
		}
	}

	// A plan target is free of per-token cost and is not free of limits, so the
	// two questions must not collapse into one answer.
	plan, _, _ := c.Lookup(provider.RouteTarget{Provider: "kimi", Surface: "coding", ModelID: "any"})
	local, _, _ := c.Lookup(provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "any"})
	if !plan.Free() || !local.Free() {
		t.Error("both should report zero per-token cost")
	}
	if plan.Metering == local.Metering {
		t.Error("a plan and a local model are metered differently and must not report the same thing")
	}
}
