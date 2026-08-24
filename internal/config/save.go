package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/BurntSushi/toml"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/fileprivacy"
	"github.com/switchboard-code/switchboard/internal/provider"
)

// header goes at the top of every file this package writes. Once the TUI
// starts editing settings, the file has two writers: the user's editor and
// this program. The program regenerates the whole file from its typed state,
// so a hand-written comment does not survive a rewrite. Saying so in the file
// itself beats letting someone learn it by losing an annotation.
const header = `# Switchboard configuration.
#
# sb rewrites this file whenever settings change inside the TUI, and a rewrite
# regenerates everything from the loaded state: comments and formatting placed
# here by hand do not survive. Hand-editing still works; it just does not mix
# with annotation. Credentials never belong in this file (§5.3).

`

// Save writes the configuration back to its file, creating ~/.switchboard on
// first save. The write is atomic: a temporary file in the same directory,
// then a rename, so a crash mid-write leaves the old file rather than half a
// new one.
func (c *Config) Save() error {
	path := c.Path
	if path == "" {
		p, err := Path()
		if err != nil {
			return fmt.Errorf("no home directory to save into: %w", err)
		}
		path = p
		c.Path = path
	}

	if err := fileprivacy.EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}

	body, err := c.render()
	if err != nil {
		return err
	}

	tmp, err := fileprivacy.CreateTemp(filepath.Dir(path), FileName+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// render produces the file. Tiers are written by hand in numeric order,
// because an encoder sorts map keys lexically and a ladder that reads t1, t10,
// t2 misstates its own ordering. Everything else goes through the encoder,
// one section at a time so an empty section is absent rather than an empty
// header.
func (c *Config) render() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(header)

	// While a profile is active the main ladder lives in mainTiers, and it
	// is the main ladder the [tiers] sections must carry: a save under a
	// profile that wrote the profile's rungs there would overwrite the
	// ladder every unprofiled session runs on.
	mainLadder := c.Tiers
	if c.ActiveProfile != "" {
		mainLadder = c.mainTiers
	}
	for _, t := range mainLadder {
		if err := writeTierSection(&buf, "tiers."+t.ID, t); err != nil {
			return nil, err
		}
	}

	profileNames := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		profileNames = append(profileNames, name)
	}
	sort.Strings(profileNames)
	for _, name := range profileNames {
		ladder := c.Profiles[name]
		// The active profile writes the live ladder, so a rung bound with
		// /models under -profile persists to the profile it was bound in.
		if name == c.ActiveProfile {
			ladder = c.Tiers
		}
		for _, t := range ladder {
			if err := writeTierSection(&buf, "profiles."+name+".tiers."+t.ID, t); err != nil {
				return nil, err
			}
		}
	}

	if len(c.Slots) > 0 {
		if err := encode(&buf, struct {
			Slots map[string]string `toml:"slots"`
		}{c.Slots}); err != nil {
			return nil, err
		}
		buf.WriteString("\n")
	}

	if auth := c.authEntries(); len(auth) > 0 {
		if err := encode(&buf, struct {
			Auth map[string]authEntry `toml:"auth"`
		}{auth}); err != nil {
			return nil, err
		}
		buf.WriteString("\n")
	}

	if len(c.Providers) > 0 {
		entries := make(map[string]providerEntry, len(c.Providers))
		for name, p := range c.Providers {
			entries[name] = providerEntry{BaseURL: p.BaseURL, ContextWindow: p.ContextWindow}
		}
		if err := encode(&buf, struct {
			Providers map[string]providerEntry `toml:"providers"`
		}{entries}); err != nil {
			return nil, err
		}
		buf.WriteString("\n")
	}

	// Defaults are absent, so only a chosen value is worth a line: writing
	// "check = true" would suggest it had been decided.
	var updates updatesEntry
	if !c.UpdateCheck {
		off := false
		updates.Check = &off
	}
	if c.UpdateAuto {
		on := true
		updates.Auto = &on
	}
	if c.UpdateChannel != "" && c.UpdateChannel != "stable" {
		updates.Channel = c.UpdateChannel
	}
	if updates.Check != nil || updates.Auto != nil || updates.Channel != "" {
		if err := encode(&buf, struct {
			Updates updatesEntry `toml:"updates"`
		}{updates}); err != nil {
			return nil, err
		}
		buf.WriteString("\n")
	}

	var compact compactEntry
	if !c.CompactAuto {
		off := false
		compact.Auto = &off
	}
	if c.CompactAtPercent != 0 && c.CompactAtPercent != 85 {
		compact.AtPercent = c.CompactAtPercent
	}
	if compact.Auto != nil || compact.AtPercent != 0 {
		if err := encode(&buf, struct {
			Compact compactEntry `toml:"compact"`
		}{compact}); err != nil {
			return nil, err
		}
		buf.WriteString("\n")
	}

	// Permissions render before routing because a reader scanning this file for
	// "what has this thing been told it may do" should meet the answers before
	// the ladder's own settings.
	if len(c.Permissions) > 0 {
		entries := make([]permissionEntry, 0, len(c.Permissions))
		for _, rule := range c.Permissions {
			entries = append(entries, permissionEntryFor(rule))
		}
		if err := encode(&buf, struct {
			Permissions []permissionEntry `toml:"permissions"`
		}{entries}); err != nil {
			return nil, err
		}
		buf.WriteString("\n")
	}

	if !c.RouteAutoOn() || len(c.Destinations) > 0 {
		entry := routingEntry{Destinations: c.Destinations}
		if !c.RouteAutoOn() {
			off := false
			entry.Auto = &off
		}
		if err := encode(&buf, struct {
			Routing routingEntry `toml:"routing"`
		}{entry}); err != nil {
			return nil, err
		}
		buf.WriteString("\n")
	}

	if c.Theme != "" || !c.NotifyOn() || !c.MouseOn() {
		ui := uiEntry{Theme: c.Theme}
		// Absent means on for both, so the file carries each setting only
		// when it is the non-default off.
		if !c.NotifyOn() {
			off := false
			ui.Notify = &off
		}
		if !c.MouseOn() {
			off := false
			ui.Mouse = &off
		}
		if err := encode(&buf, struct {
			UI uiEntry `toml:"ui"`
		}{ui}); err != nil {
			return nil, err
		}
		buf.WriteString("\n")
	}

	if c.Budget > 0 {
		if err := encode(&buf, struct {
			Limits limitsEntry `toml:"limits"`
		}{limitsEntry{Budget: c.Budget}}); err != nil {
			return nil, err
		}
		buf.WriteString("\n")
	}

	if c.Sandbox != "" && c.Sandbox != execution.SandboxOff {
		if err := encode(&buf, struct {
			Execution struct {
				Sandbox string `toml:"sandbox"`
			} `toml:"execution"`
		}{Execution: struct {
			Sandbox string `toml:"sandbox"`
		}{Sandbox: string(c.Sandbox)}}); err != nil {
			return nil, err
		}
		buf.WriteString("\n")
	}

	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func encode(buf *bytes.Buffer, v any) error {
	enc := toml.NewEncoder(buf)
	enc.Indent = ""
	return enc.Encode(v)
}

// writeTierSection renders one rung under the given heading, shared by the
// main ladder and the profiles so the two can never drift in format.
func writeTierSection(buf *bytes.Buffer, heading string, t Tier) error {
	if err := validateTierRoundTrip(t); err != nil {
		return fmt.Errorf("%s: %w", heading, err)
	}
	fmt.Fprintf(buf, "[%s]\n", heading)
	entry := tierEntry{
		Label:     t.Label,
		Model:     t.Target.Provider + "/" + t.Target.ModelID,
		Surface:   surfaceToWrite(t.Target.Provider, t.Target.Surface),
		Effort:    effortOf(t.Target),
		MaxOutput: positiveInt(t.Target.Params.MaxOutputTokens),
	}
	for _, fb := range t.Fallbacks {
		entry.Fallback = append(entry.Fallback, fb.Provider+"/"+fb.ModelID)
	}
	if err := encode(buf, entry); err != nil {
		return err
	}
	buf.WriteString("\n")
	return nil
}

// validateTierRoundTrip prevents Save from silently changing a target's wire,
// cache, or pricing identity. The current human-editable TOML schema can
// represent an explicit primary surface, reasoning effort, and one positive
// rung-wide max output, but it has no fields for temperature, explicit
// reasoning-off, fallback surfaces, or fallback-specific parameters. Refusing
// those typed values before writing is safer than producing a file that loads
// as a different target on the next run.
func validateTierRoundTrip(t Tier) error {
	if t.Target.Params.MaxOutputTokens < 0 {
		return fmt.Errorf("tier %s target %s has negative max_output", t.ID, t.Target.Display())
	}
	primary, err := ParseTarget(t.Target.Provider+"/"+t.Target.ModelID,
		surfaceToWrite(t.Target.Provider, t.Target.Surface), effortOf(t.Target))
	primary.Params.MaxOutputTokens = t.Target.Params.MaxOutputTokens
	if err != nil || primary.ID() != t.Target.ID() {
		return fmt.Errorf("tier %s target %s cannot be represented without changing its identity", t.ID, t.Target.Display())
	}
	for index, target := range t.Fallbacks {
		if target.Params.MaxOutputTokens != t.Target.Params.MaxOutputTokens {
			return fmt.Errorf("tier %s fallback %d (%s) has max_output %d, different from the rung's %d, and cannot be represented",
				t.ID, index+1, target.Display(), target.Params.MaxOutputTokens, t.Target.Params.MaxOutputTokens)
		}
		fallback, fallbackErr := ParseTarget(target.Provider+"/"+target.ModelID, "", "")
		fallback.Params.MaxOutputTokens = t.Target.Params.MaxOutputTokens
		if fallbackErr != nil || fallback.ID() != target.ID() {
			return fmt.Errorf("tier %s fallback %d (%s) cannot be represented without changing its identity",
				t.ID, index+1, target.Display())
		}
	}
	return nil
}

func (c *Config) authEntries() map[string]authEntry {
	entries := make(map[string]authEntry, len(c.Auth))
	for name, s := range c.Auth {
		e := authEntry{Env: s.Env, Helper: s.Helper}
		if s.OAuth.ClientID != "" || s.OAuth.TokenURL != "" {
			e.OAuth = &oauthEntry{
				ClientID:     s.OAuth.ClientID,
				AuthorizeURL: s.OAuth.AuthorizeURL,
				TokenURL:     s.OAuth.TokenURL,
				Scopes:       s.OAuth.Scopes,
				Audience:     s.OAuth.Audience,
				RedirectPort: s.OAuth.RedirectPort,
				ExtraParams:  s.OAuth.ExtraAuthParams,
			}
		}
		entries[name] = e
	}
	return entries
}

// surfaceToWrite omits a surface the loader would infer anyway, so the common
// case stays one line. An explicit surface that happens to equal the default
// is the same claim as an absent one; there is nothing to preserve.
func surfaceToWrite(providerName, surface string) string {
	if defaultSurfaces[providerName] == surface {
		return ""
	}
	return surface
}

func effortOf(t provider.RouteTarget) string {
	if t.Params.Reasoning == nil {
		return ""
	}
	return t.Params.Reasoning.Effort
}

func positiveInt(value int) *int {
	if value <= 0 {
		return nil
	}
	return &value
}

// BindTier creates or replaces a rung. The reference is validated the same
// way loading validates it, so a binding that saves is a binding that loads.
func (c *Config) BindTier(id, label, ref, surface, effort string) error {
	return c.BindTierWithMaxOutput(id, label, ref, surface, effort, 0)
}

// BindTierWithMaxOutput creates or replaces a rung with an explicit output
// cap shared by its primary and every fallback. Zero leaves the provider's
// output allowance unspecified; a negative cap cannot be represented by the
// configuration schema.
func (c *Config) BindTierWithMaxOutput(id, label, ref, surface, effort string, maxOutput int) error {
	if _, err := tierNumber(id); err != nil {
		return err
	}
	if maxOutput < 0 {
		return fmt.Errorf("tier %s max_output %d cannot be negative", id, maxOutput)
	}
	target, err := ParseTarget(ref, surface, effort)
	if err != nil {
		return err
	}
	target.Params.MaxOutputTokens = maxOutput
	tier := Tier{ID: id, Label: label, Target: target}
	for i, t := range c.Tiers {
		if t.ID == id {
			// Binding changes the rung's active model, not its outage policy.
			// Keep an independent copy so a UI model change cannot silently erase
			// hand-configured fallbacks or alias a caller-owned slice. max_output is
			// rung policy, so rebinding updates every retained target together.
			tier.Fallbacks = append([]provider.RouteTarget(nil), t.Fallbacks...)
			for i := range tier.Fallbacks {
				tier.Fallbacks[i].Params.MaxOutputTokens = maxOutput
			}
			c.Tiers[i] = tier
			return nil
		}
	}
	c.Tiers = append(c.Tiers, tier)
	sort.Slice(c.Tiers, func(i, j int) bool {
		a, _ := tierNumber(c.Tiers[i].ID)
		b, _ := tierNumber(c.Tiers[j].ID)
		return a < b
	})
	return nil
}

// BindTierAndSave stages a rung replacement, writes the complete configuration,
// and publishes the staged ladder to the live Config only after the atomic file
// replacement succeeds. A TUI save failure must not leave a binding that exists
// only in memory: later commands would route through a target the file never
// approved, and a subsequent unrelated save could persist it unexpectedly.
func (c *Config) BindTierAndSave(id, label, ref, surface, effort string, maxOutput int) error {
	staged := *c
	staged.Tiers = cloneTiers(c.Tiers)
	if err := staged.BindTierWithMaxOutput(id, label, ref, surface, effort, maxOutput); err != nil {
		return err
	}
	if err := staged.Save(); err != nil {
		return fmt.Errorf("saving configuration: %w", err)
	}
	c.Tiers = staged.Tiers
	c.Path = staged.Path
	return nil
}

// SetProviderBaseURLAndSave stages one endpoint change and publishes it only
// after the complete configuration has been replaced on disk. A failed save
// must not leave a server address active only in memory: a later unrelated
// save could otherwise publish an address the user was told did not stick.
func (c *Config) SetProviderBaseURLAndSave(key, baseURL string) error {
	staged := c.Snapshot()
	staged.SetProviderBaseURL(key, baseURL)
	if err := staged.Save(); err != nil {
		return fmt.Errorf("saving configuration: %w", err)
	}
	c.Providers = staged.Providers
	c.Path = staged.Path
	return nil
}

// SetAuthAndSave stages one provider's credential-resolution settings and
// publishes them only after the atomic file replacement succeeds. Credential
// values themselves never enter Config; this persists only resolver policy.
func (c *Config) SetAuthAndSave(providerName string, settings credential.Settings) error {
	staged := c.Snapshot()
	if staged.Auth == nil {
		staged.Auth = map[string]credential.Settings{}
	}
	staged.Auth[providerName] = settings
	if err := staged.Save(); err != nil {
		return fmt.Errorf("saving configuration: %w", err)
	}
	c.Auth = staged.Auth
	c.Path = staged.Path
	return nil
}

// SetRouteAutoAndSave stages the routing posture and publishes it to the live
// configuration only after the complete file has been replaced. Callers may
// then apply the matching runtime watcher state without a failed save leaving
// the current process and the durable configuration in disagreement.
func (c *Config) SetRouteAutoAndSave(on bool) error {
	staged := c.Snapshot()
	staged.RouteAuto = &on
	if err := staged.Save(); err != nil {
		return fmt.Errorf("saving configuration: %w", err)
	}
	c.RouteAuto = staged.RouteAuto
	c.Path = staged.Path
	return nil
}

// SetCompactAutoAndSave stages automatic compaction so a failed persistence
// attempt cannot silently change when the running session compacts.
func (c *Config) SetCompactAutoAndSave(on bool) error {
	staged := c.Snapshot()
	staged.CompactAuto = on
	if err := staged.Save(); err != nil {
		return fmt.Errorf("saving configuration: %w", err)
	}
	c.CompactAuto = staged.CompactAuto
	c.Path = staged.Path
	return nil
}

// SetCompactAtPercentAndSave stages the compaction threshold and adopts it
// only after the complete configuration is durable.
func (c *Config) SetCompactAtPercentAndSave(percent int) error {
	staged := c.Snapshot()
	staged.CompactAtPercent = percent
	if err := staged.Save(); err != nil {
		return fmt.Errorf("saving configuration: %w", err)
	}
	c.CompactAtPercent = staged.CompactAtPercent
	c.Path = staged.Path
	return nil
}

// SetUpdateChannelAndSave stages the release channel and adopts it only after
// persistence succeeds.
func (c *Config) SetUpdateChannelAndSave(channel string) error {
	staged := c.Snapshot()
	staged.UpdateChannel = channel
	if err := staged.Save(); err != nil {
		return fmt.Errorf("saving configuration: %w", err)
	}
	c.UpdateChannel = staged.UpdateChannel
	c.Path = staged.Path
	return nil
}

// SetUpdateAutoAndSave stages automatic updates and adopts the setting only
// after persistence succeeds.
func (c *Config) SetUpdateAutoAndSave(on bool) error {
	staged := c.Snapshot()
	staged.UpdateAuto = on
	if err := staged.Save(); err != nil {
		return fmt.Errorf("saving configuration: %w", err)
	}
	c.UpdateAuto = staged.UpdateAuto
	c.Path = staged.Path
	return nil
}

// SetProviderContextWindowAndSave stages one surface's declared context
// allowance. The live provider map is replaced only after the file is, so a
// failed /context command cannot affect routing or a later unrelated save.
func (c *Config) SetProviderContextWindowAndSave(key string, tokens int) error {
	staged := c.Snapshot()
	staged.SetProviderContextWindow(key, tokens)
	if err := staged.Save(); err != nil {
		return fmt.Errorf("saving configuration: %w", err)
	}
	c.Providers = staged.Providers
	c.Path = staged.Path
	return nil
}

// RemoveTierAndSave stages a rung removal, preserving the live ladder when
// persistence fails. The bool is false only when no such rung exists.
func (c *Config) RemoveTierAndSave(id string) (bool, error) {
	staged := c.Snapshot()
	if !staged.RemoveTier(id) {
		return false, nil
	}
	if err := staged.Save(); err != nil {
		return true, fmt.Errorf("saving configuration: %w", err)
	}
	c.Tiers = staged.Tiers
	c.Path = staged.Path
	return true, nil
}

func cloneTiers(tiers []Tier) []Tier {
	if tiers == nil {
		return nil
	}
	cloned := make([]Tier, len(tiers))
	for i, tier := range tiers {
		cloned[i] = tier
		cloned[i].Target = cloneTarget(tier.Target)
		if tier.Fallbacks != nil {
			cloned[i].Fallbacks = make([]provider.RouteTarget, len(tier.Fallbacks))
			for j, fallback := range tier.Fallbacks {
				cloned[i].Fallbacks[j] = cloneTarget(fallback)
			}
		}
	}
	return cloned
}

func cloneTarget(target provider.RouteTarget) provider.RouteTarget {
	if target.Params.Temperature != nil {
		temperature := *target.Params.Temperature
		target.Params.Temperature = &temperature
	}
	if target.Params.Reasoning != nil {
		reasoning := *target.Params.Reasoning
		target.Params.Reasoning = &reasoning
	}
	return target
}

// RemoveTier drops a rung. Remaining tiers keep their IDs: t3 does not become
// t2 because t2 left, since sessions and eval records name tiers by ID and a
// renumber would silently repoint them.
func (c *Config) RemoveTier(id string) bool {
	for i, t := range c.Tiers {
		if t.ID == id {
			c.Tiers = append(c.Tiers[:i], c.Tiers[i+1:]...)
			return true
		}
	}
	return false
}
