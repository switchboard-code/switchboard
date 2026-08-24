package config

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

const profileFixture = `
[tiers.t1]
label = "light"
model = "ollama/qwen3.5:9b-mlx"
max_output = 4096
fallback = ["ollama/qwen3.5:4b-mlx"]

[tiers.t2]
label = "deep"
model = "kimi/kimi-for-coding"

[profiles.review.tiers.t1]
label = "reviewer"
model = "kimi/kimi-for-coding"
effort = "high"
max_output = 8192
fallback = ["ollama/qwen3.8:27b-mlx"]

[profiles.docs.tiers.t1]
model = "ollama/qwen3.5:9b-mlx"
`

func TestProfilesLoadAndApply(t *testing.T) {
	c, err := LoadFile(write(t, profileFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Profiles) != 2 {
		t.Fatalf("profiles = %d, want 2", len(c.Profiles))
	}
	if err := c.ApplyProfile("review"); err != nil {
		t.Fatal(err)
	}
	if c.ActiveProfile != "review" {
		t.Errorf("ActiveProfile = %q", c.ActiveProfile)
	}
	if len(c.Tiers) != 1 || c.Tiers[0].Label != "reviewer" {
		t.Fatalf("the active ladder is not the profile's: %+v", c.Tiers)
	}
	if c.Tiers[0].Target.Params.Reasoning == nil || c.Tiers[0].Target.Params.Reasoning.Effort != "high" {
		t.Errorf("the profile rung lost its effort: %+v", c.Tiers[0].Target)
	}
	if got := c.Tiers[0].Target.Params.MaxOutputTokens; got != 8192 {
		t.Errorf("the profile rung lost max_output: got %d want 8192", got)
	}
	if len(c.Tiers[0].Fallbacks) != 1 || c.Tiers[0].Fallbacks[0].Params.MaxOutputTokens != 8192 {
		t.Errorf("the profile fallback lost the rung max_output: %+v", c.Tiers[0].Fallbacks)
	}
}

func TestApplyProfileNamesTheOnesConfigured(t *testing.T) {
	c, err := LoadFile(write(t, profileFixture))
	if err != nil {
		t.Fatal(err)
	}
	err = c.ApplyProfile("writing")
	if err == nil || !strings.Contains(err.Error(), "docs, review") {
		t.Errorf("an unknown profile must name what the file holds, got %v", err)
	}

	bare, err := LoadFile(write(t, "[tiers.t1]\nmodel = \"ollama/qwen3.5:9b-mlx\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	err = bare.ApplyProfile("review")
	if err == nil || !strings.Contains(err.Error(), "no profiles are configured") {
		t.Errorf("with none configured the error must say how to declare one, got %v", err)
	}
}

// TestSaveUnderAProfileKeepsTheMainLadder is the contract that makes
// -profile safe to combine with the TUI's own saves: /theme or /budget
// persisting mid-session must not overwrite the main ladder with the
// profile's rungs, and a rung bound under a profile lands in the profile.
func TestSaveUnderAProfileKeepsTheMainLadder(t *testing.T) {
	c, err := LoadFile(write(t, profileFixture))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyProfile("review"); err != nil {
		t.Fatal(err)
	}
	if err := c.BindTierAndSave("t2", "checker", "ollama/qwen3.8:27b-mlx", "", "", 2048); err != nil {
		t.Fatal(err)
	}

	reread, err := LoadFile(c.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(reread.Tiers) != 2 || reread.Tiers[0].Label != "light" || reread.Tiers[1].Label != "deep" {
		t.Fatalf("the main ladder did not survive a save under a profile: %+v", reread.Tiers)
	}
	if got := reread.Tiers[0].Target.Params.MaxOutputTokens; got != 4096 {
		t.Fatalf("the main ladder max_output changed under a profile: got %d want 4096", got)
	}
	if len(reread.Tiers[0].Fallbacks) != 1 || reread.Tiers[0].Fallbacks[0].Params.MaxOutputTokens != 4096 {
		t.Fatalf("the main ladder fallback cap changed under a profile: %+v", reread.Tiers[0].Fallbacks)
	}
	review := reread.Profiles["review"]
	if len(review) != 2 || review[0].Label != "reviewer" || review[1].Label != "checker" {
		t.Fatalf("the rung bound under the profile did not land in it: %+v", review)
	}
	if review[0].Target.Params.MaxOutputTokens != 8192 || review[1].Target.Params.MaxOutputTokens != 2048 {
		t.Fatalf("profile max_output values did not survive: %+v", review)
	}
	if len(review[0].Fallbacks) != 1 || review[0].Fallbacks[0].Params.MaxOutputTokens != 8192 {
		t.Fatalf("profile fallback max_output did not survive: %+v", review[0].Fallbacks)
	}
	if docs := reread.Profiles["docs"]; len(docs) != 1 {
		t.Fatalf("the inactive profile did not survive: %+v", docs)
	}
}

func TestFailedDurableBindUnderProfileChangesNeitherLadderNorFile(t *testing.T) {
	c, err := LoadFile(write(t, profileFixture))
	if err != nil {
		t.Fatal(err)
	}
	originalPath := c.Path
	originalFile, err := os.ReadFile(originalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyProfile("review"); err != nil {
		t.Fatal(err)
	}
	beforeActive := cloneTiers(c.Tiers)
	beforeMain := cloneTiers(c.mainTiers)
	beforeProfiles := make(map[string][]Tier, len(c.Profiles))
	for name, ladder := range c.Profiles {
		beforeProfiles[name] = cloneTiers(ladder)
	}

	// A directory cannot be replaced by the staged config file.
	c.Path = t.TempDir()
	pathBefore := c.Path
	err = c.BindTierAndSave("t1", "replacement", "ollama/replacement", "", "", 4096)
	if err == nil || !strings.Contains(err.Error(), "saving configuration") {
		t.Fatalf("profile durable bind error = %v, want save failure", err)
	}
	if !reflect.DeepEqual(c.Tiers, beforeActive) || !reflect.DeepEqual(c.mainTiers, beforeMain) ||
		!reflect.DeepEqual(c.Profiles, beforeProfiles) {
		t.Fatalf("failed profile bind mutated active/main/profile ladders")
	}
	if c.Path != pathBefore {
		t.Fatalf("failed profile bind changed Path from %q to %q", pathBefore, c.Path)
	}
	afterFile, err := os.ReadFile(originalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterFile, originalFile) {
		t.Fatal("failed profile bind changed the previously durable file")
	}
}

func TestProfileErrorsNameTheProfile(t *testing.T) {
	_, err := LoadFile(write(t, "[profiles.review.tiers.t1]\nmodel = \"nonsense\"\n"))
	if err == nil || !strings.Contains(err.Error(), "profile review tier t1") {
		t.Errorf("a broken rung inside a profile must say which profile, got %v", err)
	}

	_, err = LoadFile(write(t, "[profiles.review]\n"))
	if err == nil || !strings.Contains(err.Error(), "has no tiers") {
		t.Errorf("an empty profile must be refused at load, got %v", err)
	}
}
