//go:build unix

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"golang.org/x/sys/unix"
)

func planWorkspaceTool(t *testing.T, r *Registry, name string, input map[string]any) Plan {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	tool, ok := r.Get(name)
	if !ok {
		t.Fatalf("tool %q is not registered", name)
	}
	plan, err := tool.Plan(raw)
	if err != nil {
		t.Fatalf("planning %s: %v", name, err)
	}
	return plan
}

func replaceWorkspaceParentWithExternalSymlink(t *testing.T, live, displaced, outside string) {
	t.Helper()
	if err := os.Rename(live, displaced); err != nil {
		t.Fatalf("renaming workspace parent: %v", err)
	}
	if err := os.Symlink(outside, live); err != nil {
		t.Fatalf("installing external parent symlink: %v", err)
	}
}

func assertNoMutationTemporary(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".switchboard-write-") {
			t.Fatalf("mutation temporary leaked in %s: %s", dir, entry.Name())
		}
	}
}

func TestPlannedWorkspaceReadsRejectRenamedParentExternalSymlink(t *testing.T) {
	tests := []struct {
		name    string
		tool    string
		input   map[string]any
		inside  map[string]string
		outside map[string]string
		leak    string
	}{
		{
			name:    "read",
			tool:    "read",
			input:   map[string]any{"path": "nested/target.txt"},
			inside:  map[string]string{"target.txt": "workspace bytes"},
			outside: map[string]string{"target.txt": "external read payload 6c5bf6d1"},
			leak:    "external read payload 6c5bf6d1",
		},
		{
			name:    "grep",
			tool:    "grep",
			input:   map[string]any{"path": "nested", "pattern": "external-match"},
			inside:  map[string]string{"source.txt": "ordinary workspace bytes"},
			outside: map[string]string{"source.txt": "external-match payload-83b82c6e"},
			leak:    "payload-83b82c6e",
		},
		{
			name:    "glob",
			tool:    "glob",
			input:   map[string]any{"path": "nested", "pattern": "*.leak"},
			inside:  map[string]string{"ordinary.txt": "workspace"},
			outside: map[string]string{"external-0c4779d8.leak": "outside"},
			leak:    "external-0c4779d8.leak",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, root := newRegistry(t)
			live := filepath.Join(root, "nested")
			displaced := filepath.Join(root, "nested-before-swap")
			outside := t.TempDir()
			for name, content := range tt.inside {
				writeFile(t, filepath.Join(live, name), content)
			}
			for name, content := range tt.outside {
				writeFile(t, filepath.Join(outside, name), content)
			}

			plan := planWorkspaceTool(t, r, tt.tool, tt.input)
			replaceWorkspaceParentWithExternalSymlink(t, live, displaced, outside)

			result, runErr := plan.Run(context.Background())
			if runErr == nil && !result.IsError {
				t.Fatalf("%s accepted a parent swapped to an external symlink: %+v", tt.tool, result)
			}
			combined := result.Content
			if runErr != nil {
				combined += runErr.Error()
			}
			if strings.Contains(combined, tt.leak) {
				t.Fatalf("%s exposed external data after the parent swap: %q", tt.tool, combined)
			}
		})
	}
}

func TestPlannedMutationsRejectRenamedParentExternalSymlink(t *testing.T) {
	for _, tt := range []struct {
		name  string
		tool  string
		input map[string]any
	}{
		{
			name: "write",
			tool: "write",
			input: map[string]any{
				"path": "nested/target.txt", "content": "agent write",
			},
		},
		{
			name: "edit",
			tool: "edit",
			input: map[string]any{
				"path": "nested/target.txt", "old_string": "workspace source", "new_string": "agent edit",
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, root := newRegistry(t)
			recorder := newCheckpointRecorder(t, root)
			r.SetCheckpoints(recorder)
			live := filepath.Join(root, "nested")
			displaced := filepath.Join(root, "nested-before-swap")
			outside := t.TempDir()
			insideTarget := filepath.Join(live, "target.txt")
			outsideTarget := filepath.Join(outside, "target.txt")
			writeFile(t, insideTarget, "workspace source")
			writeFile(t, outsideTarget, "external source")
			if result := run(t, r, "read", map[string]any{"path": "nested/target.txt"}); result.IsError {
				t.Fatalf("arming read failed: %s", result.Content)
			}
			plan := planWorkspaceTool(t, r, tt.tool, tt.input)
			recorder.Begin(tt.name)
			replaceWorkspaceParentWithExternalSymlink(t, live, displaced, outside)

			result, runErr := plan.Run(context.Background())
			if runErr == nil && !result.IsError {
				t.Fatalf("%s accepted a parent swapped to an external symlink: %+v", tt.tool, result)
			}
			outsideBytes, err := os.ReadFile(outsideTarget)
			if err != nil || string(outsideBytes) != "external source" {
				t.Fatalf("outside target was mutated: bytes=%q err=%v", outsideBytes, err)
			}
			originalBytes, err := os.ReadFile(filepath.Join(displaced, "target.txt"))
			if err != nil || string(originalBytes) != "workspace source" {
				t.Fatalf("displaced workspace target was mutated: bytes=%q err=%v", originalBytes, err)
			}
			if turns := recorder.Turns(); len(turns) != 0 {
				t.Fatalf("refused %s retained a rollback checkpoint: %+v", tt.tool, turns)
			}
			assertNoMutationTemporary(t, displaced)
			assertNoMutationTemporary(t, outside)
		})
	}
}

func TestMutationPublicationRejectsRenamedParentExternalSymlink(t *testing.T) {
	// Write and edit share this publisher; exercise each call shape so neither
	// can regress to a pathname-based precommit check independently. The seam
	// moves the already-bound parent after the replacement is prepared; the
	// checkpoint publisher's rollback-capable linked-parent validation is the
	// CAS boundary, so a refusal must restore the preimage in that moved parent.
	for _, tt := range []struct {
		name    string
		content string
	}{
		{name: "write publication", content: "whole-file replacement"},
		{name: "edit publication", content: "workspace replacement"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, root := newRegistry(t)
			recorder := newCheckpointRecorder(t, root)
			r.SetCheckpoints(recorder)
			live := filepath.Join(root, "nested")
			displaced := filepath.Join(root, "nested-before-swap")
			outside := t.TempDir()
			insideTarget := filepath.Join(live, "target.txt")
			outsideTarget := filepath.Join(outside, "target.txt")
			writeFile(t, insideTarget, "workspace source")
			writeFile(t, outsideTarget, "external source")
			if result := run(t, r, "read", map[string]any{"path": "nested/target.txt"}); result.IsError {
				t.Fatalf("arming read failed: %s", result.Content)
			}

			tx, result, ok := r.prepareFileMutation(insideTarget, false)
			if !ok {
				t.Fatalf("preparing mutation: %s", result.Content)
			}
			defer tx.close()
			recorder.Begin(tt.name)
			err := tx.publish(context.Background(), []byte(tt.content), tx.before.mode, func() {
				replaceWorkspaceParentWithExternalSymlink(t, live, displaced, outside)
			})
			if err == nil || !errors.Is(err, checkpoint.ErrStale) {
				t.Fatalf("publication error = %v, want changed-parent refusal", err)
			}

			outsideBytes, readErr := os.ReadFile(outsideTarget)
			if readErr != nil || string(outsideBytes) != "external source" {
				t.Fatalf("outside target was mutated: bytes=%q err=%v", outsideBytes, readErr)
			}
			originalBytes, readErr := os.ReadFile(filepath.Join(displaced, "target.txt"))
			if readErr != nil || string(originalBytes) != "workspace source" {
				t.Fatalf("displaced workspace target was mutated: bytes=%q err=%v", originalBytes, readErr)
			}
			if turns := recorder.Turns(); len(turns) != 0 {
				t.Fatalf("refused publication retained a rollback checkpoint: %+v", turns)
			}
			assertNoMutationTemporary(t, displaced)
			assertNoMutationTemporary(t, outside)
		})
	}
}

func TestWorkspaceCapabilityRejectsRootPathReplacement(t *testing.T) {
	r, root := newRegistry(t)
	writeFile(t, filepath.Join(root, "target.txt"), "workspace")
	plan := planWorkspaceTool(t, r, "read", map[string]any{"path": "target.txt"})
	displaced := root + "-before-swap"
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "target.txt"), "external root payload")
	replaceWorkspaceParentWithExternalSymlink(t, root, displaced, outside)

	result, runErr := plan.Run(context.Background())
	if runErr == nil && !result.IsError {
		t.Fatalf("read accepted a replaced workspace root: %+v", result)
	}
	if strings.Contains(fmt.Sprint(result.Content, runErr), "external root payload") {
		t.Fatal("read exposed data from a replacement workspace root")
	}
}

func TestWorkspaceReadDiscardsBytesWhenRootMovesAfterOpen(t *testing.T) {
	r, root := newRegistry(t)
	const secret = "root-move-secret-7f6402a9"
	writeFile(t, filepath.Join(root, "target.txt"), secret)
	abs, err := r.resolve("target.txt")
	if err != nil {
		t.Fatal(err)
	}
	moved := root + "-moved"
	result, runErr := (&readTool{r: r}).readWithHook(abs, readInput{}, func() {
		if err := os.Rename(root, moved); err != nil {
			t.Fatalf("moving opened workspace root: %v", err)
		}
	})
	if runErr != nil {
		t.Fatal(runErr)
	}
	if !result.IsError || !strings.Contains(result.Content, "workspace changed") {
		t.Fatalf("moved-root read = %+v", result)
	}
	if strings.Contains(result.Content, secret) {
		t.Fatalf("moved-root read exposed bytes: %q", result.Content)
	}
}

func TestWorkspaceCapabilityBindingDoesNotBlockOnRootFIFOReplacement(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	moved := root + "-moved"
	var swapErr error
	workspaceRootBeforeOpenTestHook = func(path string) {
		if path != root {
			return
		}
		swapErr = os.Rename(root, moved)
		if swapErr == nil {
			swapErr = unix.Mkfifo(root, 0o600)
		}
	}
	t.Cleanup(func() { workspaceRootBeforeOpenTestHook = nil })
	done := make(chan error, 1)
	go func() {
		_, err := bindWorkspaceRootIdentity(root)
		done <- err
	}()
	select {
	case err := <-done:
		if swapErr != nil {
			t.Fatal(swapErr)
		}
		if err == nil {
			t.Fatal("workspace capability accepted a FIFO root replacement")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("workspace capability blocked on a FIFO root replacement")
	}
}
