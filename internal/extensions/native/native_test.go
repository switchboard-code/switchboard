package native

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/extensions"
)

func TestResolveGoldenAndDeterministic(t *testing.T) {
	t.Parallel()
	root, options := buildGoldenFixture(t)
	result := Resolve(options)

	reversed := options
	reversedCodex := *options.Codex
	reversedCodex.Catalogs = append([]CodexCatalog(nil), reversedCodex.Catalogs...)
	slices.Reverse(reversedCodex.Catalogs)
	reversed.Codex = &reversedCodex
	reversedClaude := *options.Claude
	reversedClaude.Settings = append([]ClaudeSettings(nil), reversedClaude.Settings...)
	slices.Reverse(reversedClaude.Settings)
	reversed.Claude = &reversedClaude
	if other := Resolve(reversed); !reflect.DeepEqual(result, other) {
		t.Fatalf("input order changed result:\nfirst:  %#v\nsecond: %#v", result, other)
	}

	for _, rejected := range []string{"override@market", "local-off@market"} {
		if containsNativeID(result, rejected) {
			t.Fatalf("disabled plugin %q was returned", rejected)
		}
	}
	if got, want := len(result.Candidates), 7; got != want {
		t.Fatalf("candidate count = %d, want %d: %#v", got, want, result)
	}
	for _, candidate := range result.Candidates {
		if candidate.Candidate.Dialect == extensions.DialectCodex &&
			(candidate.State != CandidateAvailable || candidate.ActivationEligible) {
			t.Fatalf("Codex catalog inventory was treated as installed/activation-eligible: %#v", candidate)
		}
	}
	if alpha, ok := candidateByID(result, "alpha@personal"); !ok || !alpha.NativeEnabled || alpha.ActivationEligible {
		t.Fatalf("Codex native intent was not preserved independently from installation: %#v", alpha)
	}
	if off, ok := candidateByID(result, "off@personal"); !ok || off.NativeEnabled {
		t.Fatalf("disabled Codex catalog inventory was marked native-enabled: %#v", off)
	}

	normalizeNativeGoldenResult(&result, root)
	raw, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	actual := string(raw) + "\n"
	goldenPath := filepath.Join("testdata", "resolve.golden.json")
	expected, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	expectedText := strings.ReplaceAll(string(expected), "\r\n", "\n")
	if actual != expectedText {
		t.Fatalf("golden mismatch (-want +got):\nwant:\n%s\ngot:\n%s", expected, actual)
	}
}

func normalizeNativeGoldenResult(result *Result, root string) {
	normalize := func(value string) string {
		replaced := strings.ReplaceAll(value, filepath.Clean(root), "$ROOT")
		replaced = strings.ReplaceAll(replaced, filepath.ToSlash(filepath.Clean(root)), "$ROOT")
		if replaced != value || strings.Contains(replaced, "$ROOT") {
			return strings.ReplaceAll(replaced, `\`, "/")
		}
		return value
	}
	for i := range result.Candidates {
		candidate := &result.Candidates[i]
		candidate.Candidate.Root = normalize(candidate.Candidate.Root)
		candidate.Provenance.EnablementPath = normalize(candidate.Provenance.EnablementPath)
		candidate.Provenance.RegistryPath = normalize(candidate.Provenance.RegistryPath)
		candidate.Provenance.MarketplacePath = normalize(candidate.Provenance.MarketplacePath)
		candidate.Provenance.ProjectPath = normalize(candidate.Provenance.ProjectPath)
	}
	for i := range result.Diagnostics {
		result.Diagnostics[i].Path = normalize(result.Diagnostics[i].Path)
		result.Diagnostics[i].Message = normalize(result.Diagnostics[i].Message)
	}
	for i := range result.ManagedPluginConstraints {
		result.ManagedPluginConstraints[i].Path = normalize(result.ManagedPluginConstraints[i].Path)
	}
}

func TestCodexProjectCatalogCannotEscapeWorkspace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	outside := filepath.Join(root, "outside-marketplace")
	mustMkdir(t, workspace)
	mustMarketplace(t, outside, "outside", "escape")

	result := Resolve(Options{Codex: &CodexOptions{
		Workspace: workspace,
		Catalogs: []CodexCatalog{{
			Path:        filepath.Join(outside, ".agents", "plugins", "marketplace.json"),
			Scope:       extensions.ScopeWorkspace,
			ProjectPath: workspace,
		}},
	}})
	assertNoCandidates(t, result)
	assertDiagnostic(t, result, "unsafe-catalog-path")
}

func TestCodexProjectCatalogSymlinkFailsClosed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	mustMkdir(t, filepath.Join(workspace, ".agents", "plugins"))
	outsideRoot := filepath.Join(root, "outside")
	mustMarketplace(t, outsideRoot, "hidden", "secret")
	outside := filepath.Join(outsideRoot, ".agents", "plugins", "marketplace.json")
	catalogPath := filepath.Join(workspace, ".agents", "plugins", "marketplace.json")
	if err := os.Symlink(outside, catalogPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	result := Resolve(Options{Codex: &CodexOptions{
		Workspace: workspace,
		Catalogs:  []CodexCatalog{{Path: catalogPath, Scope: extensions.ScopeWorkspace, ProjectPath: workspace}},
	}})
	assertNoCandidates(t, result)
	assertDiagnostic(t, result, "invalid-marketplace")
}

func TestCodexCatalogPluginSymlinkFailsClosed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := filepath.Join(root, "outside-plugin")
	mustPlugin(t, outside, "alpha", ".codex-plugin")
	catalogRoot := filepath.Join(root, "catalog")
	mustMkdir(t, filepath.Join(catalogRoot, "plugins"))
	if err := os.Symlink(outside, filepath.Join(catalogRoot, "plugins", "alpha")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	catalogPath := filepath.Join(catalogRoot, ".agents", "plugins", "marketplace.json")
	mustWrite(t, catalogPath, `{"name":"team","plugins":[{"name":"alpha","source":{"source":"local","path":"./plugins/alpha"},"policy":{"installation":"AVAILABLE","authentication":"ON_INSTALL"}}]}`)
	result := Resolve(Options{Codex: &CodexOptions{Catalogs: []CodexCatalog{{
		Path:  catalogPath,
		Scope: extensions.ScopeUser,
	}}}})
	assertNoCandidates(t, result)
	assertDiagnostic(t, result, "unsafe-plugin-source")
}

func TestCodexCatalogPluginTraversalFailsClosed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	catalogRoot := filepath.Join(root, "catalog")
	outside := filepath.Join(root, "outside-plugin")
	mustPlugin(t, outside, "alpha", ".codex-plugin")
	catalogPath := filepath.Join(catalogRoot, ".agents", "plugins", "marketplace.json")
	mustWrite(t, catalogPath, `{"name":"team","plugins":[{"name":"alpha","source":{"source":"local","path":"./../outside-plugin"},"policy":{"installation":"AVAILABLE","authentication":"ON_INSTALL"}}]}`)
	result := Resolve(Options{Codex: &CodexOptions{Catalogs: []CodexCatalog{{
		Path:  catalogPath,
		Scope: extensions.ScopeUser,
	}}}})
	assertNoCandidates(t, result)
	assertDiagnostic(t, result, "unsafe-plugin-source")
}

func TestRequiredCodexCatalogAbsenceIsDiagnostic(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	catalogPath := filepath.Join(root, ".agents", "plugins", "marketplace.json")
	result := Resolve(Options{Codex: &CodexOptions{Catalogs: []CodexCatalog{{Path: catalogPath, Scope: extensions.ScopeUser}}}})
	assertNoCandidates(t, result)
	assertDiagnostic(t, result, "invalid-marketplace")
}

func TestCodexStrictPluginFieldsAndDuplicateTOML(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"unknown plugin field": `[plugins."alpha@team"]
enabled = true
trusted = true
`,
		"unknown marketplace field": `[marketplaces.team]
source_type = "local"
source = "./team"
trusted = true
`,
		"duplicate key": `[plugins."alpha@team"]
enabled = true
enabled = false
`,
	}
	for name, body := range tests {
		body := body
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			configPath := filepath.Join(t.TempDir(), "config.toml")
			mustWrite(t, configPath, body)
			result := Resolve(Options{Codex: &CodexOptions{UserConfigPath: configPath}})
			assertNoCandidates(t, result)
			assertDiagnostic(t, result, "codex-config")
		})
	}
}

func TestCodexMarketplacePolicyEligibilityAndPreservation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	entries := []map[string]any{
		codexTestEntry("available", map[string]any{
			"installation":   codexInstallAvailable,
			"authentication": codexAuthOnInstall,
		}),
		codexTestEntry("default-codex", map[string]any{
			"installation":   codexInstallInstalledByDefault,
			"authentication": codexAuthOnUse,
			"products":       []string{codexProductChatGPT, codexProductCodex},
		}),
		codexTestEntry("not-available", map[string]any{
			"installation":   codexInstallNotAvailable,
			"authentication": codexAuthOnInstall,
		}),
		codexTestEntry("chatgpt-only", map[string]any{
			"installation":   codexInstallAvailable,
			"authentication": codexAuthOnUse,
			"products":       []string{codexProductChatGPT},
		}),
		codexTestEntry("no-products", map[string]any{
			"installation":   codexInstallAvailable,
			"authentication": codexAuthOnUse,
			"products":       []string{},
		}),
		codexTestEntry("atlas-only", map[string]any{
			"installation":   codexInstallAvailable,
			"authentication": codexAuthOnUse,
			"products":       []string{codexProductAtlas},
		}),
	}
	for _, name := range []string{"available", "default-codex"} {
		mustPlugin(t, filepath.Join(root, "plugins", name), name, ".codex-plugin")
	}
	// Ineligible entries deliberately point at absent roots. Policy must gate
	// them before Switchboard touches source bytes.
	catalogPath := filepath.Join(root, ".agents", "plugins", "marketplace.json")
	mustWriteJSON(t, catalogPath, map[string]any{"name": "team", "plugins": entries})

	result := Resolve(Options{Codex: &CodexOptions{Catalogs: []CodexCatalog{{
		Path: catalogPath, Scope: extensions.ScopeUser,
	}}}})
	if got, want := len(result.Candidates), 2; got != want {
		t.Fatalf("candidate count = %d, want %d: %#v", got, want, result)
	}
	available, ok := candidateByID(result, "available@team")
	if !ok || available.Provenance.MarketplacePolicy == nil {
		t.Fatalf("available policy was not preserved: %#v", available)
	}
	if available.Provenance.MarketplacePolicy.Products != nil {
		t.Fatalf("omitted products were not preserved as all-products: %#v", available.Provenance.MarketplacePolicy)
	}
	defaulted, ok := candidateByID(result, "default-codex@team")
	if !ok || defaulted.Provenance.MarketplacePolicy == nil {
		t.Fatalf("installed-by-default policy was not preserved: %#v", defaulted)
	}
	if got, want := defaulted.Provenance.MarketplacePolicy.Products, []string{codexProductChatGPT, codexProductCodex}; !reflect.DeepEqual(got, want) {
		t.Fatalf("products = %#v, want %#v", got, want)
	}
	assertDiagnostic(t, result, "plugin-not-available")
	if got := diagnosticCount(result, "plugin-product-ineligible"); got != 3 {
		t.Fatalf("product-ineligible diagnostic count = %d, want 3: %#v", got, result.Diagnostics)
	}
	if diagnosticCount(result, "unsafe-plugin-source") != 0 {
		t.Fatalf("ineligible source was inspected: %#v", result.Diagnostics)
	}
}

func TestCodexOfficialMarketplacePolicyFixture(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "codex-marketplace-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, name := range []string{"available-codex", "installed-by-default"} {
		mustPlugin(t, filepath.Join(root, "plugins", name), name, ".codex-plugin")
	}
	catalogPath := filepath.Join(root, ".agents", "plugins", "marketplace.json")
	mustWrite(t, catalogPath, string(raw))
	result := Resolve(Options{Codex: &CodexOptions{Catalogs: []CodexCatalog{{
		Path: catalogPath, Scope: extensions.ScopeUser,
	}}}})
	if got, want := len(result.Candidates), 2; got != want {
		t.Fatalf("official-shape candidate count = %d, want %d: %#v", got, want, result)
	}
	if !containsNativeID(result, "available-codex@official-shape") || !containsNativeID(result, "installed-by-default@official-shape") {
		t.Fatalf("official-shape eligible entries were not preserved: %#v", result.Candidates)
	}
	assertDiagnostic(t, result, "plugin-not-available")
	assertDiagnostic(t, result, "plugin-product-ineligible")
}

func TestCodexMarketplacePolicyDefaultsMatchNative(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	missing := codexTestEntry("missing", nil)
	delete(missing, "policy")
	entries := []map[string]any{
		missing,
		codexTestEntry("empty", map[string]any{}),
		codexTestEntry("partial", map[string]any{"installation": codexInstallInstalledByDefault}),
		codexTestEntry("null-products", map[string]any{"products": nil}),
		codexTestEntry("product-aliases", map[string]any{
			"products": []string{"codex", codexProductCodex, "atlas"},
		}),
	}
	for _, entry := range entries {
		name := entry["name"].(string)
		mustPlugin(t, filepath.Join(root, "plugins", name), name, ".codex-plugin")
	}
	catalogPath := filepath.Join(root, ".agents", "plugins", "marketplace.json")
	mustWriteJSON(t, catalogPath, map[string]any{"name": "team", "plugins": entries})
	result := Resolve(Options{Codex: &CodexOptions{Catalogs: []CodexCatalog{{
		Path: catalogPath, Scope: extensions.ScopeUser,
	}}}})
	if got, want := len(result.Candidates), len(entries); got != want {
		t.Fatalf("candidate count = %d, want %d: %#v", got, want, result)
	}
	for _, id := range []string{"missing@team", "empty@team", "null-products@team"} {
		candidate, ok := candidateByID(result, id)
		if !ok || candidate.Provenance.MarketplacePolicy == nil {
			t.Fatalf("missing defaulted policy for %q: %#v", id, candidate)
		}
		policy := candidate.Provenance.MarketplacePolicy
		if policy.Installation != codexInstallAvailable || policy.Authentication != codexAuthOnInstall || policy.Products != nil {
			t.Fatalf("defaulted policy for %q = %#v", id, policy)
		}
	}
	partial, _ := candidateByID(result, "partial@team")
	if policy := partial.Provenance.MarketplacePolicy; policy == nil || policy.Installation != codexInstallInstalledByDefault || policy.Authentication != codexAuthOnInstall {
		t.Fatalf("partial policy defaults = %#v", policy)
	}
	aliases, _ := candidateByID(result, "product-aliases@team")
	if got, want := aliases.Provenance.MarketplacePolicy.Products, []string{codexProductAtlas, codexProductCodex}; !reflect.DeepEqual(got, want) {
		t.Fatalf("normalized product aliases = %#v, want %#v", got, want)
	}
}

func TestCodexMarketplacePolicyFailsClosed(t *testing.T) {
	t.Parallel()
	tests := map[string]any{
		"null policy": nil,
		"unknown field": map[string]any{
			"installation": codexInstallAvailable, "authentication": codexAuthOnInstall, "trusted": true,
		},
		"unknown installation": map[string]any{
			"installation": "PROMPT", "authentication": codexAuthOnInstall,
		},
		"unknown authentication": map[string]any{
			"installation": codexInstallAvailable, "authentication": "NEVER",
		},
		"unknown product": map[string]any{
			"installation": codexInstallAvailable, "authentication": codexAuthOnUse, "products": []string{"SWITCHBOARD"},
		},
		"null installation": map[string]any{
			"installation": nil, "authentication": codexAuthOnUse,
		},
		"null authentication": map[string]any{
			"installation": codexInstallAvailable, "authentication": nil,
		},
	}
	for name, policy := range tests {
		name, policy := name, policy
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			entry := codexTestEntry("alpha", policy)
			catalogPath := filepath.Join(root, ".agents", "plugins", "marketplace.json")
			mustWriteJSON(t, catalogPath, map[string]any{"name": "team", "plugins": []any{entry}})
			result := Resolve(Options{Codex: &CodexOptions{Catalogs: []CodexCatalog{{
				Path: catalogPath, Scope: extensions.ScopeUser,
			}}}})
			assertNoCandidates(t, result)
			assertDiagnostic(t, result, "invalid-plugin-policy")
			if diagnosticCount(result, "unsafe-plugin-source") != 0 || diagnosticCount(result, "native-identity-unresolved") != 0 {
				t.Fatalf("invalid policy allowed source inspection: %#v", result.Diagnostics)
			}
		})
	}
}

func TestCodexInvalidPolicyRejectsWholeMarketplaceBeforeSourceReads(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	catalogPath := filepath.Join(root, ".agents", "plugins", "marketplace.json")
	mustWriteJSON(t, catalogPath, map[string]any{
		"name": "team",
		"plugins": []any{
			codexTestEntry("valid-but-absent", map[string]any{
				"installation": codexInstallAvailable, "authentication": codexAuthOnInstall,
			}),
			codexTestEntry("invalid", map[string]any{
				"installation": "PROMPT", "authentication": codexAuthOnInstall,
			}),
		},
	})
	result := Resolve(Options{Codex: &CodexOptions{Catalogs: []CodexCatalog{{
		Path: catalogPath, Scope: extensions.ScopeUser,
	}}}})
	assertNoCandidates(t, result)
	assertDiagnostic(t, result, "invalid-plugin-policy")
	if diagnosticCount(result, "unsafe-plugin-source") != 0 || diagnosticCount(result, "native-identity-unresolved") != 0 {
		t.Fatalf("invalid peer policy allowed a valid peer source read: %#v", result.Diagnostics)
	}
}

func TestCodexMarketplaceEntryLimitFailsBeforeSourceReads(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	entries := make([]map[string]any, maxCodexCatalogEntries+1)
	for index := range entries {
		entries[index] = codexTestEntry(fmt.Sprintf("plugin-%03d", index), map[string]any{
			"installation": codexInstallAvailable, "authentication": codexAuthOnInstall,
		})
	}
	catalogPath := filepath.Join(root, ".agents", "plugins", "marketplace.json")
	mustWriteJSON(t, catalogPath, map[string]any{"name": "team", "plugins": entries})
	result := Resolve(Options{Codex: &CodexOptions{Catalogs: []CodexCatalog{{
		Path: catalogPath, Scope: extensions.ScopeUser,
	}}}})
	assertNoCandidates(t, result)
	assertDiagnostic(t, result, "marketplace-entry-limit")
	if diagnosticCount(result, "unsafe-plugin-source") != 0 {
		t.Fatalf("oversized marketplace allowed source reads: %#v", result.Diagnostics)
	}
}

func TestCodexCatalogIdentityMustMatchManifest(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustPlugin(t, filepath.Join(root, "plugins", "alpha"), "different", ".codex-plugin")
	catalogPath := filepath.Join(root, ".agents", "plugins", "marketplace.json")
	mustWriteJSON(t, catalogPath, map[string]any{
		"name": "team",
		"plugins": []any{codexTestEntry("alpha", map[string]any{
			"installation": codexInstallAvailable, "authentication": codexAuthOnInstall,
		})},
	})
	result := Resolve(Options{Codex: &CodexOptions{Catalogs: []CodexCatalog{{
		Path: catalogPath, Scope: extensions.ScopeUser,
	}}}})
	assertNoCandidates(t, result)
	assertDiagnostic(t, result, "native-identity-mismatch")
}

func TestCodexCatalogInventoryDoesNotDigestSourceTree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	pluginRoot := filepath.Join(root, "plugins", "alpha")
	mustPlugin(t, pluginRoot, "alpha", ".codex-plugin")
	largePath := filepath.Join(pluginRoot, "large.bin")
	mustWrite(t, largePath, "")
	if err := os.Truncate(largePath, (256<<20)+1); err != nil {
		t.Fatal(err)
	}
	catalogPath := filepath.Join(root, ".agents", "plugins", "marketplace.json")
	mustWriteJSON(t, catalogPath, map[string]any{
		"name": "team",
		"plugins": []any{codexTestEntry("alpha", map[string]any{
			"installation": codexInstallAvailable, "authentication": codexAuthOnInstall,
		})},
	})
	result := Resolve(Options{Codex: &CodexOptions{Catalogs: []CodexCatalog{{
		Path: catalogPath, Scope: extensions.ScopeUser,
	}}}})
	if got := len(result.Candidates); got != 1 {
		t.Fatalf("catalog inventory unexpectedly digested the source tree: %#v", result)
	}
	if result.Candidates[0].ActivationEligible {
		t.Fatalf("manifest-only catalog identity became activation proof: %#v", result.Candidates[0])
	}
}

func TestCodexUserConfigSymlinkRejected(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	realConfig := filepath.Join(root, "real.toml")
	mustWrite(t, realConfig, `[plugins."alpha@team"]
enabled = true
`)
	configPath := filepath.Join(root, "config.toml")
	if err := os.Symlink(realConfig, configPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	result := Resolve(Options{Codex: &CodexOptions{UserConfigPath: configPath}})
	assertNoCandidates(t, result)
	assertDiagnostic(t, result, "codex-config")
}

func TestCodexDoesNotSearchCaches(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	configPath := filepath.Join(root, "config.toml")
	mustWrite(t, configPath, `[plugins."alpha@implicit"]
enabled = true
`)
	// A plausible cache is deliberately present but is not named by config.
	mustPlugin(t, filepath.Join(root, "cache", "implicit", "alpha"), "alpha", ".codex-plugin")
	result := Resolve(Options{Codex: &CodexOptions{UserConfigPath: configPath}})
	assertNoCandidates(t, result)
	assertDiagnostic(t, result, "enabled-plugin-install-unresolved")
}

func TestClaudeRejectsExpansionAndRelativeInstallPaths(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	settingsPath := filepath.Join(root, "settings.json")
	registryPath := filepath.Join(root, "installed_plugins.json")
	mustWrite(t, settingsPath, `{"enabledPlugins":{"alpha@market":true,"beta@market":true}}`)
	mustWriteJSON(t, registryPath, claudeInstalledPlugins{
		Version: claudeInstalledPluginsVersion,
		Plugins: map[string][]claudeInstalledRecord{
			"alpha@market": {{Scope: "user", InstallPath: "${HOME}/secret"}},
			"beta@market":  {{Scope: "user", InstallPath: "relative/plugin"}},
		},
	})
	result := Resolve(Options{Claude: &ClaudeOptions{
		InstalledPluginsPath: registryPath,
		Settings:             []ClaudeSettings{{Path: settingsPath, Scope: extensions.ScopeUser}},
	}})
	assertNoCandidates(t, result)
	if got := diagnosticCount(result, "unsafe-installed-path"); got != 2 {
		t.Fatalf("unsafe-installed-path count = %d, want 2: %#v", got, result.Diagnostics)
	}
}

func TestClaudeProjectSettingsSymlinkFailsClosed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	mustMkdir(t, filepath.Join(workspace, ".claude"))
	outside := filepath.Join(root, "outside-settings.json")
	mustWrite(t, outside, `{"enabledPlugins":{"secret@market":true}}`)
	settingsPath := filepath.Join(workspace, ".claude", "settings.json")
	if err := os.Symlink(outside, settingsPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	result := Resolve(Options{Claude: &ClaudeOptions{
		Workspace: workspace,
		Settings: []ClaudeSettings{{
			Path:        settingsPath,
			Scope:       extensions.ScopeWorkspace,
			ProjectPath: workspace,
		}},
	}})
	assertNoCandidates(t, result)
	assertDiagnostic(t, result, "claude-settings")
}

func TestClaudeProjectSettingsTraversalFailsClosed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	mustMkdir(t, workspace)
	outside := filepath.Join(root, "outside-settings.json")
	mustWrite(t, outside, `{"enabledPlugins":{"secret@market":true}}`)
	result := Resolve(Options{Claude: &ClaudeOptions{
		Workspace: workspace,
		Settings: []ClaudeSettings{{
			Path:        outside,
			Scope:       extensions.ScopeWorkspace,
			ProjectPath: workspace,
		}},
	}})
	assertNoCandidates(t, result)
	assertDiagnostic(t, result, "unsafe-settings-path")
}

func TestClaudeDuplicateJSONFailsClosed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	settingsPath := filepath.Join(root, "settings.json")
	mustWrite(t, settingsPath, `{"enabledPlugins":{"alpha@market":true},"enabledPlugins":{"alpha@market":false}}`)
	result := Resolve(Options{Claude: &ClaudeOptions{
		Settings: []ClaudeSettings{{Path: settingsPath, Scope: extensions.ScopeUser}},
	}})
	assertNoCandidates(t, result)
	assertDiagnostic(t, result, "claude-settings")
}

func TestClaudeEqualPrecedenceConflictIsOrderIndependentAndDoesNotVetoInstalled(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	one := filepath.Join(root, "one.json")
	two := filepath.Join(root, "two.json")
	registry := filepath.Join(root, "installed_plugins.json")
	pluginRoot := filepath.Join(root, "plugin")
	mustPlugin(t, pluginRoot, "alpha", ".claude-plugin")
	mustWrite(t, one, `{"enabledPlugins":{"alpha@market":true}}`)
	mustWrite(t, two, `{"enabledPlugins":{"alpha@market":false}}`)
	mustWriteJSON(t, registry, claudeInstalledPlugins{
		Version: claudeInstalledPluginsVersion,
		Plugins: map[string][]claudeInstalledRecord{
			"alpha@market": {{Scope: "user", InstallPath: pluginRoot}},
		},
	})
	options := Options{Claude: &ClaudeOptions{
		InstalledPluginsPath: registry,
		Settings: []ClaudeSettings{
			{Path: one, Scope: extensions.ScopeUser},
			{Path: two, Scope: extensions.ScopeUser},
		},
	}}
	first := Resolve(options)
	options.Claude.Settings[0], options.Claude.Settings[1] = options.Claude.Settings[1], options.Claude.Settings[0]
	second := Resolve(options)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("settings order changed conflict result:\n%#v\n%#v", first, second)
	}
	if got := len(first.Candidates); got != 1 {
		t.Fatalf("candidate count = %d, want 1: %#v", got, first)
	}
	if candidate := first.Candidates[0]; candidate.NativeEnabled || !candidate.ActivationEligible {
		t.Fatalf("ambiguous native state vetoed an exact installed identity: %#v", candidate)
	}
	assertDiagnostic(t, first, "ambiguous-enable")
}

func TestClaudeManagedEqualPrecedenceConflictDeniesActivation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	one := filepath.Join(root, "one.json")
	two := filepath.Join(root, "two.json")
	registry := filepath.Join(root, "installed_plugins.json")
	pluginRoot := filepath.Join(root, "plugin")
	mustPlugin(t, pluginRoot, "alpha", ".claude-plugin")
	mustWrite(t, one, `{"enabledPlugins":{"alpha@market":true}}`)
	mustWrite(t, two, `{"enabledPlugins":{"alpha@market":false}}`)
	mustWriteJSON(t, registry, claudeInstalledPlugins{
		Version: claudeInstalledPluginsVersion,
		Plugins: map[string][]claudeInstalledRecord{
			"alpha@market": {{Scope: "managed", InstallPath: pluginRoot}},
		},
	})
	result := Resolve(Options{Claude: &ClaudeOptions{
		InstalledPluginsPath: registry,
		Settings: []ClaudeSettings{
			{Path: one, Scope: extensions.ScopeManaged},
			{Path: two, Scope: extensions.ScopeManaged},
		},
	}})
	if got := len(result.Candidates); got != 1 {
		t.Fatalf("candidate count = %d, want 1: %#v", got, result)
	}
	candidate := result.Candidates[0]
	if candidate.NativeEnabled || candidate.ActivationEligible {
		t.Fatalf("ambiguous managed policy minted activation: %#v", candidate)
	}
	assertDiagnostic(t, result, "ambiguous-enable")
}

func TestClaudeHigherPrecedenceResolvesLowerConflict(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	one := filepath.Join(root, "one.json")
	two := filepath.Join(root, "two.json")
	managed := filepath.Join(root, "managed.json")
	registry := filepath.Join(root, "installed_plugins.json")
	pluginRoot := filepath.Join(root, "plugin")
	mustPlugin(t, pluginRoot, "alpha", ".claude-plugin")
	mustWrite(t, one, `{"enabledPlugins":{"alpha@market":true}}`)
	mustWrite(t, two, `{"enabledPlugins":{"alpha@market":false}}`)
	mustWrite(t, managed, `{"enabledPlugins":{"alpha@market":true}}`)
	mustWriteJSON(t, registry, claudeInstalledPlugins{
		Version: claudeInstalledPluginsVersion,
		Plugins: map[string][]claudeInstalledRecord{
			"alpha@market": {{Scope: "user", InstallPath: pluginRoot}},
		},
	})
	result := Resolve(Options{Claude: &ClaudeOptions{
		InstalledPluginsPath: registry,
		Settings: []ClaudeSettings{
			{Path: one, Scope: extensions.ScopeUser},
			{Path: two, Scope: extensions.ScopeUser},
			{Path: managed, Scope: extensions.ScopeManaged},
		},
	}})
	if got := len(result.Candidates); got != 1 {
		t.Fatalf("candidate count = %d, want 1: %#v", got, result)
	}
	if diagnosticCount(result, "ambiguous-enable") != 0 {
		t.Fatalf("superseded lower conflict remained fatal: %#v", result.Diagnostics)
	}
}

func TestClaudeInstalledRecordOrderIsDeterministicAndUnpreferred(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	settings := filepath.Join(root, "settings.json")
	registry := filepath.Join(root, "installed_plugins.json")
	userRoot := filepath.Join(root, "user-plugin")
	managedRoot := filepath.Join(root, "managed-plugin")
	mustPlugin(t, userRoot, "alpha", ".claude-plugin")
	mustPlugin(t, managedRoot, "alpha", ".claude-plugin")
	mustWrite(t, settings, `{"enabledPlugins":{"alpha@market":true}}`)
	records := []claudeInstalledRecord{
		{Scope: "user", InstallPath: userRoot},
		{Scope: "managed", InstallPath: managedRoot},
	}
	writeRegistry := func(records []claudeInstalledRecord) {
		t.Helper()
		mustWriteJSON(t, registry, claudeInstalledPlugins{
			Version: claudeInstalledPluginsVersion,
			Plugins: map[string][]claudeInstalledRecord{"alpha@market": records},
		})
	}
	options := Options{Claude: &ClaudeOptions{
		InstalledPluginsPath: registry,
		Settings:             []ClaudeSettings{{Path: settings, Scope: extensions.ScopeUser}},
	}}
	writeRegistry(records)
	first := Resolve(options)
	slices.Reverse(records)
	writeRegistry(records)
	second := Resolve(options)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("installed record order changed result:\n%#v\n%#v", first, second)
	}
	if got := len(first.Candidates); got != 2 {
		t.Fatalf("candidate count = %d, want 2", got)
	}
	for _, candidate := range first.Candidates {
		if candidate.ActivationEligible {
			t.Fatalf("ambiguous installed record was activation-eligible: %#v", candidate)
		}
	}
	assertDiagnostic(t, first, "ambiguous-installed-record")
}

func TestClaudeInstalledIdentityMismatchCannotActivate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	settings := filepath.Join(root, "settings.json")
	registry := filepath.Join(root, "installed_plugins.json")
	pluginRoot := filepath.Join(root, "plugin")
	mustPlugin(t, pluginRoot, "different", ".claude-plugin")
	mustWrite(t, settings, `{"enabledPlugins":{"alpha@market":true}}`)
	mustWriteJSON(t, registry, claudeInstalledPlugins{
		Version: claudeInstalledPluginsVersion,
		Plugins: map[string][]claudeInstalledRecord{
			"alpha@market": {{Scope: "user", InstallPath: pluginRoot}},
		},
	})
	result := Resolve(Options{Claude: &ClaudeOptions{
		InstalledPluginsPath: registry,
		Settings:             []ClaudeSettings{{Path: settings, Scope: extensions.ScopeUser}},
	}})
	if got := len(result.Candidates); got != 1 {
		t.Fatalf("candidate count = %d, want 1: %#v", got, result)
	}
	if result.Candidates[0].ActivationEligible {
		t.Fatalf("identity-mismatched installed plugin was activation-eligible: %#v", result.Candidates[0])
	}
	assertDiagnostic(t, result, "native-identity-mismatch")
}

func TestClaudeDisabledInstalledIdentityRemainsActivationEligible(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	settings := filepath.Join(root, "settings.json")
	registry := filepath.Join(root, "installed_plugins.json")
	pluginRoot := filepath.Join(root, "plugin")
	mustPlugin(t, pluginRoot, "alpha", ".claude-plugin")
	mustWrite(t, settings, `{"enabledPlugins":{"alpha@market":false}}`)
	mustWriteJSON(t, registry, claudeInstalledPlugins{
		Version: claudeInstalledPluginsVersion,
		Plugins: map[string][]claudeInstalledRecord{
			"alpha@market": {{Scope: "user", InstallPath: pluginRoot}},
		},
	})
	result := Resolve(Options{Claude: &ClaudeOptions{
		InstalledPluginsPath: registry,
		Settings:             []ClaudeSettings{{Path: settings, Scope: extensions.ScopeUser}},
	}})
	if got := len(result.Candidates); got != 1 {
		t.Fatalf("candidate count = %d, want 1: %#v", got, result)
	}
	candidate := result.Candidates[0]
	if candidate.NativeEnabled {
		t.Fatalf("disabled native plugin was marked native-enabled: %#v", candidate)
	}
	if !candidate.ActivationEligible {
		t.Fatalf("exact disabled installation lost independent Switchboard eligibility: %#v", candidate)
	}
}

func TestClaudeInstalledIdentityWithoutNativeSettingRemainsActivationEligible(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	registry := filepath.Join(root, "installed_plugins.json")
	pluginRoot := filepath.Join(root, "plugin")
	mustPlugin(t, pluginRoot, "alpha", ".claude-plugin")
	mustWriteJSON(t, registry, claudeInstalledPlugins{
		Version: claudeInstalledPluginsVersion,
		Plugins: map[string][]claudeInstalledRecord{
			"alpha@market": {{Scope: "user", InstallPath: pluginRoot}},
		},
	})
	result := Resolve(Options{Claude: &ClaudeOptions{InstalledPluginsPath: registry}})
	if got := len(result.Candidates); got != 1 {
		t.Fatalf("candidate count = %d, want 1: %#v", got, result)
	}
	candidate := result.Candidates[0]
	if candidate.NativeEnabled || !candidate.ActivationEligible {
		t.Fatalf("native state absence vetoed an exact installed identity: %#v", candidate)
	}
	if candidate.Provenance.EnablementPath != "" || candidate.Provenance.EnablementScope != "" {
		t.Fatalf("unmentioned plugin invented native enablement provenance: %#v", candidate.Provenance)
	}
}

func TestClaudeInstalledRegistryRejectsInvalidUnmentionedID(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	registry := filepath.Join(root, "installed_plugins.json")
	pluginRoot := filepath.Join(root, "plugin")
	mustPlugin(t, pluginRoot, "alpha", ".claude-plugin")
	mustWriteJSON(t, registry, claudeInstalledPlugins{
		Version: claudeInstalledPluginsVersion,
		Plugins: map[string][]claudeInstalledRecord{
			"../alpha@market": {{Scope: "user", InstallPath: pluginRoot}},
		},
	})
	result := Resolve(Options{Claude: &ClaudeOptions{InstalledPluginsPath: registry}})
	assertNoCandidates(t, result)
	assertDiagnostic(t, result, "invalid-native-id")
}

func TestClaudeManagedDisableDeniesActivation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	settings := filepath.Join(root, "managed-settings.json")
	registry := filepath.Join(root, "installed_plugins.json")
	pluginRoot := filepath.Join(root, "plugin")
	mustPlugin(t, pluginRoot, "alpha", ".claude-plugin")
	mustWrite(t, settings, `{"enabledPlugins":{"alpha@market":false}}`)
	mustWriteJSON(t, registry, claudeInstalledPlugins{
		Version: claudeInstalledPluginsVersion,
		Plugins: map[string][]claudeInstalledRecord{
			"alpha@market": {{Scope: "managed", InstallPath: pluginRoot}},
		},
	})
	result := Resolve(Options{Claude: &ClaudeOptions{
		InstalledPluginsPath: registry,
		Settings:             []ClaudeSettings{{Path: settings, Scope: extensions.ScopeManaged}},
	}})
	if got := len(result.Candidates); got != 1 {
		t.Fatalf("candidate count = %d, want 1: %#v", got, result)
	}
	candidate := result.Candidates[0]
	if candidate.NativeEnabled || candidate.ActivationEligible {
		t.Fatalf("managed-disabled plugin became activation-capable: %#v", candidate)
	}
	assertDiagnostic(t, result, "managed-plugin-disabled")
}

func TestClaudeUnsupportedInstalledRegistryVersionFailsClosed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	settings := filepath.Join(root, "settings.json")
	registry := filepath.Join(root, "installed_plugins.json")
	mustWrite(t, settings, `{"enabledPlugins":{"alpha@market":true}}`)
	mustWrite(t, registry, `{"version":999,"plugins":{}}`)
	result := Resolve(Options{Claude: &ClaudeOptions{
		InstalledPluginsPath: registry,
		Settings:             []ClaudeSettings{{Path: settings, Scope: extensions.ScopeUser}},
	}})
	assertNoCandidates(t, result)
	assertDiagnostic(t, result, "unsupported-installed-registry")
}

func TestClaudeInstalledRecordLimitFailsBeforeRootInspection(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	registry := filepath.Join(root, "installed_plugins.json")
	records := make([]claudeInstalledRecord, maxClaudeInstalledRecords+1)
	for index := range records {
		records[index] = claudeInstalledRecord{
			Scope:       "user",
			InstallPath: filepath.Join(root, fmt.Sprintf("absent-%03d", index)),
		}
	}
	mustWriteJSON(t, registry, claudeInstalledPlugins{
		Version: claudeInstalledPluginsVersion,
		Plugins: map[string][]claudeInstalledRecord{"alpha@market": records},
	})
	result := Resolve(Options{Claude: &ClaudeOptions{InstalledPluginsPath: registry}})
	assertNoCandidates(t, result)
	assertDiagnostic(t, result, "installed-record-limit")
	if diagnosticCount(result, "unsafe-installed-path") != 0 {
		t.Fatalf("oversized installed registry allowed root inspection: %#v", result.Diagnostics)
	}
}

func TestClaudeRegistrySymlinkRejected(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	settingsPath := filepath.Join(root, "settings.json")
	mustWrite(t, settingsPath, `{"enabledPlugins":{"alpha@market":true}}`)
	realRegistry := filepath.Join(root, "real-registry.json")
	mustWrite(t, realRegistry, `{"version":2,"plugins":{}}`)
	registryPath := filepath.Join(root, "installed_plugins.json")
	if err := os.Symlink(realRegistry, registryPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	result := Resolve(Options{Claude: &ClaudeOptions{
		InstalledPluginsPath: registryPath,
		Settings:             []ClaudeSettings{{Path: settingsPath, Scope: extensions.ScopeUser}},
	}})
	assertNoCandidates(t, result)
	assertDiagnostic(t, result, "claude-installed-registry")
}

func TestConfigurationSizeIsBounded(t *testing.T) {
	t.Parallel()
	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath, make([]byte, maxConfigBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	result := Resolve(Options{Codex: &CodexOptions{UserConfigPath: configPath}})
	assertNoCandidates(t, result)
	assertDiagnostic(t, result, "codex-config")
}

func TestDefaultLocalOptionsUseConventionalPathsAndAllowAbsence(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	workspace := filepath.Join(root, "workspace")
	mustMkdir(t, home)
	mustMkdir(t, workspace)
	options := DefaultLocalOptions(home, workspace)
	if got, want := options.Codex.UserConfigPath, filepath.Join(home, ".codex", "config.toml"); got != want {
		t.Fatalf("Codex config path = %q, want %q", got, want)
	}
	if got, want := len(options.Codex.Catalogs), 2; got != want {
		t.Fatalf("Codex catalog count = %d, want %d", got, want)
	}
	if got, want := options.Claude.InstalledPluginsPath, filepath.Join(home, ".claude", "plugins", "installed_plugins.json"); got != want {
		t.Fatalf("Claude registry path = %q, want %q", got, want)
	}
	result := Resolve(options)
	assertNoCandidates(t, result)
	if len(result.Diagnostics) != 0 {
		t.Fatalf("absent conventional defaults produced diagnostics: %#v", result.Diagnostics)
	}
}

func buildGoldenFixture(t *testing.T) (string, Options) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	userRoot := filepath.Join(root, "codex-user")
	workspace := filepath.Join(root, "workspace")
	otherProject := filepath.Join(root, "other-project")
	mustMkdir(t, otherProject)

	personalMarketplace := filepath.Join(userRoot, "marketplace")
	mustMarketplace(t, personalMarketplace, "personal", "alpha", "mixed", "off")
	userConfig := filepath.Join(userRoot, "config.toml")
	mustWrite(t, userConfig, `model = "unrelated-and-ignored"

[marketplaces.personal]
source_type = "local"
source = "./marketplace"
last_updated = "2026-08-17T00:00:00Z"

[plugins."alpha@personal"]
enabled = true

[plugins."alpha@personal".mcp_servers.readonly]
enabled = false

[plugins."off@personal"]
enabled = false
`)

	mustMarketplace(t, workspace, "project", "project-tool")
	projectCatalog := filepath.Join(workspace, ".agents", "plugins", "marketplace.json")

	claudeRoot := filepath.Join(root, "claude")
	userSettings := filepath.Join(claudeRoot, "user-settings.json")
	managedSettings := filepath.Join(claudeRoot, "managed-settings.json")
	projectSettings := filepath.Join(workspace, ".claude", "settings.json")
	localSettings := filepath.Join(workspace, ".claude", "settings.local.json")
	mustWrite(t, userSettings, `{"enabledPlugins":{"claude-user@market":true,"override@market":true}}`)
	mustWrite(t, projectSettings, `{"enabledPlugins":{"claude-project@market":true,"override@market":false}}`)
	mustWrite(t, localSettings, `{"enabledPlugins":{"local-off@market":true}}`)
	mustWrite(t, managedSettings, `{"enabledPlugins":{"claude-managed@market":true,"local-off@market":false}}`)

	userPlugin := filepath.Join(claudeRoot, "plugins", "user")
	projectPlugin := filepath.Join(claudeRoot, "plugins", "project")
	managedPlugin := filepath.Join(claudeRoot, "plugins", "managed")
	for name, pluginRoot := range map[string]string{
		"claude-user":    userPlugin,
		"claude-project": projectPlugin,
		"claude-managed": managedPlugin,
	} {
		mustPlugin(t, pluginRoot, name, ".claude-plugin")
	}
	registryPath := filepath.Join(claudeRoot, "installed_plugins.json")
	mustWriteJSON(t, registryPath, claudeInstalledPlugins{
		Version: claudeInstalledPluginsVersion,
		Plugins: map[string][]claudeInstalledRecord{
			"claude-user@market": {{Scope: "user", InstallPath: userPlugin}},
			"claude-project@market": {
				{Scope: "project", InstallPath: projectPlugin, ProjectPath: workspace},
				{Scope: "project", InstallPath: projectPlugin, ProjectPath: otherProject},
			},
			"claude-managed@market": {{Scope: "managed", InstallPath: managedPlugin}},
		},
	})

	return root, Options{
		Codex: &CodexOptions{
			UserConfigPath: userConfig,
			Workspace:      workspace,
			Catalogs: []CodexCatalog{
				{Path: projectCatalog, Scope: extensions.ScopeWorkspace, ProjectPath: workspace},
			},
		},
		Claude: &ClaudeOptions{
			Workspace:            workspace,
			InstalledPluginsPath: registryPath,
			Settings: []ClaudeSettings{
				{Path: localSettings, Scope: extensions.ScopeLocal, ProjectPath: workspace},
				{Path: managedSettings, Scope: extensions.ScopeManaged},
				{Path: projectSettings, Scope: extensions.ScopeWorkspace, ProjectPath: workspace},
				{Path: userSettings, Scope: extensions.ScopeUser},
			},
		},
	}
}

func mustMarketplace(t *testing.T, root, name string, plugins ...string) {
	t.Helper()
	entries := make([]map[string]any, 0, len(plugins))
	for _, plugin := range plugins {
		entries = append(entries, map[string]any{
			"name": plugin,
			"source": map[string]string{
				"source": "local",
				"path":   "./plugins/" + plugin,
			},
			"policy": map[string]any{
				"installation":   codexInstallAvailable,
				"authentication": codexAuthOnInstall,
			},
			"category": "Productivity",
		})
		mustPlugin(t, filepath.Join(root, "plugins", plugin), plugin, ".codex-plugin")
	}
	mustWriteJSON(t, filepath.Join(root, ".agents", "plugins", "marketplace.json"), map[string]any{
		"name":    name,
		"plugins": entries,
	})
}

func codexTestEntry(name string, policy any) map[string]any {
	return map[string]any{
		"name": name,
		"source": map[string]string{
			"source": "local",
			"path":   "./plugins/" + name,
		},
		"policy":   policy,
		"category": "Productivity",
	}
}

func mustPlugin(t *testing.T, root, name, manifestDir string) {
	t.Helper()
	mustWriteJSON(t, filepath.Join(root, manifestDir, "plugin.json"), map[string]string{"name": name})
}

func mustWriteJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, string(raw))
}

func mustWrite(t *testing.T, path, contents string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func containsNativeID(result Result, id string) bool {
	for _, candidate := range result.Candidates {
		if candidate.NativeID == id {
			return true
		}
	}
	return false
}

func candidateByID(result Result, id string) (ResolvedCandidate, bool) {
	for _, candidate := range result.Candidates {
		if candidate.NativeID == id {
			return candidate, true
		}
	}
	return ResolvedCandidate{}, false
}

func assertNoCandidates(t *testing.T, result Result) {
	t.Helper()
	if len(result.Candidates) != 0 {
		t.Fatalf("unexpected candidates: %#v", result.Candidates)
	}
}

func assertDiagnostic(t *testing.T, result Result, code string) {
	t.Helper()
	if diagnosticCount(result, code) == 0 {
		t.Fatalf("missing diagnostic %q: %#v", code, result.Diagnostics)
	}
}

func diagnosticCount(result Result, code string) int {
	count := 0
	for _, diagnostic := range result.Diagnostics {
		if diagnostic.Code == code {
			count++
		}
	}
	return count
}
