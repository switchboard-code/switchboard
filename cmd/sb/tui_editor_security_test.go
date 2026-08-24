package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
)

func replaceEditorDraft(t *testing.T, draft *editorDraft, content []byte) {
	t.Helper()
	replacement, err := fileprivacy.Create(filepath.Join(draft.dir, "replacement"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := replacement.Write(content); err != nil {
		_ = replacement.Close()
		t.Fatal(err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(draft.dir, "replacement"), draft.path()); err != nil {
		t.Fatal(err)
	}
}

func TestExternalEditorAcceptsPrivateAtomicReplacement(t *testing.T) {
	draft, err := newEditorDraft("before")
	if err != nil {
		t.Fatal(err)
	}
	replaceEditorDraft(t, draft, []byte("after\n"))
	msg := finishEditorDraft(draft, nil)
	if msg.err != nil || msg.content != "after" {
		t.Fatalf("atomic editor result = %q, %v", msg.content, msg.err)
	}
	if _, err := os.Lstat(draft.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("editor directory survived cleanup: %v", err)
	}
}

func TestExternalEditorRefusesOversizeAndInvalidUTF8(t *testing.T) {
	for _, test := range []struct {
		name string
		body []byte
		want string
	}{
		{name: "oversize", body: bytes.Repeat([]byte{'x'}, int(maxEditorPromptBytes)+1), want: "limit"},
		{name: "invalid UTF-8", body: []byte{'o', 'k', 0xff}, want: "UTF-8"},
	} {
		t.Run(test.name, func(t *testing.T) {
			draft, err := newEditorDraft("before")
			if err != nil {
				t.Fatal(err)
			}
			replaceEditorDraft(t, draft, test.body)
			msg := finishEditorDraft(draft, nil)
			if msg.err == nil || !strings.Contains(msg.err.Error(), test.want) || msg.content != "" {
				t.Fatalf("unsafe editor result = %q, %v", msg.content, msg.err)
			}
		})
	}
}

func TestExternalEditorRefusesHardlink(t *testing.T) {
	draft, err := newEditorDraft("before")
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(draft.dir, "second-link")
	if err := os.Link(draft.path(), link); err != nil {
		_ = draft.cleanup()
		t.Skipf("hard links unavailable: %v", err)
	}
	msg := finishEditorDraft(draft, nil)
	refusal := ""
	if msg.err != nil {
		refusal = msg.err.Error()
	}
	// Unix reports the policy-level single-link refusal; Windows' handle-level
	// verifier reports the observed hard-link count. Both prove the same
	// fail-closed decision, and neither platform may return the prompt.
	if msg.err == nil || (!strings.Contains(refusal, "single-link") && !strings.Contains(refusal, "hard links")) || msg.content != "" {
		t.Fatalf("hardlinked editor result = %q, %v", msg.content, msg.err)
	}
	_ = os.Remove(link)
	_ = os.Remove(draft.dir)
}
