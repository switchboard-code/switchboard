//go:build unix

package hooks

import (
	"context"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/permission"
)

func setWith(workspace string, hs ...Hook) *Set {
	return &Set{workspace: workspace, hooks: hs}
}

func TestPreToolBlocksOnNonZeroExit(t *testing.T) {
	s := setWith(t.TempDir(), Hook{
		Event: PreTool,
		Tools: []string{"exec"},
		Run:   `echo "not on my watch" >&2; exit 1`,
	})

	msg, blocked := s.PreTool(context.Background(), reqFor("exec"))
	if !blocked {
		t.Fatal("a non-zero pre_tool exit must block the call")
	}
	if !strings.Contains(msg, "not on my watch") {
		t.Errorf("msg = %q, want the hook's own words", msg)
	}

	// A tool the hook does not name passes untouched.
	if _, blocked := s.PreTool(context.Background(), reqFor("read")); blocked {
		t.Error("a hook scoped to exec must not block read")
	}
}

func TestPreToolPassesOnZeroExit(t *testing.T) {
	s := setWith(t.TempDir(), Hook{Event: PreTool, Run: "true"})
	if msg, blocked := s.PreTool(context.Background(), reqFor("exec")); blocked {
		t.Fatalf("a passing hook must not block: %s", msg)
	}
}

func TestPreToolTimeoutFailsClosed(t *testing.T) {
	s := setWith(t.TempDir(), Hook{
		Event:          PreTool,
		Run:            "sleep 30",
		TimeoutSeconds: 1,
	})
	msg, blocked := s.PreTool(context.Background(), reqFor("exec"))
	if !blocked {
		t.Fatal("a hook that hangs is a gate that failed closed, not open")
	}
	if !strings.Contains(msg, "did not answer") {
		t.Errorf("msg = %q", msg)
	}
}

func TestPreToolReceivesThePayload(t *testing.T) {
	s := setWith(t.TempDir(), Hook{
		Event: PreTool,
		// The hook reads JSON on stdin and the same facts from SB_ vars;
		// block only when both carried the tool name, then say so.
		Run: `payload=$(cat); case "$payload" in *'"tool":"exec"'*) ;; *) exit 0;; esac; [ "$SB_HOOK_TOOL" = "exec" ] && { echo "saw exec twice"; exit 1; }`,
	})
	msg, blocked := s.PreTool(context.Background(), reqFor("exec"))
	if !blocked || !strings.Contains(msg, "saw exec twice") {
		t.Fatalf("payload did not reach the hook: blocked=%v msg=%q", blocked, msg)
	}
}

func TestPostToolOutputRidesTheResult(t *testing.T) {
	s := setWith(t.TempDir(), Hook{
		Event: PostTool,
		Tools: []string{"write"},
		Run:   `echo "reformatted after write"`,
	})
	note := s.PostTool(context.Background(), permission.Request{Tool: "write", Path: "a.go"}, "wrote a.go", false)
	if !strings.Contains(note, "[hook] reformatted after write") {
		t.Errorf("note = %q", note)
	}

	// Silence is silence: a hook with no output adds nothing.
	quiet := setWith(t.TempDir(), Hook{Event: PostTool, Run: "true"})
	if note := quiet.PostTool(context.Background(), reqFor("exec"), "", false); note != "" {
		t.Errorf("a silent hook produced %q", note)
	}
}

func TestPostToolFailureIsNotedNotFatal(t *testing.T) {
	s := setWith(t.TempDir(), Hook{Event: PostTool, Run: "exit 3"})
	note := s.PostTool(context.Background(), reqFor("exec"), "", false)
	if !strings.Contains(note, "exited 3") {
		t.Errorf("note = %q, want the exit reported", note)
	}
}

func TestHookOutputCrossingCaptureBoundaryIsWithheld(t *testing.T) {
	token := "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	noisyFailure := `i=0; while [ "$i" -lt 5000 ]; do printf x; i=$((i+1)); done; printf '` + token + `'; exit 3`
	pre := setWith(t.TempDir(), Hook{Event: PreTool, Run: noisyFailure})
	msg, blocked := pre.PreTool(context.Background(), reqFor("exec"))
	if !blocked || !strings.Contains(msg, "output withheld") || strings.Contains(msg, "ghp_") {
		t.Fatalf("truncated pre-hook output = blocked=%v %q", blocked, msg)
	}

	post := setWith(t.TempDir(), Hook{Event: PostTool, Run: noisyFailure})
	note := post.PostTool(context.Background(), reqFor("exec"), "", false)
	if !strings.Contains(note, "output withheld") || strings.Contains(note, "ghp_") {
		t.Fatalf("truncated post-hook output = %q", note)
	}
}
