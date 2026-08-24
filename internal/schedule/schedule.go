// Package schedule is the per-workspace ledger behind /every, /at, and
// /schedule: prompts that fire as ordinary user turns at a local clock time
// or on an interval.
//
// The ledger is deliberately not a daemon. Nothing fires while sb is not
// running, and an entry whose moment passed while the process was down fires
// once at the next startup: a recurring entry does not catch up the ticks it
// missed, it fires once and reschedules from now. One process at a time owns
// the file the way the session logs beside it are owned, and the entries
// persist per workspace, under the session store's per-workspace directory,
// so a reminder never follows the user into a different checkout.
package schedule

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/switchboard-code/switchboard/internal/fileprivacy"
	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

// FileName is the ledger's name inside the per-workspace directory.
const FileName = "schedule.json"

// The ledger is user-authored prompt state, so it may be larger than a normal
// settings file, but startup must still have a complete finite read bound.
const maxLedgerBytes = 8 << 20

// lockName is the sidecar the process holds an advisory lock on for its
// life. The lock cannot ride the ledger itself: saves are atomic renames,
// and a lock on the renamed-away inode would guard nothing.
const lockName = "schedule.lock"

const (
	migrationQuarantinePrefix   = ".schedule-migration-"
	migrationQuarantineEntry    = "ledger"
	maxScheduleDirectoryEntries = 65536
	scheduleDirectoryReadBatch  = 256
)

var errScheduleInventoryTooLarge = errors.New("schedule state inventory exceeds its directory-entry limit")

// ErrLocked says another running sb process owns this workspace's ledger.
// One writer is what "fires once" rests on: two processes polling the same
// file would each fire the same entry.
var ErrLocked = errors.New("the schedule ledger is held by another sb process in this workspace")

// ErrMigrationConflict says canonical and historical workspace directories
// both contain valid but different ledgers. Choosing or merging either one
// would invent user intent, so migration leaves every file untouched.
var ErrMigrationConflict = errors.New("schedule ledger migration conflict")

// MaxEntries caps the ledger. A reminder list that grows without a bound is
// a todo file wearing the wrong command, and a bound the user can see is what
// keeps /schedule's listing a listing rather than a search problem.
const MaxEntries = 32

// MinEvery is the shortest interval a recurring entry takes. Anything tighter
// is a loop with a model in it, and the command surface is where that is
// refused rather than discovered as a bill.
const MinEvery = time.Minute

// Entry is one armed prompt. Every > 0 makes it recurring; At is a local
// wall-clock "15:04" and makes it one-shot. NextFire is the next instant the
// entry is due, recomputed at arm time and after each fire, so the ledger
// carries its own schedule and no clock math survives a restart.
type Entry struct {
	ID       string        `json:"id"`
	Every    time.Duration `json:"every,omitempty"`
	At       string        `json:"at,omitempty"`
	Prompt   string        `json:"prompt"`
	Created  time.Time     `json:"created"`
	NextFire time.Time     `json:"next_fire"`
}

// Recurring reports the kind the entry's fire behavior follows.
func (e Entry) Recurring() bool { return e.Every > 0 }

// Store is the persisted ledger. The zero value is unusable; open one with
// Open.
type Store struct {
	path  string
	root  *os.Root
	dir   os.FileInfo
	lock  *os.File
	image ledgerSnapshot

	mu      sync.Mutex
	entries []Entry
}

// Open loads the ledger in dir and takes the workspace's advisory lock on
// it, held until Close or process exit; a second opener gets ErrLocked. A
// missing file is an empty ledger, because a workspace that never armed a
// reminder has no file. An unreadable or corrupt file is an error and is
// left exactly as found: wiping it would delete reminders on the strength of
// a parse failure, which is the wrong direction for data the user wrote a
// command to create.
func Open(dir string) (*Store, error) {
	return OpenMigrating(dir, nil, nil)
}

// MigrationValidator re-proves that a historical state directory belongs to
// the canonical workspace. OpenMigrating calls it only for a directory that
// contains a ledger or interrupted cleanup artifact, after every participating
// schedule lock is held and before any file is changed.
type MigrationValidator func(historicalDir string) error

type lockedDir struct {
	dir  string
	lock *os.File
	root *os.Root
	info os.FileInfo
}

type ledgerImage struct {
	path      string
	name      string
	root      *os.Root // borrowed from lockedDir
	file      *os.File
	raw       []byte
	entries   []Entry
	canonical []byte
	info      os.FileInfo
}

func (i *ledgerImage) close() {
	if i != nil && i.file != nil {
		_ = i.file.Close()
		i.file = nil
	}
}

type migrationCleanupBoundary uint8

const (
	migrationBeforeQuarantine migrationCleanupBoundary = iota + 1
	migrationAfterQuarantine
	migrationBeforeQuarantineDelete
)

// migrationCleanupTestHook is a deterministic crash and substitution seam.
// Production never sets it. Tests do not run hook-bearing migrations in
// parallel.
var migrationCleanupTestHook func(migrationCleanupBoundary, string, string) error
var migrationCanonicalBeforeVerifyTestHook func(string, string) error
var scheduleLockBeforeOpenTestHook func(string)
var scheduleDirectoryBeforeBindTestHook func(string)

// OpenMigrating opens the canonical ledger and performs the one-time move
// from identity-proven historical workspace directories. All canonical and
// historical schedule locks are acquired in a stable order before inspection,
// validation, comparison, publication, or removal. A missing canonical ledger
// adopts the one historical value; equivalent duplicates are collapsed. Two
// non-equivalent existing ledgers refuse explicitly and remain untouched.
//
// validate is mandatory when a historical ledger exists. Discovery can run
// before this call, but identity must be re-proven through the callback while
// the schedule locks are held so a removed or retargeted symlink cannot win a
// startup race.
func OpenMigrating(dir string, historicalDirs []string, validate MigrationValidator) (*Store, error) {
	canonicalDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	canonicalDir = filepath.Clean(canonicalDir)

	dirSet := map[string]bool{canonicalDir: true}
	for _, historical := range historicalDirs {
		historical, err = filepath.Abs(historical)
		if err != nil {
			return nil, err
		}
		historical = filepath.Clean(historical)
		if historical != canonicalDir {
			dirSet[historical] = true
		}
	}
	ordered := make([]string, 0, len(dirSet))
	for candidate := range dirSet {
		ordered = append(ordered, candidate)
	}
	sort.Strings(ordered)

	held := make([]lockedDir, 0, len(ordered))
	defer func() {
		for i := range held {
			if held[i].root != nil {
				_ = held[i].root.Close()
				held[i].root = nil
			}
			if held[i].lock == nil {
				continue
			}
			_ = releaseLock(held[i].lock)
			_ = held[i].lock.Close()
		}
	}()
	for _, candidate := range ordered {
		before, statErr := os.Stat(candidate)
		if statErr != nil {
			return nil, fmt.Errorf("checking the schedule directory %s: %w", candidate, statErr)
		}
		if !before.IsDir() {
			return nil, fmt.Errorf("schedule directory %s is not a directory", candidate)
		}
		if scheduleDirectoryBeforeBindTestHook != nil {
			scheduleDirectoryBeforeBindTestHook(candidate)
		}
		root, bindErr := bindScheduleDirectory(candidate, before)
		if bindErr != nil {
			return nil, fmt.Errorf("binding the schedule directory %s: %w", candidate, bindErr)
		}
		lock, openErr := openScheduleLock(root, lockName)
		if openErr != nil {
			_ = root.Close()
			return nil, fmt.Errorf("opening the schedule lock in %s: %w", candidate, openErr)
		}
		if lockErr := acquireLock(lock); lockErr != nil {
			_ = lock.Close()
			_ = root.Close()
			return nil, lockErr
		}
		if bindErr := verifyScheduleLock(root, lock); bindErr != nil {
			_ = releaseLock(lock)
			_ = lock.Close()
			_ = root.Close()
			return nil, fmt.Errorf("binding the locked schedule directory %s: %w", candidate, bindErr)
		}
		rootInfo, infoErr := root.Stat(".")
		if infoErr != nil {
			_ = releaseLock(lock)
			_ = lock.Close()
			_ = root.Close()
			return nil, fmt.Errorf("reading the locked schedule directory %s: %w", candidate, infoErr)
		}
		held = append(held, lockedDir{dir: candidate, lock: lock, root: root, info: rootInfo})
	}
	var canonicalDirLock *lockedDir
	for i := range held {
		if held[i].dir == canonicalDir {
			canonicalDirLock = &held[i]
			break
		}
	}
	if canonicalDirLock == nil {
		return nil, fmt.Errorf("canonical schedule directory %s was not bound", canonicalDir)
	}
	// Only the canonical ledger is published through the generic state-file
	// transaction. Historical aliases have their own migration quarantine and
	// may deliberately disappear after their lock is bound; requiring their
	// old spelling here would preempt the migration validator's live proof.
	if err := recoverSchedulePublications(canonicalDirLock.root, canonicalDirLock.dir, canonicalDirLock.info); err != nil {
		return nil, fmt.Errorf("recovering schedule state in %s: %w", canonicalDirLock.dir, err)
	}

	var openedImages []*ledgerImage
	defer func() {
		for _, image := range openedImages {
			image.close()
		}
	}()
	canonicalPath := filepath.Join(canonicalDir, FileName)
	canonical, err := readLedger(canonicalDirLock.root, FileName, canonicalPath)
	if err != nil {
		return nil, err
	}
	if canonical != nil {
		openedImages = append(openedImages, canonical)
	}
	baseline := canonical
	var historicalImages []*ledgerImage
	for _, item := range held {
		if item.dir == canonicalDir {
			continue
		}
		path := filepath.Join(item.dir, FileName)
		exists, statErr := ledgerExists(item.root, FileName, path)
		if statErr != nil {
			return nil, statErr
		}
		artifacts, artifactErr := migrationArtifacts(item.root, item.dir)
		if artifactErr != nil {
			return nil, artifactErr
		}
		if !exists && len(artifacts) == 0 {
			continue
		}
		if validate == nil {
			return nil, fmt.Errorf("refusing to migrate schedule state in %s without workspace identity proof", item.dir)
		}
		if validateErr := validate(item.dir); validateErr != nil {
			return nil, fmt.Errorf("revalidating schedule ledger workspace identity for %s: %w", path, validateErr)
		}
		if recoverErr := recoverMigrationArtifacts(&item, artifacts, canonical); recoverErr != nil {
			return nil, recoverErr
		}
		image, readErr := readMigrationLedger(item.root, FileName, path)
		if readErr != nil {
			return nil, readErr
		}
		if image == nil {
			// A process that ignores the advisory lock removed the file between
			// validation and read. There is no state left to migrate.
			continue
		}
		openedImages = append(openedImages, image)
		historicalImages = append(historicalImages, image)
		if baseline == nil {
			baseline = image
			continue
		}
		if !bytes.Equal(baseline.canonical, image.canonical) {
			return nil, fmt.Errorf("%w: refusing to choose between non-equivalent ledgers %s and %s", ErrMigrationConflict, baseline.path, image.path)
		}
	}

	entries := []Entry(nil)
	if baseline != nil {
		entries = append(entries, baseline.entries...)
	}
	canonicalSnapshot := ledgerSnapshot{}
	if canonical != nil {
		canonicalSnapshot = ledgerSnapshot{
			existed: true, mode: scheduleFileMode(canonical.info.Mode()), content: append([]byte(nil), canonical.raw...),
		}
	}
	if canonical == nil && baseline != nil {
		desired, encodeErr := encodeLedger(entries)
		if encodeErr != nil {
			return nil, fmt.Errorf("encoding migrated schedule ledger %s: %w", canonicalPath, encodeErr)
		}
		next, published, publishErr := publishScheduleLedger(
			canonicalDirLock.root, canonicalDir, canonicalDirLock.info, FileName,
			canonicalSnapshot, desired, nil,
		)
		if published {
			canonicalSnapshot = next
		}
		if publishErr != nil {
			return nil, fmt.Errorf("publishing migrated schedule ledger %s: %w", canonicalPath, publishErr)
		}
		canonical, err = readLedger(canonicalDirLock.root, FileName, canonicalPath)
		if err != nil {
			return nil, fmt.Errorf("binding the published canonical schedule ledger %s: %w", canonicalPath, err)
		}
		if canonical == nil {
			return nil, fmt.Errorf("%w: published canonical schedule ledger %s disappeared", ErrMigrationConflict, canonicalPath)
		}
		openedImages = append(openedImages, canonical)
	}
	verifyCanonical := func(historicalPath string, runHook bool) error {
		if runHook && migrationCanonicalBeforeVerifyTestHook != nil {
			if err := migrationCanonicalBeforeVerifyTestHook(canonicalPath, historicalPath); err != nil {
				return err
			}
		}
		if canonical == nil || !canonicalSnapshot.existed {
			return fmt.Errorf("%w: canonical schedule ledger %s is unavailable before historical cleanup", ErrMigrationConflict, canonicalPath)
		}
		if err := verifyScheduleDirectory(canonicalDirLock.root, canonicalDirLock.dir, canonicalDirLock.info); err != nil {
			return fmt.Errorf("%w: canonical schedule directory changed before historical cleanup: %v", ErrMigrationConflict, err)
		}
		if err := verifyLedgerImage(canonical, canonicalDirLock.root, FileName); err != nil {
			return fmt.Errorf("%w: canonical schedule ledger changed before historical cleanup: %v", ErrMigrationConflict, err)
		}
		if scheduleFileMode(canonical.info.Mode()) != canonicalSnapshot.mode || !bytes.Equal(canonical.raw, canonicalSnapshot.content) ||
			baseline == nil || !bytes.Equal(canonical.canonical, baseline.canonical) {
			return fmt.Errorf("%w: canonical schedule ledger no longer matches the exact image selected for migration", ErrMigrationConflict)
		}
		return nil
	}
	// Canonical publication is durable before an old spelling is removed. If
	// cleanup stops partway through, the next open sees equivalent duplicates
	// and can safely finish; user state is never left only in an unflushed name.
	for _, image := range historicalImages {
		if err := removeLedgerImage(image, func() error { return verifyCanonical(image.path, true) }); err != nil {
			return nil, fmt.Errorf("removing migrated schedule ledger %s: %w", image.path, err)
		}
	}
	if len(historicalImages) > 0 {
		if err := verifyCanonical(historicalImages[len(historicalImages)-1].path, false); err != nil {
			return nil, err
		}
	}

	s := &Store{path: canonicalPath, entries: entries, image: canonicalSnapshot}
	for i := range held {
		if held[i].dir == canonicalDir {
			s.lock = held[i].lock
			s.root = held[i].root
			s.dir = held[i].info
			held[i].lock = nil
			held[i].root = nil
			break
		}
	}
	if s.lock == nil {
		return nil, fmt.Errorf("canonical schedule lock for %s was not acquired", canonicalDir)
	}
	return s, nil
}

func bindScheduleDirectory(path string, expected os.FileInfo) (_ *os.Root, resultErr error) {
	root, err := openScheduleRoot(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			_ = root.Close()
		}
	}()
	opened, err := root.Stat(".")
	if err != nil || !opened.IsDir() || !os.SameFile(expected, opened) {
		return nil, errors.Join(err, fmt.Errorf("schedule directory changed while it was bound"))
	}
	current, err := os.Stat(path)
	if err != nil || !current.IsDir() || !os.SameFile(opened, current) {
		return nil, errors.Join(err, fmt.Errorf("schedule directory changed after it was bound"))
	}
	return root, nil
}

func verifyScheduleLock(root *os.Root, lock *os.File) error {
	lockInfo, err := lock.Stat()
	if err != nil {
		return err
	}
	linkedLock, err := root.Lstat(lockName)
	if err != nil || !linkedLock.Mode().IsRegular() || !os.SameFile(lockInfo, linkedLock) {
		return errors.Join(err, fmt.Errorf("schedule lock pathname changed while its directory was bound"))
	}
	return nil
}

func openScheduleLock(root *os.Root, name string) (*os.File, error) {
	var before os.FileInfo
	if linked, err := root.Lstat(name); err == nil {
		before = linked
		if !before.Mode().IsRegular() {
			return nil, errors.New("schedule lock is not a regular file")
		}
		if scheduleLockBeforeOpenTestHook != nil {
			scheduleLockBeforeOpenTestHook(name)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, _, err := fileprivacy.OpenReadWriteOrCreateInRoot(root, name)
	if err != nil {
		return nil, err
	}
	fail := func(cause error) (*os.File, error) {
		return nil, errors.Join(cause, file.Close())
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || (before != nil && !os.SameFile(before, opened)) {
		return fail(errors.Join(err, errors.New("schedule lock changed while it was opened")))
	}
	linked, err := root.Lstat(name)
	if err != nil || !linked.Mode().IsRegular() || !os.SameFile(opened, linked) {
		return fail(errors.Join(err, errors.New("schedule lock changed while it was opened")))
	}
	if err := fileprivacy.Secure(file); err != nil {
		return fail(fmt.Errorf("securing schedule lock: %w", err))
	}
	linked, err = root.Lstat(name)
	if err != nil || !linked.Mode().IsRegular() || !os.SameFile(opened, linked) {
		return fail(errors.Join(err, errors.New("schedule lock changed while it was secured")))
	}
	return file, nil
}

func ledgerExists(root *os.Root, name, displayPath string) (bool, error) {
	_, err := root.Lstat(name)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("checking %s: %w", displayPath, err)
}

func readLedger(root *os.Root, name, displayPath string) (*ledgerImage, error) {
	return readLedgerWith(root, name, displayPath, fileprivacy.OpenInRoot)
}

// readMigrationLedger acquires the platform's mutation-capable read handle at
// the same namespace lookup that selects the image. Windows cannot safely add
// DELETE access later with ReOpenFile because rooted files originate in
// NtCreateFile, while ReOpenFile only supports CreateFile-origin handles.
func readMigrationLedger(root *os.Root, name, displayPath string) (*ledgerImage, error) {
	return readLedgerWith(root, name, displayPath, openScheduleRead)
}

func readLedgerWith(root *os.Root, name, displayPath string, open func(*os.Root, string) (*os.File, error)) (*ledgerImage, error) {
	before, err := root.Lstat(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", displayPath, err)
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("reading %s: schedule ledger is not a regular file", displayPath)
	}
	if before.Size() > maxLedgerBytes {
		return nil, fmt.Errorf("reading %s: schedule ledger is %d bytes (limit %d)", displayPath, before.Size(), maxLedgerBytes)
	}
	file, err := open(root, name)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", displayPath, err)
	}
	closeFile := true
	defer func() {
		if closeFile {
			_ = file.Close()
		}
	}()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, errors.Join(err, fmt.Errorf("reading %s: schedule ledger changed while it was opened", displayPath))
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxLedgerBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", displayPath, err)
	}
	if len(raw) > maxLedgerBytes {
		return nil, fmt.Errorf("reading %s: schedule ledger grew beyond %d bytes", displayPath, maxLedgerBytes)
	}
	finished, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", displayPath, err)
	}
	linked, linkErr := root.Lstat(name)
	if linkErr != nil || !linked.Mode().IsRegular() ||
		!os.SameFile(opened, finished) || !os.SameFile(finished, linked) ||
		opened.Size() != finished.Size() || finished.Size() != int64(len(raw)) ||
		!opened.ModTime().Equal(finished.ModTime()) {
		return nil, errors.Join(linkErr, fmt.Errorf("reading %s: schedule ledger changed while it was read", displayPath))
	}
	var entries []Entry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("reading %s: %w", displayPath, err)
	}
	if entries == nil {
		entries = []Entry{}
	}
	canonical, err := json.Marshal(entries)
	if err != nil {
		return nil, fmt.Errorf("normalizing %s: %w", displayPath, err)
	}
	closeFile = false
	return &ledgerImage{
		path:      displayPath,
		name:      name,
		root:      root,
		file:      file,
		raw:       append([]byte(nil), raw...),
		entries:   entries,
		canonical: canonical,
		info:      finished,
	}, nil
}

func removeLedgerImage(image *ledgerImage, verifyCanonical func() error) error {
	if image == nil || image.root == nil || image.file == nil || image.info == nil {
		return fmt.Errorf("%w: schedule cleanup has no bound read identity", ErrMigrationConflict)
	}
	if err := verifyLedgerImage(image, image.root, image.name); err != nil {
		return fmt.Errorf("%w: %v", ErrMigrationConflict, err)
	}
	quarantine, quarantineRoot, quarantineInfo, err := createMigrationQuarantine(image.root)
	if err != nil {
		return err
	}
	quarantineOpen := true
	defer func() {
		if quarantineOpen {
			_ = quarantineRoot.Close()
		}
	}()
	occupied := false
	defer func() {
		if !occupied {
			_ = removeMigrationQuarantineDirectory(image.root, quarantine, quarantineRoot, quarantineInfo, &quarantineOpen)
		}
	}()
	quarantinePath := filepath.Join(filepath.Dir(image.path), quarantine, migrationQuarantineEntry)
	if err := runMigrationCleanupHook(migrationBeforeQuarantine, image.path, quarantinePath); err != nil {
		return err
	}
	if verifyCanonical != nil {
		if err := verifyCanonical(); err != nil {
			return err
		}
	}
	// This is the mutation seam: it atomically selects whatever the bound source
	// name denotes now and moves that entry into a private sibling directory.
	// Nothing is unlinked until the moved entry is proved to be the descriptor
	// and bytes read above.
	published, moveErr := moveScheduleNoReplace(image.root, quarantineRoot, image.name, migrationQuarantineEntry, image.file)
	if !published {
		if entries, readErr := readRootDir(quarantineRoot); readErr == nil && len(entries) != 0 {
			occupied = true
		}
		return fmt.Errorf("%w: quarantining schedule ledger without replacement: %v", ErrMigrationConflict, moveErr)
	}
	occupied = true
	if moveErr != nil {
		return fmt.Errorf("schedule ledger was quarantined with a reported namespace error; recovery evidence was retained: %w", moveErr)
	}
	if err := syncScheduleRoot(quarantineRoot); err != nil {
		return fmt.Errorf("syncing schedule quarantine contents: %w", err)
	}
	if err := syncScheduleRoot(image.root); err != nil {
		return fmt.Errorf("syncing schedule quarantine: %w", err)
	}
	if err := runMigrationCleanupHook(migrationAfterQuarantine, image.path, quarantinePath); err != nil {
		return err
	}
	if verifyCanonical != nil {
		if err := verifyCanonical(); err != nil {
			image.close()
			restoreErr := restoreMigrationQuarantine(image.root, quarantine, quarantineRoot, quarantineInfo, image.name, &quarantineOpen)
			if restoreErr == nil {
				occupied = true
			}
			return errors.Join(err, restoreErr)
		}
	}
	if err := verifyLedgerImage(image, quarantineRoot, migrationQuarantineEntry); err != nil {
		image.close()
		restoreErr := restoreMigrationQuarantine(image.root, quarantine, quarantineRoot, quarantineInfo, image.name, &quarantineOpen)
		if restoreErr == nil {
			occupied = true
		}
		return errors.Join(
			fmt.Errorf("%w: schedule ledger changed at the cleanup seam: %v", ErrMigrationConflict, err),
			restoreErr,
		)
	}
	if linked, err := image.root.Lstat(image.name); err == nil {
		// Exact-handle namespace moves (Windows) can quarantine the image even
		// when an ignoring writer replaced its old name at the last seam. The
		// canonical copy makes the verified quarantine redundant, but the new
		// source is user state and must remain for the next ordinary conflict.
		if deleteErr := deleteVerifiedQuarantine(image, quarantineRoot, quarantinePath, verifyCanonical); deleteErr != nil {
			if errors.Is(deleteErr, ErrMigrationConflict) {
				image.close()
				restoreErr := restoreMigrationQuarantine(image.root, quarantine, quarantineRoot, quarantineInfo, image.name, &quarantineOpen)
				if restoreErr == nil {
					occupied = true
				}
				return errors.Join(deleteErr, restoreErr)
			}
			return deleteErr
		}
		if err := removeMigrationQuarantineDirectory(image.root, quarantine, quarantineRoot, quarantineInfo, &quarantineOpen); err != nil {
			return err
		}
		occupied = true
		return fmt.Errorf("%w: schedule source was replaced during cleanup and was preserved (%s)", ErrMigrationConflict, linked.Name())
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking schedule source after quarantine: %w", err)
	}
	if deleteErr := deleteVerifiedQuarantine(image, quarantineRoot, quarantinePath, verifyCanonical); deleteErr != nil {
		if errors.Is(deleteErr, ErrMigrationConflict) {
			image.close()
			restoreErr := restoreMigrationQuarantine(image.root, quarantine, quarantineRoot, quarantineInfo, image.name, &quarantineOpen)
			if restoreErr == nil {
				occupied = true
			}
			return errors.Join(deleteErr, restoreErr)
		}
		return deleteErr
	}
	if err := removeMigrationQuarantineDirectory(image.root, quarantine, quarantineRoot, quarantineInfo, &quarantineOpen); err != nil {
		return err
	}
	occupied = true
	return syncScheduleRoot(image.root)
}

func verifyLedgerImage(image *ledgerImage, root *os.Root, name string) error {
	if image == nil || image.file == nil || image.info == nil {
		return errors.New("schedule ledger has no opened identity")
	}
	before, err := image.file.Stat()
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || !os.SameFile(image.info, before) || before.Size() > maxLedgerBytes {
		return errors.New("opened schedule ledger changed identity or size")
	}
	raw, err := io.ReadAll(io.LimitReader(io.NewSectionReader(image.file, 0, maxLedgerBytes+1), maxLedgerBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > maxLedgerBytes {
		return errors.New("opened schedule ledger grew beyond its read bound")
	}
	after, err := image.file.Stat()
	if err != nil {
		return err
	}
	linked, linkErr := root.Lstat(name)
	if linkErr != nil || !linked.Mode().IsRegular() ||
		!os.SameFile(before, after) || !os.SameFile(after, linked) ||
		before.Size() != after.Size() || after.Size() != int64(len(raw)) ||
		!before.ModTime().Equal(after.ModTime()) ||
		!bytes.Equal(raw, image.raw) {
		return errors.Join(linkErr, errors.New("schedule ledger no longer matches the exact image that was read"))
	}
	return nil
}

func runMigrationCleanupHook(boundary migrationCleanupBoundary, source, quarantine string) error {
	if migrationCleanupTestHook == nil {
		return nil
	}
	return migrationCleanupTestHook(boundary, source, quarantine)
}

func createMigrationQuarantine(root *os.Root) (string, *os.Root, os.FileInfo, error) {
	for attempt := 0; attempt < 32; attempt++ {
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return "", nil, nil, fmt.Errorf("naming schedule migration quarantine: %w", err)
		}
		name := migrationQuarantinePrefix + fmt.Sprintf("%x", random[:])
		if err := root.Mkdir(name, 0o700); err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return "", nil, nil, fmt.Errorf("creating schedule migration quarantine: %w", err)
		}
		info, err := root.Lstat(name)
		if err != nil || !info.IsDir() {
			_ = root.Remove(name)
			return "", nil, nil, errors.Join(err, errors.New("schedule migration quarantine changed while it was created"))
		}
		quarantineRoot, err := rootedfs.OpenRootAt(root, name)
		if err != nil {
			_ = root.Remove(name)
			return "", nil, nil, fmt.Errorf("binding schedule migration quarantine: %w", err)
		}
		opened, openErr := quarantineRoot.Stat(".")
		linked, linkErr := root.Lstat(name)
		if openErr != nil || linkErr != nil || !opened.IsDir() || !linked.IsDir() ||
			!os.SameFile(info, opened) || !os.SameFile(opened, linked) {
			_ = quarantineRoot.Close()
			_ = root.Remove(name)
			return "", nil, nil, errors.Join(openErr, linkErr, errors.New("schedule migration quarantine changed while it was bound"))
		}
		return name, quarantineRoot, opened, nil
	}
	return "", nil, nil, errors.New("could not allocate a schedule migration quarantine")
}

func deleteVerifiedQuarantine(image *ledgerImage, quarantineRoot *os.Root, quarantinePath string, verifyCanonical func() error) error {
	if err := runMigrationCleanupHook(migrationBeforeQuarantineDelete, image.path, quarantinePath); err != nil {
		return err
	}
	if verifyCanonical != nil {
		if err := verifyCanonical(); err != nil {
			return err
		}
	}
	// Re-prove after the test seam. The quarantine is an unguessable private
	// directory, so this check is the last name lookup before disposal.
	if err := verifyLedgerImage(image, quarantineRoot, migrationQuarantineEntry); err != nil {
		return fmt.Errorf("%w: refusing to delete an unverified schedule quarantine: %v", ErrMigrationConflict, err)
	}
	if err := quarantineRoot.Remove(migrationQuarantineEntry); err != nil {
		return fmt.Errorf("removing verified schedule quarantine: %w", err)
	}
	if err := syncScheduleRoot(quarantineRoot); err != nil {
		return fmt.Errorf("syncing verified schedule quarantine removal: %w", err)
	}
	image.close()
	return nil
}

func restoreMigrationQuarantine(
	parent *os.Root,
	quarantine string,
	quarantineRoot *os.Root,
	quarantineInfo os.FileInfo,
	sourceName string,
	quarantineOpen *bool,
) error {
	moved, err := quarantineRoot.Lstat(migrationQuarantineEntry)
	if err != nil {
		return fmt.Errorf("schedule replacement could not be restored; quarantine was retained: %w", err)
	}
	if !moved.Mode().IsRegular() {
		return fmt.Errorf("schedule replacement was retained in %s because it is not a regular ledger", quarantine)
	}
	if _, err := parent.Lstat(sourceName); err == nil {
		return fmt.Errorf("schedule replacement was retained in %s because the source name is occupied", quarantine)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("checking schedule source before no-overwrite restore: %w", err)
	}
	movedFile, err := openScheduleRead(quarantineRoot, migrationQuarantineEntry)
	if err != nil {
		return fmt.Errorf("opening quarantined schedule replacement for no-overwrite restore: %w", err)
	}
	defer movedFile.Close()
	opened, err := movedFile.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(moved, opened) {
		return errors.Join(err, fmt.Errorf("schedule replacement was retained in %s because its identity changed before restore", quarantine))
	}
	// Reverse the quarantine through the same root-bound, no-replace namespace
	// primitive. A competing source makes this fail without overwriting either
	// entry; a crash leaves the selected file at one name or the other.
	published, moveErr := moveScheduleNoReplace(quarantineRoot, parent, migrationQuarantineEntry, sourceName, movedFile)
	if !published {
		return fmt.Errorf("schedule replacement was retained in %s; no-overwrite restore failed: %w", quarantine, moveErr)
	}
	if moveErr != nil {
		return fmt.Errorf("schedule replacement restore reported an ambiguous namespace result; recovery evidence was retained: %w", moveErr)
	}
	restored, restoreErr := parent.Lstat(sourceName)
	_, movedErr := quarantineRoot.Lstat(migrationQuarantineEntry)
	if restoreErr != nil || !errors.Is(movedErr, os.ErrNotExist) || !os.SameFile(opened, restored) {
		return errors.Join(restoreErr, movedErr,
			fmt.Errorf("schedule replacement was retained in %s because restore identity could not be proved", quarantine))
	}
	// Flush both directories touched by the rename before removing the now-empty
	// quarantine directory.
	if err := syncScheduleRoot(parent); err != nil {
		return fmt.Errorf("schedule replacement was restored but its source rename could not be flushed; empty quarantine was retained in %s: %w", quarantine, err)
	}
	if err := syncScheduleRoot(quarantineRoot); err != nil {
		return fmt.Errorf("schedule replacement was restored but its quarantine rename could not be flushed in %s: %w", quarantine, err)
	}
	if err := movedFile.Close(); err != nil {
		return fmt.Errorf("closing restored schedule replacement: %w", err)
	}
	if err := removeMigrationQuarantineDirectory(parent, quarantine, quarantineRoot, quarantineInfo, quarantineOpen); err != nil {
		return fmt.Errorf("schedule replacement was restored but quarantine cleanup failed: %w", err)
	}
	return syncScheduleRoot(parent)
}

func removeMigrationQuarantineDirectory(
	parent *os.Root,
	name string,
	quarantineRoot *os.Root,
	expected os.FileInfo,
	quarantineOpen *bool,
) error {
	entries, err := readRootDir(quarantineRoot)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("schedule migration quarantine %s is not empty", name)
	}
	linked, err := parent.Lstat(name)
	if err != nil || !linked.IsDir() || !os.SameFile(expected, linked) {
		return errors.Join(err, fmt.Errorf("schedule migration quarantine %s changed identity", name))
	}
	if quarantineOpen != nil && *quarantineOpen {
		if err := quarantineRoot.Close(); err != nil {
			return err
		}
		*quarantineOpen = false
	}
	if err := parent.Remove(name); err != nil {
		return fmt.Errorf("removing empty schedule migration quarantine %s: %w", name, err)
	}
	return nil
}

func readRootDir(root *os.Root) ([]os.DirEntry, error) {
	return readRootDirBounded(root, maxScheduleDirectoryEntries)
}

func readRootDirBounded(root *os.Root, limit int) ([]os.DirEntry, error) {
	if limit < 1 {
		return nil, errors.New("schedule directory inventory limit must be positive")
	}
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	entries := make([]os.DirEntry, 0, min(limit, scheduleDirectoryReadBatch))
	for {
		batch, readErr := dir.ReadDir(scheduleDirectoryReadBatch)
		entries = append(entries, batch...)
		if len(entries) > limit {
			return nil, fmt.Errorf("%w: directory contains more than %d entries; archive stale sessions or schedule recovery artifacts", errScheduleInventoryTooLarge, limit)
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}

func migrationArtifacts(root *os.Root, displayDir string) ([]string, error) {
	entries, err := readRootDir(root)
	if err != nil {
		return nil, fmt.Errorf("scanning schedule migration artifacts in %s: %w", displayDir, err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), migrationQuarantinePrefix) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func recoverMigrationArtifacts(item *lockedDir, artifacts []string, canonical *ledgerImage) error {
	for _, name := range artifacts {
		if err := recoverMigrationArtifact(item, name, canonical); err != nil {
			return err
		}
	}
	return nil
}

func recoverMigrationArtifact(item *lockedDir, name string, canonical *ledgerImage) (resultErr error) {
	linked, err := item.root.Lstat(name)
	if err != nil || !linked.IsDir() {
		return errors.Join(err, fmt.Errorf("%w: schedule migration artifact %s is not a bound directory", ErrMigrationConflict, filepath.Join(item.dir, name)))
	}
	quarantineRoot, err := rootedfs.OpenRootAt(item.root, name)
	if err != nil {
		return fmt.Errorf("opening schedule migration artifact %s: %w", filepath.Join(item.dir, name), err)
	}
	quarantineOpen := true
	defer func() {
		if quarantineOpen {
			resultErr = errors.Join(resultErr, quarantineRoot.Close())
		}
	}()
	opened, openErr := quarantineRoot.Stat(".")
	relinked, relinkErr := item.root.Lstat(name)
	if openErr != nil || relinkErr != nil || !opened.IsDir() || !relinked.IsDir() ||
		!os.SameFile(linked, opened) || !os.SameFile(opened, relinked) {
		return errors.Join(openErr, relinkErr,
			fmt.Errorf("%w: schedule migration artifact %s changed while it was bound", ErrMigrationConflict, filepath.Join(item.dir, name)))
	}
	entries, err := readRootDir(quarantineRoot)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		if err := removeMigrationQuarantineDirectory(item.root, name, quarantineRoot, opened, &quarantineOpen); err != nil {
			return err
		}
		return syncScheduleRoot(item.root)
	}
	if len(entries) != 1 || entries[0].Name() != migrationQuarantineEntry {
		return fmt.Errorf("%w: schedule migration artifact %s contains unexpected entries and was retained", ErrMigrationConflict, filepath.Join(item.dir, name))
	}
	displayPath := filepath.Join(item.dir, name, migrationQuarantineEntry)
	image, err := readMigrationLedger(quarantineRoot, migrationQuarantineEntry, displayPath)
	if err != nil {
		restoreErr := restoreMigrationQuarantine(item.root, name, quarantineRoot, opened, FileName, &quarantineOpen)
		if restoreErr == nil {
			return fmt.Errorf("%w: restored an unreadable interrupted schedule image from %s; retry to inspect it: %v", ErrMigrationConflict, displayPath, err)
		}
		return errors.Join(
			fmt.Errorf("%w: reading retained schedule migration artifact: %v", ErrMigrationConflict, err),
			restoreErr,
		)
	}
	if image == nil {
		return fmt.Errorf("%w: retained schedule migration artifact disappeared", ErrMigrationConflict)
	}
	defer image.close()
	if canonical != nil {
		verifyCanonical := func() error {
			if err := verifyLedgerImage(canonical, canonical.root, canonical.name); err != nil {
				return fmt.Errorf("%w: canonical schedule ledger changed during migration recovery: %v", ErrMigrationConflict, err)
			}
			return nil
		}
		if err := verifyCanonical(); err != nil {
			return fmt.Errorf("%w: canonical schedule ledger changed during migration recovery: %v", ErrMigrationConflict, err)
		}
		if bytes.Equal(canonical.canonical, image.canonical) {
			if deleteErr := deleteVerifiedQuarantine(image, quarantineRoot, displayPath, verifyCanonical); deleteErr != nil {
				if errors.Is(deleteErr, ErrMigrationConflict) {
					image.close()
					restoreErr := restoreMigrationQuarantine(item.root, name, quarantineRoot, opened, FileName, &quarantineOpen)
					return errors.Join(deleteErr, restoreErr)
				}
				return deleteErr
			}
			if err := removeMigrationQuarantineDirectory(item.root, name, quarantineRoot, opened, &quarantineOpen); err != nil {
				return err
			}
			return syncScheduleRoot(item.root)
		}
	}
	if moved, movedErr := quarantineRoot.Lstat(migrationQuarantineEntry); movedErr == nil {
		if source, sourceErr := item.root.Lstat(FileName); sourceErr == nil && os.SameFile(moved, source) {
			// When both names are proved to be links to the exact same inode,
			// preserve the source, collapse only the duplicate quarantine link,
			// then surface the semantic conflict with canonical state.
			if deleteErr := deleteVerifiedQuarantine(image, quarantineRoot, displayPath, nil); deleteErr != nil {
				return deleteErr
			}
			if err := removeMigrationQuarantineDirectory(item.root, name, quarantineRoot, opened, &quarantineOpen); err != nil {
				return err
			}
			if err := syncScheduleRoot(item.root); err != nil {
				return err
			}
			return fmt.Errorf("%w: completed an interrupted no-overwrite schedule restore in %s", ErrMigrationConflict, item.dir)
		}
	}
	// A non-equivalent or ownerless artifact may be the entry selected at a
	// crash seam. Restore it only if doing so cannot overwrite a newer source,
	// then stop: the ordinary conflict path on the next open will present both
	// ledgers rather than guessing which prompt state should win.
	image.close()
	if err := restoreMigrationQuarantine(item.root, name, quarantineRoot, opened, FileName, &quarantineOpen); err != nil {
		return errors.Join(
			fmt.Errorf("%w: schedule migration recovery retained %s", ErrMigrationConflict, displayPath),
			err,
		)
	}
	return fmt.Errorf("%w: restored interrupted schedule migration state from %s; retry to compare it", ErrMigrationConflict, displayPath)
}

// Close releases the ledger's lock. The kernel would do it at process exit;
// this is the tidy path.
func (s *Store) Close() {
	if s.lock != nil {
		_ = releaseLock(s.lock)
		_ = s.lock.Close()
		s.lock = nil
	}
	if s.root != nil {
		_ = s.root.Close()
		s.root = nil
	}
}

// save writes the ledger atomically: a crash mid-write must leave the old
// file whole, because the alternative is losing every armed reminder to a
// power cut at the wrong second.
func (s *Store) save() (bool, error) {
	if s.root == nil || s.dir == nil {
		return false, errors.New("schedule directory capability is unavailable")
	}
	data, err := encodeLedger(s.entries)
	if err != nil {
		return false, err
	}
	next, published, err := publishScheduleLedger(
		s.root, filepath.Dir(s.path), s.dir, filepath.Base(s.path), s.image, data, nil,
	)
	if published {
		s.image = next
	}
	return published, err
}

func writeLedgerFile(path string, entries []Entry, durable bool) error {
	directory := filepath.Dir(path)
	before, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.IsDir() {
		return errors.New("schedule directory is not a physical directory")
	}
	root, err := rootedfs.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	return writeLedgerFileBound(root, directory, before, filepath.Base(path), entries, durable)
}

func writeLedgerFileBound(root *os.Root, directory string, expected os.FileInfo, name string, entries []Entry, durable bool) error {
	if err := recoverSchedulePublications(root, directory, expected); err != nil {
		return err
	}
	image, err := readLedger(root, name, filepath.Join(directory, name))
	if err != nil {
		return err
	}
	snapshot := ledgerSnapshot{}
	if image != nil {
		snapshot = ledgerSnapshot{
			existed: true, mode: scheduleFileMode(image.info.Mode()), content: append([]byte(nil), image.raw...),
		}
		image.close()
	}
	data, err := encodeLedger(entries)
	if err != nil {
		return err
	}
	_, _, err = publishScheduleLedger(root, directory, expected, name, snapshot, data, nil)
	return err
}

func encodeLedger(entries []Entry) ([]byte, error) {
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return nil, err
	}
	if len(data) > maxLedgerBytes {
		return nil, fmt.Errorf("schedule ledger exceeds %d bytes", maxLedgerBytes)
	}
	return data, nil
}

func verifyScheduleDirectory(root *os.Root, path string, expected os.FileInfo) error {
	opened, openErr := root.Stat(".")
	linked, linkErr := os.Lstat(path)
	if openErr != nil || linkErr != nil || expected == nil || !opened.IsDir() ||
		linked.Mode()&os.ModeSymlink != 0 || !linked.IsDir() ||
		!os.SameFile(expected, opened) || !os.SameFile(opened, linked) {
		return errors.Join(openErr, linkErr, errors.New("schedule directory changed while it was used"))
	}
	return nil
}

// Add arms an entry: it validates the shape, assigns the lowest free short
// id, computes the first fire from now, and persists. The validation lives
// here and not only in the command surface because the file can also be
// edited by hand, and a malformed entry that fired is worse than a refused
// one.
func (s *Store) Add(e Entry) (Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) >= MaxEntries {
		return Entry{}, fmt.Errorf("the schedule holds at most %d entries; /schedule cancel one first", MaxEntries)
	}
	if e.Prompt == "" {
		return Entry{}, fmt.Errorf("a scheduled entry needs a prompt")
	}
	now := time.Now()
	e.Created = now.UTC()
	switch {
	case e.Every > 0 && e.At != "":
		return Entry{}, fmt.Errorf("an entry is recurring or one-shot, not both")
	case e.Every > 0:
		if e.Every < MinEvery {
			return Entry{}, fmt.Errorf("the shortest interval is %s", MinEvery)
		}
		e.NextFire = now.Add(e.Every)
	case e.At != "":
		next, err := nextAt(now, e.At)
		if err != nil {
			return Entry{}, err
		}
		// Normalize to the canonical clock form, so "7:05" and "07:05" are the
		// same entry rather than two spellings the listing renders differently.
		e.At = next.Format("15:04")
		e.NextFire = next
	default:
		return Entry{}, fmt.Errorf("an entry needs an interval or a clock time")
	}
	e.ID = s.nextID()
	s.entries = append(s.entries, e)
	published, err := s.save()
	if err != nil {
		if !published {
			s.entries = s.entries[:len(s.entries)-1]
			return Entry{}, err
		}
		return e, err
	}
	return e, nil
}

// nextID assigns the lowest free short id. Cancelled ids are reused, so the
// listing's numbers stay small enough to type into /schedule cancel.
func (s *Store) nextID() string {
	used := make(map[string]bool, len(s.entries))
	for _, e := range s.entries {
		used[e.ID] = true
	}
	for n := 1; ; n++ {
		id := "s" + strconv.Itoa(n)
		if !used[id] {
			return id
		}
	}
}

// nextAt resolves a wall clock to an instant: today at that time when it is
// still in the future in local time, tomorrow otherwise. The wall clock is
// the promise — "14:30" means 14:30 local — so the date arithmetic is
// calendar arithmetic and a daylight-saving jump moves the instant, not the
// clock reading.
func nextAt(now time.Time, clock string) (time.Time, error) {
	parsed, err := time.Parse("15:04", clock)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a 24-hour clock time like 14:30", clock)
	}
	local := now.Local()
	next := time.Date(local.Year(), local.Month(), local.Day(), parsed.Hour(), parsed.Minute(), 0, 0, time.Local)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next, nil
}

// List returns the ledger, soonest fire first, as a copy the caller may hold.
func (s *Store) List() []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]Entry(nil), s.entries...)
	sort.Slice(out, func(i, j int) bool { return out[i].NextFire.Before(out[j].NextFire) })
	return out
}

// Cancel removes an entry and reports whether it existed. The id names itself
// in the refusal rather than being corrected, because the user typed it.
func (s *Store) Cancel(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, e := range s.entries {
		if e.ID == id {
			s.entries = append(s.entries[:i], s.entries[i+1:]...)
			// A save failure here leaves the cancellation in memory only: the
			// entry returns on the next run. That is the safe direction — a
			// reminder that fires once more is recoverable, a silently lost
			// save error is not visible at all.
			_, _ = s.save()
			return true
		}
	}
	return false
}

// TakeDue returns entries due at now — at most max of them, soonest first,
// unless max is not positive — and, in the same locked step, advances the
// ledger past them: a recurring entry's next fire becomes now+Every and a
// one-shot is removed. Advancing before returning is what makes "fires once"
// a property rather than a hope — a crash after the caller fires but before
// the next save can repeat an entry, so the save happens here, before the
// caller has fired anything. Missed intervals are never made up: an entry
// five ticks overdue fires once, not five times. A save failure is returned
// with the entries: they are safe to fire, but the ledger on disk still
// holds them, and the caller is the one who can say so.
func (s *Store) TakeDue(now time.Time, max int) ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Soonest first, so a limited drain takes the most overdue entry rather
	// than the one that happens to be armed earliest.
	sort.SliceStable(s.entries, func(i, j int) bool { return s.entries[i].NextFire.Before(s.entries[j].NextFire) })
	var due []Entry
	kept := s.entries[:0]
	changed := false
	for _, e := range s.entries {
		if max > 0 && len(due) >= max {
			kept = append(kept, e)
			continue
		}
		if !e.NextFire.After(now) {
			due = append(due, e)
			changed = true
			if e.Recurring() {
				e.NextFire = now.Add(e.Every)
				kept = append(kept, e)
			}
			continue
		}
		kept = append(kept, e)
	}
	s.entries = kept
	if !changed {
		return nil, nil
	}
	if _, err := s.save(); err != nil {
		return due, err
	}
	return due, nil
}
