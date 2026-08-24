//go:build windows

package checkpoint

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestRenameBoundOpenFileWindowsExchange(t *testing.T) {
	root, _ := openWindowsTestRoot(t)
	source := createWindowsRootFile(t, root, "source", "new")
	displaced := createWindowsRootFile(t, root, "target", "old")

	published, err := renameBoundOpenFile(root, source, displaced, "source", "target", true)
	if err != nil || !published {
		t.Fatalf("exchange = published %v, %v", published, err)
	}
	if got := readWindowsRootFile(t, root, "target"); got != "new" {
		t.Fatalf("target = %q, want new", got)
	}
	if got := readWindowsRootFile(t, root, "source"); got != "old" {
		t.Fatalf("source = %q, want displaced old content", got)
	}
	if _, err := root.Lstat(restoreExchangeStagingName("source", "target")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("staging name remains after exchange: %v", err)
	}
}

func TestRenameBoundOpenFileWindowsNestedExchangeAndRollback(t *testing.T) {
	root, dir := openWindowsTestRoot(t)
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	sourceName := filepath.Join("nested", ".switchboard-undo-0123456789abcdef0123456789abcdef")
	targetName := filepath.Join("nested", ".switchboard-undo-fedcba9876543210fedcba9876543210")
	staging := restoreExchangeStagingName(sourceName, targetName)
	if reverse := restoreExchangeStagingName(targetName, sourceName); reverse != staging {
		t.Fatalf("exchange staging is directional: forward %q, reverse %q", staging, reverse)
	}
	if got, want := filepath.Dir(staging), filepath.Dir(sourceName); got != want {
		t.Fatalf("exchange staging parent = %q, want %q", got, want)
	}
	source := createWindowsRootFile(t, root, sourceName, "new")
	displaced := createWindowsRootFile(t, root, targetName, "old")

	published, err := renameBoundOpenFile(root, source, displaced, sourceName, targetName, true)
	if err != nil || !published {
		t.Fatalf("nested exchange = published %v, %v", published, err)
	}
	if got := readWindowsRootFile(t, root, targetName); got != "new" {
		t.Fatalf("nested target = %q, want new", got)
	}
	if got := readWindowsRootFile(t, root, sourceName); got != "old" {
		t.Fatalf("nested source = %q, want old", got)
	}

	rolledBack, err := rollbackBoundReplacement(root, source, displaced, sourceName, targetName)
	if err != nil || !rolledBack {
		t.Fatalf("nested rollback = rolledBack %v, %v", rolledBack, err)
	}
	if got := readWindowsRootFile(t, root, targetName); got != "old" {
		t.Fatalf("rolled-back target = %q, want old", got)
	}
	if got := readWindowsRootFile(t, root, sourceName); got != "new" {
		t.Fatalf("rolled-back source = %q, want new", got)
	}
	if _, err := root.Lstat(staging); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("nested staging name remains after rollback: %v", err)
	}
}

func TestRenameBoundOpenFileWindowsRejectsCrossParentExchange(t *testing.T) {
	root, dir := openWindowsTestRoot(t)
	for _, parent := range []string{"left", "right"} {
		if err := os.Mkdir(filepath.Join(dir, parent), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sourceName := filepath.Join("left", "source")
	targetName := filepath.Join("right", "target")
	source := createWindowsRootFile(t, root, sourceName, "new")
	displaced := createWindowsRootFile(t, root, targetName, "old")

	published, err := renameBoundOpenFile(root, source, displaced, sourceName, targetName, true)
	if err == nil || published {
		t.Fatalf("cross-parent exchange = published %v, %v; want unpublished refusal", published, err)
	}
	if got := readWindowsRootFile(t, root, sourceName); got != "new" {
		t.Fatalf("source = %q, want new", got)
	}
	if got := readWindowsRootFile(t, root, targetName); got != "old" {
		t.Fatalf("target = %q, want old", got)
	}
}

func TestRenameBoundOpenFileWindowsAncestorLeaseCoversEveryExchangeStep(t *testing.T) {
	root, dir := openWindowsTestRoot(t)
	parent := filepath.Join(dir, "nested")
	if err := os.MkdirAll(filepath.Join(parent, "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	sourceName := filepath.Join("nested", "deep", "source")
	targetName := filepath.Join("nested", "deep", "target")
	source := createWindowsRootFile(t, root, sourceName, "new")
	displaced := createWindowsRootFile(t, root, targetName, "old")
	moved := filepath.Join(t.TempDir(), "moved")
	var moveErrors []error
	windowsBeforeNativeRenameTestHook = func(_, _ string) error {
		moveErr := os.Rename(parent, moved)
		moveErrors = append(moveErrors, moveErr)
		if moveErr == nil {
			return errors.Join(
				errors.New("leased checkpoint ancestor was movable at the native rename seam"),
				os.Rename(moved, parent),
			)
		}
		return nil
	}
	t.Cleanup(func() { windowsBeforeNativeRenameTestHook = nil })

	published, err := renameBoundOpenFile(root, source, displaced, sourceName, targetName, true)
	windowsBeforeNativeRenameTestHook = nil
	if err != nil || !published {
		t.Fatalf("leased exchange = published %v, %v", published, err)
	}
	if len(moveErrors) != 3 {
		t.Fatalf("native exchange seam ran %d times, want all 3 steps", len(moveErrors))
	}
	for step, moveErr := range moveErrors {
		if moveErr == nil {
			t.Fatalf("ancestor move at exchange step %d unexpectedly succeeded", step+1)
		}
	}
	if got := readWindowsRootFile(t, root, targetName); got != "new" {
		t.Fatalf("target = %q, want new", got)
	}
	if got := readWindowsRootFile(t, root, sourceName); got != "old" {
		t.Fatalf("source = %q, want old", got)
	}
}

func TestRenameBoundOpenFileWindowsMutationHandleLeasesSourceLink(t *testing.T) {
	root, dir := openWindowsTestRoot(t)
	for _, parent := range []string{"nested", "other"} {
		if err := os.Mkdir(filepath.Join(dir, parent), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sourceName := filepath.Join("nested", "source")
	targetName := filepath.Join("nested", "target")
	source := createWindowsRootFile(t, root, sourceName, "new")
	attackerName := filepath.Join("other", "source")
	var moveErr error
	windowsBeforeNativeRenameTestHook = func(_, _ string) error {
		moveErr = root.Rename(sourceName, attackerName)
		if moveErr == nil {
			return errors.Join(
				errors.New("exact checkpoint source link was movable at the native rename seam"),
				root.Rename(attackerName, sourceName),
			)
		}
		return nil
	}
	t.Cleanup(func() { windowsBeforeNativeRenameTestHook = nil })

	published, err := renameBoundOpenFile(root, source, nil, sourceName, targetName, false)
	windowsBeforeNativeRenameTestHook = nil
	if err != nil || !published {
		t.Fatalf("leased rename = published %v, %v", published, err)
	}
	if moveErr == nil {
		t.Fatal("source-link move unexpectedly succeeded")
	}
	if got := readWindowsRootFile(t, root, targetName); got != "new" {
		t.Fatalf("target = %q, want new", got)
	}
	if _, err := root.Lstat(attackerName); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("attacker destination appeared: %v", err)
	}
}

func TestRenameBoundOpenFileWindowsBindsSourceAndDestinationAfterPreMove(t *testing.T) {
	root, dir := openWindowsTestRoot(t)
	for _, parent := range []string{"intended", "outside"} {
		if err := os.Mkdir(filepath.Join(dir, parent), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sourceName := filepath.Join("intended", "source")
	targetName := filepath.Join("intended", "target")
	movedName := filepath.Join("outside", "moved")
	escapedTarget := filepath.Join("outside", "target")
	source := createWindowsRootFile(t, root, sourceName, "new")

	// Move the open link before renameBoundOpenFile can acquire its source-link
	// lease, then put a hard link to the same inode back at the checked name.
	// Stable file identity alone cannot distinguish this state.
	if err := root.Rename(sourceName, movedName); err != nil {
		t.Fatalf("pre-moving selected source link: %v", err)
	}
	if err := os.Link(filepath.Join(dir, movedName), filepath.Join(dir, sourceName)); err != nil {
		t.Fatalf("hard-linking selected inode back at source: %v", err)
	}

	published, err := renameBoundOpenFile(root, source, nil, sourceName, targetName, false)
	if err != nil || !published {
		t.Fatalf("destination-bound rename = published %v, %v", published, err)
	}
	if got := readWindowsRootFile(t, root, targetName); got != "new" {
		t.Fatalf("bound target = %q, want new", got)
	}
	if _, err := root.Lstat(sourceName); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("checked source alias remains after move: %v", err)
	}
	if got := readWindowsRootFile(t, root, movedName); got != "new" {
		t.Fatalf("pre-moved external alias = %q, want new", got)
	}
	if _, err := root.Lstat(escapedTarget); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("rename escaped into the source link's former parent: %v", err)
	}
}

func TestRenameBoundOpenFileWindowsAncestorLeaseCoversRollback(t *testing.T) {
	root, dir := openWindowsTestRoot(t)
	parent := filepath.Join(dir, "nested")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	sourceName := filepath.Join("nested", "source")
	targetName := filepath.Join("nested", "target")
	source := createWindowsRootFile(t, root, sourceName, "new")
	displaced := createWindowsRootFile(t, root, targetName, "old")
	moved := filepath.Join(t.TempDir(), "moved")
	injected := errors.New("injected before the second native exchange call")
	var moveErrors []error
	windowsBeforeNativeRenameTestHook = func(_, _ string) error {
		moveErr := os.Rename(parent, moved)
		moveErrors = append(moveErrors, moveErr)
		if moveErr == nil {
			return errors.Join(
				errors.New("leased checkpoint ancestor was movable during exchange rollback"),
				os.Rename(moved, parent),
			)
		}
		if len(moveErrors) == 2 {
			return injected
		}
		return nil
	}
	t.Cleanup(func() { windowsBeforeNativeRenameTestHook = nil })

	published, err := renameBoundOpenFile(root, source, displaced, sourceName, targetName, true)
	windowsBeforeNativeRenameTestHook = nil
	if !published || !errors.Is(err, injected) {
		t.Fatalf("exchange rollback = published %v, %v; want published injected error", published, err)
	}
	if len(moveErrors) != 3 {
		t.Fatalf("native seam ran %d times, want stage, refused second step, and rollback", len(moveErrors))
	}
	for step, moveErr := range moveErrors {
		if moveErr == nil {
			t.Fatalf("ancestor move at rollback step %d unexpectedly succeeded", step+1)
		}
	}
	if got := readWindowsRootFile(t, root, sourceName); got != "new" {
		t.Fatalf("rolled-back source = %q, want new", got)
	}
	if got := readWindowsRootFile(t, root, targetName); got != "old" {
		t.Fatalf("rolled-back target = %q, want old", got)
	}
}

func TestRenameBoundOpenFileWindowsRefusesNestedCrossParentDestination(t *testing.T) {
	root, dir := openWindowsTestRoot(t)
	for _, parent := range []string{filepath.Join("left", "deep"), filepath.Join("right", "deep")} {
		if err := os.MkdirAll(filepath.Join(dir, parent), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sourceName := filepath.Join("left", "deep", "source")
	targetName := filepath.Join("right", "deep", "target")
	source := createWindowsRootFile(t, root, sourceName, "new")

	published, err := renameBoundOpenFile(root, source, nil, sourceName, targetName, false)
	if err == nil || published || !strings.Contains(err.Error(), "destination in the bound root") {
		t.Fatalf("nested cross-parent no-replace = published %v, %v; want unpublished secure refusal", published, err)
	}
	if got := readWindowsRootFile(t, root, sourceName); got != "new" {
		t.Fatalf("source after refusal = %q, want new", got)
	}
	if _, err := root.Lstat(targetName); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("nested destination appeared after refusal: %v", err)
	}
}

func TestAcquireWindowsNamespaceLeaseRejectsReparseDirectory(t *testing.T) {
	root, dir := openWindowsTestRoot(t)
	outside := t.TempDir()
	junction := filepath.Join(dir, "junction")
	if err := os.Symlink(outside, junction); err != nil {
		t.Skipf("creating a Windows directory reparse point requires developer mode or privilege: %v", err)
	}
	lease, err := acquireWindowsNamespaceLease(root, filepath.Join("junction", "target"))
	if lease != nil {
		_ = lease.close()
		t.Fatal("namespace lease followed a directory reparse point")
	}
	if err == nil {
		t.Fatal("namespace lease accepted a directory reparse point")
	}
}

func TestWindowsNamespaceLeaseReopensAndDeleteLeasesExactRoot(t *testing.T) {
	root, _ := openWindowsTestRoot(t)
	rootFile, err := root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer rootFile.Close()
	openDelete := func() (windows.Handle, error) {
		return reopenWindowsTestDirectoryDeleteHandle(windows.Handle(rootFile.Fd()))
	}
	probe, err := openDelete()
	if err != nil {
		t.Fatalf("calibrating exact root DELETE access: %v", err)
	}
	if err := windows.CloseHandle(probe); err != nil {
		t.Fatal(err)
	}

	lease, err := acquireWindowsDestinationNamespaceLease(root, "target", "source")
	if err != nil {
		t.Fatalf("reopening exact namespace root for lease: %v", err)
	}
	defer lease.close()
	leasedRoot, err := lease.directory("target")
	if err != nil {
		t.Fatal(err)
	}
	if err := requireSameWindowsHandle(windows.Handle(rootFile.Fd()), leasedRoot); err != nil {
		t.Fatalf("namespace root lease changed identity: %v", err)
	}
	if probe, err = openDelete(); err == nil {
		_ = windows.CloseHandle(probe)
		t.Fatal("DELETE-capable root handle opened while no-delete namespace lease was active")
	} else if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("opening DELETE-capable root handle during lease = %v, want sharing violation", err)
	}
	if err := lease.close(); err != nil {
		t.Fatal(err)
	}
	probe, err = openDelete()
	if err != nil {
		t.Fatalf("root DELETE access remained blocked after lease close: %v", err)
	}
	if err := windows.CloseHandle(probe); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsNamespaceLeaseRejectsReparseRoot(t *testing.T) {
	root, dir := openWindowsTestRoot(t)
	target := t.TempDir()
	if err := setWindowsTestMountPoint(dir, target); err != nil {
		if windowsReparseTestUnavailable(err) {
			t.Skipf("this Windows environment cannot install a directory reparse point: %v", err)
		}
		t.Fatalf("installing root reparse point: %v", err)
	}
	defer func() {
		if err := deleteWindowsTestMountPoint(dir); err != nil {
			t.Errorf("cleaning root reparse point: %v", err)
		}
	}()

	lease, err := acquireWindowsDestinationNamespaceLease(root, "target", "source")
	if lease != nil {
		_ = lease.close()
		t.Fatal("namespace lease accepted a reparse-point root")
	}
	if err == nil {
		t.Fatal("namespace lease did not reject a reparse-point root")
	}
}

func TestWindowsNamespaceLeaseFencesWriteCapableAncestorHandles(t *testing.T) {
	root, dir := openWindowsTestRoot(t)
	parent := filepath.Join(dir, "nested")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	probe, err := openWindowsDirectoryWriteHandle(parent)
	if err != nil {
		t.Skipf("this Windows filesystem does not permit a write-capable directory probe: %v", err)
	}
	if err := windows.CloseHandle(probe); err != nil {
		t.Fatal(err)
	}

	writer, err := openWindowsDirectoryWriteHandle(parent)
	if err != nil {
		t.Fatal(err)
	}
	lease, leaseErr := acquireWindowsNamespaceLease(root, filepath.Join("nested", "target"))
	if lease != nil {
		_ = lease.close()
	}
	if leaseErr == nil {
		_ = windows.CloseHandle(writer)
		t.Fatal("namespace lease succeeded alongside a preexisting write-capable ancestor handle")
	}
	if err := windows.CloseHandle(writer); err != nil {
		t.Fatal(err)
	}

	lease, err = acquireWindowsNamespaceLease(root, filepath.Join("nested", "target"))
	if err != nil {
		t.Fatalf("acquiring namespace lease after writer close: %v", err)
	}
	defer lease.close()
	writer, err = openWindowsDirectoryWriteHandle(parent)
	if err == nil {
		_ = windows.CloseHandle(writer)
		t.Fatal("write-capable ancestor handle opened while namespace lease was active")
	}
}

func TestWindowsDestinationNamespaceLeaseSharesWritesAndFencesDelete(t *testing.T) {
	root, dir := openWindowsTestRoot(t)
	parent := filepath.Join(dir, "nested")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}

	writer, err := openWindowsDirectoryWriteHandle(parent)
	if err != nil {
		t.Skipf("this Windows filesystem does not permit a write-capable directory probe: %v", err)
	}
	lease, err := acquireWindowsDestinationNamespaceLease(
		root,
		filepath.Join("nested", "target"),
		filepath.Join("nested", "source"),
	)
	if err != nil {
		_ = windows.CloseHandle(writer)
		t.Fatalf("acquiring destination lease alongside writer: %v", err)
	}
	defer lease.close()
	if err := windows.CloseHandle(writer); err != nil {
		t.Fatal(err)
	}
	writer, err = openWindowsDirectoryWriteHandle(parent)
	if err != nil {
		t.Fatalf("destination lease blocked a write-capable parent handle: %v", err)
	}
	if err := windows.CloseHandle(writer); err != nil {
		t.Fatal(err)
	}

	moved := filepath.Join(dir, "moved")
	if err := os.Rename(parent, moved); err == nil {
		_ = os.Rename(moved, parent)
		t.Fatal("destination parent was movable while its no-delete lease was active")
	}
}

func TestWindowsNamespaceAnchorIsNonemptyDeleteLeasedAndExactCleaned(t *testing.T) {
	root, _ := openWindowsTestRoot(t)
	anchor, err := createWindowsNamespaceAnchor(root)
	if err != nil {
		t.Fatalf("creating namespace anchor: %v", err)
	}
	defer anchor.close()

	if info, err := root.Lstat(anchor.name); err != nil || !info.Mode().IsRegular() || info.Size() != 0 {
		t.Fatalf("namespace anchor = %#v, %v; want zero-byte regular file", info, err)
	}
	if err := root.Remove(anchor.name); err == nil {
		t.Fatal("namespace anchor was removable while its no-delete lease was active")
	}
	if err := root.Rename(anchor.name, "moved-anchor"); err == nil {
		t.Fatal("namespace anchor was movable while its no-delete lease was active")
	}

	name := anchor.name
	if err := anchor.close(); err != nil {
		t.Fatalf("cleaning namespace anchor by exact handle: %v", err)
	}
	if _, err := root.Lstat(name); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("namespace anchor remains after exact cleanup: %v", err)
	}
}

func TestWindowsNamespaceAnchorBlocksDirectoryReparseMutation(t *testing.T) {
	target := t.TempDir()
	probeParent := t.TempDir()
	probe := filepath.Join(probeParent, "empty")
	if err := os.Mkdir(probe, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := setWindowsTestMountPoint(probe, target); err != nil {
		if windowsReparseTestUnavailable(err) {
			t.Skipf("this Windows environment cannot install a directory reparse point: %v", err)
		}
		t.Fatalf("calibrating directory reparse-point support: %v", err)
	}
	if err := deleteWindowsTestMountPoint(probe); err != nil {
		t.Fatalf("cleaning directory reparse-point calibration: %v", err)
	}
	if err := os.Remove(probe); err != nil {
		t.Fatalf("removing directory reparse-point calibration: %v", err)
	}

	root, dir := openWindowsTestRoot(t)
	anchor, err := createWindowsNamespaceAnchor(root)
	if err != nil {
		t.Fatalf("creating namespace anchor: %v", err)
	}
	defer anchor.close()

	setErr := setWindowsTestMountPoint(dir, target)
	if setErr == nil {
		cleanupErr := deleteWindowsTestMountPoint(dir)
		t.Fatalf("namespace anchor allowed its directory to become a reparse point; cleanup: %v", cleanupErr)
	}
	if !errors.Is(setErr, windows.ERROR_DIR_NOT_EMPTY) {
		t.Fatalf("setting a reparse point on the anchored directory = %v, want ERROR_DIR_NOT_EMPTY", setErr)
	}
	directory, err := root.Open(".")
	if err != nil {
		t.Fatalf("reopening anchored root after refused reparse mutation: %v", err)
	}
	defer directory.Close()
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(directory.Fd()), &info); err != nil {
		t.Fatalf("inspecting anchored root after refused reparse mutation: %v", err)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		t.Fatal("anchored root retained a reparse-point attribute after the refused mutation")
	}
}

func setWindowsTestMountPoint(path, target string) (err error) {
	buffer, err := windowsTestMountPointBuffer(target)
	if err != nil {
		return err
	}
	handle, err := openWindowsTestReparseHandle(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, windows.CloseHandle(handle)) }()
	var returned uint32
	return windows.DeviceIoControl(
		handle,
		windows.FSCTL_SET_REPARSE_POINT,
		&buffer[0],
		uint32(len(buffer)),
		nil,
		0,
		&returned,
		nil,
	)
}

func deleteWindowsTestMountPoint(path string) (err error) {
	handle, err := openWindowsTestReparseHandle(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, windows.CloseHandle(handle)) }()
	buffer := make([]byte, 8)
	binary.LittleEndian.PutUint32(buffer, windows.IO_REPARSE_TAG_MOUNT_POINT)
	var returned uint32
	return windows.DeviceIoControl(
		handle,
		windows.FSCTL_DELETE_REPARSE_POINT,
		&buffer[0],
		uint32(len(buffer)),
		nil,
		0,
		&returned,
		nil,
	)
}

func openWindowsTestReparseHandle(path string) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	return windows.CreateFile(
		name,
		windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
}

func windowsTestMountPointBuffer(target string) ([]byte, error) {
	abs, err := filepath.Abs(target)
	if err != nil {
		return nil, err
	}
	substitute, err := windows.UTF16FromString(`\??\` + abs)
	if err != nil {
		return nil, err
	}
	printName, err := windows.UTF16FromString(abs)
	if err != nil {
		return nil, err
	}
	pathWords := append(substitute, printName...)
	const mountPointFields = 8
	dataLength := mountPointFields + len(pathWords)*2
	if dataLength > int(^uint16(0)) {
		return nil, errors.New("test mount-point target is too long")
	}
	buffer := make([]byte, 8+dataLength)
	binary.LittleEndian.PutUint32(buffer[0:4], windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(buffer[4:6], uint16(dataLength))
	binary.LittleEndian.PutUint16(buffer[8:10], 0)
	binary.LittleEndian.PutUint16(buffer[10:12], uint16((len(substitute)-1)*2))
	binary.LittleEndian.PutUint16(buffer[12:14], uint16(len(substitute)*2))
	binary.LittleEndian.PutUint16(buffer[14:16], uint16((len(printName)-1)*2))
	for i, word := range pathWords {
		binary.LittleEndian.PutUint16(buffer[16+i*2:], word)
	}
	return buffer, nil
}

func windowsReparseTestUnavailable(err error) bool {
	return errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) ||
		errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_INVALID_FUNCTION) ||
		errors.Is(err, windows.ERROR_NOT_SUPPORTED)
}

func TestWindowsNamespaceAnchorDeletesOnAbruptProcessExit(t *testing.T) {
	_, dir := openWindowsTestRoot(t)
	command := exec.Command(os.Args[0], "-test.run=^TestWindowsNamespaceAnchorCrashHelper$")
	command.Env = append(os.Environ(),
		"SWITCHBOARD_TEST_NAMESPACE_ANCHOR_CRASH=1",
		"SWITCHBOARD_TEST_NAMESPACE_ANCHOR_DIR="+dir,
	)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("anchor crash helper: %v\n%s", err, output)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("namespace anchor survived abrupt process exit: %v", entries)
	}
}

func TestWindowsNamespaceAnchorCrashHelper(t *testing.T) {
	if os.Getenv("SWITCHBOARD_TEST_NAMESPACE_ANCHOR_CRASH") != "1" {
		return
	}
	dir := os.Getenv("SWITCHBOARD_TEST_NAMESPACE_ANCHOR_DIR")
	root, err := os.OpenRoot(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if _, err := createWindowsNamespaceAnchor(root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	// Deliberately bypass every defer and explicit close. Windows must retire
	// the FILE_DELETE_ON_CLOSE anchor when the process tears down its handles.
	os.Exit(0)
}

func TestRollbackBoundCreationWindowsNestedPath(t *testing.T) {
	root, dir := openWindowsTestRoot(t)
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	tempName := filepath.Join("nested", ".switchboard-undo-0123456789abcdef0123456789abcdef")
	targetName := filepath.Join("nested", "created")
	temp := createWindowsRootFile(t, root, tempName, "desired")
	result, err := renameBoundRestoreFile(root, temp, nil, tempName, targetName, false)
	if err != nil || !result.published {
		t.Fatalf("nested create = %+v, %v", result, err)
	}
	namespace := &boundRestoreNamespace{root: root, target: targetName, dir: filepath.Dir(targetName), display: targetName}
	tempInfo, err := temp.Stat()
	if err != nil {
		t.Fatal(err)
	}
	removed, err := rollbackBoundCreation(namespace, nil, tempName, temp, fingerprintBytes(true, tempInfo.Mode(), []byte("desired")))
	if err != nil || !removed {
		t.Fatalf("nested creation rollback = removed %v, %v", removed, err)
	}
	if _, err := root.Lstat(targetName); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("nested created target remains: %v", err)
	}
	if _, err := root.Lstat(tempName); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("nested rollback temporary remains: %v", err)
	}
}

func TestRetireBoundOpenFileToWindowsNestedSource(t *testing.T) {
	sourceRoot, sourceDir := openWindowsTestRoot(t)
	sinkRoot, _ := openWindowsTestRoot(t)
	if err := os.Mkdir(filepath.Join(sourceDir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join("nested", "owned")
	file := createWindowsRootFile(t, sourceRoot, name, "secret")

	if err := retireBoundOpenFileTo(sourceRoot, sinkRoot, name, file, true, nil, nil); err != nil {
		t.Fatalf("retire nested source to sink: %v", err)
	}
	if _, err := sourceRoot.Lstat(name); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("nested source remains after retirement: %v", err)
	}
	if _, err := sinkRoot.Lstat(retiredSinkName(name)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("nested retired sink name remains: %v", err)
	}
}

func TestRetireBoundOpenFileToWindowsAnchorsEmptySinkAtNativeSeam(t *testing.T) {
	sourceRoot, _ := openWindowsTestRoot(t)
	sinkRoot, _ := openWindowsTestRoot(t)
	file := createWindowsRootFile(t, sourceRoot, "owned", "secret")
	var sawAnchor bool
	windowsBeforeNativeRenameTestHook = func(_, _ string) error {
		directory, err := sinkRoot.Open(".")
		if err != nil {
			return err
		}
		entries, readErr := directory.ReadDir(-1)
		closeErr := directory.Close()
		if readErr != nil || closeErr != nil {
			return errors.Join(readErr, closeErr)
		}
		if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), ".switchboard-quarantine-") {
			return fmt.Errorf("retirement sink entries at native seam = %v; want one private anchor", entries)
		}
		if err := sinkRoot.Remove(entries[0].Name()); err == nil {
			return errors.New("retirement sink anchor was removable at native rename seam")
		}
		sawAnchor = true
		return nil
	}
	t.Cleanup(func() { windowsBeforeNativeRenameTestHook = nil })

	if err := retireBoundOpenFileTo(sourceRoot, sinkRoot, "owned", file, true, nil, nil); err != nil {
		t.Fatalf("retiring into anchored sink: %v", err)
	}
	windowsBeforeNativeRenameTestHook = nil
	if !sawAnchor {
		t.Fatal("native retirement seam did not observe the sink anchor")
	}
	directory, err := sinkRoot.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	if entries, err := directory.ReadDir(-1); err != nil || len(entries) != 0 {
		t.Fatalf("retirement sink after exact cleanup = %v, %v; want empty", entries, err)
	}
}

func TestRetireBoundOpenFileWindowsNestedUsesSiblingQuarantine(t *testing.T) {
	root, dir := openWindowsTestRoot(t)
	parentName := filepath.Join("nested", "deep")
	parent := filepath.Join(dir, parentName)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(parentName, "owned")
	file := createWindowsRootFile(t, root, name, "secret")
	injected := errors.New("retain nested quarantine for inspection")
	var quarantine string
	windowsHandlePhaseTestHook = func(phase windowsHandlePhase) error {
		if phase == windowsBeforeRetirementDisposition {
			return injected
		}
		return nil
	}
	t.Cleanup(func() { windowsHandlePhaseTestHook = nil })

	err := retireBoundOpenFile(root, name, file, true, nil, func(retired string) { quarantine = retired })
	windowsHandlePhaseTestHook = nil
	if !errors.Is(err, injected) {
		t.Fatalf("nested retirement error = %v, want injected disposition refusal", err)
	}
	if quarantine == "" {
		t.Fatal("nested retirement did not report its quarantine")
	}
	if got, want := filepath.Dir(quarantine), parentName; got != want {
		t.Fatalf("quarantine parent = %q, want source sibling parent %q", got, want)
	}
	if err := requireBoundNameWindows(root, quarantine, file); err != nil {
		t.Fatalf("sibling quarantine does not name the selected inode: %v", err)
	}
	if got := readWindowsRootFile(t, root, quarantine); got != "secret" {
		t.Fatalf("sibling quarantine = %q, want secret", got)
	}
	if err := removeTrustedRetiredFile(root, quarantine, file, true); err != nil {
		t.Fatalf("cleaning inspected sibling quarantine: %v", err)
	}
}

func TestRetireBoundOpenFileWindowsAncestorLeaseSurvivesDisposition(t *testing.T) {
	root, dir := openWindowsTestRoot(t)
	parent := filepath.Join(dir, "nested")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join("nested", "owned")
	file := createWindowsRootFile(t, root, name, "secret")
	moved := filepath.Join(t.TempDir(), "moved")
	var moveErr error
	windowsHandlePhaseTestHook = func(phase windowsHandlePhase) error {
		if phase != windowsBeforeRetirementDisposition {
			return nil
		}
		moveErr = os.Rename(parent, moved)
		if moveErr == nil {
			return errors.Join(
				errors.New("leased retirement ancestor was movable before disposition"),
				os.Rename(moved, parent),
			)
		}
		return nil
	}
	t.Cleanup(func() { windowsHandlePhaseTestHook = nil })

	if err := retireBoundOpenFile(root, name, file, true, nil, nil); err != nil {
		t.Fatalf("retiring nested file with ancestor lease: %v", err)
	}
	windowsHandlePhaseTestHook = nil
	if moveErr == nil {
		t.Fatal("nested retirement ancestor move unexpectedly succeeded")
	}
	if _, err := root.Lstat(name); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("nested source remains after retirement: %v", err)
	}
}

func TestRenameBoundOpenFileWindowsNoReplacePreservesSentinel(t *testing.T) {
	root, _ := openWindowsTestRoot(t)
	source := createWindowsRootFile(t, root, "source", "new")
	createWindowsRootFile(t, root, "target", "sentinel")

	published, err := renameBoundOpenFile(root, source, nil, "source", "target", false)
	if err == nil || published {
		t.Fatalf("no-replace rename = published %v, %v; want unpublished error", published, err)
	}
	if got := readWindowsRootFile(t, root, "target"); got != "sentinel" {
		t.Fatalf("sentinel = %q", got)
	}
	if got := readWindowsRootFile(t, root, "source"); got != "new" {
		t.Fatalf("source = %q", got)
	}
}

func TestRenameBoundOpenFileWindowsReadOnlySourceUsesExactHandle(t *testing.T) {
	root, dir := openWindowsTestRoot(t)
	source := createWindowsRootFile(t, root, "source", "read only")
	if err := os.Chmod(filepath.Join(dir, "source"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	source, err := root.Open("source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()

	lease, err := acquireWindowsDestinationNamespaceLease(root, "target", "source")
	if err != nil {
		t.Fatalf("leasing read-only source namespace: %v", err)
	}
	defer lease.close()
	mutation, err := openBoundMutationHandleWindows(lease, "source", source)
	if err != nil {
		t.Fatalf("binding read-only source for rename: %v", err)
	}
	defer closeMutationHandleWindows(&mutation)
	if mutation.explicitFlush {
		t.Fatal("read-only source unexpectedly acquired a GENERIC_WRITE mutation handle")
	}
	if err := closeMutationHandleWindows(&mutation); err != nil {
		t.Fatal(err)
	}
	if err := lease.close(); err != nil {
		t.Fatal(err)
	}
	published, err := renameBoundOpenFile(root, source, nil, "source", "target", false)
	if err != nil || !published {
		t.Fatalf("read-only rename = published %v, %v", published, err)
	}
	if _, err := root.Lstat("source"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read-only source remains after rename: %v", err)
	}
	info, err := root.Lstat("target")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("renamed read-only mode = %o, want no write bits", info.Mode().Perm())
	}
	if got := readWindowsRootFile(t, root, "target"); got != "read only" {
		t.Fatalf("renamed read-only content = %q", got)
	}
}

func TestMoveOpenFileNoReplaceWindowsAllowsNestedSource(t *testing.T) {
	root, dir := openWindowsTestRoot(t)
	private := filepath.Join(dir, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(private, "source"), []byte("nested source"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := root.OpenFile(filepath.Join("private", "source"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()

	outcome, err := MoveOpenFileNoReplace(root, source, filepath.Join("private", "source"), "target")
	if err != nil || !outcome.Published {
		t.Fatalf("nested no-replace move = %+v, %v", outcome, err)
	}
	if got := readWindowsRootFile(t, root, "target"); got != "nested source" {
		t.Fatalf("target = %q", got)
	}
	if _, err := root.Lstat(filepath.Join("private", "source")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("nested source remains: %v", err)
	}
}

func TestMoveOpenFileNoReplaceWindowsNestedSourcePreservesCollision(t *testing.T) {
	root, dir := openWindowsTestRoot(t)
	private := filepath.Join(dir, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(private, "source"), []byte("nested source"), 0o600); err != nil {
		t.Fatal(err)
	}
	createWindowsRootFile(t, root, "target", "sentinel")
	source, err := root.OpenFile(filepath.Join("private", "source"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()

	outcome, err := MoveOpenFileNoReplace(root, source, filepath.Join("private", "source"), "target")
	if err == nil || outcome.Published {
		t.Fatalf("nested collision = %+v, %v; want unpublished refusal", outcome, err)
	}
	if got := readWindowsRootFile(t, root, "target"); got != "sentinel" {
		t.Fatalf("target = %q", got)
	}
	if got := readWindowsRootFile(t, root, filepath.Join("private", "source")); got != "nested source" {
		t.Fatalf("source = %q", got)
	}
}

func TestRenameBoundOpenFileWindowsPhaseErrorsPreserveRecoverableState(t *testing.T) {
	injected := errors.New("injected Windows namespace seam failure")
	tests := []struct {
		name       string
		phase      windowsHandlePhase
		wantSource string
		wantTarget string
	}{
		{name: "before source staging flush", phase: windowsBeforeSourceStagingFlush, wantSource: "new", wantTarget: "old"},
		{name: "after source staging flush", phase: windowsAfterSourceStagingFlush, wantSource: "new", wantTarget: "old"},
		{name: "before displaced staging flush", phase: windowsBeforeDisplacedStagingFlush, wantSource: "new", wantTarget: "old"},
		{name: "after displaced staging flush", phase: windowsAfterDisplacedStagingFlush, wantSource: "new", wantTarget: "old"},
		{name: "before publication flush", phase: windowsBeforeSourcePublicationFlush, wantSource: "old", wantTarget: "new"},
		{name: "after publication flush", phase: windowsAfterSourcePublicationFlush, wantSource: "old", wantTarget: "new"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, _ := openWindowsTestRoot(t)
			source := createWindowsRootFile(t, root, "source", "new")
			displaced := createWindowsRootFile(t, root, "target", "old")
			windowsHandlePhaseTestHook = func(phase windowsHandlePhase) error {
				if phase == test.phase {
					return injected
				}
				return nil
			}
			t.Cleanup(func() { windowsHandlePhaseTestHook = nil })

			published, err := renameBoundOpenFile(root, source, displaced, "source", "target", true)
			if !published || !errors.Is(err, injected) {
				t.Fatalf("exchange = published %v, %v; want published injected error", published, err)
			}
			if got := readWindowsRootFile(t, root, "source"); got != test.wantSource {
				t.Fatalf("source = %q, want %q", got, test.wantSource)
			}
			if got := readWindowsRootFile(t, root, "target"); got != test.wantTarget {
				t.Fatalf("target = %q, want %q", got, test.wantTarget)
			}
			if _, err := root.Lstat(restoreExchangeStagingName("source", "target")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("staging name remains after handled phase error: %v", err)
			}
		})
	}
}

func TestRetireBoundOpenFileWindowsBeforeSeamLeasesSourceLink(t *testing.T) {
	root, _ := openWindowsTestRoot(t)
	owned := createWindowsRootFile(t, root, "owned", "secret")
	var moveErr error

	err := retireBoundOpenFile(root, "owned", owned, true, func() {
		moveErr = root.Rename("owned", "moved")
	}, nil)
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if moveErr == nil {
		t.Fatal("source link was movable immediately before retirement rename")
	}
	if _, err := root.Lstat("owned"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("retired source remains: %v", err)
	}
	if _, err := root.Lstat("moved"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("attacker destination appeared: %v", err)
	}
}

func TestRetireBoundOpenFileWindowsAfterSeamLeasesQuarantineLink(t *testing.T) {
	root, _ := openWindowsTestRoot(t)
	owned := createWindowsRootFile(t, root, "owned", "secret")
	var quarantine string
	var moveErr error

	err := retireBoundOpenFile(root, "owned", owned, true, nil, func(name string) {
		quarantine = name
		moveErr = root.Rename(name, "moved")
	})
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if moveErr == nil {
		t.Fatal("quarantine link was movable before exact-handle disposition")
	}
	if _, err := root.Lstat(quarantine); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("quarantine remains after retirement: %v", err)
	}
	if _, err := root.Lstat("moved"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("attacker destination appeared: %v", err)
	}
}

func TestRetireBoundOpenFileWindowsDispositionPhaseErrors(t *testing.T) {
	injected := errors.New("injected Windows disposition seam failure")
	t.Run("before", func(t *testing.T) {
		root, _ := openWindowsTestRoot(t)
		file := createWindowsRootFile(t, root, "owned", "secret")
		var quarantine string
		windowsHandlePhaseTestHook = func(phase windowsHandlePhase) error {
			if phase == windowsBeforeRetirementDisposition {
				return injected
			}
			return nil
		}
		err := retireBoundOpenFile(root, "owned", file, true, nil, func(name string) { quarantine = name })
		windowsHandlePhaseTestHook = nil
		if !errors.Is(err, injected) {
			t.Fatalf("retire error = %v, want injected failure", err)
		}
		if got := readWindowsRootFile(t, root, quarantine); got != "secret" {
			t.Fatalf("recoverable quarantine = %q", got)
		}
		if err := removeTrustedRetiredFile(root, quarantine, file, true); err != nil {
			t.Fatalf("cleaning recoverable quarantine: %v", err)
		}
	})
	t.Run("after", func(t *testing.T) {
		root, _ := openWindowsTestRoot(t)
		file := createWindowsRootFile(t, root, "owned", "secret")
		var quarantine string
		windowsHandlePhaseTestHook = func(phase windowsHandlePhase) error {
			if phase == windowsAfterRetirementDisposition {
				return injected
			}
			return nil
		}
		t.Cleanup(func() { windowsHandlePhaseTestHook = nil })
		err := retireBoundOpenFile(root, "owned", file, true, nil, func(name string) { quarantine = name })
		if !errors.Is(err, injected) {
			t.Fatalf("retire error = %v, want injected failure", err)
		}
		if _, err := root.Lstat(quarantine); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("disposed quarantine remains: %v", err)
		}
	})
}

func TestRetireBoundOpenFileWindowsDoesNotTruncateHardlinks(t *testing.T) {
	root, dir := openWindowsTestRoot(t)
	file := createWindowsRootFile(t, root, "target", "workspace")
	if err := os.Link(filepath.Join(dir, "target"), filepath.Join(dir, "alias")); err != nil {
		t.Fatalf("hard link: %v", err)
	}

	if err := retireBoundOpenFile(root, "target", file, false, nil, nil); err != nil {
		t.Fatalf("retire workspace link: %v", err)
	}
	if _, err := root.Lstat("target"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("target remains after retirement: %v", err)
	}
	if got := readWindowsRootFile(t, root, "alias"); got != "workspace" {
		t.Fatalf("hard-link content = %q", got)
	}
}

func TestRetireBoundOpenFileWindowsOwnedHardlinkFailsWithoutTruncating(t *testing.T) {
	root, dir := openWindowsTestRoot(t)
	file := createWindowsRootFile(t, root, "owned", "secret")
	if err := os.Link(filepath.Join(dir, "owned"), filepath.Join(dir, "alias")); err != nil {
		t.Fatalf("hard link: %v", err)
	}
	var quarantine string

	err := retireBoundOpenFile(root, "owned", file, true, nil, func(name string) { quarantine = name })
	if !errors.Is(err, ErrStale) {
		t.Fatalf("retire error = %v, want ErrStale", err)
	}
	if _, err := root.Lstat("owned"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("original owned link remains after quarantine: %v", err)
	}
	if got := readWindowsRootFile(t, root, quarantine); got != "secret" {
		t.Fatalf("recorded retirement evidence = %q", got)
	}
	if got := readWindowsRootFile(t, root, "alias"); got != "secret" {
		t.Fatalf("hard-link content = %q", got)
	}
}

func TestRetireBoundOpenFileWindowsPostDispositionLinkFence(t *testing.T) {
	root, dir := openWindowsTestRoot(t)
	file := createWindowsRootFile(t, root, "owned", "secret")
	var quarantine string
	windowsHandlePhaseTestHook = func(phase windowsHandlePhase) error {
		if phase != windowsAfterRetirementLinkFence {
			return nil
		}
		windowsHandlePhaseTestHook = nil
		return os.Link(filepath.Join(dir, quarantine), filepath.Join(dir, "racing-alias"))
	}
	t.Cleanup(func() { windowsHandlePhaseTestHook = nil })

	err := retireBoundOpenFile(root, "owned", file, true, nil, func(name string) { quarantine = name })
	if !errors.Is(err, ErrStale) {
		t.Fatalf("retire error = %v, want post-disposition ErrStale", err)
	}
	if _, err := root.Lstat(quarantine); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("disposed quarantine remains: %v", err)
	}
	if got := readWindowsRootFile(t, root, "racing-alias"); got != "secret" {
		t.Fatalf("racing hard-link content = %q", got)
	}
}

func TestRemoveLocalRetiredFileWindowsOwnedHardlinkFailsWithoutTruncating(t *testing.T) {
	root, dir := openWindowsTestRoot(t)
	file := createWindowsRootFile(t, root, "retired", "secret")
	if err := os.Link(filepath.Join(dir, "retired"), filepath.Join(dir, "alias")); err != nil {
		t.Fatalf("hard link: %v", err)
	}

	err := removeLocalRetiredFile(root, "retired", file, true)
	if !errors.Is(err, ErrStale) {
		t.Fatalf("local retirement cleanup error = %v, want ErrStale", err)
	}
	if got := readWindowsRootFile(t, root, "retired"); got != "secret" {
		t.Fatalf("recorded local retirement evidence = %q", got)
	}
	if got := readWindowsRootFile(t, root, "alias"); got != "secret" {
		t.Fatalf("hard-link content = %q", got)
	}
}

func TestRetireBoundOpenFileWindowsMissingNameNeverDisposesTargetLinkedInode(t *testing.T) {
	root, _ := openWindowsTestRoot(t)
	file := createWindowsRootFile(t, root, "owned", "secret")
	if err := root.Rename("owned", "target"); err != nil {
		t.Fatalf("move owned inode onto target: %v", err)
	}
	err := retireBoundOpenFile(root, "owned", file, true, nil, nil)
	if !errors.Is(err, ErrStale) {
		t.Fatalf("missing-name retirement error = %v, want ErrStale", err)
	}
	if got := readWindowsRootFile(t, root, "target"); got != "secret" {
		t.Fatalf("missing-name retirement disposed target-linked inode: %q", got)
	}
}

func TestRetireBoundOpenFileToWindowsUsesDeterministicSink(t *testing.T) {
	sourceRoot, _ := openWindowsTestRoot(t)
	sinkRoot, _ := openWindowsTestRoot(t)
	file := createWindowsRootFile(t, sourceRoot, "owned", "secret")

	if err := retireBoundOpenFileTo(sourceRoot, sinkRoot, "owned", file, true, nil, nil); err != nil {
		t.Fatalf("retire to sink: %v", err)
	}
	if _, err := sourceRoot.Lstat("owned"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("source remains after retirement: %v", err)
	}
	if _, err := sinkRoot.Lstat(retiredSinkName("owned")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("retired sink name remains: %v", err)
	}
}

func TestRetireBoundOpenFileToWindowsPreservesOccupiedSink(t *testing.T) {
	sourceRoot, _ := openWindowsTestRoot(t)
	sinkRoot, _ := openWindowsTestRoot(t)
	file := createWindowsRootFile(t, sourceRoot, "owned", "secret")
	retired := retiredSinkName("owned")
	createWindowsRootFile(t, sinkRoot, retired, "sentinel")

	err := retireBoundOpenFileTo(sourceRoot, sinkRoot, "owned", file, true, nil, nil)
	if !errors.Is(err, ErrStale) {
		t.Fatalf("retire error = %v, want ErrStale", err)
	}
	if got := readWindowsRootFile(t, sourceRoot, "owned"); got != "secret" {
		t.Fatalf("source = %q", got)
	}
	if got := readWindowsRootFile(t, sinkRoot, retired); got != "sentinel" {
		t.Fatalf("sink sentinel = %q", got)
	}
}

func TestFileRenameInformationWindowsLayout(t *testing.T) {
	wantRootOffset := uintptr(4)
	wantOffset := uintptr(12)
	if unsafe.Sizeof(uintptr(0)) == 8 {
		wantRootOffset = 8
		wantOffset = 20
	}
	var info fileRenameInfoWindows
	if got := unsafe.Offsetof(info.ReplaceIfExists); got != 0 {
		t.Fatalf("FILE_RENAME_INFORMATION ReplaceIfExists offset = %d, want 0", got)
	}
	if got := unsafe.Offsetof(info.RootDirectory); got != wantRootOffset {
		t.Fatalf("FILE_RENAME_INFORMATION RootDirectory offset = %d, want %d", got, wantRootOffset)
	}
	if got := unsafe.Offsetof(info.FileName); got != wantOffset {
		t.Fatalf("FILE_RENAME_INFORMATION FileName offset = %d, want %d", got, wantOffset)
	}
}

func TestFileIDInfoWindowsLayout(t *testing.T) {
	var info fileIDInfoWindows
	if got := unsafe.Sizeof(info); got != 24 {
		t.Fatalf("FILE_ID_INFO size = %d, want 24", got)
	}
	if got := unsafe.Offsetof(info.FileID); got != 8 {
		t.Fatalf("FILE_ID_INFO FileId offset = %d, want 8", got)
	}
}

func TestProbeWindowsRetirementPrimitivesLeavesNoArtifacts(t *testing.T) {
	root, _ := openWindowsTestRoot(t)
	if err := probeWindowsRetirementPrimitives(root); err != nil {
		t.Fatalf("retirement capability probe: %v", err)
	}
	directory, err := root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	entries, err := directory.ReadDir(-1)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("capability probe left %d artifact(s): %v", len(entries), entries)
	}
}

func TestDisposeCapabilityProbeOriginalWindowsPreservesReplacement(t *testing.T) {
	root, _ := openWindowsTestRoot(t)
	selected := createWindowsRootFile(t, root, "probe", "selected")
	if err := root.Rename("probe", "moved"); err != nil {
		t.Fatalf("moving selected probe link: %v", err)
	}
	createWindowsRootFile(t, root, "probe", "replacement")

	if err := disposeCapabilityProbeOriginalWindows(windows.Handle(selected.Fd())); err != nil {
		t.Fatalf("disposing selected probe by exact handle: %v", err)
	}
	if _, err := root.Lstat("moved"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("selected probe link remains: %v", err)
	}
	if got := readWindowsRootFile(t, root, "probe"); got != "replacement" {
		t.Fatalf("replacement probe = %q, want replacement", got)
	}
}

func TestStableWindowsFileIDSurvivesRenameAndDistinguishesFiles(t *testing.T) {
	root, _ := openWindowsTestRoot(t)
	first := createWindowsRootFile(t, root, "first", "one")
	second := createWindowsRootFile(t, root, "second", "two")
	firstID, err := stableWindowsFileID(windows.Handle(first.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if firstID.VolumeSerialNumber == 0 || firstID.FileID == ([16]byte{}) {
		t.Fatalf("first file returned an unstable FILE_ID_INFO value: %#v", firstID)
	}
	secondID, err := stableWindowsFileID(windows.Handle(second.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if firstID == secondID {
		t.Fatal("distinct files reported the same FILE_ID_INFO identity")
	}
	if err := root.Rename("first", "renamed"); err != nil {
		t.Fatal(err)
	}
	renamed := openWindowsRootFile(t, root, "renamed")
	renamedID, err := stableWindowsFileID(windows.Handle(renamed.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	if renamedID != firstID {
		t.Fatalf("FILE_ID_INFO changed across rename: %#v -> %#v", firstID, renamedID)
	}
}

func openWindowsTestRoot(t *testing.T) (*os.Root, string) {
	t.Helper()
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root, dir
}

func createWindowsRootFile(t *testing.T, root *os.Root, name, content string) *os.File {
	t.Helper()
	file, err := root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL|os.O_SYNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if _, err := file.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	return file
}

func openWindowsRootFile(t *testing.T, root *os.Root, name string) *os.File {
	t.Helper()
	file, err := root.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func readWindowsRootFile(t *testing.T, root *os.Root, name string) string {
	t.Helper()
	file, err := root.Open(name)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	content, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func openWindowsDirectoryWriteHandle(path string) (windows.Handle, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	return windows.CreateFile(
		name,
		windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
}

func reopenWindowsTestDirectoryDeleteHandle(handle windows.Handle) (windows.Handle, error) {
	result, _, callErr := reOpenFileWindows.Call(
		uintptr(handle),
		uintptr(uint32(windows.DELETE)),
		uintptr(uint32(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)),
		uintptr(uint32(windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT)),
	)
	reopened := windows.Handle(result)
	if reopened == windows.InvalidHandle || reopened == 0 {
		if callErr == nil || callErr == windows.ERROR_SUCCESS {
			callErr = windows.ERROR_INVALID_HANDLE
		}
		return windows.InvalidHandle, callErr
	}
	return reopened, nil
}
