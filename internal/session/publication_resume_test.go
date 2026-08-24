package session

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/switchboard-code/switchboard/internal/provider"
)

func TestPublishedStagedOpenRequiresMarkerDurabilityBeforeEveryWritableEntry(t *testing.T) {
	store, workspace, id, logPath, markerPath := publishedStagedSession(t)
	logBefore := readTestFile(t, logPath)
	markerBefore := readTestFile(t, markerPath)
	injected := errors.New("injected resume marker sync failure")
	markerSyncs := 0
	directorySyncs := 0
	store.openPublicationMarkerSync = func(*os.File) error {
		markerSyncs++
		return injected
	}
	store.openPublicationDirectorySync = func(*os.File) error {
		directorySyncs++
		return nil
	}

	entries := []struct {
		name string
		open func() (*Session, error)
	}{
		{"Open", func() (*Session, error) { return store.Open(id) }},
		{"OpenInWorkspace", func() (*Session, error) { return store.OpenInWorkspace(id, workspace) }},
		{"Latest", func() (*Session, error) { return store.Latest(workspace) }},
	}
	for _, entry := range entries {
		t.Run(entry.name, func(t *testing.T) {
			opened, err := entry.open()
			if opened != nil {
				_ = opened.Close()
				t.Fatal("writable session escaped after its publication marker sync failed")
			}
			assertPublicationResumeRecoveryError(t, err, injected)
			if got := readTestFile(t, logPath); !bytes.Equal(got, logBefore) {
				t.Fatal("failed durability recovery mutated the session log")
			}
			if got := readTestFile(t, markerPath); !bytes.Equal(got, markerBefore) {
				t.Fatal("failed durability recovery mutated the publication marker")
			}
		})
	}
	if markerSyncs != len(entries) || directorySyncs != 0 {
		t.Fatalf("sync calls = marker %d, directory %d; want %d, 0", markerSyncs, directorySyncs, len(entries))
	}

	// A failed attempt releases the append lock and preserves everything needed
	// for the next process to retry the same ownerless durability barrier.
	store.openPublicationMarkerSync = nil
	store.openPublicationDirectorySync = nil
	opened, err := store.Open(id)
	if err != nil {
		t.Fatalf("stable restart could not recover published session: %v", err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPublishedStagedOpenRefusesDirectorySyncFailureWithoutMutation(t *testing.T) {
	store, _, id, logPath, markerPath := publishedStagedSession(t)
	logBefore := readTestFile(t, logPath)
	markerBefore := readTestFile(t, markerPath)
	injected := errors.New("injected resume directory sync failure")
	var order []string
	store.openPublicationMarkerSync = func(marker *os.File) error {
		order = append(order, "marker")
		return marker.Sync()
	}
	store.openPublicationDirectorySync = func(*os.File) error {
		order = append(order, "directory")
		return injected
	}

	opened, err := store.Open(id)
	if opened != nil {
		_ = opened.Close()
		t.Fatal("writable session escaped after its publication directory sync failed")
	}
	assertPublicationResumeRecoveryError(t, err, injected)
	if strings.Join(order, ",") != "marker,directory" {
		t.Fatalf("durability order = %q, want marker,directory", strings.Join(order, ","))
	}
	if got := readTestFile(t, logPath); !bytes.Equal(got, logBefore) {
		t.Fatal("directory-sync failure mutated the session log")
	}
	if got := readTestFile(t, markerPath); !bytes.Equal(got, markerBefore) {
		t.Fatal("directory-sync failure mutated the publication marker")
	}

	store.openPublicationMarkerSync = nil
	store.openPublicationDirectorySync = nil
	opened, err = store.Open(id)
	if err != nil {
		t.Fatalf("stable restart could not recover after directory-sync failure: %v", err)
	}
	_ = opened.Close()
}

func TestPublishedStagedOpenRejectsInPlaceMutationAfterMarkerSync(t *testing.T) {
	store, _, id, logPath, markerPath := publishedStagedSession(t)
	logBefore := readTestFile(t, logPath)
	markerBefore := readTestFile(t, markerPath)
	before, err := os.Stat(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	directorySyncs := 0
	store.openPublicationMarkerSync = func(marker *os.File) error {
		if err := marker.Sync(); err != nil {
			return err
		}
		return os.WriteFile(markerPath, markerBefore[:len(markerBefore)-1], 0o600)
	}
	store.openPublicationDirectorySync = func(*os.File) error {
		directorySyncs++
		return nil
	}

	opened, openErr := store.Open(id)
	if opened != nil {
		_ = opened.Close()
		t.Fatal("in-place marker mutation admitted a writable session")
	}
	assertPublicationResumeRecoveryError(t, openErr, nil)
	if directorySyncs != 0 {
		t.Fatalf("directory syncs after in-place marker mutation = %d, want 0", directorySyncs)
	}
	after, err := os.Stat(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("mutation seam replaced the marker instead of changing its opened inode")
	}
	if got := readTestFile(t, logPath); !bytes.Equal(got, logBefore) {
		t.Fatal("in-place marker mutation refusal changed the session log")
	}

	if err := os.WriteFile(markerPath, markerBefore, 0o600); err != nil {
		t.Fatal(err)
	}
	store.openPublicationMarkerSync = nil
	store.openPublicationDirectorySync = nil
	opened, err = store.Open(id)
	if err != nil {
		t.Fatalf("stable restored-marker restart could not recover: %v", err)
	}
	_ = opened.Close()
}

func TestPublishedStagedOpenRejectsExactInPlaceRewriteAfterMarkerSync(t *testing.T) {
	store, _, id, _, markerPath := publishedStagedSession(t)
	markerBytes := readTestFile(t, markerPath)
	before, err := os.Stat(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	directorySyncs := 0
	store.openPublicationMarkerSync = func(marker *os.File) error {
		if err := marker.Sync(); err != nil {
			return err
		}
		return os.WriteFile(markerPath, markerBytes, 0o600)
	}
	store.openPublicationDirectorySync = func(*os.File) error {
		directorySyncs++
		return nil
	}

	opened, openErr := store.Open(id)
	if opened != nil {
		_ = opened.Close()
		t.Fatal("exact same-inode rewrite admitted a writable session")
	}
	assertPublicationResumeRecoveryError(t, openErr, nil)
	if directorySyncs != 0 {
		t.Fatalf("directory syncs after exact marker rewrite = %d, want 0", directorySyncs)
	}
	after, err := os.Stat(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("exact rewrite seam replaced the marker inode")
	}
	if got := readTestFile(t, markerPath); !bytes.Equal(got, markerBytes) {
		t.Fatal("exact rewrite changed marker bytes")
	}

	store.openPublicationMarkerSync = nil
	store.openPublicationDirectorySync = nil
	opened, err = store.Open(id)
	if err != nil {
		t.Fatalf("stable exact-rewrite restart could not recover: %v", err)
	}
	_ = opened.Close()
}

func TestPublishedStagedOpenRejectsDirectoryEntryABAAfterDirectorySync(t *testing.T) {
	store, _, id, _, markerPath := publishedStagedSession(t)
	markerBytes := readTestFile(t, markerPath)
	store.openPublicationMarkerSync = func(marker *os.File) error { return marker.Sync() }
	store.openPublicationDirectorySync = func(directory *os.File) error {
		return publicationMarkerDirectoryABA(markerPath, markerBytes,
			func() error { return syncOpenedSessionDirectory(directory) })
	}

	opened, openErr := store.Open(id)
	if opened != nil {
		_ = opened.Close()
		t.Fatal("directory-entry ABA admitted a writable session")
	}
	assertPublicationResumeRecoveryError(t, openErr, nil)
	if got := readTestFile(t, markerPath); !bytes.Equal(got, markerBytes) {
		t.Fatal("directory ABA did not restore the original marker")
	}

	store.openPublicationMarkerSync = nil
	store.openPublicationDirectorySync = nil
	opened, err := store.Open(id)
	if err != nil {
		t.Fatalf("stable directory-ABA restart could not recover: %v", err)
	}
	_ = opened.Close()
}

func TestPublishedStagedOpenSyncFailurePrecedesTornTailRepair(t *testing.T) {
	store, _, id, logPath, _ := publishedStagedSession(t)
	raw := readTestFile(t, logPath)
	finalStart := bytes.LastIndexByte(raw[:len(raw)-1], '\n') + 1
	if finalStart <= 0 || finalStart >= len(raw) {
		t.Fatalf("could not locate final frame in %d-byte staged log", len(raw))
	}
	torn := append([]byte(nil), raw[:finalStart+(len(raw)-finalStart)/2]...)
	if err := os.WriteFile(logPath, torn, 0o600); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("marker sync fails before repair")
	store.openPublicationMarkerSync = func(*os.File) error { return injected }

	opened, err := store.Open(id)
	if opened != nil {
		_ = opened.Close()
		t.Fatal("writable session escaped after marker sync failed beside a torn tail")
	}
	assertPublicationResumeRecoveryError(t, err, injected)
	if got := readTestFile(t, logPath); !bytes.Equal(got, torn) {
		t.Fatal("marker-sync failure truncated the recoverable tail before durability was proven")
	}

	store.openPublicationMarkerSync = nil
	opened, err = store.Open(id)
	if err != nil {
		t.Fatalf("stable restart did not repair the preserved torn tail: %v", err)
	}
	if opened.TruncatedBytes() == 0 {
		t.Fatal("stable restart did not report its torn-tail repair")
	}
	_ = opened.Close()
}

func TestPublishedStagedOpenDurabilityRecoveryIsIdempotent(t *testing.T) {
	store, workspace, id, logPath, markerPath := publishedStagedSession(t)
	logBefore := readTestFile(t, logPath)
	markerBefore := readTestFile(t, markerPath)
	var order []string
	store.openPublicationMarkerSync = func(marker *os.File) error {
		order = append(order, "marker")
		return marker.Sync()
	}
	store.openPublicationDirectorySync = func(directory *os.File) error {
		order = append(order, "directory")
		return syncOpenedSessionDirectory(directory)
	}

	first, err := store.Open(id)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := store.OpenInWorkspace(id, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "marker,directory,marker,directory" {
		t.Fatalf("durability order = %q", strings.Join(order, ","))
	}
	if got := readTestFile(t, logPath); !bytes.Equal(got, logBefore) {
		t.Fatal("idempotent durability recovery changed the session log")
	}
	if got := readTestFile(t, markerPath); !bytes.Equal(got, markerBefore) {
		t.Fatal("idempotent durability recovery changed the publication marker")
	}
}

func TestPublishedStagedOpenRefusesTornMarkerWithoutMutation(t *testing.T) {
	store, workspace, id, logPath, markerPath := publishedStagedSession(t)
	logBefore := readTestFile(t, logPath)
	complete := readTestFile(t, markerPath)
	if err := os.WriteFile(markerPath, complete[:len(complete)-1], 0o600); err != nil {
		t.Fatal(err)
	}
	torn := readTestFile(t, markerPath)

	entries := []struct {
		name string
		open func() (*Session, error)
	}{
		{"Open", func() (*Session, error) { return store.Open(id) }},
		{"OpenInWorkspace", func() (*Session, error) { return store.OpenInWorkspace(id, workspace) }},
		{"Latest", func() (*Session, error) { return store.Latest(workspace) }},
	}
	for _, entry := range entries {
		t.Run(entry.name, func(t *testing.T) {
			opened, err := entry.open()
			if opened != nil {
				_ = opened.Close()
				t.Fatal("torn publication marker admitted a writable session")
			}
			if err == nil {
				t.Fatal("torn publication marker returned no error")
			}
			if got := readTestFile(t, logPath); !bytes.Equal(got, logBefore) {
				t.Fatal("torn-marker refusal mutated the session log")
			}
			if got := readTestFile(t, markerPath); !bytes.Equal(got, torn) {
				t.Fatal("torn-marker refusal rewrote the marker")
			}
		})
	}

	if err := os.WriteFile(markerPath, complete, 0o600); err != nil {
		t.Fatal(err)
	}
	opened, err := store.Open(id)
	if err != nil {
		t.Fatalf("restored exact marker was not resumable: %v", err)
	}
	_ = opened.Close()
}

func TestPublishedStagedOpenRejectsMarkerPathReplacementAfterOpen(t *testing.T) {
	store, _, id, logPath, markerPath := publishedStagedSession(t)
	logBefore := readTestFile(t, logPath)
	markerBytes := readTestFile(t, markerPath)
	moved := markerPath + ".opened"
	hookCalls := 0
	store.openPublicationAfterMarker = func(path string) {
		hookCalls++
		if err := os.Rename(path, moved); err != nil {
			t.Fatalf("renaming opened marker: %v", err)
		}
		if err := os.WriteFile(path, markerBytes, 0o600); err != nil {
			t.Fatalf("writing exact marker replacement: %v", err)
		}
	}

	opened, err := store.Open(id)
	if opened != nil {
		_ = opened.Close()
		t.Fatal("replacement marker admitted a writable session")
	}
	if hookCalls != 1 {
		t.Fatalf("marker replacement hook calls = %d, want 1", hookCalls)
	}
	assertPublicationResumeRecoveryError(t, err, nil)
	if got := readTestFile(t, logPath); !bytes.Equal(got, logBefore) {
		t.Fatal("marker replacement refusal mutated the session log")
	}
	if got := readTestFile(t, markerPath); !bytes.Equal(got, markerBytes) {
		t.Fatal("marker replacement was unexpectedly modified")
	}
	if got := readTestFile(t, moved); !bytes.Equal(got, markerBytes) {
		t.Fatal("opened original marker was unexpectedly modified")
	}

	store.openPublicationAfterMarker = nil
	opened, err = store.Open(id)
	if err != nil {
		t.Fatalf("stable exact replacement could not be recovered on restart: %v", err)
	}
	_ = opened.Close()
}

func TestPublishedStagedOpenRejectsChildHistoryTruncationBeforeAndAfterCommitCapture(t *testing.T) {
	for _, phase := range []string{"before directory bind", "after marker sync"} {
		t.Run(phase, func(t *testing.T) {
			store, _, id, logPath, _ := publishedStagedSession(t)
			raw := readTestFile(t, logPath)
			headerEnd := bytes.IndexByte(raw, '\n')
			if headerEnd < 0 {
				t.Fatal("published fixture has no header terminator")
			}
			firstRecordRelativeEnd := bytes.IndexByte(raw[headerEnd+1:], '\n')
			if firstRecordRelativeEnd < 0 {
				t.Fatal("published fixture has no session_start terminator")
			}
			firstRecordEnd := headerEnd + 1 + firstRecordRelativeEnd + 1
			if firstRecordEnd >= len(raw) {
				t.Fatal("published fixture has no history after session_start")
			}
			truncate := func() error { return os.Truncate(logPath, int64(firstRecordEnd)) }
			var mutationErr error
			if phase == "before directory bind" {
				store.openPublicationBeforeDirectory = func(string) { mutationErr = truncate() }
			}
			directorySyncs := 0
			store.openPublicationMarkerSync = func(marker *os.File) error {
				if err := marker.Sync(); err != nil {
					return err
				}
				if phase == "after marker sync" {
					mutationErr = truncate()
				}
				return mutationErr
			}
			store.openPublicationDirectorySync = func(*os.File) error {
				directorySyncs++
				return nil
			}

			opened, openErr := store.Open(id)
			if mutationErr != nil {
				t.Fatal(mutationErr)
			}
			if opened != nil {
				_ = opened.Close()
				t.Fatal("child history truncation admitted a writable session")
			}
			assertPublicationResumeRecoveryError(t, openErr, nil)
			if directorySyncs != 0 {
				t.Fatalf("directory syncs after child history truncation = %d, want 0", directorySyncs)
			}
			if got := readTestFile(t, logPath); !bytes.Equal(got, raw[:firstRecordEnd]) {
				t.Fatal("history truncation seam did not leave the intended header and first session_start")
			}
		})
	}
}

func TestPublishedStagedOpenRejectsExactChildLogRewriteAfterMarkerSync(t *testing.T) {
	store, _, id, logPath, _ := publishedStagedSession(t)
	original := readTestFile(t, logPath)
	before, err := os.Stat(logPath)
	if err != nil {
		t.Fatal(err)
	}
	directorySyncs := 0
	store.openPublicationMarkerSync = func(marker *os.File) error {
		if err := marker.Sync(); err != nil {
			return err
		}
		return rewriteExactSessionLogPreservingMtime(logPath, original, before)
	}
	store.openPublicationDirectorySync = func(*os.File) error {
		directorySyncs++
		return nil
	}

	opened, openErr := store.Open(id)
	if opened != nil {
		_ = opened.Close()
		t.Fatal("exact child-log rewrite admitted a writable session")
	}
	assertPublicationResumeRecoveryError(t, openErr, nil)
	if directorySyncs != 0 {
		t.Fatalf("directory syncs after exact child rewrite = %d, want 0", directorySyncs)
	}
	assertExactSessionLogRewrite(t, logPath, original, before)
}

func publishedStagedSession(t *testing.T) (*Store, string, string, string, string) {
	t.Helper()
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	staged, err := store.CreateStaged(workspace, provider.RouteTargetID("test/local/published"), "rev")
	if err != nil {
		t.Fatal(err)
	}
	if err := staged.AppendNote("info", "published staged child"); err != nil {
		t.Fatal(err)
	}
	if outcome, err := staged.PublishDurably(); err != nil || !outcome.Visible || !outcome.Durable {
		t.Fatalf("publishing staged fixture: outcome=%+v err=%v", outcome, err)
	}
	id, logPath := staged.ID(), staged.Path()
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
	return store, workspace, id, logPath, publicationMarkerPath(logPath)
}

func readTestFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func assertPublicationResumeRecoveryError(t *testing.T, err, cause error) {
	t.Helper()
	if err == nil {
		t.Fatal("writable publication recovery returned no error")
	}
	if cause != nil && !errors.Is(err, cause) {
		t.Fatalf("publication recovery error = %v, want cause %v", err, cause)
	}
	for _, phrase := range []string{"refusing writable resume", ".log and .published", "restart Switchboard", "retry recovery"} {
		if !strings.Contains(err.Error(), phrase) {
			t.Fatalf("publication recovery error %q does not contain %q", err, phrase)
		}
	}
}
