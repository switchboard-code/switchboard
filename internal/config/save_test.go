package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestSaveRoundTrips is the load-save-load contract: everything the loader
// understands survives a rewrite. If this breaks, the TUI editing one setting
// silently discards another.
func TestSaveRoundTrips(t *testing.T) {
	path := write(t, `
[tiers.t1]
label = "light"
model = "ollama/qwen3.5:9b-mlx"

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
	c := &Config{Path: filepath.Join(t.TempDir(), FileName), UpdateCheck: true, UpdateAuto: true}
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
	for _, absent := range []string{"[slots]", "[auth", "[providers", "[updates]", "[ui]", "surface", "effort"} {
		if strings.Contains(string(data), absent) {
			t.Errorf("%q appears in a file where it carries no information:\n%s", absent, data)
		}
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
	info, err := os.Stat(c.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("config written with mode %o; it names auth sources and belongs to the user alone", perm)
	}
}
