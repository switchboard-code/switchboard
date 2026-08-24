package config

import (
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/fileprivacy"
	"github.com/switchboard-code/switchboard/internal/provider"
)

// TestSaveRoundTrips is the load-save-load contract: everything the loader
// understands survives a rewrite. If this breaks, the TUI editing one setting
// silently discards another.
func TestSaveRoundTrips(t *testing.T) {
	path := write(t, `
[tiers.t1]
label = "light"
model = "ollama/qwen3.5:9b-mlx"
max_output = 4096

[tiers.t2]
label = "deep"
model = "kimi/kimi-for-coding"
effort = "high"

[tiers.t3]
model = "openaicompat/somewhere/foo"
surface = "somewhere"

[slots]
summarizer = "t1"

[auth.kimi]
env = "MY_KIMI_KEY"

[auth.openai.oauth]
client_id = "abc"
authorize_url = "https://example.com/authorize"
token_url = "https://example.com/token"
scopes = ["openid"]

[providers.ollama]
base_url = "http://10.0.0.5:11434"

[updates]
check = false

[compact]
auto = false
at_percent = 70

[ui]
theme = "light"
notify = false
mouse = true
`)
	before, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := before.Save(); err != nil {
		t.Fatal(err)
	}
	after, err := LoadFile(path)
	if err != nil {
		t.Fatalf("the file Save wrote does not load: %v", err)
	}

	if !reflect.DeepEqual(before.Tiers, after.Tiers) {
		t.Errorf("tiers changed across a rewrite:\nbefore %+v\nafter  %+v", before.Tiers, after.Tiers)
	}
	if !reflect.DeepEqual(before.Slots, after.Slots) {
		t.Errorf("slots changed: %+v vs %+v", before.Slots, after.Slots)
	}
	if !reflect.DeepEqual(before.Auth, after.Auth) {
		t.Errorf("auth changed: %+v vs %+v", before.Auth, after.Auth)
	}
	if !reflect.DeepEqual(before.Providers, after.Providers) {
		t.Errorf("providers changed: %+v vs %+v", before.Providers, after.Providers)
	}
	if after.UpdateCheck {
		t.Error("check = false did not survive the rewrite")
	}
	if after.CompactAuto || after.CompactAtPercent != 70 {
		t.Errorf("compact settings changed across a rewrite: auto=%v at=%d", after.CompactAuto, after.CompactAtPercent)
	}
	if after.Theme != "light" {
		t.Errorf("theme %q did not survive the rewrite", after.Theme)
	}
	if after.NotifyOn() {
		t.Error("notify = false did not survive the rewrite")
	}
	if !after.MouseOn() {
		t.Error("mouse = true did not survive the rewrite")
	}
}

func TestSaveWritesTiersInNumericOrder(t *testing.T) {
	c := &Config{Path: filepath.Join(t.TempDir(), FileName)}
	for _, id := range []string{"t10", "t2", "t1"} {
		if err := c.BindTier(id, "", "ollama/m-"+id, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(c.Path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	i1 := strings.Index(body, "[tiers.t1]")
	i2 := strings.Index(body, "[tiers.t2]")
	i10 := strings.Index(body, "[tiers.t10]")
	if i1 < 0 || i2 < 0 || i10 < 0 {
		t.Fatalf("a tier section is missing:\n%s", body)
	}
	if !(i1 < i2 && i2 < i10) {
		t.Errorf("ladder written out of order (t1 at %d, t2 at %d, t10 at %d):\n%s", i1, i2, i10, body)
	}
}

func TestSaveOmitsWhatWasNotSet(t *testing.T) {
	c := &Config{Path: filepath.Join(t.TempDir(), FileName), UpdateCheck: true}
	if err := c.BindTier("t1", "light", "ollama/small", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(c.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"[slots]", "[auth", "[providers", "[updates]", "[ui]", "surface", "effort", "max_output"} {
		if strings.Contains(string(data), absent) {
			t.Errorf("%q appears in a file where it carries no information:\n%s", absent, data)
		}
	}
}

func TestAutomaticUpdateDefaultsOffAndExplicitOptInRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	c, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.UpdateAuto {
		t.Fatal("a missing auto setting silently enabled executable replacement")
	}
	if !c.UpdateCheck {
		t.Fatal("release notices should remain enabled by default")
	}

	c.UpdateAuto = true
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "[updates]") || !strings.Contains(string(data), "auto = true") {
		t.Fatalf("explicit auto-install opt-in was not persisted:\n%s", data)
	}

	after, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.UpdateAuto {
		t.Fatal("explicit auto-install opt-in did not survive reload")
	}

	after.UpdateAuto = false
	if err := after.Save(); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "auto =") {
		t.Fatalf("default-off auto setting should be omitted after opt-out:\n%s", data)
	}
}

func TestBindTierReplacesInPlace(t *testing.T) {
	c := &Config{}
	if err := c.BindTier("t1", "light", "ollama/small", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.BindTier("t1", "light", "ollama/smaller", "", ""); err != nil {
		t.Fatal(err)
	}
	if len(c.Tiers) != 1 {
		t.Fatalf("rebinding t1 grew the ladder to %d rungs", len(c.Tiers))
	}
	if c.Tiers[0].Target.ModelID != "smaller" {
		t.Errorf("rebinding kept the old target %q", c.Tiers[0].Target.ModelID)
	}
}

func TestBindTierWithMaxOutputSetsAndClearsTheCap(t *testing.T) {
	c := &Config{}
	if err := c.BindTierWithMaxOutput("t1", "light", "ollama/small", "", "", 2048); err != nil {
		t.Fatal(err)
	}
	if got := c.Tiers[0].Target.Params.MaxOutputTokens; got != 2048 {
		t.Fatalf("max output = %d, want 2048", got)
	}
	fallback, err := ParseTarget("ollama/backup", "", "")
	if err != nil {
		t.Fatal(err)
	}
	fallback.Params.MaxOutputTokens = 2048
	c.Tiers[0].Fallbacks = []provider.RouteTarget{fallback}
	if err := c.BindTier("t1", "light", "ollama/smaller", "", ""); err != nil {
		t.Fatal(err)
	}
	if got := c.Tiers[0].Target.Params.MaxOutputTokens; got != 0 {
		t.Fatalf("ordinary rebind retained max output %d from another model", got)
	}
	if got := c.Tiers[0].Fallbacks[0].Params.MaxOutputTokens; got != 0 {
		t.Fatalf("ordinary rebind left fallback max output %d out of sync with the rung", got)
	}
	before := c.Tiers[0]
	if err := c.BindTierWithMaxOutput("t1", "light", "ollama/smaller", "", "", -1); err == nil {
		t.Fatal("negative max output was accepted")
	}
	if !reflect.DeepEqual(c.Tiers[0], before) {
		t.Fatalf("failed binding mutated the tier: before %+v after %+v", before, c.Tiers[0])
	}
}

func TestBindTierAndSaveDoesNotPublishAFailedWrite(t *testing.T) {
	c := &Config{Path: filepath.Join(t.TempDir(), FileName)}
	if err := c.BindTierWithMaxOutput("t1", "light", "ollama/original", "", "high", 2048); err != nil {
		t.Fatal(err)
	}
	fallback, err := ParseTarget("ollama/backup", "", "")
	if err != nil {
		t.Fatal(err)
	}
	fallback.Params.MaxOutputTokens = 2048
	c.Tiers[0].Fallbacks = []provider.RouteTarget{fallback}
	before := cloneTiers(c.Tiers)

	// An existing directory cannot be atomically replaced by the temporary
	// config file, giving this test a portable failure after staging/rendering.
	c.Path = t.TempDir()
	pathBefore := c.Path
	err = c.BindTierAndSave("t1", "light", "ollama/replacement", "", "low", 4096)
	if err == nil || !strings.Contains(err.Error(), "saving configuration") {
		t.Fatalf("BindTierAndSave error = %v, want a save failure", err)
	}
	if !reflect.DeepEqual(c.Tiers, before) {
		t.Fatalf("failed durable bind changed the live ladder:\nbefore: %+v\nafter:  %+v", before, c.Tiers)
	}
	if c.Path != pathBefore {
		t.Fatalf("failed durable bind changed Path from %q to %q", pathBefore, c.Path)
	}
}

func TestProviderAddressAndSaveDoesNotPublishAFailedWrite(t *testing.T) {
	key := ProviderSurfaceKey("openaicompat", "generic")
	c := &Config{
		Path: t.TempDir(), // a directory cannot be replaced by the config file
		Providers: map[string]ProviderSettings{
			key: {BaseURL: "http://original.invalid", ContextWindow: 32_768},
		},
	}
	before := maps.Clone(c.Providers)
	pathBefore := c.Path

	err := c.SetProviderBaseURLAndSave(key, "http://replacement.invalid/v1")
	if err == nil || !strings.Contains(err.Error(), "saving configuration") {
		t.Fatalf("SetProviderBaseURLAndSave error = %v, want save failure", err)
	}
	if !reflect.DeepEqual(c.Providers, before) {
		t.Fatalf("failed durable address change mutated providers:\nbefore: %+v\nafter:  %+v", before, c.Providers)
	}
	if c.Path != pathBefore {
		t.Fatalf("failed durable address change changed Path from %q to %q", pathBefore, c.Path)
	}
}

func TestAuthAndSaveDoesNotPublishAFailedWrite(t *testing.T) {
	original := credential.Settings{Env: "ORIGINAL_API_KEY", Helper: []string{"original-helper"}}
	c := &Config{
		Path: t.TempDir(), // a directory cannot be replaced by the config file
		Auth: map[string]credential.Settings{"openai": original},
	}
	before := c.Snapshot().Auth
	pathBefore := c.Path

	replacement := original
	replacement.Helper = []string{"replacement-helper"}
	err := c.SetAuthAndSave("openai", replacement)
	if err == nil || !strings.Contains(err.Error(), "saving configuration") {
		t.Fatalf("SetAuthAndSave error = %v, want save failure", err)
	}
	if !reflect.DeepEqual(c.Auth, before) {
		t.Fatalf("failed durable auth change mutated auth:\nbefore: %+v\nafter:  %+v", before, c.Auth)
	}
	if c.Path != pathBefore {
		t.Fatalf("failed durable auth change changed Path from %q to %q", pathBefore, c.Path)
	}
}

func TestRemoveTierAndSaveDoesNotPublishAFailedWrite(t *testing.T) {
	c := &Config{Path: t.TempDir()} // a directory cannot be replaced by the config file
	if err := c.BindTier("t1", "light", "ollama/original", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.BindTier("t2", "deep", "ollama/larger", "", "high"); err != nil {
		t.Fatal(err)
	}
	before := cloneTiers(c.Tiers)
	pathBefore := c.Path

	removed, err := c.RemoveTierAndSave("t2")
	if !removed || err == nil || !strings.Contains(err.Error(), "saving configuration") {
		t.Fatalf("RemoveTierAndSave = %v, %v; want found rung and save failure", removed, err)
	}
	if !reflect.DeepEqual(c.Tiers, before) {
		t.Fatalf("failed durable removal mutated ladder:\nbefore: %+v\nafter:  %+v", before, c.Tiers)
	}
	if c.Path != pathBefore {
		t.Fatalf("failed durable removal changed Path from %q to %q", pathBefore, c.Path)
	}
}

func TestSaveRejectsNegativeMaxOutputWithoutChangingFile(t *testing.T) {
	path := write(t, "[tiers.t1]\nmodel = \"ollama/small\"\n")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c.Tiers[0].Target.Params.MaxOutputTokens = -1
	if err := c.Save(); err == nil || !strings.Contains(err.Error(), "negative max_output") {
		t.Fatalf("Save error = %v, want negative max_output refusal", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("failed Save changed the existing file:\n%s", after)
	}
}

func TestBindTierRejectsWhatLoadWouldReject(t *testing.T) {
	c := &Config{}
	if err := c.BindTier("primary", "", "ollama/small", "", ""); err == nil {
		t.Error("a tier id outside t1..tN saved now would fail to load later")
	}
	if err := c.BindTier("t1", "", "not-a-target", "", ""); err == nil {
		t.Error("a model reference without a provider saved now would fail to load later")
	}
}

func TestRemoveTierKeepsRemainingIDs(t *testing.T) {
	c := &Config{}
	for _, id := range []string{"t1", "t2", "t3"} {
		if err := c.BindTier(id, "", "ollama/m-"+id, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if !c.RemoveTier("t2") {
		t.Fatal("t2 was configured and was not removed")
	}
	if c.RemoveTier("t2") {
		t.Error("removing an absent tier claimed success")
	}
	got := make([]string, 0, len(c.Tiers))
	for _, tier := range c.Tiers {
		got = append(got, tier.ID)
	}
	if !reflect.DeepEqual(got, []string{"t1", "t3"}) {
		t.Errorf("remaining rungs are %v; removal must not renumber", got)
	}
}

func TestSaveCreatesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent", "nested")
	c := &Config{Path: filepath.Join(dir, FileName)}
	if err := c.BindTier("t1", "", "ollama/small", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatalf("first save on a fresh machine has no directory yet: %v", err)
	}
	f, err := fileprivacy.Open(c.Path)
	if err != nil {
		t.Fatal(err)
	}
	ownerOnly, ownerErr := fileprivacy.IsOwnerOnly(f)
	closeErr := f.Close()
	if ownerErr != nil || closeErr != nil || !ownerOnly {
		t.Errorf("config names auth sources but is not owner-only: owner-only=%v err=%v close=%v", ownerOnly, ownerErr, closeErr)
	}
}
