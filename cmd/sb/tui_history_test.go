package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func setHistoryHome(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
}

func TestHistoryRoundTripsThroughDisk(t *testing.T) {
	home := t.TempDir()
	setHistoryHome(t, home)

	ws := filepath.Join(home, "project")
	appendHistory(ws, "first prompt")
	appendHistory(ws, "second\nprompt with a newline")
	appendHistory(ws, `literal \\n stays literal\tand tabs stay tabs`)

	got := loadHistory(ws)
	if len(got) != 3 {
		t.Fatalf("loaded %d entries, want 3", len(got))
	}
	if got[1] != "second\nprompt with a newline" {
		t.Fatalf("a multiline prompt did not survive the disk: %q", got[1])
	}
	if got[2] != `literal \\n stays literal\tand tabs stay tabs` {
		t.Fatalf("backslashes or tabs did not survive the disk: %q", got[2])
	}

	other := loadHistory(filepath.Join(home, "elsewhere"))
	if len(other) != 0 {
		t.Fatal("another workspace sees this one's history")
	}

	info, err := os.Stat(filepath.Join(home, ".switchboard", "history"))
	if err != nil || !info.IsDir() {
		t.Fatalf("history directory missing: %v", err)
	}
}

func TestHistoryNeverStoresRecognizedCredentials(t *testing.T) {
	home := t.TempDir()
	setHistoryHome(t, home)
	ws := filepath.Join(home, "project")

	appendHistory(ws, "use "+testGitHubToken)
	path, err := historyPath(ws)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), testGitHubToken) {
		t.Fatal("prompt history stored a raw credential")
	}
	got := loadHistory(ws)
	if len(got) != 1 || strings.Contains(got[0], testGitHubToken) || !strings.Contains(got[0], "[redacted: a GitHub token]") {
		t.Fatalf("history did not keep the safe spelling: %#v", got)
	}
}

func TestLoadingLegacyHistoryScrubsCredentials(t *testing.T) {
	home := t.TempDir()
	setHistoryHome(t, home)
	ws := filepath.Join(home, "project")
	path, err := historyPath(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old "+testGitHubToken+"\\nsecond line\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := loadHistory(ws)
	if len(got) != 1 || strings.Contains(got[0], testGitHubToken) {
		t.Fatalf("legacy history was not scrubbed: %#v", got)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), testGitHubToken) {
		t.Fatal("legacy history kept the credential on disk after load")
	}
	if ownerOnly, err := historyPathIsOwnerOnly(path); err != nil || !ownerOnly {
		t.Fatalf("legacy history ACL is owner-only = %v, err = %v; want true", ownerOnly, err)
	}
}

func TestHistoryRefusesSymlinkAndLeavesTargetUntouched(t *testing.T) {
	home := t.TempDir()
	setHistoryHome(t, home)
	ws := filepath.Join(home, "project")
	path, err := historyPath(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(home, "victim.txt")
	const sentinel = "do not append or replace\n"
	if err := os.WriteFile(victim, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if got := loadHistory(ws); len(got) != 0 {
		t.Fatalf("symlink history loaded entries: %#v", got)
	}
	appendHistory(ws, "must not reach the symlink target")
	raw, err := os.ReadFile(victim)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != sentinel {
		t.Fatalf("history symlink changed its target: %q", raw)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("history symlink was replaced: info=%v err=%v", info, err)
	}
}

func TestHistoryRefusesSymlinkedLockFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.hist")
	victim := filepath.Join(dir, "victim.txt")
	const sentinel = "lock target must stay untouched\n"
	if err := os.WriteFile(victim, []byte(sentinel), 0o644); err != nil {
		t.Fatal(err)
	}
	victimInfo, err := os.Stat(victim)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, path+".lock"); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := appendHistoryPrompt(path, "must not create history", nil); err == nil {
		t.Fatal("history append accepted a symlinked lock")
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("history was created despite symlinked lock: %v", err)
	}
	raw, err := os.ReadFile(victim)
	if err != nil || string(raw) != sentinel {
		t.Fatalf("symlinked lock target = %q, err=%v", raw, err)
	}
	if info, err := os.Stat(victim); err != nil || info.Mode().Perm() != victimInfo.Mode().Perm() {
		t.Fatalf("symlinked lock target mode changed: info=%v err=%v", info, err)
	}
}

func TestHistoryRefusesNonRegularFile(t *testing.T) {
	home := t.TempDir()
	setHistoryHome(t, home)
	ws := filepath.Join(home, "project")
	path, err := historyPath(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}

	if got := loadHistory(ws); len(got) != 0 {
		t.Fatalf("directory history loaded entries: %#v", got)
	}
	appendHistory(ws, "must not replace a directory")
	if info, err := os.Lstat(path); err != nil || !info.IsDir() {
		t.Fatalf("non-regular history was replaced: info=%v err=%v", info, err)
	}
}

func TestHistoryReadRefusesPathReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.hist")
	const original = `"original"` + "\n"
	const replacement = `"replacement"` + "\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	retired := path + ".retired"
	var hookErr error
	_, _, err := readHistoryFile(path, func() {
		if hookErr = os.Rename(path, retired); hookErr == nil {
			hookErr = os.WriteFile(path, []byte(replacement), 0o600)
		}
	})
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err == nil {
		t.Fatal("stable history read accepted a pathname replacement")
	}
	for file, want := range map[string]string{retired: original, path: replacement} {
		raw, readErr := os.ReadFile(file)
		if readErr != nil || string(raw) != want {
			t.Fatalf("%s = %q, err=%v; want %q", file, raw, readErr, want)
		}
	}
}

func TestHistoryAppendRefusesPathReplacement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.hist")
	const original = `"original"` + "\n"
	const replacement = `"replacement"` + "\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	retired := path + ".retired"
	var hookErr error
	err := appendHistoryPrompt(path, "must not be written", func() {
		if hookErr = os.Rename(path, retired); hookErr == nil {
			hookErr = os.WriteFile(path, []byte(replacement), 0o600)
		}
	})
	if hookErr != nil {
		t.Fatal(hookErr)
	}
	if err == nil {
		t.Fatal("history append accepted a pathname replacement")
	}
	for file, want := range map[string]string{retired: original, path: replacement} {
		raw, readErr := os.ReadFile(file)
		if readErr != nil || string(raw) != want {
			t.Fatalf("%s = %q, err=%v; want %q", file, raw, readErr, want)
		}
	}
}

func TestHistoryRewriteRefusesReplacementAndConcurrentAppend(t *testing.T) {
	t.Run("replacement", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "history.hist")
		const original = "legacy prompt\n"
		const replacement = `"replacement"` + "\n"
		if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
			t.Fatal(err)
		}
		_, snapshot, err := readHistoryFile(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		retired := path + ".retired"
		var hookErr error
		err = rewriteHistoryIfUnchanged(path, []string{"safe"}, snapshot, func() {
			if hookErr = os.Rename(path, retired); hookErr == nil {
				hookErr = os.WriteFile(path, []byte(replacement), 0o600)
			}
		})
		if hookErr != nil {
			t.Fatal(hookErr)
		}
		if err == nil {
			t.Fatal("history rewrite replaced a concurrently substituted file")
		}
		for file, want := range map[string]string{retired: original, path: replacement} {
			raw, readErr := os.ReadFile(file)
			if readErr != nil || string(raw) != want {
				t.Fatalf("%s = %q, err=%v; want %q", file, raw, readErr, want)
			}
		}
	})

	t.Run("append", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "history.hist")
		if err := os.WriteFile(path, []byte("legacy prompt\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, snapshot, err := readHistoryFile(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := appendHistoryPrompt(path, "new concurrent prompt", nil); err != nil {
			t.Fatal(err)
		}
		if err := rewriteHistoryIfUnchanged(path, []string{"stale rewrite"}, snapshot, nil); err == nil {
			t.Fatal("stale rewrite erased a concurrent append")
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "legacy prompt") || !strings.Contains(string(raw), "new concurrent prompt") ||
			strings.Contains(string(raw), "stale rewrite") {
			t.Fatalf("concurrent append was not preserved: %q", raw)
		}
	})
}

func TestConcurrentHistoryWriterIsRefusedWithoutInterleaving(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.hist")
	entered := make(chan struct{})
	release := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- appendHistoryPrompt(path, "first writer", func() {
			close(entered)
			<-release
		})
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first history writer did not reach the locked append seam")
	}
	if err := appendHistoryPrompt(path, "second writer", nil); !errors.Is(err, errHistoryBusy) {
		t.Fatalf("concurrent history writer error = %v, want %v", err, errHistoryBusy)
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `"first writer"`+"\n" {
		t.Fatalf("concurrent history output interleaved or admitted the refused writer: %q", raw)
	}
}

func TestHistoryAppendNarrowsModeAndBoundsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.hist")
	if err := os.WriteFile(path, []byte(`"old"`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := appendHistoryPrompt(path, "new", nil); err != nil {
		t.Fatal(err)
	}
	if ownerOnly, err := historyPathIsOwnerOnly(path); err != nil || !ownerOnly {
		t.Fatalf("appended history ACL is owner-only = %v, err=%v; want true", ownerOnly, err)
	}

	oversized := filepath.Join(dir, "oversized.hist")
	f, err := os.OpenFile(oversized, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(historyMaxBytes + 1); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readHistoryFile(oversized, nil); err == nil {
		t.Fatal("oversized history was read")
	}
	if err := appendHistoryPrompt(oversized, "must not grow", nil); err == nil {
		t.Fatal("oversized history accepted an append")
	}
	if info, err := os.Stat(oversized); err != nil || info.Size() != historyMaxBytes+1 {
		t.Fatalf("oversized history changed: info=%v err=%v", info, err)
	}
}

func historyPathIsOwnerOnly(path string) (ownerOnly bool, resultErr error) {
	f, err := openHistoryDataDescriptor(path, false, false)
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
	return historyFileIsOwnerOnly(f)
}

func TestHistoryLoadNarrowsWellFormedFileToOwnerOnly(t *testing.T) {
	home := t.TempDir()
	setHistoryHome(t, home)
	ws := filepath.Join(home, "project")
	path, err := historyPath(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`"already JSON"`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadHistory(ws); len(got) != 1 || got[0] != "already JSON" {
		t.Fatalf("well-formed history = %#v", got)
	}
	if ownerOnly, err := historyPathIsOwnerOnly(path); err != nil || !ownerOnly {
		t.Fatalf("loaded history ACL is owner-only = %v, err=%v; want true", ownerOnly, err)
	}
}

func TestReverseSearchFindsNewestFirst(t *testing.T) {
	m := testModel(t)
	m.history = []string{"fix the parser", "run the tests", "fix the linter"}

	m.startHistorySearch()
	for _, r := range "fix" {
		m.historySearchKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if hit := m.historyMatch(m.histMatch); hit != "fix the linter" {
		t.Fatalf("first match should be the newest, got %q", hit)
	}

	m.historySearchKey(tea.KeyMsg{Type: tea.KeyCtrlR})
	if hit := m.historyMatch(m.histMatch); hit != "fix the parser" {
		t.Fatalf("ctrl+r should step to the next older match, got %q", hit)
	}

	m.historySearchKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.histSearch {
		t.Fatal("enter should end the search")
	}
	if got := m.ta.Value(); got != "fix the parser" {
		t.Fatalf("enter should accept the match into the input, got %q", got)
	}
}

func TestReverseSearchEscapeLeavesInputAlone(t *testing.T) {
	m := testModel(t)
	m.history = []string{"something"}
	m.ta.SetValue("draft in progress")

	m.startHistorySearch()
	m.historySearchKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("some")})
	m.historySearchKey(tea.KeyMsg{Type: tea.KeyEsc})

	if m.ta.Value() != "draft in progress" {
		t.Fatalf("escape clobbered the draft: %q", m.ta.Value())
	}
}

func TestHistoryTraversalRestoresLiveDraft(t *testing.T) {
	for _, draft := range []string{"unfinished thought", ""} {
		t.Run(draft, func(t *testing.T) {
			m := testModel(t)
			m.history = []string{"oldest", "newest"}
			m.resetHistoryNavigation()
			m.ta.SetValue(draft)

			m.historyMove(-1)
			if got := m.ta.Value(); got != "newest" {
				t.Fatalf("up recalled %q, want newest", got)
			}
			m.historyMove(-1)
			if got := m.ta.Value(); got != "oldest" {
				t.Fatalf("second up recalled %q, want oldest", got)
			}
			m.historyMove(1)
			m.historyMove(1)
			if got := m.ta.Value(); got != draft {
				t.Fatalf("down past newest restored %q, want live draft %q", got, draft)
			}
			if m.histIdx != len(m.history) || m.histDraftSet {
				t.Fatalf("restored draft left traversal active: index=%d saved=%v", m.histIdx, m.histDraftSet)
			}
		})
	}
}

func TestEditingRecalledHistoryMakesItTheLiveDraft(t *testing.T) {
	m := testModel(t)
	m.history = []string{"old prompt"}
	m.resetHistoryNavigation()
	m.ta.SetValue("original draft")
	m.historyMove(-1)
	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("!")})
	if got := m.ta.Value(); got != "old prompt!" {
		t.Fatalf("edited recalled prompt = %q", got)
	}
	if m.histIdx != len(m.history) || m.histDraftSet {
		t.Fatalf("editing did not leave traversal: index=%d saved=%v", m.histIdx, m.histDraftSet)
	}

	m.historyMove(-1)
	m.historyMove(1)
	if got := m.ta.Value(); got != "old prompt!" {
		t.Fatalf("new traversal restored stale pre-history draft: %q", got)
	}
}

func TestHistoryTraversalOwnsArrowsAcrossRecalledPopupsAndMultiline(t *testing.T) {
	for _, recalled := range []string{"/help", "review @design", "first line\nsecond line"} {
		t.Run(recalled, func(t *testing.T) {
			m := testModel(t)
			m.history = []string{recalled}
			m.resetHistoryNavigation()
			m.ta.SetValue("live draft")

			m.key(tea.KeyMsg{Type: tea.KeyUp})
			if got := m.ta.Value(); got != recalled {
				t.Fatalf("up recalled %q, want %q", got, recalled)
			}
			m.key(tea.KeyMsg{Type: tea.KeyDown})
			if got := m.ta.Value(); got != "live draft" {
				t.Fatalf("down restored %q after recalled entry, want live draft", got)
			}
		})
	}
}

func TestExternalEditorPromotesRecalledHistoryToLiveDraft(t *testing.T) {
	m := testModel(t)
	m.history = []string{"old prompt"}
	m.resetHistoryNavigation()
	m.ta.SetValue("draft before history")
	m.historyMove(-1)

	draft, err := newEditorDraft("edited recalled prompt\n")
	if err != nil {
		t.Fatal(err)
	}
	m.onEditorDone(finishEditorDraft(draft, nil))
	if got := m.ta.Value(); got != "edited recalled prompt" {
		t.Fatalf("edited prompt = %q", got)
	}
	if m.histIdx != len(m.history) || m.histDraftSet {
		t.Fatalf("editor did not leave traversal: index=%d saved=%v", m.histIdx, m.histDraftSet)
	}

	m.historyMove(-1)
	m.historyMove(1)
	if got := m.ta.Value(); got != "edited recalled prompt" {
		t.Fatalf("new traversal restored stale pre-editor draft: %q", got)
	}
}
