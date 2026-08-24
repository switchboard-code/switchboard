package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/extensions"
	extnative "github.com/switchboard-code/switchboard/internal/extensions/native"
	"github.com/switchboard-code/switchboard/internal/mcppolicy"
	"github.com/switchboard-code/switchboard/internal/skills"
)

func TestNativePluginNeedsSwitchboardActivationBeforeSkillsLoad(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	pluginRoot := makeClaudePluginFixture(t, home, "review")
	mustWritePluginTest(t, filepath.Join(home, ".claude", "settings.json"),
		`{"enabledPlugins":{"review@market":true}}`)
	mustWritePluginTest(t, filepath.Join(home, ".claude", "plugins", "installed_plugins.json"),
		`{"version":2,"plugins":{"review@market":[{"scope":"user","installPath":`+quotedPluginPath(pluginRoot)+`}]}}`)
	state, err := extensions.OpenStateFile(filepath.Join(t.TempDir(), extensions.StateFileName))
	if err != nil {
		t.Fatal(err)
	}

	before := discoverPlugins(home, workspace, state, true)
	if len(before.records) != 1 {
		t.Fatalf("records before activation = %#v; diagnostics = %#v", before.records, before.diagnostics)
	}
	record := before.records[0]
	if !record.NativeEnabled || !record.ActivationEligible || record.Activation.Enabled {
		t.Fatalf("native state granted or lost authority: %#v", record)
	}
	if roots, _ := before.enabledSkillRoots(); len(roots) != 0 {
		t.Fatalf("native enablement loaded skills without Switchboard activation: %#v", roots)
	}

	candidate, err := extensions.InstallActivation(record.Plugin, filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Enable(candidate, workspace); err != nil {
		t.Fatal(err)
	}
	after := discoverPlugins(home, workspace, state, false)
	if len(after.records) != 1 || !after.records[0].Activation.Enabled {
		t.Fatalf("Switchboard activation was not joined: %#v", after.records)
	}
	roots, notes := after.enabledSkillRoots()
	if len(notes) != 0 || len(roots) != 1 {
		t.Fatalf("plugin skill roots = %#v, notes = %#v", roots, notes)
	}
	loaded, loadNotes := skills.LoadAdditional(roots)
	if len(loadNotes) != 0 || len(loaded) != 1 || !strings.HasPrefix(loaded[0].Key(), "plugin:claude%3Areview:") {
		t.Fatalf("loaded plugin skills = %#v, notes = %#v", loaded, loadNotes)
	}
}

func TestCodexCatalogIsVisibleButNotActivationEligible(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	marketplace := filepath.Join(home, "marketplace")
	pluginRoot := filepath.Join(marketplace, "plugins", "review")
	mustWritePluginTest(t, filepath.Join(pluginRoot, ".codex-plugin", "plugin.json"), `{"name":"review"}`)
	mustWritePluginTest(t, filepath.Join(pluginRoot, "skills", "review", "SKILL.md"), pluginSkillBody("review"))
	mustWritePluginTest(t, filepath.Join(marketplace, ".agents", "plugins", "marketplace.json"),
		`{"name":"personal","plugins":[{"name":"review","source":{"source":"local","path":"./plugins/review"}}]}`)
	mustWritePluginTest(t, filepath.Join(home, ".codex", "config.toml"),
		"[marketplaces.personal]\nsource_type = \"local\"\nsource = "+quotedTOMLPath(marketplace)+"\n\n[plugins.\"review@personal\"]\nenabled = true\n")
	state, err := extensions.OpenStateFile(filepath.Join(t.TempDir(), extensions.StateFileName))
	if err != nil {
		t.Fatal(err)
	}

	inv := discoverPlugins(home, workspace, state, true)
	if len(inv.records) != 1 {
		t.Fatalf("Codex records = %#v; diagnostics = %#v", inv.records, inv.diagnostics)
	}
	record := inv.records[0]
	if record.NativeState != "available" || !record.NativeEnabled || record.ActivationEligible || record.Activation.Enabled {
		t.Fatalf("catalog bytes were treated as installed/active: %#v", record)
	}
	if got, err := inv.resolve("review@personal"); err != nil || got.Plugin.ID != "codex:review" {
		t.Fatalf("native selector resolution = %#v, %v", got, err)
	}
	if lean := discoverPlugins(home, workspace, state, false); len(lean.records) != 0 {
		t.Fatalf("session assembly eagerly digested unactivated catalog inventory: %#v", lean.records)
	}
}

func TestSavedProjectPluginDoesNotCrossWorkspace(t *testing.T) {
	home := t.TempDir()
	firstWorkspace := t.TempDir()
	secondWorkspace := t.TempDir()
	root := makeClaudePluginFixture(t, home, "project-review")
	discovered := extensions.Discover([]extensions.Candidate{{
		Root: root, Scope: extensions.ScopeWorkspace, Dialect: extensions.DialectClaude,
	}})
	if len(discovered.Plugins) != 1 {
		t.Fatalf("fixture discovery = %#v", discovered)
	}
	state, err := extensions.OpenStateFile(filepath.Join(t.TempDir(), extensions.StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := extensions.InstallActivation(discovered.Plugins[0], filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.Enable(candidate, firstWorkspace); err != nil {
		t.Fatal(err)
	}

	if got := discoverPlugins(home, secondWorkspace, state, false); len(got.records) != 0 {
		t.Fatalf("other workspace inherited plugin inventory: %#v", got.records)
	}
	if got := discoverPlugins(home, firstWorkspace, state, false); len(got.records) != 1 || !got.records[0].Activation.Enabled {
		t.Fatalf("own workspace lost plugin inventory: %#v", got.records)
	}
}

func TestEnabledDuplicatePluginIDsKeepAllSkillsOff(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	state, err := extensions.OpenStateFile(filepath.Join(t.TempDir(), extensions.StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, parent := range []string{"one", "two"} {
		root := filepath.Join(t.TempDir(), parent, "duplicate")
		makeClaudePluginAt(t, root, "duplicate")
		result := extensions.Discover([]extensions.Candidate{{Root: root, Scope: extensions.ScopeUser, Dialect: extensions.DialectClaude}})
		if len(result.Plugins) != 1 {
			t.Fatalf("fixture discovery = %#v", result)
		}
		candidate, err := extensions.InstallActivation(result.Plugins[0], filepath.Join(t.TempDir(), "cache"))
		if err != nil {
			t.Fatal(err)
		}
		if err := state.Enable(candidate, ""); err != nil {
			t.Fatal(err)
		}
	}
	inv := discoverPlugins(home, workspace, state, false)
	roots, notes := inv.enabledSkillRoots()
	if len(roots) != 0 || len(notes) == 0 {
		t.Fatalf("duplicate plugin skills were not refused: roots=%#v notes=%#v", roots, notes)
	}
}

func TestPluginActionsKeepActivationAndExecutableTrustSeparate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	workspace := t.TempDir()
	pluginRoot := makeClaudePluginFixture(t, home, "tools")
	mustWritePluginTest(t, filepath.Join(pluginRoot, ".mcp.json"), `{"servers":{"review":{"command":"review-server"}}}`)
	mustWritePluginTest(t, filepath.Join(home, ".claude", "settings.json"),
		`{"enabledPlugins":{"tools@market":true}}`)
	mustWritePluginTest(t, filepath.Join(home, ".claude", "plugins", "installed_plugins.json"),
		`{"version":2,"plugins":{"tools@market":[{"scope":"user","installPath":`+quotedPluginPath(pluginRoot)+`}]}}`)
	state, err := extensions.OpenStateFile(filepath.Join(t.TempDir(), extensions.StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	inv := discoverPlugins(home, workspace, state, true)
	if len(inv.records) != 1 || !inv.records[0].Plugin.Executable {
		t.Fatalf("fixture did not expose executable plugin: %#v; diagnostics=%#v", inv.records, inv.diagnostics)
	}

	var output strings.Builder
	if err := runPluginsAction(&output, workspace, inv, []string{"enable", "tools@market"}); err != nil {
		t.Fatal(err)
	}
	owned := discoverPlugins(home, workspace, state, false)
	if len(owned.records) != 1 {
		t.Fatalf("enabled cache inventory = %#v", owned.records)
	}
	status := state.Status(owned.records[0].Plugin, workspace)
	if !status.Enabled || status.ExecutableTrusted {
		t.Fatalf("enable implicitly trusted executable bytes: %#v", status)
	}

	inv = discoverPlugins(home, workspace, state, true)
	if err := runPluginsAction(&output, workspace, inv, []string{"trust", inv.records[0].Plugin.ID}); err != nil {
		t.Fatal(err)
	}
	status = state.Status(inv.records[0].Plugin, workspace)
	if !status.Enabled || !status.ExecutableTrusted {
		t.Fatalf("explicit trust did not bind current digest: %#v", status)
	}

	inv = discoverPlugins(home, workspace, state, true)
	if err := runPluginsAction(&output, workspace, inv, []string{"untrust", inv.records[0].Plugin.RealPath}); err != nil {
		t.Fatal(err)
	}
	status = state.Status(inv.records[0].Plugin, workspace)
	if !status.Enabled || status.ExecutableTrusted {
		t.Fatalf("untrust changed activation or retained trust: %#v", status)
	}

	inv = discoverPlugins(home, workspace, state, true)
	if err := runPluginsAction(&output, workspace, inv, []string{"disable", inv.records[0].Plugin.ID}); err != nil {
		t.Fatal(err)
	}
	if status = state.Status(inv.records[0].Plugin, workspace); status.Enabled || status.ExecutableTrusted {
		t.Fatalf("disable retained authority: %#v", status)
	}
	if !strings.Contains(output.String(), "next Switchboard run") {
		t.Fatalf("mutation output did not explain frozen-zone timing: %q", output.String())
	}
}

func TestPluginActionsRefuseCatalogOnlyEnableAndRenderProvenance(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	marketplace := filepath.Join(home, "marketplace")
	pluginRoot := filepath.Join(marketplace, "plugins", "review")
	mustWritePluginTest(t, filepath.Join(pluginRoot, ".codex-plugin", "plugin.json"), `{"name":"review"}`)
	mustWritePluginTest(t, filepath.Join(pluginRoot, "skills", "review", "SKILL.md"), pluginSkillBody("review"))
	mustWritePluginTest(t, filepath.Join(marketplace, ".agents", "plugins", "marketplace.json"),
		`{"name":"personal","plugins":[{"name":"review","source":{"source":"local","path":"./plugins/review"}}]}`)
	mustWritePluginTest(t, filepath.Join(home, ".codex", "config.toml"),
		"[marketplaces.personal]\nsource_type = \"local\"\nsource = "+quotedTOMLPath(marketplace)+"\n")
	state, err := extensions.OpenStateFile(filepath.Join(t.TempDir(), extensions.StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	inv := discoverPlugins(home, workspace, state, true)
	if err := runPluginsAction(&strings.Builder{}, workspace, inv, []string{"enable", "review@personal"}); err == nil || !strings.Contains(err.Error(), "install it") {
		t.Fatalf("catalog-only enable error = %v", err)
	}

	var list strings.Builder
	if err := runPluginsAction(&list, workspace, inv, nil); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"codex:review", "available"} {
		if !strings.Contains(list.String(), want) {
			t.Errorf("plugin list missing %q:\n%s", want, list.String())
		}
	}
	var listedPath string
	for _, line := range strings.Split(strings.TrimSpace(list.String()), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) >= 5 && fields[0] == "codex:review" {
			listedPath = fields[4]
			break
		}
	}
	wantInfo, wantErr := os.Stat(pluginRoot)
	gotInfo, gotErr := os.Stat(listedPath)
	if listedPath == "" || wantErr != nil || gotErr != nil || !os.SameFile(gotInfo, wantInfo) {
		t.Errorf("plugin list path %q does not identify discovered root %q: listed=%v root=%v\n%s",
			listedPath, pluginRoot, gotErr, wantErr, list.String())
	}
	var inspect strings.Builder
	if err := runPluginsAction(&inspect, workspace, inv, []string{"inspect", "review@personal"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"native ids: review@personal", "state: available", "components:"} {
		if !strings.Contains(inspect.String(), want) {
			t.Errorf("plugin inspect missing %q:\n%s", want, inspect.String())
		}
	}
}

func TestPluginInstallPromotesLocalInventoryAndEnablesPromptComponents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOMEDRIVE", filepath.VolumeName(home))
	t.Setenv("HOMEPATH", strings.TrimPrefix(home, filepath.VolumeName(home)))
	workspace := t.TempDir()
	marketplace := filepath.Join(home, "marketplace")
	source := filepath.Join(marketplace, "plugins", "review")
	mustWritePluginTest(t, filepath.Join(source, ".codex-plugin", "plugin.json"), `{"name":"review"}`)
	mustWritePluginTest(t, filepath.Join(source, "skills", "review", "SKILL.md"), pluginSkillBody("review"))
	mustWritePluginTest(t, filepath.Join(marketplace, ".agents", "plugins", "marketplace.json"),
		`{"name":"personal","plugins":[{"name":"review","source":{"source":"local","path":"./plugins/review"}}]}`)
	mustWritePluginTest(t, filepath.Join(home, ".codex", "config.toml"),
		"[marketplaces.personal]\nsource_type = \"local\"\nsource = "+quotedTOMLPath(marketplace)+"\n")
	state, err := extensions.OpenStateFile(filepath.Join(home, ".switchboard", extensions.StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	inv := discoverPlugins(home, workspace, state, true)
	if len(inv.records) != 1 || inv.records[0].NativeState != "available" {
		t.Fatalf("available fixture = %#v; diagnostics=%#v", inv.records, inv.diagnostics)
	}
	var output strings.Builder
	if err := runPluginsAction(&output, workspace, inv, []string{"install", "review@personal"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "installed and enabled codex:review") ||
		!strings.Contains(output.String(), "executable components remain untrusted") {
		t.Fatalf("install output = %q", output.String())
	}

	lean := discoverPlugins(home, workspace, state, false)
	if len(lean.records) != 1 || !lean.records[0].Activation.Enabled {
		t.Fatalf("installed plugin did not become lean session inventory: %#v; diagnostics=%#v", lean.records, lean.diagnostics)
	}
	installed := lean.records[0]
	if installed.Plugin.RealPath == source || !strings.Contains(installed.Plugin.RealPath, filepath.Join(".switchboard", "plugin-cache")) {
		t.Fatalf("plugin was not promoted into Switchboard cache: %s", installed.Plugin.RealPath)
	}
	roots, notes := lean.enabledSkillRoots()
	if len(notes) != 0 || len(roots) != 1 {
		t.Fatalf("installed skill roots = %#v, notes=%#v", roots, notes)
	}
	full := discoverPlugins(home, workspace, state, true)
	resolved, err := full.resolve("codex:review")
	if err != nil || resolved.Plugin.RealPath != installed.Plugin.RealPath {
		t.Fatalf("canonical ID did not prefer the unique installed copy: %#v, %v", resolved, err)
	}
}

func TestClaudeManagedDenyConstrainsCachedActivationAcrossNativeDrift(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	nativeRoot := makeClaudePluginFixture(t, home, "alpha")
	mustWritePluginTest(t, filepath.Join(nativeRoot, ".mcp.json"), `{"servers":{"tool":{"command":"tool"}}}`)
	userSettings := filepath.Join(home, "user-settings.json")
	managedSettings := filepath.Join(home, "managed-settings.json")
	registry := filepath.Join(home, "installed_plugins.json")
	mustWritePluginTest(t, userSettings, `{"enabledPlugins":{"alpha@market":true}}`)
	mustWritePluginTest(t, registry, `{"version":2,"plugins":{"alpha@market":[{"scope":"user","installPath":`+quotedPluginPath(nativeRoot)+`}]}}`)
	options := extnative.Options{Claude: &extnative.ClaudeOptions{
		Workspace: workspace, InstalledPluginsPath: registry,
		Settings: []extnative.ClaudeSettings{
			{Path: userSettings, Scope: extensions.ScopeUser},
			{Path: managedSettings, Scope: extensions.ScopeManaged},
		},
	}}
	state, err := extensions.OpenStateFile(filepath.Join(t.TempDir(), extensions.StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	initial := discoverPluginsWithNativeOptions(home, workspace, state, true, options, nil)
	if len(initial.records) != 1 || len(initial.records[0].NativeIDs) != 1 {
		t.Fatalf("initial inventory = %#v; diagnostics=%#v", initial.records, initial.diagnostics)
	}
	candidate, err := extensions.InstallActivation(initial.records[0].Plugin, filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.EnableWithNativeIDs(candidate, workspace, initial.records[0].NativeIDs); err != nil {
		t.Fatal(err)
	}
	if err := state.TrustExecutable(candidate, workspace); err != nil {
		t.Fatal(err)
	}

	assertDenied := func(t *testing.T, inv *pluginInventory) {
		t.Helper()
		var cached *pluginRecord
		for i := range inv.records {
			if inv.records[i].Plugin.RealPath == candidate.Plugin().RealPath {
				cached = &inv.records[i]
				break
			}
		}
		if cached == nil || !cached.ManagedDenied || !cached.Activation.Enabled || cached.behaviorEnabled() {
			t.Fatalf("cached managed-denial join = %#v; inventory=%#v diagnostics=%#v", cached, inv.records, inv.diagnostics)
		}
		if roots, _ := inv.enabledSkillRoots(); len(roots) != 0 {
			t.Fatalf("managed-denied cache loaded skills: %#v", roots)
		}
		if specs, _, err := enabledPluginMCPSpecs(inv, workspace, allowAllNativeMCPAssemblyPolicy(t)); err != nil || len(specs) != 0 {
			t.Fatalf("managed-denied cache loaded MCP: specs=%#v err=%v", specs, err)
		}
	}

	mustWritePluginTest(t, managedSettings, `{"enabledPlugins":{"alpha@market":false}}`)
	mustWritePluginTest(t, filepath.Join(nativeRoot, "skills", "alpha", "SKILL.md"), pluginSkillBody("alpha")+"\nchanged\n")
	assertDenied(t, discoverPluginsWithNativeOptions(home, workspace, state, true, options, nil))

	removed := nativeRoot + ".removed"
	if err := os.Rename(nativeRoot, removed); err != nil {
		t.Fatal(err)
	}
	assertDenied(t, discoverPluginsWithNativeOptions(home, workspace, state, true, options, nil))

	mustWritePluginTest(t, userSettings, `{not-json`)
	assertDenied(t, discoverPluginsWithNativeOptions(home, workspace, state, true, options, nil))

	// A repository-controlled project settings symlink may invalidate the
	// lower layer, but it must not prevent the independent system-managed deny
	// from being read and applied to the cached global activation.
	mustWritePluginTest(t, userSettings, `{"enabledPlugins":{"alpha@market":true}}`)
	outsideClaude := filepath.Join(t.TempDir(), ".claude")
	mustWritePluginTest(t, filepath.Join(outsideClaude, "settings.json"), `{}`)
	if err := os.Symlink(outsideClaude, filepath.Join(workspace, ".claude")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	options.Claude.Settings = append(options.Claude.Settings, extnative.ClaudeSettings{
		Path: filepath.Join(workspace, ".claude", "settings.json"), Scope: extensions.ScopeWorkspace, ProjectPath: workspace,
	})
	assertDenied(t, discoverPluginsWithNativeOptions(home, workspace, state, true, options, nil))

	policyRoot := t.TempDir()
	policyHome := filepath.Join(policyRoot, "home")
	policyWorkspace := filepath.Join(policyRoot, "workspace")
	policySystem := filepath.Join(policyRoot, "system")
	for _, directory := range []string{policyHome, policyWorkspace, policySystem} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	policyPaths := mcppolicy.Paths{
		CodexRequirements: filepath.Join(policySystem, "requirements.toml"), CodexAuth: filepath.Join(policyHome, "auth.json"),
		ClaudeManagedSettings: filepath.Join(policySystem, "managed-settings.json"), ClaudeManagedDropIns: filepath.Join(policySystem, "managed-settings.d"),
		ClaudeManagedMCP: filepath.Join(policySystem, "managed-mcp.json"), ClaudeRemoteSettings: filepath.Join(policyHome, "remote-settings.json"),
		ClaudeState: filepath.Join(policyHome, ".claude.json"), ClaudeUserSettings: filepath.Join(policyHome, "settings.json"),
		ClaudeProjectSettings: filepath.Join(policyWorkspace, ".claude", "settings.json"), ClaudeLocalSettings: filepath.Join(policyWorkspace, ".claude", "settings.local.json"),
	}
	mustWritePluginTest(t, filepath.Join(policyPaths.ClaudeManagedDropIns, "10-plugin.json"), `{"enabledPlugins":{"alpha@market":false}}`)
	checker, diagnostics, err := mcppolicy.Load(mcppolicy.Options{
		HomeDir: policyHome, Workspace: policyWorkspace, GOOS: runtime.GOOS, Paths: &policyPaths, CloudRequirementsChecked: true, StartupEnv: []string{},
	})
	if err != nil || len(diagnostics) != 0 {
		t.Fatalf("shared managed loader: diagnostics=%#v err=%v", diagnostics, err)
	}
	externalPolicy := extnative.Options{}
	bindClaudePluginPolicy(&externalPolicy, checker)
	assertDenied(t, discoverPluginsWithNativeOptions(home, workspace, state, true, externalPolicy, nil))
}

func TestStalePluginActivationRecoveryIsKeyedAndCancellable(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	source := makeClaudePluginFixture(t, home, "stale")
	mustWritePluginTest(t, filepath.Join(source, ".mcp.json"), `{"servers":{"tool":{"command":"tool"}}}`)
	statePath := filepath.Join(t.TempDir(), extensions.StateFileName)
	state, err := extensions.OpenStateFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	discovered := extensions.Discover([]extensions.Candidate{{Root: source, Scope: extensions.ScopeUser, Dialect: extensions.DialectClaude}})
	candidate, err := extensions.InstallActivation(discovered.Plugins[0], filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if err := state.EnableWithNativeIDs(candidate, workspace, []string{"stale@market"}); err != nil {
		t.Fatal(err)
	}
	if err := state.TrustExecutable(candidate, workspace); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(candidate.Plugin().RealPath, candidate.Plugin().RealPath+".deleted"); err != nil {
		t.Fatal(err)
	}
	inv := discoverPluginsWithNativeOptions(home, workspace, state, true, extnative.Options{}, nil)
	if len(inv.stale) != 1 || !strings.HasPrefix(inv.stale[0].RecoveryToken, "saved:") ||
		strings.Contains(inv.stale[0].RecoveryToken, "stale") || strings.Contains(inv.stale[0].RecoveryToken, candidate.Plugin().RealPath) {
		t.Fatalf("stale recovery reference = %#v", inv.stale)
	}
	token := inv.stale[0].RecoveryToken
	reopened, err := extensions.OpenStateFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	reopenedInv := discoverPluginsWithNativeOptions(home, workspace, reopened, true, extnative.Options{}, nil)
	if len(reopenedInv.stale) != 1 || reopenedInv.stale[0].RecoveryToken != token {
		t.Fatalf("recovery token did not persist: before=%q after=%#v", token, reopenedInv.stale)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runPluginsActionContext(cancelled, &strings.Builder{}, workspace, reopenedInv, []string{"disable", token}); err == nil {
		t.Fatal("cancelled recovery mutation succeeded")
	}
	if len(reopened.Activations()) != 1 {
		t.Fatal("cancelled recovery mutation changed state")
	}
	if err := runPluginsAction(&strings.Builder{}, workspace, reopenedInv, []string{"untrust", token}); err != nil {
		t.Fatal(err)
	}
	if activations := reopened.Activations(); len(activations) != 1 || activations[0].TrustDigest != "" {
		t.Fatalf("opaque recovery selector did not revoke stale trust: %#v", activations)
	}
	if err := runPluginsAction(&strings.Builder{}, workspace, reopenedInv, []string{"disable", token}); err != nil {
		t.Fatal(err)
	}
	if len(reopened.Activations()) != 0 {
		t.Fatal("opaque recovery selector did not disable stale activation")
	}
}

func TestStalePluginFriendlySelectorRequiresOpaqueTokenWhenAmbiguous(t *testing.T) {
	inv := &pluginInventory{stale: []pluginActivationReference{
		{Activation: extensions.Activation{ID: "claude:alpha", RealPath: "/one"}, RecoveryToken: "saved:first"},
		{Activation: extensions.Activation{ID: "claude:alpha", RealPath: "/two"}, RecoveryToken: "saved:second"},
	}}
	if _, _, err := inv.resolveSavedActivation("claude:alpha"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous friendly selector error = %v", err)
	}
	got, saved, err := inv.resolveSavedActivation("saved:second")
	if err != nil || !saved || got.Activation.RealPath != "/two" {
		t.Fatalf("opaque selector resolution = %#v saved=%t err=%v", got, saved, err)
	}
}

func TestLivePluginRecoverySelectorDisambiguatesScopeBoundActivations(t *testing.T) {
	home := t.TempDir()
	workspace := t.TempDir()
	source := makeClaudePluginFixture(t, home, "alpha")
	discovered := extensions.Discover([]extensions.Candidate{
		{Root: source, Scope: extensions.ScopeUser, Dialect: extensions.DialectClaude},
		{Root: source, Scope: extensions.ScopeWorkspace, Dialect: extensions.DialectClaude},
	})
	if len(discovered.Plugins) != 2 {
		t.Fatalf("scope fixture discovery = %#v", discovered)
	}
	state, err := extensions.OpenStateFile(filepath.Join(t.TempDir(), extensions.StateFileName))
	if err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(t.TempDir(), "cache")
	for _, plugin := range discovered.Plugins {
		candidate, err := extensions.InstallActivation(plugin, cache)
		if err != nil {
			t.Fatal(err)
		}
		if err := state.EnableWithNativeIDs(candidate, workspace, []string{"alpha@market"}); err != nil {
			t.Fatal(err)
		}
	}
	inv := discoverPluginsWithNativeOptions(home, workspace, state, true, extnative.Options{}, nil)
	if len(inv.records) != 2 {
		t.Fatalf("live scoped records = %#v diagnostics=%#v", inv.records, inv.diagnostics)
	}
	if _, err := inv.resolve("claude:alpha"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("friendly selector ambiguity = %v", err)
	}
	tokens := make(map[string]extensions.Activation)
	for _, record := range inv.records {
		if record.Reference == nil || !strings.HasPrefix(record.Reference.RecoveryToken, "saved:") {
			t.Fatalf("live record has no opaque selector: %#v", record)
		}
		tokens[record.Reference.RecoveryToken] = record.Reference.Activation
	}
	if len(tokens) != 2 {
		t.Fatalf("live selectors are not unique: %#v", tokens)
	}
	var output strings.Builder
	writePluginList(&output, inv)
	for token := range tokens {
		if !strings.Contains(output.String(), token) {
			t.Fatalf("live selector %q missing from list:\n%s", token, output.String())
		}
	}
	var selected string
	for token, activation := range tokens {
		if activation.Scope == extensions.ScopeWorkspace {
			selected = token
		}
	}
	if err := runPluginsAction(&strings.Builder{}, workspace, inv, []string{"disable", selected}); err != nil {
		t.Fatal(err)
	}
	references, err := state.ActivationReferencesFor(workspace)
	if err != nil || len(references) != 1 || references[0].Activation.Scope != extensions.ScopeUser {
		t.Fatalf("exact live selector mutated wrong activation: %#v err=%v", references, err)
	}
}

func makeClaudePluginFixture(t *testing.T, parent, name string) string {
	t.Helper()
	root := filepath.Join(parent, "plugin-fixtures", name)
	makeClaudePluginAt(t, root, name)
	return root
}

func makeClaudePluginAt(t *testing.T, root, name string) {
	t.Helper()
	mustWritePluginTest(t, filepath.Join(root, ".claude-plugin", "plugin.json"), `{"name":"`+name+`"}`)
	mustWritePluginTest(t, filepath.Join(root, "skills", name, "SKILL.md"), pluginSkillBody(name))
}

func pluginSkillBody(name string) string {
	return "---\nname: " + name + "\ndescription: Review changes safely.\n---\n\nInspect the relevant changes.\n"
}

func mustWritePluginTest(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func quotedPluginPath(path string) string {
	return `"` + strings.ReplaceAll(path, `\`, `\\`) + `"`
}

func quotedTOMLPath(path string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(path, `\`, `\\`), `"`, `\"`) + `"`
}
