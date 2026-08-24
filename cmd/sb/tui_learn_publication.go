package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/fileprivacy"
	"github.com/switchboard-code/switchboard/internal/rootedfs"
	"github.com/switchboard-code/switchboard/internal/session"
)

const (
	learnPublicationDirectory = ".learn-publication"
	learnPublicationLockFile  = "publication.lock"
	maxLearnedSkillBytes      = 128 << 10
)

// learnPublicationBeforeCommitTestHook is a deterministic namespace-race seam.
// Production callers leave it nil.
var learnPublicationBeforeCommitTestHook func()

type learnedSkillDirectory struct {
	root *os.Root
	path string
	info os.FileInfo
}

func (d *learnedSkillDirectory) close() error {
	if d == nil || d.root == nil {
		return nil
	}
	err := d.root.Close()
	d.root = nil
	return err
}

func (d *learnedSkillDirectory) verifyOwnerBound() error {
	if d == nil || d.root == nil || d.info == nil {
		return errors.New("learn publication directory capability is closed")
	}
	opened, err := d.root.Stat(".")
	linked, linkErr := os.Lstat(d.path)
	if err != nil || linkErr != nil || !opened.IsDir() || !linked.IsDir() ||
		opened.Mode()&fs.ModeSymlink != 0 || linked.Mode()&fs.ModeSymlink != 0 ||
		!os.SameFile(d.info, opened) || !os.SameFile(opened, linked) {
		return errors.Join(err, linkErr, fmt.Errorf("learn publication directory %s changed identity", d.path))
	}
	directory, err := d.root.Open(".")
	if err != nil {
		return fmt.Errorf("opening learn publication directory %s: %w", d.path, err)
	}
	defer directory.Close()
	// Elevated Windows tokens commonly create ordinary workspace directories
	// with TokenOwner (for example, Administrators) rather than TokenUser. That
	// SID is still part of the current token's owner authority. Accept it for
	// binding the user-selected workspace tree; owner-private recovery files
	// and locks below retain their exact protected TokenUser-only DACL check.
	owned, ownerErr := fileprivacy.IsOwnedByCurrentTokenAuthority(directory)
	if ownerErr != nil || !owned {
		if ownerErr == nil {
			ownerErr = errors.New("directory is not owned by the current user")
		}
		return fmt.Errorf("checking learn publication directory %s: %w", d.path, ownerErr)
	}
	return nil
}

func bindLearnedSkillWorkspace(workspace string) (*learnedSkillDirectory, error) {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolving learn workspace: %w", err)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolving learn workspace: %w", err)
	}
	real = filepath.Clean(real)
	linked, err := os.Lstat(real)
	if err != nil || !linked.IsDir() || linked.Mode()&fs.ModeSymlink != 0 {
		return nil, errors.Join(err, errors.New("learn workspace is not a physical directory"))
	}
	root, err := rootedfs.OpenRoot(real)
	if err != nil {
		return nil, fmt.Errorf("binding learn workspace: %w", err)
	}
	opened, statErr := root.Stat(".")
	if statErr != nil || !opened.IsDir() || !os.SameFile(linked, opened) {
		return nil, errors.Join(statErr, errors.New("learn workspace changed while it was bound"), root.Close())
	}
	dir := &learnedSkillDirectory{root: root, path: real, info: opened}
	if err := dir.verifyOwnerBound(); err != nil {
		return nil, errors.Join(err, dir.close())
	}
	return dir, nil
}

func openLearnedSkillChild(parent *learnedSkillDirectory, name string, create bool) (*learnedSkillDirectory, bool, error) {
	if parent == nil || parent.root == nil || name == "" || !filepath.IsLocal(name) || filepath.Base(name) != name {
		return nil, false, errors.New("invalid learn publication directory component")
	}
	if err := parent.verifyOwnerBound(); err != nil {
		return nil, false, err
	}
	linked, err := parent.root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		if !create {
			return nil, false, nil
		}
		if err := parent.root.Mkdir(name, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, false, fmt.Errorf("creating %s: %w", filepath.Join(parent.path, name), err)
		}
		linked, err = parent.root.Lstat(name)
	}
	if err != nil || !linked.IsDir() || linked.Mode()&fs.ModeSymlink != 0 {
		return nil, false, errors.Join(err,
			fmt.Errorf("learn publication path %s is not a physical directory", filepath.Join(parent.path, name)))
	}
	root, err := rootedfs.OpenRootAt(parent.root, name)
	if err != nil {
		return nil, false, fmt.Errorf("binding learn publication directory %s: %w", filepath.Join(parent.path, name), err)
	}
	opened, statErr := root.Stat(".")
	if statErr != nil || !opened.IsDir() || !os.SameFile(linked, opened) {
		return nil, false, errors.Join(statErr,
			fmt.Errorf("learn publication directory %s changed while it was bound", filepath.Join(parent.path, name)), root.Close())
	}
	child := &learnedSkillDirectory{root: root, path: filepath.Join(parent.path, name), info: opened}
	if err := child.verifyOwnerBound(); err != nil {
		return nil, false, errors.Join(err, child.close())
	}
	return child, true, nil
}

// inspectLearnedSkillDestination is the cheap pre-provider check. It never
// follows a repository symlink and does not create the skill tree. The final
// publisher repeats every check and uses an absent-target CAS, so this check
// is only an early diagnostic, never publication authority.
func inspectLearnedSkillDestination(workspace, name string) (string, bool, error) {
	root, err := bindLearnedSkillWorkspace(workspace)
	if err != nil {
		return "", false, err
	}
	defer root.close()
	current := root
	var opened []*learnedSkillDirectory
	defer func() {
		for i := len(opened) - 1; i >= 0; i-- {
			_ = opened[i].close()
		}
	}()
	for _, component := range []string{".agents", "skills", name} {
		child, exists, err := openLearnedSkillChild(current, component, false)
		if err != nil {
			return "", false, err
		}
		if !exists {
			return filepath.Join(root.path, ".agents", "skills", name, "SKILL.md"), false, nil
		}
		opened = append(opened, child)
		current = child
	}
	dest := filepath.Join(current.path, "SKILL.md")
	_, err = current.root.Lstat("SKILL.md")
	if errors.Is(err, fs.ErrNotExist) {
		return dest, false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("inspecting learned skill destination: %w", err)
	}
	return dest, true, nil
}

type learnPublicationAuthority struct {
	workspace *learnedSkillDirectory
	agents    *learnedSkillDirectory
	skills    *learnedSkillDirectory
	skill     *learnedSkillDirectory
	journal   *learnedSkillDirectory
	lock      *os.File
	locked    bool
}

func (a *learnPublicationAuthority) close() error {
	if a == nil {
		return nil
	}
	var result error
	if a.lock != nil {
		if a.locked {
			result = errors.Join(result, unlockLearnPublication(a.lock))
			a.locked = false
		}
		result = errors.Join(result, a.lock.Close())
		a.lock = nil
	}
	for _, directory := range []*learnedSkillDirectory{a.journal, a.skill, a.skills, a.agents, a.workspace} {
		result = errors.Join(result, directory.close())
	}
	return result
}

func (a *learnPublicationAuthority) verify() error {
	if a == nil || a.lock == nil || !a.locked {
		return errors.New("learn publication has no lock authority")
	}
	var result error
	for _, directory := range []*learnedSkillDirectory{a.workspace, a.agents, a.skills, a.skill, a.journal} {
		result = errors.Join(result, directory.verifyOwnerBound())
	}
	ownerOnly, err := fileprivacy.IsOwnerOnly(a.lock)
	if err != nil || !ownerOnly {
		if err == nil {
			err = errors.New("learn publication lock is not owner-private")
		}
		result = errors.Join(result, err)
	}
	return result
}

func acquireLearnPublicationAuthority(ctx context.Context, store *session.Store, workspace, name string) (*learnPublicationAuthority, error) {
	if ctx == nil {
		return nil, errors.New("learn publication has no context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if store == nil {
		return nil, errors.New("learn publication has no session store")
	}
	workspaceRoot, err := bindLearnedSkillWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	authority := &learnPublicationAuthority{workspace: workspaceRoot}
	fail := func(cause error) (*learnPublicationAuthority, error) {
		return nil, errors.Join(cause, authority.close())
	}
	current := workspaceRoot
	for index, component := range []string{".agents", "skills", name} {
		child, _, err := openLearnedSkillChild(current, component, true)
		if err != nil {
			return fail(err)
		}
		switch index {
		case 0:
			authority.agents = child
		case 1:
			authority.skills = child
		case 2:
			authority.skill = child
		}
		current = child
	}
	if _, err := authority.skill.root.Lstat("SKILL.md"); err == nil {
		return fail(errors.New("the learned skill destination already exists"))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fail(fmt.Errorf("inspecting learned skill destination: %w", err))
	}

	sessionDir, err := store.WorkspaceDir(workspaceRoot.path)
	if err != nil {
		return fail(fmt.Errorf("preparing learn publication recovery: %w", err))
	}
	sessionRoot, err := bindLearnedSkillWorkspace(sessionDir)
	if err != nil {
		return fail(fmt.Errorf("binding learn publication recovery directory: %w", err))
	}
	defer sessionRoot.close()
	sessionDirectory, err := sessionRoot.root.Open(".")
	if err != nil {
		return fail(fmt.Errorf("opening learn publication recovery parent: %w", err))
	}
	private, privateErr := fileprivacy.DirectoryIsOwnerOnly(sessionDirectory)
	closeErr := sessionDirectory.Close()
	if privateErr != nil || closeErr != nil || !private {
		if privateErr == nil && !private {
			privateErr = errors.New("session workspace directory is not owner-private")
		}
		return fail(errors.Join(privateErr, closeErr))
	}
	journalRoot, err := fileprivacy.EnsurePrivateDirInRoot(sessionRoot.root, learnPublicationDirectory)
	if err != nil {
		return fail(fmt.Errorf("creating learn publication recovery directory: %w", err))
	}
	journalInfo, err := journalRoot.Stat(".")
	if err != nil {
		_ = journalRoot.Close()
		return fail(fmt.Errorf("identifying learn publication recovery directory: %w", err))
	}
	authority.journal = &learnedSkillDirectory{
		root: journalRoot,
		path: filepath.Join(sessionRoot.path, learnPublicationDirectory),
		info: journalInfo,
	}
	journalDirectory, err := journalRoot.Open(".")
	if err != nil {
		return fail(fmt.Errorf("opening learn publication recovery directory: %w", err))
	}
	private, privateErr = fileprivacy.DirectoryIsOwnerOnly(journalDirectory)
	closeErr = journalDirectory.Close()
	if privateErr != nil || closeErr != nil || !private {
		if privateErr == nil && !private {
			privateErr = errors.New("directory is not owner-private")
		}
		return fail(errors.Join(privateErr, closeErr))
	}
	lock, _, err := fileprivacy.OpenReadWriteOrCreateInRoot(journalRoot, learnPublicationLockFile)
	if err != nil {
		return fail(fmt.Errorf("opening learn publication lock: %w", err))
	}
	authority.lock = lock
	acquired, err := tryLearnPublicationLock(lock)
	if err != nil {
		return fail(fmt.Errorf("locking learn publication: %w", err))
	}
	if !acquired {
		return fail(errors.New("another /learn publication is in progress"))
	}
	authority.locked = true
	if err := authority.verify(); err != nil {
		return fail(err)
	}
	return authority, nil
}

func publishLearnedSkill(ctx context.Context, store *session.Store, workspace, name, content string) (dest string, err error) {
	if !skillNamePattern.MatchString(name) {
		return "", errors.New("invalid learned skill name")
	}
	if len(content) == 0 || len(content) > maxLearnedSkillBytes {
		return "", fmt.Errorf("learned skill must be between 1 and %d bytes", maxLearnedSkillBytes)
	}
	authority, err := acquireLearnPublicationAuthority(ctx, store, workspace, name)
	if err != nil {
		return "", err
	}
	defer func() { err = errors.Join(err, authority.close()) }()
	if err := checkpoint.RecoverFilePublicationCleanupBound(
		authority.journal.path, authority.workspace.path, authority.journal.root, authority.workspace.root,
	); err != nil {
		return "", fmt.Errorf("recovering interrupted learn publication: %w", err)
	}
	if err := authority.verify(); err != nil {
		return "", err
	}

	publicationCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var boundaryErr error
	beforePublication := func() {
		if learnPublicationBeforeCommitTestHook != nil {
			learnPublicationBeforeCommitTestHook()
		}
		boundaryErr = authority.verify()
		if boundaryErr != nil {
			cancel()
		}
	}
	dest = filepath.Join(authority.skill.path, "SKILL.md")
	published, publishErr := checkpoint.PublishStandaloneFileCASBound(
		publicationCtx,
		authority.journal.path,
		authority.workspace.path,
		authority.journal.root,
		authority.workspace.root,
		dest,
		authority.skill.root,
		"SKILL.md",
		false,
		0,
		nil,
		0o644,
		[]byte(content),
		maxLearnedSkillBytes,
		fileprivacy.Secure,
		beforePublication,
	)
	postErr := authority.verify()
	resultErr := errors.Join(boundaryErr, publishErr, postErr)
	if !published {
		if resultErr == nil {
			resultErr = errors.New("learned skill was not published")
		}
		return "", resultErr
	}
	if resultErr != nil {
		return dest, fmt.Errorf("the skill was published but final validation failed: %w", resultErr)
	}
	return dest, nil
}
