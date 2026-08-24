package extensions

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

type pluginStateDirectory struct {
	root        *os.Root
	path        string
	info        fs.FileInfo
	name        string
	journalRoot *os.Root
	journalPath string
	journalInfo fs.FileInfo
}

const pluginStateRecoveryDirectory = ".plugin-state-recovery"

var pluginStateAfterReadTestHook func()

type pluginStateSnapshot struct {
	existed bool
	mode    fs.FileMode
	content []byte
}

func openPluginStateDirectory(statePath string) (*pluginStateDirectory, error) {
	if strings.TrimSpace(statePath) == "" {
		return nil, errors.New("plugin state path is empty")
	}
	abs, err := filepath.Abs(statePath)
	if err != nil {
		return nil, fmt.Errorf("resolving plugin state path: %w", err)
	}
	abs = filepath.Clean(abs)
	name := filepath.Base(abs)
	if name == "." || name == string(filepath.Separator) || !filepath.IsLocal(name) {
		return nil, errors.New("plugin state path has no local file name")
	}
	parentPath := filepath.Dir(abs)
	if err := fileprivacy.EnsurePrivateDir(parentPath); err != nil {
		return nil, fmt.Errorf("securing %s: %w", parentPath, err)
	}
	before, err := os.Lstat(parentPath)
	if err != nil {
		return nil, fmt.Errorf("inspecting plugin state directory: %w", err)
	}
	if before.Mode()&fs.ModeSymlink != 0 || !before.IsDir() {
		return nil, errors.New("plugin state parent is not a physical directory")
	}
	root, err := rootedfs.OpenRoot(parentPath)
	if err != nil {
		return nil, fmt.Errorf("opening plugin state directory capability: %w", err)
	}
	fail := func(cause error) (*pluginStateDirectory, error) {
		return nil, errors.Join(cause, root.Close())
	}
	opened, err := root.Stat(".")
	if err != nil || !opened.IsDir() || !os.SameFile(before, opened) {
		return fail(errors.Join(errors.New("plugin state parent changed while it was opened"), err))
	}
	directory, err := root.Open(".")
	if err != nil {
		return fail(fmt.Errorf("opening plugin state directory descriptor: %w", err))
	}
	ownerOnly, ownerErr := fileprivacy.DirectoryIsOwnerOnly(directory)
	closeErr := directory.Close()
	if ownerErr != nil || closeErr != nil || !ownerOnly {
		return fail(errors.Join(errors.New("plugin state parent is not owner-only"), ownerErr, closeErr))
	}
	linked, err := os.Lstat(parentPath)
	if err != nil || linked.Mode()&fs.ModeSymlink != 0 || !linked.IsDir() || !os.SameFile(opened, linked) {
		return fail(errors.Join(errors.New("plugin state parent changed while it was opened"), err))
	}
	journalRoot, err := fileprivacy.EnsurePrivateDirInRoot(root, pluginStateRecoveryDirectory)
	if err != nil {
		return fail(fmt.Errorf("securing plugin state recovery directory: %w", err))
	}
	journalInfo, err := journalRoot.Stat(".")
	if err != nil {
		_ = journalRoot.Close()
		return fail(fmt.Errorf("identifying plugin state recovery directory: %w", err))
	}
	return &pluginStateDirectory{
		root:        root,
		path:        parentPath,
		info:        opened,
		name:        name,
		journalRoot: journalRoot,
		journalPath: filepath.Join(parentPath, pluginStateRecoveryDirectory),
		journalInfo: journalInfo,
	}, nil
}

func (d *pluginStateDirectory) statePath() string {
	if d == nil {
		return ""
	}
	return filepath.Join(d.path, d.name)
}

func (d *pluginStateDirectory) validateLinked() error {
	if d == nil || d.root == nil || d.info == nil {
		return errors.New("plugin state directory capability is closed")
	}
	opened, err := d.root.Stat(".")
	if err != nil || !opened.IsDir() || !os.SameFile(d.info, opened) {
		return errors.Join(errors.New("plugin state directory capability changed identity"), err)
	}
	linked, err := os.Lstat(d.path)
	if err != nil || linked.Mode()&fs.ModeSymlink != 0 || !linked.IsDir() || !os.SameFile(d.info, linked) {
		return errors.Join(errors.New("plugin state parent no longer names its retained directory"), err)
	}
	journalLinked, err := d.root.Lstat(pluginStateRecoveryDirectory)
	if err != nil || !journalLinked.IsDir() || journalLinked.Mode()&fs.ModeSymlink != 0 ||
		!os.SameFile(d.journalInfo, journalLinked) {
		return errors.Join(errors.New("plugin state recovery directory changed identity"), err)
	}
	journalBound, err := d.journalRoot.Stat(".")
	if err != nil || !journalBound.IsDir() || !os.SameFile(d.journalInfo, journalBound) {
		return errors.Join(errors.New("plugin state recovery capability changed identity"), err)
	}
	journal, err := d.journalRoot.Open(".")
	if err != nil {
		return err
	}
	ownerOnly, ownerErr := fileprivacy.DirectoryIsOwnerOnly(journal)
	closeErr := journal.Close()
	if ownerErr != nil || closeErr != nil || !ownerOnly {
		return errors.Join(errors.New("plugin state recovery directory is not owner-only"), ownerErr, closeErr)
	}
	return nil
}

func (d *pluginStateDirectory) close() error {
	if d == nil || d.root == nil {
		return nil
	}
	var err error
	if d.journalRoot != nil {
		err = d.journalRoot.Close()
		d.journalRoot = nil
	}
	err = errors.Join(err, d.root.Close())
	d.root = nil
	return err
}

func readPluginStateSnapshot(d *pluginStateDirectory) (pluginStateSnapshot, error) {
	if err := d.validateLinked(); err != nil {
		return pluginStateSnapshot{}, err
	}
	path := d.statePath()
	before, err := d.root.Lstat(d.name)
	if errors.Is(err, fs.ErrNotExist) {
		if err := d.validateLinked(); err != nil {
			return pluginStateSnapshot{}, err
		}
		return pluginStateSnapshot{}, nil
	}
	if err != nil {
		return pluginStateSnapshot{}, fmt.Errorf("reading %s: %w", path, err)
	}
	if before.Mode()&fs.ModeSymlink != 0 {
		return pluginStateSnapshot{}, fmt.Errorf("reading %s: plugin state must not be a symbolic link", path)
	}
	if !before.Mode().IsRegular() {
		return pluginStateSnapshot{}, fmt.Errorf("reading %s: plugin state is not a regular file", path)
	}
	if before.Size() < 0 || before.Size() > maxStateBytes {
		return pluginStateSnapshot{}, fmt.Errorf("reading %s: plugin state exceeds %d bytes", path, maxStateBytes)
	}
	f, err := fileprivacy.OpenInRoot(d.root, d.name)
	if err != nil {
		return pluginStateSnapshot{}, fmt.Errorf("reading %s: %w", path, err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return pluginStateSnapshot{}, errors.Join(
			fmt.Errorf("reading %s: plugin state changed while it was opened", path), err)
	}
	ownerOnly, err := fileprivacy.IsOwnerOnly(f)
	if err != nil {
		return pluginStateSnapshot{}, fmt.Errorf("reading %s permissions: %w", path, err)
	}
	if !ownerOnly {
		return pluginStateSnapshot{}, fmt.Errorf("reading %s: plugin state permissions are not owner-only", path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil {
		return pluginStateSnapshot{}, fmt.Errorf("reading %s: %w", path, err)
	}
	if len(raw) > maxStateBytes {
		return pluginStateSnapshot{}, fmt.Errorf("reading %s: plugin state exceeds %d bytes", path, maxStateBytes)
	}
	if pluginStateAfterReadTestHook != nil {
		pluginStateAfterReadTestHook()
	}
	finished, err := f.Stat()
	if err != nil {
		return pluginStateSnapshot{}, fmt.Errorf("reading %s: %w", path, err)
	}
	linked, linkErr := d.root.Lstat(d.name)
	finishedOwnerOnly, ownerErr := fileprivacy.IsOwnerOnly(f)
	if ownerErr != nil || !finishedOwnerOnly || linkErr != nil || !linked.Mode().IsRegular() ||
		!os.SameFile(opened, finished) || !os.SameFile(finished, linked) ||
		opened.Size() != finished.Size() || finished.Size() != int64(len(raw)) ||
		opened.Mode() != finished.Mode() || linked.Mode() != finished.Mode() || linked.Size() != finished.Size() ||
		!opened.ModTime().Equal(finished.ModTime()) || !linked.ModTime().Equal(finished.ModTime()) {
		return pluginStateSnapshot{}, errors.Join(
			fmt.Errorf("reading %s: plugin state changed or lost owner-only permissions while it was read", path),
			ownerErr, linkErr)
	}
	if err := d.validateLinked(); err != nil {
		return pluginStateSnapshot{}, err
	}
	return pluginStateSnapshot{existed: true, mode: finished.Mode(), content: raw}, nil
}
