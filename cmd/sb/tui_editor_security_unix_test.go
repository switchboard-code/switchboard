//go:build unix

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestExternalEditorFIFORefusesWithoutBlocking(t *testing.T) {
	for _, test := range []struct {
		name string
		swap bool
	}{
		{name: "direct"},
		{name: "regular to FIFO swap", swap: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			draft, err := newEditorDraft("before")
			if err != nil {
				t.Fatal(err)
			}
			var replaceErr error
			replace := func() {
				if err := os.Remove(draft.path()); err != nil {
					replaceErr = err
					return
				}
				replaceErr = unix.Mkfifo(draft.path(), 0o600)
			}
			if !test.swap {
				replace()
				if replaceErr != nil {
					t.Fatal(replaceErr)
				}
			}
			done := make(chan error, 1)
			go func() {
				var readErr error
				if test.swap {
					_, readErr = draft.readWithHook(replace)
				} else {
					_, readErr = draft.read()
				}
				done <- readErr
			}()
			select {
			case readErr := <-done:
				if replaceErr != nil {
					t.Fatal(replaceErr)
				}
				if readErr == nil {
					t.Fatal("FIFO editor prompt was accepted")
				}
			case <-time.After(time.Second):
				t.Fatal("editor prompt read blocked on a FIFO")
			}
			_ = draft.cleanup()
		})
	}
}

func TestExternalEditorDirectoryFIFOSwapCannotBlockPrivacyCheck(t *testing.T) {
	draft, err := newEditorDraft("inside")
	if err != nil {
		t.Fatal(err)
	}
	original := draft.dir + "-moved"
	var swapErr error
	swap := func() {
		if err := os.Rename(draft.dir, original); err != nil {
			swapErr = err
			return
		}
		swapErr = unix.Mkfifo(draft.dir, 0o600)
	}

	done := make(chan error, 1)
	go func() { done <- draft.verifyDirectoryWithHook(swap) }()
	select {
	case verifyErr := <-done:
		if swapErr != nil {
			t.Fatal(swapErr)
		}
		if verifyErr == nil || !strings.Contains(verifyErr.Error(), "changed") {
			t.Fatalf("directory FIFO swap error = %v, want identity refusal", verifyErr)
		}
	case <-time.After(time.Second):
		t.Fatal("external-editor directory privacy check blocked on a FIFO path swap")
	}

	_ = draft.cleanup()
	if err := os.Remove(draft.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(original, editorPromptName))
	_ = os.Remove(original)
}

func TestExternalEditorRefusesSymlinkAndDirectorySwap(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		draft, err := newEditorDraft("before")
		if err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(outside, []byte("outside secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(draft.path()); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, draft.path()); err != nil {
			t.Fatal(err)
		}
		msg := finishEditorDraft(draft, nil)
		if msg.err == nil || strings.Contains(msg.content, "outside secret") {
			t.Fatalf("symlink editor result = %q, %v", msg.content, msg.err)
		}
	})

	t.Run("private directory path replacement", func(t *testing.T) {
		draft, err := newEditorDraft("inside")
		if err != nil {
			t.Fatal(err)
		}
		original := draft.dir + "-moved"
		if err := os.Rename(draft.dir, original); err != nil {
			t.Fatal(err)
		}
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, editorPromptName), []byte("outside secret"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, draft.dir); err != nil {
			t.Fatal(err)
		}
		msg := finishEditorDraft(draft, nil)
		if msg.err == nil || strings.Contains(msg.content, "outside secret") {
			t.Fatalf("directory-swap editor result = %q, %v", msg.content, msg.err)
		}
		if _, err := os.Lstat(filepath.Join(original, editorPromptName)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("original private draft was not cleaned through its retained capability: %v", err)
		}
		if err := os.Remove(draft.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(original, editorPromptName)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		_ = os.Remove(original)
	})
}
