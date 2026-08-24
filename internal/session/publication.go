package session

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	publicationMarkerVersion = 1
	publicationIDBytes       = 16
	maxPublicationMarker     = 512
)

type publicationStep uint8

const (
	publicationStepOpen publicationStep = iota + 1
	publicationStepPrefixWrite
	publicationStepPrefixSync
	publicationStepCommitWrite
	publicationStepCommitVisible
	publicationStepCommitSync
	publicationStepClose
	publicationStepDirectorySync
)

// PublicationOutcome separates discovery visibility from crash durability.
// Visible means callers must never roll a workspace transaction back: readers
// can already adopt the child. Durable means the marker file and its directory
// crossed every persistence boundary, so a retry journal may be retired.
type PublicationOutcome struct {
	Visible bool
	Durable bool
}

func newPublicationID() (string, error) {
	var token [publicationIDBytes]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", fmt.Errorf("creating session publication capability: %w", err)
	}
	return hex.EncodeToString(token[:]), nil
}

func validPublicationID(id string) bool {
	if len(id) != publicationIDBytes*2 {
		return false
	}
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == publicationIDBytes
}

func publicationMarkerPath(logPath string) string { return logPath + ".published" }

func publicationMarker(id, publicationID string) string {
	return fmt.Sprintf("switchboard-session-publish %d\n%s\n%s\n", publicationMarkerVersion, id, publicationID)
}

func publicationRecoveryIdentity(id, publicationID string) string {
	digest := sha256.Sum256([]byte(publicationMarker(id, publicationID)))
	return hex.EncodeToString(digest[:])
}

// PublicationRecoveryIdentity returns an opaque digest binding this creator
// handle to the staged child's exact session id and publication capability.
// It is intentionally unavailable on replayed, ordinary, or foreign handles.
func (s *Session) PublicationRecoveryIdentity() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.f == nil {
		return "", errors.New("cannot identify publication for a closed session")
	}
	if !s.state.publicationPending || s.publicationOwner == "" ||
		s.publicationOwner != s.state.publicationID || !validPublicationID(s.state.publicationID) {
		return "", ErrPublicationOwnership
	}
	return publicationRecoveryIdentity(s.state.ID, s.state.publicationID), nil
}

func readPublicationMarker(path string) ([]byte, error) {
	return readPublicationMarkerWithHook(path, nil)
}

// readPublicationMarkerWithHook is the single descriptor-stable sidecar read.
// The hook is nil in production and lets tests replace the pathname after the
// open without racing the scheduler.
func readPublicationMarkerWithHook(path string, afterOpen func()) ([]byte, error) {
	f, err := openSessionLog(path, false)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if afterOpen != nil {
		afterOpen()
	}
	data, err := readPublicationMarkerFile(f, path)
	if err != nil {
		return nil, err
	}
	if err := verifyCurrentSessionLogPath(f, path); err != nil {
		return nil, fmt.Errorf("publication marker changed while it was read: %w", err)
	}
	return data, nil
}

func readPublicationMarkerFile(f *os.File, path string) ([]byte, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxPublicationMarker {
		return nil, fmt.Errorf("publication marker %s exceeds its %d-byte limit", path, maxPublicationMarker)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxPublicationMarker+1))
	if err != nil {
		return nil, fmt.Errorf("reading publication marker %s: %w", path, err)
	}
	if len(data) > maxPublicationMarker {
		return nil, fmt.Errorf("publication marker %s exceeds its %d-byte limit", path, maxPublicationMarker)
	}
	return data, nil
}

func validatePublishedMarker(path string, start SessionStart) error {
	if !start.Staged {
		if start.PublicationID != "" {
			return fmt.Errorf("%s has a publication id without staged state", path)
		}
		return nil
	}
	if !validPublicationID(start.PublicationID) {
		return fmt.Errorf("%w: %s has an invalid publication id", ErrSessionUnpublished, path)
	}

	markerPath := publicationMarkerPath(path)
	data, err := readPublicationMarker(markerPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrSessionUnpublished, path)
		}
		return fmt.Errorf("checking publication marker for %s: %w", path, err)
	}
	if string(data) != publicationMarker(start.ID, start.PublicationID) {
		return fmt.Errorf("%w: publication marker does not own %s", ErrSessionUnpublished, path)
	}
	return nil
}

func readFirstSessionStart(f *os.File, path string) (SessionStart, error) {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return SessionStart{}, err
	}
	r := bufio.NewReader(f)
	if err := checkHeader(r, path); err != nil {
		return SessionStart{}, err
	}
	lastSeq := 0
	first, _, err := decodeSequencedRecord(r, &lastSeq)
	if err != nil {
		return SessionStart{}, fmt.Errorf("%s has no valid first session_start: %w", path, err)
	}
	if first.Type != RecordSessionStart || first.Seq != 1 {
		return SessionStart{}, fmt.Errorf("%s first record is %s sequence %d, want session_start sequence 1", path, first.Type, first.Seq)
	}
	var start SessionStart
	if err := json.Unmarshal(first.Payload, &start); err != nil {
		return SessionStart{}, fmt.Errorf("%s has an invalid first session_start: %w", path, err)
	}
	if start.ID == "" || start.Workspace == "" {
		return SessionStart{}, fmt.Errorf("%s first session_start has no identity", path)
	}
	if !validSessionID(start.ID) {
		return SessionStart{}, fmt.Errorf("%s first session_start has invalid id %q", path, start.ID)
	}
	if start.Staged && !validPublicationID(start.PublicationID) {
		return SessionStart{}, fmt.Errorf("%w: %s has an invalid publication id", ErrSessionUnpublished, path)
	}
	if !start.Staged && start.PublicationID != "" {
		return SessionStart{}, fmt.Errorf("%s has a publication id without staged state", path)
	}
	return start, nil
}

// PublicationStatus is the read-only crash-recovery boundary for a journal
// that already holds an exact child path. It deliberately reads only the log
// header and first session_start, then the bounded sidecar: later conversation
// corruption does not change whether the publication commit happened.
//
// A regular log or exact staged marker returns true. An absent marker or an
// owned prefix left before the final commit byte returns false with no error.
// Malformed identity and foreign/mismatched markers are errors, never guessed
// into either state.
func PublicationStatus(path string) (bool, error) {
	return publicationStatus(path, "")
}

// PublicationStatusExpected is PublicationStatus with an exact staged-child
// identity check. Recovery journals use it so replacing a child path with a
// different valid session can never decide whether workspace files commit.
func PublicationStatusExpected(path, expectedIdentity string) (bool, error) {
	digest, err := hex.DecodeString(expectedIdentity)
	if err != nil || len(digest) != sha256.Size {
		return false, errors.New("retry publication identity is not a SHA-256 digest")
	}
	return publicationStatus(path, expectedIdentity)
}

func publicationStatus(path, expectedIdentity string) (bool, error) {
	f, err := openSessionLog(path, false)
	if err != nil {
		return false, err
	}
	defer f.Close()
	start, err := readFirstSessionStart(f, path)
	if err != nil {
		return false, err
	}
	if err := validatePublicationStatusStart(path, start, expectedIdentity); err != nil {
		return false, err
	}
	if !start.Staged {
		if err := verifyCurrentSessionLogPath(f, path); err != nil {
			return false, err
		}
		return true, nil
	}

	markerPath := publicationMarkerPath(path)
	data, err := readPublicationMarker(markerPath)
	if errors.Is(err, os.ErrNotExist) {
		if identityErr := verifyCurrentSessionLogPath(f, path); identityErr != nil {
			return false, identityErr
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := verifyCurrentSessionLogPath(f, path); err != nil {
		return false, err
	}
	expected := []byte(publicationMarker(start.ID, start.PublicationID))
	if bytes.Equal(data, expected) {
		return true, nil
	}
	if bytes.HasPrefix(expected, data) {
		return false, nil
	}
	return false, fmt.Errorf("publication marker does not own %s", path)
}

func validatePublicationStatusStart(path string, start SessionStart, expectedIdentity string) error {
	if err := validateSessionStartPathIdentity(path, start); err != nil {
		return err
	}
	if expectedIdentity != "" {
		if !start.Staged {
			return fmt.Errorf("%s is not the staged retry child recorded by the recovery journal", path)
		}
		actual := publicationRecoveryIdentity(start.ID, start.PublicationID)
		if subtle.ConstantTimeCompare([]byte(actual), []byte(expectedIdentity)) != 1 {
			return fmt.Errorf("%s does not match the retry journal's publication identity", path)
		}
	}
	return nil
}

// validateSessionStartPathIdentity binds a parsed identity to the pathname that
// selected it. Candidate discovery performs the stronger store-root check; this
// local form is shared by recovery and read-only path consumers so a valid log
// substituted after discovery cannot be presented as the selected session.
func validateSessionStartPathIdentity(path string, start SessionStart) error {
	workspace, err := filepath.Abs(start.Workspace)
	if err != nil || !filepath.IsAbs(start.Workspace) || workspace != start.Workspace {
		return fmt.Errorf("%s has an invalid session_start workspace %q", path, start.Workspace)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if filepath.Base(absPath) != start.ID+".log" || filepath.Base(filepath.Dir(absPath)) != workspaceKey(start.Workspace) {
		return fmt.Errorf("%s does not match session_start identity", path)
	}
	return nil
}

// EnsurePublicationDurableExpected is the ownerless restart half of
// PublishDurably. A journal may retire only after the exact visible marker is
// flushed and its directory entry is synced; merely observing page-cache bytes
// after a process crash does not prove they will survive a later power loss.
// Missing or owned-partial markers remain unpublished and return false.
func EnsurePublicationDurableExpected(path, expectedIdentity string) (bool, error) {
	return ensurePublicationDurableExpected(path, expectedIdentity,
		func(f *os.File) error { return f.Sync() }, syncOpenedSessionDirectory)
}

func ensurePublicationDurableExpected(path, expectedIdentity string, syncMarker, syncDirectory func(*os.File) error) (bool, error) {
	return ensurePublicationDurableExpectedWithHooks(path, expectedIdentity, syncMarker, syncDirectory, nil, nil)
}

func ensurePublicationDurableExpectedWithHooks(path, expectedIdentity string, syncMarker, syncDirectory func(*os.File) error, beforeMarkerOpen, afterMarkerOpen func()) (bool, error) {
	if syncMarker == nil || syncDirectory == nil {
		return false, errors.New("publication durability recovery has no persistence barrier")
	}
	digest, err := hex.DecodeString(expectedIdentity)
	if err != nil || len(digest) != sha256.Size {
		return false, errors.New("retry publication identity is not a SHA-256 digest")
	}
	child, err := openSessionLog(path, false)
	if err != nil {
		return false, err
	}
	defer child.Close()
	logStamp, err := captureStablePublicationLogStamp(child, path)
	if err != nil {
		return false, err
	}
	start, err := readFirstSessionStart(child, path)
	if err != nil {
		return false, err
	}
	if err := validatePublicationStatusStart(path, start, expectedIdentity); err != nil {
		return false, err
	}

	markerPath := publicationMarkerPath(path)
	directoryPath := filepath.Dir(markerPath)
	root, directory, directoryInfo, err := openBoundSessionDirectory(directoryPath, nil)
	if err != nil {
		return false, fmt.Errorf("binding recovered session publication directory: %w", err)
	}
	defer directory.Close()
	defer root.Close()

	markerName := filepath.Base(markerPath)
	if beforeMarkerOpen != nil {
		beforeMarkerOpen()
	}
	marker, err := openRootedPublicationMarker(root, markerName)
	if afterMarkerOpen != nil {
		afterMarkerOpen()
	}
	if errors.Is(err, os.ErrNotExist) {
		if identityErr := verifyCurrentSessionLogPath(child, path); identityErr != nil {
			return false, identityErr
		}
		if identityErr := verifyBoundSessionDirectory(root, directory, directoryInfo, directoryPath); identityErr != nil {
			return false, identityErr
		}
		if _, identityErr := root.Lstat(markerName); !errors.Is(identityErr, os.ErrNotExist) {
			return false, errors.Join(
				fmt.Errorf("recovered session publication marker %s changed while its absence was verified", markerName),
				identityErr)
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	markerClosed := false
	defer func() {
		if !markerClosed {
			_ = marker.Close()
		}
	}()
	data, err := readPublicationMarkerFile(marker, markerPath)
	if err != nil {
		return false, err
	}
	expected := []byte(publicationMarker(start.ID, start.PublicationID))
	if !bytes.Equal(data, expected) {
		if bytes.HasPrefix(expected, data) {
			if err := verifyPublishedSessionOpenPaths(child, path, root, directory, directoryInfo, marker, markerName); err != nil {
				return false, err
			}
			return false, nil
		}
		return false, fmt.Errorf("publication marker does not own %s", path)
	}
	stamp, err := capturePublishedSessionCommit(child, path, root, directory, directoryInfo, marker, markerName, markerPath, expected, logStamp)
	if err != nil {
		return false, err
	}
	if err := syncMarker(marker); err != nil {
		return false, fmt.Errorf("syncing recovered session publication marker: %w", err)
	}
	if err := verifyPublishedSessionCommitStamp(child, path, root, directory, directoryInfo, marker, markerName, markerPath, expected, stamp); err != nil {
		return false, err
	}
	directoryStamp, err := captureBoundSessionDirectoryStamp(root, directory, directoryInfo, directoryPath)
	if err != nil {
		return false, err
	}
	if err := syncDirectory(directory); err != nil {
		return false, fmt.Errorf("syncing recovered session publication directory: %w", err)
	}
	if err := verifyPublishedSessionCommitStamp(child, path, root, directory, directoryInfo, marker, markerName, markerPath, expected, stamp); err != nil {
		return false, err
	}
	if err := verifyBoundSessionDirectoryStamp(root, directory, directoryInfo, directoryPath, directoryStamp); err != nil {
		return false, err
	}
	if err := marker.Close(); err != nil {
		return false, fmt.Errorf("closing recovered session publication marker: %w", err)
	}
	markerClosed = true
	return true, nil
}

// PublicationPending reports whether this live creator handle still needs an
// adoption commit. Replayed published sessions return false; callers must not
// use this as authority to publish, which remains Publish's capability check.
func (s *Session) PublicationPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.publicationPending
}

// Publish preserves the historical visibility-only adoption contract for
// compatibility. New production callers must use PublishDurably: this method
// intentionally masks a visible marker whose persistence barrier failed, so it
// cannot tell a caller whether continuing work is safe.
func (s *Session) Publish() error {
	outcome, err := s.PublishDurably()
	if outcome.Visible {
		return nil
	}
	return err
}

// PublishDurably returns three disjoint outcomes. An error with Visible false
// means publication did not commit and a caller may roll back. Visible true
// with Durable false means it must not roll back but must retain recovery state.
// Visible and Durable true permits recovery-state retirement.
func (s *Session) PublishDurably() (outcome PublicationOutcome, resultErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.f == nil {
		return PublicationOutcome{}, errors.New("cannot publish a closed session")
	}
	if !s.state.publicationPending {
		if s.state.publicationID == "" {
			return PublicationOutcome{Visible: true, Durable: true}, nil // ordinary session
		}
		if s.publicationCommitted && s.publicationOwner == s.state.publicationID {
			if s.publicationDurable {
				return PublicationOutcome{Visible: true, Durable: true}, nil
			}
			start := SessionStart{ID: s.state.ID, Staged: true, PublicationID: s.state.publicationID}
			return s.ensureVisiblePublicationDurableLocked(start)
		}
		return PublicationOutcome{}, ErrPublicationOwnership
	}
	if s.publicationOwner == "" || s.publicationOwner != s.state.publicationID {
		return PublicationOutcome{}, ErrPublicationOwnership
	}
	if s.poisoned != nil {
		return PublicationOutcome{}, fmt.Errorf("%w: %v", ErrSessionPoisoned, s.poisoned)
	}
	if err := verifyCurrentSessionLogPath(s.f, s.path); err != nil {
		return PublicationOutcome{}, fmt.Errorf("publishing session through its creator log: %w", err)
	}
	if !s.publicationLogStamped {
		stamp, err := captureStablePublicationLogStamp(s.f, s.path)
		if err != nil {
			return PublicationOutcome{}, fmt.Errorf("binding session log before publication: %w", err)
		}
		s.publicationLogStamp = stamp
		s.publicationLogStamped = true
	} else if err := verifyPublicationLogStamp(s.f, s.path, s.publicationLogStamp); err != nil {
		return PublicationOutcome{}, fmt.Errorf("session log changed after publication began: %w", err)
	}

	start := SessionStart{ID: s.state.ID, Staged: true, PublicationID: s.state.publicationID}
	markerPath := publicationMarkerPath(s.path)
	marker := publicationMarker(s.state.ID, s.state.publicationID)
	expectedMarker := []byte(marker)
	prefix := expectedMarker[:len(expectedMarker)-1]
	if err := s.failPublicationStep(publicationStepOpen); err != nil {
		return PublicationOutcome{}, fmt.Errorf("creating session publication marker: %w", err)
	}
	var (
		f   *os.File
		err error
	)
	resumePartial := s.publicationMarkerInfo != nil
	if !resumePartial {
		f, err = createPrivateSessionFile(markerPath)
		if err != nil {
			if s.publicationCommitValidLocked(start) {
				s.markPublicationCommittedLocked(false)
				return s.ensureVisiblePublicationDurableLocked(start)
			}
			return PublicationOutcome{}, fmt.Errorf("publishing session %s without replacing an existing marker: %w", s.state.ID, err)
		}
		createdInfo, statErr := f.Stat()
		if statErr != nil {
			return s.failPublicationLocked(f, markerPath, start,
				fmt.Errorf("binding created session publication marker: %w", statErr))
		}
		s.publicationMarkerInfo = createdInfo
	}
	directoryPath := filepath.Dir(markerPath)
	root, directory, directoryInfo, err := openBoundSessionDirectory(directoryPath, nil)
	if err != nil {
		return s.failPublicationLocked(f, markerPath, start,
			fmt.Errorf("binding session publication directory: %w", err))
	}
	defer func() {
		resultErr = errors.Join(resultErr, directory.Close(), root.Close())
	}()
	markerName := filepath.Base(markerPath)
	written := 0
	if resumePartial {
		f, err = openRootedPublicationMarker(root, markerName)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return s.failPublicationLocked(nil, markerPath, start,
					fmt.Errorf("reopening retained session publication marker: %w", err))
			}
			// An unlink observed through the retained directory capability cannot
			// be repaired by deleting anything. The live creator still owns the
			// append-locked log and its original mutation fingerprint, so it may
			// replace only stable absence with a fresh exclusive marker.
			if err := verifyPublicationLogStamp(s.f, s.path, s.publicationLogStamp); err != nil {
				return s.failPublicationLocked(nil, markerPath, start,
					fmt.Errorf("session log changed beside an absent retained publication marker: %w", err))
			}
			if err := verifyBoundSessionDirectory(root, directory, directoryInfo, directoryPath); err != nil {
				return s.failPublicationLocked(nil, markerPath, start,
					fmt.Errorf("session publication directory changed beside an absent retained marker: %w", err))
			}
			if _, statErr := root.Lstat(markerName); !errors.Is(statErr, os.ErrNotExist) {
				return s.failPublicationLocked(nil, markerPath, start,
					errors.Join(errors.New("retained session publication marker changed while its absence was verified"), statErr))
			}
			f, err = createPrivateSessionFileInRoot(root, markerName)
			if err != nil {
				return s.failPublicationLocked(nil, markerPath, start,
					fmt.Errorf("recreating absent retained session publication marker: %w", err))
			}
			createdInfo, statErr := f.Stat()
			if statErr != nil {
				return s.failPublicationLocked(f, markerPath, start,
					fmt.Errorf("binding recreated session publication marker: %w", statErr))
			}
			s.publicationMarkerInfo = createdInfo
			resumePartial = false
		}
		if resumePartial {
			openedInfo, statErr := f.Stat()
			if statErr != nil || !os.SameFile(s.publicationMarkerInfo, openedInfo) {
				return s.failPublicationLocked(f, markerPath, start,
					errors.Join(errors.New("retained session publication marker changed identity"), statErr))
			}
			data, readErr := readPublicationMarkerFile(f, markerPath)
			if readErr != nil {
				return s.failPublicationLocked(f, markerPath, start,
					fmt.Errorf("reading retained session publication marker: %w", readErr))
			}
			if len(data) >= len(expectedMarker) || !bytes.HasPrefix(expectedMarker, data) {
				return s.failPublicationLocked(f, markerPath, start,
					errors.New("retained session publication marker is not the strict prefix created by this publisher"))
			}
			written = len(data)
		}
	}
	if err := verifyPublishedSessionOpenPaths(s.f, s.path, root, directory, directoryInfo, f, markerName); err != nil {
		return s.failPublicationLocked(f, markerPath, start,
			fmt.Errorf("binding session publication marker: %w", err))
	}
	if written < len(prefix) {
		if _, err := f.Seek(int64(written), io.SeekStart); err != nil {
			return s.failPublicationLocked(f, markerPath, start, fmt.Errorf("seeking retained session publication prefix: %w", err))
		}
		if err := s.failPublicationStep(publicationStepPrefixWrite); err != nil {
			return s.failPublicationLocked(f, markerPath, start, fmt.Errorf("writing session publication prefix: %w", err))
		}
		if err := writePublicationPart(f, prefix[written:]); err != nil {
			return s.failPublicationLocked(f, markerPath, start, fmt.Errorf("writing session publication prefix: %w", err))
		}
	}
	if err := s.failPublicationStep(publicationStepPrefixSync); err != nil {
		return s.failPublicationLocked(f, markerPath, start, fmt.Errorf("syncing session publication prefix: %w", err))
	}
	if err := f.Sync(); err != nil {
		return s.failPublicationLocked(f, markerPath, start, fmt.Errorf("syncing session publication prefix: %w", err))
	}
	if err := s.failPublicationStep(publicationStepCommitWrite); err != nil {
		return s.failPublicationLocked(f, markerPath, start, fmt.Errorf("writing session publication commit: %w", err))
	}
	if err := verifyPublishedSessionOpenPaths(s.f, s.path, root, directory, directoryInfo, f, markerName); err != nil {
		return s.failPublicationLocked(f, markerPath, start, fmt.Errorf("session log changed before publication commit: %w", err))
	}
	if err := writePublicationPart(f, []byte{'\n'}); err != nil {
		return s.failPublicationLocked(f, markerPath, start, fmt.Errorf("writing session publication commit: %w", err))
	}
	if err := s.failPublicationStep(publicationStepCommitVisible); err != nil {
		return s.failPublicationLocked(f, markerPath, start, fmt.Errorf("publication commit became visible: %w", err))
	}
	stamp, err := capturePublishedSessionCommit(s.f, s.path, root, directory, directoryInfo, f, markerName, markerPath, expectedMarker, s.publicationLogStamp)
	if err != nil {
		return s.failPublicationLocked(f, markerPath, start, fmt.Errorf("session log changed as publication committed: %w", err))
	}
	if err := s.failPublicationStep(publicationStepCommitSync); err != nil {
		return s.failPublicationLocked(f, markerPath, start, fmt.Errorf("syncing session publication commit: %w", err))
	}
	if err := f.Sync(); err != nil {
		return s.failPublicationLocked(f, markerPath, start, fmt.Errorf("syncing session publication commit: %w", err))
	}
	if err := verifyPublishedSessionCommitStamp(s.f, s.path, root, directory, directoryInfo, f, markerName, markerPath, expectedMarker, stamp); err != nil {
		return s.failPublicationLocked(f, markerPath, start,
			fmt.Errorf("session publication changed after its marker was synced: %w", err))
	}
	if err := s.failPublicationStep(publicationStepDirectorySync); err != nil {
		return s.failPublicationLocked(f, markerPath, start, fmt.Errorf("syncing session publication directory: %w", err))
	}
	if err := verifyPublishedSessionCommitStamp(s.f, s.path, root, directory, directoryInfo, f, markerName, markerPath, expectedMarker, stamp); err != nil {
		return s.failPublicationLocked(f, markerPath, start,
			fmt.Errorf("session publication changed before its directory was synced: %w", err))
	}
	directoryStamp, err := captureBoundSessionDirectoryStamp(root, directory, directoryInfo, directoryPath)
	if err != nil {
		return s.failPublicationLocked(f, markerPath, start,
			fmt.Errorf("capturing session publication directory before sync: %w", err))
	}
	if err := syncOpenedSessionDirectory(directory); err != nil {
		return s.failPublicationLocked(f, markerPath, start, fmt.Errorf("syncing session publication directory: %w", err))
	}
	if err := verifyPublishedSessionCommitStamp(s.f, s.path, root, directory, directoryInfo, f, markerName, markerPath, expectedMarker, stamp); err != nil {
		return s.failPublicationLocked(f, markerPath, start,
			fmt.Errorf("session publication changed after its directory was synced: %w", err))
	}
	if err := verifyBoundSessionDirectoryStamp(root, directory, directoryInfo, directoryPath, directoryStamp); err != nil {
		return s.failPublicationLocked(f, markerPath, start,
			fmt.Errorf("session publication directory changed across its sync: %w", err))
	}
	if err := s.failPublicationStep(publicationStepClose); err != nil {
		return s.failPublicationLocked(f, markerPath, start, fmt.Errorf("closing session publication marker: %w", err))
	}
	if err := verifyPublishedSessionCommitStamp(s.f, s.path, root, directory, directoryInfo, f, markerName, markerPath, expectedMarker, stamp); err != nil {
		return s.failPublicationLocked(f, markerPath, start,
			fmt.Errorf("session publication changed before publication completed: %w", err))
	}
	if err := verifyBoundSessionDirectoryStamp(root, directory, directoryInfo, directoryPath, directoryStamp); err != nil {
		return s.failPublicationLocked(f, markerPath, start,
			fmt.Errorf("session publication directory changed before publication completed: %w", err))
	}
	if err := f.Close(); err != nil {
		return s.failPublicationLocked(nil, markerPath, start, fmt.Errorf("closing session publication marker: %w", err))
	}
	s.markPublicationCommittedLocked(true)
	return PublicationOutcome{Visible: true, Durable: true}, nil
}

func (s *Session) failPublicationStep(step publicationStep) error {
	if s.publicationFault == nil {
		return nil
	}
	return s.publicationFault(step)
}

func writePublicationPart(f *os.File, data []byte) error {
	n, err := f.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return err
}

func (s *Session) markPublicationCommittedLocked(durable bool) {
	s.state.publicationPending = false
	s.publicationCommitted = true
	s.publicationDurable = durable
}

func (s *Session) publicationCommitValidLocked(start SessionStart) bool {
	return verifyCurrentSessionLogPath(s.f, s.path) == nil && validatePublishedMarker(s.path, start) == nil
}

// failPublicationLocked reports visibility independently of the durability
// failure that led here. A valid marker is irrevocably committed from the
// caller's perspective, but its journal remains necessary until a later sync
// proves the commit can survive power loss. Failure cleanup deliberately does
// not remove an absent or partial marker: a marker and log are separate names,
// so retaining invisible evidence is safer than trying to delete both across
// a commit race. Age-gated maintenance owns eventual cleanup.
func (s *Session) failPublicationLocked(f *os.File, markerPath string, start SessionStart, cause error) (PublicationOutcome, error) {
	if s.publicationCommitValidLocked(start) {
		var closeErr error
		if f != nil {
			closeErr = f.Close()
		}
		s.markPublicationCommittedLocked(false)
		return PublicationOutcome{Visible: true}, errors.Join(cause, closeErr)
	}
	if s.publicationFailCheck != nil {
		s.publicationFailCheck(markerPath)
	}
	var cleanupErr error
	if f != nil {
		cleanupErr = errors.Join(cleanupErr, f.Close())
	}
	_, markerExists, markerOwned, markerErr := inspectOwnedPublicationMarker(s.path, start, false)
	cleanupErr = errors.Join(cleanupErr, markerErr)
	if markerExists && !markerOwned && markerErr == nil && validatePublishedMarker(s.path, start) != nil {
		cleanupErr = errors.Join(cleanupErr, errors.New("publication marker changed ownership during cleanup"))
	}
	if s.publicationCommitValidLocked(start) {
		s.markPublicationCommittedLocked(false)
		return PublicationOutcome{Visible: true}, errors.Join(cause, cleanupErr)
	}
	return PublicationOutcome{}, errors.Join(cause, cleanupErr)
}

// ensureVisiblePublicationDurableLocked replays only persistence barriers; it
// never rewrites marker bytes or accepts a different path identity. This lets a
// creator retry an uncertain visible commit without creating a second marker.
func (s *Session) ensureVisiblePublicationDurableLocked(start SessionStart) (PublicationOutcome, error) {
	return s.ensureVisiblePublicationDurableLockedWith(start,
		func(f *os.File) error { return f.Sync() }, syncOpenedSessionDirectory)
}

func (s *Session) ensureVisiblePublicationDurableLockedWith(start SessionStart, syncMarker, syncDirectory func(*os.File) error) (PublicationOutcome, error) {
	visible := PublicationOutcome{Visible: true}
	if syncMarker == nil || syncDirectory == nil {
		return visible, errors.New("visible session publication durability retry has no persistence barrier")
	}
	if !s.publicationCommitValidLocked(start) {
		return visible, errors.New("visible session publication no longer has its exact marker")
	}
	markerPath := publicationMarkerPath(s.path)
	directoryPath := filepath.Dir(markerPath)
	root, directory, directoryInfo, err := openBoundSessionDirectory(directoryPath, nil)
	if err != nil {
		return visible, fmt.Errorf("binding visible session publication directory: %w", err)
	}
	defer directory.Close()
	defer root.Close()
	markerName := filepath.Base(markerPath)
	f, err := openRootedPublicationMarker(root, markerName)
	if err != nil {
		return visible, fmt.Errorf("reopening visible session publication marker: %w", err)
	}
	markerClosed := false
	defer func() {
		if !markerClosed {
			_ = f.Close()
		}
	}()
	data, err := readPublicationMarkerFile(f, markerPath)
	if err != nil {
		return visible, fmt.Errorf("reading visible session publication marker: %w", err)
	}
	if !bytes.Equal(data, []byte(publicationMarker(start.ID, start.PublicationID))) {
		return visible, errors.New("visible session publication no longer has its exact marker")
	}
	expected := []byte(publicationMarker(start.ID, start.PublicationID))
	if !s.publicationLogStamped {
		return visible, errors.New("visible session publication has no validated child fingerprint")
	}
	stamp, err := capturePublishedSessionCommit(s.f, s.path, root, directory, directoryInfo, f, markerName, markerPath, expected, s.publicationLogStamp)
	if err != nil {
		return visible, err
	}
	if err := syncMarker(f); err != nil {
		return visible, fmt.Errorf("syncing visible session publication marker: %w", err)
	}
	if err := verifyPublishedSessionCommitStamp(s.f, s.path, root, directory, directoryInfo, f, markerName, markerPath, expected, stamp); err != nil {
		return visible, fmt.Errorf("visible session publication changed after its marker was synced: %w", err)
	}
	directoryStamp, err := captureBoundSessionDirectoryStamp(root, directory, directoryInfo, directoryPath)
	if err != nil {
		return visible, fmt.Errorf("capturing visible session publication directory before sync: %w", err)
	}
	if err := syncDirectory(directory); err != nil {
		return visible, fmt.Errorf("syncing visible session publication directory: %w", err)
	}
	if err := verifyPublishedSessionCommitStamp(s.f, s.path, root, directory, directoryInfo, f, markerName, markerPath, expected, stamp); err != nil {
		return visible, fmt.Errorf("visible session publication changed after its directory was synced: %w", err)
	}
	if err := verifyBoundSessionDirectoryStamp(root, directory, directoryInfo, directoryPath, directoryStamp); err != nil {
		return visible, fmt.Errorf("visible session publication directory changed across its sync: %w", err)
	}
	if err := f.Close(); err != nil {
		return visible, fmt.Errorf("closing visible session publication marker: %w", err)
	}
	markerClosed = true
	s.markPublicationCommittedLocked(true)
	return PublicationOutcome{Visible: true, Durable: true}, nil
}

// CloseDiscardingStaged closes an unpublished creator without deleting its
// log. A marker commit and a log removal are distinct directory entries, so no
// finite inspect-then-remove sequence can bind both names. The invisible
// artifact is instead left for the store's age-gated maintenance pass, whose
// narrower authority is an abandoned private-store log held under the
// cross-process append lock.
func (s *Session) CloseDiscardingStaged() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.state.publicationPending {
		return s.closeLocked()
	}
	if s.publicationOwner == "" || s.publicationOwner != s.state.publicationID {
		closeErr := s.closeLocked()
		return errors.Join(ErrPublicationOwnership, closeErr)
	}
	path := s.path
	if err := verifyCurrentSessionLogPath(s.f, path); err != nil {
		return errors.Join(fmt.Errorf("refusing to discard a replaced staged session: %w", err), s.closeLocked())
	}
	start := SessionStart{ID: s.state.ID, Staged: true, PublicationID: s.state.publicationID}
	// A complete exact marker is the adoption commit even if an interrupted
	// caller has not yet folded that fact into memory. Never turn a committed
	// session back into a ghost during cleanup.
	if validatePublishedMarker(path, start) == nil {
		s.markPublicationCommittedLocked(false)
		return s.closeLocked()
	}
	if s.discardBeforeInspect != nil {
		s.discardBeforeInspect(path)
	}
	if s.discardBeforeClose != nil {
		s.discardBeforeClose(path)
	}
	if err := verifyCurrentSessionLogPath(s.f, path); err != nil {
		return errors.Join(fmt.Errorf("refusing to discard a replaced staged session: %w", err), s.closeLocked())
	}
	_, markerExists, markerOwned, markerCommitted, markerErr := inspectDiscardPublicationMarker(path, start)
	if markerCommitted {
		s.markPublicationCommittedLocked(false)
		return s.closeLocked()
	}
	if markerErr != nil || markerExists && !markerOwned {
		return errors.Join(markerErr, s.closeLocked())
	}
	return s.closeLocked()
}

// inspectDiscardPublicationMarker distinguishes a completed adoption commit
// from a partial marker this creator may leave for maintenance. Any other
// existing marker is deliberately non-discardable: deleting the log beside it
// could orphan a commit that raced the first visibility check.
func inspectDiscardPublicationMarker(path string, start SessionStart) (info os.FileInfo, exists, owned, committed bool, err error) {
	if validatePublishedMarker(path, start) == nil {
		return nil, true, false, true, nil
	}
	info, exists, owned, err = inspectOwnedPublicationMarker(path, start, false)
	if err != nil || !exists || owned {
		return info, exists, owned, false, err
	}
	// The marker may have completed between the first bounded read and the
	// ownership inspection. One final exact read recognizes that commit;
	// otherwise preserving both files is the only safe outcome.
	if validatePublishedMarker(path, start) == nil {
		return info, true, false, true, nil
	}
	return info, true, false, false,
		fmt.Errorf("refusing to discard session %s beside a non-owned publication marker", start.ID)
}
