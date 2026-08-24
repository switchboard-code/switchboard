package tools

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"
)

// pathLocks serialize first-party mutations of one resolved path across every
// registry in the process. A registry-local lock would still allow a delegate
// or another session rooted at the same workspace to interleave a stale check
// and publication.
//
// The map key is deliberately case-folded on every platform. A resolved path
// retains the caller's spelling, and case-insensitive filesystems accept two
// spellings for the same leaf; keying the lease by that spelling would let two
// registries publish concurrently. Folding everywhere is conservative on a
// case-sensitive volume (case-distinct files serialize) but does not merge
// their contents or change which path is validated, opened, checkpointed, or
// rendered to the user.
var pathLocks = struct {
	sync.Mutex
	locks map[string]*pathLock
}{locks: map[string]*pathLock{}}

type pathLock struct {
	mu   sync.Mutex
	refs int
}

func lockPath(path string) func() {
	key := mutationLockKey(path)
	pathLocks.Lock()
	l := pathLocks.locks[key]
	if l == nil {
		l = &pathLock{}
		pathLocks.locks[key] = l
	}
	l.refs++
	pathLocks.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		pathLocks.Lock()
		l.refs--
		if l.refs == 0 {
			delete(pathLocks.locks, key)
		}
		pathLocks.Unlock()
	}
}

func mutationLockKey(path string) string {
	return strings.Map(func(r rune) rune {
		canonical := r
		for folded := unicode.SimpleFold(r); folded != r; folded = unicode.SimpleFold(folded) {
			if folded < canonical {
				canonical = folded
			}
		}
		return canonical
	}, filepath.Clean(path))
}

type diskFile struct {
	existed bool
	mode    fs.FileMode
	content []byte
	digest  [sha256.Size]byte
	info    fs.FileInfo
}

func (f diskFile) sameContent(other diskFile) bool {
	if f.existed != other.existed {
		return false
	}
	if !f.existed {
		return true
	}
	return f.mode == other.mode && f.digest == other.digest
}

func readDiskFile(parent *os.Root, leaf, display string, beforeOpen func()) (diskFile, error) {
	linfo, err := parent.Lstat(leaf)
	if err != nil {
		if os.IsNotExist(err) {
			return diskFile{}, nil
		}
		return diskFile{}, err
	}
	if !linfo.Mode().IsRegular() {
		return diskFile{}, fmt.Errorf("%s is not a regular file", display)
	}
	if linfo.Size() > maxWorkspaceFileBytes {
		return diskFile{}, fmt.Errorf("%s is %d bytes; mutation file limit is %d", display, linfo.Size(), maxWorkspaceFileBytes)
	}

	f, opened, err := openRegularWorkspaceFile(parent, leaf, display, linfo, beforeOpen)
	if err != nil {
		return diskFile{}, err
	}
	defer f.Close()
	content, err := io.ReadAll(io.LimitReader(f, maxWorkspaceFileBytes+1))
	if err != nil {
		return diskFile{}, err
	}
	if int64(len(content)) > maxWorkspaceFileBytes {
		return diskFile{}, fmt.Errorf("%s grew beyond the %d-byte mutation file limit", display, maxWorkspaceFileBytes)
	}
	finished, err := f.Stat()
	if err != nil {
		return diskFile{}, err
	}
	if !os.SameFile(opened, finished) || opened.Size() != finished.Size() ||
		finished.Size() != int64(len(content)) || !opened.ModTime().Equal(finished.ModTime()) ||
		restorableFileMode(opened.Mode()) != restorableFileMode(finished.Mode()) {
		return diskFile{}, fmt.Errorf("%s changed while it was being read", display)
	}
	linked, linkErr := parent.Lstat(leaf)
	if linkErr != nil || !linked.Mode().IsRegular() || !os.SameFile(finished, linked) {
		return diskFile{}, errors.Join(linkErr, fmt.Errorf("%s changed identity while it was being read", display))
	}
	return diskFile{
		existed: true,
		mode:    restorableFileMode(finished.Mode()),
		content: content,
		digest:  sha256.Sum256(content),
		info:    finished,
	}, nil
}

type fileMutation struct {
	r            *Registry
	abs          string
	root         *os.Root
	parent       *os.Root
	parentInfo   fs.FileInfo
	parentRel    string
	leaf         string
	before       diskFile
	readToken    string
	readTokenSet bool
	unlock       func()
	closed       bool
}

// prepareFileMutation takes the per-path lease, reads one exact source image,
// and enforces read-before-write against that same image. Validation such as
// exact edit matching happens while the lease remains held and before a
// checkpoint is prepared.
func (r *Registry) prepareFileMutation(abs string, allowMissing bool) (*fileMutation, Result, bool) {
	// Capture the caller's read token before waiting for another mutation of
	// this path. Two calls launched from the same source image must not let
	// the first call's success silently authorize the second call to overwrite
	// it. A genuinely sequential follow-up observes the updated token here.
	recorded, versionKnown := r.versions.get(abs)
	unlock := lockPath(abs)
	var root *os.Root
	var parent *os.Root
	fail := func(format string, args ...any) (*fileMutation, Result, bool) {
		if parent != nil {
			_ = parent.Close()
		}
		if root != nil {
			_ = root.Close()
		}
		unlock()
		res, _ := errorf(format, args...)
		return nil, res, false
	}

	var rel string
	var err error
	root, rel, err = r.openResolvedWorkspace(abs)
	if err != nil {
		return fail("cannot safely access %s: %v", r.display(abs), err)
	}
	if rel == "." {
		return fail("cannot safely access %s: path names the workspace root", r.display(abs))
	}
	parentRel := filepath.Dir(rel)
	leaf := filepath.Base(rel)
	parentInfo := fs.FileInfo(nil)
	parent, parentInfo, err = bindWorkspaceParent(root, parentRel, false)
	before := diskFile{}
	if err == nil {
		before, err = readDiskFile(parent, leaf, r.display(abs), nil)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fail("cannot read %s: %v", r.display(abs), err)
	}
	if err != nil {
		if parent != nil {
			_ = parent.Close()
			parent = nil
		}
		parentInfo = nil
		before = diskFile{}
	}
	if !before.existed && !allowMissing {
		return fail("cannot read %s: file does not exist", r.display(abs))
	}
	if !before.existed {
		return &fileMutation{
			r: r, abs: abs, root: root, parent: parent, parentInfo: parentInfo,
			parentRel: parentRel, leaf: leaf, before: before,
			readToken: recorded, readTokenSet: versionKnown, unlock: unlock,
		}, Result{}, true
	}

	if !versionKnown {
		return fail("%s exists but has not been read in this session. Read it first so "+
			"the change is made against its current contents.", r.display(abs))
	}
	if recorded != hashContent(before.content) {
		r.versions.forgetIf(abs, recorded)
		return fail("%s changed since it was read. Read it again before writing.", r.display(abs))
	}
	return &fileMutation{
		r: r, abs: abs, root: root, parent: parent, parentInfo: parentInfo,
		parentRel: parentRel, leaf: leaf, before: before,
		readToken: recorded, readTokenSet: versionKnown, unlock: unlock,
	}, Result{}, true
}

// forgetIf removes only the stale token this call actually relied on. A
// concurrent read may already have refreshed the path while this mutation was
// waiting for its lease; erasing that newer evidence would be safe but wrong.
func (v *fileVersions) forgetIf(path, stale string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.seen[path] != stale {
		return
	}
	delete(v.seen, path)
	delete(v.whole, path)
}

func (m *fileMutation) close() {
	if m == nil || m.closed {
		return
	}
	m.closed = true
	if m.parent != nil {
		_ = m.parent.Close()
		m.parent = nil
	}
	if m.root != nil {
		_ = m.root.Close()
		m.root = nil
	}
	m.unlock()
}

type publishResult struct {
	published bool
	after     diskFile
}

// publish atomically replaces the target after preparing its checkpoint.
// hook is test-only fault injection run after the durable temporary file is
// ready and before the source compare-and-swap check.
func (m *fileMutation) publish(ctx context.Context, content []byte, mode fs.FileMode, hook func()) error {
	// The preimage reader and every later mutation of this path share one
	// complete-file bound. Refuse an oversized post-image before preparing a
	// checkpoint or temporary: discovering the limit only during post-rename
	// verification would report failure after the file had already changed and
	// would leave a result the tool itself could no longer read or edit.
	if int64(len(content)) > maxWorkspaceFileBytes {
		return fmt.Errorf("%s would be %d bytes; mutation file limit is %d",
			m.r.display(m.abs), len(content), maxWorkspaceFileBytes)
	}
	mode = restorableFileMode(mode)
	if m.before.existed && bytes.Equal(m.before.content, content) && m.before.mode == mode {
		return nil
	}

	publisher, err := m.prepareCheckpoint()
	if err != nil {
		return err
	}
	result, err := publishFile(ctx, m, publisher, content, mode, hook)
	if result.published {
		postMode := mode
		postDigest := sha256.Sum256(content)
		if result.after.existed {
			postMode = result.after.mode
			postDigest = result.after.digest
		}
		m.commitCheckpoint(true, postMode, postDigest)
	} else {
		m.abortCheckpoint()
	}
	if err != nil {
		// Publication errors can mean an external writer won the source CAS or
		// that verification after rename failed. In both cases retaining a
		// read token would make the next attempt less safe.
		if m.readTokenSet {
			m.r.versions.forgetIf(m.abs, m.readToken)
		}
		return err
	}
	m.r.versions.record(m.abs, hashContent(content), result.after.info)
	return nil
}

type exactStateCheckpointer interface {
	RecordState(abs string, existed bool, mode fs.FileMode, content []byte)
}

type lifecycleCheckpointer interface {
	Commit(abs string, existed bool, mode fs.FileMode, digest [sha256.Size]byte)
	Abort(abs string)
}

type atomicFilePublisher interface {
	PublishFileCAS(
		ctx context.Context,
		path string,
		parent *os.Root,
		leaf string,
		expectedExisted bool,
		expectedMode fs.FileMode,
		expectedContent []byte,
		desiredMode fs.FileMode,
		desiredContent []byte,
		beforePublication func(),
	) (published bool, err error)
}

func (m *fileMutation) prepareCheckpoint() (atomicFilePublisher, error) {
	if m.r.checkpoints == nil {
		return nil, errors.New("atomic workspace mutation requires a checkpoint recorder")
	}
	exact, exactOK := m.r.checkpoints.(exactStateCheckpointer)
	_, lifecycleOK := m.r.checkpoints.(lifecycleCheckpointer)
	publisher, publisherOK := m.r.checkpoints.(atomicFilePublisher)
	if !exactOK || !lifecycleOK || !publisherOK {
		return nil, errors.New("checkpoint recorder does not support atomic workspace publication")
	}
	exact.RecordState(m.abs, m.before.existed, m.before.mode, m.before.content)
	return publisher, nil
}

func (m *fileMutation) commitCheckpoint(existed bool, mode fs.FileMode, digest [sha256.Size]byte) {
	if lifecycle, ok := m.r.checkpoints.(lifecycleCheckpointer); ok {
		lifecycle.Commit(m.abs, existed, mode, digest)
	}
}

func (m *fileMutation) abortCheckpoint() {
	if lifecycle, ok := m.r.checkpoints.(lifecycleCheckpointer); ok {
		lifecycle.Abort(m.abs)
	}
}

func publishFile(ctx context.Context, mutation *fileMutation, publisher atomicFilePublisher, content []byte, mode fs.FileMode, hook func()) (out publishResult, retErr error) {
	if mutation == nil || mutation.root == nil {
		return out, errors.New("workspace mutation has no root capability")
	}
	if publisher == nil {
		return out, errors.New("workspace mutation has no atomic publisher")
	}
	if mutation.parent == nil {
		parent, info, err := bindWorkspaceParent(mutation.root, mutation.parentRel, true)
		if err != nil {
			return out, err
		}
		mutation.parent, mutation.parentInfo = parent, info
	}
	if err := mutation.r.verifyWorkspaceParent(mutation.root, mutation.parentRel, mutation.parentInfo); err != nil {
		return out, err
	}
	out.published, retErr = publisher.PublishFileCAS(
		ctx,
		mutation.abs,
		mutation.parent,
		mutation.leaf,
		mutation.before.existed,
		mutation.before.mode,
		mutation.before.content,
		mode,
		content,
		hook,
	)
	if retErr == nil && !out.published {
		retErr = errors.New("atomic workspace publication returned without committing")
	}
	if retErr != nil || !out.published {
		return out, retErr
	}
	after, err := readDiskFile(mutation.parent, mutation.leaf, mutation.r.display(mutation.abs), nil)
	if err != nil {
		return out, err
	}
	out.after = after
	want := diskFile{existed: true, mode: mode, content: content, digest: sha256.Sum256(content)}
	if !want.sameContent(after) {
		return out, fmt.Errorf("verifying %s after atomic replace: post-image mismatch", mutation.abs)
	}
	return out, nil
}

func sameSource(before, current diskFile) bool {
	if !before.sameContent(current) {
		return false
	}
	if !before.existed {
		return true
	}
	return before.info != nil && current.info != nil && os.SameFile(before.info, current.info)
}
