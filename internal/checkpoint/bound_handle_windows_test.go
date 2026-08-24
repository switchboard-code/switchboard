//go:build windows

package checkpoint

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
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

func TestRenameBoundOpenFileWindowsLeasesBothNoReplaceParentChains(t *testing.T) {
	root, dir := openWindowsTestRoot(t)
	for _, parent := range []string{filepath.Join("left", "deep"), filepath.Join("right", "deep")} {
		if err := os.MkdirAll(filepath.Join(dir, parent), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	sourceName := filepath.Join("left", "deep", "source")
	targetName := filepath.Join("right", "deep", "target")
	source := createWindowsRootFile(t, root, sourceName, "new")
	outside := t.TempDir()
	ancestors := []string{
		filepath.Join(dir, "left", "deep"),
		filepath.Join(dir, "left"),
		filepath.Join(dir, "right", "deep"),
		filepath.Join(dir, "right"),
	}
	var moveErrors []error
	windowsBeforeNativeRenameTestHook = func(_, _ string) error {
		for _, ancestor := range ancestors {
			destination := filepath.Join(outside, filepath.Base(ancestor))
			moveErr := os.Rename(ancestor, destination)
			moveErrors = append(moveErrors, moveErr)
			if moveErr == nil {
				return errors.Join(
					errors.New("leased source or destination ancestor was movable"),
					os.Rename(destination, ancestor),
				)
			}
		}
		return nil
	}
	t.Cleanup(func() { windowsBeforeNativeRenameTestHook = nil })

	published, err := renameBoundOpenFile(root, source, nil, sourceName, targetName, false)
	windowsBeforeNativeRenameTestHook = nil
	if err != nil || !published {
		t.Fatalf("cross-parent no-replace = published %v, %v", published, err)
	}
	if len(moveErrors) != len(ancestors) {
		t.Fatalf("attempted %d ancestor moves, want %d", len(moveErrors), len(ancestors))
	}
	for i, moveErr := range moveErrors {
		if moveErr == nil {
			t.Fatalf("ancestor move %d unexpectedly succeeded", i+1)
		}
	}
	if got := readWindowsRootFile(t, root, targetName); got != "new" {
		t.Fatalf("target = %q, want new", got)
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

func TestRetireBoundOpenFileWindowsBeforeSeamPreservesReplacement(t *testing.T) {
	root, _ := openWindowsTestRoot(t)
	owned := createWindowsRootFile(t, root, "owned", "secret")

	err := retireBoundOpenFile(root, "owned", owned, true, func() {
		if err := root.Rename("owned", "moved"); err != nil {
			t.Fatalf("move owned inode: %v", err)
		}
		createWindowsRootFile(t, root, "owned", "sentinel")
	}, nil)
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if got := readWindowsRootFile(t, root, "owned"); got != "sentinel" {
		t.Fatalf("replacement = %q", got)
	}
	if _, err := root.Lstat("moved"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("selected inode remains at moved: %v", err)
	}
}

func TestRetireBoundOpenFileWindowsAfterSeamPreservesReplacement(t *testing.T) {
	root, _ := openWindowsTestRoot(t)
	owned := createWindowsRootFile(t, root, "owned", "secret")
	var quarantine string

	err := retireBoundOpenFile(root, "owned", owned, true, nil, func(name string) {
		quarantine = name
		if err := root.Rename(name, "moved"); err != nil {
			t.Fatalf("move quarantined inode: %v", err)
		}
		createWindowsRootFile(t, root, name, "sentinel")
	})
	if !errors.Is(err, ErrStale) {
		t.Fatalf("retire error = %v, want ErrStale", err)
	}
	if got := readWindowsRootFile(t, root, quarantine); got != "sentinel" {
		t.Fatalf("quarantine replacement = %q", got)
	}
	if _, err := root.Lstat("moved"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("selected inode remains at moved: %v", err)
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
