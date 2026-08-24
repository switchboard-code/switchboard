//go:build windows

package checkpoint

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var reOpenFileWindows = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReOpenFile")

// fileRenameInfoWindows is FILE_RENAME_INFO's FileRenameInfoEx layout. The
// first union member is a DWORD for the Ex information class. Go supplies the
// architecture-specific padding before RootDirectory.
type fileRenameInfoWindows struct {
	Flags          uint32
	RootDirectory  windows.Handle
	FileNameLength uint32
	FileName       [1]uint16
}

type fileDispositionInfoExWindows struct {
	Flags uint32
}

type fileDispositionInfoWindows struct {
	DeleteFile byte
}

type fileIDInfoWindows struct {
	VolumeSerialNumber uint64
	FileID             [16]byte
}

type windowsHandlePhase uint8

const (
	windowsBeforeSourceStagingFlush windowsHandlePhase = iota
	windowsAfterSourceStagingFlush
	windowsBeforeDisplacedStagingFlush
	windowsAfterDisplacedStagingFlush
	windowsBeforeSourcePublicationFlush
	windowsAfterSourcePublicationFlush
	windowsBeforeRetirementDisposition
	windowsAfterRetirementLinkFence
	windowsAfterRetirementDisposition
)

var windowsHandlePhaseTestHook func(windowsHandlePhase) error

// windowsBeforeNativeRenameTestHook runs after the FILE_RENAME_INFO buffer is
// complete and immediately before SetFileInformationByHandle. Production
// leaves it nil. Windows-only race tests use it to prove that the ancestor
// leases are already active at the native namespace linearization point.
var windowsBeforeNativeRenameTestHook func(from, to string) error

// windowsNamespaceLease keeps every directory ancestor below the retained
// root open with read sharing only. An attacker that already holds a
// write- or delete-capable handle makes acquisition fail; once acquisition
// succeeds, opening either kind of handle is refused until close. That fences
// both ancestor rename/removal and in-place reparse metadata changes.
// Components are opened one at a time under the retained root with
// OBJ_DONT_REPARSE, so a junction cannot redirect acquisition itself.
type windowsNamespaceLease struct {
	root      *os.File
	ancestors []windows.Handle
}

func acquireWindowsNamespaceLease(root *os.Root, names ...string) (*windowsNamespaceLease, error) {
	if root == nil {
		return nil, errors.New("Windows namespace lease requires a bound root")
	}
	for _, name := range names {
		if err := validateBoundRelativeWindows(name); err != nil {
			return nil, err
		}
	}
	rootFile, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("opening Windows namespace lease root: %w", err)
	}
	lease := &windowsNamespaceLease{root: rootFile}
	fail := func(err error) (*windowsNamespaceLease, error) {
		return nil, errors.Join(err, lease.close())
	}

	// Canonical paths let shared prefixes reuse the exact leased handle. Keep
	// the spelling case-sensitive here: on a case-sensitive NTFS directory two
	// differently cased components may genuinely be distinct, and redundant
	// handles on ordinary NTFS are harmless.
	opened := map[string]windows.Handle{".": windows.Handle(rootFile.Fd())}
	for _, name := range names {
		parent := filepath.Dir(name)
		if parent == "." {
			continue
		}
		current := windows.Handle(rootFile.Fd())
		prefix := ""
		for _, component := range strings.Split(parent, string(filepath.Separator)) {
			if prefix == "" {
				prefix = component
			} else {
				prefix = filepath.Join(prefix, component)
			}
			if handle, ok := opened[prefix]; ok {
				current = handle
				continue
			}
			handle, err := openWindowsNamespaceAncestor(current, component)
			if err != nil {
				return fail(fmt.Errorf("leasing checkpoint namespace ancestor %q: %w", prefix, err))
			}
			lease.ancestors = append(lease.ancestors, handle)
			opened[prefix] = handle
			current = handle
		}
	}
	runtime.KeepAlive(rootFile)
	return lease, nil
}

func openWindowsNamespaceAncestor(parent windows.Handle, component string) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(component)
	if err != nil {
		return windows.InvalidHandle, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		RootDirectory: parent,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	attributes.Length = uint32(unsafe.Sizeof(*attributes))
	var handle windows.Handle
	err = windows.NtCreateFile(
		&handle,
		windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		attributes,
		&windows.IO_STATUS_BLOCK{},
		nil,
		0,
		windows.FILE_SHARE_READ,
		windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	)
	runtime.KeepAlive(objectName)
	if err != nil {
		return windows.InvalidHandle, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return windows.InvalidHandle, errors.Join(err, windows.CloseHandle(handle))
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return windows.InvalidHandle, errors.Join(
			errors.New("checkpoint namespace ancestor is not a directory"),
			windows.CloseHandle(handle),
		)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return windows.InvalidHandle, errors.Join(
			errors.New("checkpoint namespace ancestor is a reparse point"),
			windows.CloseHandle(handle),
		)
	}
	if _, err := stableWindowsFileID(handle); err != nil {
		return windows.InvalidHandle, errors.Join(
			fmt.Errorf("checkpoint namespace ancestor has no stable identity: %w", err),
			windows.CloseHandle(handle),
		)
	}
	return handle, nil
}

func (lease *windowsNamespaceLease) close() error {
	if lease == nil {
		return nil
	}
	var err error
	for i := len(lease.ancestors) - 1; i >= 0; i-- {
		err = errors.Join(err, windows.CloseHandle(lease.ancestors[i]))
	}
	lease.ancestors = nil
	if lease.root != nil {
		err = errors.Join(err, lease.root.Close())
		lease.root = nil
	}
	return err
}

var windowsRetirementCapabilities = struct {
	sync.Mutex
	roots map[fileIDInfoWindows]struct{}
}{roots: make(map[fileIDInfoWindows]struct{})}

type windowsCapabilityProbeFile struct {
	name     string
	opened   *os.File
	mutation windows.Handle
}

// ensureRetirementCompatible proves the Windows namespace primitives on
// private files before a restore is allowed to publish into the workspace.
// The filesystem-name check excludes filesystems whose identity behavior has
// not been verified, while the probe catches old kernels, redirectors, and
// filter drivers that do not implement the Ex information classes faithfully.
func ensureRetirementCompatible(source, sink *os.Root) error {
	if source == nil {
		return errors.New("checkpoint retirement requires a bound source root")
	}
	sourceID, err := windowsRetirementRootID(source)
	if err != nil {
		return fmt.Errorf("checking checkpoint workspace retirement support: %w", err)
	}
	type rootAndID struct {
		root *os.Root
		id   fileIDInfoWindows
	}
	roots := []rootAndID{{root: source, id: sourceID}}
	if sink != nil {
		sinkID, err := windowsRetirementRootID(sink)
		if err != nil {
			return fmt.Errorf("checking checkpoint cleanup-directory support: %w", err)
		}
		if sourceID.VolumeSerialNumber != sinkID.VolumeSerialNumber {
			return errors.New("checkpoint workspace and cleanup directory are on different Windows volumes")
		}
		if sourceID != sinkID {
			roots = append(roots, rootAndID{root: sink, id: sinkID})
		}
	}
	for _, candidate := range roots {
		if err := ensureWindowsRetirementPrimitives(candidate.root, candidate.id); err != nil {
			return err
		}
	}
	return nil
}

func windowsRetirementRootID(root *os.Root) (fileIDInfoWindows, error) {
	directory, err := root.Open(".")
	if err != nil {
		return fileIDInfoWindows{}, err
	}
	defer directory.Close()
	if err := requireStableWindowsFilesystem(directory); err != nil {
		return fileIDInfoWindows{}, err
	}
	return stableWindowsFileID(windows.Handle(directory.Fd()))
}

func ensureWindowsRetirementPrimitives(root *os.Root, id fileIDInfoWindows) error {
	windowsRetirementCapabilities.Lock()
	defer windowsRetirementCapabilities.Unlock()
	if _, ok := windowsRetirementCapabilities.roots[id]; ok {
		return nil
	}
	if err := probeWindowsRetirementPrimitives(root); err != nil {
		return fmt.Errorf("checkpoint retirement is unsupported in this Windows directory: %w", err)
	}
	windowsRetirementCapabilities.roots[id] = struct{}{}
	return nil
}

func probeWindowsRetirementPrimitives(root *os.Root) (err error) {
	var probes []*windowsCapabilityProbeFile
	defer func() {
		var cleanupErr error
		for _, probe := range probes {
			cleanupErr = errors.Join(cleanupErr, cleanupWindowsCapabilityProbe(probe))
		}
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("cleaning private Windows capability probes: %w", cleanupErr))
		}
	}()

	source, err := createWindowsCapabilityProbe(root)
	if err != nil {
		return err
	}
	probes = append(probes, source)
	occupied, err := createWindowsCapabilityProbe(root)
	if err != nil {
		return err
	}
	probes = append(probes, occupied)
	destination, err := unusedQuarantineName(root)
	if err != nil {
		return err
	}
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()

	// A zero Flags field in FileRenameInfoEx is a no-replace request. First
	// prove that an occupied exact destination is retained, then prove that the
	// same call succeeds and flushes when its destination is absent.
	published, collisionErr := renameMutationHandleWindows(
		root, root, source.opened, source.mutation, windows.Handle(directory.Fd()), source.name, occupied.name,
	)
	if published || collisionErr == nil {
		return errors.Join(collisionErr,
			errors.New("FileRenameInfoEx did not refuse an occupied no-replace destination"))
	}
	if err := requireBoundNameWindows(root, source.name, source.opened); err != nil {
		return fmt.Errorf("FileRenameInfoEx changed its source on a no-replace collision: %w", err)
	}
	if err := requireBoundNameWindows(root, occupied.name, occupied.opened); err != nil {
		return fmt.Errorf("FileRenameInfoEx replaced an occupied no-replace destination: %w", err)
	}

	published, renameErr := renameMutationHandleWindows(
		root, root, source.opened, source.mutation, windows.Handle(directory.Fd()), source.name, destination,
	)
	if !published || renameErr != nil {
		return errors.Join(renameErr, errors.New("FileRenameInfoEx no-replace probe failed"))
	}
	source.name = destination
	if err := flushMutationHandleWindows(source.mutation, "Windows retirement capability rename"); err != nil {
		return err
	}
	runtime.KeepAlive(directory)
	if err := requireBoundNameWindows(root, destination, source.opened); err != nil {
		return fmt.Errorf("FileRenameInfoEx published an unexpected inode: %w", err)
	}

	if err := disposeMutationHandleWindows(source.opened, &source.mutation, false); err != nil {
		return fmt.Errorf("FileDispositionInfoEx probe failed: %w", err)
	}
	if _, err := root.Lstat(destination); !errors.Is(err, fs.ErrNotExist) {
		return errors.Join(err, errors.New("FileDispositionInfoEx did not remove its exact link on handle close"))
	}
	if err := disposeMutationHandleWindows(occupied.opened, &occupied.mutation, false); err != nil {
		return fmt.Errorf("cleaning occupied FileDispositionInfoEx probe: %w", err)
	}
	if _, err := root.Lstat(occupied.name); !errors.Is(err, fs.ErrNotExist) {
		return errors.Join(err, errors.New("FileDispositionInfoEx retained an occupied probe link"))
	}
	return nil
}

func createWindowsCapabilityProbe(root *os.Root) (*windowsCapabilityProbeFile, error) {
	directory, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("opening root for private Windows capability probe: %w", err)
	}
	defer directory.Close()
	for range 100 {
		name, err := randomQuarantineName()
		if err != nil {
			return nil, err
		}
		objectName, err := windows.NewNTUnicodeString(name)
		if err != nil {
			return nil, err
		}
		attributes := &windows.OBJECT_ATTRIBUTES{
			RootDirectory: windows.Handle(directory.Fd()),
			ObjectName:    objectName,
			Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
		}
		attributes.Length = uint32(unsafe.Sizeof(*attributes))
		var handle windows.Handle
		err = windows.NtCreateFile(
			&handle,
			windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.DELETE,
			attributes,
			&windows.IO_STATUS_BLOCK{},
			nil,
			windows.FILE_ATTRIBUTE_NORMAL,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			windows.FILE_CREATE,
			windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|
				windows.FILE_OPEN_REPARSE_POINT|windows.FILE_WRITE_THROUGH,
			0,
			0,
		)
		runtime.KeepAlive(directory)
		if status, ok := err.(windows.NTStatus); ok && status == windows.STATUS_OBJECT_NAME_COLLISION {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("creating private Windows capability probe: %w", err)
		}
		opened := os.NewFile(uintptr(handle), name)
		if opened == nil {
			cleanupErr := disposeCapabilityProbeHandleWindows(handle)
			closeErr := windows.CloseHandle(handle)
			return nil, errors.Join(errors.New("wrapping private Windows capability probe handle"), cleanupErr, closeErr)
		}
		probe := &windowsCapabilityProbeFile{
			name:     name,
			opened:   opened,
			mutation: windows.InvalidHandle,
		}
		mutation, reopenErr := reopenMutationHandleWindows(opened)
		if reopenErr != nil {
			cleanupErr := disposeCapabilityProbeHandleWindows(handle)
			closeErr := opened.Close()
			return nil, errors.Join(
				fmt.Errorf("ReOpenFile could not bind DELETE access to private probe %s: %w", name, reopenErr),
				cleanupErr,
				closeErr,
			)
		}
		probe.mutation = mutation
		if syncErr := opened.Sync(); syncErr != nil {
			cleanupErr := cleanupWindowsCapabilityProbe(probe)
			return nil, errors.Join(fmt.Errorf("flushing private Windows capability probe: %w", syncErr), cleanupErr)
		}
		return probe, nil
	}
	return nil, errors.New("could not allocate a private Windows capability probe")
}

// disposeCapabilityProbeHandleWindows is a cleanup path, not a capability
// acceptance path. It may use legacy disposition after the Ex call fails,
// because the private zero-byte probe must not be left behind merely to prove
// that production retirement correctly refuses the legacy filesystem.
func disposeCapabilityProbeHandleWindows(handle windows.Handle) error {
	disposition := fileDispositionInfoExWindows{Flags: windows.FILE_DISPOSITION_DELETE |
		windows.FILE_DISPOSITION_POSIX_SEMANTICS |
		windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE}
	exErr := windows.SetFileInformationByHandle(
		handle,
		windows.FileDispositionInfoEx,
		(*byte)(unsafe.Pointer(&disposition)),
		uint32(unsafe.Sizeof(disposition)),
	)
	if exErr == nil {
		return windows.FlushFileBuffers(handle)
	}
	legacy := fileDispositionInfoWindows{DeleteFile: 1}
	legacyErr := windows.SetFileInformationByHandle(
		handle,
		windows.FileDispositionInfo,
		(*byte)(unsafe.Pointer(&legacy)),
		uint32(unsafe.Sizeof(legacy)),
	)
	if legacyErr != nil {
		return errors.Join(exErr, legacyErr)
	}
	return windows.FlushFileBuffers(handle)
}

func cleanupWindowsCapabilityProbe(probe *windowsCapabilityProbeFile) error {
	if probe == nil {
		return nil
	}
	var cleanupErr error
	if probe.mutation != windows.InvalidHandle {
		if disposeErr := disposeCapabilityProbeHandleWindows(probe.mutation); disposeErr != nil {
			cleanupErr = errors.Join(cleanupErr, disposeErr,
				fmt.Errorf("private probe %s could not be disposed by its exact handle and may remain", probe.name))
		}
		cleanupErr = errors.Join(cleanupErr, closeMutationHandleWindows(&probe.mutation))
	}
	if probe.opened != nil {
		cleanupErr = errors.Join(cleanupErr, probe.opened.Close())
		probe.opened = nil
	}
	return cleanupErr
}

func runWindowsHandlePhase(phase windowsHandlePhase) error {
	if windowsHandlePhaseTestHook == nil {
		return nil
	}
	return windowsHandlePhaseTestHook(phase)
}

// renameBoundOpenFile moves the inode selected by file, not whichever inode
// happens to occupy fromName when the mutation reaches the kernel. Every
// source and destination ancestor is leased below the bound root for the whole
// exchange, including rollback, so a junction or directory rename cannot
// redirect any of the root-relative names.
//
// published is true as soon as SetFileInformationByHandle accepts the rename.
// A later flush or handle-close failure is therefore a published, ambiguous
// outcome which callers must preserve for recovery.
func renameBoundOpenFile(root *os.Root, source, displaced *os.File, fromName, toName string, replace bool) (published bool, err error) {
	if err := validateBoundRelativeWindows(fromName); err != nil {
		return false, fmt.Errorf("invalid checkpoint rename source: %w", err)
	}
	if err := validateBoundRelativeWindows(toName); err != nil {
		return false, fmt.Errorf("invalid checkpoint rename destination: %w", err)
	}
	if root == nil || source == nil {
		return false, errors.New("checkpoint rename requires live root and file handles")
	}
	if replace && displaced == nil {
		return false, errors.New("checkpoint replacement requires the exact displaced file handle")
	}
	if !replace && displaced != nil {
		return false, errors.New("checkpoint no-replace rename received an unexpected displaced file handle")
	}
	if replace && filepath.Dir(fromName) != filepath.Dir(toName) {
		return false, errors.New("checkpoint exchange names must share one parent directory")
	}
	lease, err := acquireWindowsNamespaceLease(root, fromName, toName)
	if err != nil {
		return false, fmt.Errorf("acquiring checkpoint namespace lease: %w", err)
	}
	defer func() { err = errors.Join(err, lease.close()) }()

	sourceMutation, err := reopenMutationHandleWindows(source)
	if err != nil {
		return false, fmt.Errorf("reopening checkpoint file for exact-handle rename: %w", err)
	}
	defer func() { err = errors.Join(err, windows.CloseHandle(sourceMutation)) }()
	var displacedMutation windows.Handle
	if replace {
		displacedMutation, err = reopenMutationHandleWindows(displaced)
		if err != nil {
			return false, fmt.Errorf("reopening displaced checkpoint file for exact-handle exchange: %w", err)
		}
		defer func() { err = errors.Join(err, windows.CloseHandle(displacedMutation)) }()
	}

	directory, err := root.Open(".")
	if err != nil {
		return false, fmt.Errorf("opening bound checkpoint parent: %w", err)
	}
	defer func() { err = errors.Join(err, directory.Close()) }()

	if err := requireBoundNameWindows(root, fromName, source); err != nil {
		return false, err
	}
	if !replace {
		published, renameErr := renameMutationHandleWindows(
			root, root, source, sourceMutation, windows.Handle(directory.Fd()), fromName, toName,
		)
		if !published {
			return false, fmt.Errorf("renaming checkpoint file by handle: %w", renameErr)
		}
		flushErr := flushMutationHandleWindows(sourceMutation, "checkpoint rename")
		runtime.KeepAlive(directory)
		return true, errors.Join(renameErr, flushErr)
	}
	if err := requireBoundNameWindows(root, toName, displaced); err != nil {
		return false, fmt.Errorf("binding displaced checkpoint target: %w", err)
	}
	sameHandles, err := sameWindowsHandle(windows.Handle(source.Fd()), windows.Handle(displaced.Fd()))
	if err != nil {
		return false, fmt.Errorf("comparing checkpoint exchange handles: %w", err)
	}
	if sameHandles {
		return false, fmt.Errorf("%w: checkpoint source and displaced target are the same inode", ErrStale)
	}
	staging := restoreExchangeStagingName(fromName, toName)
	if _, err := root.Lstat(staging); !errors.Is(err, fs.ErrNotExist) {
		return false, errors.Join(err,
			fmt.Errorf("%w: deterministic checkpoint staging name is occupied", ErrStale))
	}
	// Allocate the staging name before the last observations so there is no
	// caller-controlled or fallible pathname work between the checks and the
	// first exact-handle namespace mutation.
	if err := requireBoundNameWindows(root, fromName, source); err != nil {
		return false, err
	}
	if err := requireBoundNameWindows(root, toName, displaced); err != nil {
		return false, fmt.Errorf("rebinding displaced checkpoint target: %w", err)
	}

	// Windows has no rename-exchange primitive. Build the equivalent from
	// three no-replace operations on exact handles. At every intermediate
	// state an injected name makes the next operation fail; no caller-owned
	// inode is ever overwritten or unlinked.
	stepOne, stepOneErr := renameMutationHandleWindows(
		root, root, source, sourceMutation, windows.Handle(directory.Fd()), fromName, staging,
	)
	if !stepOne {
		return false, fmt.Errorf("staging checkpoint source by handle: %w", stepOneErr)
	}
	published = true
	if stepOneErr != nil {
		rollbackErr := rollbackOneRenameWindows(root, source, sourceMutation, windows.Handle(directory.Fd()), staging, fromName)
		return true, finishBoundRenameRollback(stepOneErr, rollbackErr)
	}
	if hookErr := runWindowsHandlePhase(windowsBeforeSourceStagingFlush); hookErr != nil {
		rollbackErr := rollbackOneRenameWindows(root, source, sourceMutation, windows.Handle(directory.Fd()), staging, fromName)
		return true, finishBoundRenameRollback(hookErr, rollbackErr)
	}
	if flushErr := flushMutationHandleWindows(sourceMutation, "staged checkpoint source"); flushErr != nil {
		rollbackErr := rollbackOneRenameWindows(root, source, sourceMutation, windows.Handle(directory.Fd()), staging, fromName)
		return true, finishBoundRenameRollback(flushErr, rollbackErr)
	}
	if hookErr := runWindowsHandlePhase(windowsAfterSourceStagingFlush); hookErr != nil {
		rollbackErr := rollbackOneRenameWindows(root, source, sourceMutation, windows.Handle(directory.Fd()), staging, fromName)
		return true, finishBoundRenameRollback(hookErr, rollbackErr)
	}

	stepTwo, stepTwoErr := renameMutationHandleWindows(
		root, root, displaced, displacedMutation, windows.Handle(directory.Fd()), toName, fromName,
	)
	if !stepTwo {
		rollbackErr := rollbackOneRenameWindows(root, source, sourceMutation, windows.Handle(directory.Fd()), staging, fromName)
		return true, finishBoundRenameRollback(fmt.Errorf("staging displaced checkpoint target by handle: %w", stepTwoErr), rollbackErr)
	}
	if stepTwoErr != nil {
		rollbackErr := rollbackExchangeWindows(root, source, displaced, sourceMutation, displacedMutation,
			windows.Handle(directory.Fd()), staging, fromName, toName)
		return true, finishBoundRenameRollback(stepTwoErr, rollbackErr)
	}
	if hookErr := runWindowsHandlePhase(windowsBeforeDisplacedStagingFlush); hookErr != nil {
		rollbackErr := rollbackExchangeWindows(root, source, displaced, sourceMutation, displacedMutation,
			windows.Handle(directory.Fd()), staging, fromName, toName)
		return true, finishBoundRenameRollback(hookErr, rollbackErr)
	}
	if flushErr := flushMutationHandleWindows(displacedMutation, "staged displaced checkpoint target"); flushErr != nil {
		rollbackErr := rollbackExchangeWindows(root, source, displaced, sourceMutation, displacedMutation,
			windows.Handle(directory.Fd()), staging, fromName, toName)
		return true, finishBoundRenameRollback(flushErr, rollbackErr)
	}
	if hookErr := runWindowsHandlePhase(windowsAfterDisplacedStagingFlush); hookErr != nil {
		rollbackErr := rollbackExchangeWindows(root, source, displaced, sourceMutation, displacedMutation,
			windows.Handle(directory.Fd()), staging, fromName, toName)
		return true, finishBoundRenameRollback(hookErr, rollbackErr)
	}

	stepThree, stepThreeErr := renameMutationHandleWindows(
		root, root, source, sourceMutation, windows.Handle(directory.Fd()), staging, toName,
	)
	if !stepThree {
		rollbackErr := rollbackExchangeWindows(root, source, displaced, sourceMutation, displacedMutation,
			windows.Handle(directory.Fd()), staging, fromName, toName)
		return true, finishBoundRenameRollback(fmt.Errorf("publishing checkpoint source by handle: %w", stepThreeErr), rollbackErr)
	}
	beforeFlushErr := runWindowsHandlePhase(windowsBeforeSourcePublicationFlush)
	if beforeFlushErr != nil {
		runtime.KeepAlive(directory)
		return true, beforeFlushErr
	}
	flushErr := flushMutationHandleWindows(sourceMutation, "published checkpoint source")
	afterFlushErr := runWindowsHandlePhase(windowsAfterSourcePublicationFlush)
	runtime.KeepAlive(directory)
	return true, errors.Join(stepThreeErr, flushErr, afterFlushErr)
}

// retireBoundOpenFile removes sensitive checkpoint state without ever
// deleting a pathname after checking it. It first quarantines the exact open
// inode under the bound directory, makes that rename durable, then requests
// POSIX disposition on the exact handle. A positively owned inode is scrubbed
// only after the quarantine link is gone and its surviving link count is zero.
// Workspace files are never truncated because another hard link may
// legitimately expose their content. If a writer substitutes name or the
// quarantine at either test seam, that writer's inode is retained untouched.
func retireBoundOpenFile(root *os.Root, name string, file *os.File, owned bool, before func(), after func(string)) (err error) {
	if err := validateBoundRelativeWindows(name); err != nil {
		return fmt.Errorf("invalid checkpoint retirement source: %w", err)
	}
	if root == nil || file == nil {
		return errors.New("checkpoint retirement requires live root and file handles")
	}
	lease, err := acquireWindowsNamespaceLease(root, name)
	if err != nil {
		return fmt.Errorf("acquiring checkpoint retirement namespace lease: %w", err)
	}
	defer func() { err = errors.Join(err, lease.close()) }()

	mutation, err := reopenMutationHandleWindows(file)
	if err != nil {
		return fmt.Errorf("reopening checkpoint file for exact-handle retirement: %w", err)
	}
	defer func() { err = errors.Join(err, closeMutationHandleWindows(&mutation)) }()

	linked, linkErr := root.Lstat(name)
	if errors.Is(linkErr, fs.ErrNotExist) {
		return fmt.Errorf("%w: workspace cleanup name disappeared before retirement", ErrStale)
	}
	if linkErr != nil {
		return linkErr
	}
	if !linked.Mode().IsRegular() {
		return fmt.Errorf("%w: checkpoint cleanup name is not a regular file", ErrStale)
	}
	if err := requireBoundNameWindows(root, name, file); err != nil {
		return err
	}

	quarantine, err := unusedQuarantineSibling(root, name)
	if err != nil {
		return err
	}
	directory, err := root.Open(".")
	if err != nil {
		return fmt.Errorf("opening bound checkpoint parent: %w", err)
	}
	defer func() { err = errors.Join(err, directory.Close()) }()

	// This is the final pathname observation. The hook deliberately runs after
	// it: the following operation names the already-open inode, so even a swap
	// at this seam cannot redirect the mutation to the replacement.
	if err := requireBoundNameWindows(root, name, file); err != nil {
		return err
	}
	if before != nil {
		before()
	}
	published, renameErr := renameMutationHandleWindows(
		root, root, file, mutation, windows.Handle(directory.Fd()), name, quarantine,
	)
	if !published {
		return fmt.Errorf("quarantining checkpoint file by handle: %w", renameErr)
	}
	if after != nil {
		after(quarantine)
	}

	flushErr := windows.FlushFileBuffers(mutation)
	runtime.KeepAlive(directory)
	identityErr := requireBoundNameWindows(root, quarantine, file)
	if flushErr != nil || identityErr != nil {
		if flushErr != nil {
			flushErr = fmt.Errorf("flushing checkpoint quarantine rename: %w", flushErr)
		}
		return errors.Join(renameErr, flushErr, identityErr)
	}
	cleanupErr := disposeRetiredMutationWindows(file, &mutation, owned)
	return errors.Join(renameErr, identityErr, cleanupErr)
}

// retireBoundOpenFileTo moves a selected source link into a trusted sink
// directory before scrubbing or disposition. The source and destination are
// both resolved from already-bound roots. Ordinary restores preflight a
// same-volume sink. Direct/unconfigured callers that reach this helper with a
// sink on another volume fall back to a deterministic name under sourceRoot,
// preserving recovery evidence rather than losing the selected link.
func retireBoundOpenFileTo(sourceRoot, sinkRoot *os.Root, name string, file *os.File, owned bool, before func(), after func(string)) (err error) {
	if err := validateBoundRelativeWindows(name); err != nil {
		return fmt.Errorf("invalid checkpoint retirement source: %w", err)
	}
	if sourceRoot == nil || file == nil {
		return errors.New("checkpoint retirement requires live source and file handles")
	}
	if sinkRoot == nil {
		return retireBoundOpenFile(sourceRoot, name, file, owned, before, after)
	}
	sourceLease, err := acquireWindowsNamespaceLease(sourceRoot, name)
	if err != nil {
		return fmt.Errorf("acquiring checkpoint retirement source lease: %w", err)
	}
	defer func() { err = errors.Join(err, sourceLease.close()) }()
	quarantine := retiredSinkName(name)
	sinkLease, err := acquireWindowsNamespaceLease(sinkRoot, quarantine)
	if err != nil {
		return fmt.Errorf("acquiring checkpoint retirement sink lease: %w", err)
	}
	defer func() { err = errors.Join(err, sinkLease.close()) }()

	mutation, err := reopenMutationHandleWindows(file)
	if err != nil {
		return fmt.Errorf("reopening checkpoint file for exact-handle retirement: %w", err)
	}
	defer func() { err = errors.Join(err, closeMutationHandleWindows(&mutation)) }()

	if err := requireBoundNameWindows(sourceRoot, name, file); err != nil {
		return err
	}
	if _, err := sinkRoot.Lstat(quarantine); !errors.Is(err, fs.ErrNotExist) {
		return errors.Join(err,
			fmt.Errorf("%w: deterministic checkpoint retirement name is occupied", ErrStale))
	}
	directory, err := sinkRoot.Open(".")
	if err != nil {
		return fmt.Errorf("opening bound checkpoint retirement sink: %w", err)
	}
	defer func() { err = errors.Join(err, directory.Close()) }()

	// The hook is the final adversarial seam. Everything after it addresses
	// the selected source handle and the bound sink directory handle directly.
	if err := requireBoundNameWindows(sourceRoot, name, file); err != nil {
		return err
	}
	if before != nil {
		before()
	}
	published, renameErr := renameMutationHandleWindows(
		sourceRoot, sinkRoot, file, mutation, windows.Handle(directory.Fd()), name, quarantine,
	)
	retirementRoot := sinkRoot
	retirementDirectory := directory
	if !published && errors.Is(renameErr, windows.ERROR_NOT_SAME_DEVICE) {
		if _, localErr := sourceRoot.Lstat(quarantine); !errors.Is(localErr, fs.ErrNotExist) {
			return errors.Join(renameErr, localErr,
				fmt.Errorf("%w: same-volume checkpoint retirement name is occupied", ErrStale))
		}
		localDirectory, localErr := sourceRoot.Open(".")
		if localErr != nil {
			return errors.Join(renameErr,
				fmt.Errorf("opening same-volume checkpoint retirement directory: %w", localErr))
		}
		defer func() { err = errors.Join(err, localDirectory.Close()) }()
		published, localErr = renameMutationHandleWindows(
			sourceRoot, sourceRoot, file, mutation, windows.Handle(localDirectory.Fd()), name, quarantine,
		)
		if !published {
			return errors.Join(renameErr,
				fmt.Errorf("moving checkpoint file to same-volume retirement name: %w", localErr))
		}
		renameErr = localErr
		retirementRoot = sourceRoot
		retirementDirectory = localDirectory
	}
	if !published {
		return fmt.Errorf("moving checkpoint file to trusted retirement sink: %w", renameErr)
	}
	if after != nil {
		after(quarantine)
	}
	flushErr := flushMutationHandleWindows(mutation, "checkpoint retirement-sink rename")
	runtime.KeepAlive(retirementDirectory)
	identityErr := requireBoundNameWindows(retirementRoot, quarantine, file)
	if flushErr != nil {
		return errors.Join(renameErr, flushErr, identityErr)
	}
	disposeErr := disposeRetiredMutationWindows(file, &mutation, owned)
	return errors.Join(renameErr, identityErr, disposeErr)
}

// removeTrustedRetiredFile disposes an already-quarantined exact inode. The
// pathname is used only to prove which open handle owns the retired name; the
// destructive operation itself is handle-bound.
func removeTrustedRetiredFile(root *os.Root, name string, file *os.File, owned bool) (err error) {
	if root == nil || file == nil {
		return errors.New("trusted checkpoint cleanup requires live root and file handles")
	}
	if err := validateBoundRelativeWindows(name); err != nil {
		return err
	}
	lease, err := acquireWindowsNamespaceLease(root, name)
	if err != nil {
		return fmt.Errorf("acquiring trusted checkpoint cleanup namespace lease: %w", err)
	}
	defer func() { err = errors.Join(err, lease.close()) }()
	if err := requireBoundNameWindows(root, name, file); err != nil {
		return err
	}
	mutation, err := reopenMutationHandleWindows(file)
	if err != nil {
		return fmt.Errorf("reopening trusted checkpoint file for disposition: %w", err)
	}
	defer func() { err = errors.Join(err, closeMutationHandleWindows(&mutation)) }()
	return disposeRetiredMutationWindows(file, &mutation, owned)
}

// removeLocalRetiredFile handles recovery of a deterministic retirement link
// left in the workspace parent. Windows can remove that exact link through its
// open handle, so it has the same race-free implementation as trusted-sink
// cleanup.
func removeLocalRetiredFile(root *os.Root, name string, file *os.File, owned bool) error {
	return removeTrustedRetiredFile(root, name, file, owned)
}

// All Windows namespace names are canonical paths relative to the retained
// workspace root. A replacement exchange additionally requires sibling names
// because its deterministic staging name must share their leased parent.
func validateBoundRelativeWindows(name string) error {
	if name == "" || name == "." || !filepath.IsLocal(name) || filepath.IsAbs(name) || filepath.Clean(name) != name {
		return fmt.Errorf("%q is not a local relative name", name)
	}
	return nil
}

func requireBoundNameWindows(root *os.Root, name string, file *os.File) error {
	same, err := boundNameMatchesWindows(root, name, file)
	if err != nil || !same {
		return errors.Join(err, fmt.Errorf("%w: checkpoint source name changed identity", ErrStale))
	}
	return nil
}

func boundNameMatchesWindows(root *os.Root, name string, file *os.File) (bool, error) {
	opened, err := file.Stat()
	if err != nil {
		return false, err
	}
	linked, err := root.Lstat(name)
	if err != nil || !linked.Mode().IsRegular() || !opened.Mode().IsRegular() {
		return false, err
	}
	linkedFile, err := openCheckpointRootRead(root, name)
	if err != nil {
		return false, err
	}
	defer linkedFile.Close()
	same, err := sameWindowsHandle(windows.Handle(file.Fd()), windows.Handle(linkedFile.Fd()))
	return same, err
}

func reopenMutationHandleWindows(file *os.File) (windows.Handle, error) {
	original := windows.Handle(file.Fd())
	result, _, callErr := reOpenFileWindows.Call(
		uintptr(original),
		uintptr(uint32(windows.GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES|windows.DELETE)),
		uintptr(uint32(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)),
		uintptr(uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_WRITE_THROUGH)),
	)
	runtime.KeepAlive(file)
	handle := windows.Handle(result)
	if handle == windows.InvalidHandle {
		if callErr == nil || callErr == windows.ERROR_SUCCESS {
			callErr = windows.ERROR_INVALID_HANDLE
		}
		return windows.InvalidHandle, callErr
	}
	if err := requireSameWindowsHandle(original, handle); err != nil {
		return windows.InvalidHandle, errors.Join(err, windows.CloseHandle(handle))
	}
	return handle, nil
}

func requireSameWindowsHandle(left, right windows.Handle) error {
	same, err := sameWindowsHandle(left, right)
	if err != nil {
		return err
	}
	if !same {
		return fmt.Errorf("%w: ReOpenFile returned a different checkpoint inode", ErrStale)
	}
	return nil
}

func sameWindowsHandle(left, right windows.Handle) (bool, error) {
	leftInfo, err := stableWindowsFileID(left)
	if err != nil {
		return false, err
	}
	rightInfo, err := stableWindowsFileID(right)
	if err != nil {
		return false, err
	}
	return leftInfo == rightInfo, nil
}

func stableWindowsFileID(file windows.Handle) (fileIDInfoWindows, error) {
	var info fileIDInfoWindows
	if err := windows.GetFileInformationByHandleEx(
		file,
		windows.FileIdInfo,
		(*byte)(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		return fileIDInfoWindows{}, err
	}
	if info.VolumeSerialNumber == 0 {
		return fileIDInfoWindows{}, errors.New("Windows file identity has no stable volume serial number")
	}
	if info.FileID == ([16]byte{}) {
		return fileIDInfoWindows{}, errors.New("Windows filesystem returned an empty 128-bit file identity")
	}
	return info, nil
}

func renameMutationHandleWindows(sourceRoot, destinationRoot *os.Root, opened *os.File, file, directory windows.Handle, from, to string) (bool, error) {
	encoded, err := windows.UTF16FromString(to)
	if err != nil {
		return false, err
	}
	encoded = encoded[:len(encoded)-1]
	var layout fileRenameInfoWindows
	offset := unsafe.Offsetof(layout.FileName)
	length := uintptr(len(encoded)) * unsafe.Sizeof(uint16(0))
	if offset+length > uintptr(^uint32(0)) {
		return false, errors.New("checkpoint rename destination is too long")
	}
	buffer := make([]byte, offset+length)
	info := (*fileRenameInfoWindows)(unsafe.Pointer(&buffer[0]))
	info.RootDirectory = directory
	info.FileNameLength = uint32(length)
	copy(unsafe.Slice(&info.FileName[0], len(encoded)), encoded)
	if windowsBeforeNativeRenameTestHook != nil {
		if hookErr := windowsBeforeNativeRenameTestHook(from, to); hookErr != nil {
			return false, hookErr
		}
	}
	renameErr := windows.SetFileInformationByHandle(file, windows.FileRenameInfoEx, &buffer[0], uint32(len(buffer)))
	if renameErr == nil {
		return true, nil
	}
	return classifyRenameFailureWindows(sourceRoot, destinationRoot, opened, from, to, renameErr)
}

func flushMutationHandleWindows(file windows.Handle, operation string) error {
	if err := windows.FlushFileBuffers(file); err != nil {
		return fmt.Errorf("flushing %s: %w", operation, err)
	}
	return nil
}

func rollbackOneRenameWindows(root *os.Root, opened *os.File, file, directory windows.Handle, from, to string) error {
	published, renameErr := renameMutationHandleWindows(root, root, opened, file, directory, from, to)
	if !published {
		return fmt.Errorf("rolling back checkpoint namespace: %w", renameErr)
	}
	return errors.Join(renameErr, flushMutationHandleWindows(file, "checkpoint namespace rollback"))
}

func rollbackExchangeWindows(root *os.Root, source, displaced *os.File, sourceHandle, displacedHandle, directory windows.Handle, staging, from, to string) error {
	displacedRestored, displacedErr := renameMutationHandleWindows(
		root, root, displaced, displacedHandle, directory, from, to,
	)
	if !displacedRestored {
		return fmt.Errorf("rolling back displaced checkpoint target: %w", displacedErr)
	}
	displacedFlushErr := flushMutationHandleWindows(displacedHandle, "displaced checkpoint rollback")
	sourceRestored, sourceErr := renameMutationHandleWindows(
		root, root, source, sourceHandle, directory, staging, from,
	)
	if !sourceRestored {
		return errors.Join(displacedErr, displacedFlushErr,
			fmt.Errorf("rolling back checkpoint source: %w", sourceErr))
	}
	return errors.Join(displacedErr, displacedFlushErr, sourceErr,
		flushMutationHandleWindows(sourceHandle, "checkpoint source rollback"))
}

// A namespace API can report an operational error after a remote or filter
// driver has made the rename visible. Prove the unpublished case from the two
// bound names; every other state is conservatively classified as published so
// callers retain their recovery evidence.
func classifyRenameFailureWindows(sourceRoot, destinationRoot *os.Root, opened *os.File, from, to string, renameErr error) (bool, error) {
	destinationSame, destinationErr := boundNameMatchesWindows(destinationRoot, to, opened)
	if destinationSame {
		return true, errors.Join(ErrDurableUndoRecoveryRequired, renameErr,
			errors.New("checkpoint rename became visible despite the reported failure"))
	}
	sourceSame, sourceErr := boundNameMatchesWindows(sourceRoot, from, opened)
	if sourceSame &&
		(destinationErr == nil || errors.Is(destinationErr, fs.ErrNotExist)) {
		return false, renameErr
	}
	return true, errors.Join(ErrDurableUndoRecoveryRequired, renameErr, sourceErr, destinationErr,
		errors.New("checkpoint rename outcome is ambiguous"))
}

func disposeRetiredMutationWindows(opened *os.File, file *windows.Handle, scrub bool) error {
	if err := runWindowsHandlePhase(windowsBeforeRetirementDisposition); err != nil {
		return err
	}
	if err := disposeMutationHandleWindows(opened, file, scrub); err != nil {
		return err
	}
	return runWindowsHandlePhase(windowsAfterRetirementDisposition)
}

func disposeMutationHandleWindows(opened *os.File, file *windows.Handle, scrub bool) error {
	if opened == nil || file == nil || *file == windows.InvalidHandle {
		return errors.New("checkpoint disposition requires live exact handles")
	}
	if scrub {
		links, err := windowsHandleLinkCount(opened)
		if err != nil {
			return fmt.Errorf("checking checkpoint hardlinks before disposition: %w", err)
		}
		if links != 1 {
			return fmt.Errorf("%w: owned checkpoint inode has %d links; retaining its recorded retirement name and refusing to scrub external aliases",
				ErrStale, links)
		}
		if err := runWindowsHandlePhase(windowsAfterRetirementLinkFence); err != nil {
			return err
		}
	}
	disposition := fileDispositionInfoExWindows{Flags: windows.FILE_DISPOSITION_DELETE |
		windows.FILE_DISPOSITION_POSIX_SEMANTICS |
		windows.FILE_DISPOSITION_FORCE_IMAGE_SECTION_CHECK |
		windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE}
	if err := windows.SetFileInformationByHandle(
		*file,
		windows.FileDispositionInfoEx,
		(*byte)(unsafe.Pointer(&disposition)),
		uint32(unsafe.Sizeof(disposition)),
	); err != nil {
		return fmt.Errorf("disposing checkpoint file by handle: %w", err)
	}
	if err := windows.FlushFileBuffers(*file); err != nil {
		return fmt.Errorf("flushing checkpoint disposition: %w", err)
	}
	// POSIX disposition removes the link when the disposition handle closes,
	// while opened keeps the exact inode alive for the post-unlink hardlink
	// check and (when safe) scrubbing.
	if err := closeMutationHandleWindows(file); err != nil {
		return fmt.Errorf("closing checkpoint disposition handle: %w", err)
	}
	if !scrub {
		return nil
	}
	// Disposition removes the trusted quarantine link before the link-count
	// decision. If it was the last name, no adversary can create a new hardlink
	// in the gap before truncation. If another link remains, preserve its data:
	// ownership of the temporary name is not ownership of every hardlink.
	links, err := windowsHandleLinkCount(opened)
	if err != nil {
		return fmt.Errorf("checking disposed checkpoint hardlinks: %w", err)
	}
	if links != 0 {
		return fmt.Errorf("%w: disposed checkpoint inode still has %d external hardlink(s); content was not scrubbed",
			ErrStale, links)
	}
	if err := opened.Truncate(0); err != nil {
		return fmt.Errorf("scrubbing disposed checkpoint file: %w", err)
	}
	if err := opened.Sync(); err != nil {
		return fmt.Errorf("flushing scrubbed checkpoint file: %w", err)
	}
	return nil
}

func windowsHandleLinkCount(file *os.File) (uint32, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return 0, err
	}
	return info.NumberOfLinks, nil
}

func closeMutationHandleWindows(file *windows.Handle) error {
	if file == nil || *file == windows.InvalidHandle {
		return nil
	}
	if err := windows.CloseHandle(*file); err != nil {
		return err
	}
	*file = windows.InvalidHandle
	return nil
}
