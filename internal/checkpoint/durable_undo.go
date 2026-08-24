package checkpoint

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

const (
	durableUndoJournalName     = ".switchboard-retry-transaction"
	durableUndoLockName        = ".switchboard-retry-transaction.lock"
	durableUndoJournalMagic    = "switchboard-retry-transaction 1\n"
	maxDurableUndoJournalBytes = 48 << 20
)

var (
	// ErrDurableUndoPending means another retry transaction already owns this
	// workspace, or a previous process left one for recovery. Starting a second
	// inverse would make neither journal authoritative.
	ErrDurableUndoPending = errors.New("a durable retry transaction is already pending")

	// ErrDurableUndoLocked means a live process still owns the pending journal.
	// Recovery must never inspect or remove a transaction while its creator can
	// still be restoring files.
	ErrDurableUndoLocked = errors.New("the durable retry transaction is locked by another process")

	// ErrDurableUndoRecoveryRequired means an unpublished retry could not prove
	// that its journal was retired. Another workspace mutation could turn a
	// recoverable pre/post image into an unrecognised third state.
	ErrDurableUndoRecoveryRequired = errors.New("durable retry recovery is required before another workspace mutation")

	// durableUndoRecoveryHandleHook is deterministic cleanup fault injection.
	// Production leaves it nil; tests use it only before concurrent work starts.
	durableUndoRecoveryHandleHook func(*durableUndoHandle)
	cleanupPreparingBeforeRetire  func(string)
)

type durableUndoJournal struct {
	Version           int                `json:"version"`
	Workspace         string             `json:"workspace"`
	WorkspaceIdentity string             `json:"workspace_identity"`
	ChildPath         string             `json:"child_path"`
	ChildIdentity     string             `json:"child_identity"`
	SessionID         string             `json:"session_id"`
	OpeningMessage    int                `json:"opening_message"`
	Files             []durableUndoEntry `json:"files"`
}

type durableUndoEntry struct {
	Path     string                 `json:"path"`
	TempName string                 `json:"temp_name"`
	Pre      durableFileFingerprint `json:"pre"`
	Post     durableFileState       `json:"post"`
}

type durableFileFingerprint struct {
	Existed bool   `json:"existed"`
	Mode    uint32 `json:"mode,omitempty"`
	Size    int64  `json:"size,omitempty"`
	Digest  string `json:"sha256,omitempty"`
}

type durableFileState struct {
	Existed bool   `json:"existed"`
	Mode    uint32 `json:"mode,omitempty"`
	Content []byte `json:"content,omitempty"`
}

type durableUndoHandle struct {
	file         *os.File
	lock         *os.File
	path         string
	dir          string
	dirIdentity  string
	dirRoot      *os.Root
	identity     fs.FileInfo
	beforeRetire func(string) // deterministic pre-retirement substitution seam
	afterRetire  func(string) // deterministic path-replacement seam for tests
}

type durableUndoPublication struct {
	published          bool
	exactAtDestination bool
}

type durableUndoBoundary uint8

const (
	durableUndoJournalCreated durableUndoBoundary = iota + 1
	durableUndoJournalHeaderWritten
	durableUndoJournalPayloadWritten
	durableUndoJournalFileSynced
	durableUndoJournalBeforeInstall
	durableUndoJournalAfterInstall
	durableUndoJournalReopened
	durableUndoBeforeRestore
	durableUndoAfterRestore
	durableUndoBeforeCommit
	durableUndoAfterCommit
)

// DurableUndoRecovery describes work completed before a session was adopted.
// Published means the child already committed, so its restored pre-images were
// retained. RolledForward counts paths returned to their post-turn state for an
// unpublished child; AlreadyPost counts paths that were already there.
type DurableUndoRecovery struct {
	Found          bool
	Published      bool
	RolledForward  int
	AlreadyPost    int
	CleanupWarning error
}

// PrepareDurableUndoCurrent is PrepareUndoCurrent with a process-crash journal.
// The journal is synced before this method returns, so ApplyAndCommit may not
// publish even its first pre-image unless restart recovery has the exact bytes
// needed to return an unpublished transaction to the source workspace.
func (r *Recorder) PrepareDurableUndoCurrent(identity TurnIdentity, journalDir, workspace, childPath, childIdentity string) (*PreparedUndo, error) {
	if err := r.ensureRestoreCleanup(journalDir, workspace); err != nil {
		return nil, err
	}
	r.mu.Lock()
	boundWorkspaceIdentity := r.restoreCleanup.workspaceIdentity
	r.mu.Unlock()
	prepared, err := r.PrepareUndoCurrent(identity)
	if err != nil {
		return nil, err
	}
	if r.beforeDurableJournalHook != nil {
		if err := r.beforeDurableJournalHook(); err != nil {
			return nil, err
		}
	}
	handle, err := writeDurableUndoJournal(prepared, journalDir, workspace, childPath, childIdentity, boundWorkspaceIdentity)
	if err != nil {
		return nil, err
	}
	prepared.journal = handle
	return prepared, nil
}

func writeDurableUndoJournal(prepared *PreparedUndo, journalDir, workspace, childPath, childIdentity, expectedWorkspaceIdentity string) (*durableUndoHandle, error) {
	if prepared == nil || prepared.recorder == nil {
		return nil, fmt.Errorf("%w: retry has no prepared inverse", ErrStale)
	}
	dir, err := resolvedDirectory(journalDir)
	if err != nil {
		return nil, fmt.Errorf("resolving retry journal directory: %w", err)
	}
	root, err := resolvedDirectory(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolving retry workspace: %w", err)
	}
	workspaceRoot, err := openRestoreScope(root)
	if err != nil {
		return nil, fmt.Errorf("binding retry workspace: %w", err)
	}
	workspaceIdentity := workspaceRoot.identity
	if expectedWorkspaceIdentity == "" || workspaceIdentity != expectedWorkspaceIdentity {
		_ = workspaceRoot.close()
		return nil, fmt.Errorf("%w: retry workspace changed identity between preparation and journal binding", ErrStale)
	}
	if err := workspaceRoot.close(); err != nil {
		return nil, fmt.Errorf("closing bound retry workspace: %w", err)
	}
	child, err := resolvedFileInDirectory(childPath, dir)
	if err != nil {
		return nil, fmt.Errorf("binding retry child: %w", err)
	}
	if childIdentity == "" || len(childIdentity) > 256 {
		return nil, errors.New("binding retry child: publication identity is empty or over its bound")
	}

	journal := durableUndoJournal{
		Version:           1,
		Workspace:         root,
		WorkspaceIdentity: workspaceIdentity,
		ChildPath:         child,
		ChildIdentity:     childIdentity,
		SessionID:         prepared.identity.SessionID,
		OpeningMessage:    prepared.identity.OpeningMessage,
		Files:             make([]durableUndoEntry, 0, len(prepared.files)),
	}
	seen := make(map[string]bool, len(prepared.files))
	var postBytes int64
	for i := range prepared.files {
		entry := &prepared.files[i]
		path, err := resolvedWorkspaceTarget(entry.path, root)
		if err != nil {
			return nil, fmt.Errorf("binding retry path %s: %w", entry.path, err)
		}
		if pathInside(path, dir) {
			return nil, fmt.Errorf("%w: retry path %s overlaps its recovery-control directory", ErrStale, path)
		}
		if seen[path] {
			return nil, fmt.Errorf("%w: retry path %s appears twice", ErrStale, path)
		}
		seen[path] = true
		tempName, err := randomRestoreName()
		if err != nil {
			return nil, err
		}
		entry.tempName = tempName
		pre := fingerprintBytes(entry.restore.existed, entry.restore.mode, entry.restore.content)
		stored := durableUndoEntry{
			Path:     path,
			TempName: tempName,
			Pre:      encodeDurableFingerprint(pre),
			Post: durableFileState{
				Existed: entry.post.Existed,
				Mode:    uint32(restorableMode(entry.post.Mode)),
				Content: append([]byte(nil), entry.post.Content...),
			},
		}
		if !stored.Post.Existed {
			stored.Post.Mode = 0
			stored.Post.Content = nil
		} else {
			postBytes += int64(len(stored.Post.Content))
			if len(stored.Post.Content) > maxFileBytes || postBytes > maxPreparedUndoBytes {
				return nil, fmt.Errorf("%w: durable retry post-images exceed their bound", ErrPreparedUndoTooLarge)
			}
		}
		journal.Files = append(journal.Files, stored)
	}
	if len(journal.Files) > maxPreparedUndoFiles {
		return nil, fmt.Errorf("%w: durable retry has too many paths", ErrPreparedUndoTooLarge)
	}

	payload, err := json.Marshal(journal)
	if err != nil {
		return nil, fmt.Errorf("encoding retry transaction journal: %w", err)
	}
	if len(payload) > maxDurableUndoJournalBytes {
		return nil, fmt.Errorf("%w: encoded retry journal exceeds %d bytes", ErrPreparedUndoTooLarge, maxDurableUndoJournalBytes)
	}
	digest := sha256.Sum256(payload)
	frame := make([]byte, 0, len(durableUndoJournalMagic)+sha256.Size*2+1+len(payload))
	frame = append(frame, durableUndoJournalMagic...)
	frame = append(frame, hex.EncodeToString(digest[:])...)
	frame = append(frame, '\n')
	frame = append(frame, payload...)

	path := filepath.Join(dir, durableUndoJournalName)
	lock, err := openDurableUndoLock(dir)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err == nil {
		_ = lock.Close()
		return nil, fmt.Errorf("%w: %s", ErrDurableUndoPending, path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		_ = lock.Close()
		return nil, fmt.Errorf("inspecting retry transaction journal: %w", err)
	}
	f, tempPath, err := createPreparingJournal(dir)
	if err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("creating retry transaction journal: %w", err)
	}
	handle := &durableUndoHandle{file: f, lock: lock, path: tempPath, dir: dir}
	hook := prepared.recorder.durableUndoHook
	if hook != nil {
		hook(durableUndoJournalCreated, -1)
	}
	cleanup := func(cause error) (*durableUndoHandle, error) {
		return nil, errors.Join(cause, handle.remove())
	}
	headerBytes := len(durableUndoJournalMagic) + sha256.Size*2 + 1
	if err := writeAll(f, frame[:headerBytes]); err != nil {
		return cleanup(fmt.Errorf("writing retry transaction journal: %w", err))
	}
	if hook != nil {
		hook(durableUndoJournalHeaderWritten, -1)
	}
	if err := writeAll(f, frame[headerBytes:]); err != nil {
		return cleanup(fmt.Errorf("writing retry transaction journal: %w", err))
	}
	if hook != nil {
		hook(durableUndoJournalPayloadWritten, -1)
	}
	if err := f.Sync(); err != nil {
		return cleanup(fmt.Errorf("syncing retry transaction journal: %w", err))
	}
	if hook != nil {
		hook(durableUndoJournalFileSynced, -1)
	}
	written, err := f.Stat()
	if err != nil {
		return cleanup(fmt.Errorf("binding written retry transaction journal: %w", err))
	}
	handle.identity = written
	if hook != nil {
		hook(durableUndoJournalBeforeInstall, -1)
	}
	publication, err := publishDurableUndoJournal(f, tempPath, path)
	if publication.published {
		handle.path = path
	}
	if err != nil {
		// A complete journal that crossed publication is safer left for startup
		// recovery than removed through a pathname we could not durably bind.
		if publication.exactAtDestination {
			return nil, errors.Join(ErrDurableUndoRecoveryRequired,
				fmt.Errorf("publishing retry transaction journal: %w", err), handle.close())
		}
		if publication.published {
			// The fixed name contains a foreign inode. Retire/scrub only the
			// exact O_EXCL journal handle; the replacement remains untouched.
			return nil, errors.Join(ErrDurableUndoRecoveryRequired,
				fmt.Errorf("publishing retry transaction journal: %w", err), handle.remove())
		}
		return cleanup(fmt.Errorf("publishing retry transaction journal: %w", err))
	}
	if hook != nil {
		hook(durableUndoJournalAfterInstall, -1)
	}
	reopenRoot, err := rootedfs.OpenRoot(dir)
	if err != nil {
		return nil, errors.Join(ErrDurableUndoRecoveryRequired,
			fmt.Errorf("binding published retry transaction journal directory: %w", err), handle.close())
	}
	reopened, reopenErr := reopenRoot.OpenFile(durableUndoJournalName, os.O_RDWR, 0o600)
	rootCloseErr := reopenRoot.Close()
	err = errors.Join(reopenErr, rootCloseErr)
	if err != nil {
		var reopenedCloseErr error
		if reopened != nil {
			reopenedCloseErr = reopened.Close()
		}
		return nil, errors.Join(ErrDurableUndoRecoveryRequired,
			fmt.Errorf("reopening published retry transaction journal: %w", err), reopenedCloseErr, handle.close())
	}
	linked, err := reopened.Stat()
	if err != nil || !os.SameFile(written, linked) {
		// The fixed pathname no longer names the inode we published. Generic
		// cleanup would bind itself to and delete that foreign replacement.
		return nil, errors.Join(ErrDurableUndoRecoveryRequired, err,
			fmt.Errorf("%w: published retry journal changed identity; replacement retained", ErrStale),
			reopened.Close(), handle.close())
	}
	// The publication handle still names the same inode, but only the reopened
	// descriptor is retained from here. Close the old owner before transferring
	// handle.file so every success, abort, and recovery path owns exactly one
	// journal descriptor.
	handle.file = nil
	if err := f.Close(); err != nil {
		return nil, errors.Join(ErrDurableUndoRecoveryRequired,
			fmt.Errorf("closing published retry transaction journal preparation handle: %w", err),
			reopened.Close(), handle.close())
	}
	handle.file = reopened
	if hook != nil {
		hook(durableUndoJournalReopened, -1)
	}
	return handle, nil
}

func encodeDurableFingerprint(value fingerprint) durableFileFingerprint {
	out := durableFileFingerprint{Existed: value.existed}
	if value.existed {
		out.Mode = uint32(restorableMode(value.mode))
		out.Size = value.size
		out.Digest = hex.EncodeToString(value.digest[:])
	}
	return out
}

func (value durableFileFingerprint) decode() (fingerprint, error) {
	return value.decodeBound(maxFileBytes)
}

func (value durableFileFingerprint) decodeBound(maxBytes int64) (fingerprint, error) {
	if !value.Existed {
		if value.Mode != 0 || value.Size != 0 || value.Digest != "" {
			return fingerprint{}, errors.New("non-existent fingerprint carries file data")
		}
		return fingerprint{}, nil
	}
	if maxBytes < 0 || value.Size < 0 || value.Size > maxBytes || value.Digest == "" {
		return fingerprint{}, errors.New("existing fingerprint is incomplete")
	}
	if !validStoredFileMode(value.Mode) {
		return fingerprint{}, errors.New("fingerprint carries unsupported mode bits")
	}
	digest, err := hex.DecodeString(value.Digest)
	if err != nil || len(digest) != sha256.Size {
		return fingerprint{}, errors.New("fingerprint has an invalid SHA-256 digest")
	}
	out := fingerprint{existed: true, mode: restorableMode(fs.FileMode(value.Mode)), size: value.Size}
	copy(out.digest[:], digest)
	return out, nil
}

func (value durableFileState) decode() (FileState, error) {
	if !value.Existed {
		if value.Mode != 0 || len(value.Content) != 0 {
			return FileState{}, errors.New("non-existent post-image carries file data")
		}
		return FileState{}, nil
	}
	if len(value.Content) > maxFileBytes {
		return FileState{}, fmt.Errorf("post-image exceeds %d bytes", maxFileBytes)
	}
	if !validStoredFileMode(value.Mode) {
		return FileState{}, errors.New("post-image carries unsupported mode bits")
	}
	return FileState{
		Existed: true,
		Mode:    restorableMode(fs.FileMode(value.Mode)),
		Content: append([]byte(nil), value.Content...),
	}, nil
}

func resolvedDirectory(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is not a real directory", resolved)
	}
	return filepath.Clean(resolved), nil
}

func resolvedFileInDirectory(path, dir string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("session log path %q is not absolute", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	linkedBefore, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if linkedBefore.Mode()&fs.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is a symbolic link, not a session log", abs)
	}
	if !linkedBefore.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular session log", abs)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	resolved := filepath.Join(parent, filepath.Base(abs))
	if filepath.Dir(resolved) != dir || filepath.Ext(resolved) != ".log" {
		return "", fmt.Errorf("%s is not a session log directly inside %s", resolved, dir)
	}
	f, err := openCheckpointPathRead(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return "", err
	}
	linkedAfter, err := os.Lstat(abs)
	if err != nil || linkedAfter.Mode()&fs.ModeSymlink != 0 ||
		!os.SameFile(linkedBefore, opened) || !os.SameFile(opened, linkedAfter) {
		return "", errors.Join(err, fmt.Errorf("%w: session log changed identity while binding", ErrStale))
	}
	if !opened.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular session log", resolved)
	}
	return resolved, nil
}

func resolvedWorkspaceTarget(path, root string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("retry target path %q is not absolute", path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return "", err
	}
	resolved := filepath.Join(parent, filepath.Base(abs))
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path escapes workspace %s", root)
	}
	if rel == "." {
		return "", errors.New("retry target is the workspace directory")
	}
	return filepath.Clean(resolved), nil
}

func pathInside(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func writeAll(f *os.File, data []byte) error {
	for len(data) > 0 {
		n, err := f.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

// AbortDurable removes a prepared journal before ApplyAndCommit has changed a
// file. It is used when an asynchronous retry is cancelled or its staged child
// is rejected before adoption. A used token refuses: recovery owns any journal
// whose filesystem phase may already have started.
func (p *PreparedUndo) AbortDurable() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.used {
		return fmt.Errorf("%w: prepared undo already entered its apply phase", ErrStale)
	}
	if p.journal == nil {
		return nil
	}
	err := p.journal.remove()
	p.journal = nil
	if err != nil {
		return errors.Join(ErrDurableUndoRecoveryRequired, err)
	}
	return nil
}

func (h *durableUndoHandle) remove() error {
	if h == nil {
		return nil
	}
	if h.file == nil && h.identity == nil {
		return h.close()
	}
	scope, err := configuredRestoreScope(h.dir, h.dirIdentity, h.dirRoot)
	if err != nil {
		return errors.Join(err, h.close())
	}
	defer scope.close()
	name := filepath.Base(h.path)
	if h.file == nil {
		file, openErr := scope.root.OpenFile(name, os.O_RDWR, 0)
		if openErr != nil {
			return errors.Join(openErr, h.close())
		}
		opened, statErr := file.Stat()
		if statErr != nil || h.identity == nil || !os.SameFile(opened, h.identity) {
			_ = file.Close()
			return errors.Join(statErr,
				fmt.Errorf("%w: retry journal path changed identity", ErrStale), h.close())
		}
		h.file = file
	}
	var before func()
	if h.beforeRetire != nil {
		before = func() { h.beforeRetire(h.path) }
	}
	var after func(string)
	if h.afterRetire != nil {
		after = func(name string) { h.afterRetire(filepath.Join(h.dir, name)) }
	}
	retireErr := retireBoundOpenFile(scope.root, name, h.file, true, before, after)
	h.identity = nil
	return errors.Join(retireErr, h.close())
}

func (h *durableUndoHandle) close() error {
	if h == nil {
		return nil
	}
	var err error
	if h.file != nil {
		err = errors.Join(err, h.file.Close())
		h.file = nil
	}
	if h.lock != nil {
		err = errors.Join(err, h.lock.Close())
		h.lock = nil
	}
	return err
}

// RecoverDurableUndo resolves the one pending retry transaction for a
// workspace before any session is selected or adopted. publication must
// validate the exact child log: true commits the pre-images, false rolls the
// source workspace forward, and an error leaves everything untouched.
func RecoverDurableUndo(journalDir, workspace string, publication func(string, string) (bool, error)) (DurableUndoRecovery, error) {
	var result DurableUndoRecovery
	if publication == nil {
		return result, errors.New("retry recovery has no publication validator")
	}
	dir, err := resolvedDirectory(journalDir)
	if err != nil {
		return result, fmt.Errorf("resolving retry journal directory: %w", err)
	}
	root, err := resolvedDirectory(workspace)
	if err != nil {
		return result, fmt.Errorf("resolving retry workspace: %w", err)
	}
	restoreRoot, err := openRestoreScope(root)
	if err != nil {
		return result, fmt.Errorf("binding retry workspace: %w", err)
	}
	defer restoreRoot.close()
	retirementScope, err := openRestoreScope(dir)
	if err != nil {
		return result, fmt.Errorf("binding retry retirement directory: %w", err)
	}
	defer retirementScope.close()
	path := filepath.Join(dir, durableUndoJournalName)
	handle, err := openLockedJournal(path)
	if errors.Is(err, fs.ErrNotExist) {
		if cleanupErr := cleanupRestoreTempLedgers(retirementScope, restoreRoot); cleanupErr != nil {
			return result, cleanupErr
		}
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if durableUndoRecoveryHandleHook != nil {
		durableUndoRecoveryHandleHook(handle)
	}
	defer func() {
		_ = handle.close()
	}()
	result.Found = true

	journal, err := decodeDurableUndoJournal(handle.file, dir, root, restoreRoot.identity)
	if err != nil {
		return result, fmt.Errorf("reading pending retry transaction: %w", err)
	}
	if err := cleanupRestoreTempLedgers(retirementScope, restoreRoot); err != nil {
		return result, err
	}
	if err := cleanupRecordedRestoreTemps(restoreRoot, journal); err != nil {
		return result, err
	}
	published, err := publication(journal.ChildPath, journal.ChildIdentity)
	if err != nil {
		return result, fmt.Errorf("checking pending retry child publication: %w", err)
	}
	// The publication callback can run arbitrary session-store logic while the
	// workspace is concurrently renamed or recreated. Revalidate the pathname
	// against the root capability held since recovery began before trusting
	// either verdict; in particular, never retire the journal as published when
	// the name now denotes a replacement workspace.
	if err := restoreRoot.validateLinked(); err != nil {
		return result, fmt.Errorf("validating retry workspace after publication check: %w", err)
	}
	if published {
		result.Published = true
		if err := handle.remove(); err != nil {
			result.CleanupWarning = fmt.Errorf("removing committed retry journal: %w", err)
		}
		return result, nil
	}

	type recoveryFile struct {
		entry durableUndoEntry
		pre   fingerprint
		post  FileState
		state *fileState
		done  bool
	}
	files := make([]recoveryFile, 0, len(journal.Files))
	for _, entry := range journal.Files {
		pre, err := entry.Pre.decode()
		if err != nil {
			return result, fmt.Errorf("retry path %s has invalid pre-image: %w", entry.Path, err)
		}
		post, err := entry.Post.decode()
		if err != nil {
			return result, fmt.Errorf("retry path %s has invalid post-image: %w", entry.Path, err)
		}
		current, err := restoreRoot.fingerprint(entry.Path, maxFileBytes)
		if err != nil {
			return result, fmt.Errorf("checking retry path %s: %w", entry.Path, err)
		}
		postFingerprint := fingerprintBytes(post.Existed, post.Mode, post.Content)
		file := recoveryFile{entry: entry, pre: pre, post: post}
		switch {
		case sameFingerprint(current, postFingerprint):
			file.done = true
			result.AlreadyPost++
		case sameFingerprint(current, pre):
			parent, parentErr := restoreRoot.parentInfo(entry.Path)
			if parentErr != nil {
				return result, fmt.Errorf("%w: cannot establish a safe parent for retry path %s: %v", ErrStale, entry.Path, parentErr)
			}
			file.state = &fileState{
				existed: post.Existed,
				mode:    restorableMode(post.Mode), content: append([]byte(nil), post.Content...),
				after: pre, parent: parent, parentSet: true,
			}
		default:
			return result, fmt.Errorf("%w: retry path %s is neither its recorded pre-image nor post-image; journal retained", ErrStale, entry.Path)
		}
		files = append(files, file)
	}

	// The complete preflight above makes a third-state path a no-write refusal.
	// Each successful restore is independently durable; if the process dies here,
	// the same journal sees that path as AlreadyPost and resumes idempotently.
	for _, file := range files {
		if file.done {
			continue
		}
		outcome := restoreInScope(restoreRoot, file.entry.Path, file.state, restoreHooks{
			tempName:       file.entry.TempName,
			retirementRoot: retirementScope.root,
		})
		if outcome.err != nil {
			return result, fmt.Errorf("rolling retry path %s forward: %w", file.entry.Path, outcome.err)
		}
		if !outcome.published {
			return result, fmt.Errorf("rolling retry path %s forward did not publish", file.entry.Path)
		}
		result.RolledForward++
	}
	if err := handle.remove(); err != nil {
		return result, errors.Join(ErrDurableUndoRecoveryRequired,
			fmt.Errorf("removing recovered unpublished retry journal: %w", err))
	}
	return result, nil
}

func boundedDurableFingerprint(path string) (fingerprint, error) {
	return fingerprintPathBounded(path, maxFileBytes)
}

func openDurableUndoLock(dir string) (*os.File, error) {
	path := filepath.Join(dir, durableUndoLockName)
	before, err := os.Lstat(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("inspecting retry transaction lock: %w", err)
	}
	if err == nil && !before.Mode().IsRegular() {
		return nil, errors.New("retry transaction lock is not a regular file")
	}
	lock, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening retry transaction lock: %w", err)
	}
	after, statErr := lock.Stat()
	linked, linkErr := os.Lstat(path)
	if statErr != nil || linkErr != nil || !linked.Mode().IsRegular() || !os.SameFile(linked, after) {
		_ = lock.Close()
		return nil, errors.Join(statErr, linkErr, fmt.Errorf("%w: retry transaction lock changed identity", ErrStale))
	}
	if err := acquireJournalLock(lock); err != nil {
		_ = lock.Close()
		return nil, fmt.Errorf("%w: %v", ErrDurableUndoLocked, err)
	}
	// Recheck the linked name after acquiring the descriptor lock. A process
	// that saw a replaced inode before the lock must not proceed on an orphan.
	linked, err = os.Lstat(path)
	if err != nil || !linked.Mode().IsRegular() || !os.SameFile(linked, after) {
		_ = lock.Close()
		return nil, errors.Join(err, fmt.Errorf("%w: retry transaction lock changed after locking", ErrStale))
	}
	return lock, nil
}

func openLockedJournal(path string) (*durableUndoHandle, error) {
	dir := filepath.Dir(path)
	lock, err := openDurableUndoLock(dir)
	if err != nil {
		return nil, err
	}
	if err := cleanupPreparingJournals(dir); err != nil {
		_ = lock.Close()
		return nil, err
	}
	scope, err := openRestoreScope(dir)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	defer scope.close()
	name := filepath.Base(path)
	before, err := scope.root.Lstat(name)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	frameLimit := int64(maxDurableUndoJournalBytes + len(durableUndoJournalMagic) + sha256.Size*2 + 1)
	if !before.Mode().IsRegular() || before.Size() > frameLimit {
		_ = lock.Close()
		return nil, fmt.Errorf("pending retry journal is not a bounded regular file")
	}
	f, err := scope.root.OpenFile(name, os.O_RDWR, 0o600)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) {
		_ = f.Close()
		_ = lock.Close()
		return nil, fmt.Errorf("%w: retry journal changed identity while opening", ErrStale)
	}
	return &durableUndoHandle{file: f, lock: lock, path: path, dir: dir}, nil
}

func cleanupPreparingJournals(dir string) error {
	scope, err := openRestoreScope(dir)
	if err != nil {
		return fmt.Errorf("opening retry journal directory for cleanup: %w", err)
	}
	defer scope.close()
	directory, err := scope.root.Open(".")
	if err != nil {
		return fmt.Errorf("opening bound retry journal inventory: %w", err)
	}
	defer directory.Close()
	frameLimit := int64(maxDurableUndoJournalBytes + len(durableUndoJournalMagic) + sha256.Size*2 + 1)
	removed, entriesSeen := 0, 0
	for {
		entries, readErr := directory.ReadDir(128)
		for _, entry := range entries {
			entriesSeen++
			if entriesSeen > maxRestoreLedgerDirEntries {
				return fmt.Errorf("retry journal directory exceeds its %d-entry cleanup bound", maxRestoreLedgerDirEntries)
			}
			if !isPreparingJournalName(entry.Name()) {
				continue
			}
			removed++
			if removed > maxPreparedUndoFiles {
				return fmt.Errorf("too many abandoned retry journal preparations in %s", dir)
			}
			path := filepath.Join(dir, entry.Name())
			linked, err := scope.root.Lstat(entry.Name())
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				return fmt.Errorf("inspecting abandoned retry journal %s: %w", path, err)
			}
			if !linked.Mode().IsRegular() || linked.Size() > frameLimit {
				return fmt.Errorf("abandoned retry journal %s is not a bounded regular file", path)
			}
			file, err := scope.root.OpenFile(entry.Name(), os.O_RDWR, 0)
			if err != nil {
				return fmt.Errorf("opening abandoned retry journal %s: %w", path, err)
			}
			opened, statErr := file.Stat()
			if statErr != nil || !os.SameFile(linked, opened) {
				_ = file.Close()
				return errors.Join(statErr,
					fmt.Errorf("%w: abandoned retry journal %s changed identity", ErrStale, path))
			}
			magic := make([]byte, len(durableUndoJournalMagic))
			_, magicErr := io.ReadFull(file, magic)
			if magicErr != nil || string(magic) != durableUndoJournalMagic {
				// Exact random grammar is only reservation evidence. Switchboard
				// ownership begins when the magic is written; retain anything else.
				if closeErr := file.Close(); closeErr != nil {
					return closeErr
				}
				continue
			}
			// A preparation that is also the fixed journal is authoritative, not
			// abandoned. Current publication is a single-link no-clobber move;
			// keep this identity check for crash artifacts written by older builds.
			if fixed, fixedErr := scope.root.Lstat(durableUndoJournalName); fixedErr == nil &&
				fixed.Mode().IsRegular() && os.SameFile(fixed, opened) {
				if closeErr := file.Close(); closeErr != nil {
					return closeErr
				}
				continue
			} else if fixedErr != nil && !errors.Is(fixedErr, fs.ErrNotExist) {
				_ = file.Close()
				return fixedErr
			}
			var before func()
			if cleanupPreparingBeforeRetire != nil {
				before = func() { cleanupPreparingBeforeRetire(path) }
			}
			retireErr := retireBoundOpenFile(scope.root, entry.Name(), file, true, before, nil)
			closeErr := file.Close()
			if retireErr != nil || closeErr != nil {
				return errors.Join(fmt.Errorf("retiring abandoned retry journal %s: %w", path, retireErr), closeErr)
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return fmt.Errorf("reading retry journal directory for cleanup: %w", readErr)
		}
	}
	return nil
}

func createPreparingJournal(dir string) (*os.File, string, error) {
	scope, err := openRestoreScope(dir)
	if err != nil {
		return nil, "", err
	}
	defer scope.close()
	for range 100 {
		name, err := randomRestoreName()
		if err != nil {
			return nil, "", err
		}
		suffix := strings.TrimPrefix(name, ".switchboard-undo-")
		path := filepath.Join(dir, durableUndoJournalName+".preparing-"+suffix)
		file, err := scope.root.OpenFile(filepath.Base(path), os.O_RDWR|os.O_CREATE|os.O_EXCL|os.O_SYNC, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return file, path, err
	}
	return nil, "", errors.New("could not allocate a retry journal preparation name")
}

func isPreparingJournalName(name string) bool {
	prefix := durableUndoJournalName + ".preparing-"
	if len(name) != len(prefix)+32 || !strings.HasPrefix(name, prefix) {
		return false
	}
	_, err := hex.DecodeString(name[len(prefix):])
	return err == nil
}

func decodeDurableUndoJournal(f *os.File, dir, root, rootIdentity string) (durableUndoJournal, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return durableUndoJournal{}, err
	}
	r := bufio.NewReader(io.LimitReader(f, maxDurableUndoJournalBytes+int64(len(durableUndoJournalMagic)+sha256.Size*2+2)))
	magic, err := r.ReadString('\n')
	if err != nil || magic != durableUndoJournalMagic {
		return durableUndoJournal{}, errors.New("retry journal has an invalid header")
	}
	wantHex, err := r.ReadString('\n')
	if err != nil || len(wantHex) != sha256.Size*2+1 {
		return durableUndoJournal{}, errors.New("retry journal has an invalid checksum header")
	}
	want, err := hex.DecodeString(strings.TrimSuffix(wantHex, "\n"))
	if err != nil || len(want) != sha256.Size {
		return durableUndoJournal{}, errors.New("retry journal checksum is not SHA-256")
	}
	payload, err := io.ReadAll(r)
	if err != nil {
		return durableUndoJournal{}, err
	}
	if len(payload) == 0 || len(payload) > maxDurableUndoJournalBytes {
		return durableUndoJournal{}, errors.New("retry journal payload is empty or over its bound")
	}
	got := sha256.Sum256(payload)
	if !bytes.Equal(got[:], want) {
		return durableUndoJournal{}, errors.New("retry journal checksum does not match")
	}
	var journal durableUndoJournal
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&journal); err != nil {
		return durableUndoJournal{}, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return durableUndoJournal{}, err
	}
	if journal.Version != 1 || journal.SessionID == "" || journal.OpeningMessage < 0 ||
		journal.ChildIdentity == "" || len(journal.ChildIdentity) > 256 {
		return durableUndoJournal{}, errors.New("retry journal has invalid transaction identity")
	}
	if journal.Workspace != root {
		return durableUndoJournal{}, fmt.Errorf("retry journal belongs to workspace %s, not %s", journal.Workspace, root)
	}
	if journal.WorkspaceIdentity == "" || journal.WorkspaceIdentity != rootIdentity {
		return durableUndoJournal{}, fmt.Errorf("%w: retry workspace %s changed identity before recovery", ErrStale, root)
	}
	child, err := resolvedFileInDirectory(journal.ChildPath, dir)
	if err != nil {
		// A failed operation may already have discarded its unpublished child.
		// Preserve the clean path for the publication callback to classify missing
		// as unpublished, but reject every other escape or extension change.
		if !errors.Is(err, fs.ErrNotExist) {
			return durableUndoJournal{}, fmt.Errorf("retry journal child is invalid: %w", err)
		}
		clean := filepath.Clean(journal.ChildPath)
		if filepath.Dir(clean) != dir || filepath.Ext(clean) != ".log" {
			return durableUndoJournal{}, fmt.Errorf("retry journal child is invalid: %w", err)
		}
		child = clean
	}
	journal.ChildPath = child
	if len(journal.Files) > maxPreparedUndoFiles {
		return durableUndoJournal{}, fmt.Errorf("retry journal exceeds the %d-file bound", maxPreparedUndoFiles)
	}
	seen := make(map[string]bool, len(journal.Files))
	seenTemps := make(map[string]bool, len(journal.Files))
	var postBytes int64
	for i := range journal.Files {
		entry := &journal.Files[i]
		storedPath := filepath.Clean(entry.Path)
		path, err := resolvedWorkspaceTarget(entry.Path, root)
		if err != nil {
			return durableUndoJournal{}, fmt.Errorf("retry journal path %s is invalid: %w", entry.Path, err)
		}
		if path != storedPath {
			return durableUndoJournal{}, fmt.Errorf("%w: retry journal path %s now resolves to %s", ErrStale, storedPath, path)
		}
		if pathInside(path, dir) {
			return durableUndoJournal{}, fmt.Errorf("%w: retry journal path %s overlaps its recovery-control directory", ErrStale, path)
		}
		if seen[path] {
			return durableUndoJournal{}, fmt.Errorf("retry journal path %s appears twice", path)
		}
		seen[path] = true
		if !isRestoreTempName(entry.TempName) {
			return durableUndoJournal{}, fmt.Errorf("retry journal path %s has an invalid temporary name", path)
		}
		tempPath := filepath.Join(filepath.Dir(path), entry.TempName)
		if seenTemps[tempPath] {
			return durableUndoJournal{}, fmt.Errorf("retry journal temporary %s appears twice", tempPath)
		}
		seenTemps[tempPath] = true
		entry.Path = path
		if _, err := entry.Pre.decode(); err != nil {
			return durableUndoJournal{}, fmt.Errorf("retry journal path %s pre-image: %w", path, err)
		}
		post, err := entry.Post.decode()
		if err != nil {
			return durableUndoJournal{}, fmt.Errorf("retry journal path %s post-image: %w", path, err)
		}
		postBytes += int64(len(post.Content))
		if postBytes > maxPreparedUndoBytes {
			return durableUndoJournal{}, fmt.Errorf("retry journal post-images exceed %d bytes", maxPreparedUndoBytes)
		}
	}
	return journal, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("retry journal has trailing JSON")
		}
		return err
	}
	return nil
}
