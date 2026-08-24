//go:build unix

package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/hooks"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
)

func TestPostToolHookCannotLaunderTruncatedCredentialToProviderOrSession(t *testing.T) {
	token := "ghp_" + strings.Repeat("H", 36)
	const hookExcerptCap = 4 << 10
	const visibleBeforeOldCut = 8

	tests := []struct {
		name        string
		result      string
		wantLocal   string
		wantRawUses int
	}{
		{
			name:        "complete credential inside cap",
			result:      "ordinary " + token + " tail",
			wantLocal:   "[hook] " + token,
			wantRawUses: 2,
		},
		{
			name:        "credential straddles cap",
			result:      strings.Repeat("x", hookExcerptCap-visibleBeforeOldCut) + token + " tail",
			wantLocal:   "[hook] safe excerpt",
			wantRawUses: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			h := newHarness(t, permission.ModeDefault,
				toolTurn(use("call_1", "read", `{"path":"credential.txt"}`)),
				textTurn("done"),
			)
			if err := os.WriteFile(filepath.Join(h.root, "credential.txt"), []byte(test.result), 0o600); err != nil {
				t.Fatal(err)
			}

			hookFile := filepath.Join(t.TempDir(), hooks.FileName)
			const hookConfig = `
[[hooks.post_tool]]
tools = ["read"]
run = '''fragment=$(grep -o 'ghp_[A-Za-z0-9]*' || true); if [ -n "$fragment" ]; then printf '%s' "$fragment"; else printf 'safe excerpt'; fi'''
`
			if err := os.WriteFile(hookFile, []byte(hookConfig), 0o600); err != nil {
				t.Fatal(err)
			}
			hookSet, err := hooks.Load(hookFile, h.root)
			if err != nil {
				t.Fatal(err)
			}
			h.loop.Hooks = hookSet

			if err := h.loop.Turn(context.Background(), "read the file"); err != nil {
				t.Fatal(err)
			}

			// Hooks and observers are local. This proves the inside-cap control was
			// delivered whole, while the boundary-straddling token never became a
			// short hook echo that the common egress scanner could not recognize.
			h.obs.mu.Lock()
			observed := append([]string(nil), h.obs.toolEnds...)
			var local string
			if len(h.obs.results) == 1 {
				local = h.obs.results[0].Content
			}
			h.obs.mu.Unlock()
			if len(observed) != 1 || local == "" || !strings.Contains(local, test.wantLocal) {
				t.Fatalf("local hook result = %q, tool ends = %v", local, observed)
			}
			if got := strings.Count(local, token); got != test.wantRawUses {
				t.Fatalf("local complete token uses = %d, want %d in %q", got, test.wantRawUses, local)
			}

			if len(h.provider.requests) != 2 {
				t.Fatalf("provider requests = %d, want 2", len(h.provider.requests))
			}
			providerWire, err := json.Marshal(h.provider.requests[1])
			if err != nil {
				t.Fatal(err)
			}
			assertNoHookCredentialFragment(t, string(providerWire), token)

			state := h.sess.State()
			if len(state.Messages) < 3 || state.Messages[2].Role != provider.RoleTool {
				t.Fatalf("session messages = %+v", state.Messages)
			}
			stored, ok := state.Messages[2].Content[0].(provider.ToolResult)
			if !ok {
				t.Fatalf("stored tool result = %#v", state.Messages[2].Content)
			}
			assertNoHookCredentialFragment(t, stored.Content, token)

			durable, err := os.ReadFile(h.sess.Path())
			if err != nil {
				t.Fatal(err)
			}
			assertNoHookCredentialFragment(t, string(durable), token)
		})
	}
}

func assertNoHookCredentialFragment(t *testing.T, text, token string) {
	t.Helper()
	if strings.Contains(text, token) || strings.Contains(text, token[:8]) || strings.Contains(text, "ghp_") {
		t.Fatalf("provider/session boundary retained raw credential material: %q", text)
	}
	if !strings.Contains(text, "[redacted: a GitHub token]") {
		t.Fatalf("provider/session boundary lost the redaction marker: %q", text)
	}
}
