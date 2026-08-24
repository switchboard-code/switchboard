package session

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func pathReaderChecks(path string) map[string]func() error {
	return map[string]func() error{
		"PublicationStatus":    func() error { _, err := PublicationStatus(path); return err },
		"ReadState":            func() error { _, err := ReadState(path); return err },
		"ReadRaces":            func() error { _, err := ReadRaces(path); return err },
		"ReadPermissions":      func() error { _, err := ReadPermissions(path); return err },
		"ReadTimeline":         func() error { _, err := ReadTimeline(path); return err },
		"ReadUsages":           func() error { _, err := ReadUsages(path); return err },
		"ReadWorkspace":        func() error { _, err := ReadWorkspace(path); return err },
		"ReadOpening":          func() error { _, err := ReadOpening(path); return err },
		"ReadOpeningSummary":   func() error { _, err := ReadOpeningSummary(path); return err },
		"ReadFileEdits":        func() error { _, err := ReadFileEdits(path); return err },
		"ReadTurnCosts":        func() error { _, err := ReadTurnCosts(path); return err },
		"ReadAccountingLedger": func() error { _, err := ReadAccountingLedger(path); return err },
	}
}

func TestPublicationMarkerSymlinkAndHardLinkAreRejected(t *testing.T) {
	store, workspace := newStore(t)
	staged, err := store.CreateStaged(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := staged.Publish(); err != nil {
		t.Fatal(err)
	}
	id, path := staged.ID(), staged.Path()
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
	markerPath := publicationMarkerPath(path)
	original, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(t.TempDir(), "outside.published")
	if err := os.Rename(markerPath, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, markerPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if published, err := PublicationStatus(path); err == nil || published {
		t.Fatalf("PublicationStatus followed marker symlink: published=%v err=%v", published, err)
	}
	if _, err := ReadState(path); err == nil {
		t.Fatal("ReadState accepted a symlinked publication marker")
	}
	if infos, err := store.List(workspace); err != nil || len(infos) != 0 {
		t.Fatalf("List admitted symlinked publication marker: %+v, %v", infos, err)
	}
	if opened, err := store.Open(id); err == nil {
		_ = opened.Close()
		t.Fatal("Open accepted a symlinked publication marker")
	}
	after, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("rejecting marker symlink modified its target")
	}

	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(outside, markerPath); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "marker-hardlink")
	if err := os.Link(markerPath, alias); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	if published, err := PublicationStatus(path); err == nil || published {
		t.Fatalf("PublicationStatus accepted hard-linked marker: published=%v err=%v", published, err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if published, err := PublicationStatus(path); err != nil || !published {
		t.Fatalf("single-link marker did not recover: published=%v err=%v", published, err)
	}
}

func TestPublicationMarkerReplacementAfterOpenIsRejected(t *testing.T) {
	store, workspace := newStore(t)
	staged, err := store.CreateStaged(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := staged.Publish(); err != nil {
		t.Fatal(err)
	}
	path := staged.Path()
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
	markerPath := publicationMarkerPath(path)
	original, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	moved := markerPath + ".opened"
	replacement := []byte("foreign replacement must survive\n")
	_, err = readPublicationMarkerWithHook(markerPath, func() {
		if renameErr := os.Rename(markerPath, moved); renameErr != nil {
			t.Fatal(renameErr)
		}
		if writeErr := os.WriteFile(markerPath, replacement, 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
	})
	if err == nil {
		t.Fatal("marker reader accepted a pathname replacement after open")
	}
	if got, readErr := os.ReadFile(markerPath); readErr != nil || !bytes.Equal(got, replacement) {
		t.Fatalf("marker replacement changed: %q, %v", got, readErr)
	}
	if got, readErr := os.ReadFile(moved); readErr != nil || !bytes.Equal(got, original) {
		t.Fatalf("opened marker changed: %q, %v", got, readErr)
	}
}

func closedSessionWithMessage(t *testing.T) (*Store, string, string, string, []byte) {
	t.Helper()
	store, workspace := newStore(t)
	sess, err := store.Create(workspace, "test/local/model", "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AppendMessage(provider.UserText("descriptor-stable opening")); err != nil {
		t.Fatal(err)
	}
	id, path := sess.ID(), sess.Path()
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return store, workspace, id, path, raw
}

func TestSessionLogSymlinkIsRejectedByDiscoveryOpenForkAndReaders(t *testing.T) {
	store, workspace, id, path, original := closedSessionWithMessage(t)
	outside := filepath.Join(t.TempDir(), "outside.log")
	if err := os.Rename(path, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	infos, err := store.List(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 0 {
		t.Fatalf("List admitted symlinked log: %+v", infos)
	}
	all, err := store.ListAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all[workspace]) != 0 {
		t.Fatalf("ListAll admitted symlinked log: %+v", all[workspace])
	}
	if opened, err := store.Open(id); err == nil {
		_ = opened.Close()
		t.Fatal("Open accepted a symlinked log")
	}
	if fork, err := store.Fork(id, 1); err == nil {
		_ = fork.Close()
		t.Fatal("Fork accepted a symlinked log")
	}
	for name, read := range pathReaderChecks(path) {
		t.Run(name, func(t *testing.T) {
			if err := read(); err == nil {
				t.Fatal("reader accepted a symlinked log")
			}
		})
	}

	after, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("rejecting the symlink modified its outside target")
	}
}

func TestSessionLogSymlinkedParentIsRejected(t *testing.T) {
	store, workspace, id, path, original := closedSessionWithMessage(t)
	parent := filepath.Dir(path)
	movedParent := filepath.Join(t.TempDir(), "outside-session-directory")
	if err := os.Rename(parent, movedParent); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(movedParent, parent); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if infos, err := store.List(workspace); err != nil || len(infos) != 0 {
		t.Fatalf("List through symlinked parent = %+v, %v", infos, err)
	}
	if opened, err := store.Open(id); err == nil {
		_ = opened.Close()
		t.Fatal("Open followed a symlinked session directory")
	}
	if _, err := ReadState(path); err == nil {
		t.Fatal("ReadState followed a symlinked session directory")
	}
	after, err := os.ReadFile(filepath.Join(movedParent, filepath.Base(path)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("rejecting the symlinked parent modified its outside log")
	}
}

func TestSessionLogHardLinkIsRejectedWithoutMutation(t *testing.T) {
	store, workspace, id, path, original := closedSessionWithMessage(t)
	alias := filepath.Join(t.TempDir(), "alias.log")
	if err := os.Link(path, alias); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}

	if infos, err := store.List(workspace); err != nil || len(infos) != 0 {
		t.Fatalf("List with aliased log = %+v, %v", infos, err)
	}
	if opened, err := store.Open(id); err == nil {
		_ = opened.Close()
		t.Fatal("Open accepted a hard-linked log")
	}
	if _, err := ReadState(path); err == nil || !strings.Contains(err.Error(), "hard links") {
		t.Fatalf("ReadState hard-link error = %v", err)
	}
	after, err := os.ReadFile(alias)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("rejecting a hard link changed the aliased bytes")
	}

	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if state, err := ReadState(path); err != nil || state.ID != id {
		t.Fatalf("ordinary single-link log did not recover: state=%+v err=%v", state, err)
	}
}

func TestSessionLogDirectoryIsRejected(t *testing.T) {
	_, _, _, path, _ := closedSessionWithMessage(t)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadState(path); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory error = %v", err)
	}
}

func TestPublishedLogDescriptorStaysPinnedAfterPathReplacement(t *testing.T) {
	_, _, _, path, original := closedSessionWithMessage(t)
	f, err := openPublishedLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	moved := path + ".moved"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	replacement := []byte("attacker-controlled replacement\n")
	if err := os.WriteFile(path, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("opened descriptor switched to replacement: got %q", got)
	}
	if err := verifyCurrentSessionLogPath(f, path); err == nil {
		t.Fatal("path recheck accepted a replacement inode")
	}
}

func TestPathReadersRejectValidLogSubstitutedAtAnotherSessionPath(t *testing.T) {
	store, workspace := newStore(t)
	create := func(opening string) (string, string) {
		sess, err := store.Create(workspace, "test/local/model", "rev")
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.AppendMessage(provider.UserText(opening)); err != nil {
			t.Fatal(err)
		}
		id, path := sess.ID(), sess.Path()
		if err := sess.Close(); err != nil {
			t.Fatal(err)
		}
		return id, path
	}

	selectedID, selectedPath := create("selected session")
	substituteID, substitutePath := create("substituted session")
	if err := os.Rename(selectedPath, selectedPath+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(substitutePath, selectedPath); err != nil {
		t.Fatal(err)
	}

	for name, read := range pathReaderChecks(selectedPath) {
		t.Run(name, func(t *testing.T) {
			if err := read(); err == nil || !strings.Contains(err.Error(), "does not match session_start identity") {
				t.Fatalf("reader accepted session %s at selected path for %s: %v", substituteID, selectedID, err)
			}
		})
	}
}

func TestPathReadersRejectSessionStartWorkspaceThatDoesNotOwnParent(t *testing.T) {
	_, workspace, id, path, _ := closedSessionWithMessage(t)
	foreignWorkspace := t.TempDir()
	writeCandidate(t, path,
		candidateStart(t, 1, id, foreignWorkspace),
		candidateRecord(t, 2, RecordMessage, provider.UserText("foreign workspace")),
	)

	for name, read := range pathReaderChecks(path) {
		t.Run(name, func(t *testing.T) {
			if err := read(); err == nil || !strings.Contains(err.Error(), "does not match session_start identity") {
				t.Fatalf("reader accepted workspace %s at parent for %s: %v", foreignWorkspace, workspace, err)
			}
		})
	}
}

func TestForkRechecksPublicationAfterCandidateResolution(t *testing.T) {
	store, workspace, id, path, original := closedSessionWithMessage(t)
	candidate, err := store.resolveCandidate(id)
	if err != nil {
		t.Fatal(err)
	}
	moved := path + ".resolved"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}

	publicationID := strings.Repeat("a", publicationIDBytes*2)
	start := candidateRecord(t, 1, RecordSessionStart, SessionStart{
		ID: id, Workspace: workspace, Target: "test/local/model", Binary: "test",
		Staged: true, PublicationID: publicationID,
	})
	message := candidateRecord(t, 2, RecordMessage, provider.UserText("hidden replacement"))
	var staged bytes.Buffer
	fmt.Fprintf(&staged, "%s %d\n", magic, SchemaVersion)
	staged.Write(start)
	staged.Write(message)
	if err := os.WriteFile(path, staged.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	fork, err := store.forkPathOnto(id, candidate.path, 1, "", false, nil, nil, true)
	if fork != nil {
		_ = fork.Close()
	}
	if !errors.Is(err, ErrSessionUnpublished) {
		t.Fatalf("fork replacement error = %v, want ErrSessionUnpublished", err)
	}
	if infos, listErr := store.List(workspace); listErr != nil || len(infos) != 0 {
		t.Fatalf("unpublished replacement or failed child became visible: %+v, %v", infos, listErr)
	}
	after, err := os.ReadFile(moved)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, original) {
		t.Fatal("failed fork modified the resolved source")
	}
}

func TestSessionHeaderAndRecordFramingAreBounded(t *testing.T) {
	header := strings.Repeat("x", maxSessionHeaderBytes+1)
	if err := checkHeader(bufio.NewReader(strings.NewReader(header)), "hostile.log"); !errors.Is(err, ErrSessionHeaderTooLarge) {
		t.Fatalf("oversize header error = %v, want ErrSessionHeaderTooLarge", err)
	}

	declared := fmt.Sprintf("%08x %08x ", maxSessionRecordBytes+1, 0)
	_, consumed, err := decodeRecord(bufio.NewReader(strings.NewReader(declared)))
	if !errors.Is(err, ErrCorruptRecord) || !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("oversize frame error = %v, want corrupt and too-large", err)
	}
	if consumed != frameHeaderLen {
		t.Fatalf("oversize frame consumed %d bytes, want fixed header %d", consumed, frameHeaderLen)
	}
}

func TestOversizeRecordBlocksResumeWithoutAllocatingOrMutating(t *testing.T) {
	store, workspace, id, path, _ := closedSessionWithMessage(t)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(f, "%08x %08x ", maxSessionRecordBytes+1, 0); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if opened, err := store.Open(id); !errors.Is(err, ErrRecordTooLarge) {
		if opened != nil {
			_ = opened.Close()
		}
		t.Fatalf("Open error = %v, want ErrRecordTooLarge", err)
	}
	if _, err := ReadState(path); !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("ReadState error = %v, want ErrRecordTooLarge", err)
	}
	infos, err := store.List(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].ID != id || !infos[0].Health.CorruptRecord {
		t.Fatalf("oversize preserved log inventory = %+v", infos)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("oversize-frame refusal modified or truncated the source")
	}
}
