//go:build unix

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
	"github.com/switchboard-code/switchboard/internal/tools"
)

const (
	crashToolDraftHelperEnv = "SWITCHBOARD_TOOL_DRAFT_CRASH_HELPER"
	crashToolDraftStoreEnv  = "SWITCHBOARD_TOOL_DRAFT_CRASH_STORE"
	crashToolDraftWorkEnv   = "SWITCHBOARD_TOOL_DRAFT_CRASH_WORKSPACE"
	crashToolDraftIDEnv     = "SWITCHBOARD_TOOL_DRAFT_CRASH_ID_FILE"
	crashToolDraftReadyEnv  = "SWITCHBOARD_TOOL_DRAFT_CRASH_READY_FILE"
	crashToolDraftCallID    = "write-visible-before-kill"
)

func TestSIGKILLToolDraftIsExcludedAndNeverExecutedOnResume(t *testing.T) {
	if os.Getenv(crashToolDraftHelperEnv) == "1" {
		runToolDraftCrashHelper(t)
		return
	}

	root := t.TempDir()
	storeRoot := filepath.Join(root, "sessions")
	workspace := filepath.Join(root, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	protected := filepath.Join(workspace, "must-not-change.txt")
	if err := os.WriteFile(protected, []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	idFile := filepath.Join(root, "session-id")
	readyFile := filepath.Join(root, "tool-draft-durable")
	cmd := exec.Command(os.Args[0], "-test.run=^TestSIGKILLToolDraftIsExcludedAndNeverExecutedOnResume$")
	cmd.Env = append(os.Environ(),
		crashToolDraftHelperEnv+"=1",
		crashToolDraftStoreEnv+"="+storeRoot,
		crashToolDraftWorkEnv+"="+workspace,
		crashToolDraftIDEnv+"="+idFile,
		crashToolDraftReadyEnv+"="+readyFile,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	defer func() {
		if !killed && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(readyFile); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper never durably checkpointed the tool draft: %s", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	killed = true
	if err := cmd.Wait(); err == nil {
		t.Fatal("tool-draft helper exited successfully instead of being killed")
	}

	idBytes, err := os.ReadFile(idFile)
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewStore(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(strings.TrimSpace(string(idBytes)))
	if err != nil {
		t.Fatalf("opening the killed process's session: %v\nhelper stderr: %s", err, stderr.String())
	}
	defer reopened.Close()
	assertIncompleteToolDraftWithoutResult(t, reopened.State().Messages)

	replay := provider.ReplayRequest(provider.Request{Messages: reopened.State().Messages})
	if len(replay.Messages) != 1 || replay.Messages[0].Role != provider.RoleUser {
		t.Fatalf("incomplete tool draft reached replay before resume: %#v", replay.Messages)
	}

	registry, err := tools.NewRegistry(workspace, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	next := &scriptedProvider{turns: []scriptTurn{textTurn("continued safely")}}
	loop := &Loop{
		Provider: next,
		Target:   provider.RouteTarget{Provider: "scripted", Surface: "local", ModelID: "resumed"},
		Tools:    registry,
		Perms:    permission.NewEngine(permission.ModeDefault, execution.Capability{}),
		Session:  reopened,
		Observer: NopObserver{},
	}
	if err := loop.BindSession(reopened); err != nil {
		t.Fatalf("binding killed session: %v", err)
	}
	assertIncompleteToolDraftWithoutResult(t, reopened.State().Messages)
	if err := loop.Turn(context.Background(), "continue after the crash"); err != nil {
		t.Fatal(err)
	}
	if len(next.requests) != 1 {
		t.Fatalf("resumed provider requests = %d, want 1", len(next.requests))
	}
	for _, message := range next.requests[0].Messages {
		if message.Incomplete {
			t.Fatalf("resumed provider received an incomplete message: %#v", message)
		}
		for _, block := range message.Content {
			switch block := block.(type) {
			case provider.ToolUse:
				if block.ID == crashToolDraftCallID {
					t.Fatalf("resumed provider received killed tool use: %#v", block)
				}
			case provider.ToolResult:
				if block.ToolUseID == crashToolDraftCallID {
					t.Fatalf("resume invented a result for killed tool use: %#v", block)
				}
			}
		}
	}
	for _, message := range reopened.State().Messages {
		if message.Role == provider.RoleTool {
			t.Fatalf("resume invented a durable tool-result message: %#v", message)
		}
	}
	data, err := os.ReadFile(protected)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "before" {
		t.Fatalf("resume executed the killed write tool: %q", data)
	}
}

func assertIncompleteToolDraftWithoutResult(t *testing.T, messages []provider.Message) {
	t.Helper()
	if len(messages) != 2 || messages[0].Role != provider.RoleUser || !messages[1].Incomplete {
		t.Fatalf("killed tool-draft messages = %#v", messages)
	}
	uses := messages[1].ToolUses()
	if len(uses) != 1 || uses[0].ID != crashToolDraftCallID || uses[0].Name != "write" {
		t.Fatalf("killed assistant draft lost its tool evidence: %#v", messages[1])
	}
	for _, message := range messages {
		for _, block := range message.Content {
			if result, ok := block.(provider.ToolResult); ok && result.ToolUseID == crashToolDraftCallID {
				t.Fatalf("resume invented a result for incomplete tool use: %#v", result)
			}
		}
	}
}

func runToolDraftCrashHelper(t *testing.T) {
	store, err := session.NewStore(os.Getenv(crashToolDraftStoreEnv))
	if err != nil {
		t.Fatal(err)
	}
	workspace := os.Getenv(crashToolDraftWorkEnv)
	sess, err := store.Create(workspace, "scripted/local/tool-crash", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv(crashToolDraftIDEnv), []byte(sess.ID()), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(workspace, execution.Capability{})
	if err != nil {
		t.Fatal(err)
	}
	loop := &Loop{
		Provider: &crashToolDraftProvider{ready: os.Getenv(crashToolDraftReadyEnv)},
		Target:   provider.RouteTarget{Provider: "scripted", Surface: "local", ModelID: "tool-crash"},
		Tools:    registry,
		Perms:    permission.NewEngine(permission.ModeDefault, execution.Capability{}),
		Session:  sess,
		Observer: NopObserver{},
	}
	if err := loop.Turn(context.Background(), "write the protected file"); err != nil {
		t.Fatalf("tool-draft crash helper returned before it could be killed: %v", err)
	}
}

type crashToolDraftProvider struct{ ready string }

func (*crashToolDraftProvider) Name() string { return "crash-tool-draft" }
func (p *crashToolDraftProvider) Stream(context.Context, provider.RouteTarget, provider.Request) (provider.EventStream, error) {
	return &crashToolDraftStream{ready: p.ready}, nil
}
func (*crashToolDraftProvider) CountTokens(context.Context, provider.RouteTarget, provider.Request) (provider.TokenEstimate, error) {
	return provider.TokenEstimate{}, nil
}
func (*crashToolDraftProvider) Probe(context.Context, provider.RouteTarget) (provider.ProbeResult, error) {
	return provider.ProbeResult{Reachable: true, ModelPresent: true}, nil
}

type crashToolDraftStream struct {
	ready string
	step  int
}

func (s *crashToolDraftStream) Next() (provider.Event, error) {
	switch s.step {
	case 0:
		s.step++
		return provider.Event{Type: provider.EventToolUse, Index: 0, ToolUse: &provider.ToolUse{
			ID: crashToolDraftCallID, Name: "write",
			Input: json.RawMessage(`{"path":"must-not-change.txt","content":"after"}`),
		}}, nil
	case 1:
		s.step++
		// streamDraft commits every tool-use event synchronously. Reaching the
		// next Next call therefore proves the draft is durable before the parent
		// sends SIGKILL.
		if err := os.WriteFile(s.ready, []byte("tool draft durable"), 0o600); err != nil {
			return provider.Event{}, err
		}
		select {}
	default:
		return provider.Event{}, errors.New("unreachable tool-draft stream state")
	}
}

func (*crashToolDraftStream) Close() error { return nil }
