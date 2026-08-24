// Package workspace provides the revision-aware, workspace-contained file
// identity used by human-facing editor features. A Location is useful only
// together with the exact bytes it was derived from: callers must Verify it
// before turning a viewed range into an attachment or mutation.
package workspace

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

const DefaultDocumentLimit int64 = 4 << 20

var (
	ErrOutsideRoot           = errors.New("path is outside the workspace")
	ErrStaleLocation         = errors.New("source location is stale")
	ErrBinary                = errors.New("file is binary")
	ErrTooLarge              = errors.New("file is too large")
	ErrNotRegular            = errors.New("file is not a regular file")
	ErrSecureReadUnsupported = errors.New("secure workspace reads are unsupported")
)

// Position is a human-facing, one-based line and column. LSP wire positions
// intentionally use their own zero-based UTF-16 type in internal/lsp.
type Position struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

// Range is half-open. A zero End means the location names the whole line or
// file, depending on the surface presenting it.
type Range struct {
	Start Position `json:"start"`
	End   Position `json:"end,omitempty"`
}

// Revision is the exact regular-file snapshot behind a view. Size is carried
// for useful diagnostics; SHA256 is the authority.
type Revision struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Location is workspace-relative and slash-normalized. It never carries an
// absolute host path into a transcript or provider request.
type Location struct {
	Path     string   `json:"path"`
	Range    Range    `json:"range,omitempty"`
	Revision Revision `json:"revision"`
}

func (l Location) String() string {
	if l.Range.Start.Line <= 0 {
		return l.Path
	}
	if l.Range.Start.Column <= 0 {
		return fmt.Sprintf("%s:%d", l.Path, l.Range.Start.Line)
	}
	return fmt.Sprintf("%s:%d:%d", l.Path, l.Range.Start.Line, l.Range.Start.Column)
}

// Document is one immutable regular-file snapshot. Read refuses binary
// content; ReadBinary preserves it.
type Document struct {
	Location Location `json:"location"`
	Content  []byte   `json:"-"`
	Mode     fs.FileMode
}

// Root is a canonical workspace boundary.
type Root struct {
	path     string
	identity os.FileInfo
}

func Open(root string) (*Root, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("workspace root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolving workspace root: %w", err)
	}
	// EvalSymlinks selects the intended root. Bind that selection to an opened
	// root handle before retaining its identity, so replacing the directory (or
	// replacing it with a symlink) across this seam cannot bless another tree.
	expected, err := os.Lstat(real)
	if err != nil {
		return nil, err
	}
	if !expected.IsDir() {
		return nil, fmt.Errorf("workspace root %s is not a directory", root)
	}
	rooted, err := rootedfs.OpenRoot(real)
	if err != nil {
		return nil, err
	}
	opened, statErr := rooted.Stat(".")
	closeErr := rooted.Close()
	if statErr != nil {
		return nil, statErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if !os.SameFile(expected, opened) {
		return nil, fmt.Errorf("%w: workspace root changed while it was opened", ErrStaleLocation)
	}
	return &Root{path: filepath.Clean(real), identity: opened}, nil
}

func (r *Root) Path() string { return r.path }

// Resolve follows existing symlinks before applying the workspace boundary.
// The target must exist; editor views never create paths.
func (r *Root) Resolve(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("path is required")
	}
	candidate := name
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(r.path, filepath.FromSlash(candidate))
	}
	real, err := filepath.EvalSymlinks(filepath.Clean(candidate))
	if err != nil {
		return "", err
	}
	if !within(r.path, real) {
		return "", fmt.Errorf("%w: %s", ErrOutsideRoot, name)
	}
	return real, nil
}

func (r *Root) Relative(abs string) (string, error) {
	real, err := filepath.EvalSymlinks(filepath.Clean(abs))
	if err != nil {
		return "", err
	}
	if !within(r.path, real) {
		return "", fmt.Errorf("%w: %s", ErrOutsideRoot, abs)
	}
	rel, err := filepath.Rel(r.path, real)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

func within(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ReadBinary returns an exact, bounded regular-file snapshot, including
// binary content. Resolution selects an internal target, then the platform
// opener binds containment and the read to one descriptor or handle. A
// pathname is never re-opened after its security checks.
func (r *Root) ReadBinary(name string, limit int64) (Document, error) {
	if limit <= 0 {
		limit = DefaultDocumentLimit
	}
	rel, err := r.resolveReadPath(name)
	if err != nil {
		return Document{}, err
	}
	return r.readBinaryResolved(name, rel, limit)
}

// resolveReadPath deliberately returns the canonical workspace-relative
// target rather than the original spelling. This preserves ordinary internal
// symlinks while allowing the platform opener to reject any symlink or parent
// replacement that occurs after resolution.
func (r *Root) resolveReadPath(name string) (string, error) {
	abs, err := r.Resolve(name)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(r.path, abs)
	if err != nil {
		return "", err
	}
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", ErrOutsideRoot, name)
	}
	return clean, nil
}

func (r *Root) readBinaryResolved(name, rel string, limit int64) (Document, error) {
	return r.readBinaryResolvedWithHook(name, rel, limit, nil)
}

func (r *Root) readBinaryResolvedWithHook(name, rel string, limit int64, afterOpen func()) (Document, error) {
	file, err := openWorkspaceReadFile(r.path, r.identity, rel)
	if err != nil {
		return Document{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Document{}, err
	}
	if !info.Mode().IsRegular() {
		return Document{}, fmt.Errorf("%w: %s", ErrNotRegular, name)
	}
	if info.Size() > limit {
		return Document{}, fmt.Errorf("%w: %s is %d bytes (limit %d)", ErrTooLarge, name, info.Size(), limit)
	}
	if afterOpen != nil {
		afterOpen()
	}
	document, err := readInspectedFile(file, info, name, rel, limit)
	if err != nil {
		return Document{}, err
	}
	if err := r.verifyRootPath(); err != nil {
		return Document{}, err
	}
	return document, nil
}

func (r *Root) verifyRootPath() error {
	current, err := os.Lstat(r.path)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(r.identity, current) {
		return fmt.Errorf("%w: workspace root changed while it was read", ErrStaleLocation)
	}
	return nil
}

func (r *Root) openCapability() (*os.Root, error) {
	capability, err := openVerifiedWorkspaceRoot(r.path, r.identity)
	if err != nil {
		return nil, err
	}
	if err := r.verifyCapability(capability); err != nil {
		_ = capability.Close()
		return nil, err
	}
	return capability, nil
}

func (r *Root) verifyCapability(capability *os.Root) error {
	if capability == nil {
		return fmt.Errorf("%w: workspace root capability is unavailable", ErrStaleLocation)
	}
	opened, err := capability.Stat(".")
	if err != nil || !os.SameFile(r.identity, opened) {
		return fmt.Errorf("%w: workspace root capability changed", ErrStaleLocation)
	}
	return r.verifyRootPath()
}

// openVerifiedWorkspaceRoot returns a per-call root handle bound to the exact
// directory identity captured by Open. On supported platforms os.Root keeps
// referring to that directory across renames, so descendants are opened
// relative to the handle rather than by joining another pathname.
func openVerifiedWorkspaceRoot(path string, identity os.FileInfo) (*os.Root, error) {
	rooted, err := rootedfs.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	opened, err := rooted.Stat(".")
	if err != nil {
		_ = rooted.Close()
		return nil, err
	}
	if !os.SameFile(identity, opened) {
		_ = rooted.Close()
		return nil, fmt.Errorf("%w: workspace root changed", ErrStaleLocation)
	}
	return rooted, nil
}

// readInspectedFile is split from the descriptor inspection so tests can
// deterministically grow the same already-open file across the check/read
// seam. Production reaches it only with the FileInfo returned by that handle.
func readInspectedFile(file *os.File, info os.FileInfo, name, rel string, limit int64) (Document, error) {
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return Document{}, err
	}
	if int64(len(data)) > limit {
		return Document{}, fmt.Errorf("%w: %s exceeds %d bytes", ErrTooLarge, name, limit)
	}
	afterFD, err := file.Stat()
	if err != nil {
		return Document{}, err
	}
	if int64(len(data)) != info.Size() || info.Size() != afterFD.Size() ||
		!info.ModTime().Equal(afterFD.ModTime()) {
		return Document{}, fmt.Errorf("%w: %s changed while it was read", ErrStaleLocation, name)
	}
	revision := revision(data)
	return Document{
		Location: Location{Path: filepath.ToSlash(rel), Revision: revision},
		Content:  data,
		Mode:     info.Mode(),
	}, nil
}

// Read returns an exact, bounded regular-file snapshot. Binary data is
// refused instead of being painted as source text.
func (r *Root) Read(name string, limit int64) (Document, error) {
	doc, err := r.ReadBinary(name, limit)
	if err != nil {
		return Document{}, err
	}
	if bytes.IndexByte(doc.Content, 0) >= 0 || !utf8.Valid(doc.Content) {
		return Document{}, fmt.Errorf("%w: %s", ErrBinary, name)
	}
	return doc, nil
}

// Verify proves that a location still names the bytes that were viewed.
func (r *Root) Verify(location Location) error {
	doc, err := r.Read(location.Path, max(DefaultDocumentLimit, location.Revision.Size))
	if err != nil {
		return err
	}
	if doc.Location.Revision != location.Revision {
		return fmt.Errorf("%w: %s changed from %s to %s", ErrStaleLocation, location.Path,
			location.Revision.SHA256, doc.Location.Revision.SHA256)
	}
	return nil
}

func revision(data []byte) Revision {
	sum := sha256.Sum256(data)
	return Revision{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}
}
