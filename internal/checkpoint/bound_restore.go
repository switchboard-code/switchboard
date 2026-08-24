package checkpoint

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

type boundRenameResult struct {
	published      bool
	sourceRetained bool
}

// boundNamespaceLinearizationTestHook is the final portable adversarial seam
// before a workspace-root-relative namespace primitive begins. Production
// leaves it nil. Platform tests may additionally use narrower syscall seams.
var boundNamespaceLinearizationTestHook func()

// renameBoundRestoreFile describes the namespace state produced by a
// successful primitive. Replacement is an exchange and leaves the displaced
// inode at the source name; no-replace publication is a move and leaves no
// source alias. Callers must retire only a source the primitive says remains.
func renameBoundRestoreFile(root *os.Root, source, displaced *os.File, from, to string, replace bool) (boundRenameResult, error) {
	if root == nil || source == nil {
		return boundRenameResult{}, errors.New("checkpoint namespace mutation requires live root and source handles")
	}
	if err := validateBoundRelativeName(from); err != nil {
		return boundRenameResult{}, fmt.Errorf("invalid checkpoint namespace source: %w", err)
	}
	if err := validateBoundRelativeName(to); err != nil {
		return boundRenameResult{}, fmt.Errorf("invalid checkpoint namespace destination: %w", err)
	}
	if replace && filepath.Dir(from) != filepath.Dir(to) {
		return boundRenameResult{}, errors.New("checkpoint exchange names must share one parent directory")
	}
	if boundNamespaceLinearizationTestHook != nil {
		boundNamespaceLinearizationTestHook()
	}
	published, err := renameBoundOpenFile(root, source, displaced, from, to, replace)
	return boundRenameResult{
		published:      published,
		sourceRetained: published && replace && err == nil,
	}, err
}

func validateBoundRelativeName(name string) error {
	if name == "" || name == "." || filepath.IsAbs(name) || !filepath.IsLocal(name) || filepath.Clean(name) != name {
		return fmt.Errorf("%q is not a canonical local relative name", name)
	}
	return nil
}

// restoreScope is a stable capability for a workspace directory. Paths are
// still rendered and recorded as absolute names, but recovery resolves them
// through this handle so a concurrent parent symlink cannot redirect a
// privileged restore outside the workspace.
type restoreScope struct {
	root     *os.Root
	path     string
	info     fs.FileInfo
	identity string
	borrowed bool
}

func openRestoreScope(path string) (*restoreScope, error) {
	if err := validateBoundRestorePlatform(); err != nil {
		return nil, err
	}
	path = filepath.Clean(path)
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: restore root %s is not a real directory", ErrStale, path)
	}
	root, err := rootedfs.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*restoreScope, error) {
		return nil, errors.Join(err, root.Close())
	}
	opened, err := root.Stat(".")
	if err != nil {
		return fail(err)
	}
	linked, linkErr := os.Lstat(path)
	if linkErr != nil || !linked.IsDir() || linked.Mode()&fs.ModeSymlink != 0 ||
		!os.SameFile(before, opened) || !os.SameFile(opened, linked) {
		return fail(errors.Join(linkErr,
			fmt.Errorf("%w: restore root %s changed identity while it was opened", ErrStale, path)))
	}
	identity, err := boundRootIdentity(root)
	if err != nil {
		return fail(fmt.Errorf("identifying restore root %s: %w", path, err))
	}
	return &restoreScope{root: root, path: path, info: opened, identity: identity}, nil
}

func (s *restoreScope) close() error {
	if s == nil || s.root == nil {
		return nil
	}
	var err error
	if !s.borrowed {
		err = s.root.Close()
	}
	s.root = nil
	return err
}

func borrowRestoreScope(path string, root *os.Root) (*restoreScope, error) {
	if root == nil {
		return nil, errors.New("restore scope has no retained root")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	path = filepath.Clean(path)
	linked, err := os.Lstat(path)
	if err != nil || !linked.IsDir() || linked.Mode()&fs.ModeSymlink != 0 {
		return nil, errors.Join(err, fmt.Errorf("%w: restore root %s is not a linked physical directory", ErrStale, path))
	}
	bound, err := root.Stat(".")
	if err != nil || !bound.IsDir() || !os.SameFile(linked, bound) {
		return nil, errors.Join(err, fmt.Errorf("%w: restore root %s does not name its retained capability", ErrStale, path))
	}
	identity, err := boundRootIdentity(root)
	if err != nil {
		return nil, err
	}
	return &restoreScope{root: root, path: path, info: bound, identity: identity, borrowed: true}, nil
}

func openFilesystemRestoreScope(path string) (*restoreScope, string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, "", err
	}
	volumeRoot := filepath.VolumeName(abs) + string(filepath.Separator)
	if volumeRoot == "" {
		volumeRoot = string(filepath.Separator)
	}
	scope, err := openRestoreScope(volumeRoot)
	if err != nil {
		return nil, "", err
	}
	target, err := resolvedWorkspaceTarget(abs, scope.path)
	if err != nil {
		_ = scope.close()
		return nil, "", err
	}
	return scope, target, nil
}

func (s *restoreScope) relative(path string) (string, error) {
	if s == nil || s.root == nil {
		return "", errors.New("restore scope is closed")
	}
	path = filepath.Clean(path)
	rel, err := filepath.Rel(s.path, path)
	if err != nil || rel == "." || !filepath.IsLocal(rel) || filepath.IsAbs(rel) {
		return "", errors.Join(err, fmt.Errorf("%w: restore path %s is outside %s", ErrStale, path, s.path))
	}
	return rel, nil
}

func (s *restoreScope) validateLinked() error {
	if s == nil || s.root == nil || s.info == nil {
		return errors.New("restore scope is closed")
	}
	linked, err := os.Lstat(s.path)
	if err != nil || !linked.IsDir() || linked.Mode()&fs.ModeSymlink != 0 || !os.SameFile(s.info, linked) {
		return errors.Join(err,
			fmt.Errorf("%w: restore root %s changed identity", ErrStale, s.path))
	}
	return nil
}

func (s *restoreScope) parentInfo(path string) (fs.FileInfo, error) {
	rel, err := s.relative(path)
	if err != nil {
		return nil, err
	}
	parent, err := rootedfs.OpenRootAt(s.root, filepath.Dir(rel))
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	info, err := parent.Stat(".")
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%w: parent of %s is not a real directory", ErrStale, path)
	}
	return info, nil
}

func (s *restoreScope) fingerprint(path string, maxBytes int64) (fingerprint, error) {
	rel, err := s.relative(path)
	if err != nil {
		return fingerprint{}, err
	}
	return fingerprintInRoot(s.root, rel, path, maxBytes, nil, nil)
}

// boundRestoreParent owns the directory capability used from compare through
// publication and verification. The display path is never used for I/O.
type boundRestoreParent struct {
	root    *os.Root
	name    string
	display string
}

// boundRestoreNamespace names the target and its siblings from the retained
// workspace root. The parent capability remains useful for descriptor reads
// and directory durability, but namespace mutations must use this root: a
// directory moved out of the workspace before the atomic syscall then becomes
// unreachable instead of carrying publication to its new location.
type boundRestoreNamespace struct {
	root    *os.Root
	target  string
	dir     string
	display string
}

func openBoundRestoreNamespace(scope *restoreScope, parent *boundRestoreParent, path string) (*boundRestoreNamespace, error) {
	if parent == nil || parent.root == nil {
		return nil, errors.New("restore parent is closed")
	}
	if scope == nil {
		return &boundRestoreNamespace{
			root: parent.root, target: parent.name, dir: ".", display: path,
		}, nil
	}
	rel, err := scope.relative(path)
	if err != nil {
		return nil, err
	}
	if filepath.Base(rel) != parent.name {
		return nil, fmt.Errorf("%w: restore target changed while its namespace was bound", ErrStale)
	}
	return &boundRestoreNamespace{
		root: scope.root, target: rel, dir: filepath.Dir(rel), display: path,
	}, nil
}

func (n *boundRestoreNamespace) sibling(name string) (string, error) {
	if n == nil || n.root == nil {
		return "", errors.New("restore namespace is closed")
	}
	if name == "" || name == "." || filepath.Base(name) != name || !filepath.IsLocal(name) {
		return "", fmt.Errorf("%w: invalid restore sibling name %q", ErrStale, name)
	}
	if n.dir == "." {
		return name, nil
	}
	return filepath.Join(n.dir, name), nil
}

func (n *boundRestoreNamespace) fingerprintTarget(maxBytes int64, expected fs.FileInfo) (fingerprint, error) {
	return fingerprintInRoot(n.root, n.target, n.display, maxBytes, nil, expected)
}

func (n *boundRestoreNamespace) createTemp(requested string) (*os.File, string, string, error) {
	create := func(name string) (*os.File, string, string, error) {
		rel, err := n.sibling(name)
		if err != nil {
			return nil, "", "", err
		}
		f, err := n.root.OpenFile(rel, os.O_RDWR|os.O_CREATE|os.O_EXCL|os.O_SYNC, 0o600)
		if err != nil {
			return nil, "", "", err
		}
		if lockErr := acquireRestoreLivenessLock(f); lockErr != nil {
			_ = retireBoundOpenFile(n.root, rel, f, true, nil, nil)
			_ = f.Close()
			return nil, "", "", fmt.Errorf("locking undo temporary file: %w", lockErr)
		}
		return f, name, rel, nil
	}
	if requested != "" {
		if !isRestoreTempName(requested) {
			return nil, "", "", fmt.Errorf("invalid recorded undo temporary name %q", requested)
		}
		return create(requested)
	}
	for range 100 {
		name, err := randomRestoreName()
		if err != nil {
			return nil, "", "", err
		}
		f, leaf, rel, err := create(name)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return f, leaf, rel, err
	}
	return nil, "", "", errors.New("could not allocate a unique undo temporary file")
}

func openBoundRestoreParent(scope *restoreScope, path string, st *fileState) (*boundRestoreParent, error) {
	if err := validateBoundRestorePlatform(); err != nil {
		return nil, err
	}
	if !st.parentSet || st.parent == nil {
		return nil, fmt.Errorf("%w: no trustworthy parent identity was captured for %s", ErrStale, path)
	}
	if scope == nil {
		if err := validateParentIdentity(path, st); err != nil {
			return nil, err
		}
	} else if err := scope.validateLinked(); err != nil {
		return nil, err
	}

	var (
		parent *os.Root
		err    error
	)
	if scope == nil {
		parent, err = rootedfs.OpenRoot(filepath.Dir(path))
	} else {
		rel, relErr := scope.relative(path)
		if relErr != nil {
			return nil, relErr
		}
		parent, err = rootedfs.OpenRootAt(scope.root, filepath.Dir(rel))
	}
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*boundRestoreParent, error) {
		return nil, errors.Join(err, parent.Close())
	}
	opened, err := parent.Stat(".")
	if err != nil {
		return fail(err)
	}
	if !opened.IsDir() || opened.Mode()&fs.ModeSymlink != 0 || !os.SameFile(st.parent, opened) {
		return fail(fmt.Errorf("%w: parent of %s changed identity while it was bound", ErrStale, path))
	}
	// Keep the historical full-chain check for its stronger stale-reporting
	// contract. Security does not depend on this second pathname observation:
	// publication is later resolved from the retained workspace namespace even
	// if this parent handle is renamed immediately after the check.
	if len(st.parents) > 0 {
		if err := validateParentIdentity(path, st); err != nil {
			return fail(err)
		}
	} else if scope != nil {
		if err := scope.validateLinked(); err != nil {
			return fail(err)
		}
	}
	name := filepath.Base(path)
	if name == "." || name == string(filepath.Separator) || strings.ContainsRune(name, filepath.Separator) {
		return fail(fmt.Errorf("%w: invalid restore target %s", ErrStale, path))
	}
	return &boundRestoreParent{root: parent, name: name, display: path}, nil
}

func (p *boundRestoreParent) close() error {
	if p == nil || p.root == nil {
		return nil
	}
	err := p.root.Close()
	p.root = nil
	return err
}

func (p *boundRestoreParent) fingerprint(maxBytes int64) (fingerprint, error) {
	return fingerprintInRoot(p.root, p.name, p.display, maxBytes, nil, nil)
}

func (p *boundRestoreParent) fingerprintLinkedTo(maxBytes int64, expected fs.FileInfo) (fingerprint, error) {
	return fingerprintInRoot(p.root, p.name, p.display, maxBytes, nil, expected)
}

func fingerprintInRoot(root *os.Root, name, display string, maxBytes int64, afterHash func(), expected fs.FileInfo) (fingerprint, error) {
	return fingerprintInRootWithHooks(root, name, display, maxBytes, nil, afterHash, expected)
}

func fingerprintInRootWithHooks(root *os.Root, name, display string, maxBytes int64, beforeOpen, afterHash func(), expected fs.FileInfo) (fingerprint, error) {
	linfo, err := root.Lstat(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fingerprint{}, nil
		}
		return fingerprint{}, err
	}
	if !linfo.Mode().IsRegular() {
		return fingerprint{}, fmt.Errorf("%s is not a regular file", display)
	}
	if expected != nil && !os.SameFile(expected, linfo) {
		return fingerprint{}, fmt.Errorf("%w: %s is not the temporary inode that was published", ErrStale, display)
	}
	if maxBytes >= 0 && linfo.Size() > maxBytes {
		return fingerprint{}, fmt.Errorf("%w: %s exceeds the %d-byte fingerprint bound", ErrSnapshotTooLarge, display, maxBytes)
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	f, err := openCheckpointRootRead(root, name)
	if err != nil {
		return fingerprint{}, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return fingerprint{}, err
	}
	if !os.SameFile(linfo, opened) {
		return fingerprint{}, fmt.Errorf("%w: %s changed identity while it was opened", ErrStale, display)
	}
	h := sha256.New()
	reader := io.Reader(f)
	if maxBytes >= 0 {
		reader = io.LimitReader(f, maxBytes+1)
	}
	n, err := io.Copy(h, reader)
	if err != nil {
		return fingerprint{}, err
	}
	if maxBytes >= 0 && n > maxBytes {
		return fingerprint{}, fmt.Errorf("%w: %s grew beyond the %d-byte fingerprint bound", ErrSnapshotTooLarge, display, maxBytes)
	}
	finished, err := f.Stat()
	if err != nil {
		return fingerprint{}, err
	}
	if !os.SameFile(opened, finished) || opened.Size() != finished.Size() || finished.Size() != n ||
		!opened.ModTime().Equal(finished.ModTime()) || restorableMode(opened.Mode()) != restorableMode(finished.Mode()) {
		return fingerprint{}, fmt.Errorf("%w: %s changed while it was fingerprinted", ErrStale, display)
	}
	if afterHash != nil {
		afterHash()
	}
	linked, err := root.Lstat(name)
	if err != nil || !linked.Mode().IsRegular() || !os.SameFile(finished, linked) ||
		linked.Size() != finished.Size() || !linked.ModTime().Equal(finished.ModTime()) ||
		restorableMode(linked.Mode()) != restorableMode(finished.Mode()) {
		return fingerprint{}, errors.Join(err,
			fmt.Errorf("%w: %s changed identity while it was fingerprinted", ErrStale, display))
	}
	fp := fingerprint{existed: true, mode: restorableMode(finished.Mode()), size: finished.Size()}
	copy(fp.digest[:], h.Sum(nil))
	return fp, nil
}

func (p *boundRestoreParent) createTemp(requested string) (*os.File, string, error) {
	if requested != "" {
		name, err := p.unusedRestoreName(requested)
		if err != nil {
			return nil, "", err
		}
		return p.createTempNamed(name)
	}
	for range 100 {
		name, err := randomRestoreName()
		if err != nil {
			return nil, "", err
		}
		f, _, err := p.createTempNamed(name)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		return f, name, err
	}
	return nil, "", errors.New("could not allocate a unique undo temporary file")
}

func (p *boundRestoreParent) createTempNamed(name string) (*os.File, string, error) {
	f, err := p.root.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL|os.O_SYNC, 0o600)
	if err != nil {
		return nil, "", err
	}
	if lockErr := acquireRestoreLivenessLock(f); lockErr != nil {
		_ = retireBoundOpenFile(p.root, name, f, true, nil, nil)
		_ = f.Close()
		return nil, "", fmt.Errorf("locking undo temporary file: %w", lockErr)
	}
	return f, name, nil
}

func randomRestoreName() (string, error) {
	var random [16]byte
	if _, err := io.ReadFull(rand.Reader, random[:]); err != nil {
		return "", fmt.Errorf("naming undo temporary file: %w", err)
	}
	return ".switchboard-undo-" + hex.EncodeToString(random[:]), nil
}

func isRestoreTempName(name string) bool {
	const prefix = ".switchboard-undo-"
	if len(name) != len(prefix)+32 || !strings.HasPrefix(name, prefix) {
		return false
	}
	_, err := hex.DecodeString(name[len(prefix):])
	return err == nil
}

func (p *boundRestoreParent) unusedRestoreName(requested string) (string, error) {
	if requested != "" {
		if !isRestoreTempName(requested) {
			return "", fmt.Errorf("invalid recorded undo temporary name %q", requested)
		}
		if _, err := p.root.Lstat(requested); errors.Is(err, fs.ErrNotExist) {
			return requested, nil
		} else if err != nil {
			return "", err
		}
		return "", fmt.Errorf("%w: recorded undo temporary %s already exists", ErrStale, requested)
	}
	for range 100 {
		name, err := randomRestoreName()
		if err != nil {
			return "", err
		}
		if _, err := p.root.Lstat(name); errors.Is(err, fs.ErrNotExist) {
			return name, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", errors.New("could not allocate a unique undo tombstone name")
}

func randomQuarantineName() (string, error) {
	var random [16]byte
	if _, err := io.ReadFull(rand.Reader, random[:]); err != nil {
		return "", fmt.Errorf("naming checkpoint quarantine: %w", err)
	}
	return ".switchboard-quarantine-" + hex.EncodeToString(random[:]), nil
}

func unusedQuarantineName(root *os.Root) (string, error) {
	for range 100 {
		name, err := randomQuarantineName()
		if err != nil {
			return "", err
		}
		if _, err := root.Lstat(name); errors.Is(err, fs.ErrNotExist) {
			return name, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", errors.New("could not allocate a checkpoint quarantine name")
}

func unusedQuarantineSibling(root *os.Root, source string) (string, error) {
	dir := filepath.Dir(source)
	for range 100 {
		leaf, err := randomQuarantineName()
		if err != nil {
			return "", err
		}
		name := leaf
		if dir != "." {
			name = filepath.Join(dir, leaf)
		}
		if _, err := root.Lstat(name); errors.Is(err, fs.ErrNotExist) {
			return name, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", errors.New("could not allocate a checkpoint quarantine name")
}

func retiredSinkName(name string) string {
	digest := sha256.Sum256([]byte("switchboard checkpoint retired\x00" + name))
	return ".switchboard-retired-" + hex.EncodeToString(digest[:16])
}

func restoreStagingName(name string) string {
	dir, leaf := filepath.Dir(name), filepath.Base(name)
	digest := sha256.Sum256([]byte("switchboard checkpoint staging\x00" + leaf))
	staging := ".switchboard-staging-" + hex.EncodeToString(digest[:16])
	if dir == "." {
		return staging
	}
	return filepath.Join(dir, staging)
}

// restoreExchangeStagingName is stable in both directions of an exchange.
// The cleanup ledger records the restore temporary leaf, so a rollback must
// stage beside that same temporary rather than deriving a second name from the
// ordinary target path.
func restoreExchangeStagingName(left, right string) string {
	dir := filepath.Dir(left)
	leftLeaf, rightLeaf := filepath.Base(left), filepath.Base(right)
	if rightLeaf < leftLeaf {
		leftLeaf, rightLeaf = rightLeaf, leftLeaf
	}
	digest := sha256.Sum256([]byte("switchboard checkpoint exchange staging\x00" + leftLeaf + "\x00" + rightLeaf))
	staging := ".switchboard-staging-" + hex.EncodeToString(digest[:16])
	if dir == "." {
		return staging
	}
	return filepath.Join(dir, staging)
}

// scrubBoundOpenFile erases sensitive checkpoint bytes through the descriptor
// that selected them. A hostile same-directory writer can move names, but it
// cannot redirect this operation to its replacement inode.
func scrubBoundOpenFile(file *os.File) error {
	if err := file.Truncate(0); err != nil {
		return err
	}
	return file.Sync()
}
