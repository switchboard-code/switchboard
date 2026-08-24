package schedule

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
)

// Schedule publication has its own cleanup namespace. The per-workspace
// directory also contains session/checkpoint state; sharing their cleanup
// inventory would let one component reconcile another component's pending
// transaction without holding its lock.
const schedulePublicationJournalName = ".schedule-publication"

// Deterministic publication seams used only by package tests.
var (
	schedulePublicationBeforeTestHook func()
	schedulePublicationAfterTestHook  func(bool, error) error
)

type ledgerSnapshot struct {
	existed bool
	mode    fs.FileMode
	content []byte
}

type schedulePublicationJournal struct {
	root       *os.Root
	directory  *os.File
	info       fs.FileInfo
	parent     *os.Root
	parentInfo fs.FileInfo
	parentPath string
	path       string
}

func openSchedulePublicationJournal(parent *os.Root, parentPath string, parentInfo fs.FileInfo, create bool) (*schedulePublicationJournal, error) {
	if err := verifyScheduleDirectory(parent, parentPath, parentInfo); err != nil {
		return nil, err
	}
	linked, err := parent.Lstat(schedulePublicationJournalName)
	created := false
	if errors.Is(err, fs.ErrNotExist) {
		if !create {
			return nil, nil
		}
		if err := parent.Mkdir(schedulePublicationJournalName, 0o700); err != nil {
			return nil, fmt.Errorf("creating schedule publication journal: %w", err)
		}
		created = true
		linked, err = parent.Lstat(schedulePublicationJournalName)
	}
	if err != nil {
		return nil, fmt.Errorf("inspecting schedule publication journal: %w", err)
	}
	if !linked.IsDir() || linked.Mode()&fs.ModeSymlink != 0 {
		return nil, errors.New("schedule publication journal is not a physical directory")
	}
	root, err := rootedfs.OpenRootAt(parent, schedulePublicationJournalName)
	if err != nil {
		return nil, fmt.Errorf("binding schedule publication journal: %w", err)
	}
	failRoot := func(cause error) (*schedulePublicationJournal, error) {
		return nil, errors.Join(cause, root.Close())
	}
	directory, err := root.Open(".")
	if err != nil {
		return failRoot(fmt.Errorf("opening schedule publication journal: %w", err))
	}
	fail := func(cause error) (*schedulePublicationJournal, error) {
		return nil, errors.Join(cause, directory.Close(), root.Close())
	}
	opened, err := directory.Stat()
	relinked, relinkErr := parent.Lstat(schedulePublicationJournalName)
	if err != nil || relinkErr != nil || !opened.IsDir() || !relinked.IsDir() ||
		!os.SameFile(linked, opened) || !os.SameFile(opened, relinked) {
		return fail(errors.Join(err, relinkErr, errors.New("schedule publication journal changed while it was bound")))
	}
	if created {
		if err := fileprivacy.SecureDirectory(directory); err != nil {
			return fail(fmt.Errorf("securing schedule publication journal: %w", err))
		}
	} else {
		ownerOnly, ownerErr := fileprivacy.DirectoryIsOwnerOnly(directory)
		if ownerErr != nil || !ownerOnly {
			if ownerErr == nil {
				ownerErr = errors.New("directory is not owner-only")
			}
			return fail(fmt.Errorf("checking schedule publication journal privacy: %w", ownerErr))
		}
	}
	journal := &schedulePublicationJournal{
		root: root, directory: directory, info: opened,
		parent: parent, parentInfo: parentInfo, parentPath: parentPath,
		path: filepath.Join(parentPath, schedulePublicationJournalName),
	}
	if err := journal.validate(); err != nil {
		return nil, errors.Join(err, journal.close())
	}
	return journal, nil
}

func (j *schedulePublicationJournal) validate() error {
	if j == nil || j.root == nil || j.directory == nil || j.info == nil {
		return errors.New("schedule publication journal capability is closed")
	}
	if err := verifyScheduleDirectory(j.parent, j.parentPath, j.parentInfo); err != nil {
		return err
	}
	opened, err := j.directory.Stat()
	rootInfo, rootErr := j.root.Stat(".")
	linked, linkErr := j.parent.Lstat(schedulePublicationJournalName)
	absLinked, absErr := os.Lstat(j.path)
	if err != nil || rootErr != nil || linkErr != nil || absErr != nil ||
		!opened.IsDir() || !rootInfo.IsDir() || !linked.IsDir() || !absLinked.IsDir() ||
		linked.Mode()&fs.ModeSymlink != 0 || absLinked.Mode()&fs.ModeSymlink != 0 ||
		!os.SameFile(j.info, opened) || !os.SameFile(opened, rootInfo) ||
		!os.SameFile(rootInfo, linked) || !os.SameFile(linked, absLinked) {
		return errors.Join(err, rootErr, linkErr, absErr,
			errors.New("schedule publication journal no longer names its retained directory"))
	}
	ownerOnly, ownerErr := fileprivacy.DirectoryIsOwnerOnly(j.directory)
	if ownerErr != nil || !ownerOnly {
		if ownerErr == nil {
			ownerErr = errors.New("directory is not owner-only")
		}
		return fmt.Errorf("checking schedule publication journal privacy: %w", ownerErr)
	}
	return nil
}

func (j *schedulePublicationJournal) close() error {
	if j == nil {
		return nil
	}
	var result error
	if j.directory != nil {
		result = errors.Join(result, j.directory.Close())
		j.directory = nil
	}
	if j.root != nil {
		result = errors.Join(result, j.root.Close())
		j.root = nil
	}
	return result
}

func recoverSchedulePublications(parent *os.Root, parentPath string, parentInfo fs.FileInfo) error {
	journal, err := openSchedulePublicationJournal(parent, parentPath, parentInfo, false)
	if err != nil || journal == nil {
		return err
	}
	defer journal.close()
	if err := checkpoint.RecoverFilePublicationCleanupBound(journal.path, parentPath, journal.root, parent); err != nil {
		return fmt.Errorf("recovering schedule publication: %w", err)
	}
	return journal.validate()
}

func publishScheduleLedger(
	parent *os.Root,
	parentPath string,
	parentInfo fs.FileInfo,
	name string,
	expected ledgerSnapshot,
	desired []byte,
	beforePublication func(),
) (ledgerSnapshot, bool, error) {
	if len(desired) > maxLedgerBytes {
		return expected, false, fmt.Errorf("schedule ledger exceeds %d bytes", maxLedgerBytes)
	}
	if err := verifyScheduleDirectory(parent, parentPath, parentInfo); err != nil {
		return expected, false, err
	}
	journal, err := openSchedulePublicationJournal(parent, parentPath, parentInfo, true)
	if err != nil {
		return expected, false, err
	}
	defer journal.close()
	if err := checkpoint.RecoverFilePublicationCleanupBound(journal.path, parentPath, journal.root, parent); err != nil {
		return expected, false, fmt.Errorf("recovering schedule publication: %w", err)
	}
	if err := journal.validate(); err != nil {
		return expected, false, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var boundaryErr error
	boundary := func() {
		boundaryErr = errors.Join(
			verifyScheduleDirectory(parent, parentPath, parentInfo),
			journal.validate(),
		)
		if boundaryErr != nil {
			cancel()
		}
		if beforePublication != nil {
			beforePublication()
		}
		if schedulePublicationBeforeTestHook != nil {
			schedulePublicationBeforeTestHook()
		}
	}
	path := filepath.Join(parentPath, name)
	published, publishErr := checkpoint.PublishStandaloneFileCASBound(
		ctx, journal.path, parentPath, journal.root, parent, path, parent, name,
		expected.existed, expected.mode, expected.content,
		0o600, desired, maxLedgerBytes, fileprivacy.Secure, boundary,
	)
	postErr := errors.Join(
		verifyScheduleDirectory(parent, parentPath, parentInfo),
		journal.validate(),
	)
	resultErr := errors.Join(boundaryErr, publishErr, postErr)
	if schedulePublicationAfterTestHook != nil {
		resultErr = errors.Join(resultErr, schedulePublicationAfterTestHook(published, resultErr))
	}
	if !published {
		return expected, false, resultErr
	}
	return ledgerSnapshot{existed: true, mode: scheduleFileMode(0o600), content: append([]byte(nil), desired...)}, true, resultErr
}
