package checkpoint

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

const (
	restoreTempLedgerPrefix       = ".switchboard-undo-cleanup-"
	restoreTempLedgerMagic        = "switchboard-undo-cleanup 1\n"
	maxRestoreTempLedgerBytes     = 4096
	maxRestoreTempLedgerFileBytes = 3 * (maxRestoreTempLedgerBytes + len(restoreTempLedgerMagic) + sha256.Size*2 + 2)
	maxRestoreLedgerInventory     = 2048
	maxRestoreLedgerDirEntries    = 100000
)

var cleanupExactRestoreTempBeforeRetire func(target, name string)

type restoreCleanupConfig struct {
	dir               string
	dirIdentity       string
	dirRoot           *os.Root
	workspace         string
	workspaceIdentity string
	workspaceRoot     *os.Root
}

type restoreTempLedgerBoundary uint8

const (
	restoreLedgerReservationBeforeSync restoreTempLedgerBoundary = iota + 1
	restoreLedgerReservationAfterSync
	restoreLedgerBindingBeforeSync
	restoreLedgerBindingAfterSync
	restoreLedgerDisplacedBeforeSync
	restoreLedgerDisplacedAfterSync
)

type restoreTempLedgerRecord struct {
	Version           int                    `json:"version"`
	Workspace         string                 `json:"workspace"`
	WorkspaceIdentity string                 `json:"workspace_identity"`
	Target            string                 `json:"target"`
	TempName          string                 `json:"temp_name"`
	TempIdentity      string                 `json:"temp_identity,omitempty"`
	TempOwned         bool                   `json:"temp_owned,omitempty"`
	DisplacedIdentity string                 `json:"displaced_identity,omitempty"`
	Expected          durableFileFingerprint `json:"expected"`
	Desired           durableFileFingerprint `json:"desired"`
}

type restoreTempLease struct {
	handle *durableUndoHandle
	record restoreTempLedgerRecord
	hook   func(restoreTempLedgerBoundary)
}

// ConfigureRestoreCleanup enables crash cleanup for ordinary /undo as well as
// durable /retry. The ledger lives in the user-owned per-workspace session
// directory, never in the repository. Call it once before the recorder is
// shared with tools.
func (r *Recorder) ConfigureRestoreCleanup(journalDir, workspace string) error {
	config, err := resolveRestoreCleanup(journalDir, workspace)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.cur != nil || len(r.turns) != 0 || r.activeTransactions != 0 || r.activeRestores != 0 {
		return errors.New("checkpoint cleanup must be configured before recording starts")
	}
	r.restoreCleanup = config
	return nil
}

func resolveRestoreCleanup(journalDir, workspace string) (*restoreCleanupConfig, error) {
	dir, err := resolvedDirectory(journalDir)
	if err != nil {
		return nil, fmt.Errorf("resolving checkpoint cleanup directory: %w", err)
	}
	retirement, err := openRestoreScope(dir)
	if err != nil {
		return nil, fmt.Errorf("binding checkpoint cleanup directory: %w", err)
	}
	dirIdentity := retirement.identity
	if err := retirement.close(); err != nil {
		return nil, err
	}
	root, err := resolvedDirectory(workspace)
	if err != nil {
		return nil, fmt.Errorf("resolving checkpoint cleanup workspace: %w", err)
	}
	scope, err := openRestoreScope(root)
	if err != nil {
		return nil, fmt.Errorf("binding checkpoint cleanup workspace: %w", err)
	}
	identity := scope.identity
	if err := scope.close(); err != nil {
		return nil, err
	}
	return &restoreCleanupConfig{
		dir: dir, dirIdentity: dirIdentity,
		workspace: root, workspaceIdentity: identity,
	}, nil
}

func resolveRestoreCleanupBound(journalDir, workspace string, journalRoot, workspaceRoot *os.Root) (*restoreCleanupConfig, error) {
	if journalRoot == nil || workspaceRoot == nil {
		return nil, errors.New("bound checkpoint cleanup requires journal and workspace capabilities")
	}
	journalScope, err := borrowRestoreScope(journalDir, journalRoot)
	if err != nil {
		return nil, fmt.Errorf("binding retained checkpoint cleanup directory: %w", err)
	}
	defer journalScope.close()
	workspaceScope, err := borrowRestoreScope(workspace, workspaceRoot)
	if err != nil {
		return nil, fmt.Errorf("binding retained checkpoint cleanup workspace: %w", err)
	}
	defer workspaceScope.close()
	return &restoreCleanupConfig{
		dir: journalScope.path, dirIdentity: journalScope.identity, dirRoot: journalRoot,
		workspace: workspaceScope.path, workspaceIdentity: workspaceScope.identity, workspaceRoot: workspaceRoot,
	}, nil
}

func configuredRestoreScope(path, identity string, root *os.Root) (*restoreScope, error) {
	if root == nil {
		scope, err := openRestoreScope(path)
		if err != nil || (identity != "" && scope.identity != identity) {
			if scope != nil {
				_ = scope.close()
			}
			return nil, errors.Join(fmt.Errorf("%w: restore scope changed identity", ErrStale), err)
		}
		return scope, nil
	}
	bound, err := root.Stat(".")
	if err != nil || !bound.IsDir() {
		return nil, errors.Join(fmt.Errorf("%w: retained restore scope is unavailable", ErrStale), err)
	}
	currentIdentity, err := boundRootIdentity(root)
	if err != nil || currentIdentity != identity {
		return nil, errors.Join(fmt.Errorf("%w: retained restore scope changed identity", ErrStale), err)
	}
	return &restoreScope{root: root, path: path, info: bound, identity: identity, borrowed: true}, nil
}

// RecoverFilePublicationCleanup reconciles temporary files and cleanup
// ledgers left by an interrupted PublishFileCAS call. It intentionally does
// not inspect the durable retry journal: callers that use the atomic file
// publisher outside /retry own only these per-publication records.
//
// Callers must serialize this recovery with their own state-file lock before
// accepting another mutation. journalDir is the trusted retirement directory;
// workspace is the directory tree containing the published target.
func RecoverFilePublicationCleanup(journalDir, workspace string) error {
	config, err := resolveRestoreCleanup(journalDir, workspace)
	if err != nil {
		return err
	}
	return recoverFilePublicationCleanup(config)
}

// RecoverFilePublicationCleanupBound is RecoverFilePublicationCleanup bound
// to the exact retained journal and workspace capabilities held by the state
// authority whose lock serializes recovery.
func RecoverFilePublicationCleanupBound(journalDir, workspace string, journalRoot, workspaceRoot *os.Root) error {
	config, err := resolveRestoreCleanupBound(journalDir, workspace, journalRoot, workspaceRoot)
	if err != nil {
		return err
	}
	return recoverFilePublicationCleanup(config)
}

func recoverFilePublicationCleanup(config *restoreCleanupConfig) error {
	restoreRoot, err := configuredRestoreScope(config.workspace, config.workspaceIdentity, config.workspaceRoot)
	if err != nil {
		return errors.Join(fmt.Errorf("%w: binding file-publication workspace", ErrStale), err)
	}
	defer restoreRoot.close()
	retirement, err := configuredRestoreScope(config.dir, config.dirIdentity, config.dirRoot)
	if err != nil {
		return errors.Join(fmt.Errorf("%w: binding file-publication cleanup directory", ErrStale), err)
	}
	defer retirement.close()
	return cleanupRestoreTempLedgers(retirement, restoreRoot)
}

func (r *Recorder) ensureRestoreCleanup(journalDir, workspace string) error {
	config, err := resolveRestoreCleanup(journalDir, workspace)
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.restoreCleanup != nil {
		if *r.restoreCleanup != *config {
			return fmt.Errorf("%w: checkpoint cleanup was configured for another workspace", ErrStale)
		}
		return nil
	}
	if r.activeTransactions != 0 || r.activeRestores != 0 {
		return fmt.Errorf("%w: checkpoint cleanup cannot be configured during mutation", ErrStale)
	}
	r.restoreCleanup = config
	return nil
}

func (r *Recorder) restoreWithLedger(path string, state *fileState, hooks restoreHooks) restoreOutcome {
	r.mu.Lock()
	config := r.restoreCleanup
	hook := r.restoreTempLedgerHook
	r.mu.Unlock()
	if config == nil {
		scope, target, err := openFilesystemRestoreScope(path)
		if err != nil {
			return unpublishedRestore(fmt.Errorf("binding filesystem restore namespace: %w", err))
		}
		defer scope.close()
		return restoreInScope(scope, target, state, hooks)
	}
	lease, err := beginRestoreTempLease(config, path, state, hooks.tempName, hook)
	if err != nil {
		return unpublishedRestore(err)
	}
	retirementScope, err := configuredRestoreScope(config.dir, config.dirIdentity, config.dirRoot)
	if err != nil {
		retainErr := lease.retain()
		if retirementScope != nil {
			_ = retirementScope.close()
		}
		return unpublishedRestore(errors.Join(
			fmt.Errorf("%w: binding checkpoint retirement directory", ErrStale), err, retainErr))
	}
	defer retirementScope.close()
	workspaceScope, err := configuredRestoreScope(config.workspace, config.workspaceIdentity, config.workspaceRoot)
	if err != nil {
		retainErr := lease.retain()
		return unpublishedRestore(errors.Join(
			fmt.Errorf("%w: binding checkpoint workspace", ErrStale), err, retainErr))
	}
	defer workspaceScope.close()
	hooks.tempName = lease.record.TempName
	hooks.bindTemp = lease.bindTemp
	hooks.bindDisplaced = lease.bindDisplaced
	hooks.retirementRoot = retirementScope.root
	// The configured workspace is canonical, while callers may reach the same
	// inode through a platform alias (for example /var versus /private/var on
	// macOS). The durable reservation already resolved and identity-checked the
	// target against that workspace; use that exact canonical spelling for all
	// root-relative namespace operations.
	outcome := restoreInScope(workspaceScope, lease.record.Target, state, hooks)
	if outcome.cleanupPending || (outcome.published && outcome.err != nil) {
		// Preserve evidence whenever namespace publication was ambiguous or the
		// exact temporary retirement was not positively completed. A clean
		// pre-publication refusal, including one before a temporary was created,
		// removes the reservation below instead of leaving a permanent false
		// recovery record.
		if err := lease.retain(); err != nil {
			outcome.err = errors.Join(outcome.err,
				fmt.Errorf("retaining checkpoint cleanup evidence: %w", err))
		}
		return outcome
	}
	if err := lease.remove(); err != nil {
		if outcome.published {
			return publishedRestoreError(path, errors.Join(outcome.err,
				fmt.Errorf("removing checkpoint cleanup ledger: %w", err)))
		}
		outcome.err = errors.Join(outcome.err, fmt.Errorf("removing checkpoint cleanup ledger: %w", err))
	}
	return outcome
}

func beginRestoreTempLease(config *restoreCleanupConfig, path string, state *fileState, requested string, hook func(restoreTempLedgerBoundary)) (*restoreTempLease, error) {
	if config == nil || state == nil {
		return nil, errors.New("checkpoint cleanup has no configuration or state")
	}
	scope, err := configuredRestoreScope(config.workspace, config.workspaceIdentity, config.workspaceRoot)
	if err != nil {
		return nil, err
	}
	canonicalTarget, err := resolvedWorkspaceTarget(path, config.workspace)
	if err != nil {
		_ = scope.close()
		return nil, err
	}
	if _, err := scope.relative(canonicalTarget); err != nil {
		_ = scope.close()
		return nil, err
	}
	if err := scope.close(); err != nil {
		return nil, err
	}
	tempName := requested
	if tempName == "" {
		tempName, err = randomRestoreName()
		if err != nil {
			return nil, err
		}
	}
	if !isRestoreTempName(tempName) {
		return nil, fmt.Errorf("invalid checkpoint cleanup temporary name %q", tempName)
	}
	record := restoreTempLedgerRecord{
		Version:           1,
		Workspace:         config.workspace,
		WorkspaceIdentity: config.workspaceIdentity,
		Target:            canonicalTarget,
		TempName:          tempName,
		Expected:          encodeDurableFingerprint(state.after),
		Desired: encodeDurableFingerprint(
			fingerprintBytes(state.existed, state.mode, state.content)),
	}
	frame, err := encodeRestoreTempLedger(record)
	if err != nil {
		return nil, err
	}
	suffix := strings.TrimPrefix(tempName, ".switchboard-undo-")
	ledgerPath := filepath.Join(config.dir, restoreTempLedgerPrefix+suffix)
	control, err := configuredRestoreScope(config.dir, config.dirIdentity, config.dirRoot)
	if err != nil {
		if control != nil {
			_ = control.close()
		}
		return nil, errors.Join(
			fmt.Errorf("%w: binding checkpoint cleanup directory", ErrStale), err)
	}
	file, err := control.root.OpenFile(filepath.Base(ledgerPath), os.O_RDWR|os.O_CREATE|os.O_EXCL|os.O_SYNC, 0o600)
	closeRootErr := control.close()
	if err != nil {
		return nil, errors.Join(fmt.Errorf("creating checkpoint cleanup ledger: %w", err), closeRootErr)
	}
	if closeRootErr != nil {
		_ = file.Close()
		return nil, closeRootErr
	}
	handle := &durableUndoHandle{
		file: file, path: ledgerPath, dir: config.dir,
		dirIdentity: config.dirIdentity, dirRoot: config.dirRoot,
	}
	cleanup := func(cause error) (*restoreTempLease, error) {
		return nil, errors.Join(cause, handle.remove())
	}
	if err := acquireJournalLock(file); err != nil {
		return cleanup(fmt.Errorf("locking checkpoint cleanup ledger: %w", err))
	}
	if err := writeAll(file, frame); err != nil {
		return cleanup(fmt.Errorf("writing checkpoint cleanup ledger: %w", err))
	}
	if hook != nil {
		hook(restoreLedgerReservationBeforeSync)
	}
	if err := file.Sync(); err != nil {
		return cleanup(fmt.Errorf("syncing checkpoint cleanup ledger: %w", err))
	}
	if hook != nil {
		hook(restoreLedgerReservationAfterSync)
	}
	info, err := file.Stat()
	if err != nil {
		return cleanup(err)
	}
	handle.identity = info
	control, err = configuredRestoreScope(config.dir, config.dirIdentity, config.dirRoot)
	if err != nil {
		if control != nil {
			_ = control.close()
		}
		return cleanup(errors.Join(fmt.Errorf("%w: rebinding checkpoint cleanup directory", ErrStale), err))
	}
	syncErr := syncBoundDirectory(control.root)
	closeErr := control.close()
	if syncErr != nil || closeErr != nil {
		return cleanup(fmt.Errorf("publishing checkpoint cleanup ledger: %w", errors.Join(syncErr, closeErr)))
	}
	return &restoreTempLease{handle: handle, record: record, hook: hook}, nil
}

func (l *restoreTempLease) bindTemp(file *os.File, owned bool) error {
	if l == nil || l.handle == nil || l.handle.file == nil {
		return errors.New("checkpoint cleanup lease is closed")
	}
	identity, err := boundOpenFileIdentity(file)
	if err != nil {
		return fmt.Errorf("identifying checkpoint temporary: %w", err)
	}
	l.record.TempIdentity = identity
	l.record.TempOwned = owned
	return l.appendRecord()
}

func (l *restoreTempLease) bindDisplaced(file *os.File) error {
	if l == nil || l.handle == nil || l.handle.file == nil {
		return errors.New("checkpoint cleanup lease is closed")
	}
	identity, err := boundOpenFileIdentity(file)
	if err != nil {
		return fmt.Errorf("identifying displaced checkpoint target: %w", err)
	}
	l.record.DisplacedIdentity = identity
	return l.appendRecord()
}

func (l *restoreTempLease) appendRecord() error {
	frame, err := encodeRestoreTempLedger(l.record)
	if err != nil {
		return err
	}
	ledger := l.handle.file
	// Append the bound-inode transition. The initial reservation frame remains
	// valid until this complete frame is synced, so a kill at any byte leaves at
	// least one decodable state. Pre-image bytes are written only after return.
	if _, err := ledger.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	if err := writeAll(ledger, frame); err != nil {
		return err
	}
	if l.hook != nil {
		if l.record.DisplacedIdentity == "" {
			l.hook(restoreLedgerBindingBeforeSync)
		} else {
			l.hook(restoreLedgerDisplacedBeforeSync)
		}
	}
	if err := ledger.Sync(); err != nil {
		return err
	}
	if l.hook != nil {
		if l.record.DisplacedIdentity == "" {
			l.hook(restoreLedgerBindingAfterSync)
		} else {
			l.hook(restoreLedgerDisplacedAfterSync)
		}
	}
	return nil
}

func (l *restoreTempLease) remove() error {
	if l == nil || l.handle == nil {
		return nil
	}
	err := l.handle.remove()
	l.handle = nil
	return err
}

func (l *restoreTempLease) retain() error {
	if l == nil || l.handle == nil {
		return nil
	}
	err := l.handle.close()
	l.handle = nil
	return err
}

func encodeRestoreTempLedger(record restoreTempLedgerRecord) ([]byte, error) {
	payload, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	if len(payload) > maxRestoreTempLedgerBytes {
		return nil, errors.New("checkpoint cleanup ledger exceeds its byte bound")
	}
	digest := sha256.Sum256(payload)
	frame := make([]byte, 0, len(restoreTempLedgerMagic)+sha256.Size*2+1+len(payload))
	frame = append(frame, restoreTempLedgerMagic...)
	frame = append(frame, hex.EncodeToString(digest[:])...)
	frame = append(frame, '\n')
	frame = append(frame, payload...)
	frame = append(frame, '\n')
	return frame, nil
}

func decodeRestoreTempLedger(file *os.File) (restoreTempLedgerRecord, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return restoreTempLedgerRecord{}, err
	}
	reader := bufio.NewReader(io.LimitReader(file, int64(maxRestoreTempLedgerFileBytes+1)))
	var last restoreTempLedgerRecord
	haveLast := false
	fallback := func(consumed []byte, cause error) (restoreTempLedgerRecord, error) {
		if !haveLast {
			return restoreTempLedgerRecord{}, cause
		}
		remaining, readErr := io.ReadAll(reader)
		if readErr != nil {
			return restoreTempLedgerRecord{}, errors.Join(cause, readErr)
		}
		// A crash can tear only the append at EOF. If another framed record
		// follows the invalid bytes, the corruption is interior rather than a
		// torn final transition and must fail closed. Skip the current frame's
		// own valid magic before looking for a later one.
		trailing := append(append([]byte(nil), consumed...), remaining...)
		search := trailing
		if bytes.HasPrefix(search, []byte(restoreTempLedgerMagic)) {
			search = search[len(restoreTempLedgerMagic):]
		}
		if bytes.Contains(search, []byte(restoreTempLedgerMagic)) {
			return restoreTempLedgerRecord{}, cause
		}
		return last, nil
	}
	for {
		magic, err := reader.ReadString('\n')
		if errors.Is(err, io.EOF) && magic == "" {
			if haveLast {
				return last, nil
			}
			return restoreTempLedgerRecord{}, errors.New("checkpoint cleanup ledger is empty")
		}
		if err != nil {
			return fallback([]byte(magic), errors.New("checkpoint cleanup ledger has an incomplete header"))
		}
		if magic != restoreTempLedgerMagic {
			return fallback([]byte(magic), errors.New("checkpoint cleanup ledger has an invalid header"))
		}
		wantHex, err := reader.ReadString('\n')
		if err != nil {
			return fallback([]byte(magic+wantHex), errors.New("checkpoint cleanup ledger has an incomplete checksum"))
		}
		if len(wantHex) != sha256.Size*2+1 {
			return fallback([]byte(magic+wantHex), errors.New("checkpoint cleanup ledger has an invalid checksum header"))
		}
		want, err := hex.DecodeString(strings.TrimSuffix(wantHex, "\n"))
		if err != nil || len(want) != sha256.Size {
			return fallback([]byte(magic+wantHex), errors.New("checkpoint cleanup ledger checksum is invalid"))
		}
		payloadLine, err := reader.ReadString('\n')
		if err != nil {
			return fallback([]byte(magic+wantHex+payloadLine), errors.New("checkpoint cleanup ledger has an incomplete payload"))
		}
		payload := []byte(strings.TrimSuffix(payloadLine, "\n"))
		if len(payload) == 0 || len(payload) > maxRestoreTempLedgerBytes {
			return fallback([]byte(magic+wantHex+payloadLine), errors.New("checkpoint cleanup ledger payload is invalid"))
		}
		got := sha256.Sum256(payload)
		if !bytes.Equal(got[:], want) {
			return fallback([]byte(magic+wantHex+payloadLine), errors.New("checkpoint cleanup ledger checksum does not match"))
		}
		var record restoreTempLedgerRecord
		decoder := json.NewDecoder(bytes.NewReader(payload))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&record); err != nil {
			return fallback([]byte(magic+wantHex+payloadLine), err)
		}
		if err := ensureJSONEOF(decoder); err != nil {
			return fallback([]byte(magic+wantHex+payloadLine), err)
		}
		last, haveLast = record, true
	}
}

// cleanupRestoreTempLedgers has two independent bounds: directory inventory
// and matching ledger count. It never walks the workspace and never treats a
// repository filename prefix as ownership evidence.
func cleanupRestoreTempLedgers(control, scope *restoreScope) error {
	if control == nil || control.root == nil {
		return errors.New("checkpoint cleanup directory capability is closed")
	}
	directory, err := control.root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	entriesSeen, ledgersSeen := 0, 0
	for {
		entries, readErr := directory.ReadDir(128)
		for _, entry := range entries {
			entriesSeen++
			if entriesSeen > maxRestoreLedgerDirEntries {
				return fmt.Errorf("checkpoint cleanup directory exceeds its %d-entry inventory bound", maxRestoreLedgerDirEntries)
			}
			if !isRestoreTempLedgerName(entry.Name()) {
				continue
			}
			ledgersSeen++
			if ledgersSeen > maxRestoreLedgerInventory {
				return fmt.Errorf("checkpoint cleanup exceeds its %d-ledger bound", maxRestoreLedgerInventory)
			}
			if err := cleanupRestoreTempLedger(control, entry.Name(), scope); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func isRestoreTempLedgerName(name string) bool {
	if len(name) != len(restoreTempLedgerPrefix)+32 || !strings.HasPrefix(name, restoreTempLedgerPrefix) {
		return false
	}
	_, err := hex.DecodeString(name[len(restoreTempLedgerPrefix):])
	return err == nil
}

func cleanupRestoreTempLedger(control *restoreScope, name string, scope *restoreScope) error {
	path := filepath.Join(control.path, name)
	linked, err := control.root.Lstat(name)
	if err != nil {
		return err
	}
	if !linked.Mode().IsRegular() || linked.Size() > int64(maxRestoreTempLedgerFileBytes) {
		return fmt.Errorf("checkpoint cleanup ledger %s is not a bounded regular file", path)
	}
	file, err := control.root.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(linked, opened) {
		_ = file.Close()
		return errors.Join(err, fmt.Errorf("%w: checkpoint cleanup ledger changed identity", ErrStale))
	}
	if err := acquireJournalLock(file); err != nil {
		_ = file.Close()
		return nil // a live restore owns the exact ledger
	}
	record, err := decodeRestoreTempLedger(file)
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("reading checkpoint cleanup ledger %s: %w", path, err)
	}
	wantSuffix := strings.TrimPrefix(record.TempName, ".switchboard-undo-")
	if record.Version != 1 || record.Workspace != scope.path || record.WorkspaceIdentity != scope.identity ||
		!isRestoreTempName(record.TempName) || name != restoreTempLedgerPrefix+wantSuffix {
		_ = file.Close()
		return fmt.Errorf("%w: checkpoint cleanup ledger %s does not bind this workspace and temporary", ErrStale, path)
	}
	if _, err := scope.relative(record.Target); err != nil {
		_ = file.Close()
		return err
	}
	if err := reconcileRestoreTempRecord(scope, control.root, record); err != nil {
		_ = file.Close()
		return err
	}
	retireErr := retireBoundOpenFile(control.root, name, file, true, nil, nil)
	return errors.Join(retireErr, file.Close())
}

type restoreCleanupIdentity struct {
	value string
	owned bool
}

func reconcileRestoreTempRecord(scope *restoreScope, retirementRoot *os.Root, record restoreTempLedgerRecord) error {
	expected, err := record.Expected.decodeBound(maxStandaloneFilePublicationBytes)
	if err != nil {
		return err
	}
	desired, err := record.Desired.decodeBound(maxStandaloneFilePublicationBytes)
	if err != nil {
		return err
	}
	fingerprintLimit := expected.size
	if desired.size > fingerprintLimit {
		fingerprintLimit = desired.size
	}
	current, err := scope.fingerprint(record.Target, fingerprintLimit)
	if err != nil {
		return err
	}
	stateExpected := sameFingerprint(current, expected)
	stateDesired := sameFingerprint(current, desired)
	if stateExpected && stateDesired && current.existed && record.TempIdentity != "" {
		targetIdentity, identityErr := restoreTargetIdentity(scope, record.Target)
		if identityErr != nil {
			return identityErr
		}
		switch targetIdentity {
		case record.TempIdentity:
			stateExpected = false
		case record.DisplacedIdentity:
			stateDesired = false
		default:
			return fmt.Errorf("%w: equal checkpoint states have an unrecognised target inode", ErrDurableUndoRecoveryRequired)
		}
	}
	if !stateExpected && !stateDesired && !current.existed && expected.existed &&
		record.DisplacedIdentity != "" {
		// Windows' exact-handle exchange has a bounded intermediate state:
		// source is at the deterministic staging name, displaced target is at
		// TempName, and the target name is absent. Roll the displaced target back
		// with a no-clobber exact rename before cleaning the owned staging inode.
		if err := rollbackDisplacedRestoreTarget(scope, record); err != nil {
			return fmt.Errorf("%w: recovering interrupted checkpoint exchange: %v", ErrDurableUndoRecoveryRequired, err)
		}
		stateExpected = true
	}
	if !stateExpected && !stateDesired {
		return fmt.Errorf("%w: checkpoint cleanup target %s is neither its recorded pre- nor post-publication state", ErrDurableUndoRecoveryRequired, record.Target)
	}
	if stateDesired && desired.existed {
		if record.TempIdentity == "" {
			return fmt.Errorf("%w: checkpoint cleanup target %s was published without a bound temporary identity", ErrDurableUndoRecoveryRequired, record.Target)
		}
		identity, identityErr := restoreTargetIdentity(scope, record.Target)
		if identityErr != nil {
			return identityErr
		}
		if identity != record.TempIdentity {
			return fmt.Errorf("%w: checkpoint cleanup target %s is not the inode selected for publication", ErrDurableUndoRecoveryRequired, record.Target)
		}
	}

	var tempCandidates []restoreCleanupIdentity
	var stagingCandidates []restoreCleanupIdentity
	if stateExpected {
		tempCandidates = append(tempCandidates,
			restoreCleanupIdentity{value: record.TempIdentity, owned: record.TempOwned},
			restoreCleanupIdentity{value: record.DisplacedIdentity, owned: false})
		stagingCandidates = append(stagingCandidates,
			restoreCleanupIdentity{value: record.TempIdentity, owned: record.TempOwned})
	} else {
		// Once the selected temp inode is linked at the target, every remaining
		// name for that inode is merely an alias. Never scrub it through an alias
		// or the published target would be truncated too.
		tempCandidates = append(tempCandidates,
			restoreCleanupIdentity{value: record.DisplacedIdentity, owned: false},
			restoreCleanupIdentity{value: record.TempIdentity, owned: false})
		stagingCandidates = append(stagingCandidates,
			restoreCleanupIdentity{value: record.TempIdentity, owned: false})
	}
	if err := cleanupExactRestoreTemp(scope, retirementRoot, record.Target, record.TempName, tempCandidates...); err != nil {
		return err
	}
	targetRel, err := scope.relative(record.Target)
	if err != nil {
		return err
	}
	tempRel := filepath.Join(filepath.Dir(targetRel), record.TempName)
	stagingNames := []string{filepath.Base(restoreExchangeStagingName(tempRel, targetRel))}
	// Clean the two historical directional spellings as well. Every candidate
	// remains descriptor- and ledger-identity checked before disposition.
	for _, legacy := range []string{
		restoreStagingName(record.TempName),
		restoreStagingName(filepath.Base(targetRel)),
	} {
		seen := false
		for _, candidate := range stagingNames {
			seen = seen || candidate == legacy
		}
		if !seen {
			stagingNames = append(stagingNames, legacy)
		}
	}
	for _, stagingName := range stagingNames {
		if err := cleanupExactRestoreTemp(scope, retirementRoot, record.Target, stagingName, stagingCandidates...); err != nil {
			return err
		}
	}
	return nil
}

func restoreTargetIdentity(scope *restoreScope, target string) (string, error) {
	rel, err := scope.relative(target)
	if err != nil {
		return "", err
	}
	file, err := openCheckpointRootRead(scope.root, rel)
	if err != nil {
		return "", err
	}
	identity, identityErr := boundOpenFileIdentity(file)
	closeErr := file.Close()
	return identity, errors.Join(identityErr, closeErr)
}

func cleanupExactRestoreTemp(scope *restoreScope, retirementRoot *os.Root, target, name string, identities ...restoreCleanupIdentity) error {
	if err := cleanupExactWorkspaceTemp(scope, retirementRoot, target, name, identities...); err != nil {
		return err
	}
	if err := cleanupExactRetiredSink(scope, retirementRoot, target, name, identities...); err != nil {
		return err
	}
	return cleanupExactLocalRetired(scope, target, name, identities...)
}

func cleanupExactWorkspaceTemp(scope *restoreScope, retirementRoot *os.Root, target, name string, identities ...restoreCleanupIdentity) error {
	hasIdentity := false
	for _, identity := range identities {
		hasIdentity = hasIdentity || identity.value != ""
	}
	if !hasIdentity {
		// The ledger was durable before O_EXCL creation. Without a subsequently
		// synced inode identity, the exact name is only a reservation: another
		// process may have created it after the owner crashed. No pre-image bytes
		// are written until identity is recorded, so leaving it is fail-closed and
		// does not retain checkpoint content.
		return nil
	}
	rel, err := scope.relative(target)
	if err != nil {
		return err
	}
	tempRel := filepath.Join(filepath.Dir(rel), name)
	linked, err := scope.root.Lstat(tempRel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || !linked.Mode().IsRegular() {
		return errors.Join(err, fmt.Errorf("%w: recorded checkpoint temporary for %s is not a regular file", ErrStale, target))
	}
	file, err := scope.root.OpenFile(tempRel, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	opened, statErr := file.Stat()
	if statErr != nil || !os.SameFile(linked, opened) {
		_ = file.Close()
		return errors.Join(statErr, fmt.Errorf("%w: recorded checkpoint temporary for %s changed identity", ErrStale, target))
	}
	got, identityErr := boundOpenFileIdentity(file)
	owned, matched := false, false
	for _, identity := range identities {
		if identity.value != "" && got == identity.value {
			owned, matched = identity.owned, true
			break
		}
	}
	if identityErr != nil || !matched {
		_ = file.Close()
		return errors.Join(identityErr,
			fmt.Errorf("%w: recorded checkpoint temporary for %s has the wrong inode", ErrStale, target))
	}
	if lockErr := acquireRestoreLivenessLock(file); lockErr != nil {
		_ = file.Close()
		return fmt.Errorf("%w: recorded checkpoint temporary for %s is still live", ErrDurableUndoLocked, target)
	}
	var before func()
	if cleanupExactRestoreTempBeforeRetire != nil {
		before = func() { cleanupExactRestoreTempBeforeRetire(target, name) }
	}
	removeErr := retireBoundOpenFileTo(scope.root, retirementRoot, tempRel, file, owned, before, nil)
	closeErr := file.Close()
	if removeErr != nil || closeErr != nil {
		return errors.Join(removeErr, closeErr)
	}
	return nil
}

func cleanupExactRetiredSink(scope *restoreScope, root *os.Root, target, originalName string, identities ...restoreCleanupIdentity) error {
	if root == nil {
		return nil
	}
	rel, err := scope.relative(target)
	if err != nil {
		return err
	}
	tempRel := filepath.Join(filepath.Dir(rel), originalName)
	names := []string{retiredSinkName(tempRel)}
	// Version-1 cleanup ledgers written before workspace-root publication used
	// only the leaf when deriving the trusted-sink name. Accept that exact
	// legacy spelling as a second identity-checked candidate.
	if legacy := retiredSinkName(originalName); legacy != names[0] {
		names = append(names, legacy)
	}
	for _, name := range names {
		if err := cleanupExactRetiredSinkName(root, target, name, identities...); err != nil {
			return err
		}
	}
	return nil
}

func cleanupExactRetiredSinkName(root *os.Root, target, name string, identities ...restoreCleanupIdentity) error {
	linked, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || !linked.Mode().IsRegular() {
		return errors.Join(err, fmt.Errorf("%w: trusted retired checkpoint file for %s is not regular", ErrStale, target))
	}
	file, err := root.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	opened, statErr := file.Stat()
	if statErr != nil || !os.SameFile(linked, opened) {
		_ = file.Close()
		return errors.Join(statErr, fmt.Errorf("%w: trusted retired checkpoint file changed identity", ErrStale))
	}
	got, identityErr := boundOpenFileIdentity(file)
	owned, matched := false, false
	for _, identity := range identities {
		if identity.value != "" && got == identity.value {
			owned, matched = identity.owned, true
			break
		}
	}
	if identityErr != nil || !matched {
		_ = file.Close()
		return errors.Join(identityErr, fmt.Errorf("%w: trusted retired checkpoint file has the wrong inode", ErrStale))
	}
	if lockErr := acquireRestoreLivenessLock(file); lockErr != nil {
		_ = file.Close()
		return fmt.Errorf("%w: trusted retired checkpoint file is still live", ErrDurableUndoLocked)
	}
	// The session/control root is the trusted retirement sink. Workspace
	// writers never resolve this final unlink, and identity was checked through
	// the still-open descriptor immediately above.
	removeErr := removeTrustedRetiredFile(root, name, file, owned)
	closeErr := file.Close()
	if removeErr != nil || closeErr != nil {
		return errors.Join(removeErr, closeErr)
	}
	return nil
}

func cleanupExactLocalRetired(scope *restoreScope, target, originalName string, identities ...restoreCleanupIdentity) error {
	rel, err := scope.relative(target)
	if err != nil {
		return err
	}
	tempRel := filepath.Join(filepath.Dir(rel), originalName)
	names := []string{retiredSinkName(tempRel)}
	legacy := filepath.Join(filepath.Dir(rel), retiredSinkName(originalName))
	if legacy != names[0] {
		names = append(names, legacy)
	}
	for _, name := range names {
		if err := cleanupExactLocalRetiredName(scope, target, name, identities...); err != nil {
			return err
		}
	}
	return nil
}

func cleanupExactLocalRetiredName(scope *restoreScope, target, name string, identities ...restoreCleanupIdentity) error {
	linked, err := scope.root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || !linked.Mode().IsRegular() {
		return errors.Join(err, fmt.Errorf("%w: local retired checkpoint file for %s is not regular", ErrStale, target))
	}
	file, err := scope.root.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	opened, statErr := file.Stat()
	if statErr != nil || !os.SameFile(linked, opened) {
		return errors.Join(statErr, fmt.Errorf("%w: local retired checkpoint file changed identity", ErrStale))
	}
	got, identityErr := boundOpenFileIdentity(file)
	owned, matched := false, false
	for _, identity := range identities {
		if identity.value != "" && got == identity.value {
			owned, matched = identity.owned, true
			break
		}
	}
	if identityErr != nil || !matched {
		return errors.Join(identityErr, fmt.Errorf("%w: local retired checkpoint file has the wrong inode", ErrStale))
	}
	// Platform policy decides whether this exact local retirement link can be
	// removed. Windows has handle-bound disposition; POSIX retains an unowned
	// workspace inode because no portable unlink-by-descriptor primitive exists.
	return removeLocalRetiredFile(scope.root, name, file, owned)
}

func rollbackDisplacedRestoreTarget(scope *restoreScope, record restoreTempLedgerRecord) error {
	rel, err := scope.relative(record.Target)
	if err != nil {
		return err
	}
	tempRel := filepath.Join(filepath.Dir(rel), record.TempName)
	file, err := scope.root.OpenFile(tempRel, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	identity, err := boundOpenFileIdentity(file)
	if err != nil || identity != record.DisplacedIdentity {
		return errors.Join(err, fmt.Errorf("%w: displaced checkpoint target has the wrong inode", ErrStale))
	}
	if _, err := scope.root.Lstat(rel); !errors.Is(err, fs.ErrNotExist) {
		return errors.Join(err, fmt.Errorf("%w: checkpoint target appeared during exchange recovery", ErrStale))
	}
	result, err := renameBoundRestoreFile(scope.root, file, nil, tempRel, rel, false)
	if err != nil {
		return err
	}
	if !result.published {
		return errors.New("displaced checkpoint rollback did not publish")
	}
	parent, err := rootedfs.OpenRootAt(scope.root, filepath.Dir(rel))
	if err != nil {
		return err
	}
	defer parent.Close()
	return syncBoundDirectory(parent)
}

func cleanupRecordedRestoreTemps(scope *restoreScope, journal durableUndoJournal) error {
	if len(journal.Files) > maxPreparedUndoFiles {
		return fmt.Errorf("retry journal exceeds its temporary cleanup bound")
	}
	for _, entry := range journal.Files {
		if err := cleanupExactRestoreTemp(scope, nil, entry.Path, entry.TempName); err != nil {
			return err
		}
	}
	return scope.validateLinked()
}
