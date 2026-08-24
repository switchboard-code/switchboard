package rootedfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// WalkLimits bounds both work and memory while enumerating an untrusted tree.
// MaxDirectories includes base. MaxDepth counts descendant directories from
// base, so zero visits regular files in base but enters no child directory.
type WalkLimits struct {
	MaxEntries     int
	MaxDirectories int
	MaxDepth       int
	ReadDirBatch   int
}

// WalkStatus describes how much of a tree WalkRegularFiles inspected. Policy
// skips requested by skipDir are complete by definition and do not contribute
// to Omitted. Every other uninspected portion makes Partial report true.
type WalkStatus struct {
	Entries          int
	Directories      int
	Omitted          int
	EntryLimit       bool
	DirectoryLimit   bool
	DepthLimit       bool
	StoppedEarly     bool
	AuthorityChanged bool
}

func (s WalkStatus) Partial() bool {
	return s.Omitted > 0 || s.EntryLimit || s.DirectoryLimit || s.DepthLimit || s.StoppedEarly || s.AuthorityChanged
}

// WalkRegularFiles enumerates regular files beneath base using retained
// directory capabilities. Directory reads are positive-size batches and only
// one batch is sorted at a time, so a single attacker-controlled directory can
// neither allocate nor sort an unbounded entry list.
//
// Symlinks and non-regular files are never followed and count as observed
// omissions. skipDir is a policy exclusion and therefore does not make the
// result partial. visit receives a parent capability that is valid only for
// the duration of the callback;
// callers that read the file should use parent and name rather than reopening
// relative through root. Because an ancestor rename can be detected only as
// the retained directory stack unwinds, content readers must stage their
// results and discard them when AuthorityChanged is set. Returning fs.SkipAll
// stops successfully and records StoppedEarly so a caller cannot accidentally
// describe the coverage as complete.
func WalkRegularFiles(
	ctx context.Context,
	root *os.Root,
	base string,
	limits WalkLimits,
	skipDir func(relative string, info fs.FileInfo) bool,
	visit func(relative string, parent *os.Root, name string, info fs.FileInfo) error,
) (WalkStatus, error) {
	var status WalkStatus
	if ctx == nil {
		return status, errors.New("walk context is nil")
	}
	if root == nil {
		return status, errors.New("walk root is nil")
	}
	if limits.MaxEntries <= 0 || limits.MaxDirectories <= 0 || limits.MaxDepth < 0 || limits.ReadDirBatch <= 0 {
		return status, errors.New("walk limits require positive entries, directories, and batch, and non-negative depth")
	}
	if visit == nil {
		return status, errors.New("walk visitor is nil")
	}

	base = filepath.Clean(filepath.FromSlash(base))
	if base == "" {
		base = "."
	}
	if filepath.IsAbs(base) || base == ".." || strings.HasPrefix(base, ".."+string(filepath.Separator)) {
		return status, errors.New("walk base is outside the root")
	}

	baseRoot, baseInfo, err := bindPhysicalDirectory(root, base)
	if err != nil {
		return status, err
	}
	defer baseRoot.Close()
	status.Directories = 1
	if skipDir != nil && skipDir(base, baseInfo) {
		if !samePhysicalDirectory(root, base, baseRoot) {
			status.Omitted++
			status.AuthorityChanged = true
		}
		return status, nil
	}

	state := walkState{
		ctx:     ctx,
		limits:  limits,
		skipDir: skipDir,
		visit:   visit,
		status:  &status,
	}
	walkErr := state.directory(baseRoot, base, baseInfo, 0)
	if !samePhysicalDirectory(root, base, baseRoot) {
		status.Omitted++
		status.AuthorityChanged = true
	}
	if errors.Is(walkErr, fs.SkipAll) {
		status.StoppedEarly = true
		return status, nil
	}
	if walkErr != nil {
		return status, walkErr
	}
	return status, nil
}

type walkState struct {
	ctx     context.Context
	limits  WalkLimits
	skipDir func(string, fs.FileInfo) bool
	visit   func(string, *os.Root, string, fs.FileInfo) error
	status  *WalkStatus
}

func (s *walkState) directory(dir *os.Root, relative string, expected fs.FileInfo, depth int) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	opened, err := dir.Stat(".")
	if err != nil || expected == nil || !opened.IsDir() || !os.SameFile(expected, opened) {
		s.status.Omitted++
		return nil
	}
	file, err := dir.Open(".")
	if err != nil {
		s.status.Omitted++
		return nil
	}
	defer file.Close()

	for {
		if err := s.ctx.Err(); err != nil {
			return err
		}
		remaining := s.limits.MaxEntries - s.status.Entries
		if remaining == 0 {
			// One descriptor-relative sentinel read distinguishes an exact cap
			// from an unknown tail without retaining or sorting that tail.
			entries, readErr := file.ReadDir(1)
			if len(entries) > 0 {
				s.status.EntryLimit = true
				return nil
			}
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			if readErr != nil {
				s.status.Omitted++
			}
			return nil
		}

		batch := min(s.limits.ReadDirBatch, remaining)
		entries, readErr := file.ReadDir(batch)
		// ReadDir preserves filesystem order. Sorting a bounded batch keeps
		// ordinary results stable without ever sorting a whole directory.
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, entry := range entries {
			s.status.Entries++
			if err := s.entry(dir, relative, entry, depth); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			s.status.Omitted++
			return nil
		}
	}
}

func (s *walkState) entry(parent *os.Root, parentRelative string, entry fs.DirEntry, depth int) error {
	name := entry.Name()
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		s.status.Omitted++
		return nil
	}
	relative := filepath.Join(parentRelative, name)
	before, err := parent.Lstat(name)
	if err != nil {
		s.status.Omitted++
		return nil
	}
	if before.Mode()&os.ModeSymlink != 0 {
		s.status.Omitted++
		return nil
	}
	if before.IsDir() {
		if s.skipDir != nil && s.skipDir(relative, before) {
			after, afterErr := parent.Lstat(name)
			if afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !after.IsDir() || !os.SameFile(before, after) {
				s.status.Omitted++
				s.status.AuthorityChanged = true
			}
			return nil
		}
		if depth >= s.limits.MaxDepth {
			s.status.DepthLimit = true
			return nil
		}
		if s.status.Directories >= s.limits.MaxDirectories {
			s.status.DirectoryLimit = true
			return nil
		}
		child, childInfo, err := openPhysicalChild(parent, name, before)
		if err != nil {
			s.status.Omitted++
			return nil
		}
		s.status.Directories++
		err = s.directory(child, relative, childInfo, depth+1)
		_ = child.Close()
		after, afterErr := parent.Lstat(name)
		if afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !after.IsDir() || !os.SameFile(childInfo, after) {
			s.status.Omitted++
			s.status.AuthorityChanged = true
		}
		return err
	}
	if !before.Mode().IsRegular() {
		s.status.Omitted++
		return nil
	}
	visitErr := s.visit(relative, parent, name, before)
	after, afterErr := parent.Lstat(name)
	if afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		s.status.Omitted++
	}
	return visitErr
}

func samePhysicalDirectory(root *os.Root, relative string, expected *os.Root) bool {
	if expected == nil {
		return false
	}
	expectedInfo, err := expected.Stat(".")
	if err != nil || !expectedInfo.IsDir() {
		return false
	}
	rebound, reboundInfo, err := bindPhysicalDirectory(root, relative)
	if err != nil {
		return false
	}
	_ = rebound.Close()
	return os.SameFile(expectedInfo, reboundInfo)
}

func bindPhysicalDirectory(root *os.Root, relative string) (*os.Root, fs.FileInfo, error) {
	current, err := OpenRootAt(root, ".")
	if err != nil {
		return nil, nil, err
	}
	fail := func(cause error) (*os.Root, fs.FileInfo, error) {
		_ = current.Close()
		return nil, nil, cause
	}
	if relative != "." {
		for _, component := range strings.Split(relative, string(filepath.Separator)) {
			if component == "" || component == "." || component == ".." {
				return fail(fmt.Errorf("invalid walk directory component %q", component))
			}
			before, err := current.Lstat(component)
			if err != nil {
				return fail(err)
			}
			child, _, err := openPhysicalChild(current, component, before)
			if err != nil {
				return fail(err)
			}
			_ = current.Close()
			current = child
		}
	}
	info, err := current.Stat(".")
	if err != nil {
		return fail(err)
	}
	return current, info, nil
}

func openPhysicalChild(parent *os.Root, name string, before fs.FileInfo) (*os.Root, fs.FileInfo, error) {
	if before == nil || before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, nil, fmt.Errorf("%s is not a physical directory", name)
	}
	child, err := OpenRootAt(parent, name)
	if err != nil {
		return nil, nil, err
	}
	opened, openedErr := child.Stat(".")
	after, afterErr := parent.Lstat(name)
	if openedErr != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !after.IsDir() ||
		!os.SameFile(before, opened) || !os.SameFile(opened, after) {
		_ = child.Close()
		return nil, nil, errors.Join(openedErr, afterErr, fmt.Errorf("%s changed while its directory capability was opened", name))
	}
	return child, opened, nil
}
