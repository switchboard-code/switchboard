package session

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

// ensurePublishedSessionDurableForOpen is the ownerless durability half of a
// staged-session adoption. Publication makes a child visible by writing the
// exact sidecar, but a process may have died after that byte became visible
// and before either persistence barrier completed. Writable resume therefore
// repeats both barriers before it can migrate, repair, or append to the log.
func (s *Store) ensurePublishedSessionDurableForOpen(log *os.File, logPath string, start SessionStart, logStamp publicationMutationStamp) (resultErr error) {
	if !start.Staged {
		return nil
	}
	if !validPublicationID(start.PublicationID) {
		return publicationResumeRecoveryError(start.ID,
			errors.New("staged session has an invalid publication identity"))
	}

	directoryPath := filepath.Dir(logPath)
	root, directory, directoryInfo, err := openBoundSessionDirectory(directoryPath, s.openPublicationBeforeDirectory)
	if err != nil {
		return publicationResumeRecoveryError(start.ID,
			fmt.Errorf("binding session publication directory: %w", err))
	}
	defer func() {
		resultErr = errors.Join(resultErr, directory.Close(), root.Close())
	}()

	markerName := filepath.Base(publicationMarkerPath(logPath))
	marker, err := openRootedPublicationMarker(root, markerName)
	if err != nil {
		return publicationResumeRecoveryError(start.ID,
			fmt.Errorf("opening session publication marker: %w", err))
	}
	markerClosed := false
	defer func() {
		if !markerClosed {
			resultErr = errors.Join(resultErr, marker.Close())
		}
	}()
	if s.openPublicationAfterMarker != nil {
		s.openPublicationAfterMarker(publicationMarkerPath(logPath))
	}

	data, err := readPublicationMarkerFile(marker, publicationMarkerPath(logPath))
	if err != nil {
		return publicationResumeRecoveryError(start.ID,
			fmt.Errorf("reading session publication marker: %w", err))
	}
	expected := []byte(publicationMarker(start.ID, start.PublicationID))
	if !bytes.Equal(data, expected) {
		return publicationResumeRecoveryError(start.ID,
			errors.New("session publication marker is torn or does not exactly match its session"))
	}
	stamp, err := capturePublishedSessionCommit(log, logPath, root, directory, directoryInfo, marker, markerName, publicationMarkerPath(logPath), expected, logStamp)
	if err != nil {
		return publicationResumeRecoveryError(start.ID, err)
	}

	if s.openPublicationMarkerSync != nil {
		err = s.openPublicationMarkerSync(marker)
	} else {
		err = marker.Sync()
	}
	if err != nil {
		return publicationResumeRecoveryError(start.ID,
			fmt.Errorf("syncing session publication marker: %w", err))
	}
	if err := verifyPublishedSessionCommitStamp(log, logPath, root, directory, directoryInfo, marker, markerName, publicationMarkerPath(logPath), expected, stamp); err != nil {
		return publicationResumeRecoveryError(start.ID, err)
	}
	directoryStamp, err := captureBoundSessionDirectoryStamp(root, directory, directoryInfo, directoryPath)
	if err != nil {
		return publicationResumeRecoveryError(start.ID, err)
	}

	if s.openPublicationDirectorySync != nil {
		err = s.openPublicationDirectorySync(directory)
	} else {
		err = syncOpenedSessionDirectory(directory)
	}
	if err != nil {
		return publicationResumeRecoveryError(start.ID,
			fmt.Errorf("syncing session publication directory: %w", err))
	}
	if err := verifyPublishedSessionCommitStamp(log, logPath, root, directory, directoryInfo, marker, markerName, publicationMarkerPath(logPath), expected, stamp); err != nil {
		return publicationResumeRecoveryError(start.ID, err)
	}
	if err := verifyBoundSessionDirectoryStamp(root, directory, directoryInfo, directoryPath, directoryStamp); err != nil {
		return publicationResumeRecoveryError(start.ID, err)
	}
	if err := marker.Close(); err != nil {
		return publicationResumeRecoveryError(start.ID,
			fmt.Errorf("closing session publication marker: %w", err))
	}
	markerClosed = true
	return nil
}

func publicationResumeRecoveryError(id string, err error) error {
	return fmt.Errorf(
		"refusing writable resume of published session %s because its publication durability could not be verified; leave its .log and .published files in place, then restart Switchboard to retry recovery: %w",
		id, err)
}

func openBoundSessionDirectory(path string, beforeOpen func(string)) (*os.Root, *os.File, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, nil, nil, fmt.Errorf("session publication parent %s is not a real directory", path)
	}
	if beforeOpen != nil {
		beforeOpen(path)
	}
	root, err := rootedfs.OpenRoot(path)
	if err != nil {
		return nil, nil, nil, err
	}
	fail := func(cause error) (*os.Root, *os.File, os.FileInfo, error) {
		return nil, nil, nil, errors.Join(cause, root.Close())
	}
	rootInfo, err := root.Stat(".")
	if err != nil || !rootInfo.IsDir() || !os.SameFile(before, rootInfo) {
		return fail(errors.Join(fmt.Errorf("session publication parent %s changed while it was bound", path), err))
	}
	directory, err := root.Open(".")
	if err != nil {
		return fail(err)
	}
	directoryInfo, err := directory.Stat()
	if err != nil || !directoryInfo.IsDir() || !os.SameFile(rootInfo, directoryInfo) {
		return nil, nil, nil, errors.Join(
			fmt.Errorf("session publication parent %s changed while its sync handle was opened", path),
			err, directory.Close(), root.Close())
	}
	current, err := os.Lstat(path)
	if err != nil || !current.IsDir() || !os.SameFile(rootInfo, current) {
		return nil, nil, nil, errors.Join(
			fmt.Errorf("session publication parent %s no longer owns its path", path),
			err, directory.Close(), root.Close())
	}
	return root, directory, directoryInfo, nil
}

func openRootedPublicationMarker(root *os.Root, name string) (*os.File, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("session publication marker %s is not a regular file", name)
	}
	marker, err := openRootedPublicationMarkerDescriptor(root, name)
	if err != nil {
		return nil, err
	}
	if err := verifyRootedPublicationMarker(root, marker, name, before); err != nil {
		return nil, errors.Join(err, marker.Close())
	}
	return marker, nil
}

func verifyPublishedSessionOpenPaths(log *os.File, logPath string, root *os.Root, directory *os.File, directoryInfo os.FileInfo, marker *os.File, markerName string) error {
	if err := verifyCurrentSessionLogPath(log, logPath); err != nil {
		return fmt.Errorf("session log changed while publication durability was verified: %w", err)
	}
	if err := verifyBoundSessionDirectory(root, directory, directoryInfo, filepath.Dir(logPath)); err != nil {
		return err
	}
	markerInfo, err := marker.Stat()
	if err != nil {
		return err
	}
	if err := verifyRootedPublicationMarker(root, marker, markerName, markerInfo); err != nil {
		return err
	}
	return nil
}

// verifyPublishedSessionCommit binds exact marker bytes to the descriptors
// whose persistence is being proved. The mutation stamps captured around this
// check cover both marker and child log: path identity alone cannot detect a
// same-inode truncate or rewrite across a persistence barrier.
func verifyPublishedSessionCommit(log *os.File, logPath string, root *os.Root, directory *os.File, directoryInfo os.FileInfo, marker *os.File, markerName, markerPath string, expected []byte) error {
	if err := verifyPublishedSessionOpenPaths(log, logPath, root, directory, directoryInfo, marker, markerName); err != nil {
		return err
	}
	if err := verifyExactPublicationMarkerBytes(marker, markerPath, expected); err != nil {
		return err
	}
	return verifyPublishedSessionOpenPaths(log, logPath, root, directory, directoryInfo, marker, markerName)
}

type publicationMutationStamp struct {
	size                       int64
	modifiedSeconds            int64
	modifiedNanoseconds        int64
	changedSeconds             int64
	changedNanoseconds         int64
	identityHigh, identityLow  uint64
	volumeOrDevice, attributes uint64
}

func captureStablePublicationLogStamp(log *os.File, path string) (publicationMutationStamp, error) {
	if err := verifyCurrentSessionLogPath(log, path); err != nil {
		return publicationMutationStamp{}, err
	}
	first, err := publicationObjectMutationStamp(log)
	if err != nil {
		return publicationMutationStamp{}, err
	}
	if err := verifyCurrentSessionLogPath(log, path); err != nil {
		return publicationMutationStamp{}, err
	}
	second, err := publicationObjectMutationStamp(log)
	if err != nil {
		return publicationMutationStamp{}, err
	}
	if first != second {
		return publicationMutationStamp{}, fmt.Errorf("session publication child %s changed while its validated fingerprint was captured", path)
	}
	return second, nil
}

func verifyPublicationLogStamp(log *os.File, path string, want publicationMutationStamp) error {
	if err := verifyCurrentSessionLogPath(log, path); err != nil {
		return err
	}
	first, err := publicationObjectMutationStamp(log)
	if err != nil {
		return err
	}
	if first != want {
		return fmt.Errorf("session publication child %s changed after its validated fingerprint", path)
	}
	if err := verifyCurrentSessionLogPath(log, path); err != nil {
		return err
	}
	second, err := publicationObjectMutationStamp(log)
	if err != nil {
		return err
	}
	if second != want {
		return fmt.Errorf("session publication child %s changed while its validated fingerprint was verified", path)
	}
	return nil
}

type publishedSessionCommitStamp struct {
	log    publicationMutationStamp
	marker publicationMutationStamp
}

func capturePublishedSessionCommit(log *os.File, logPath string, root *os.Root, directory *os.File, directoryInfo os.FileInfo, marker *os.File, markerName, markerPath string, expected []byte, expectedLog publicationMutationStamp) (publishedSessionCommitStamp, error) {
	if err := verifyPublishedSessionCommit(log, logPath, root, directory, directoryInfo, marker, markerName, markerPath, expected); err != nil {
		return publishedSessionCommitStamp{}, err
	}
	firstMarker, err := publicationObjectMutationStamp(marker)
	if err != nil {
		return publishedSessionCommitStamp{}, err
	}
	firstLog, err := publicationObjectMutationStamp(log)
	if err != nil {
		return publishedSessionCommitStamp{}, err
	}
	if firstLog != expectedLog {
		return publishedSessionCommitStamp{}, fmt.Errorf("session publication child %s changed after its validated fingerprint", logPath)
	}
	if err := verifyPublishedSessionCommit(log, logPath, root, directory, directoryInfo, marker, markerName, markerPath, expected); err != nil {
		return publishedSessionCommitStamp{}, err
	}
	secondMarker, err := publicationObjectMutationStamp(marker)
	if err != nil {
		return publishedSessionCommitStamp{}, err
	}
	secondLog, err := publicationObjectMutationStamp(log)
	if err != nil {
		return publishedSessionCommitStamp{}, err
	}
	if firstMarker != secondMarker {
		return publishedSessionCommitStamp{}, fmt.Errorf("session publication marker %s changed while its pre-sync fingerprint was captured", markerPath)
	}
	if firstLog != secondLog {
		return publishedSessionCommitStamp{}, fmt.Errorf("session publication child %s changed while its pre-sync fingerprint was captured", logPath)
	}
	if secondLog != expectedLog {
		return publishedSessionCommitStamp{}, fmt.Errorf("session publication child %s no longer matches its validated fingerprint", logPath)
	}
	return publishedSessionCommitStamp{log: secondLog, marker: secondMarker}, nil
}

func verifyPublishedSessionCommitStamp(log *os.File, logPath string, root *os.Root, directory *os.File, directoryInfo os.FileInfo, marker *os.File, markerName, markerPath string, expected []byte, want publishedSessionCommitStamp) error {
	if err := verifyPublishedSessionCommit(log, logPath, root, directory, directoryInfo, marker, markerName, markerPath, expected); err != nil {
		return err
	}
	firstMarker, err := publicationObjectMutationStamp(marker)
	if err != nil {
		return err
	}
	if firstMarker != want.marker {
		return fmt.Errorf("session publication marker %s changed after its pre-sync fingerprint", markerPath)
	}
	firstLog, err := publicationObjectMutationStamp(log)
	if err != nil {
		return err
	}
	if firstLog != want.log {
		return fmt.Errorf("session publication child %s changed after its pre-sync fingerprint", logPath)
	}
	if err := verifyPublishedSessionCommit(log, logPath, root, directory, directoryInfo, marker, markerName, markerPath, expected); err != nil {
		return err
	}
	secondMarker, err := publicationObjectMutationStamp(marker)
	if err != nil {
		return err
	}
	if secondMarker != want.marker {
		return fmt.Errorf("session publication marker %s changed while its sync fingerprint was verified", markerPath)
	}
	secondLog, err := publicationObjectMutationStamp(log)
	if err != nil {
		return err
	}
	if secondLog != want.log {
		return fmt.Errorf("session publication child %s changed while its sync fingerprint was verified", logPath)
	}
	return nil
}

func captureBoundSessionDirectoryStamp(root *os.Root, directory *os.File, directoryInfo os.FileInfo, path string) (publicationMutationStamp, error) {
	if err := verifyBoundSessionDirectory(root, directory, directoryInfo, path); err != nil {
		return publicationMutationStamp{}, err
	}
	first, err := publicationObjectMutationStamp(directory)
	if err != nil {
		return publicationMutationStamp{}, err
	}
	if err := verifyBoundSessionDirectory(root, directory, directoryInfo, path); err != nil {
		return publicationMutationStamp{}, err
	}
	second, err := publicationObjectMutationStamp(directory)
	if err != nil {
		return publicationMutationStamp{}, err
	}
	if first != second {
		return publicationMutationStamp{}, fmt.Errorf("session publication directory %s changed while its pre-sync fingerprint was captured", path)
	}
	return second, nil
}

func verifyBoundSessionDirectoryStamp(root *os.Root, directory *os.File, directoryInfo os.FileInfo, path string, want publicationMutationStamp) error {
	if err := verifyBoundSessionDirectory(root, directory, directoryInfo, path); err != nil {
		return err
	}
	first, err := publicationObjectMutationStamp(directory)
	if err != nil {
		return err
	}
	if first != want {
		return fmt.Errorf("session publication directory %s changed after its pre-sync fingerprint", path)
	}
	if err := verifyBoundSessionDirectory(root, directory, directoryInfo, path); err != nil {
		return err
	}
	second, err := publicationObjectMutationStamp(directory)
	if err != nil {
		return err
	}
	if second != want {
		return fmt.Errorf("session publication directory %s changed while its sync fingerprint was verified", path)
	}
	return nil
}

func verifyExactPublicationMarkerBytes(marker *os.File, path string, expected []byte) error {
	if len(expected) > maxPublicationMarker {
		return fmt.Errorf("expected publication marker %s exceeds its %d-byte limit", path, maxPublicationMarker)
	}
	before, err := marker.Stat()
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() || before.Size() != int64(len(expected)) {
		return fmt.Errorf("session publication marker %s changed size during durability verification", path)
	}
	data := make([]byte, len(expected))
	n, readErr := marker.ReadAt(data, 0)
	if readErr != nil || n != len(data) {
		return errors.Join(
			fmt.Errorf("reading exact session publication marker %s: read %d of %d bytes", path, n, len(data)),
			readErr)
	}
	after, err := marker.Stat()
	if err != nil {
		return err
	}
	if !after.Mode().IsRegular() || after.Size() != int64(len(expected)) ||
		!before.ModTime().Equal(after.ModTime()) || !bytes.Equal(data, expected) {
		return fmt.Errorf("session publication marker %s changed bytes during durability verification", path)
	}
	return nil
}

func verifyBoundSessionDirectory(root *os.Root, directory *os.File, expected os.FileInfo, path string) error {
	rootInfo, rootErr := root.Stat(".")
	directoryInfo, directoryErr := directory.Stat()
	current, currentErr := os.Lstat(path)
	if rootErr != nil || directoryErr != nil || currentErr != nil ||
		!rootInfo.IsDir() || !directoryInfo.IsDir() || !current.IsDir() ||
		!os.SameFile(expected, rootInfo) || !os.SameFile(expected, directoryInfo) || !os.SameFile(expected, current) {
		return errors.Join(
			fmt.Errorf("session publication parent %s changed during durability verification", path),
			rootErr, directoryErr, currentErr)
	}
	return nil
}

func verifyRootedPublicationMarker(root *os.Root, marker *os.File, name string, expected os.FileInfo) error {
	opened, openedErr := marker.Stat()
	current, currentErr := root.Lstat(name)
	if openedErr != nil || currentErr != nil ||
		!opened.Mode().IsRegular() || !current.Mode().IsRegular() ||
		!os.SameFile(expected, opened) || !os.SameFile(expected, current) {
		return errors.Join(
			fmt.Errorf("session publication marker %s changed during durability verification", name),
			openedErr, currentErr)
	}
	links, err := sessionLogLinkCount(marker)
	if err != nil {
		return fmt.Errorf("checking link count for session publication marker %s: %w", name, err)
	}
	if links != 1 {
		return fmt.Errorf("session publication marker %s has %d hard links", name, links)
	}
	return nil
}
