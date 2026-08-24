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

// fileRenameInfoWindows is FILE_RENAME_INFORMATION's legacy no-replace
// layout. FileRenameInformation interprets the first union member as a
// BOOLEAN; the DWORD Flags member belongs to FileRenameInformationEx. Keeping
// this byte zero is the kernel-enforced no-replace guarantee.
type fileRenameInfoWindows struct {
	ReplaceIfExists byte
	RootDirectory   windows.Handle
	FileNameLength  uint32
	FileName        [1]uint16
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

type fileBasicInfoWindows struct {
	CreationTime, LastAccessTime int64
	LastWriteTime, ChangeTime    int64
	FileAttributes               uint32
	_                            uint32
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
// root open through an exact handle. Source-only ancestors use read sharing
// only, fencing both rename/removal and in-place reparse metadata changes. A
// nested destination's final parent additionally shares writes because
// FileRenameInformation reopens that directory for FILE_ADD_FILE; it still
// refuses delete sharing, so the selected directory cannot be moved or
// removed. Components are opened one at a time under the retained root with
// OBJ_DONT_REPARSE, so a junction cannot redirect acquisition itself.
type windowsNamespaceLease struct {
	root        *os.File
	ancestors   []windows.Handle
	directories map[string]windows.Handle
}

// windowsNamespaceAnchor keeps a separate retirement sink nonempty while
// FileRenameInformation derives and reopens its destination directory by
// pathname. NTFS refuses to install a directory reparse point on a nonempty
// directory; the anchor's no-delete share mode prevents an attacker from
// removing or moving this exact link until the cross-root mutation finishes.
type windowsNamespaceAnchor struct {
	name   string
	handle windows.Handle
}

func createWindowsNamespaceAnchor(root *os.Root) (*windowsNamespaceAnchor, error) {
	if root == nil {
		return nil, errors.New("Windows namespace anchor requires a bound root")
	}
	directory, err := root.Open(".")
	if err != nil {
		return nil, fmt.Errorf("opening Windows namespace anchor root: %w", err)
	}
	closeDirectory := func(err error) error {
		return errors.Join(err, directory.Close())
	}
	for range 100 {
		name, err := randomQuarantineName()
		if err != nil {
			return nil, closeDirectory(err)
		}
		objectName, err := windows.NewNTUnicodeString(name)
		if err != nil {
			return nil, closeDirectory(err)
		}
		attributes := &windows.OBJECT_ATTRIBUTES{
			RootDirectory: windows.Handle(directory.Fd()),
			ObjectName:    objectName,
			Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
		}
		attributes.Length = uint32(unsafe.Sizeof(*attributes))
		handle := windows.InvalidHandle
		err = windows.NtCreateFile(
			&handle,
			windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE|windows.DELETE,
			attributes,
			&windows.IO_STATUS_BLOCK{},
			nil,
			windows.FILE_ATTRIBUTE_NORMAL,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
			windows.FILE_CREATE,
			windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|
				windows.FILE_OPEN_REPARSE_POINT|windows.FILE_WRITE_THROUGH|windows.FILE_DELETE_ON_CLOSE,
			0,
			0,
		)
		runtime.KeepAlive(objectName)
		runtime.KeepAlive(directory)
		if status, ok := err.(windows.NTStatus); ok && status == windows.STATUS_OBJECT_NAME_COLLISION {
			continue
		}
		if err != nil {
			return nil, closeDirectory(fmt.Errorf("creating private Windows namespace anchor: %w", err))
		}
		anchor := &windowsNamespaceAnchor{name: name, handle: handle}
		if closeErr := directory.Close(); closeErr != nil {
			return nil, errors.Join(closeErr, anchor.close())
		}
		return anchor, nil
	}
	return nil, closeDirectory(errors.New("could not allocate a private Windows namespace anchor"))
}

func (anchor *windowsNamespaceAnchor) close() error {
	if anchor == nil || anchor.handle == windows.InvalidHandle || anchor.handle == 0 {
		return nil
	}
	handle := anchor.handle
	anchor.handle = windows.InvalidHandle
	// FILE_DELETE_ON_CLOSE makes cleanup part of the kernel handle lifetime:
	// normal close and abrupt process termination both retire the exact anchor
	// link, with no pathname lookup and no unjournaled crash debris.
	return windows.CloseHandle(handle)
}

func acquireWindowsNamespaceLease(root *os.Root, names ...string) (*windowsNamespaceLease, error) {
	return acquireWindowsNamespaceLeaseForDestination(root, "", names...)
}

// acquireWindowsDestinationNamespaceLease gives the exact destination parent
// the write sharing required by FileRenameInformation without weakening any
// source-only ancestor. A nested destination is therefore permitted only when
// every named file is its sibling; cross-parent moves may target only the
// already-bound root.
func acquireWindowsDestinationNamespaceLease(root *os.Root, destination string, names ...string) (*windowsNamespaceLease, error) {
	if err := validateBoundRelativeWindows(destination); err != nil {
		return nil, err
	}
	destinationParent := filepath.Dir(destination)
	allNames := make([]string, 0, len(names)+1)
	allNames = append(allNames, names...)
	allNames = append(allNames, destination)
	if destinationParent != "." {
		for _, name := range allNames {
			if err := validateBoundRelativeWindows(name); err != nil {
				return nil, err
			}
			if filepath.Dir(name) != destinationParent {
				return nil, errors.New("secure Windows destination lease requires sibling names or a destination in the bound root")
			}
		}
	}
	return acquireWindowsNamespaceLeaseForDestination(root, destinationParent, allNames...)
}

func acquireWindowsNamespaceLeaseForDestination(root *os.Root, destinationParent string, names ...string) (*windowsNamespaceLease, error) {
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
	rootHandle := windows.Handle(rootFile.Fd())
	// ReOpenFile only accepts an original handle created by CreateFile, while
	// root.Open returns an NtCreateFile handle. NtCreateFile's empty relative
	// object name denotes the RootDirectory handle itself (the same mapping Go
	// uses for os.Root's "." operations), so it can narrow sharing without a
	// pathname lookup. For a cross-root sink this runs after its anchor is
	// created, so a reparse tag installed while the directory was empty is
	// rejected and a later installation is blocked by the retained anchor entry.
	leasedRoot, err := reopenWindowsNamespaceRoot(rootHandle, destinationParent == ".")
	if err != nil {
		return fail(fmt.Errorf("leasing checkpoint namespace root: %w", err))
	}
	if err := requireSameWindowsHandle(rootHandle, leasedRoot); err != nil {
		return fail(errors.Join(err, windows.CloseHandle(leasedRoot)))
	}
	lease.ancestors = append(lease.ancestors, leasedRoot)

	// Canonical paths let shared prefixes reuse the exact leased handle. Keep
	// the spelling case-sensitive here: on a case-sensitive NTFS directory two
	// differently cased components may genuinely be distinct, and redundant
	// handles on ordinary NTFS are harmless.
	opened := map[string]windows.Handle{".": leasedRoot}
	lease.directories = opened
	for _, name := range names {
		parent := filepath.Dir(name)
		if parent == "." {
			continue
		}
		current := leasedRoot
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
			handle, err := openWindowsNamespaceAncestor(current, component, prefix == destinationParent)
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

// directory returns the already-leased exact parent of name. When it is used
// as FileRenameInformation's RootDirectory, the object manager may reopen the
// directory; the nonzero handle remains the authority for that relative open.
func (lease *windowsNamespaceLease) directory(name string) (windows.Handle, error) {
	if lease == nil || lease.root == nil {
		return windows.InvalidHandle, errors.New("Windows namespace lease is closed")
	}
	parent := filepath.Dir(name)
	handle, ok := lease.directories[parent]
	if !ok {
		return windows.InvalidHandle, fmt.Errorf("checkpoint parent %q was not leased", parent)
	}
	if handle == 0 || handle == windows.InvalidHandle {
		return windows.InvalidHandle, fmt.Errorf("checkpoint parent %q has no live exact handle", parent)
	}
	return handle, nil
}

func reopenWindowsNamespaceRoot(root windows.Handle, shareWrites bool) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString("")
	if err != nil {
		return windows.InvalidHandle, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		RootDirectory: root,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
	}
	attributes.Length = uint32(unsafe.Sizeof(*attributes))
	share := uint32(windows.FILE_SHARE_READ)
	if shareWrites {
		share |= windows.FILE_SHARE_WRITE
	}
	var handle windows.Handle
	err = windows.NtCreateFile(
		&handle,
		windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		attributes,
		&windows.IO_STATUS_BLOCK{},
		nil,
		0,
		share,
		windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT,
		0,
		0,
	)
	runtime.KeepAlive(objectName)
	if status, ok := err.(windows.NTStatus); ok {
		err = status.Errno()
	}
	if err != nil {
		return windows.InvalidHandle, err
	}
	if err := validateWindowsNamespaceDirectory(handle); err != nil {
		return windows.InvalidHandle, errors.Join(err, windows.CloseHandle(handle))
	}
	return handle, nil
}

func openWindowsNamespaceAncestor(parent windows.Handle, component string, shareWrites bool) (windows.Handle, error) {
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
	share := uint32(windows.FILE_SHARE_READ)
	if shareWrites {
		share |= windows.FILE_SHARE_WRITE
	}
	var handle windows.Handle
	err = windows.NtCreateFile(
		&handle,
		windows.FILE_TRAVERSE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		attributes,
		&windows.IO_STATUS_BLOCK{},
		nil,
		0,
		share,
		windows.FILE_OPEN,
		windows.FILE_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	)
	runtime.KeepAlive(objectName)
	if err != nil {
		return windows.InvalidHandle, err
	}
	if err := validateWindowsNamespaceDirectory(handle); err != nil {
		return windows.InvalidHandle, errors.Join(err, windows.CloseHandle(handle))
	}
	return handle, nil
}

func validateWindowsNamespaceDirectory(handle windows.Handle) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return errors.New("checkpoint namespace ancestor is not a directory")
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errors.New("checkpoint namespace ancestor is a reparse point")
	}
	if _, err := stableWindowsFileID(handle); err != nil {
		return fmt.Errorf("checkpoint namespace ancestor has no stable identity: %w", err)
	}
	return nil
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
	lease.directories = nil
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
	mutation windowsMutationHandle
	disposed bool
}

// windowsMutationHandle distinguishes a normal write-capable mutation handle
// from the minimal DELETE handle needed for a read-only inode. Both are opened
// with FILE_FLAG_WRITE_THROUGH. The former also gets an explicit
// FlushFileBuffers barrier; the latter cannot legally request GENERIC_WRITE
// while FILE_ATTRIBUTE_READONLY is set, so completion of its synchronous
// write-through namespace operation is the barrier.
type windowsMutationHandle struct {
	handle        windows.Handle
	original      windows.Handle
	explicitFlush bool
}

func invalidWindowsMutationHandle() windowsMutationHandle {
	return windowsMutationHandle{handle: windows.InvalidHandle}
}

func (handle windowsMutationHandle) valid() bool {
	return handle.handle != windows.InvalidHandle && handle.handle != 0
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

	// A false ReplaceIfExists field in FileRenameInformation is a no-replace
	// request. First prove that an occupied exact destination is retained, then
	// prove that the same call succeeds and flushes when its destination is
	// absent. Drop the occupied file's DELETE lease for the collision attempt:
	// its retained read/write handle shares deletion, so only the kernel's
	// no-replace decision can refuse the rename. Keeping the mutation handle
	// open here would turn a broken replace request into a sharing violation and
	// let the probe pass for the wrong reason.
	if err := closeMutationHandleWindows(&occupied.mutation); err != nil {
		return fmt.Errorf("releasing occupied no-replace probe lease: %w", err)
	}
	published, collisionErr := renameMutationHandleWindows(
		root, root, source.opened, source.mutation, windows.Handle(directory.Fd()), source.name, occupied.name,
	)
	if published || collisionErr == nil {
		return errors.Join(collisionErr,
			errors.New("FileRenameInformation did not refuse an occupied no-replace destination"))
	}
	if err := requireBoundNameWindows(root, source.name, source.opened); err != nil {
		return fmt.Errorf("FileRenameInformation changed its source on a no-replace collision: %w", err)
	}
	if err := requireBoundNameWindows(root, occupied.name, occupied.opened); err != nil {
		return fmt.Errorf("FileRenameInformation replaced an occupied no-replace destination: %w", err)
	}
	occupied.mutation, err = reopenMutationHandleWindows(occupied.opened)
	if err != nil {
		return fmt.Errorf("rebinding occupied FileDispositionInfoEx probe: %w", err)
	}

	published, renameErr := renameMutationHandleWindows(
		root, root, source.opened, source.mutation, windows.Handle(directory.Fd()), source.name, destination,
	)
	if !published || renameErr != nil {
		return errors.Join(renameErr, errors.New("FileRenameInformation no-replace probe failed"))
	}
	source.name = destination
	if err := flushMutationHandleWindows(source.mutation, "Windows retirement capability rename"); err != nil {
		return err
	}
	runtime.KeepAlive(directory)
	if err := requireBoundNameWindows(root, destination, source.opened); err != nil {
		return fmt.Errorf("FileRenameInformation published an unexpected inode: %w", err)
	}

	if err := disposeMutationHandleWindows(source.opened, &source.mutation, false); err != nil {
		return fmt.Errorf("FileDispositionInfoEx probe failed: %w", err)
	}
	source.disposed = true
	if _, err := root.Lstat(destination); !errors.Is(err, fs.ErrNotExist) {
		return errors.Join(err, errors.New("FileDispositionInfoEx did not remove its exact link on handle close"))
	}
	if err := disposeMutationHandleWindows(occupied.opened, &occupied.mutation, false); err != nil {
		return fmt.Errorf("cleaning occupied FileDispositionInfoEx probe: %w", err)
	}
	occupied.disposed = true
	if _, err := root.Lstat(occupied.name); !errors.Is(err, fs.ErrNotExist) {
		return errors.Join(err, errors.New("FileDispositionInfoEx retained an occupied probe link"))
	}

	// A read-only file cannot be reopened with GENERIC_WRITE, but DELETE access
	// remains sufficient for an exact-handle rename and POSIX disposition. Pin
	// that fallback against the live filesystem before accepting the root.
	readOnly, err := createWindowsCapabilityProbe(root)
	if err != nil {
		return err
	}
	probes = append(probes, readOnly)
	if err := closeMutationHandleWindows(&readOnly.mutation); err != nil {
		return fmt.Errorf("closing writable read-only capability probe handle: %w", err)
	}
	var basic fileBasicInfoWindows
	if err := windows.GetFileInformationByHandleEx(
		windows.Handle(readOnly.opened.Fd()), windows.FileBasicInfo,
		(*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic)),
	); err != nil {
		return fmt.Errorf("reading Windows capability probe attributes: %w", err)
	}
	basic.FileAttributes &^= windows.FILE_ATTRIBUTE_NORMAL
	basic.FileAttributes |= windows.FILE_ATTRIBUTE_READONLY
	if err := windows.SetFileInformationByHandle(
		windows.Handle(readOnly.opened.Fd()), windows.FileBasicInfo,
		(*byte)(unsafe.Pointer(&basic)), uint32(unsafe.Sizeof(basic)),
	); err != nil {
		return fmt.Errorf("marking Windows capability probe read-only: %w", err)
	}
	readOnly.mutation, err = reopenMutationHandleWindows(readOnly.opened)
	if err != nil {
		return fmt.Errorf("binding read-only Windows capability probe: %w", err)
	}
	readOnlyDestination, err := unusedQuarantineName(root)
	if err != nil {
		return err
	}
	published, renameErr = renameMutationHandleWindows(
		root, root, readOnly.opened, readOnly.mutation, windows.Handle(directory.Fd()), readOnly.name, readOnlyDestination,
	)
	if !published || renameErr != nil {
		return errors.Join(renameErr, errors.New("read-only FileRenameInformation no-replace probe failed"))
	}
	readOnly.name = readOnlyDestination
	if err := flushMutationHandleWindows(readOnly.mutation, "read-only Windows retirement capability rename"); err != nil {
		return err
	}
	if err := disposeMutationHandleWindows(readOnly.opened, &readOnly.mutation, false); err != nil {
		return fmt.Errorf("read-only FileDispositionInfoEx probe failed: %w", err)
	}
	readOnly.disposed = true
	if _, err := root.Lstat(readOnlyDestination); !errors.Is(err, fs.ErrNotExist) {
		return errors.Join(err, errors.New("read-only FileDispositionInfoEx retained its probe link"))
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
			windows.FILE_GENERIC_READ|windows.FILE_GENERIC_WRITE,
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
			cleanupErr := disposeCapabilityProbeOriginalWindows(handle)
			closeErr := windows.CloseHandle(handle)
			return nil, errors.Join(errors.New("wrapping private Windows capability probe handle"), cleanupErr, closeErr)
		}
		probe := &windowsCapabilityProbeFile{
			name:     name,
			opened:   opened,
			mutation: invalidWindowsMutationHandle(),
		}
		mutation, reopenErr := reopenMutationHandleWindows(opened)
		if reopenErr != nil {
			cleanupErr := disposeCapabilityProbeOriginalWindows(handle)
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

// disposeCapabilityProbeOriginalWindows reopens a freshly-created probe by
// its exact retained handle for failure cleanup. The retained handle omits
// DELETE so the production mutation handle can own the no-delete-sharing link
// lease; this permissively shared cleanup handle is used only before returning
// a failed capability result. No pathname is consulted, so a replacement at
// the probe's former name can never be removed.
func disposeCapabilityProbeOriginalWindows(original windows.Handle) error {
	result, _, callErr := reOpenFileWindows.Call(
		uintptr(original),
		uintptr(uint32(windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_ATTRIBUTES|windows.SYNCHRONIZE)),
		uintptr(uint32(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE)),
		uintptr(uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_WRITE_THROUGH)),
	)
	handle := windows.Handle(result)
	if handle == windows.InvalidHandle {
		if callErr == nil || callErr == windows.ERROR_SUCCESS {
			callErr = windows.ERROR_INVALID_HANDLE
		}
		return callErr
	}
	if err := requireSameWindowsHandle(original, handle); err != nil {
		return errors.Join(err, windows.CloseHandle(handle))
	}
	disposeErr := disposeCapabilityProbeHandleWindows(handle)
	closeErr := windows.CloseHandle(handle)
	return errors.Join(disposeErr, closeErr)
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
		return flushCapabilityProbeCleanupWindows(handle)
	}
	// The Ex form can remove a read-only probe directly. If the filesystem
	// rejects it, clear the attribute through the exact handle before trying
	// legacy disposition so a failed capability check never strands its own
	// private probe.
	var basic fileBasicInfoWindows
	if err := windows.GetFileInformationByHandleEx(
		handle,
		windows.FileBasicInfo,
		(*byte)(unsafe.Pointer(&basic)),
		uint32(unsafe.Sizeof(basic)),
	); err == nil && basic.FileAttributes&windows.FILE_ATTRIBUTE_READONLY != 0 {
		basic.FileAttributes &^= windows.FILE_ATTRIBUTE_READONLY
		if err := windows.SetFileInformationByHandle(
			handle,
			windows.FileBasicInfo,
			(*byte)(unsafe.Pointer(&basic)),
			uint32(unsafe.Sizeof(basic)),
		); err != nil {
			return errors.Join(exErr, fmt.Errorf("clearing read-only capability probe attribute: %w", err))
		}
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
	return flushCapabilityProbeCleanupWindows(handle)
}

func flushCapabilityProbeCleanupWindows(handle windows.Handle) error {
	// Every probe cleanup handle is synchronous and write-through. The exact
	// failure-cleanup reopen intentionally requests only DELETE and attribute
	// access, so FlushFileBuffers can report ACCESS_DENIED even after disposition
	// succeeded. In that case the completed write-through namespace operation is
	// the durability barrier, matching the production read-only-handle path.
	if err := windows.FlushFileBuffers(handle); err != nil && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return err
	}
	return nil
}

func cleanupWindowsCapabilityProbe(probe *windowsCapabilityProbeFile) error {
	if probe == nil {
		return nil
	}
	var cleanupErr error
	if probe.mutation.valid() && !probe.disposed {
		if disposeErr := disposeCapabilityProbeHandleWindows(probe.mutation.handle); disposeErr != nil {
			cleanupErr = errors.Join(cleanupErr, disposeErr,
				fmt.Errorf("private probe %s could not be disposed by its exact handle and may remain", probe.name))
		} else {
			probe.disposed = true
		}
	}
	if probe.mutation.valid() {
		cleanupErr = errors.Join(cleanupErr, closeMutationHandleWindows(&probe.mutation))
	}
	if probe.opened != nil {
		if !probe.disposed {
			if disposeErr := disposeCapabilityProbeOriginalWindows(windows.Handle(probe.opened.Fd())); disposeErr != nil {
				cleanupErr = errors.Join(cleanupErr, disposeErr,
					fmt.Errorf("private probe %s could not be disposed by its original exact handle and may remain", probe.name))
			} else {
				probe.disposed = true
			}
		}
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
	if filepath.Dir(fromName) != filepath.Dir(toName) && filepath.Dir(toName) != "." {
		// A nested destination parent would have to be both a write-sharing
		// rename target and a strict source-chain fence. Production cross-parent
		// moves target the bound root; refuse every other shape instead of
		// weakening either authority.
		return false, errors.New("secure Windows cross-parent rename requires a destination in the bound root")
	}
	lease, err := acquireWindowsDestinationNamespaceLease(root, toName, fromName)
	if err != nil {
		return false, fmt.Errorf("acquiring checkpoint namespace lease: %w", err)
	}
	defer func() { err = errors.Join(err, lease.close()) }()
	directory, err := lease.directory(toName)
	if err != nil {
		return false, err
	}

	sourceMutation, err := openBoundMutationHandleWindows(lease, fromName, source)
	if err != nil {
		return false, fmt.Errorf("binding checkpoint source link for exact-handle rename: %w", err)
	}
	defer func() { err = errors.Join(err, closeMutationHandleWindows(&sourceMutation)) }()
	displacedMutation := invalidWindowsMutationHandle()
	if replace {
		displacedMutation, err = openBoundMutationHandleWindows(lease, toName, displaced)
		if err != nil {
			return false, fmt.Errorf("binding displaced checkpoint link for exact-handle exchange: %w", err)
		}
		defer func() { err = errors.Join(err, closeMutationHandleWindows(&displacedMutation)) }()
	}

	if err := requireBoundNameWindows(root, fromName, source); err != nil {
		return false, err
	}
	if !replace {
		published, renameErr := renameMutationHandleWindows(
			root, root, source, sourceMutation, directory, fromName, toName,
		)
		if !published {
			return false, fmt.Errorf("renaming checkpoint file by handle: %w", renameErr)
		}
		flushErr := flushMutationHandleWindows(sourceMutation, "checkpoint rename")
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
		root, root, source, sourceMutation, directory, fromName, staging,
	)
	if !stepOne {
		return false, fmt.Errorf("staging checkpoint source by handle: %w", stepOneErr)
	}
	published = true
	if stepOneErr != nil {
		rollbackErr := rollbackOneRenameWindows(root, source, sourceMutation, directory, staging, fromName)
		return true, finishBoundRenameRollback(stepOneErr, rollbackErr)
	}
	if hookErr := runWindowsHandlePhase(windowsBeforeSourceStagingFlush); hookErr != nil {
		rollbackErr := rollbackOneRenameWindows(root, source, sourceMutation, directory, staging, fromName)
		return true, finishBoundRenameRollback(hookErr, rollbackErr)
	}
	if flushErr := flushMutationHandleWindows(sourceMutation, "staged checkpoint source"); flushErr != nil {
		rollbackErr := rollbackOneRenameWindows(root, source, sourceMutation, directory, staging, fromName)
		return true, finishBoundRenameRollback(flushErr, rollbackErr)
	}
	if hookErr := runWindowsHandlePhase(windowsAfterSourceStagingFlush); hookErr != nil {
		rollbackErr := rollbackOneRenameWindows(root, source, sourceMutation, directory, staging, fromName)
		return true, finishBoundRenameRollback(hookErr, rollbackErr)
	}

	stepTwo, stepTwoErr := renameMutationHandleWindows(
		root, root, displaced, displacedMutation, directory, toName, fromName,
	)
	if !stepTwo {
		rollbackErr := rollbackOneRenameWindows(root, source, sourceMutation, directory, staging, fromName)
		return true, finishBoundRenameRollback(fmt.Errorf("staging displaced checkpoint target by handle: %w", stepTwoErr), rollbackErr)
	}
	if stepTwoErr != nil {
		rollbackErr := rollbackExchangeWindows(root, source, displaced, sourceMutation, displacedMutation,
			directory, staging, fromName, toName)
		return true, finishBoundRenameRollback(stepTwoErr, rollbackErr)
	}
	if hookErr := runWindowsHandlePhase(windowsBeforeDisplacedStagingFlush); hookErr != nil {
		rollbackErr := rollbackExchangeWindows(root, source, displaced, sourceMutation, displacedMutation,
			directory, staging, fromName, toName)
		return true, finishBoundRenameRollback(hookErr, rollbackErr)
	}
	if flushErr := flushMutationHandleWindows(displacedMutation, "staged displaced checkpoint target"); flushErr != nil {
		rollbackErr := rollbackExchangeWindows(root, source, displaced, sourceMutation, displacedMutation,
			directory, staging, fromName, toName)
		return true, finishBoundRenameRollback(flushErr, rollbackErr)
	}
	if hookErr := runWindowsHandlePhase(windowsAfterDisplacedStagingFlush); hookErr != nil {
		rollbackErr := rollbackExchangeWindows(root, source, displaced, sourceMutation, displacedMutation,
			directory, staging, fromName, toName)
		return true, finishBoundRenameRollback(hookErr, rollbackErr)
	}

	stepThree, stepThreeErr := renameMutationHandleWindows(
		root, root, source, sourceMutation, directory, staging, toName,
	)
	if !stepThree {
		rollbackErr := rollbackExchangeWindows(root, source, displaced, sourceMutation, displacedMutation,
			directory, staging, fromName, toName)
		return true, finishBoundRenameRollback(fmt.Errorf("publishing checkpoint source by handle: %w", stepThreeErr), rollbackErr)
	}
	beforeFlushErr := runWindowsHandlePhase(windowsBeforeSourcePublicationFlush)
	if beforeFlushErr != nil {
		return true, beforeFlushErr
	}
	flushErr := flushMutationHandleWindows(sourceMutation, "published checkpoint source")
	afterFlushErr := runWindowsHandlePhase(windowsAfterSourcePublicationFlush)
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
	quarantine, err := unusedQuarantineSibling(root, name)
	if err != nil {
		return err
	}
	lease, err := acquireWindowsDestinationNamespaceLease(root, quarantine, name)
	if err != nil {
		return fmt.Errorf("acquiring checkpoint retirement namespace lease: %w", err)
	}
	defer func() { err = errors.Join(err, lease.close()) }()

	mutation, err := openBoundMutationHandleWindows(lease, name, file)
	if err != nil {
		return fmt.Errorf("binding checkpoint source link for exact-handle retirement: %w", err)
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

	directory, err := lease.directory(quarantine)
	if err != nil {
		return err
	}

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
		root, root, file, mutation, directory, name, quarantine,
	)
	if !published {
		return fmt.Errorf("quarantining checkpoint file by handle: %w", renameErr)
	}
	if after != nil {
		after(quarantine)
	}

	flushErr := flushMutationHandleWindows(mutation, "checkpoint quarantine rename")
	identityErr := requireBoundNameWindows(root, quarantine, file)
	if flushErr != nil {
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
	quarantine := retiredSinkName(name)
	// The source root is also the exact fallback destination when the sink is
	// unexpectedly on another volume. Lease that destination up front so the
	// fallback cannot reopen or rename an unbound parent.
	sourceLease, err := acquireWindowsDestinationNamespaceLease(sourceRoot, quarantine, name)
	if err != nil {
		return fmt.Errorf("acquiring checkpoint retirement source lease: %w", err)
	}
	defer func() { err = errors.Join(err, sourceLease.close()) }()
	sinkAnchor, err := createWindowsNamespaceAnchor(sinkRoot)
	if err != nil {
		return fmt.Errorf("anchoring checkpoint retirement sink: %w", err)
	}
	defer func() { err = errors.Join(err, sinkAnchor.close()) }()
	sinkLease, err := acquireWindowsDestinationNamespaceLease(sinkRoot, quarantine)
	if err != nil {
		return fmt.Errorf("acquiring checkpoint retirement sink lease: %w", err)
	}
	defer func() { err = errors.Join(err, sinkLease.close()) }()

	mutation, err := openBoundMutationHandleWindows(sourceLease, name, file)
	if err != nil {
		return fmt.Errorf("binding checkpoint source link for exact-handle retirement: %w", err)
	}
	defer func() { err = errors.Join(err, closeMutationHandleWindows(&mutation)) }()

	if err := requireBoundNameWindows(sourceRoot, name, file); err != nil {
		return err
	}
	if _, err := sinkRoot.Lstat(quarantine); !errors.Is(err, fs.ErrNotExist) {
		return errors.Join(err,
			fmt.Errorf("%w: deterministic checkpoint retirement name is occupied", ErrStale))
	}
	directory, err := sinkLease.directory(quarantine)
	if err != nil {
		return err
	}

	// The hook is the final adversarial seam. Everything after it addresses
	// the selected source handle and the bound sink directory handle directly.
	if err := requireBoundNameWindows(sourceRoot, name, file); err != nil {
		return err
	}
	if before != nil {
		before()
	}
	published, renameErr := renameMutationHandleWindows(
		sourceRoot, sinkRoot, file, mutation, directory, name, quarantine,
	)
	retirementRoot := sinkRoot
	if !published && errors.Is(renameErr, windows.ERROR_NOT_SAME_DEVICE) {
		if _, localErr := sourceRoot.Lstat(quarantine); !errors.Is(localErr, fs.ErrNotExist) {
			return errors.Join(renameErr, localErr,
				fmt.Errorf("%w: same-volume checkpoint retirement name is occupied", ErrStale))
		}
		localDirectory, localErr := sourceLease.directory(quarantine)
		if localErr != nil {
			return errors.Join(renameErr,
				fmt.Errorf("opening same-volume checkpoint retirement directory: %w", localErr))
		}
		published, localErr = renameMutationHandleWindows(
			sourceRoot, sourceRoot, file, mutation, localDirectory, name, quarantine,
		)
		if !published {
			return errors.Join(renameErr,
				fmt.Errorf("moving checkpoint file to same-volume retirement name: %w", localErr))
		}
		renameErr = localErr
		retirementRoot = sourceRoot
	}
	if !published {
		return fmt.Errorf("moving checkpoint file to trusted retirement sink: %w", renameErr)
	}
	if after != nil {
		after(quarantine)
	}
	flushErr := flushMutationHandleWindows(mutation, "checkpoint retirement-sink rename")
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
	mutation, err := openBoundMutationHandleWindows(lease, name, file)
	if err != nil {
		return fmt.Errorf("binding trusted checkpoint link for disposition: %w", err)
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

// openBoundMutationHandleWindows binds the exact source link named beneath an
// already-leased parent, then proves it is the inode selected by the caller's
// retained handle. ReOpenFile alone is insufficient: if that original link
// was moved before lease acquisition and a same-inode hard link was put back
// at name, ReOpenFile would keep following the moved link. Opening the checked
// leaf here preserves move semantics while the no-delete share mode leases the
// selected source link through rename, rollback, or disposition.
func openBoundMutationHandleWindows(lease *windowsNamespaceLease, name string, file *os.File) (windowsMutationHandle, error) {
	if file == nil {
		return invalidWindowsMutationHandle(), errors.New("checkpoint mutation requires a retained file handle")
	}
	if err := validateBoundRelativeWindows(name); err != nil {
		return invalidWindowsMutationHandle(), err
	}
	parent, err := lease.directory(name)
	if err != nil {
		return invalidWindowsMutationHandle(), err
	}
	objectName, err := windows.NewNTUnicodeString(filepath.Base(name))
	if err != nil {
		return invalidWindowsMutationHandle(), err
	}
	open := func(access uint32) (windows.Handle, error) {
		attributes := &windows.OBJECT_ATTRIBUTES{
			RootDirectory: parent,
			ObjectName:    objectName,
			Attributes:    windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE,
		}
		attributes.Length = uint32(unsafe.Sizeof(*attributes))
		var handle windows.Handle
		openErr := windows.NtCreateFile(
			&handle,
			access,
			attributes,
			&windows.IO_STATUS_BLOCK{},
			nil,
			0,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE,
			windows.FILE_OPEN,
			windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|
				windows.FILE_OPEN_REPARSE_POINT|windows.FILE_WRITE_THROUGH,
			0,
			0,
		)
		if status, ok := openErr.(windows.NTStatus); ok {
			openErr = status.Errno()
		}
		if openErr != nil {
			return windows.InvalidHandle, openErr
		}
		return handle, nil
	}
	handle, openErr := open(windows.FILE_GENERIC_WRITE | windows.FILE_READ_ATTRIBUTES | windows.DELETE)
	explicitFlush := true
	if errors.Is(openErr, windows.ERROR_ACCESS_DENIED) {
		handle, openErr = open(windows.DELETE | windows.FILE_READ_ATTRIBUTES | windows.FILE_WRITE_ATTRIBUTES | windows.SYNCHRONIZE)
		explicitFlush = false
	}
	runtime.KeepAlive(objectName)
	if openErr != nil {
		// The leased parent has already been resolved and held through an exact
		// handle, so a not-found result here can only mean that the selected leaf
		// disappeared before its mutation handle was bound. Report the checkpoint
		// contract's stale sentinel rather than leaking a platform-specific
		// ERROR_FILE_NOT_FOUND to callers.
		if errors.Is(openErr, fs.ErrNotExist) {
			return invalidWindowsMutationHandle(), errors.Join(openErr,
				fmt.Errorf("%w: checkpoint mutation source link is missing", ErrStale))
		}
		return invalidWindowsMutationHandle(), openErr
	}
	original := windows.Handle(file.Fd())
	sameErr := requireSameWindowsHandle(original, handle)
	runtime.KeepAlive(file)
	if sameErr != nil {
		return invalidWindowsMutationHandle(), errors.Join(sameErr, windows.CloseHandle(handle))
	}
	return windowsMutationHandle{handle: handle, original: original, explicitFlush: explicitFlush}, nil
}

// reopenMutationHandleWindows is used by private capability probes whose
// freshly allocated source link has not crossed an adversarial seam.
func reopenMutationHandleWindows(file *os.File) (windowsMutationHandle, error) {
	original := windows.Handle(file.Fd())
	reopen := func(access uint32) (windows.Handle, error) {
		result, _, callErr := reOpenFileWindows.Call(
			uintptr(original),
			uintptr(access),
			// The mutation handle is also the lease on the exact source
			// link. Refusing delete sharing makes acquisition fail when a
			// delete-capable opener already exists and prevents a new opener
			// from moving that link between the final identity check and the
			// native rename. The destination parent is independently bound;
			// both authorities are required to select one exact source link and
			// one exact destination directory.
			uintptr(uint32(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE)),
			uintptr(uint32(windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_WRITE_THROUGH)),
		)
		handle := windows.Handle(result)
		if handle == windows.InvalidHandle {
			if callErr == nil || callErr == windows.ERROR_SUCCESS {
				callErr = windows.ERROR_INVALID_HANDLE
			}
			return windows.InvalidHandle, callErr
		}
		return handle, nil
	}
	handle, callErr := reopen(windows.GENERIC_WRITE | windows.FILE_READ_ATTRIBUTES | windows.DELETE)
	explicitFlush := true
	if errors.Is(callErr, windows.ERROR_ACCESS_DENIED) {
		// FILE_ATTRIBUTE_READONLY and a pre-existing read-only descriptor can
		// reject GENERIC_WRITE even though exact-handle rename/disposition needs
		// only DELETE. Keep the same link-leasing, write-through contract and
		// fall back to the minimum access needed for the namespace operation.
		handle, callErr = reopen(windows.DELETE | windows.FILE_READ_ATTRIBUTES | windows.FILE_WRITE_ATTRIBUTES | windows.SYNCHRONIZE)
		explicitFlush = false
	}
	runtime.KeepAlive(file)
	if callErr != nil {
		return invalidWindowsMutationHandle(), callErr
	}
	if err := requireSameWindowsHandle(original, handle); err != nil {
		return invalidWindowsMutationHandle(), errors.Join(err, windows.CloseHandle(handle))
	}
	return windowsMutationHandle{handle: handle, original: original, explicitFlush: explicitFlush}, nil
}

func requireSameWindowsHandle(left, right windows.Handle) error {
	same, err := sameWindowsHandle(left, right)
	if err != nil {
		return err
	}
	if !same {
		return fmt.Errorf("%w: exact Windows handle selected a different checkpoint inode", ErrStale)
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

func renameMutationHandleWindows(sourceRoot, destinationRoot *os.Root, opened *os.File, file windowsMutationHandle, directory windows.Handle, from, to string) (bool, error) {
	if !file.valid() {
		return false, errors.New("checkpoint rename requires a live mutation handle")
	}
	if directory == 0 || directory == windows.InvalidHandle {
		return false, errors.New("checkpoint rename requires a live exact destination-parent handle")
	}
	encoded, err := windows.UTF16FromString(filepath.Base(to))
	if err != nil {
		return false, err
	}
	encoded = encoded[:len(encoded)-1]
	var layout fileRenameInfoWindows
	length := uintptr(len(encoded)) * unsafe.Sizeof(uint16(0))
	bufferLength := unsafe.Sizeof(layout) + length
	if bufferLength > uintptr(^uint32(0)) {
		return false, errors.New("checkpoint rename destination is too long")
	}
	// FileNameLength excludes the terminator. Keep one in the allocation even
	// though the NT contract does not require it: filesystem filters in the
	// rename stack have historically inspected FileName as a terminated string.
	buffer := make([]byte, bufferLength)
	info := (*fileRenameInfoWindows)(unsafe.Pointer(&buffer[0]))
	info.RootDirectory = directory
	info.FileNameLength = uint32(length)
	copy(unsafe.Slice(&info.FileName[0], len(encoded)), encoded)
	if windowsBeforeNativeRenameTestHook != nil {
		if hookErr := windowsBeforeNativeRenameTestHook(from, to); hookErr != nil {
			return false, hookErr
		}
	}
	renameErr := windows.NtSetInformationFile(
		file.handle,
		&windows.IO_STATUS_BLOCK{},
		&buffer[0],
		uint32(len(buffer)),
		windows.FileRenameInformation,
	)
	runtime.KeepAlive(buffer)
	if status, ok := renameErr.(windows.NTStatus); ok {
		renameErr = status.Errno()
	}
	if renameErr == nil {
		return true, nil
	}
	return classifyRenameFailureWindows(sourceRoot, destinationRoot, opened, from, to, renameErr)
}

func flushMutationHandleWindows(file windowsMutationHandle, operation string) error {
	if !file.valid() {
		return errors.New("checkpoint flush requires a live mutation handle")
	}
	if !file.explicitFlush {
		// The minimal DELETE handle is synchronous and FILE_FLAG_WRITE_THROUGH.
		// Prefer an explicit barrier through the retained original descriptor
		// when it was opened writable (as owned temporaries are). An existing
		// read-only target can legally reject that flush; its completed
		// write-through namespace operation remains the durability boundary.
		if err := windows.FlushFileBuffers(file.original); err != nil && !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
			return fmt.Errorf("flushing %s through the original file handle: %w", operation, err)
		}
		return nil
	}
	if err := windows.FlushFileBuffers(file.handle); err != nil {
		return fmt.Errorf("flushing %s: %w", operation, err)
	}
	return nil
}

func rollbackOneRenameWindows(root *os.Root, opened *os.File, file windowsMutationHandle, directory windows.Handle, from, to string) error {
	published, renameErr := renameMutationHandleWindows(root, root, opened, file, directory, from, to)
	if !published {
		return fmt.Errorf("rolling back checkpoint namespace: %w", renameErr)
	}
	return errors.Join(renameErr, flushMutationHandleWindows(file, "checkpoint namespace rollback"))
}

func rollbackExchangeWindows(root *os.Root, source, displaced *os.File, sourceHandle, displacedHandle windowsMutationHandle, directory windows.Handle, staging, from, to string) error {
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

func disposeRetiredMutationWindows(opened *os.File, file *windowsMutationHandle, scrub bool) error {
	if err := runWindowsHandlePhase(windowsBeforeRetirementDisposition); err != nil {
		return err
	}
	if err := disposeMutationHandleWindows(opened, file, scrub); err != nil {
		return err
	}
	return runWindowsHandlePhase(windowsAfterRetirementDisposition)
}

func disposeMutationHandleWindows(opened *os.File, file *windowsMutationHandle, scrub bool) error {
	if opened == nil || file == nil || !file.valid() {
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
		file.handle,
		windows.FileDispositionInfoEx,
		(*byte)(unsafe.Pointer(&disposition)),
		uint32(unsafe.Sizeof(disposition)),
	); err != nil {
		return fmt.Errorf("disposing checkpoint file by handle: %w", err)
	}
	if err := flushMutationHandleWindows(*file, "checkpoint disposition"); err != nil {
		return err
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

func closeMutationHandleWindows(file *windowsMutationHandle) error {
	if file == nil || !file.valid() {
		return nil
	}
	if err := windows.CloseHandle(file.handle); err != nil {
		return err
	}
	*file = invalidWindowsMutationHandle()
	return nil
}
