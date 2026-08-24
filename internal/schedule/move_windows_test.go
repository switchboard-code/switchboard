//go:build windows

package schedule

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"unsafe"
)

func openWindowsMoveFixture(t *testing.T) (string, *os.Root, *ledgerImage, *os.Root) {
	t.Helper()
	dir := t.TempDir()
	path := writeMigrationLedger(t, dir, []Entry{migrationEntry("opened image")})
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	image, err := readLedger(root, FileName, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(image.close)
	if err := root.Mkdir("quarantine", 0o700); err != nil {
		t.Fatal(err)
	}
	quarantine, err := root.OpenRoot("quarantine")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = quarantine.Close() })
	return dir, root, image, quarantine
}

func TestMoveScheduleNoReplaceWindowsPreservesOccupiedDestination(t *testing.T) {
	dir, root, image, quarantine := openWindowsMoveFixture(t)
	const sentinel = "occupied"
	if err := quarantine.WriteFile(migrationQuarantineEntry, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	published, err := moveScheduleNoReplace(root, quarantine, FileName, migrationQuarantineEntry, image.file)
	if published || err == nil {
		t.Fatalf("occupied no-replace move = published %v, %v", published, err)
	}
	if got := string(readMigrationBytes(t, filepath.Join(dir, "quarantine", migrationQuarantineEntry))); got != sentinel {
		t.Fatalf("occupied quarantine was replaced: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, FileName)); err != nil {
		t.Fatalf("source moved on occupied no-replace destination: %v", err)
	}
}

func TestMoveScheduleNoReplaceWindowsMovesExactOpenedImage(t *testing.T) {
	dir, root, image, quarantine := openWindowsMoveFixture(t)
	source := filepath.Join(dir, FileName)
	backup := filepath.Join(dir, "read-image")
	if err := os.Rename(source, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	published, err := moveScheduleNoReplace(root, quarantine, FileName, migrationQuarantineEntry, image.file)
	if !published || err != nil {
		t.Fatalf("exact-handle no-replace move = published %v, %v", published, err)
	}
	if got := string(readMigrationBytes(t, source)); got != "replacement" {
		t.Fatalf("source replacement changed: %q", got)
	}
	if _, err := os.Stat(backup); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("opened image backup survived exact-handle rename: %v", err)
	}
	if err := verifyLedgerImage(image, quarantine, migrationQuarantineEntry); err != nil {
		t.Fatalf("quarantine does not hold exact opened image: %v", err)
	}
}

func TestScheduleRenameInfoWindowsLayout(t *testing.T) {
	wantRootOffset := uintptr(4)
	wantNameOffset := uintptr(12)
	if unsafe.Sizeof(uintptr(0)) == 8 {
		wantRootOffset = 8
		wantNameOffset = 20
	}
	var info scheduleRenameInfo
	if got := unsafe.Offsetof(info.ReplaceIfExists); got != 0 {
		t.Fatalf("FILE_RENAME_INFORMATION ReplaceIfExists offset = %d, want 0", got)
	}
	if got := unsafe.Offsetof(info.RootDirectory); got != wantRootOffset {
		t.Fatalf("FILE_RENAME_INFORMATION RootDirectory offset = %d, want %d", got, wantRootOffset)
	}
	if got := unsafe.Offsetof(info.FileName); got != wantNameOffset {
		t.Fatalf("FILE_RENAME_INFORMATION FileName offset = %d, want %d", got, wantNameOffset)
	}
}
