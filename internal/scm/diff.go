package scm

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// DiffSectionKind distinguishes Git-tracked output from explicit untracked
// patches. Other is an untracked non-regular path that Git status can name but
// no safe file patch can represent.
type DiffSectionKind string

const (
	DiffTracked         DiffSectionKind = "tracked"
	DiffUntrackedText   DiffSectionKind = "untracked_text"
	DiffUntrackedBinary DiffSectionKind = "untracked_binary"
	DiffUntrackedOther  DiffSectionKind = "untracked_other"
)

type DiffOptions struct {
	Paths    []string
	MaxBytes int
}

// DiffSection maps one typed file to its byte range in DiffResult.Text.
type DiffSection struct {
	Path         string
	OriginalPath string
	Kind         DiffSectionKind
	Offset       int
	Length       int
	Binary       bool
	Truncated    bool
	Status       PathState
}

// DiffResult is the working tree relative to HEAD, including staged and
// unstaged changes together, plus explicit patches for untracked files.
type DiffResult struct {
	Base      string // "HEAD", or the empty tree for an unborn repository
	Unborn    bool
	Files     []PathState
	Text      []byte
	Sections  []DiffSection
	Omitted   []PathState // complete patches absent because Text reached its cap
	Truncated bool
}

// DiffHEAD returns a bounded patch without changing the index. Ignored files
// remain status metadata and are not included as changes. In an unborn
// repository, current tracked and untracked files are represented as additions
// from the empty tree.
func (r *Repository) DiffHEAD(ctx context.Context, opts DiffOptions) (DiffResult, error) {
	limit, err := diffLimit(opts.MaxBytes)
	if err != nil {
		return DiffResult{}, err
	}
	pathspecs, err := r.pathspecs(opts.Paths)
	if err != nil {
		return DiffResult{}, err
	}
	states, err := r.StatusPaths(ctx, pathspecs)
	if err != nil {
		return DiffResult{}, err
	}
	hasHEAD, err := r.hasHEAD(ctx)
	if err != nil {
		return DiffResult{}, err
	}
	result := DiffResult{Base: "HEAD", Unborn: !hasHEAD, Files: states}
	if !hasHEAD {
		result.Base = "empty tree"
	}

	for i, state := range states {
		if state.Ignored {
			continue
		}
		remaining := limit - len(result.Text)
		if remaining <= 0 {
			result.Truncated = true
			result.Omitted = appendChanged(result.Omitted, states[i:]...)
			break
		}

		section := DiffSection{Path: state.Path, OriginalPath: state.OriginalPath, Status: state}
		var patch []byte
		var truncated bool
		switch {
		case state.Untracked || !hasHEAD:
			section.Kind, patch, truncated, err = r.untrackedPatch(ctx, state, remaining)
		default:
			section.Kind = DiffTracked
			patch, truncated, err = r.trackedPatch(ctx, state, remaining)
		}
		if err != nil {
			return DiffResult{}, err
		}
		section.Binary = binaryPatch(patch)
		if section.Kind == DiffUntrackedText && section.Binary {
			section.Kind = DiffUntrackedBinary
		}
		section.Offset = len(result.Text)
		section.Length = len(patch)
		section.Truncated = truncated
		if len(patch) > 0 || section.Kind == DiffUntrackedOther {
			result.Sections = append(result.Sections, section)
			result.Text = append(result.Text, patch...)
			if len(patch) > 0 && patch[len(patch)-1] != '\n' && len(result.Text) < limit {
				result.Text = append(result.Text, '\n')
				result.Sections[len(result.Sections)-1].Length = len(result.Text) - section.Offset
			}
		}
		if truncated {
			result.Truncated = true
			// The first entry can have a partial section. It belongs in the
			// omitted inventory because its complete patch is not present.
			result.Omitted = append(result.Omitted, state)
			result.Omitted = appendChanged(result.Omitted, states[i+1:]...)
			break
		}
	}
	return result, nil
}

func appendChanged(dst []PathState, states ...PathState) []PathState {
	for _, state := range states {
		if !state.Ignored {
			dst = append(dst, state)
		}
	}
	return dst
}

func diffLimit(requested int) (int, error) {
	if requested == 0 {
		return DefaultMaxDiffBytes, nil
	}
	if requested < 0 || requested > MaxDiffBytes {
		return 0, fmt.Errorf("diff byte limit must be between 1 and %d", MaxDiffBytes)
	}
	return requested, nil
}

func (r *Repository) hasHEAD(ctx context.Context) (bool, error) {
	if err := r.executionAllowed(); err != nil {
		return false, err
	}
	result := runGit(ctx, r.git, r.Root, maxDiagnosticBytes,
		"rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	if result.err == nil {
		return true, nil
	}
	if result.exitCode() == 1 {
		return false, nil
	}
	return false, result.commandError("resolving Git HEAD")
}

func (r *Repository) trackedPatch(ctx context.Context, state PathState, limit int) ([]byte, bool, error) {
	if err := r.executionAllowed(); err != nil {
		return nil, false, err
	}
	paths := []string{state.Path}
	if state.OriginalPath != "" && state.OriginalPath != state.Path {
		paths = append(paths, state.OriginalPath)
	}
	args := []string{"diff", "--no-ext-diff", "--no-textconv", "--binary", "--full-index", "--find-renames", "HEAD", "--"}
	args = append(args, paths...)
	result := runGit(ctx, r.git, r.Root, limit, args...)
	if result.err != nil {
		return nil, false, result.commandError("reading tracked Git diff for " + state.Path)
	}
	return result.stdout, result.stdoutTruncated, nil
}

func (r *Repository) untrackedPatch(ctx context.Context, state PathState, limit int) (DiffSectionKind, []byte, bool, error) {
	if err := r.executionAllowed(); err != nil {
		return "", nil, false, err
	}
	abs := filepath.Join(r.Root, filepath.FromSlash(state.Path))
	info, err := os.Lstat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			// An unborn repository can hold an index entry whose working-tree
			// file was then removed. Relative to the empty tree, its current
			// working-tree state is still empty.
			return DiffUntrackedText, nil, false, nil
		}
		return "", nil, false, fmt.Errorf("inspecting %s: %w", state.Path, err)
	}
	if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
		return DiffUntrackedOther, nil, false, nil
	}
	args := []string{"diff", "--no-ext-diff", "--no-textconv", "--binary", "--full-index", "--no-index", "--", os.DevNull, state.Path}
	result := runGit(ctx, r.git, r.Root, limit, args...)
	if result.err != nil && result.exitCode() != 1 {
		return "", nil, false, result.commandError("reading untracked Git diff for " + state.Path)
	}
	kind := DiffUntrackedText
	if binaryPatch(result.stdout) {
		kind = DiffUntrackedBinary
	}
	return kind, result.stdout, result.stdoutTruncated, nil
}

func binaryPatch(patch []byte) bool {
	return bytes.Contains(patch, []byte("\nGIT binary patch\n")) ||
		bytes.Contains(patch, []byte("\nBinary files ")) ||
		bytes.HasPrefix(patch, []byte("Binary files "))
}
