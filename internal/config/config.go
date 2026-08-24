// Package config reads the user's tier ladder and slot bindings.
//
// Tiers are an ordered quality-and-compute policy ladder, identified as t1
// through tN with user-assignable labels. The ordering is the user's intent,
// not a claim that model capability is globally one-dimensional: two tiers may
// bind the same model at different effort, and a cloud-hosted small model may
// outrun a local large one (§3.1).
package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
)

const FileName = "config.toml"

// MaxTiers is a soft ceiling. §21.1 leaves open whether the eval set and the
// interface stay comprehensible past roughly six tiers; refusing outright
// would answer a question the design deliberately left open, so this is high
// enough not to constrain experiments and low enough to catch a typo.
const MaxTiers = 32

// Tier binds one rung of the ladder to a route target.
type Tier struct {
	ID     string
	Label  string
	Target provider.RouteTarget

	// Fallbacks is the ordered list of targets that may serve this tier when
	// its primary cannot be reached (§5.4). Every entry was written into the
	// config by the user, which is what makes each an approved destination;
	// the substitution still renders before content is sent. Entries use the
	// provider's default serving surface. A tier's max_output is rung policy,
	// so every fallback carries the same concrete output cap as the primary.
	Fallbacks []provider.RouteTarget
}

func (t Tier) String() string {
	if t.Label == "" {
		return fmt.Sprintf("%s  %s", t.ID, t.Target.Display())
	}
	return fmt.Sprintf("%s  %-10s %s", t.ID, t.Label, t.Target.Display())
}

type Config struct {
	// Tiers is ordered ascending by the user's intended quality and compute
	// policy. It may be empty, which means no ladder is configured.
	Tiers []Tier

	// Slots bind named roles to a model or to a tier alias such as "t1".
	Slots map[string]string

	// Auth is keyed by provider name.
	Auth map[string]credential.Settings

	// Providers holds per-provider endpoint overrides, keyed by provider name.
	Providers map[string]ProviderSettings

	// UpdateCheck controls the release check the TUI runs at startup. It is
	// operational traffic, not telemetry (§16/§18): one request naming only the
	// running version. Default on; [updates] check = false or
	// SB_NO_UPDATE_CHECK=1 turns it off.
	UpdateCheck bool

	// UpdateAuto installs a newer release in the background when the startup
	// check finds one, leaving the running process alone; the new binary runs
	// on the next start. Default off: release checks only show a notice unless
	// the user explicitly opts in. Installs owned by a package manager are
	// detected and never touched regardless of this setting (§18).
	UpdateAuto bool

	// UpdateChannel is "stable" (release tags only, the default) or "beta"
	// (prereleases count too).
	UpdateChannel string

	// CompactAuto summarizes the session into a fresh context automatically
	// when the window crosses CompactAtPercent. Default on: the alternative
	// is a session that works until the moment it cannot, with the failure
	// arriving as a provider error instead of a visible handoff.
	CompactAuto bool

	// CompactAtPercent is how full the window gets before auto-compaction,
	// as a percentage. Default 85.
	CompactAtPercent int

	// RouteAuto lets the routing policy choose the rung: the opening route at
	// each user turn, and the escalation policy moving the primary mid-task on
	// its own signals. Default on: a visible move beats a session spent on the
	// wrong rung. Off keeps every rung change the user's — the current rung is
	// held the way a pin holds it, hard checks included — and the signals are
	// still detected and recorded, so /why answers what would have happened.
	// Read it through RouteAutoOn.
	RouteAuto *bool

	// Permissions are the standing answers this user has written down. They
	// are the engine's own rules, and the surface puts them ahead of any rule
	// a server declared for itself: the user's file outranks a server's
	// self-description, while a deny in either wins over every allow.
	Permissions []permission.Rule

	// Destinations names the providers a turn may be routed to. Empty means
	// no restriction, which is the default and the only value most workspaces
	// ever want. It is a hard requirement rather than a preference: the router
	// checks it before economics, so a workspace that may not talk to a vendor
	// reports the exclusion as policy and never as a price (§8.1).
	//
	// Provider names, not metering classes. "local only" is a conclusion about
	// where a server runs, and a provider name is the only part of a target
	// identity this program can state without guessing: an OpenAI-compatible
	// endpoint may be a laptop or a data centre and the name says neither.
	Destinations []string

	// Theme is the TUI color theme, persisted so /theme survives a restart.
	// Empty means the built-in default; the TUI owns what names are valid.
	Theme string

	// Notify rings the terminal bell when a turn finishes or a permission
	// ask arrives, so a session left in another pane says when it needs its
	// person. Nil is the default, which is on: a config built in code and
	// saved must not quietly persist an opinion nobody stated. Read it
	// through NotifyOn.
	Notify *bool

	// Mouse hands the terminal's mouse to the TUI, where the wheel scrolls
	// the transcript and a click expands a tool rail. Nil is the default,
	// which is on: the wheel is how most people scroll, and drag-to-select
	// still works through the terminal's modifier — shift, option, or fn by
	// terminal — because a terminal reporting mouse events to a program
	// needs the modifier to know a drag is its own. Read it through MouseOn.
	Mouse *bool

	// Budget is a per-session dollar ceiling, persisted so /budget survives a
	// restart. Zero means no ceiling. It governs what the catalog prices in
	// dollars; a local rung consumes nothing scarce and a plan rung consumes
	// quota, and neither is what this bounds (§4, §15).
	Budget catalog.Money

	// Sandbox is command confinement, independent of permission mode. Off is
	// the zero value and default; on requires verified confinement at session
	// assembly; auto uses it when verified and stays visibly off otherwise.
	Sandbox execution.SandboxMode

	// Profiles are alternate ladders for other workloads, selected at
	// launch with -profile: a review ladder that opens high, a docs ladder
	// that never leaves the local rung. A profile holds tiers and nothing
	// else — slots, auth, and settings stay global — because the ladder is
	// what a workload changes.
	Profiles map[string][]Tier

	// ActiveProfile names the launch selection, empty when the main ladder
	// runs. While a profile is active, Tiers holds its ladder and mainTiers
	// keeps the main one for the file: a save under a profile must not
	// overwrite the main ladder with the profile's rungs.
	ActiveProfile string
	mainTiers     []Tier

	Path string
}

// Snapshot returns a deep, independently readable copy of the effective
// configuration. TUI discovery runs off the event-loop goroutine; handing it
// the live maps and tier slices would race a later /setup or /models write even
// when the stale UI result itself is rejected.
func (c *Config) Snapshot() *Config {
	if c == nil {
		return nil
	}
	out := *c
	out.Tiers = cloneTiers(c.Tiers)
	out.mainTiers = cloneTiers(c.mainTiers)
	out.Slots = maps.Clone(c.Slots)
	out.Providers = maps.Clone(c.Providers)
	out.Permissions = slices.Clone(c.Permissions)
	for i := range out.Permissions {
		out.Permissions[i].ArgvPrefix = slices.Clone(c.Permissions[i].ArgvPrefix)
		if c.Permissions[i].Shell != nil {
			value := *c.Permissions[i].Shell
			out.Permissions[i].Shell = &value
		}
	}
	out.Destinations = slices.Clone(c.Destinations)
	out.Auth = make(map[string]credential.Settings, len(c.Auth))
	for name, settings := range c.Auth {
		settings.Helper = slices.Clone(settings.Helper)
		settings.OAuth.Scopes = slices.Clone(settings.OAuth.Scopes)
		settings.OAuth.ExtraAuthParams = maps.Clone(settings.OAuth.ExtraAuthParams)
		out.Auth[name] = settings
	}
	out.Profiles = make(map[string][]Tier, len(c.Profiles))
	for name, tiers := range c.Profiles {
		out.Profiles[name] = cloneTiers(tiers)
	}
	out.RouteAuto = cloneBool(c.RouteAuto)
	out.Notify = cloneBool(c.Notify)
	out.Mouse = cloneBool(c.Mouse)
	return &out
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

// ApplyProfile swaps the active ladder for the named profile's. Launch time
// only: the ladder feeds session assembly and the frozen zone, and a swap
// mid-session would repoint records that name tiers by id.
func (c *Config) ApplyProfile(name string) error {
	ladder, ok := c.Profiles[name]
	if !ok {
		if len(c.Profiles) == 0 {
			return fmt.Errorf("no profiles are configured; a [profiles.%s.tiers.t1] section in %s declares one", name, c.Path)
		}
		names := make([]string, 0, len(c.Profiles))
		for n := range c.Profiles {
			names = append(names, n)
		}
		sort.Strings(names)
		return fmt.Errorf("profile %q is not configured; the file holds %s", name, strings.Join(names, ", "))
	}
	if len(ladder) == 0 {
		return fmt.Errorf("profile %q has no tiers; an empty ladder cannot open a session", name)
	}
	c.mainTiers = c.Tiers
	c.Tiers = ladder
	c.ActiveProfile = name
	return nil
}

type ProviderSettings struct {
	BaseURL string

	// ContextWindow is how many tokens this endpoint will accept, for a
	// target whose window nothing else can report. The chat-completions
	// format has no field for it and the catalog cannot price a server it has
	// never seen, so on a compatible endpoint the number is the user's to
	// state. Zero means unknown, which is not the same as unlimited.
	ContextWindow int
}

// ProviderFor returns the settings for a provider, which is the zero value when
// none are configured: every adapter has a default address.
func (c *Config) ProviderFor(name string) ProviderSettings {
	return c.Providers[name]
}

// ProviderForTarget returns the settings that govern one serving surface.
//
// A surface-qualified key wins over the provider-wide one, because a provider
// whose whole reason for existing is that it fronts arbitrary servers has more
// than one address at a time: openaicompat/ollama is the local server's
// compatibility endpoint and openaicompat/generic is whatever the user pointed
// it at, and redirecting one must not silently redirect the other.
func (c *Config) ProviderForTarget(name, surface string) ProviderSettings {
	wide := c.Providers[name]
	if surface == "" {
		return wide
	}
	scoped, ok := c.Providers[ProviderSurfaceKey(name, surface)]
	if !ok {
		return wide
	}
	// Each field falls back on its own. A surface that states only its window
	// still inherits the provider-wide address, and stating only an address
	// does not erase a window set beside it.
	if scoped.BaseURL == "" {
		scoped.BaseURL = wide.BaseURL
	}
	if scoped.ContextWindow == 0 {
		scoped.ContextWindow = wide.ContextWindow
	}
	return scoped
}

// ProviderSurfaceKey is the config key one surface's endpoint is written
// under, so the reader and the writer cannot spell it differently.
func ProviderSurfaceKey(name, surface string) string { return name + "/" + surface }

// SetProviderBaseURL records an endpoint, or forgets one when the address is
// empty: an entry with a blank address would be written to the file as a
// setting that says nothing.
func (c *Config) SetProviderBaseURL(key, baseURL string) {
	baseURL = strings.TrimSpace(baseURL)
	settings := c.Providers[key]
	settings.BaseURL = strings.TrimSuffix(baseURL, "/")
	c.setProvider(key, settings)
}

// SetProviderContextWindow records how large a window this endpoint accepts.
// Zero forgets it, because an unknown window and a window of zero are the
// same claim and neither is worth a line in the file.
func (c *Config) SetProviderContextWindow(key string, tokens int) {
	settings := c.Providers[key]
	settings.ContextWindow = tokens
	c.setProvider(key, settings)
}

func (c *Config) setProvider(key string, settings ProviderSettings) {
	if settings == (ProviderSettings{}) {
		delete(c.Providers, key)
		return
	}
	if c.Providers == nil {
		c.Providers = map[string]ProviderSettings{}
	}
	c.Providers[key] = settings
}

// AuthFor returns the credential settings for a provider, which is the zero
// value when none are configured: the default chain still works, because the
// environment and the platform store need no configuration.
func (c *Config) AuthFor(providerName string) credential.Settings {
	return c.Auth[providerName]
}

// RouteAutoOn is how the routing setting is read: absent means on.
func (c *Config) RouteAutoOn() bool { return c.RouteAuto == nil || *c.RouteAuto }

// NotifyOn is how the bell setting is read: absent means on.
func (c *Config) NotifyOn() bool {
	return c.Notify == nil || *c.Notify
}

// MouseOn is how the mouse setting is read: absent means on.
func (c *Config) MouseOn() bool { return c.Mouse == nil || *c.Mouse }

// Default returns the tier a session starts on. The bottom of the ladder is
// the deliberate default: an escalation the user can see beats a silent spend
// they cannot (design principle 3).
func (c *Config) Default() (Tier, bool) {
	if len(c.Tiers) == 0 {
		return Tier{}, false
	}
	return c.Tiers[0], true
}

func (c *Config) Tier(id string) (Tier, bool) {
	for _, t := range c.Tiers {
		if t.ID == id {
			return t, true
		}
	}
	return Tier{}, false
}

type file struct {
	Tiers     map[string]tierEntry     `toml:"tiers"`
	Profiles  map[string]profileEntry  `toml:"profiles"`
	Slots     map[string]string        `toml:"slots"`
	Auth      map[string]authEntry     `toml:"auth"`
	Providers map[string]providerEntry `toml:"providers"`
	Updates   updatesEntry             `toml:"updates"`
	Compact   compactEntry             `toml:"compact"`
	Routing   routingEntry             `toml:"routing"`
	UI        uiEntry                  `toml:"ui"`
	Limits    limitsEntry              `toml:"limits"`
	Execution executionEntry           `toml:"execution"`

	// Permissions is a list rather than a table because order decides which
	// non-deny rule answers first, and a TOML table has no order.
	Permissions []permissionEntry `toml:"permissions"`
}

// any accepts both the concise sandbox = true form and the named
// off/on/auto form. The loader normalizes both into execution.SandboxMode.
type executionEntry struct {
	Sandbox any `toml:"sandbox,omitempty"`
}

// profileEntry is one alternate ladder. Deliberately tiers-only: a key that
// tried to override slots or auth per profile would be refused by the
// undecoded-keys check, which is the honest answer until a workload proves
// it needs more than a different ladder.
type profileEntry struct {
	Tiers map[string]tierEntry `toml:"tiers"`
}

// limitsEntry holds the spending ceiling. Money's own text form is what the
// file reads and writes, so the value is "2.50", not a count of micro-dollars.
type limitsEntry struct {
	Budget catalog.Money `toml:"budget,omitempty"`
}

// compactEntry holds the auto-compaction settings. Auto is a *bool so
// "absent" and "explicitly off" are different facts: the default is on.
type compactEntry struct {
	Auto      *bool `toml:"auto,omitempty"`
	AtPercent int   `toml:"at_percent,omitempty"`
}

// routingEntry holds the escalation setting. Auto is a *bool so "absent" and
// "explicitly off" are different facts: the default is on.
type routingEntry struct {
	Auto         *bool    `toml:"auto,omitempty"`
	Destinations []string `toml:"destinations,omitempty"`
}

// uiEntry holds presentation settings. They live in the config rather than a
// separate state file because the TUI writes this file anyway, and two files
// that both mean "how sb behaves for this user" is one file too many. Notify
// and Mouse are *bool so "absent" and "explicitly off" are different facts:
// the defaults are on.
type uiEntry struct {
	Theme  string `toml:"theme,omitempty"`
	Notify *bool  `toml:"notify,omitempty"`
	Mouse  *bool  `toml:"mouse,omitempty"`
}

// updatesEntry holds the update settings. Booleans are *bool because the
// release check defaults on while automatic installation defaults off.
type updatesEntry struct {
	Check   *bool  `toml:"check,omitempty"`
	Auto    *bool  `toml:"auto,omitempty"`
	Channel string `toml:"channel,omitempty"`
}

// providerEntry redirects a provider at a different endpoint. A gateway, an
// Azure deployment, a self-hosted proxy, and a corporate egress point all need
// this, and hardcoding one address per vendor would make every one of them a
// code change.
//
// It does not change target identity. A provider reached at another address is
// still that provider as far as the catalog and the credential are concerned,
// so redirecting to something that prices differently is the user asserting
// they know that.
type providerEntry struct {
	BaseURL       string `toml:"base_url"`
	ContextWindow int    `toml:"context_window,omitempty"`
}

// authEntry configures where a provider's credential comes from. It carries no
// field for the credential itself: §5.3 keeps secrets out of this file, and a
// key that exists is a key someone pastes a secret into.
type authEntry struct {
	Env    string      `toml:"env,omitempty"`
	Helper []string    `toml:"helper,omitempty"`
	OAuth  *oauthEntry `toml:"oauth,omitempty"`
}

// oauthEntry configures a login flow. It has a client id and no client secret,
// because a command-line program cannot keep one: this is a public client and
// PKCE is what stands in for a secret. A field for one would invite storing it
// here in the clear, which is the thing §5.3 rules out.
type oauthEntry struct {
	ClientID     string            `toml:"client_id"`
	AuthorizeURL string            `toml:"authorize_url"`
	TokenURL     string            `toml:"token_url"`
	Scopes       []string          `toml:"scopes,omitempty"`
	Audience     string            `toml:"audience,omitempty"`
	RedirectPort int               `toml:"redirect_port,omitempty"`
	ExtraParams  map[string]string `toml:"extra_params,omitempty"`
}

type tierEntry struct {
	Label     string   `toml:"label,omitempty"`
	Model     string   `toml:"model"`
	Surface   string   `toml:"surface,omitempty"`
	Effort    string   `toml:"effort,omitempty"`
	MaxOutput *int     `toml:"max_output,omitempty"`
	Fallback  []string `toml:"fallback,omitempty"`
}

// Load reads the user's configuration. A missing file is not an error: the
// tool runs without one, driven entirely by flags.
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return &Config{
			Slots:            map[string]string{},
			Auth:             map[string]credential.Settings{},
			Providers:        map[string]ProviderSettings{},
			UpdateCheck:      true,
			UpdateAuto:       false,
			CompactAuto:      true,
			CompactAtPercent: 85,
			Sandbox:          execution.SandboxOff,
		}, nil
	}
	return LoadFile(path)
}

func LoadFile(path string) (*Config, error) {
	c := &Config{
		Slots:            map[string]string{},
		Auth:             map[string]credential.Settings{},
		Providers:        map[string]ProviderSettings{},
		UpdateCheck:      true,
		UpdateAuto:       false,
		CompactAuto:      true,
		CompactAtPercent: 85,
		Sandbox:          execution.SandboxOff,
		Path:             path,
	}

	read, found, err := readConfigBytes(path)
	if err != nil {
		return nil, err
	}
	if !found {
		return c, nil
	}
	defer read.Close()
	data := read.data

	var f file
	meta, err := toml.Decode(string(data), &f)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if undecoded := meta.Undecoded(); len(undecoded) > 0 {
		// A misspelled key that is silently ignored is a configuration the
		// user believes is in effect and is not.
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("%s: unrecognized settings: %s", path, strings.Join(keys, ", "))
	}

	for k, v := range f.Slots {
		c.Slots[k] = v
	}
	for k, v := range f.Auth {
		s := credential.Settings{Env: v.Env, Helper: v.Helper}
		if v.OAuth != nil {
			s.OAuth = credential.OAuthSettings{
				ClientID:        v.OAuth.ClientID,
				AuthorizeURL:    v.OAuth.AuthorizeURL,
				TokenURL:        v.OAuth.TokenURL,
				Scopes:          v.OAuth.Scopes,
				Audience:        v.OAuth.Audience,
				RedirectPort:    v.OAuth.RedirectPort,
				ExtraAuthParams: v.OAuth.ExtraParams,
			}
		}
		c.Auth[k] = s
	}
	for k, v := range f.Providers {
		if v.ContextWindow < 0 {
			return nil, fmt.Errorf("%s: providers.%s context_window %d is negative", path, k, v.ContextWindow)
		}
		c.Providers[k] = ProviderSettings{BaseURL: v.BaseURL, ContextWindow: v.ContextWindow}
	}
	if f.Updates.Check != nil {
		c.UpdateCheck = *f.Updates.Check
	}
	if f.Updates.Auto != nil {
		c.UpdateAuto = *f.Updates.Auto
	}
	switch f.Updates.Channel {
	case "", "stable", "beta":
		c.UpdateChannel = f.Updates.Channel
	default:
		// The §16/§18 posture on configuration mistakes: a value that is
		// silently ignored is a setting the user believes is in effect.
		return nil, fmt.Errorf("%s: updates.channel %q is not stable or beta", path, f.Updates.Channel)
	}
	if f.Compact.Auto != nil {
		c.CompactAuto = *f.Compact.Auto
	}
	c.RouteAuto = f.Routing.Auto
	c.Destinations = f.Routing.Destinations
	permissions, err := buildPermissionRules(f.Permissions, path)
	if err != nil {
		return nil, err
	}
	c.Permissions = permissions
	if f.Compact.AtPercent != 0 {
		if f.Compact.AtPercent < 50 || f.Compact.AtPercent > 95 {
			// Below half the window it would compact constantly; above 95 it
			// would fire after the request that already failed.
			return nil, fmt.Errorf("%s: compact.at_percent %d is outside 50–95", path, f.Compact.AtPercent)
		}
		c.CompactAtPercent = f.Compact.AtPercent
	}
	c.Theme = f.UI.Theme
	c.Notify = f.UI.Notify
	c.Mouse = f.UI.Mouse
	if f.Execution.Sandbox != nil {
		var raw string
		switch value := f.Execution.Sandbox.(type) {
		case bool:
			if value {
				raw = "on"
			} else {
				raw = "off"
			}
		case string:
			raw = value
		default:
			return nil, fmt.Errorf("%s: execution.sandbox must be true, false, off, on, or auto", path)
		}
		mode, parseErr := execution.ParseSandboxMode(raw)
		if parseErr != nil {
			return nil, fmt.Errorf("%s: execution.sandbox: %w", path, parseErr)
		}
		c.Sandbox = mode
	}
	if f.Limits.Budget < 0 {
		return nil, fmt.Errorf("%s: limits.budget %s is negative; a ceiling below zero rules out every turn", path, f.Limits.Budget)
	}
	c.Budget = f.Limits.Budget
	if err := c.buildTiers(f.Tiers, path); err != nil {
		return nil, err
	}
	if len(f.Profiles) > 0 {
		c.Profiles = make(map[string][]Tier, len(f.Profiles))
		for name, p := range f.Profiles {
			ladder, err := buildTierList(p.Tiers, path, "profile "+name+" ")
			if err != nil {
				return nil, err
			}
			if len(ladder) == 0 {
				return nil, fmt.Errorf("%s: profile %s has no tiers; an empty ladder cannot open a session", path, name)
			}
			c.Profiles[name] = ladder
		}
	}
	if err := read.Verify(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) buildTiers(entries map[string]tierEntry, path string) error {
	tiers, err := buildTierList(entries, path, "")
	if err != nil {
		return err
	}
	c.Tiers = tiers
	return nil
}

// buildTierList validates and orders one ladder's entries. The where prefix
// scopes error messages, so a broken rung inside a profile names the
// profile rather than pointing at the main ladder.
func buildTierList(entries map[string]tierEntry, path, where string) ([]Tier, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	if len(entries) > MaxTiers {
		return nil, fmt.Errorf("%s: %d %stiers configured, more than the %d ceiling", path, len(entries), where, MaxTiers)
	}

	ids := make([]string, 0, len(entries))
	for id := range entries {
		if _, err := tierNumber(id); err != nil {
			return nil, fmt.Errorf("%s: %s%w", path, where, err)
		}
		ids = append(ids, id)
	}
	// Numeric order, not lexical: t10 comes after t9.
	sort.Slice(ids, func(i, j int) bool {
		a, _ := tierNumber(ids[i])
		b, _ := tierNumber(ids[j])
		return a < b
	})

	var tiers []Tier
	for _, id := range ids {
		entry := entries[id]
		maxOutput := 0
		if entry.MaxOutput != nil {
			if *entry.MaxOutput <= 0 {
				return nil, fmt.Errorf("%s: %stier %s max_output %d must be positive", path, where, id, *entry.MaxOutput)
			}
			maxOutput = *entry.MaxOutput
		}
		target, err := ParseTarget(entry.Model, entry.Surface, entry.Effort)
		if err != nil {
			return nil, fmt.Errorf("%s: %stier %s: %w", path, where, id, err)
		}
		target.Params.MaxOutputTokens = maxOutput
		tier := Tier{ID: id, Label: entry.Label, Target: target}
		for _, ref := range entry.Fallback {
			fb, err := ParseTarget(ref, "", "")
			if err != nil {
				return nil, fmt.Errorf("%s: %stier %s fallback: %w", path, where, id, err)
			}
			fb.Params.MaxOutputTokens = maxOutput
			tier.Fallbacks = append(tier.Fallbacks, fb)
		}
		tiers = append(tiers, tier)
	}
	return tiers, nil
}

// tierNumber enforces the t1..tN scheme. Numeric IDs are the only scheme that
// generalizes over a configurable N without encoding a capability claim the
// system cannot guarantee (§3.1).
func tierNumber(id string) (int, error) {
	rest, ok := strings.CutPrefix(id, "t")
	if !ok {
		return 0, fmt.Errorf("tier %q must be named t1 through t%d", id, MaxTiers)
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("tier %q must be named t1 through t%d", id, MaxTiers)
	}
	return n, nil
}

// defaultSurfaces records the serving surface assumed when configuration names
// only a provider and a model. Anything else has to be written out, because
// price, cache behavior, and retention differ per surface and guessing one
// would attach the wrong catalog entry.
var defaultSurfaces = map[string]string{
	"ollama":    "local",
	"anthropic": "first-party",
	"openai":    "first-party",
	"kimi":      "coding",

	// An unnamed compatible endpoint is the generic profile: the floor of
	// assumed capability, which is the honest default for a server nobody has
	// characterized. Reaching Ollama's compatibility endpoint is the named
	// case and still has to say surface = "ollama".
	"openaicompat": "generic",
}

// ParseTarget reads a "provider/model" reference into a route target.
//
// The split is on the first slash only, because model identifiers legitimately
// contain slashes: an Ollama model pulled from a registry is named like
// "hf.co/user/model".
func ParseTarget(ref, surface, effort string) (provider.RouteTarget, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return provider.RouteTarget{}, errors.New("no model given")
	}

	providerName, model, ok := strings.Cut(ref, "/")
	if !ok || model == "" {
		return provider.RouteTarget{}, errors.New(
			"model must be written as provider/model, for example ollama/qwen3.5:9b-mlx")
	}
	if err := provider.ValidateModelID(model); err != nil {
		return provider.RouteTarget{}, err
	}

	if surface == "" {
		known, ok := defaultSurfaces[providerName]
		if !ok {
			return provider.RouteTarget{}, fmt.Errorf(
				"provider %q has no default serving surface; set surface explicitly", providerName)
		}
		surface = known
	}

	target := provider.RouteTarget{Provider: providerName, Surface: surface, ModelID: model}
	if effort != "" {
		target.Params.Reasoning = &provider.Reasoning{Enabled: true, Effort: effort}
	}
	return target, nil
}

func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".switchboard", FileName), nil
}
