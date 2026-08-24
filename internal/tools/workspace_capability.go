package tools

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

var workspaceRootBeforeOpenTestHook func(string)

// bindWorkspaceRootIdentity proves the canonical workspace pathname names one
// physical directory. Registry operations reopen that directory and compare
// against this identity before every filesystem-capability use.
func bindWorkspaceRootIdentity(path string) (os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return nil, errors.New("workspace root is not a physical directory")
	}
	if workspaceRootBeforeOpenTestHook != nil {
		workspaceRootBeforeOpenTestHook(path)
	}
	root, err := rootedfs.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	opened, err := root.Stat(".")
	if err != nil {
		return nil, err
	}
	current, currentErr := os.Lstat(path)
	if currentErr != nil || current.Mode()&os.ModeSymlink != 0 || !current.IsDir() ||
		!os.SameFile(before, opened) || !os.SameFile(opened, current) {
		return nil, errors.Join(currentErr, errors.New("workspace root changed while it was opened"))
	}
	return opened, nil
}

// openWorkspaceRoot returns a handle-relative capability for the exact root
// selected when the registry was assembled. A renamed or replaced workspace
// fails closed rather than redirecting a later tool through the new pathname.
func (r *Registry) openWorkspaceRoot() (*os.Root, error) {
	if r == nil || r.root == "" || r.rootInfo == nil {
		return nil, errors.New("workspace root is not bound")
	}
	root, err := rootedfs.OpenRoot(r.root)
	if err != nil {
		return nil, err
	}
	if err := r.verifyWorkspaceRoot(root); err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

func (r *Registry) verifyWorkspaceRoot(root *os.Root) error {
	opened, err := root.Stat(".")
	if err != nil {
		return err
	}
	current, currentErr := os.Lstat(r.root)
	if currentErr != nil || current.Mode()&os.ModeSymlink != 0 || !current.IsDir() ||
		!os.SameFile(r.rootInfo, opened) || !os.SameFile(opened, current) {
		return errors.Join(currentErr, errors.New("workspace root changed after session assembly"))
	}
	return nil
}

// workspaceRelative converts a path already accepted by resolve into the name
// consumed by os.Root. Repeating the lexical boundary check here keeps a
// planned absolute pathname from becoming authority by itself.
func (r *Registry) workspaceRelative(abs string) (string, error) {
	rel, err := filepath.Rel(r.root, filepath.Clean(abs))
	if err != nil {
		return "", err
	}
	rel = filepath.Clean(rel)
	if filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errOutsideWorkspace
	}
	if rel == "" {
		rel = "."
	}
	return rel, nil
}

func (r *Registry) openResolvedWorkspace(abs string) (*os.Root, string, error) {
	rel, err := r.workspaceRelative(abs)
	if err != nil {
		return nil, "", err
	}
	root, err := r.openWorkspaceRoot()
	if err != nil {
		return nil, "", err
	}
	return root, rel, nil
}

// bindWorkspaceParent walks one physical directory at a time beneath root.
// It rejects symlink components even when they point back inside the workspace:
// mutation identity is the path the user approved, not a retargetable alias.
func bindWorkspaceParent(root *os.Root, relative string, create bool) (*os.Root, os.FileInfo, error) {
	relative = filepath.Clean(relative)
	if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, nil, errOutsideWorkspace
	}
	current, err := rootedfs.OpenRootAt(root, ".")
	if err != nil {
		return nil, nil, err
	}
	fail := func(cause error) (*os.Root, os.FileInfo, error) {
		_ = current.Close()
		return nil, nil, cause
	}
	if relative != "." && relative != "" {
		for _, component := range strings.Split(relative, string(filepath.Separator)) {
			if component == "" || component == "." || component == ".." {
				return fail(fmt.Errorf("invalid workspace parent component %q", component))
			}
			before, statErr := current.Lstat(component)
			if errors.Is(statErr, fs.ErrNotExist) && create {
				if mkdirErr := current.Mkdir(component, 0o755); mkdirErr != nil && !errors.Is(mkdirErr, fs.ErrExist) {
					return fail(mkdirErr)
				}
				before, statErr = current.Lstat(component)
			}
			if statErr != nil {
				return fail(statErr)
			}
			if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
				return fail(fmt.Errorf("workspace parent component %q is not a physical directory", component))
			}
			child, openErr := rootedfs.OpenRootAt(current, component)
			if openErr != nil {
				return fail(openErr)
			}
			opened, openStatErr := child.Stat(".")
			after, afterErr := current.Lstat(component)
			if openStatErr != nil || afterErr != nil || after.Mode()&os.ModeSymlink != 0 || !after.IsDir() ||
				!os.SameFile(before, opened) || !os.SameFile(opened, after) {
				_ = child.Close()
				return fail(errors.Join(openStatErr, afterErr, errors.New("workspace parent changed while it was opened")))
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

func (r *Registry) verifyWorkspaceParent(root *os.Root, relative string, expected os.FileInfo) error {
	if err := r.verifyWorkspaceRoot(root); err != nil {
		return err
	}
	current, info, err := bindWorkspaceParent(root, relative, false)
	if err != nil {
		return err
	}
	defer current.Close()
	if expected == nil || !os.SameFile(expected, info) {
		return errors.New("workspace parent changed before commit")
	}
	return nil
}
