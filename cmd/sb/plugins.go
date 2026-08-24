package main

// Native plugins are discovered as inventory first and activated only through
// Switchboard's own ledger. Another client's enabled/trusted bit is useful
// provenance, never permission. Prompt-only skills may load from an enabled
// plugin without executable trust; MCP and hook components additionally need a
// digest-bound executable grant before their adapters may see them.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/switchboard-code/switchboard/internal/extensions"
	extnative "github.com/switchboard-code/switchboard/internal/extensions/native"
	"github.com/switchboard-code/switchboard/internal/mcppolicy"
	"github.com/switchboard-code/switchboard/internal/skills"
)

type pluginRecord struct {
	Plugin extensions.Plugin

	NativeIDs          []string
	NativeState        extnative.CandidateState
	NativeEnabled      bool
	ActivationEligible bool
	ManagedDenied      bool
	SwitchboardOwned   bool
	Activation         extensions.ActivationStatus
	Reference          *extensions.ActivationReference
	Provenance         []extnative.Provenance
}

type pluginInventory struct {
	state       *extensions.State
	records     []pluginRecord
	stale       []pluginActivationReference
	diagnostics []mcpNote
}

type pluginActivationReference = extensions.ActivationReference

type pluginInput struct {
	candidate extensions.Candidate
	native    *extnative.ResolvedCandidate
	state     *extensions.Activation
	recovery  string
}

func openPluginInventory(workspace string, full bool) *pluginInventory {
	home, homeErr := os.UserHomeDir()
	state, stateErr := extensions.OpenState()
	inv := discoverPlugins(home, workspace, state, full)
	if homeErr != nil {
		inv.diagnostics = append(inv.diagnostics, mcpNote{"warn", "plugins: home directory unavailable: " + homeErr.Error()})
	}
	if stateErr != nil {
		inv.state = nil
		inv.diagnostics = append(inv.diagnostics, mcpNote{"error", "plugins: activation state is unavailable; all plugins stay off: " + stateErr.Error()})
	}
	return inv
}

func discoverPlugins(home, workspace string, state *extensions.State, full bool) *pluginInventory {
	options, optionsErr := pluginNativeOptions(home, workspace)
	return discoverPluginsWithNativeOptions(home, workspace, state, full, options, optionsErr)
}

// pluginNativeOptions uses the same platform path resolver as managed MCP
// policy. In particular, Claude's system managed-settings.json is not a user
// preference: it is an authoritative deny surface and must be present in the
// plugin resolver before any cached activation contributes behavior.
func pluginNativeOptions(home, workspace string) (extnative.Options, error) {
	options := extnative.DefaultLocalOptions(home, workspace)
	policyWorkspace := workspace
	if strings.TrimSpace(policyWorkspace) == "" {
		policyWorkspace = home
	}
	pathOptions := mcppolicy.Options{
		HomeDir:      home,
		Workspace:    policyWorkspace,
		ProgramData:  os.Getenv("ProgramData"),
		ProgramFiles: os.Getenv("ProgramFiles"),
	}
	if configured := strings.TrimSpace(os.Getenv("CODEX_HOME")); configured != "" {
		pathOptions.CodexConfigDir = configured
		if options.Codex != nil {
			options.Codex.UserConfigPath = filepath.Join(configured, "config.toml")
		}
	}
	if configured := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); configured != "" {
		pathOptions.ClaudeConfigDir = configured
	}
	paths, err := mcppolicy.ResolvePaths(pathOptions)
	if err != nil {
		return options, err
	}
	checker, _, err := mcppolicy.Load(pathOptions)
	if err != nil {
		return options, err
	}
	bindClaudePluginPolicy(&options, checker)
	if options.Claude != nil {
		options.Claude.InstalledPluginsPath = filepath.Join(filepath.Dir(paths.ClaudeUserSettings), "plugins", "installed_plugins.json")
		var settings []extnative.ClaudeSettings
		settings = append(settings, extnative.ClaudeSettings{Path: paths.ClaudeUserSettings, Scope: extensions.ScopeUser})
		if workspace != "" {
			settings = append(settings,
				extnative.ClaudeSettings{Path: paths.ClaudeProjectSettings, Scope: extensions.ScopeWorkspace, ProjectPath: workspace},
				extnative.ClaudeSettings{Path: paths.ClaudeLocalSettings, Scope: extensions.ScopeLocal, ProjectPath: workspace},
			)
		}
		options.Claude.Settings = settings
	}
	return options, nil
}

func bindClaudePluginPolicy(options *extnative.Options, checker *mcppolicy.Checker) {
	constraints, constraintErr := checker.ClaudePluginConstraints()
	if constraintErr != nil {
		options.ClaudeManagedPolicyUnavailable = true
		return
	}
	for _, constraint := range constraints {
		options.ManagedPluginConstraints = append(options.ManagedPluginConstraints, extnative.ManagedPluginConstraint{
			Dialect: extensions.DialectClaude, NativeID: constraint.NativeID, Denied: constraint.Denied,
		})
	}
}

func discoverPluginsWithNativeOptions(home, workspace string, state *extensions.State, full bool, options extnative.Options, optionsErr error) *pluginInventory {
	inv := &pluginInventory{state: state}
	nativeResult := extnative.Result{}
	if full {
		if optionsErr != nil {
			nativeResult.ClaudeManagedPolicyUnavailable = true
			inv.diagnostics = append(inv.diagnostics, mcpNote{"error", "plugins/native managed-policy-path: Claude managed plugin policy is unavailable; Claude plugins stay off: " + optionsErr.Error()})
		} else {
			nativeResult = extnative.Resolve(options)
		}
		for _, diagnostic := range nativeResult.Diagnostics {
			inv.diagnostics = append(inv.diagnostics, pluginNativeNote(diagnostic))
		}
		if nativeResult.ClaudeManagedPolicyUnavailable {
			inv.diagnostics = append(inv.diagnostics, mcpNote{"error", "plugins/native managed-policy-unavailable: an authoritative Claude managed settings source could not be evaluated; Claude plugins stay off"})
		}
	}

	var inputs []pluginInput
	managedDeniedNativeIDs := make(map[string]struct{})
	for _, constraint := range nativeResult.ManagedPluginConstraints {
		if constraint.Denied {
			managedDeniedNativeIDs[string(constraint.Dialect)+"\x00"+constraint.NativeID] = struct{}{}
		}
	}
	activationCandidates := make(map[string]bool)
	if state != nil {
		references, err := state.ActivationReferencesFor(workspace)
		if err != nil {
			inv.diagnostics = append(inv.diagnostics, mcpNote{"error", "plugins: cannot resolve activations for this workspace: " + err.Error()})
		} else {
			for i := range references {
				activation := references[i].Activation
				copyOfActivation := activation
				candidate := extensions.Candidate{Root: activation.RealPath, Scope: activation.Scope, Dialect: activation.Dialect}
				if key, keyErr := pluginCandidateKey(candidate); keyErr == nil {
					activationCandidates[key] = true
				}
				inputs = append(inputs, pluginInput{
					candidate: candidate,
					state:     &copyOfActivation,
					recovery:  references[i].RecoveryToken,
				})
			}
		}
	}
	for i := range nativeResult.Candidates {
		candidate := nativeResult.Candidates[i]
		key, err := pluginCandidateKey(candidate.Candidate)
		if err != nil || !full && !activationCandidates[key] {
			continue
		}
		copyOfCandidate := candidate
		inputs = append(inputs, pluginInput{candidate: candidate.Candidate, native: &copyOfCandidate})
	}

	candidates := make([]extensions.Candidate, 0, len(inputs))
	seenCandidates := make(map[string]bool)
	for _, input := range inputs {
		key, err := pluginCandidateKey(input.candidate)
		if err != nil || seenCandidates[key] {
			continue
		}
		seenCandidates[key] = true
		candidates = append(candidates, input.candidate)
	}
	discovered := extensions.Discover(candidates)
	for _, diagnostic := range discovered.Diagnostics {
		inv.diagnostics = append(inv.diagnostics, pluginDiscoveryNote(diagnostic))
	}

	byCandidate := make(map[string][]pluginInput)
	for _, input := range inputs {
		key, err := pluginCandidateKey(input.candidate)
		if err != nil {
			continue
		}
		byCandidate[key] = append(byCandidate[key], input)
	}
	seenActivations := make(map[string]bool)
	for _, plugin := range discovered.Plugins {
		key := pluginKey(plugin)
		record := pluginRecord{Plugin: plugin}
		for _, input := range byCandidate[key] {
			if input.native != nil {
				record.NativeIDs = append(record.NativeIDs, input.native.NativeID)
				record.NativeEnabled = record.NativeEnabled || input.native.NativeEnabled
				if input.native.State == extnative.CandidateInstalled {
					record.NativeState = extnative.CandidateInstalled
				} else if record.NativeState == "" {
					record.NativeState = input.native.State
				}
				record.ActivationEligible = record.ActivationEligible ||
					(input.native.State == extnative.CandidateInstalled && input.native.ActivationEligible)
				record.ManagedDenied = record.ManagedDenied || input.native.ManagedDenied
				record.Provenance = append(record.Provenance, input.native.Provenance)
			}
			if input.state != nil {
				record.NativeIDs = append(record.NativeIDs, input.state.NativeIDs...)
				activationKey := pluginActivationDisplayKey(*input.state)
				if input.state.ID != plugin.ID || filepath.Clean(input.state.RealPath) != plugin.RealPath {
					inv.diagnostics = append(inv.diagnostics, mcpNote{"error", fmt.Sprintf(
						"plugins: saved activation %s resolved as %s at %s; it stays off",
						input.state.ID, plugin.ID, plugin.RealPath)})
					continue
				}
				seenActivations[activationKey] = true
				record.SwitchboardOwned = true
				record.ActivationEligible = true
				record.Reference = &extensions.ActivationReference{
					Activation: *input.state, RecoveryToken: input.recovery,
				}
			}
		}
		record.NativeIDs = uniqueStrings(record.NativeIDs)
		for _, nativeID := range record.NativeIDs {
			if _, denied := managedDeniedNativeIDs[string(record.Plugin.Dialect)+"\x00"+nativeID]; denied {
				record.ManagedDenied = true
			}
		}
		if len(record.NativeIDs) == 0 && record.Plugin.Dialect == extensions.DialectClaude &&
			legacyClaudeNamespaceDenied(record.Plugin.Namespace, managedDeniedNativeIDs) {
			// Version-1 activations did not persist the marketplace-qualified
			// identity. A managed deny for the same native namespace therefore
			// fails closed until the activation is explicitly recreated.
			record.ManagedDenied = true
		}
		sort.Slice(record.Provenance, func(i, j int) bool {
			if record.Provenance[i].NativeID != record.Provenance[j].NativeID {
				return record.Provenance[i].NativeID < record.Provenance[j].NativeID
			}
			return record.Provenance[i].RegistryPath < record.Provenance[j].RegistryPath
		})
		if state != nil && record.ActivationEligible {
			record.Activation = state.Status(plugin, workspace)
		}
		if nativeResult.ClaudeManagedPolicyUnavailable && plugin.Dialect == extensions.DialectClaude {
			record.ManagedDenied = true
		}
		inv.records = append(inv.records, record)
	}

	for _, input := range inputs {
		if input.state == nil || seenActivations[pluginActivationDisplayKey(*input.state)] {
			continue
		}
		inv.diagnostics = append(inv.diagnostics, mcpNote{"error", fmt.Sprintf(
			"plugins: enabled plugin %s at %s could not be rediscovered; it stays off",
			input.state.ID, input.state.RealPath)})
		inv.stale = append(inv.stale, pluginActivationReference{
			Activation:    *input.state,
			RecoveryToken: input.recovery,
		})
	}
	inv.records = coalesceOwnedPluginRecords(inv.records)
	for i := range inv.records {
		if nativeResult.ClaudeManagedPolicyUnavailable && inv.records[i].Plugin.Dialect == extensions.DialectClaude {
			inv.records[i].ManagedDenied = true
		}
	}
	sort.Slice(inv.stale, func(i, j int) bool {
		left, right := inv.stale[i].Activation, inv.stale[j].Activation
		if left.ID != right.ID {
			return left.ID < right.ID
		}
		if left.RealPath != right.RealPath {
			return left.RealPath < right.RealPath
		}
		return left.Workspace < right.Workspace
	})

	sort.Slice(inv.records, func(i, j int) bool {
		if inv.records[i].Plugin.ID != inv.records[j].Plugin.ID {
			return inv.records[i].Plugin.ID < inv.records[j].Plugin.ID
		}
		if inv.records[i].Plugin.Scope != inv.records[j].Plugin.Scope {
			return inv.records[i].Plugin.Scope < inv.records[j].Plugin.Scope
		}
		return inv.records[i].Plugin.RealPath < inv.records[j].Plugin.RealPath
	})
	sort.SliceStable(inv.diagnostics, func(i, j int) bool {
		if inv.diagnostics[i].level != inv.diagnostics[j].level {
			return inv.diagnostics[i].level == "error"
		}
		return inv.diagnostics[i].text < inv.diagnostics[j].text
	})
	return inv
}

func legacyClaudeNamespaceDenied(namespace string, denied map[string]struct{}) bool {
	prefix := string(extensions.DialectClaude) + "\x00"
	for key := range denied {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		nativeID := strings.TrimPrefix(key, prefix)
		if index := strings.LastIndex(nativeID, "@"); index > 0 && nativeID[:index] == namespace {
			return true
		}
	}
	return false
}

// coalesceOwnedPluginRecords keeps the Switchboard cache as the sole
// authority-bearing record while retaining matching native provenance. A
// byte-identical native install or marketplace source remains visible in the
// NATIVE column, but it cannot create a second skill namespace or make the
// canonical selector ambiguous. A different digest stays separate so updates
// remain visible and cannot inherit activation or trust.
func coalesceOwnedPluginRecords(records []pluginRecord) []pluginRecord {
	owner := make(map[string]int)
	for i, record := range records {
		if record.SwitchboardOwned {
			owner[pluginContentKey(record.Plugin)] = i
		}
	}
	removed := make(map[int]bool)
	for i, record := range records {
		if record.SwitchboardOwned {
			continue
		}
		ownedIndex, ok := owner[pluginContentKey(record.Plugin)]
		if !ok {
			continue
		}
		owned := &records[ownedIndex]
		owned.NativeIDs = uniqueStrings(append(owned.NativeIDs, record.NativeIDs...))
		owned.NativeEnabled = owned.NativeEnabled || record.NativeEnabled
		owned.ManagedDenied = owned.ManagedDenied || record.ManagedDenied
		if record.NativeState == extnative.CandidateInstalled {
			owned.NativeState = extnative.CandidateInstalled
		} else if owned.NativeState == "" {
			owned.NativeState = record.NativeState
		}
		owned.Provenance = append(owned.Provenance, record.Provenance...)
		removed[i] = true
	}
	out := records[:0]
	for i, record := range records {
		if removed[i] {
			continue
		}
		record.NativeIDs = uniqueStrings(record.NativeIDs)
		sort.Slice(record.Provenance, func(i, j int) bool {
			if record.Provenance[i].NativeID != record.Provenance[j].NativeID {
				return record.Provenance[i].NativeID < record.Provenance[j].NativeID
			}
			return record.Provenance[i].RegistryPath < record.Provenance[j].RegistryPath
		})
		out = append(out, record)
	}
	return out
}

func (record pluginRecord) behaviorEnabled() bool {
	return record.Activation.Enabled && !record.ManagedDenied
}

func pluginContentKey(plugin extensions.Plugin) string {
	return strings.Join([]string{plugin.ID, string(plugin.Dialect), plugin.Digest}, "\x00")
}

func pluginCandidateKey(candidate extensions.Candidate) (string, error) {
	abs, err := filepath.Abs(candidate.Root)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{string(candidate.Dialect), string(candidate.Scope), filepath.Clean(real)}, "\x00"), nil
}

func pluginKey(plugin extensions.Plugin) string {
	return strings.Join([]string{string(plugin.Dialect), string(plugin.Scope), plugin.RealPath}, "\x00")
}

func pluginActivationDisplayKey(activation extensions.Activation) string {
	return strings.Join([]string{
		activation.ID, string(activation.Dialect), string(activation.Scope),
		filepath.Clean(activation.RealPath), filepath.Clean(activation.Workspace),
	}, "\x00")
}

func pluginNativeNote(diagnostic extensions.Diagnostic) mcpNote {
	level := "warn"
	if diagnostic.Severity == extensions.SeverityError {
		level = "error"
	}
	text := "plugins/native " + diagnostic.Code + ": " + diagnostic.Message
	if diagnostic.Path != "" {
		text += " (" + diagnostic.Path + ")"
	}
	return mcpNote{level, text}
}

func pluginDiscoveryNote(diagnostic extensions.Diagnostic) mcpNote {
	level := "warn"
	if diagnostic.Severity == extensions.SeverityError {
		level = "error"
	}
	text := "plugins " + diagnostic.Code + ": " + diagnostic.Message
	if diagnostic.Path != "" {
		text += " (" + diagnostic.Path + ")"
	}
	return mcpNote{level, text}
}

func uniqueStrings(values []string) []string {
	sort.Strings(values)
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}

func (inv *pluginInventory) enabledSkillRoots() ([]skills.AdditionalRoot, []mcpNote) {
	if inv == nil {
		return nil, nil
	}
	enabledIDCounts := make(map[string]int)
	for _, record := range inv.records {
		if record.behaviorEnabled() {
			enabledIDCounts[record.Plugin.ID]++
		}
	}
	var roots []skills.AdditionalRoot
	var notes []mcpNote
	for _, record := range inv.records {
		if !record.behaviorEnabled() {
			continue
		}
		if enabledIDCounts[record.Plugin.ID] > 1 {
			notes = append(notes, mcpNote{"error", fmt.Sprintf(
				"plugins: %s is enabled from more than one root; all of its skills stay off", record.Plugin.ID)})
			continue
		}
		dialect, ok := pluginSkillDialect(record.Plugin.Dialect)
		if !ok {
			continue
		}
		scope, ok := pluginSkillScope(record.Plugin.Scope)
		if !ok {
			continue
		}
		for _, component := range record.Plugin.Components {
			if component.Kind != extensions.ComponentSkill || component.RealPath == "" {
				continue
			}
			roots = append(roots, skills.AdditionalRoot{
				Path: component.RealPath, Namespace: record.Plugin.ID, Dialect: dialect, Scope: scope,
			})
		}
	}
	sort.Slice(roots, func(i, j int) bool {
		if roots[i].Namespace != roots[j].Namespace {
			return roots[i].Namespace < roots[j].Namespace
		}
		return roots[i].Path < roots[j].Path
	})
	return roots, notes
}

func (inv *pluginInventory) requiresCodexAppServer() bool {
	if inv == nil {
		return false
	}
	for _, record := range inv.records {
		if record.Plugin.Dialect == extensions.DialectCodex && pluginHasMCP(record.Plugin) &&
			record.behaviorEnabled() && record.Activation.ExecutableTrusted && !record.Activation.Changed {
			return true
		}
	}
	return false
}

func pluginSkillDialect(dialect extensions.Dialect) (skills.Ecosystem, bool) {
	switch dialect {
	case extensions.DialectCodex:
		return skills.EcosystemCodex, true
	case extensions.DialectClaude:
		return skills.EcosystemClaude, true
	default:
		return "", false
	}
}

func pluginSkillScope(scope extensions.Scope) (skills.Scope, bool) {
	switch scope {
	case extensions.ScopeUser:
		return skills.ScopeUser, true
	case extensions.ScopeWorkspace:
		return skills.ScopeWorkspace, true
	case extensions.ScopeLocal:
		return skills.ScopeLocal, true
	case extensions.ScopeManaged:
		return skills.ScopeManaged, true
	default:
		return "", false
	}
}

func (inv *pluginInventory) resolve(selector string) (pluginRecord, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return pluginRecord{}, fmt.Errorf("plugin selector is empty")
	}
	var matches []pluginRecord
	for _, record := range inv.records {
		matched := record.Plugin.ID == selector || record.Plugin.RealPath == selector
		if record.Reference != nil && record.Reference.RecoveryToken == selector {
			matched = true
		}
		if !matched {
			for _, nativeID := range record.NativeIDs {
				if nativeID == selector {
					matched = true
					break
				}
			}
		}
		if matched {
			matches = append(matches, record)
		}
	}
	switch len(matches) {
	case 0:
		return pluginRecord{}, fmt.Errorf("no plugin matches %q", selector)
	case 1:
		return matches[0], nil
	default:
		// A catalog source and its Switchboard-owned installed copy keep the
		// same canonical manifest ID. For that exact ID, prefer the unique
		// Switchboard-owned copy: executable and prompt authority are bound to
		// those cached bytes, never to the native source tree. With no owned
		// copy, a unique native-installed root wins over catalog inventory.
		if strings.Contains(selector, ":") {
			var owned []pluginRecord
			for _, record := range matches {
				if record.Plugin.ID == selector && record.SwitchboardOwned && record.ActivationEligible {
					owned = append(owned, record)
				}
			}
			if len(owned) == 1 {
				return owned[0], nil
			}
			if len(owned) > 1 {
				return pluginRecord{}, fmt.Errorf("plugin selector %q is ambiguous across %d Switchboard-owned roots; use the exact path", selector, len(owned))
			}
			var installed []pluginRecord
			for _, record := range matches {
				if record.Plugin.ID == selector && record.ActivationEligible && record.NativeState == extnative.CandidateInstalled {
					installed = append(installed, record)
				}
			}
			if len(installed) == 1 {
				return installed[0], nil
			}
		}
		return pluginRecord{}, fmt.Errorf("plugin selector %q is ambiguous across %d roots; use the exact path", selector, len(matches))
	}
}

// resolveSavedActivation keeps removal recoverable after cached bytes were
// deleted, became unreadable, or stopped parsing as the saved plugin. The
// opaque token covers the complete persisted identity; friendly IDs and paths
// are accepted only when they select one stale record and no live record.
func (inv *pluginInventory) resolveSavedActivation(selector string) (pluginActivationReference, bool, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return pluginActivationReference{}, false, fmt.Errorf("plugin selector is empty")
	}
	var matches []pluginActivationReference
	for _, reference := range inv.stale {
		activation := reference.Activation
		if activation.ID == selector || activation.RealPath == selector || reference.RecoveryToken == selector {
			matches = append(matches, reference)
		}
	}
	if len(matches) == 0 {
		return pluginActivationReference{}, false, nil
	}
	if len(matches) > 1 {
		return pluginActivationReference{}, false, fmt.Errorf(
			"plugin selector %q is ambiguous across %d saved activations; use the saved: recovery selector shown by sb plugins list", selector, len(matches))
	}
	if selector != matches[0].RecoveryToken {
		if _, err := inv.resolve(selector); err == nil || !strings.Contains(err.Error(), "no plugin matches") {
			return pluginActivationReference{}, false, fmt.Errorf(
				"plugin selector %q matches live and saved activation state; use the saved: recovery selector shown by sb plugins list", selector)
		}
	}
	return matches[0], true, nil
}

func runPluginsCLI(w io.Writer, workspace string, args []string) error {
	return runPluginsCLIContext(context.Background(), w, workspace, args)
}

func runPluginsCLIContext(ctx context.Context, w io.Writer, workspace string, args []string) error {
	return runPluginsActionContext(ctx, w, workspace, openPluginInventory(workspace, true), args)
}

func runPluginsAction(w io.Writer, workspace string, inv *pluginInventory, args []string) error {
	return runPluginsActionContext(context.Background(), w, workspace, inv, args)
}

func runPluginsActionContext(ctx context.Context, w io.Writer, workspace string, inv *pluginInventory, args []string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if inv == nil {
		return fmt.Errorf("plugin inventory is unavailable")
	}
	action := "list"
	if len(args) > 0 {
		action = strings.ToLower(strings.TrimSpace(args[0]))
		args = args[1:]
	}
	switch action {
	case "", "list":
		if len(args) != 0 {
			return fmt.Errorf("sb plugins list takes no argument; %q is extra", args[0])
		}
		writePluginList(w, inv)
		return nil
	case "inspect":
		if len(args) != 1 {
			return fmt.Errorf("sb plugins inspect takes exactly one plugin selector")
		}
		record, err := inv.resolve(args[0])
		if err != nil {
			return err
		}
		writePluginInspect(w, record)
		return nil
	case "install", "enable", "disable", "trust", "untrust":
		if len(args) != 1 {
			return fmt.Errorf("sb plugins %s takes exactly one plugin selector", action)
		}
		if inv.state == nil {
			return fmt.Errorf("plugin activation state is unavailable; refusing to %s anything", action)
		}
		selector := strings.TrimSpace(args[0])
		var savedReference pluginActivationReference
		var saved bool
		var err error
		if action == "disable" || action == "untrust" {
			savedReference, saved, err = inv.resolveSavedActivation(selector)
			if err != nil {
				return err
			}
		}
		var record pluginRecord
		if !saved {
			record, err = inv.resolve(selector)
			if err != nil {
				return err
			}
		} else {
			activation := savedReference.Activation
			record.Plugin = extensions.Plugin{
				ID: activation.ID, Dialect: activation.Dialect, Scope: activation.Scope, RealPath: activation.RealPath,
			}
		}
		switch action {
		case "install":
			if record.ManagedDenied {
				return fmt.Errorf("%s is disabled by authoritative managed policy", record.Plugin.ID)
			}
			if record.NativeState != extnative.CandidateAvailable || record.ActivationEligible {
				return fmt.Errorf("%s is already installed; use enable", record.Plugin.ID)
			}
			cacheRoot, cacheErr := extensions.DefaultInstallRoot()
			if cacheErr != nil {
				return cacheErr
			}
			candidate, installErr := extensions.InstallActivationContext(ctx, record.Plugin, cacheRoot)
			if installErr != nil {
				return fmt.Errorf("install %s: %w", record.Plugin.ID, installErr)
			}
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("install %s: %w", record.Plugin.ID, err)
			}
			if enableErr := inv.state.EnableWithNativeIDsContext(ctx, candidate, workspace, record.NativeIDs); enableErr != nil {
				return fmt.Errorf("installed %s at %s, but could not activate it: %w",
					record.Plugin.ID, candidate.Plugin().RealPath, enableErr)
			}
			fmt.Fprintf(w, "installed and enabled %s at %s; executable components remain untrusted; the change applies on the next Switchboard run\n",
				cliText(record.Plugin.ID), cliText(candidate.Plugin().RealPath))
			return nil
		case "enable":
			if record.ManagedDenied {
				return fmt.Errorf("%s is disabled by authoritative managed policy", record.Plugin.ID)
			}
			if !record.ActivationEligible {
				return fmt.Errorf("%s is available inventory, not an installed plugin; install it before enabling it", record.Plugin.ID)
			}
			candidate, candidateErr := cachePluginActivationContext(ctx, record.Plugin)
			if candidateErr != nil {
				return fmt.Errorf("prepare %s for activation: %w", record.Plugin.ID, candidateErr)
			}
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("enable %s: %w", record.Plugin.ID, err)
			}
			err = inv.state.EnableWithNativeIDsContext(ctx, candidate, workspace, record.NativeIDs)
		case "disable":
			if contextErr := ctx.Err(); contextErr != nil {
				return fmt.Errorf("disable %s: %w", record.Plugin.ID, contextErr)
			}
			if saved {
				err = inv.state.DisableActivationContext(ctx, savedReference.Activation)
			} else if record.Reference != nil {
				err = inv.state.DisableActivationContext(ctx, record.Reference.Activation)
			} else {
				err = inv.state.DisableContext(ctx, record.Plugin, workspace)
			}
		case "trust":
			if record.ManagedDenied {
				return fmt.Errorf("%s is disabled by authoritative managed policy", record.Plugin.ID)
			}
			if !record.Activation.Enabled {
				return fmt.Errorf("%s must be enabled before executable trust is granted", record.Plugin.ID)
			}
			candidate, candidateErr := cachePluginActivationContext(ctx, record.Plugin)
			if candidateErr != nil {
				return fmt.Errorf("prepare %s for executable trust: %w", record.Plugin.ID, candidateErr)
			}
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("trust %s: %w", record.Plugin.ID, err)
			}
			err = inv.state.TrustExecutableContext(ctx, candidate, workspace)
		case "untrust":
			if contextErr := ctx.Err(); contextErr != nil {
				return fmt.Errorf("untrust %s: %w", record.Plugin.ID, contextErr)
			}
			if saved {
				err = inv.state.RevokeActivationTrustContext(ctx, savedReference.Activation)
			} else if record.Reference != nil {
				err = inv.state.RevokeActivationTrustContext(ctx, record.Reference.Activation)
			} else {
				err = inv.state.RevokeExecutableContext(ctx, record.Plugin, workspace)
			}
		}
		if err != nil {
			return fmt.Errorf("%s %s: %w", action, record.Plugin.ID, err)
		}
		fmt.Fprintf(w, "%s %s; the change applies on the next Switchboard run\n", pluginActionPastTense(action), cliText(record.Plugin.ID))
		return nil
	default:
		return fmt.Errorf("unknown plugins action %q: use list, inspect, install, enable, disable, trust, or untrust", action)
	}
}

// cachePluginActivation is the only transition from discovered bytes to an
// activation capability. Native installed roots are copied into
// Switchboard's bounded content-addressed cache first; a marketplace/catalog
// tree cannot be enabled in place. Re-running it for an already owned cache
// entry is an idempotent rediscovery of the same identity and digest.
func cachePluginActivation(plugin extensions.Plugin) (*extensions.ActivationCandidate, error) {
	return cachePluginActivationContext(context.Background(), plugin)
}

func cachePluginActivationContext(ctx context.Context, plugin extensions.Plugin) (*extensions.ActivationCandidate, error) {
	cacheRoot, err := extensions.DefaultInstallRoot()
	if err != nil {
		return nil, err
	}
	return extensions.InstallActivationContext(ctx, plugin, cacheRoot)
}

func pluginActionPastTense(action string) string {
	switch action {
	case "enable":
		return "enabled"
	case "disable":
		return "disabled"
	case "trust":
		return "trusted executable bytes for"
	case "untrust":
		return "revoked executable trust for"
	default:
		return action
	}
}

func writePluginList(w io.Writer, inv *pluginInventory) {
	if len(inv.records) == 0 && len(inv.stale) == 0 {
		fmt.Fprintln(w, "no native Codex or Claude plugins discovered")
	} else {
		fmt.Fprintln(w, "PLUGIN\tSTATE\tNATIVE\tEXECUTION\tPATH\tRECOVERY")
		for _, record := range inv.records {
			recovery := ""
			if record.Reference != nil {
				recovery = record.Reference.RecoveryToken
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				cliText(record.Plugin.ID), cliText(pluginActivationLabel(record)), cliText(pluginNativeLabel(record)),
				cliText(pluginExecutionLabel(record)), cliText(record.Plugin.RealPath), cliText(valueOrDash(recovery)))
		}
		for _, reference := range inv.stale {
			fmt.Fprintf(w, "%s\tsaved-unavailable\tunknown\t-\t%s\t%s\n",
				cliText(reference.Activation.ID), cliText(reference.Activation.RealPath), cliText(reference.RecoveryToken))
		}
	}
	writePluginDiagnostics(w, inv.diagnostics)
}

func writePluginInspect(w io.Writer, record pluginRecord) {
	fmt.Fprintf(w, "plugin: %s\n", cliText(record.Plugin.ID))
	fmt.Fprintf(w, "native ids: %s\n", cliText(valueOrDash(strings.Join(record.NativeIDs, ", "))))
	fmt.Fprintf(w, "dialect: %s\nscope: %s\npath: %s\n", cliText(string(record.Plugin.Dialect)), cliText(string(record.Plugin.Scope)), cliText(record.Plugin.RealPath))
	fmt.Fprintf(w, "state: %s\nnative: %s\nexecution: %s\ndigest: %s\n",
		cliText(pluginActivationLabel(record)), cliText(pluginNativeLabel(record)), cliText(pluginExecutionLabel(record)), cliText(record.Plugin.Digest))
	if record.Reference != nil {
		fmt.Fprintf(w, "recovery: %s\n", cliText(record.Reference.RecoveryToken))
	}
	if len(record.Plugin.Components) == 0 {
		fmt.Fprintln(w, "components: none")
	} else {
		fmt.Fprintln(w, "components:")
		for _, component := range record.Plugin.Components {
			path := component.RealPath
			if component.Inline {
				path = "inline"
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\n", cliText(string(component.Kind)), cliText(string(component.Source)), cliText(valueOrDash(path)))
		}
	}
	for _, warning := range record.Plugin.Warnings {
		fmt.Fprintf(w, "warning: %s: %s\n", cliText(warning.Code), cliText(warning.Message))
	}
	for _, provenance := range record.Provenance {
		fmt.Fprintf(w, "source: %s (%s)\n", cliText(provenance.NativeID), cliText(valueOrDash(provenance.RegistryPath)))
	}
}

func pluginActivationLabel(record pluginRecord) string {
	if record.ManagedDenied {
		return "managed-denied"
	}
	if record.Activation.Changed {
		return "changed"
	}
	if record.Activation.Enabled {
		return "enabled"
	}
	if record.NativeState == extnative.CandidateAvailable {
		return "available"
	}
	return "disabled"
}

func pluginNativeLabel(record pluginRecord) string {
	if len(record.NativeIDs) == 0 {
		return "switchboard"
	}
	if record.NativeEnabled {
		return "enabled"
	}
	return "disabled"
}

func pluginExecutionLabel(record pluginRecord) string {
	if record.ManagedDenied {
		return "policy-denied"
	}
	if !record.Plugin.Executable {
		return "prompt-only"
	}
	if record.Activation.Changed {
		return "changed/untrusted"
	}
	if record.Activation.ExecutableTrusted {
		return "trusted"
	}
	return "untrusted"
}

func writePluginDiagnostics(w io.Writer, notes []mcpNote) {
	for _, note := range notes {
		fmt.Fprintf(w, "%s: %s\n", cliText(note.level), cliText(note.text))
	}
}

func valueOrDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
