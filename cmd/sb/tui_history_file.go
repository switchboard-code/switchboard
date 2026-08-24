package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/fileprivacy"
	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

// Prompt history is convenience data, but it still lives in the user's home
// directory. Bound every read and rewrite so a corrupt file cannot turn opening
// the workbench into an unbounded allocation.
const historyMaxBytes int64 = 8 << 20

var errHistoryBusy = errors.New("prompt history is being changed by another process")

var errHistoryRecoveryRequired = errors.New("prompt history transaction requires recovery")

const (
	historyTransactionVersion = 1
	historyTransactionLimit   = 32 << 10
)

// These seams are deliberately package-private and nil in production. Tests
// use them to put an uncooperative writer in the few instructions between the
// final comparison and the namespace primitive, and to model a process death
// after a durable namespace transition.
var (
	historyNamespaceMutationTestHook        func(operation, path string)
	historyTransactionBoundaryHook          func(operation string, boundary historyTransactionBoundary) error
	historyTransactionRecordWriteTestHook   func(file *os.File) error
	historyTransactionRecordPublishTestHook func(file *os.File) error
	historyStageWriteTestHook               func(operation string, file *os.File) error
	historyParentBeforeRootOpenTestHook     func(string)
	historyRollbackTestHook                 func() error
)

type historyTransactionBoundary uint8

const (
	historyTransactionRecorded historyTransactionBoundary = iota + 1
	historyTransactionNamespaceChanged
	historyTransactionRetired
)

type historyTransactionImage struct {
	Identity string `json:"identity"`
	Size     int64  `json:"size"`
	Digest   string `json:"sha256"`
}

type historyTransaction struct {
	Version       int                     `json:"version"`
	Operation     string                  `json:"operation"`
	Target        string                  `json:"target"`
	Stage         string                  `json:"stage"`
	RetiredStage  string                  `json:"retired_stage"`
	InternalStage string                  `json:"internal_stage,omitempty"`
	RollbackStage string                  `json:"rollback_stage,omitempty"`
	Expected      historyTransactionImage `json:"expected"`
	Replacement   historyTransactionImage `json:"replacement,omitempty"`
}

type historyBoundParent struct {
	root *os.Root
	info os.FileInfo
	path string
	name string
}

type historySnapshot struct {
	info      os.FileInfo
	parent    os.FileInfo
	size      int64
	digest    [sha256.Size]byte
	ownerOnly bool
}

// readHistoryFile keeps the sidecar lock for the whole descriptor read. The
// hook is an explicit test seam for a pathname replacement after open.
func readHistoryFile(path string, afterRead func()) ([]byte, historySnapshot, error) {
	var data []byte
	var snapshot historySnapshot
	err := withHistoryLock(path, false, func(parent os.FileInfo) error {
		var err error
		data, snapshot, err = readHistoryFileLocked(path, parent, afterRead)
		return err
	})
	return data, snapshot, err
}

func readHistoryFileLocked(path string, expectedParent os.FileInfo, afterRead func()) ([]byte, historySnapshot, error) {
	f, parent, err := openHistoryFile(path, false, false, expectedParent)
	if err != nil {
		return nil, historySnapshot{}, err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return nil, historySnapshot{}, err
	}
	if before.Size() < 0 || before.Size() > historyMaxBytes {
		return nil, historySnapshot{}, fmt.Errorf("prompt history exceeds %d bytes", historyMaxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, historyMaxBytes+1))
	if err != nil {
		return nil, historySnapshot{}, err
	}
	if int64(len(data)) > historyMaxBytes {
		return nil, historySnapshot{}, fmt.Errorf("prompt history exceeds %d bytes", historyMaxBytes)
	}
	if afterRead != nil {
		afterRead()
	}
	if err := verifyHistoryPath(f, path, parent); err != nil {
		return nil, historySnapshot{}, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, historySnapshot{}, err
	}
	ownerOnly, err := historyFileIsOwnerOnly(f)
	if err != nil {
		return nil, historySnapshot{}, err
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || int64(len(data)) != after.Size() {
		return nil, historySnapshot{}, errors.New("prompt history changed while it was read")
	}
	return data, historySnapshot{
		info: after, parent: parent, size: after.Size(), digest: sha256.Sum256(data), ownerOnly: ownerOnly,
	}, nil
}

// appendHistoryPrompt performs one bounded append while holding a stable
// sidecar lock. The hook lets tests replace the pathname between open and the
// last ownership check; production passes nil.
func appendHistoryPrompt(path, prompt string, afterOpen func()) error {
	prompt = historySafe(prompt)
	if prompt == "" {
		return nil
	}
	if int64(len(prompt)) > historyMaxBytes {
		return fmt.Errorf("prompt exceeds the history size bound")
	}
	raw, err := encodeHistoryPrompt(prompt)
	if err != nil {
		return err
	}
	return withHistoryLock(path, true, func(expectedParent os.FileInfo) error {
		f, parent, err := openHistoryFile(path, true, true, expectedParent)
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return err
		}
		if info.Size() < 0 || info.Size() > historyMaxBytes-int64(len(raw)) {
			return fmt.Errorf("prompt history exceeds %d bytes", historyMaxBytes)
		}
		if afterOpen != nil {
			afterOpen()
		}
		if err := verifyHistoryPath(f, path, parent); err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekEnd); err != nil {
			return err
		}
		n, err := f.Write(raw)
		if err == nil && n != len(raw) {
			err = io.ErrShortWrite
		}
		if err != nil {
			return err
		}
		if err := f.Sync(); err != nil {
			return err
		}
		if err := verifyHistoryPath(f, path, parent); err != nil {
			return err
		}
		return syncHistoryDirectory(filepath.Dir(path))
	})
}

func encodeHistoryPrompt(prompt string) ([]byte, error) {
	raw, err := json.Marshal(prompt)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)+1) > historyMaxBytes {
		return nil, fmt.Errorf("prompt exceeds the history size bound")
	}
	return append(raw, '\n'), nil
}

func encodeHistory(prompts []string) ([]byte, error) {
	var b bytes.Buffer
	for _, prompt := range prompts {
		prompt = historySafe(prompt)
		if int64(len(prompt)) > historyMaxBytes {
			return nil, fmt.Errorf("prompt exceeds the history size bound")
		}
		raw, err := encodeHistoryPrompt(prompt)
		if err != nil {
			return nil, err
		}
		if int64(b.Len()) > historyMaxBytes-int64(len(raw)) {
			return nil, fmt.Errorf("prompt history exceeds %d bytes", historyMaxBytes)
		}
		_, _ = b.Write(raw)
	}
	return b.Bytes(), nil
}

// rewriteHistoryIfUnchanged publishes a same-directory owner-only replacement only
// if the exact descriptor identity and bytes loaded by the caller are still at
// the path. A concurrent append makes the rewrite lose the race rather than
// erasing the new prompt. The hook is an explicit replacement-race test seam.
func rewriteHistoryIfUnchanged(path string, prompts []string, expected historySnapshot, beforeReplace func()) error {
	return withHistoryLock(path, false, func(lockedParent os.FileInfo) error {
		return rewriteHistoryLocked(path, prompts, expected, lockedParent, beforeReplace)
	})
}

func rewriteHistoryLocked(path string, prompts []string, expected historySnapshot, lockedParent os.FileInfo, beforeReplace func()) error {
	raw, err := encodeHistory(prompts)
	if err != nil {
		return err
	}
	if !os.SameFile(expected.parent, lockedParent) {
		return errors.New("prompt history parent changed before rewrite")
	}
	parent, err := openHistoryBoundParent(path, lockedParent)
	if err != nil {
		return err
	}
	defer parent.close()
	current, err := openHistorySnapshotInRoot(parent, expected, true)
	if err != nil {
		return err
	}
	defer current.Close()
	tmp, tmpName, err := createHistoryBoundTemp(parent, ".history-rewrite-")
	if err != nil {
		return err
	}
	defer tmp.Close()
	tmpRetired := false
	txnRecorded := false
	defer func() {
		// An unpublished temporary is still selected by its open descriptor.
		// Quarantine and scrub that exact inode; never unlink a checked path.
		if !tmpRetired && !txnRecorded {
			_ = retireHistoryBoundFile(parent, tmpName, tmp, nil)
		}
	}()
	tmpInfo, statErr := tmp.Stat()
	tmpOwnerOnly, ownerErr := historyFileIsOwnerOnly(tmp)
	if err = errors.Join(statErr, ownerErr); err != nil {
		return err
	}
	if tmpInfo == nil || !tmpInfo.Mode().IsRegular() || !tmpOwnerOnly {
		return errors.New("staged prompt history is not an owner-only regular file")
	}
	tmpIdentity, err := historyFileIdentity(tmp)
	if err != nil {
		return err
	}
	rawDigest := sha256.Sum256(raw)
	replacement := historyTransactionImage{
		Identity: tmpIdentity,
		Size:     int64(len(raw)),
		Digest:   hex.EncodeToString(rawDigest[:]),
	}
	currentIdentity, err := historyFileIdentity(current)
	if err != nil {
		return err
	}
	txn := historyTransaction{
		Version:       historyTransactionVersion,
		Operation:     "rewrite",
		Target:        parent.name,
		Stage:         tmpName,
		RetiredStage:  historyClearedName(tmpName),
		InternalStage: historyExchangeStagingName(tmpName),
		RollbackStage: historyExchangeStagingName(parent.name),
		Expected: historyTransactionImage{
			Identity: currentIdentity,
			Size:     expected.size,
			Digest:   hex.EncodeToString(expected.digest[:]),
		},
		Replacement: replacement,
	}
	record, recordName, err := createHistoryTransactionRecord(parent, txn)
	if record != nil {
		defer record.Close()
		txnRecorded = true
	}
	if err != nil {
		return err
	}
	if err := runHistoryTransactionBoundary("rewrite", historyTransactionRecorded); err != nil {
		return err
	}
	if err := secureHistoryFile(tmp); err != nil {
		return err
	}
	n, err := tmp.Write(raw)
	if err == nil && n != len(raw) {
		err = io.ErrShortWrite
	}
	if err == nil && historyStageWriteTestHook != nil {
		err = historyStageWriteTestHook("rewrite", tmp)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if err != nil {
		return err
	}
	staged, err := readHistoryBoundDescriptor(tmp)
	if err != nil || !bytes.Equal(staged, raw) {
		return errors.Join(errors.New("staged prompt history does not match its transaction record"), err)
	}
	if beforeReplace != nil {
		beforeReplace()
	}
	if err := matchHistorySnapshotInRoot(parent, expected); err != nil {
		return err
	}
	if err := verifyHistoryBoundName(parent, tmpName, tmp); err != nil {
		return err
	}
	if historyNamespaceMutationTestHook != nil {
		historyNamespaceMutationTestHook("rewrite", path)
	}
	result, exchangeErr := checkpoint.ExchangeOpenFiles(parent.root, tmp, current, tmpName, parent.name)
	if result.Published {
		if boundaryErr := runHistoryTransactionBoundary("rewrite", historyTransactionNamespaceChanged); boundaryErr != nil {
			return errors.Join(exchangeErr, boundaryErr)
		}
	}
	if exchangeErr != nil {
		return fmt.Errorf("atomically exchanging prompt history: %w", exchangeErr)
	}
	if err := errors.Join(
		verifyHistoryExchange(parent, tmpName, tmp, parent.name, current),
		verifyHistoryTransactionImageFile(current, txn.Expected, false),
		verifyHistoryTransactionImageFile(tmp, txn.Replacement, true),
	); err != nil {
		if historyRollbackTestHook != nil {
			if hookErr := historyRollbackTestHook(); hookErr != nil {
				return errors.Join(errHistoryRecoveryRequired, err, hookErr)
			}
		}
		rolledBack, rollbackErr := checkpoint.RollbackOpenFileExchange(parent.root, tmp, current, tmpName, parent.name)
		if !rolledBack || rollbackErr != nil {
			return errors.Join(errHistoryRecoveryRequired, err,
				fmt.Errorf("rolling back mismatched prompt history exchange: %w", rollbackErr))
		}
		if verifyErr := verifyHistoryBoundName(parent, tmpName, tmp); verifyErr != nil {
			return errors.Join(errHistoryRecoveryRequired, err,
				fmt.Errorf("verifying rolled-back prompt history stage: %w", verifyErr))
		}
		if scrubErr := retireHistoryBoundFileTo(parent, tmpName, txn.RetiredStage, tmp); scrubErr != nil {
			return errors.Join(errHistoryRecoveryRequired, err,
				fmt.Errorf("scrubbing rolled-back prompt history stage: %w", scrubErr))
		}
		tmpRetired = true
		if retireErr := retireHistoryBoundFile(parent, recordName, record, nil); retireErr != nil {
			return errors.Join(errHistoryRecoveryRequired, err,
				fmt.Errorf("retiring rolled-back prompt history transaction: %w", retireErr))
		}
		return err
	}
	if err := tmp.Sync(); err != nil {
		return errors.Join(errHistoryRecoveryRequired, fmt.Errorf("syncing published prompt history: %w", err))
	}
	if err := syncHistoryBoundDirectory(parent.root); err != nil {
		return errors.Join(errHistoryRecoveryRequired, fmt.Errorf("syncing prompt history parent: %w", err))
	}
	if err := retireHistoryBoundFileTo(parent, tmpName, txn.RetiredStage, current); err != nil {
		return errors.Join(errHistoryRecoveryRequired, fmt.Errorf("scrubbing displaced prompt history: %w", err))
	}
	tmpRetired = true // tmp is the published descriptor; never retire its target name.
	if err := runHistoryTransactionBoundary("rewrite", historyTransactionRetired); err != nil {
		return err
	}
	if err := retireHistoryBoundFile(parent, recordName, record, nil); err != nil {
		return errors.Join(errHistoryRecoveryRequired, fmt.Errorf("retiring prompt history transaction record: %w", err))
	}
	return nil
}

// createHistoryLocked publishes a previously absent canonical history under
// its already-held sidecar lock. Prompt bytes are written only after a durable
// transaction record names an owner-only private stage. The complete stage is
// then moved no-replace into the canonical name, so process death can leave an
// unpublished stage or a complete canonical image, never a partial canonical.
func createHistoryLocked(path string, prompts []string, lockedParent os.FileInfo) error {
	raw, err := encodeHistory(prompts)
	if err != nil {
		return err
	}
	parent, err := openHistoryBoundParent(path, lockedParent)
	if err != nil {
		return err
	}
	defer parent.close()
	if _, err := parent.root.Lstat(parent.name); err == nil {
		return errors.New("canonical prompt history appeared before no-replace publication")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	stage, stageName, err := createHistoryBoundTemp(parent, ".history-create-")
	if err != nil {
		return err
	}
	defer stage.Close()
	txnRecorded := false
	defer func() {
		if !txnRecorded {
			_ = retireHistoryBoundFile(parent, stageName, stage, nil)
		}
	}()
	stageIdentity, err := historyFileIdentity(stage)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	txn := historyTransaction{
		Version:      historyTransactionVersion,
		Operation:    "create",
		Target:       parent.name,
		Stage:        stageName,
		RetiredStage: historyClearedName(stageName),
		Replacement: historyTransactionImage{
			Identity: stageIdentity,
			Size:     int64(len(raw)),
			Digest:   hex.EncodeToString(digest[:]),
		},
	}
	record, recordName, err := createHistoryTransactionRecord(parent, txn)
	if record != nil {
		defer record.Close()
		txnRecorded = true
	}
	if err != nil {
		return err
	}
	if err := runHistoryTransactionBoundary("create", historyTransactionRecorded); err != nil {
		return err
	}
	if err := secureHistoryFile(stage); err != nil {
		return err
	}
	n, err := stage.Write(raw)
	if err == nil && n != len(raw) {
		err = io.ErrShortWrite
	}
	if err == nil && historyStageWriteTestHook != nil {
		err = historyStageWriteTestHook("create", stage)
	}
	if err != nil {
		return err
	}
	if err := stage.Sync(); err != nil {
		return err
	}
	staged, err := readHistoryBoundDescriptor(stage)
	if err != nil || !bytes.Equal(staged, raw) {
		return errors.Join(errors.New("staged canonical prompt history does not match its transaction record"), err)
	}
	moved, err := moveHistoryBoundFileNoReplaceAtSeam(parent, stage, stageName, parent.name, "create", path)
	if moved {
		if boundaryErr := runHistoryTransactionBoundary("create", historyTransactionNamespaceChanged); boundaryErr != nil {
			return errors.Join(err, boundaryErr)
		}
	}
	if err != nil {
		return err
	}
	if !moved {
		return errHistoryRecoveryRequired
	}
	if err := errors.Join(
		verifyHistoryBoundName(parent, parent.name, stage),
		verifyHistoryTransactionImageFile(stage, txn.Replacement, true),
	); err != nil {
		// The exact O_EXCL stage changed in place at the final seam. Remove that
		// inode from the canonical name before returning; an occupied stage name
		// or any ambiguous move leaves the transaction for recovery.
		restored, restoreErr := moveHistoryBoundFileNoReplace(parent, stage, parent.name, stageName)
		if !restored || restoreErr != nil {
			return errors.Join(errHistoryRecoveryRequired, err, restoreErr)
		}
		if retireErr := retireHistoryBoundFileTo(parent, stageName, txn.RetiredStage, stage); retireErr != nil {
			return errors.Join(errHistoryRecoveryRequired, err, retireErr)
		}
		if retireErr := retireHistoryBoundFile(parent, recordName, record, nil); retireErr != nil {
			return errors.Join(errHistoryRecoveryRequired, err, retireErr)
		}
		return err
	}
	if err := stage.Sync(); err != nil {
		return errors.Join(errHistoryRecoveryRequired, err)
	}
	if err := syncHistoryBoundDirectory(parent.root); err != nil {
		return errors.Join(errHistoryRecoveryRequired, err)
	}
	if err := runHistoryTransactionBoundary("create", historyTransactionRetired); err != nil {
		return err
	}
	if err := retireHistoryBoundFile(parent, recordName, record, nil); err != nil {
		return errors.Join(errHistoryRecoveryRequired, fmt.Errorf("retiring prompt history creation transaction: %w", err))
	}
	return nil
}

func removeHistoryIfUnchangedLocked(path string, expected historySnapshot, lockedParent os.FileInfo, beforeRemove func()) error {
	if !os.SameFile(expected.parent, lockedParent) {
		return errors.New("prompt history parent changed before migration cleanup")
	}
	parent, err := openHistoryBoundParent(path, lockedParent)
	if err != nil {
		return err
	}
	defer parent.close()
	current, err := openHistorySnapshotInRoot(parent, expected, true)
	if err != nil {
		return err
	}
	defer current.Close()
	quarantine, err := unusedHistoryBoundName(parent, ".history-retired-")
	if err != nil {
		return err
	}
	currentIdentity, err := historyFileIdentity(current)
	if err != nil {
		return err
	}
	txn := historyTransaction{
		Version:      historyTransactionVersion,
		Operation:    "remove",
		Target:       parent.name,
		Stage:        quarantine,
		RetiredStage: historyClearedName(quarantine),
		Expected: historyTransactionImage{
			Identity: currentIdentity,
			Size:     expected.size,
			Digest:   hex.EncodeToString(expected.digest[:]),
		},
	}
	record, recordName, err := createHistoryTransactionRecord(parent, txn)
	if record != nil {
		defer record.Close()
	}
	if err != nil {
		return err
	}
	if err := runHistoryTransactionBoundary("remove", historyTransactionRecorded); err != nil {
		return err
	}
	if beforeRemove != nil {
		beforeRemove()
	}
	if err := matchHistorySnapshotInRoot(parent, expected); err != nil {
		return err
	}
	moved, err := moveHistoryBoundFileNoReplaceAtSeam(parent, current, parent.name, quarantine, "remove", path)
	if moved {
		if boundaryErr := runHistoryTransactionBoundary("remove", historyTransactionNamespaceChanged); boundaryErr != nil {
			return errors.Join(err, boundaryErr)
		}
	}
	if err != nil {
		return err
	}
	if imageErr := verifyHistoryTransactionImageFile(current, txn.Expected, false); imageErr != nil {
		// The selected inode changed in place after the last pre-move hash. Put
		// that exact modified inode back under its original name; never scrub a
		// source whose bytes no longer match the migration snapshot.
		restored, restoreErr := moveHistoryBoundFileNoReplace(parent, current, quarantine, parent.name)
		if !restored || restoreErr != nil {
			return errors.Join(errHistoryRecoveryRequired, imageErr, restoreErr)
		}
		if verifyErr := verifyHistoryBoundName(parent, parent.name, current); verifyErr != nil {
			return errors.Join(errHistoryRecoveryRequired, imageErr, verifyErr)
		}
		if retireErr := retireHistoryBoundFile(parent, recordName, record, nil); retireErr != nil {
			return errors.Join(errHistoryRecoveryRequired, imageErr, retireErr)
		}
		return imageErr
	}
	if err := retireHistoryBoundFileTo(parent, quarantine, txn.RetiredStage, current); err != nil {
		return errors.Join(errHistoryRecoveryRequired, fmt.Errorf("scrubbing migrated prompt history: %w", err))
	}
	if err := runHistoryTransactionBoundary("remove", historyTransactionRetired); err != nil {
		return err
	}
	if err := retireHistoryBoundFile(parent, recordName, record, nil); err != nil {
		return errors.Join(errHistoryRecoveryRequired, fmt.Errorf("retiring prompt history transaction record: %w", err))
	}
	return nil
}

func openHistoryBoundParent(path string, expected os.FileInfo) (*historyBoundParent, error) {
	path = filepath.Clean(path)
	parentPath := filepath.Dir(path)
	before, err := os.Lstat(parentPath)
	if err != nil {
		return nil, err
	}
	if expected == nil || !before.IsDir() || before.Mode()&fs.ModeSymlink != 0 || !os.SameFile(expected, before) {
		return nil, errors.New("prompt history parent changed before it was bound")
	}
	if historyParentBeforeRootOpenTestHook != nil {
		historyParentBeforeRootOpenTestHook(parentPath)
	}
	root, err := rootedfs.OpenRoot(parentPath)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*historyBoundParent, error) {
		return nil, errors.Join(err, root.Close())
	}
	opened, err := root.Stat(".")
	if err != nil {
		return fail(err)
	}
	linked, linkErr := os.Lstat(parentPath)
	if linkErr != nil || !linked.IsDir() || linked.Mode()&fs.ModeSymlink != 0 ||
		!os.SameFile(before, opened) || !os.SameFile(opened, linked) {
		return fail(errors.Join(linkErr, errors.New("prompt history parent changed while it was bound")))
	}
	if err := checkpoint.ValidateAtomicNamespaceRoot(root); err != nil {
		return fail(fmt.Errorf("prompt history parent lacks secure atomic namespace support: %w", err))
	}
	name := filepath.Base(path)
	if name == "." || name == string(filepath.Separator) || !filepath.IsLocal(name) || filepath.Base(name) != name {
		return fail(errors.New("prompt history has an invalid leaf name"))
	}
	return &historyBoundParent{root: root, info: opened, path: parentPath, name: name}, nil
}

func (p *historyBoundParent) close() error {
	if p == nil || p.root == nil {
		return nil
	}
	err := p.root.Close()
	p.root = nil
	return err
}

func createHistoryBoundTemp(parent *historyBoundParent, prefix string) (*os.File, string, error) {
	for range 100 {
		name, err := randomHistoryBoundName(prefix)
		if err != nil {
			return nil, "", err
		}
		f, err := createHistoryBoundPrivateFile(parent.root, name)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		if err := secureHistoryFile(f); err != nil {
			return nil, "", errors.Join(err, retireHistoryBoundFile(parent, name, f, nil), f.Close())
		}
		if err := verifyHistoryBoundName(parent, name, f); err != nil {
			return nil, "", errors.Join(err, retireHistoryBoundFile(parent, name, f, nil), f.Close())
		}
		return f, name, nil
	}
	return nil, "", errors.New("could not allocate a private prompt history temporary")
}

func unusedHistoryBoundName(parent *historyBoundParent, prefix string) (string, error) {
	for range 100 {
		name, err := randomHistoryBoundName(prefix)
		if err != nil {
			return "", err
		}
		if _, err := parent.root.Lstat(name); errors.Is(err, os.ErrNotExist) {
			return name, nil
		} else if err != nil {
			return "", err
		}
	}
	return "", errors.New("could not allocate a prompt history quarantine name")
}

func randomHistoryBoundName(prefix string) (string, error) {
	var random [16]byte
	if _, err := io.ReadFull(rand.Reader, random[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(random[:]), nil
}

func openHistoryBoundFile(parent *historyBoundParent, name string, writable bool) (*os.File, error) {
	before, err := parent.root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("prompt history bound name is not a regular file")
	}
	flags := os.O_RDONLY
	if writable {
		flags = os.O_RDWR
	}
	f, err := parent.root.OpenFile(name, flags, 0)
	if err != nil {
		return nil, err
	}
	if err := verifyHistoryBoundName(parent, name, f); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, errors.Join(errors.New("prompt history changed while its bound descriptor was opened"), err, f.Close())
	}
	return f, nil
}

func verifyHistoryBoundName(parent *historyBoundParent, name string, f *os.File) error {
	if parent == nil || parent.root == nil || f == nil {
		return errors.New("prompt history bound verification requires live handles")
	}
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() {
		return errors.New("prompt history bound name is not a regular file")
	}
	links, err := historyFileLinkCount(f)
	if err != nil {
		return err
	}
	if links != 1 {
		return fmt.Errorf("prompt history bound file has %d hard links", links)
	}
	linked, err := parent.root.Lstat(name)
	if err != nil || !linked.Mode().IsRegular() || !os.SameFile(opened, linked) {
		return errors.Join(errors.New("prompt history bound name changed identity"), err)
	}
	parentInfo, err := parent.root.Stat(".")
	if err != nil || !parentInfo.IsDir() || !os.SameFile(parent.info, parentInfo) {
		return errors.Join(errors.New("prompt history bound parent changed identity"), err)
	}
	return nil
}

func openHistorySnapshotInRoot(parent *historyBoundParent, expected historySnapshot, writable bool) (*os.File, error) {
	if expected.info == nil || expected.parent == nil || !os.SameFile(expected.parent, parent.info) {
		return nil, errors.New("prompt history rewrite has no source identity")
	}
	f, err := openHistoryBoundFile(parent, parent.name, writable)
	if err != nil {
		return nil, err
	}
	data, err := readHistoryBoundDescriptor(f)
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(expected.info, opened) || expected.size != int64(len(data)) ||
		expected.digest != sha256.Sum256(data) {
		return nil, errors.Join(errors.New("prompt history changed before rewrite"), err, f.Close())
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

func matchHistorySnapshotInRoot(parent *historyBoundParent, expected historySnapshot) error {
	f, err := openHistorySnapshotInRoot(parent, expected, false)
	if err != nil {
		return err
	}
	return f.Close()
}

func readHistoryBoundDescriptor(f *os.File) ([]byte, error) {
	before, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if before.Size() < 0 || before.Size() > historyMaxBytes {
		return nil, fmt.Errorf("prompt history exceeds %d bytes", historyMaxBytes)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(f, historyMaxBytes+1))
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > historyMaxBytes || int64(len(data)) != after.Size() ||
		before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return nil, errors.New("prompt history changed while its bound descriptor was read")
	}
	return data, nil
}

func historyTransactionImageFor(f *os.File, data []byte) (historyTransactionImage, error) {
	identity, err := historyFileIdentity(f)
	if err != nil {
		return historyTransactionImage{}, err
	}
	info, err := f.Stat()
	if err != nil {
		return historyTransactionImage{}, err
	}
	digest := sha256.Sum256(data)
	return historyTransactionImage{Identity: identity, Size: info.Size(), Digest: hex.EncodeToString(digest[:])}, nil
}

func verifyHistoryExchange(parent *historyBoundParent, sourceName string, source *os.File, targetName string, target *os.File) error {
	if err := verifyHistoryBoundName(parent, targetName, source); err != nil {
		return fmt.Errorf("published prompt history is not the staged inode: %w", err)
	}
	if err := verifyHistoryBoundName(parent, sourceName, target); err != nil {
		return fmt.Errorf("displaced prompt history is not the selected inode: %w", err)
	}
	ownerOnly, err := historyFileIsOwnerOnly(source)
	if err != nil || !ownerOnly {
		return errors.Join(errors.New("published prompt history is not owner-only"), err)
	}
	return nil
}

func verifyHistoryTransactionImageFile(f *os.File, image historyTransactionImage, requireOwnerOnly bool) error {
	identity, err := historyFileIdentity(f)
	if err != nil {
		return err
	}
	data, err := readHistoryBoundDescriptor(f)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(data)
	if identity != image.Identity || int64(len(data)) != image.Size ||
		hex.EncodeToString(digest[:]) != image.Digest {
		return errors.New("prompt history image no longer matches its transaction evidence")
	}
	if requireOwnerOnly {
		ownerOnly, err := historyFileIsOwnerOnly(f)
		if err != nil || !ownerOnly {
			return errors.Join(errors.New("prompt history transaction image is not owner-only"), err)
		}
	}
	return nil
}

func finalizeHistoryPublishedReplacement(parent *historyBoundParent, name string, file *os.File, image historyTransactionImage) error {
	// The identity came from an O_EXCL stage recorded before any prompt bytes.
	// Restore privacy through that exact descriptor if its mode/DACL widened
	// while the process was down, then prove both content and name again.
	if err := secureHistoryFile(file); err != nil {
		return err
	}
	if err := verifyHistoryBoundName(parent, name, file); err != nil {
		return err
	}
	if err := verifyHistoryTransactionImageFile(file, image, true); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return syncHistoryBoundDirectory(parent.root)
}

func moveHistoryBoundFileNoReplace(parent *historyBoundParent, file *os.File, from, to string) (bool, error) {
	return moveHistoryBoundFileNoReplaceAtSeam(parent, file, from, to, "", "")
}

func moveHistoryBoundFileNoReplaceAtSeam(parent *historyBoundParent, file *os.File, from, to, operation, path string) (bool, error) {
	if err := verifyHistoryBoundName(parent, from, file); err != nil {
		return false, err
	}
	if operation != "" && historyNamespaceMutationTestHook != nil {
		historyNamespaceMutationTestHook(operation, path)
	}
	result, err := checkpoint.MoveOpenFileNoReplace(parent.root, file, from, to)
	if err == nil {
		if verifyErr := verifyHistoryBoundName(parent, to, file); verifyErr == nil {
			if _, sourceErr := parent.root.Lstat(from); errors.Is(sourceErr, os.ErrNotExist) {
				return true, nil
			}
		}
	}
	// On POSIX the atomic move names its source. A final-seam substitution can
	// therefore move a foreign inode to the quarantine before the post-check
	// catches it. Put that exact foreign inode back only when the source name is
	// still absent; otherwise retain both names and require recovery.
	if _, sourceErr := parent.root.Lstat(from); errors.Is(sourceErr, os.ErrNotExist) {
		moved, openErr := openHistoryBoundFile(parent, to, false)
		if openErr == nil {
			rollback, rollbackErr := checkpoint.MoveOpenFileNoReplace(parent.root, moved, to, from)
			closeErr := moved.Close()
			if rollback.Published && rollbackErr == nil {
				return false, errors.Join(errHistoryRecoveryRequired, err, closeErr,
					errors.New("a final-seam prompt history substitution was restored"))
			}
			return true, errors.Join(errHistoryRecoveryRequired, err, openErr, rollbackErr, closeErr)
		}
		return true, errors.Join(errHistoryRecoveryRequired, err, openErr)
	}
	return result.Published, errors.Join(errHistoryRecoveryRequired, err,
		errors.New("prompt history no-replace move had an ambiguous namespace outcome"))
}

func retireHistoryBoundFile(parent *historyBoundParent, name string, file *os.File, before func()) error {
	quarantine, err := unusedHistoryBoundName(parent, ".history-cleared-")
	if err != nil {
		return err
	}
	if before != nil {
		before()
	}
	return retireHistoryBoundFileTo(parent, name, quarantine, file)
}

func retireHistoryBoundFileTo(parent *historyBoundParent, name, quarantine string, file *os.File) error {
	if quarantine == "" || !filepath.IsLocal(quarantine) || filepath.Base(quarantine) != quarantine {
		return errors.New("prompt history retirement has an invalid quarantine name")
	}
	if _, err := parent.root.Lstat(quarantine); !errors.Is(err, os.ErrNotExist) {
		return errors.Join(err, errHistoryRecoveryRequired,
			errors.New("prompt history retirement quarantine is occupied"))
	}
	moved, err := moveHistoryBoundFileNoReplace(parent, file, name, quarantine)
	if err != nil {
		return err
	}
	if !moved {
		return errHistoryRecoveryRequired
	}
	if err := scrubHistoryRetiredFile(file); err != nil {
		return err
	}
	return syncHistoryBoundDirectory(parent.root)
}

func runHistoryTransactionBoundary(operation string, boundary historyTransactionBoundary) error {
	if historyTransactionBoundaryHook == nil {
		return nil
	}
	return historyTransactionBoundaryHook(operation, boundary)
}

func historyTransactionRecordName(target string) string {
	digest := sha256.Sum256([]byte("switchboard prompt history transaction\x00" + target))
	return ".history-transaction-" + hex.EncodeToString(digest[:16])
}

func historyExchangeStagingName(source string) string {
	// checkpoint.ExchangeOpenFiles uses this deterministic Windows staging
	// name for its three exact-handle moves. Recording it is what lets history
	// recover a process death after move one or two without guessing.
	digest := sha256.Sum256([]byte("switchboard checkpoint staging\x00" + source))
	return ".switchboard-staging-" + hex.EncodeToString(digest[:16])
}

func historyClearedName(source string) string {
	digest := sha256.Sum256([]byte("switchboard prompt history cleared\x00" + source))
	return ".history-cleared-" + hex.EncodeToString(digest[:16])
}

func createHistoryTransactionRecord(parent *historyBoundParent, txn historyTransaction) (*os.File, string, error) {
	raw, err := json.Marshal(txn)
	if err != nil {
		return nil, "", err
	}
	recordBytes := append(raw, '\n')
	if len(recordBytes) > historyTransactionLimit {
		return nil, "", errors.New("prompt history transaction record exceeds its bound")
	}
	name := historyTransactionRecordName(parent.name)
	if _, err := parent.root.Lstat(name); err == nil {
		return nil, "", errors.Join(errHistoryRecoveryRequired, errors.New("a prompt history transaction is already pending"))
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, "", err
	}
	f, prepareName, err := createHistoryBoundTemp(parent, ".history-transaction-prepare-")
	if err != nil {
		return nil, "", err
	}
	fail := func(cause error) (*os.File, string, error) {
		return nil, "", errors.Join(cause, retireHistoryBoundFile(parent, prepareName, f, nil), f.Close())
	}
	if err := verifyHistoryBoundName(parent, prepareName, f); err != nil {
		return fail(err)
	}
	if n, err := f.Write(recordBytes); err != nil || n != len(recordBytes) {
		if err == nil {
			err = io.ErrShortWrite
		}
		return fail(err)
	}
	if historyTransactionRecordWriteTestHook != nil {
		if err := historyTransactionRecordWriteTestHook(f); err != nil {
			return fail(err)
		}
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := verifyHistoryBoundName(parent, prepareName, f); err != nil {
		return fail(err)
	}
	stored, err := readHistoryTransactionRecord(f)
	if err != nil || !bytes.Equal(stored, recordBytes) {
		return fail(errors.Join(errors.New("prompt history transaction preparation changed before publication"), err))
	}
	if historyTransactionRecordPublishTestHook != nil {
		if err := historyTransactionRecordPublishTestHook(f); err != nil {
			return fail(err)
		}
	}
	moved, err := moveHistoryBoundFileNoReplace(parent, f, prepareName, name)
	if !moved {
		if err == nil {
			err = errors.New("prompt history transaction record was not published")
		}
		return fail(err)
	}
	// Once published, return the live record even alongside a durability or
	// verification error. The caller must preserve its stage and let recovery
	// reconcile the valid ledger rather than treating it as unrecorded.
	if err != nil {
		return f, name, errors.Join(errHistoryRecoveryRequired, err)
	}
	if err := verifyHistoryBoundName(parent, name, f); err != nil {
		return f, name, errors.Join(errHistoryRecoveryRequired, err)
	}
	stored, readErr := readHistoryTransactionRecord(f)
	if readErr != nil || !bytes.Equal(stored, recordBytes) {
		mismatch := errors.Join(errors.New("published prompt history transaction changed at its final seam"), readErr)
		restored, restoreErr := moveHistoryBoundFileNoReplace(parent, f, name, prepareName)
		if !restored || restoreErr != nil {
			return f, name, errors.Join(errHistoryRecoveryRequired, mismatch, restoreErr)
		}
		retireErr := retireHistoryBoundFile(parent, prepareName, f, nil)
		return nil, "", errors.Join(mismatch, retireErr, f.Close())
	}
	if err := syncHistoryBoundDirectory(parent.root); err != nil {
		return f, name, errors.Join(errHistoryRecoveryRequired, err)
	}
	return f, name, nil
}

func recoverHistoryTransactionLocked(path string, lockedParent os.FileInfo) error {
	parent, err := openHistoryBoundParent(path, lockedParent)
	if err != nil {
		return err
	}
	defer parent.close()
	recordName := historyTransactionRecordName(parent.name)
	if _, err := parent.root.Lstat(recordName); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	record, err := openHistoryBoundFile(parent, recordName, true)
	if err != nil {
		return errors.Join(errHistoryRecoveryRequired, err)
	}
	defer record.Close()
	ownerOnly, err := historyFileIsOwnerOnly(record)
	if err != nil || !ownerOnly {
		return errors.Join(errHistoryRecoveryRequired,
			errors.New("prompt history transaction record is not owner-only"), err)
	}
	raw, err := readHistoryTransactionRecord(record)
	if err != nil {
		return errors.Join(errHistoryRecoveryRequired, err)
	}
	var txn historyTransaction
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&txn); err != nil {
		return errors.Join(errHistoryRecoveryRequired, fmt.Errorf("decoding prompt history transaction: %w", err))
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.Join(errHistoryRecoveryRequired, errors.New("prompt history transaction has trailing data"))
	}
	if err := validateHistoryTransaction(parent, txn); err != nil {
		return errors.Join(errHistoryRecoveryRequired, err)
	}
	var recoveryErr error
	switch txn.Operation {
	case "create":
		recoveryErr = recoverHistoryCreateTransaction(parent, txn)
	case "rewrite":
		recoveryErr = recoverHistoryRewriteTransaction(parent, txn)
	case "remove":
		recoveryErr = recoverHistoryRemoveTransaction(parent, txn)
	default:
		recoveryErr = errors.New("prompt history transaction has an unknown operation")
	}
	if recoveryErr != nil {
		return errors.Join(errHistoryRecoveryRequired, recoveryErr)
	}
	if err := retireHistoryBoundFile(parent, recordName, record, nil); err != nil {
		return errors.Join(errHistoryRecoveryRequired, fmt.Errorf("retiring recovered prompt history transaction: %w", err))
	}
	return nil
}

func readHistoryTransactionRecord(f *os.File) ([]byte, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < 1 || info.Size() > historyTransactionLimit {
		return nil, errors.New("prompt history transaction record has an invalid size")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(f, historyTransactionLimit+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > historyTransactionLimit || int64(len(raw)) != info.Size() {
		return nil, errors.New("prompt history transaction changed while it was read")
	}
	return raw, nil
}

func validateHistoryTransaction(parent *historyBoundParent, txn historyTransaction) error {
	if txn.Version != historyTransactionVersion || txn.Target != parent.name {
		return errors.New("prompt history transaction does not name this target")
	}
	validLeaf := func(name string) bool {
		return name != "" && filepath.IsLocal(name) && filepath.Base(name) == name &&
			name != "." && !strings.ContainsRune(name, filepath.Separator)
	}
	if !validLeaf(txn.Stage) || !strings.HasPrefix(txn.Stage, ".history-") {
		return errors.New("prompt history transaction has an invalid stage name")
	}
	validRandomStage := func(prefix string) bool {
		if len(txn.Stage) != len(prefix)+32 || !strings.HasPrefix(txn.Stage, prefix) {
			return false
		}
		_, err := hex.DecodeString(txn.Stage[len(prefix):])
		return err == nil
	}
	if txn.RetiredStage != historyClearedName(txn.Stage) || !validLeaf(txn.RetiredStage) {
		return errors.New("prompt history transaction has an invalid retired stage name")
	}
	switch txn.Operation {
	case "create":
		if !validRandomStage(".history-create-") {
			return errors.New("prompt history creation has an invalid stage name")
		}
		if txn.Expected != (historyTransactionImage{}) || txn.InternalStage != "" || txn.RollbackStage != "" {
			return errors.New("prompt history creation has unexpected source evidence")
		}
		if err := validateHistoryTransactionImage(txn.Replacement); err != nil {
			return fmt.Errorf("invalid prompt history creation evidence: %w", err)
		}
		return nil
	case "rewrite":
		if !validRandomStage(".history-rewrite-") {
			return errors.New("prompt history rewrite has an invalid stage name")
		}
		if txn.InternalStage != historyExchangeStagingName(txn.Stage) || !validLeaf(txn.InternalStage) {
			return errors.New("prompt history transaction has an invalid exchange stage")
		}
		if txn.RollbackStage != historyExchangeStagingName(txn.Target) || !validLeaf(txn.RollbackStage) ||
			txn.RollbackStage == txn.InternalStage {
			return errors.New("prompt history transaction has an invalid rollback stage")
		}
		if err := validateHistoryTransactionImage(txn.Replacement); err != nil {
			return fmt.Errorf("invalid prompt history replacement evidence: %w", err)
		}
		return validateHistoryTransactionImage(txn.Expected)
	case "remove":
		if !validRandomStage(".history-retired-") {
			return errors.New("prompt history removal has an invalid stage name")
		}
		if txn.InternalStage != "" || txn.RollbackStage != "" || txn.Replacement != (historyTransactionImage{}) {
			return errors.New("prompt history removal has unexpected replacement evidence")
		}
		return validateHistoryTransactionImage(txn.Expected)
	default:
		return errors.New("prompt history transaction has an unknown operation")
	}
}

func validateHistoryTransactionImage(image historyTransactionImage) error {
	if image.Identity == "" || image.Size < 0 || image.Size > historyMaxBytes {
		return errors.New("missing or invalid prompt history image identity")
	}
	digest, err := hex.DecodeString(image.Digest)
	if err != nil || len(digest) != sha256.Size {
		return errors.New("invalid prompt history image digest")
	}
	return nil
}

type historyRecoveryImageState struct {
	exists        bool
	identityMatch bool
	ownerOnly     bool
	match         bool
	retired       bool
	file          *os.File
}

func inspectHistoryRecoveryImage(parent *historyBoundParent, name string, image historyTransactionImage) (historyRecoveryImageState, error) {
	if _, err := parent.root.Lstat(name); errors.Is(err, os.ErrNotExist) {
		return historyRecoveryImageState{}, nil
	} else if err != nil {
		return historyRecoveryImageState{}, err
	}
	f, err := openHistoryBoundFile(parent, name, true)
	if err != nil {
		return historyRecoveryImageState{exists: true}, err
	}
	identity, identityErr := historyFileIdentity(f)
	data, readErr := readHistoryBoundDescriptor(f)
	ownerOnly, ownerErr := historyFileIsOwnerOnly(f)
	if identityErr != nil || readErr != nil || ownerErr != nil {
		return historyRecoveryImageState{exists: true}, errors.Join(identityErr, readErr, ownerErr, f.Close())
	}
	digest := sha256.Sum256(data)
	identityMatch := identity == image.Identity
	match := identityMatch && int64(len(data)) == image.Size &&
		hex.EncodeToString(digest[:]) == image.Digest
	retired := identityMatch && len(data) == 0 && ownerOnly
	return historyRecoveryImageState{
		exists: true, identityMatch: identityMatch, ownerOnly: ownerOnly,
		match: match, retired: retired, file: f,
	}, nil
}

func retireHistoryOwnedRecoveryState(parent *historyBoundParent, from, to string, state historyRecoveryImageState) error {
	if !state.exists || !state.identityMatch || state.file == nil {
		return errors.New("prompt history recovery image is not the recorded O_EXCL inode")
	}
	if err := secureHistoryFile(state.file); err != nil {
		return err
	}
	return retireHistoryBoundFileTo(parent, from, to, state.file)
}

func closeHistoryRecoveryStates(states ...historyRecoveryImageState) error {
	var err error
	for _, state := range states {
		if state.file != nil {
			err = errors.Join(err, state.file.Close())
		}
	}
	return err
}

func recoverHistoryCreateTransaction(parent *historyBoundParent, txn historyTransaction) (resultErr error) {
	target, err := inspectHistoryRecoveryImage(parent, txn.Target, txn.Replacement)
	if err != nil {
		return err
	}
	stage, err := inspectHistoryRecoveryImage(parent, txn.Stage, txn.Replacement)
	if err != nil {
		return errors.Join(err, closeHistoryRecoveryStates(target))
	}
	cleared, err := inspectHistoryRecoveryImage(parent, txn.RetiredStage, txn.Replacement)
	if err != nil {
		return errors.Join(err, closeHistoryRecoveryStates(target, stage))
	}
	defer func() { resultErr = errors.Join(resultErr, closeHistoryRecoveryStates(target, stage, cleared)) }()
	switch {
	case stage.identityMatch && !cleared.exists:
		// Publication never consumed the exact O_EXCL stage. It is safe to
		// retire even after a short write: its identity, privacy, and single-link
		// invariant prove this is Switchboard's unpublished inode.
		return retireHistoryOwnedRecoveryState(parent, txn.Stage, txn.RetiredStage, stage)
	case target.match && !stage.exists && !cleared.exists:
		return finalizeHistoryPublishedReplacement(parent, txn.Target, target.file, txn.Replacement)
	case target.identityMatch && !stage.exists && !cleared.exists:
		// A same-inode writer changed Switchboard's published stage after the
		// final comparison. Move that exact owned inode out of the canonical name
		// and scrub it; never accept partial or substituted canonical bytes.
		if err := secureHistoryFile(target.file); err != nil {
			return err
		}
		moved, err := moveHistoryBoundFileNoReplace(parent, target.file, txn.Target, txn.Stage)
		if err != nil || !moved {
			return errors.Join(errHistoryRecoveryRequired, err)
		}
		return retireHistoryOwnedRecoveryState(parent, txn.Stage, txn.RetiredStage, target)
	case !stage.exists && cleared.identityMatch:
		if err := secureHistoryFile(cleared.file); err != nil {
			return err
		}
		return scrubHistoryRetiredFile(cleared.file)
	default:
		return errors.New("prompt history creation recovery found foreign or ambiguous namespace state")
	}
}

func recoverHistoryRemoveTransaction(parent *historyBoundParent, txn historyTransaction) (resultErr error) {
	target, err := inspectHistoryRecoveryImage(parent, txn.Target, txn.Expected)
	if err != nil {
		return err
	}
	stage, err := inspectHistoryRecoveryImage(parent, txn.Stage, txn.Expected)
	if err != nil {
		return errors.Join(err, closeHistoryRecoveryStates(target))
	}
	cleared, err := inspectHistoryRecoveryImage(parent, txn.RetiredStage, txn.Expected)
	if err != nil {
		return errors.Join(err, closeHistoryRecoveryStates(target, stage))
	}
	defer func() { resultErr = errors.Join(resultErr, closeHistoryRecoveryStates(target, stage, cleared)) }()
	switch {
	case !stage.exists && !cleared.exists && target.exists:
		// The durable record preceded the namespace move. Leave the source in
		// place (whatever an uncooperative writer now names there) and let the
		// migration retry after the record is retired.
		return nil
	case !target.identityMatch && (stage.match || stage.retired) && !cleared.exists:
		return retireHistoryBoundFileTo(parent, txn.Stage, txn.RetiredStage, stage.file)
	case !target.exists && stage.identityMatch && !cleared.exists:
		// The exact selected source changed in place at the final seam. Restore
		// that modified inode to its original name instead of scrubbing it.
		moved, err := moveHistoryBoundFileNoReplace(parent, stage.file, txn.Stage, txn.Target)
		if err != nil || !moved {
			return errors.Join(errHistoryRecoveryRequired, err)
		}
		return nil
	case !target.identityMatch && !stage.exists && cleared.match:
		return scrubHistoryRetiredFile(cleared.file)
	case !target.identityMatch && !stage.exists && cleared.retired:
		return nil
	default:
		return errors.New("prompt history removal recovery found foreign or ambiguous namespace state")
	}
}

func recoverHistoryRewriteTransaction(parent *historyBoundParent, txn historyTransaction) (resultErr error) {
	var states []historyRecoveryImageState
	inspect := func(name string, image historyTransactionImage) (historyRecoveryImageState, error) {
		state, err := inspectHistoryRecoveryImage(parent, name, image)
		if err != nil {
			return historyRecoveryImageState{}, errors.Join(err, closeHistoryRecoveryStates(states...))
		}
		states = append(states, state)
		return state, nil
	}
	targetExpected, err := inspect(txn.Target, txn.Expected)
	if err != nil {
		return err
	}
	targetReplacement, err := inspect(txn.Target, txn.Replacement)
	if err != nil {
		return err
	}
	stageExpected, err := inspect(txn.Stage, txn.Expected)
	if err != nil {
		return err
	}
	stageReplacement, err := inspect(txn.Stage, txn.Replacement)
	if err != nil {
		return err
	}
	internalReplacement, err := inspect(txn.InternalStage, txn.Replacement)
	if err != nil {
		return err
	}
	rollbackReplacement, err := inspect(txn.RollbackStage, txn.Replacement)
	if err != nil {
		return err
	}
	clearedExpected, err := inspect(txn.RetiredStage, txn.Expected)
	if err != nil {
		return err
	}
	clearedReplacement, err := inspect(txn.RetiredStage, txn.Replacement)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, closeHistoryRecoveryStates(states...))
	}()

	switch {
	case stageReplacement.identityMatch && !internalReplacement.exists && !rollbackReplacement.exists &&
		!clearedExpected.exists && !clearedReplacement.exists:
		// Unpublished or fully rolled back: retire only Switchboard's staged
		// replacement and leave whatever exact target now exists untouched. The
		// exact owner-only O_EXCL identity also makes a partial write safe to
		// retire after process death.
		return retireHistoryOwnedRecoveryState(parent, txn.Stage, txn.RetiredStage, stageReplacement)
	case !stageExpected.exists && !stageReplacement.exists && !internalReplacement.exists && !rollbackReplacement.exists &&
		clearedReplacement.identityMatch:
		if err := secureHistoryFile(clearedReplacement.file); err != nil {
			return err
		}
		return scrubHistoryRetiredFile(clearedReplacement.file)
	case !targetExpected.identityMatch && !targetReplacement.identityMatch &&
		(stageExpected.match || stageExpected.retired) && !internalReplacement.exists && !rollbackReplacement.exists &&
		!clearedExpected.exists && !clearedReplacement.exists:
		// Publication committed, but the canonical replacement was subsequently
		// removed or replaced. Retire only the exact unchanged old image and leave
		// the new canonical namespace untouched.
		return retireHistoryBoundFileTo(parent, txn.Stage, txn.RetiredStage, stageExpected.file)
	case !targetExpected.identityMatch && !targetReplacement.identityMatch &&
		!stageExpected.exists && !stageReplacement.exists && !internalReplacement.exists && !rollbackReplacement.exists &&
		(clearedExpected.match || clearedExpected.retired):
		if clearedExpected.match {
			return scrubHistoryRetiredFile(clearedExpected.file)
		}
		return nil
	case targetReplacement.identityMatch && stageExpected.identityMatch && !internalReplacement.exists && !rollbackReplacement.exists &&
		!clearedExpected.exists && !clearedReplacement.exists:
		if targetReplacement.match && (stageExpected.match || stageExpected.retired) {
			if err := finalizeHistoryPublishedReplacement(parent, txn.Target, targetReplacement.file, txn.Replacement); err != nil {
				return err
			}
			return retireHistoryBoundFileTo(parent, txn.Stage, txn.RetiredStage, stageExpected.file)
		}
		// Either exact inode changed in place after exchange. Roll back the exact
		// pair, preserving the changed source as canonical and retiring only the
		// O_EXCL replacement. A Windows interruption is covered by RollbackStage.
		if err := secureHistoryFile(targetReplacement.file); err != nil {
			return err
		}
		rolledBack, rollbackErr := checkpoint.RollbackOpenFileExchange(
			parent.root, targetReplacement.file, stageExpected.file, txn.Stage, txn.Target,
		)
		if !rolledBack || rollbackErr != nil {
			return errors.Join(errHistoryRecoveryRequired, rollbackErr)
		}
		if err := errors.Join(
			verifyHistoryBoundName(parent, txn.Target, stageExpected.file),
			verifyHistoryBoundName(parent, txn.Stage, targetReplacement.file),
		); err != nil {
			return errors.Join(errHistoryRecoveryRequired, err)
		}
		return retireHistoryOwnedRecoveryState(parent, txn.Stage, txn.RetiredStage, targetReplacement)
	case targetReplacement.match && !stageExpected.exists && !stageReplacement.exists &&
		!internalReplacement.exists && !rollbackReplacement.exists && clearedExpected.match:
		if err := finalizeHistoryPublishedReplacement(parent, txn.Target, targetReplacement.file, txn.Replacement); err != nil {
			return err
		}
		return scrubHistoryRetiredFile(clearedExpected.file)
	case targetReplacement.match && !stageExpected.exists && !stageReplacement.exists &&
		!internalReplacement.exists && !rollbackReplacement.exists && clearedExpected.retired:
		if err := finalizeHistoryPublishedReplacement(parent, txn.Target, targetReplacement.file, txn.Replacement); err != nil {
			return err
		}
		return nil
	case targetExpected.identityMatch && !stageExpected.exists && !stageReplacement.exists &&
		internalReplacement.identityMatch && !rollbackReplacement.exists && !clearedExpected.exists && !clearedReplacement.exists:
		// Windows exchange step one completed. Restore the staged replacement
		// to its owned name, then retire it as an unpublished temporary.
		if err := secureHistoryFile(internalReplacement.file); err != nil {
			return err
		}
		moved, err := moveHistoryBoundFileNoReplace(parent, internalReplacement.file, txn.InternalStage, txn.Stage)
		if err != nil || !moved {
			return errors.Join(errHistoryRecoveryRequired, err)
		}
		return retireHistoryOwnedRecoveryState(parent, txn.Stage, txn.RetiredStage, internalReplacement)
	case targetExpected.identityMatch && !stageExpected.exists && !stageReplacement.exists &&
		!internalReplacement.exists && rollbackReplacement.identityMatch && !clearedExpected.exists && !clearedReplacement.exists:
		// Windows rollback step two completed. Restore the replacement to its
		// private stage and retire it; the exact source is already canonical.
		if err := secureHistoryFile(rollbackReplacement.file); err != nil {
			return err
		}
		moved, err := moveHistoryBoundFileNoReplace(parent, rollbackReplacement.file, txn.RollbackStage, txn.Stage)
		if err != nil || !moved {
			return errors.Join(errHistoryRecoveryRequired, err)
		}
		return retireHistoryOwnedRecoveryState(parent, txn.Stage, txn.RetiredStage, rollbackReplacement)
	case internalReplacement.identityMatch && !stageExpected.identityMatch && !stageReplacement.identityMatch &&
		!targetReplacement.identityMatch && !rollbackReplacement.identityMatch &&
		!clearedExpected.exists && !clearedReplacement.exists:
		// Forward step one left only the owned replacement in its deterministic
		// internal name. If external writers removed/replaced the source or
		// occupied its now-free stage name, discard the exact owned inode directly
		// without touching either foreign namespace entry.
		return retireHistoryOwnedRecoveryState(parent, txn.InternalStage, txn.RetiredStage, internalReplacement)
	case rollbackReplacement.identityMatch && !stageExpected.identityMatch && !stageReplacement.identityMatch &&
		!targetReplacement.identityMatch && !internalReplacement.identityMatch &&
		!clearedExpected.exists && !clearedReplacement.exists:
		// The equivalent state after rollback step two uses RollbackStage.
		return retireHistoryOwnedRecoveryState(parent, txn.RollbackStage, txn.RetiredStage, rollbackReplacement)
	case !targetExpected.exists && !targetReplacement.exists && stageExpected.identityMatch &&
		internalReplacement.identityMatch && !rollbackReplacement.exists && !clearedExpected.exists && !clearedReplacement.exists:
		if stageExpected.match && internalReplacement.match {
			// Windows forward exchange step two completed. Finish publication with
			// the exact complete replacement, then retire the unchanged old image.
			if err := secureHistoryFile(internalReplacement.file); err != nil {
				return err
			}
			moved, err := moveHistoryBoundFileNoReplace(parent, internalReplacement.file, txn.InternalStage, txn.Target)
			if err != nil || !moved {
				return errors.Join(errHistoryRecoveryRequired, err)
			}
			if err := finalizeHistoryPublishedReplacement(parent, txn.Target, internalReplacement.file, txn.Replacement); err != nil {
				return err
			}
			return retireHistoryBoundFileTo(parent, txn.Stage, txn.RetiredStage, stageExpected.file)
		}
		// One of the exact images changed in place. Finish a rollback instead:
		// restore the changed source, then retire only the owned replacement.
		moved, err := moveHistoryBoundFileNoReplace(parent, stageExpected.file, txn.Stage, txn.Target)
		if err != nil || !moved {
			return errors.Join(errHistoryRecoveryRequired, err)
		}
		if err := secureHistoryFile(internalReplacement.file); err != nil {
			return err
		}
		moved, err = moveHistoryBoundFileNoReplace(parent, internalReplacement.file, txn.InternalStage, txn.Stage)
		if err != nil || !moved {
			return errors.Join(errHistoryRecoveryRequired, err)
		}
		return retireHistoryOwnedRecoveryState(parent, txn.Stage, txn.RetiredStage, internalReplacement)
	case !targetExpected.exists && !targetReplacement.exists && stageExpected.identityMatch &&
		!internalReplacement.exists && rollbackReplacement.identityMatch && !clearedExpected.exists && !clearedReplacement.exists:
		// Windows rollback step one completed. Finish both remaining exact moves;
		// either intermediate state is itself recognized on the next recovery.
		moved, err := moveHistoryBoundFileNoReplace(parent, stageExpected.file, txn.Stage, txn.Target)
		if err != nil || !moved {
			return errors.Join(errHistoryRecoveryRequired, err)
		}
		if err := secureHistoryFile(rollbackReplacement.file); err != nil {
			return err
		}
		moved, err = moveHistoryBoundFileNoReplace(parent, rollbackReplacement.file, txn.RollbackStage, txn.Stage)
		if err != nil || !moved {
			return errors.Join(errHistoryRecoveryRequired, err)
		}
		return retireHistoryOwnedRecoveryState(parent, txn.Stage, txn.RetiredStage, rollbackReplacement)
	default:
		return errors.New("prompt history rewrite recovery found foreign or ambiguous namespace state")
	}
}

func openHistoryFile(path string, writable, create bool, expectedParent os.FileInfo) (*os.File, os.FileInfo, error) {
	parentPath := filepath.Dir(path)
	parent, err := os.Lstat(parentPath)
	if err != nil {
		return nil, nil, err
	}
	if !parent.IsDir() {
		return nil, nil, fmt.Errorf("prompt history parent %s is not a directory", parentPath)
	}
	if expectedParent != nil && !os.SameFile(expectedParent, parent) {
		return nil, nil, fmt.Errorf("prompt history parent %s changed", parentPath)
	}
	var before os.FileInfo
	createNew := false
	if before, err = os.Lstat(path); err == nil {
		if !before.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("prompt history %s is not a regular file", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) || !create {
		return nil, nil, err
	} else {
		createNew = true
	}
	f, err := openHistoryDataDescriptor(path, writable, createNew)
	if err != nil {
		return nil, nil, err
	}
	if err := verifyHistoryPath(f, path, parent); err != nil {
		_ = f.Close()
		return nil, nil, err
	}
	if before != nil {
		opened, err := f.Stat()
		if err != nil || !os.SameFile(before, opened) {
			_ = f.Close()
			return nil, nil, errors.Join(fmt.Errorf("prompt history %s changed while opening", path), err)
		}
	}
	if writable {
		if err := secureHistoryFile(f); err != nil {
			_ = f.Close()
			return nil, nil, err
		}
		ownerOnly, err := historyFileIsOwnerOnly(f)
		if err != nil || !ownerOnly {
			_ = f.Close()
			return nil, nil, errors.Join(errors.New("prompt history is not owner-only after securing it"), err)
		}
		if err := verifyHistoryPath(f, path, parent); err != nil {
			_ = f.Close()
			return nil, nil, err
		}
	}
	return f, parent, nil
}

func verifyHistoryPath(f *os.File, path string, expectedParent os.FileInfo) error {
	opened, err := f.Stat()
	if err != nil {
		return err
	}
	if !opened.Mode().IsRegular() {
		return fmt.Errorf("prompt history %s is not a regular file", path)
	}
	links, err := historyFileLinkCount(f)
	if err != nil {
		return err
	}
	if links != 1 {
		return fmt.Errorf("prompt history %s has %d hard links", path, links)
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(opened, current) {
		return errors.Join(fmt.Errorf("prompt history %s changed while open", path), err)
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil || !parent.IsDir() || !os.SameFile(expectedParent, parent) {
		return errors.Join(fmt.Errorf("prompt history parent changed while open"), err)
	}
	return nil
}

func withHistoryLock(path string, createParent bool, fn func(os.FileInfo) error) (resultErr error) {
	path = filepath.Clean(path)
	return withHistoryLocks([]string{path}, createParent, func(parents map[string]os.FileInfo) error {
		return fn(parents[path])
	})
}

type heldHistoryLock struct {
	path   string
	parent os.FileInfo
	file   *os.File
}

// withHistoryLocks holds every named history sidecar in lexical order. A
// one-time workspace-key migration must exclude appenders for both the old and
// new names at once; nested withHistoryLock calls would deadlock and acquiring
// them in caller order would deadlock two processes that discovered aliases in
// a different order.
func withHistoryLocks(paths []string, createParents bool, fn func(map[string]os.FileInfo) error) (resultErr error) {
	unique := make(map[string]bool, len(paths))
	ordered := make([]string, 0, len(paths))
	for _, path := range paths {
		path = filepath.Clean(path)
		if unique[path] {
			continue
		}
		unique[path] = true
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	if len(ordered) == 0 {
		return fn(map[string]os.FileInfo{})
	}

	held := make([]heldHistoryLock, 0, len(ordered))
	defer func() {
		for i := len(held) - 1; i >= 0; i-- {
			resultErr = errors.Join(resultErr, unlockHistoryFileLock(held[i].file), held[i].file.Close())
		}
	}()
	parents := make(map[string]os.FileInfo, len(ordered))
	for _, path := range ordered {
		parentPath := filepath.Dir(path)
		if createParents {
			if err := fileprivacy.EnsurePrivateDir(parentPath); err != nil {
				return err
			}
		}
		parent, err := os.Lstat(parentPath)
		if err != nil {
			return err
		}
		if !parent.IsDir() {
			return fmt.Errorf("prompt history parent %s is not a directory", parentPath)
		}
		lockPath := path + ".lock"
		var lockBefore os.FileInfo
		createLock := false
		if lockBefore, err = os.Lstat(lockPath); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			createLock = true
		} else if !lockBefore.Mode().IsRegular() {
			return fmt.Errorf("prompt history lock %s is not a regular file", lockPath)
		}
		lock, err := openHistoryLockDescriptor(lockPath, createLock)
		if err != nil {
			return err
		}
		closeOnError := func(err error) error { return errors.Join(err, lock.Close()) }
		if err := verifyHistoryPath(lock, lockPath, parent); err != nil {
			return closeOnError(err)
		}
		if lockBefore != nil {
			opened, statErr := lock.Stat()
			if statErr != nil || !os.SameFile(lockBefore, opened) {
				return closeOnError(errors.Join(errors.New("prompt history lock changed while opening"), statErr))
			}
		}
		if err := secureHistoryFile(lock); err != nil {
			return closeOnError(err)
		}
		ownerOnly, err := historyFileIsOwnerOnly(lock)
		if err != nil || !ownerOnly {
			return closeOnError(errors.Join(errors.New("prompt history lock is not owner-only after securing it"), err))
		}
		if err := verifyHistoryPath(lock, lockPath, parent); err != nil {
			return closeOnError(err)
		}
		locked, err := tryHistoryFileLock(lock)
		if err != nil {
			return closeOnError(err)
		}
		if !locked {
			return closeOnError(errHistoryBusy)
		}
		if err := verifyHistoryPath(lock, lockPath, parent); err != nil {
			return errors.Join(err, unlockHistoryFileLock(lock), lock.Close())
		}
		held = append(held, heldHistoryLock{path: lockPath, parent: parent, file: lock})
		parents[path] = parent
	}
	for _, item := range held {
		if err := verifyHistoryPath(item.file, item.path, item.parent); err != nil {
			return err
		}
	}
	// A Windows exchange spans three exact-handle moves, and any platform can
	// die after its atomic namespace transition but before displaced bytes are
	// scrubbed. Recover each deterministic, owner-only transaction record only
	// after all cooperating names are locked. Ambiguous or foreign state fails
	// closed and remains available for manual recovery.
	for _, path := range ordered {
		if err := recoverHistoryTransactionLocked(path, parents[path]); err != nil {
			return err
		}
	}
	operationErr := fn(parents)
	var verifyErr error
	for _, item := range held {
		verifyErr = errors.Join(verifyErr, verifyHistoryPath(item.file, item.path, item.parent))
	}
	return errors.Join(operationErr, verifyErr)
}
