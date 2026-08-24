package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// A staged log younger than this may belong to a long-lived operation or
	// be useful crash evidence. Maintenance favors leaving debris over deleting
	// anything whose ownership is not unequivocally stale.
	stagedRetention    = 30 * 24 * time.Hour
	stagedCleanupLimit = 16
	stagedScanLimit    = 512
	maintenanceErrCap  = 8
)

// StagedMaintenanceReport is the bounded startup maintenance result. Staged
// logs never appear as sessions; this report makes their conservative cleanup
// observable without turning them into resumable history.
type StagedMaintenanceReport struct {
	Scanned int
	Expired int
	Removed int
	Locked  int
	Refused int
	Failed  int
	Errors  []string
}

// MaintenanceReport returns the cleanup performed when this Store was opened.
func (s *Store) MaintenanceReport() StagedMaintenanceReport {
	report := s.maintenance
	report.Errors = append([]string(nil), report.Errors...)
	return report
}

func (s *Store) cleanupExpiredStaged(before time.Time, removeLimit int) StagedMaintenanceReport {
	// Maintenance authority is deliberately narrower than live publication
	// cleanup: the private-store log must be older than the retention window and
	// exclusively append-locked for the whole decision/removal sequence. Every
	// Switchboard publisher takes that same lock. Non-cooperative same-user
	// mutation is outside that serialization guarantee; when a seam observes
	// one, the repeated identity/marker checks below fail closed and retain the
	// evidence rather than claiming such mutation is impossible.
	var report StagedMaintenanceReport
	recordErr := func(path string, err error) {
		report.Failed++
		if len(report.Errors) < maintenanceErrCap {
			report.Errors = append(report.Errors, fmt.Sprintf("%s: %v", path, err))
		}
	}
	// Inventory a deterministic bounded prefix before removing anything.
	// filepath.WalkDir reads and sorts a whole directory before invoking its
	// callback, so its callback counter is not an allocation bound.
	var candidates []string
	rootEntries, err := readSessionDirectory(s.root, maxSessionWorkspaceDirectories)
	if err != nil {
		recordErr(s.root, err)
		return report
	}
	for _, entry := range rootEntries {
		if report.Scanned >= stagedScanLimit {
			break
		}
		path := filepath.Join(s.root, entry.Name())
		report.Scanned++
		if !entry.IsDir() {
			if strings.HasSuffix(entry.Name(), ".log") {
				candidates = append(candidates, path)
			}
			continue
		}
		remaining := stagedScanLimit - report.Scanned
		if remaining == 0 {
			break
		}
		entries, readErr := readSessionDirectory(path, min(remaining, maxSessionDirectoryEntries))
		if readErr != nil {
			if errors.Is(readErr, ErrSessionInventoryTooLarge) {
				// This directory alone consumes the rest of the maintenance scan.
				// Refuse its partial ordering instead of choosing filesystem order.
				report.Scanned = stagedScanLimit
				break
			}
			recordErr(path, readErr)
			continue
		}
		for _, child := range entries {
			if report.Scanned >= stagedScanLimit {
				break
			}
			report.Scanned++
			if !child.IsDir() && strings.HasSuffix(child.Name(), ".log") {
				candidates = append(candidates, filepath.Join(path, child.Name()))
			}
		}
	}

	for _, path := range candidates {
		if report.Removed >= removeLimit {
			break
		}
		if s.maintenanceBeforeOpen != nil {
			s.maintenanceBeforeOpen(path)
		}

		f, err := openSessionLog(path, true)
		if err != nil {
			recordErr(path, err)
			continue
		}
		if err := acquireLock(f); err != nil {
			_ = f.Close()
			if errors.Is(err, ErrSessionLocked) {
				report.Locked++
			} else {
				recordErr(path, err)
			}
			continue
		}
		if err := verifyCurrentSessionLogPath(f, path); err != nil {
			_ = releaseLock(f)
			_ = f.Close()
			recordErr(path, err)
			continue
		}
		openedInfo, err := f.Stat()
		if err != nil {
			_ = releaseLock(f)
			_ = f.Close()
			recordErr(path, err)
			continue
		}
		// DirEntry metadata was observed before this file was opened and can
		// belong to an inode that has already been replaced. Expiry is authority
		// only when it comes from the descriptor held under the append lock.
		if !openedInfo.Mode().IsRegular() || !openedInfo.ModTime().Before(before) {
			_ = releaseLock(f)
			_ = f.Close()
			continue
		}
		start, err := readFirstSessionStart(f, path)
		if err != nil || !start.Staged {
			_ = releaseLock(f)
			_ = f.Close()
			continue
		}
		validate := validatePublishedMarker
		if s.maintenanceValidate != nil {
			validate = s.maintenanceValidate
		}
		publicationErr := validate(path, start)
		if publicationErr == nil {
			_ = releaseLock(f)
			_ = f.Close()
			continue
		}
		if !errors.Is(publicationErr, ErrSessionUnpublished) {
			report.Refused++
			recordErr(path, publicationErr)
			_ = releaseLock(f)
			_ = f.Close()
			continue
		}
		report.Expired++
		if s.maintenanceBeforeOwned != nil {
			s.maintenanceBeforeOwned(path)
		}
		markerInfo, markerExists, ownedMarker, markerErr := inspectOwnedPublicationMarker(path, start, false)
		if markerErr != nil || !ownedMarker {
			report.Refused++
			if markerErr != nil {
				recordErr(path, markerErr)
			}
			_ = releaseLock(f)
			_ = f.Close()
			continue
		}
		if s.maintenanceBeforeRemove != nil {
			s.maintenanceBeforeRemove(path)
		}
		// Rebind cleanup ownership after the final test/operation seam. The append
		// lock excludes cooperative publishers; this repeated bounded read makes
		// an observed non-cooperative exact or foreign marker a refusal too.
		markerInfo, markerExists, ownedMarker, markerErr = inspectOwnedPublicationMarker(path, start, false)
		if markerErr != nil || !ownedMarker {
			report.Refused++
			if markerErr != nil {
				recordErr(path, markerErr)
			}
			_ = releaseLock(f)
			_ = f.Close()
			continue
		}
		lockedInfo, statErr := f.Stat()
		identityErr := verifyCurrentSessionLogPath(f, path)
		if statErr != nil || identityErr != nil {
			_ = releaseLock(f)
			closeErr := f.Close()
			recordErr(path, errors.Join(statErr, identityErr, closeErr))
			continue
		}
		// A cooperative writer cannot acquire this lock, but re-stat anyway:
		// non-cooperative same-user work must not let a log refreshed during
		// validation be removed on stale age evidence.
		if !lockedInfo.Mode().IsRegular() || !lockedInfo.ModTime().Before(before) {
			_ = releaseLock(f)
			_ = f.Close()
			continue
		}
		// Keep the lock through quarantine so another Switchboard process cannot
		// open the staged log in the gap between validation and removal.
		removeErr := removePathIfSame(path, lockedInfo)
		unlockErr := releaseLock(f)
		closeErr := f.Close()
		if removeErr != nil {
			// removePathIfSame reports a directory-sync failure after the name is
			// already gone. Count that removal despite the error or a failing disk
			// could defeat the per-startup deletion bound.
			if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
				report.Removed++
			}
			recordErr(path, errors.Join(removeErr, unlockErr, closeErr))
			continue
		}
		// Count pathname removal immediately so durability/marker cleanup errors
		// cannot let this maintenance pass exceed its deletion bound.
		report.Removed++
		if unlockErr != nil || closeErr != nil {
			recordErr(path, errors.Join(unlockErr, closeErr))
		}
		markerPath := publicationMarkerPath(path)
		if markerExists {
			if s.maintenanceBeforeMarkerRemove != nil {
				s.maintenanceBeforeMarkerRemove(markerPath)
			}
			if err := removePathIfSame(markerPath, markerInfo); err != nil {
				recordErr(markerPath, err)
			}
		}
	}
	return report
}

// cleanupMarkerOwned accepts an absent marker or a bounded prefix of the
// marker this log names. Anything else may be another owner's collision and
// is deliberately left for a human rather than guessed away.
func cleanupMarkerOwned(logPath string, start SessionStart) (bool, error) {
	_, _, owned, err := inspectOwnedPublicationMarker(logPath, start, false)
	return owned, err
}
