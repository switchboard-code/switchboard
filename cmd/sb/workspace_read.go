package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

func openCLIWorkspaceRoot(workspace, path string) (*os.Root, string, string, os.FileInfo, error) {
	workspaceAbs, err := filepath.Abs(workspace)
	if err != nil {
		return nil, "", "", nil, err
	}
	workspaceReal, err := filepath.EvalSymlinks(workspaceAbs)
	if err != nil {
		return nil, "", "", nil, err
	}
	rootBefore, err := os.Lstat(workspaceReal)
	if err != nil {
		return nil, "", "", nil, err
	}
	if !rootBefore.IsDir() {
		return nil, "", "", nil, fmt.Errorf("workspace root is not a directory")
	}

	candidate := path
	if !filepath.IsAbs(candidate) {
		candidate = filepath.Join(workspaceAbs, filepath.FromSlash(candidate))
	}
	rel, err := filepath.Rel(workspaceAbs, filepath.Clean(candidate))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		// Callers that already hold a canonical path use the resolved workspace
		// spelling rather than the user's symlinked spelling.
		rel, err = filepath.Rel(workspaceReal, filepath.Clean(candidate))
	}
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, "", "", nil, fmt.Errorf("path is outside the workspace")
	}
	rel = filepath.Clean(rel)

	rooted, err := rootedfs.OpenRoot(workspaceReal)
	if err != nil {
		return nil, "", "", nil, err
	}
	if err := verifyCLIWorkspaceRoot(workspaceReal, rootBefore, rooted); err != nil {
		_ = rooted.Close()
		return nil, "", "", nil, err
	}
	return rooted, rel, workspaceReal, rootBefore, nil
}

func verifyCLIWorkspaceRoot(path string, expected os.FileInfo, rooted *os.Root) error {
	opened, openedErr := rooted.Stat(".")
	current, currentErr := os.Lstat(path)
	if openedErr != nil || currentErr != nil || current.Mode()&os.ModeSymlink != 0 || !current.IsDir() ||
		!os.SameFile(expected, opened) || !os.SameFile(opened, current) {
		return errors.Join(openedErr, currentErr, fmt.Errorf("workspace root changed while it was read"))
	}
	return nil
}

func readWorkspaceFileBounded(workspace, path string, limit int64, beforeOpen func()) ([]byte, error) {
	rooted, rel, rootPath, rootInfo, err := openCLIWorkspaceRoot(workspace, path)
	if err != nil {
		return nil, err
	}
	defer rooted.Close()
	before, err := rooted.Stat(rel)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if before.Size() > limit {
		return nil, fmt.Errorf("%s is %d bytes; limit is %d", path, before.Size(), limit)
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	file, err := openWorkspaceBoundedRead(rooted, rel)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, fmt.Errorf("%s changed identity while it was opened", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s grew beyond the %d-byte limit", path, limit)
	}
	finished, err := file.Stat()
	if err != nil {
		return nil, err
	}
	linked, linkErr := rooted.Stat(rel)
	if linkErr != nil || !linked.Mode().IsRegular() ||
		!os.SameFile(opened, finished) || !os.SameFile(finished, linked) ||
		opened.Size() != finished.Size() || finished.Size() != int64(len(data)) ||
		!opened.ModTime().Equal(finished.ModTime()) {
		return nil, errors.Join(linkErr, fmt.Errorf("%s changed while it was read", path))
	}
	if err := verifyCLIWorkspaceRoot(rootPath, rootInfo, rooted); err != nil {
		return nil, err
	}
	return data, nil
}

func readWorkspaceDirectoryBounded(workspace, path string, maxEntries int, beforeOpen func()) ([]os.DirEntry, error) {
	rooted, rel, rootPath, rootInfo, err := openCLIWorkspaceRoot(workspace, path)
	if err != nil {
		return nil, err
	}
	defer rooted.Close()
	before, err := rooted.Stat(rel)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() {
		return nil, fmt.Errorf("%s is not a real directory", path)
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	directory, err := openWorkspaceBoundedRead(rooted, rel)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	opened, err := directory.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.IsDir() || !os.SameFile(before, opened) {
		return nil, fmt.Errorf("%s changed identity while it was opened", path)
	}
	entries, readErr := directory.ReadDir(maxEntries + 1)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, readErr
	}
	if len(entries) > maxEntries {
		return nil, fmt.Errorf("%s has more than %d entries", path, maxEntries)
	}
	finished, err := directory.Stat()
	if err != nil {
		return nil, err
	}
	linked, linkErr := rooted.Stat(rel)
	if linkErr != nil || !linked.IsDir() ||
		!os.SameFile(opened, finished) || !os.SameFile(finished, linked) ||
		!opened.ModTime().Equal(finished.ModTime()) {
		return nil, errors.Join(linkErr, fmt.Errorf("%s changed while it was read", path))
	}
	if err := verifyCLIWorkspaceRoot(rootPath, rootInfo, rooted); err != nil {
		return nil, err
	}
	return entries, nil
}
