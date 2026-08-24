package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/provider"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSnapshotIsDeepAndIndependent(t *testing.T) {
	temperature := 0.25
	reasoning := &provider.Reasoning{Enabled: true, Effort: "high"}
	routeAuto := true
	target := provider.RouteTarget{
		Provider: "ollama", Surface: "local", ModelID: "primary",
		Params: provider.Params{Temperature: &temperature, Reasoning: reasoning},
	}
	cfg := &Config{
		Tiers: []Tier{{
			ID: "t1", Target: target,
			Fallbacks: []provider.RouteTarget{{Provider: "ollama", Surface: "local", ModelID: "fallback"}},
		}},
		Slots:     map[string]string{"advisor": "t1"},
		Providers: map[string]ProviderSettings{"ollama": {BaseURL: "http://before"}},
		Auth: map[string]credential.Settings{"ollama": {
			Helper: []string{"helper", "before"},
			OAuth: credential.OAuthSettings{
				Scopes: []string{"before"}, ExtraAuthParams: map[string]string{"aud": "before"},
			},
		}},
		Destinations: []string{"ollama"},
		Profiles:     map[string][]Tier{"review": {{ID: "t2", Target: target}}},
		RouteAuto:    &routeAuto,
	}

	snapshot := cfg.Snapshot()
	cfg.Tiers[0].Target.Params.Reasoning.Effort = "low"
	*cfg.Tiers[0].Target.Params.Temperature = 0.75
	cfg.Tiers[0].Fallbacks[0].ModelID = "changed"
	cfg.Slots["advisor"] = "t2"
	cfg.Providers["ollama"] = ProviderSettings{BaseURL: "http://after"}
	cfg.Auth["ollama"].Helper[1] = "after"
	cfg.Auth["ollama"].OAuth.Scopes[0] = "after"
	cfg.Auth["ollama"].OAuth.ExtraAuthParams["aud"] = "after"
	cfg.Destinations[0] = "openai"
	cfg.Profiles["review"][0].Target.ModelID = "changed"
	*cfg.RouteAuto = false

	if got := snapshot.Tiers[0].Target.Params.Reasoning.Effort; got != "high" {
		t.Fatalf("snapshot reasoning changed to %q", got)
	}
	if got := *snapshot.Tiers[0].Target.Params.Temperature; got != 0.25 {
		t.Fatalf("snapshot temperature changed to %v", got)
	}
	if snapshot.Tiers[0].Fallbacks[0].ModelID != "fallback" ||
		snapshot.Slots["advisor"] != "t1" || snapshot.Providers["ollama"].BaseURL != "http://before" ||
		snapshot.Auth["ollama"].Helper[1] != "before" || snapshot.Auth["ollama"].OAuth.Scopes[0] != "before" ||
		snapshot.Auth["ollama"].OAuth.ExtraAuthParams["aud"] != "before" ||
		snapshot.Destinations[0] != "ollama" || snapshot.Profiles["review"][0].Target.ModelID != "primary" ||
		!*snapshot.RouteAuto {
		t.Fatalf("snapshot retained mutable aliases: %+v", snapshot)
	}
}

func TestMissingFileIsNotAnError(t *testing.T) {
	c, err := LoadFile(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("running without a config file is the normal first-run case: %v", err)
	}
	if len(c.Tiers) != 0 {
		t.Errorf("got %d tiers from an absent file", len(c.Tiers))
	}
	if _, ok := c.Default(); ok {
		t.Error("an empty ladder has no default tier")
	}
}

func TestTiersLoadInNumericOrder(t *testing.T) {
	path := write(t, `
[tiers.t10]
label = "max"
model = "ollama/big"

[tiers.t2]
label = "standard"
model = "ollama/medium"

[tiers.t1]
label = "light"
model = "ollama/small"
`)
	c, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	// Lexical order would put t10 second. The ladder is ascending policy, so
	// the order has to be numeric.
	var got []string
	for _, tier := range c.Tiers {
		got = append(got, tier.ID)
	}
	if strings.Join(got, ",") != "t1,t2,t10" {
		t.Errorf("tier order = %v, want t1,t2,t10", got)
	}

	def, ok := c.Default()
	if !ok || def.ID != "t1" {
		t.Errorf("default tier = %+v, want t1: a session starts at the bottom of the ladder", def)
	}
	if def.Label != "light" {
		t.Errorf("label = %q", def.Label)
	}
}

func TestTierBindsTargetAndEffort(t *testing.T) {
	path := write(t, `
[tiers.t1]
model = "ollama/qwen3.5:9b-mlx"

[tiers.t2]
model = "ollama/qwen3.6:27b"
effort = "high"
`)
	c, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	t1, _ := c.Tier("t1")
	if t1.Target.Provider != "ollama" || t1.Target.Surface != "local" {
		t.Errorf("target = %+v, want the ollama default surface", t1.Target)
	}
	if t1.Target.ModelID != "qwen3.5:9b-mlx" {
		t.Errorf("model id = %q; a colon in the name must survive the split", t1.Target.ModelID)
	}

	t2, _ := c.Tier("t2")
	if t2.Target.Params.Reasoning == nil || t2.Target.Params.Reasoning.Effort != "high" {
		t.Errorf("effort did not bind: %+v", t2.Target.Params.Reasoning)
	}
	// Two tiers on the same model at different effort are different targets,
	// because effort changes cache identity (§3.1).
	if t1.Target.ID() == t2.Target.ID() {
		t.Error("tiers with different effort produced the same target id")
	}
}

func TestTierBindsExplicitMaxOutput(t *testing.T) {
	path := write(t, `
[tiers.t1]
model = "ollama/custom"
max_output = 4096
fallback = ["openaicompat/custom-backup"]
`)
	c, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	tier, _ := c.Tier("t1")
	if got := tier.Target.Params.MaxOutputTokens; got != 4096 {
		t.Fatalf("max output = %d, want 4096", got)
	}
	withoutCap, err := ParseTarget("ollama/custom", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if tier.Target.ID() == withoutCap.ID() {
		t.Fatal("an explicit output cap did not participate in target identity")
	}
	if len(tier.Fallbacks) != 1 || tier.Fallbacks[0].Params.MaxOutputTokens != 4096 {
		t.Fatalf("rung max_output did not bind its fallback: %+v", tier.Fallbacks)
	}
	withoutFallbackCap, err := ParseTarget("openaicompat/custom-backup", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if tier.Fallbacks[0].ID() == withoutFallbackCap.ID() {
		t.Fatal("the rung cap did not participate in fallback target identity")
	}
}

func TestTierMaxOutputMustBePositiveWhenPresent(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			name: "zero",
			body: "[tiers.t1]\nmodel = \"ollama/custom\"\nmax_output = 0\n",
			want: "tier t1 max_output 0 must be positive",
		},
		{
			name: "negative",
			body: "[tiers.t1]\nmodel = \"ollama/custom\"\nmax_output = -1\n",
			want: "tier t1 max_output -1 must be positive",
		},
		{
			name: "profile",
			body: "[profiles.review.tiers.t1]\nmodel = \"ollama/custom\"\nmax_output = 0\n",
			want: "profile review tier t1 max_output 0 must be positive",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadFile(write(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadFile error = %v, want %q", err, tc.want)
			}
		})
	}
}

// Model identifiers legitimately contain slashes, so the provider split has to
// take the first one only.
func TestModelNamesMayContainSlashes(t *testing.T) {
	target, err := ParseTarget("ollama/hf.co/someone/a-model", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if target.Provider != "ollama" {
		t.Errorf("provider = %q", target.Provider)
	}
	if target.ModelID != "hf.co/someone/a-model" {
		t.Errorf("model id = %q, want the whole remainder", target.ModelID)
	}
}

func TestExplicitSurface(t *testing.T) {
	target, err := ParseTarget("anthropic/claude-opus-5", "bedrock", "")
	if err != nil {
		t.Fatal(err)
	}
	if target.Surface != "bedrock" {
		t.Errorf("surface = %q, want the explicit value", target.Surface)
	}
}

// Guessing a surface would attach the wrong catalog entry, and price, cache
// behavior, and retention all differ per surface.
func TestUnknownProviderNeedsAnExplicitSurface(t *testing.T) {
	_, err := ParseTarget("acme/some-model", "", "")
	if err == nil || !strings.Contains(err.Error(), "surface") {
		t.Errorf("err = %v, want a complaint about the missing surface", err)
	}
}

func TestMalformedModelReference(t *testing.T) {
	for _, ref := range []string{"", "just-a-model", "ollama/"} {
		if _, err := ParseTarget(ref, "", ""); err == nil {
			t.Errorf("%q should not parse as a target", ref)
		}
	}
}

func TestUnsafeModelIDsNeverReachConfig(t *testing.T) {
	token := "ghp_" + strings.Repeat("A", 40)
	for name, ref := range map[string]string{
		"credential": "ollama/model-" + token,
		"control":    "ollama/model\nspoof",
		"oversize":   "ollama/" + strings.Repeat("m", provider.MaxProviderModelIDBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseTarget(ref, "", "")
			if err == nil {
				t.Fatal("unsafe model ID was accepted")
			}
			if strings.Contains(err.Error(), token) {
				t.Fatalf("error exposed credential-shaped model ID: %v", err)
			}
		})
	}
}

func TestTierNamesMustFollowTheScheme(t *testing.T) {
	for _, name := range []string{"fast", "t0", "tier1", "tx"} {
		path := write(t, "[tiers."+name+"]\nmodel = \"ollama/m\"\n")
		if _, err := LoadFile(path); err == nil {
			t.Errorf("tier name %q should be rejected", name)
		}
	}
}

// A misspelled key that is silently ignored is a setting the user believes is
// in effect and is not.
func TestUnrecognizedKeysAreRejected(t *testing.T) {
	path := write(t, `
[tiers.t1]
model = "ollama/m"
labl = "typo"
`)
	_, err := LoadFile(path)
	if err == nil {
		t.Fatal("a misspelled key must be an error, not silently dropped")
	}
	if !strings.Contains(err.Error(), "labl") {
		t.Errorf("err = %v, want it to name the unrecognized key", err)
	}
}

func TestSlots(t *testing.T) {
	path := write(t, `
[tiers.t1]
model = "ollama/m"

[slots]
title = "t1"
embed = "ollama/nomic-embed-text"
`)
	c, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Slots["title"] != "t1" {
		t.Errorf("a slot aliasing a tier did not load: %v", c.Slots)
	}
	if c.Slots["embed"] != "ollama/nomic-embed-text" {
		t.Errorf("a slot naming a model directly did not load: %v", c.Slots)
	}
}

func TestTooManyTiers(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= MaxTiers+1; i++ {
		fmt.Fprintf(&b, "[tiers.t%d]\nmodel = \"ollama/m\"\n\n", i)
	}
	path := write(t, b.String())
	if _, err := LoadFile(path); err == nil {
		t.Errorf("more than %d tiers should be rejected", MaxTiers)
	}
}

// A provider reached at another address is still that provider: a gateway, an
// Azure deployment, or a proxy does not change which credential pays or which
// catalog entry prices it.
func TestProviderBaseURLOverride(t *testing.T) {
	path := write(t, `
[tiers.t1]
model = "openai/some-model"

[providers.openai]
base_url = "https://gateway.example.com/v1"
`)
	c, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.ProviderFor("openai").BaseURL; got != "https://gateway.example.com/v1" {
		t.Errorf("base url = %q", got)
	}
	if got := c.ProviderFor("anthropic").BaseURL; got != "" {
		t.Errorf("an unconfigured provider reported %q rather than falling back to its default", got)
	}

	t1, _ := c.Tier("t1")
	if t1.Target.Provider != "openai" || t1.Target.Surface != "first-party" {
		t.Errorf("the override changed target identity: %+v", t1.Target)
	}
}

func TestUnrecognizedProviderKeyIsRejected(t *testing.T) {
	path := write(t, "[providers.openai]\nbase_urls = \"typo\"\n")
	if _, err := LoadFile(path); err == nil {
		t.Error("a misspelled provider key must be an error, not silently ignored")
	}
}

func TestBudgetRoundTripsAndRejectsNegative(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	c := &Config{Path: path, Budget: 2_500_000}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Budget != 2_500_000 {
		t.Errorf("Budget = %d, want the saved ceiling back", loaded.Budget)
	}

	if err := os.WriteFile(path, []byte("[limits]\nbudget = \"-1.00\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err == nil {
		t.Error("a negative ceiling must refuse to load, not rule out every turn quietly")
	}
}

func TestTierFallbacksRoundTripAndValidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(
		"[tiers.t1]\nmodel = \"ollama/small\"\nfallback = [\"ollama/backup\", \"kimi/kimi-for-coding\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	fbs := c.Tiers[0].Fallbacks
	if len(fbs) != 2 || fbs[0].ModelID != "backup" || fbs[1].Provider != "kimi" {
		t.Fatalf("Fallbacks = %+v, want both entries in order", fbs)
	}
	if fbs[0].Surface == "" {
		t.Error("a fallback must resolve its provider's default surface")
	}
	for i, fallback := range fbs {
		if fallback.Params.MaxOutputTokens != 0 {
			t.Fatalf("legacy fallback %d acquired max_output %d", i+1, fallback.Params.MaxOutputTokens)
		}
	}

	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "max_output") {
		t.Fatalf("saving a legacy uncapped rung invented max_output:\n%s", data)
	}
	again, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Tiers[0].Fallbacks) != 2 {
		t.Errorf("fallbacks did not survive a save round trip: %+v", again.Tiers[0].Fallbacks)
	}

	if err := os.WriteFile(path, []byte(
		"[tiers.t1]\nmodel = \"ollama/small\"\nfallback = [\"nonsense\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFile(path); err == nil {
		t.Error("a fallback entry without provider/model form must refuse to load")
	}
}

func TestSharedTierTargetCanSaveAndLoad(t *testing.T) {
	path := write(t, "[tiers.t1]\nmodel = \"ollama/shared\"\n")
	c, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.BindTier("t2", "backup rank", "ollama/shared", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatalf("BindTier saved a config that LoadFile rejects: %v", err)
	}
	if len(loaded.Tiers) != 2 || loaded.Tiers[0].Target.ID() != loaded.Tiers[1].Target.ID() {
		t.Fatalf("shared target did not survive save/load: %+v", loaded.Tiers)
	}
}

func TestBindTierPreservesFallbacksThroughSaveAndLoad(t *testing.T) {
	path := write(t, "[tiers.t1]\nmodel = \"ollama/old\"\nfallback = [\"ollama/first\", \"ollama/second\"]\n")
	c, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.BindTierWithMaxOutput("t1", "replacement", "ollama/new", "", "", 3072); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Tiers) != 1 || loaded.Tiers[0].Target.ModelID != "new" {
		t.Fatalf("replacement target did not survive: %+v", loaded.Tiers)
	}
	if got := loaded.Tiers[0].Target.Params.MaxOutputTokens; got != 3072 {
		t.Fatalf("replacement max output = %d, want 3072", got)
	}
	fallbacks := loaded.Tiers[0].Fallbacks
	if len(fallbacks) != 2 || fallbacks[0].ModelID != "first" || fallbacks[1].ModelID != "second" {
		t.Fatalf("BindTier erased or reordered configured fallbacks: %+v", fallbacks)
	}
	for i, fallback := range fallbacks {
		if fallback.Params.MaxOutputTokens != 3072 {
			t.Fatalf("fallback %d max output = %d, want rung cap 3072", i+1, fallback.Params.MaxOutputTokens)
		}
	}
}

func TestSaveRefusesUnrepresentableTargetsWithoutChangingFile(t *testing.T) {
	base := provider.RouteTarget{Provider: "ollama", Surface: "local", ModelID: "model"}
	temperature := 0.2
	cases := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "primary temperature", want: "tier t1 target", mutate: func(c *Config) {
			c.Tiers[0].Target.Params.Temperature = &temperature
		}},
		{name: "primary explicit reasoning off", want: "tier t1 target", mutate: func(c *Config) {
			c.Tiers[0].Target.Params.Reasoning = &provider.Reasoning{Enabled: false}
		}},
		{name: "fallback nondefault surface", want: "tier t1 fallback 1", mutate: func(c *Config) {
			c.Tiers[0].Fallbacks = []provider.RouteTarget{{Provider: "ollama", Surface: "remote", ModelID: "fallback"}}
		}},
		{name: "fallback parameters", want: "tier t1 fallback 1", mutate: func(c *Config) {
			fallback := base
			fallback.Params.MaxOutputTokens = 512
			c.Tiers[0].Fallbacks = []provider.RouteTarget{fallback}
		}},
		{name: "fallback cap differs from rung", want: "different from the rung", mutate: func(c *Config) {
			c.Tiers[0].Target.Params.MaxOutputTokens = 2048
			fallback := base
			fallback.Params.MaxOutputTokens = 512
			c.Tiers[0].Fallbacks = []provider.RouteTarget{fallback}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := write(t, "[tiers.t1]\nmodel = \"ollama/model\"\n")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(cfg)
			err = cfg.Save()
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "cannot be represented") {
				t.Fatalf("Save error = %v, want precise identity-preservation refusal", err)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(after) != string(before) {
				t.Fatalf("failed Save changed the existing file:\n%s", after)
			}
		})
	}
}

func TestSaveRoundTripsEveryRepresentableTargetIdentity(t *testing.T) {
	path := write(t, `[tiers.t1]
model = "anthropic/claude-test"
surface = "bedrock"
effort = "high"
max_output = 3072
fallback = ["ollama/first", "kimi/second"]
`)
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantPrimary := cfg.Tiers[0].Target.ID()
	wantFallbacks := []provider.RouteTargetID{cfg.Tiers[0].Fallbacks[0].ID(), cfg.Tiers[0].Fallbacks[1].ID()}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := reloaded.Tiers[0].Target.ID(); got != wantPrimary {
		t.Fatalf("primary identity changed across Save: got %s want %s", got, wantPrimary)
	}
	for index, want := range wantFallbacks {
		if got := reloaded.Tiers[0].Fallbacks[index].ID(); got != want {
			t.Fatalf("fallback %d identity changed across Save: got %s want %s", index+1, got, want)
		}
	}
}

// One provider can front several servers at once, so an address belongs to a
// surface. The provider-wide key stays the fallback, which is what keeps a
// gateway redirect meaning what it always did.
func TestAnAddressBelongsToASurface(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	c := &Config{Path: path, Providers: map[string]ProviderSettings{}}
	c.SetProviderBaseURL("openaicompat", "http://gateway.internal/v1")
	c.SetProviderBaseURL(ProviderSurfaceKey("openaicompat", "generic"), "http://localhost:1234/v1/")
	if err := c.BindTier("t1", "", "openaicompat/qwen3", "generic", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}

	saved, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := saved.ProviderForTarget("openaicompat", "generic").BaseURL; got != "http://localhost:1234/v1" {
		t.Fatalf("the generic surface resolved to %q, want its own address with the trailing slash dropped", got)
	}
	if got := saved.ProviderForTarget("openaicompat", "ollama").BaseURL; got != "http://gateway.internal/v1" {
		t.Fatalf("a surface with no address of its own resolved to %q, want the provider-wide one", got)
	}
	if got := saved.ProviderForTarget("anthropic", "first-party").BaseURL; got != "" {
		t.Fatalf("an unconfigured provider resolved to %q, want the adapter's default", got)
	}

	// The compatible adapter's default surface round-trips without the file
	// having to spell it out, the way every other provider's does.
	tier, ok := saved.Tier("t1")
	if !ok {
		t.Fatal("t1 did not survive the round trip")
	}
	if tier.Target.Surface != "generic" || tier.Target.ModelID != "qwen3" {
		t.Fatalf("t1 loaded as %+v, want the generic compatible surface", tier.Target)
	}

	c.SetProviderBaseURL(ProviderSurfaceKey("openaicompat", "generic"), "")
	if _, still := c.Providers[ProviderSurfaceKey("openaicompat", "generic")]; still {
		t.Fatal("clearing an address should remove the entry, not write a blank one")
	}
}
