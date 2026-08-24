package session

// Candidate validation is deliberately stricter than ordinary read-only
// replay. Only an unterminated final frame is a recoverable tail; a complete
// corrupt frame refuses the candidate. Before --continue can recover that
// tail it first has to prove which session the prefix belongs to. A magic
// header by itself, or a start record copied under another id/workspace
// directory, is not an identity.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

type candidateExpectation struct {
	id        string
	workspace string
}

type validatedCandidate struct {
	start                SessionStart
	state                State
	fileInfo             os.FileInfo
	recoveredCorruptTail bool
	blockedByCorruption  bool
	blockedByReplayLimit bool
}

func (c validatedCandidate) blockedForInventory() bool {
	return c.blockedByCorruption || c.blockedByReplayLimit
}

type resolvedCandidate struct {
	path string
	validatedCandidate
}

// resolveCandidate is the single ID-to-log rule used by Open and every
// ID-based fork. Invalid matches cannot shadow a valid one through Glob order,
// and two independently valid matches are refused because an ID that names two
// histories has no safe implicit winner.
func (s *Store) resolveCandidate(id string) (resolvedCandidate, error) {
	if !validSessionID(id) {
		return resolvedCandidate{}, fmt.Errorf("invalid session id %q", id)
	}
	matches, err := s.candidatePaths(id, maxSessionWorkspaceDirectories)
	if err != nil {
		return resolvedCandidate{}, err
	}
	if len(matches) == 0 {
		return resolvedCandidate{}, fmt.Errorf("session %s not found", id)
	}
	var valid []resolvedCandidate
	var invalid []error
	for _, path := range matches {
		checked, err := s.validateCandidate(path, candidateExpectation{id: id})
		if err != nil {
			invalid = append(invalid, err)
			continue
		}
		valid = append(valid, resolvedCandidate{path: path, validatedCandidate: checked})
	}
	if len(valid) == 0 {
		return resolvedCandidate{}, fmt.Errorf("session %s has no valid log candidate: %w", id, errors.Join(invalid...))
	}
	if len(valid) > 1 {
		return resolvedCandidate{}, fmt.Errorf("session %s is ambiguous: %d valid log candidates", id, len(valid))
	}
	return valid[0], nil
}

// candidatePaths finds one exact leaf beneath each bounded workspace
// directory. filepath.Glob materializes and sorts unbounded directory
// inventories before returning, which lets a corrupt private store bypass the
// same cap used by List and ListAll.
func (s *Store) candidatePaths(id string, workspaceLimit int) ([]string, error) {
	dirs, err := readSessionDirectory(s.root, workspaceLimit)
	if err != nil {
		return nil, err
	}
	leaf := id + ".log"
	matches := make([]string, 0, 1)
	for _, entry := range dirs {
		if !entry.IsDir() {
			continue
		}
		dirPath := filepath.Join(s.root, entry.Name())
		root, err := rootedfs.OpenRoot(dirPath)
		if err != nil {
			continue
		}
		info, statErr := root.Lstat(leaf)
		closeErr := root.Close()
		if statErr != nil || closeErr != nil || !info.Mode().IsRegular() {
			continue
		}
		matches = append(matches, filepath.Join(dirPath, leaf))
	}
	return matches, nil
}

// validateCandidate opens a log read-only for List/Open discovery. Opening for
// append validates the same facts again after acquiring the file lock, closing
// the gap between choosing a candidate and making it writable.
func (s *Store) validateCandidate(path string, expect candidateExpectation) (validatedCandidate, error) {
	f, err := openSessionLog(path, false)
	if err != nil {
		return validatedCandidate{}, err
	}
	defer f.Close()
	return s.validateCandidateFile(f, path, expect)
}

func (s *Store) validateCandidateFile(f *os.File, path string, expect candidateExpectation) (checked validatedCandidate, resultErr error) {
	defer func() {
		if err := verifyCurrentSessionLogPath(f, path); err != nil {
			checked = validatedCandidate{}
			resultErr = errors.Join(resultErr, err)
			return
		}
		info, err := f.Stat()
		if err != nil {
			checked = validatedCandidate{}
			resultErr = errors.Join(resultErr, err)
			return
		}
		checked.fileInfo = info
	}()

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return validatedCandidate{}, err
	}
	r := bufio.NewReader(f)
	header, _, err := readSessionHeader(r, path)
	if err != nil {
		return validatedCandidate{}, err
	}

	lastSeq := 0
	budget := newReplayBudget(s.replayLimit, len(header))
	first, _, err := budget.decode(r, &lastSeq)
	switch {
	case errors.Is(err, io.EOF):
		return validatedCandidate{}, fmt.Errorf("%s has a session header but no session_start record", path)
	case errors.Is(err, ErrCorruptRecord):
		return validatedCandidate{}, fmt.Errorf("%s has a torn or corrupt first session_start record", path)
	case err != nil:
		return validatedCandidate{}, err
	}

	start, err := s.validateFirstStart(path, first, expect)
	if err != nil {
		return validatedCandidate{}, err
	}
	// Hidden children stop here. Do not parse, health-check, or surface any
	// later payload until the first record's publication capability has a
	// matching commit marker.
	if err := validatePublishedMarker(path, start); err != nil {
		return validatedCandidate{start: start}, err
	}
	replay := &Session{}
	if err := replay.apply(first); err != nil {
		return validatedCandidate{}, fmt.Errorf("%s has an invalid first session_start: %w", path, err)
	}

	for {
		rec, _, err := budget.decode(r, &lastSeq)
		if errors.Is(err, io.EOF) {
			return finishValidatedCandidate(path, start, replay.state, expect, false, false, false, nil)
		}
		if errors.Is(err, errTornFinalRecord) {
			// Once identity is established, writable replay owns recovery of
			// this provably incomplete final append. Discovery remains read-only.
			return finishValidatedCandidate(path, start, replay.state, expect, true, false, false, nil)
		}
		if errors.Is(err, ErrCorruptRecord) {
			// The identity and prefix are still useful to read-only inventory: a
			// preserved corrupt log must remain discoverable even though Open and
			// every full-history reader refuse it. Callers that require a valid
			// candidate continue to reject the non-nil error.
			return finishValidatedCandidate(path, start, replay.state, expect, false, true, false,
				fmt.Errorf("%s contains a complete corrupt record; original log preserved: %w", path, err))
		}
		if errors.Is(err, ErrSessionReplayTooLarge) {
			// The complete validated prefix remains useful to read-only Resume
			// Doctor inventory, but opening or projecting a partial transcript is
			// refused by the non-nil error.
			return finishValidatedCandidate(path, start, replay.state, expect, false, false, true,
				fmt.Errorf("%s exceeds the cumulative replay limit; original log preserved: %w", path, err))
		}
		if err != nil {
			return validatedCandidate{}, err
		}
		if rec.Type == RecordSessionStart {
			return validatedCandidate{}, fmt.Errorf("%s has a duplicate session_start at record %d", path, rec.Seq)
		}
		if err := replay.apply(rec); err != nil {
			return validatedCandidate{}, fmt.Errorf("%s record %d is invalid: %w", path, rec.Seq, err)
		}
	}
}

func finishValidatedCandidate(path string, start SessionStart, state State, expect candidateExpectation, recoveredTail, blocked, replayBlocked bool, replayErr error) (validatedCandidate, error) {
	checked := validatedCandidate{
		start:                start,
		state:                state,
		recoveredCorruptTail: recoveredTail,
		blockedByCorruption:  blocked,
		blockedByReplayLimit: replayBlocked,
	}
	// Publication is checked before returning even a corruption-blocked prefix.
	// A staged child therefore cannot leak into resume health merely because a
	// later record is corrupt.
	if err := validatePublishedMarker(path, start); err != nil {
		// List admits corruption-blocked published prefixes for Resume Doctor.
		// Clear that admission bit when the stronger publication gate fails.
		checked.blockedByCorruption = false
		checked.blockedByReplayLimit = false
		return checked, err
	}
	if err := validateWorkspaceExpectation(path, start.Workspace, state.WorkspaceBinding, expect.workspace); err != nil {
		checked.blockedByCorruption = false
		checked.blockedByReplayLimit = false
		return checked, err
	}
	checked.state.publicationPending = false
	return checked, replayErr
}

// validateFirstStart is shared with fork's streaming copy: both candidate
// selection and a live-source fork bind the first physical record to the same
// filename/workspace identity before trusting anything that follows it.
func (s *Store) validateFirstStart(path string, first Record, expect candidateExpectation) (SessionStart, error) {
	if first.Type != RecordSessionStart {
		return SessionStart{}, fmt.Errorf("%s first record is %s, want session_start", path, first.Type)
	}
	if first.Seq != 1 {
		return SessionStart{}, fmt.Errorf("%s first session_start has sequence %d, want 1", path, first.Seq)
	}
	var start SessionStart
	if err := json.Unmarshal(first.Payload, &start); err != nil {
		return SessionStart{}, fmt.Errorf("%s has an invalid first session_start: %w", path, err)
	}
	if err := s.validateStartPath(path, start, expect); err != nil {
		return SessionStart{}, err
	}
	return start, nil
}

func (s *Store) validateStartPath(path string, start SessionStart, expect candidateExpectation) error {
	if strings.TrimSpace(start.ID) == "" {
		return fmt.Errorf("%s session_start has no id", path)
	}
	if !validSessionID(start.ID) {
		return fmt.Errorf("%s session_start has invalid id %q", path, start.ID)
	}
	if strings.TrimSpace(start.Workspace) == "" {
		return fmt.Errorf("%s session_start has no workspace", path)
	}
	if start.Staged {
		if !validPublicationID(start.PublicationID) {
			return fmt.Errorf("%s staged session_start has an invalid publication id", path)
		}
	} else if start.PublicationID != "" {
		return fmt.Errorf("%s session_start has a publication id but is not staged", path)
	}
	absWorkspace, err := filepath.Abs(start.Workspace)
	if err != nil {
		return fmt.Errorf("%s session_start workspace: %w", path, err)
	}
	if !filepath.IsAbs(start.Workspace) || start.Workspace != absWorkspace {
		return fmt.Errorf("%s session_start workspace %q is not an absolute clean path", path, start.Workspace)
	}
	if expect.id != "" && start.ID != expect.id {
		return fmt.Errorf("%s session_start id %q does not match requested id %q", path, start.ID, expect.id)
	}
	wantPath := filepath.Join(s.root, workspaceKey(start.Workspace), start.ID+".log")
	gotPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	wantPath, err = filepath.Abs(wantPath)
	if err != nil {
		return err
	}
	if filepath.Clean(gotPath) != filepath.Clean(wantPath) {
		return fmt.Errorf("%s does not match session_start identity; want %s", gotPath, wantPath)
	}
	return nil
}

// cleanAbsoluteWorkspace preserves the session format's existing path rule:
// workspace identities are absolute, lexical-clean strings. Canonicalization
// is a separate, filesystem-backed assertion and must never be guessed from
// string normalization alone.
func cleanAbsoluteWorkspace(workspace string) (string, error) {
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// workspaceIdentityMatches accepts the exact historical boundary without
// touching the filesystem. A different spelling is accepted only when both
// paths currently name the same directory object. Missing and retargeted
// legacy aliases therefore cannot authorize a transcript in another runtime.
func workspaceIdentityMatches(recorded, requested string) bool {
	recorded, err := cleanAbsoluteWorkspace(recorded)
	if err != nil {
		return false
	}
	requested, err = cleanAbsoluteWorkspace(requested)
	if err != nil {
		return false
	}
	if recorded == requested {
		return true
	}
	recordedInfo, err := os.Stat(recorded)
	if err != nil || !recordedInfo.IsDir() {
		return false
	}
	requestedInfo, err := os.Stat(requested)
	if err != nil || !requestedInfo.IsDir() {
		return false
	}
	return os.SameFile(recordedInfo, requestedInfo)
}

func effectiveWorkspace(startWorkspace, binding string) string {
	if binding != "" {
		return binding
	}
	return startWorkspace
}

func validateWorkspaceExpectation(path, startWorkspace, binding, requested string) error {
	if requested == "" {
		return nil
	}
	requested, err := cleanAbsoluteWorkspace(requested)
	if err != nil {
		return err
	}
	effective := effectiveWorkspace(startWorkspace, binding)
	if workspaceIdentityMatches(effective, requested) {
		return nil
	}
	return fmt.Errorf("%s session workspace %q does not match requested workspace %q", path, effective, requested)
}
