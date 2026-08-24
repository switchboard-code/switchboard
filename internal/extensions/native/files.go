package native

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

const (
	maxConfigBytes   = 4 << 20
	maxJSONDepth     = 128
	marketplaceIndex = ".agents/plugins/marketplace.json"
)

func readBoundedFile(filePath string, limit int64) ([]byte, error) {
	return readBoundedFileWithHook(filePath, limit, nil)
}

func readBoundedFileWithHook(filePath string, limit int64, beforeOpen func()) ([]byte, error) {
	info, err := os.Lstat(filePath)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("configuration file is a symlink")
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("configuration path is not a regular file")
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("configuration is %d bytes; limit is %d", info.Size(), limit)
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	file, err := openNativePathRead(filePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, errors.New("configuration changed while it was opened")
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("configuration grew beyond %d-byte limit while reading", limit)
	}
	finished, err := file.Stat()
	if err != nil {
		return nil, err
	}
	linked, linkErr := os.Lstat(filePath)
	if linkErr != nil || linked.Mode()&os.ModeSymlink != 0 || !linked.Mode().IsRegular() ||
		!os.SameFile(opened, finished) || !os.SameFile(finished, linked) ||
		opened.Size() != finished.Size() || finished.Size() != int64(len(raw)) ||
		!opened.ModTime().Equal(finished.ModTime()) {
		return nil, errors.Join(linkErr, errors.New("configuration changed while it was read"))
	}
	return raw, nil
}

func validateUniqueJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := consumeJSONValue(decoder, 0); err != nil {
		return err
	}
	if token, err := decoder.Token(); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("unexpected JSON value after document: %v", token)
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder, depth int) error {
	if depth > maxJSONDepth {
		return fmt.Errorf("JSON exceeds %d-level depth limit", maxJSONDepth)
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, structured := token.(json.Delim)
	if !structured {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object contains a non-string key")
			}
			if seen[key] {
				return fmt.Errorf("JSON object contains duplicate key %q", key)
			}
			seen[key] = true
			if err := consumeJSONValue(decoder, depth+1); err != nil {
				return fmt.Errorf("field %q: %w", key, err)
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return errors.New("JSON object has invalid closing token")
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return errors.New("JSON array has invalid closing token")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	return nil
}

func readJSONFile(filePath string, target any) error {
	raw, err := readBoundedFile(filePath, maxConfigBytes)
	if err != nil {
		return err
	}
	if err := validateUniqueJSON(raw); err != nil {
		return err
	}
	if err := json.Unmarshal(raw, target); err != nil {
		return err
	}
	return nil
}

func readJSONWithin(rootPath, relative string, target any) error {
	raw, err := readWithin(rootPath, relative, maxConfigBytes)
	if err != nil {
		return err
	}
	if err := validateUniqueJSON(raw); err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func readWithin(rootPath, relative string, limit int64) ([]byte, error) {
	return readWithinWithHook(rootPath, relative, limit, nil)
}

func readWithinWithHook(rootPath, relative string, limit int64, beforeOpen func()) ([]byte, error) {
	relative, err := safeRelative(relative, false)
	if err != nil {
		return nil, err
	}
	realRoot, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		return nil, err
	}
	root, err := rootedfs.OpenRoot(realRoot)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err := rejectSymlinkComponents(root, relative); err != nil {
		return nil, err
	}
	info, err := root.Lstat(filepath.FromSlash(relative))
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("path is not a regular file")
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("file is %d bytes; limit is %d", info.Size(), limit)
	}
	if beforeOpen != nil {
		beforeOpen()
	}
	file, err := openNativeRootRead(root, filepath.FromSlash(relative))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return nil, errors.New("path changed while it was opened")
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("file grew beyond %d-byte limit while reading", limit)
	}
	finished, err := file.Stat()
	if err != nil {
		return nil, err
	}
	linked, linkErr := root.Lstat(filepath.FromSlash(relative))
	if linkErr != nil || linked.Mode()&os.ModeSymlink != 0 || !linked.Mode().IsRegular() ||
		!os.SameFile(opened, finished) || !os.SameFile(finished, linked) ||
		opened.Size() != finished.Size() || finished.Size() != int64(len(raw)) ||
		!opened.ModTime().Equal(finished.ModTime()) {
		return nil, errors.Join(linkErr, errors.New("path changed while it was read"))
	}
	return raw, nil
}

func resolveLocalDirectory(rootPath, declared string) (string, error) {
	relative, err := safeRelative(declared, true)
	if err != nil {
		return "", err
	}
	absRoot, err := filepath.Abs(rootPath)
	if err != nil {
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", err
	}
	root, err := rootedfs.OpenRoot(realRoot)
	if err != nil {
		return "", err
	}
	defer root.Close()
	if err := rejectSymlinkComponents(root, relative); err != nil {
		return "", err
	}
	info, err := root.Lstat(filepath.FromSlash(relative))
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("local plugin source is not a directory")
	}
	return filepath.Join(filepath.Clean(absRoot), filepath.FromSlash(relative)), nil
}

func rejectSymlinkComponents(root *os.Root, relative string) error {
	parts := strings.Split(relative, "/")
	for index := range parts {
		prefix := strings.Join(parts[:index+1], "/")
		info, err := root.Lstat(filepath.FromSlash(prefix))
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink component %q is not allowed", prefix)
		}
		if index < len(parts)-1 && !info.IsDir() {
			return fmt.Errorf("path component %q is not a directory", prefix)
		}
	}
	return nil
}

func safeRelative(declared string, requireDot bool) (string, error) {
	if declared == "" || strings.ContainsRune(declared, '\x00') {
		return "", errors.New("path is empty or contains NUL")
	}
	if strings.Contains(declared, "\\") {
		return "", errors.New("backslash is not a portable path separator")
	}
	if filepath.IsAbs(declared) || strings.HasPrefix(declared, "/") || looksLikeDrive(declared) {
		return "", errors.New("absolute path is not allowed")
	}
	if requireDot && !strings.HasPrefix(declared, "./") {
		return "", errors.New("local source path must begin with ./")
	}
	for _, part := range strings.Split(declared, "/") {
		if part == ".." {
			return "", errors.New("parent traversal is not allowed")
		}
	}
	clean := path.Clean(declared)
	clean = strings.TrimPrefix(clean, "./")
	if clean == "." || clean == "" || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("path must stay below its root")
	}
	return clean, nil
}

func looksLikeDrive(value string) bool {
	if len(value) < 2 || value[1] != ':' {
		return false
	}
	return value[0] >= 'a' && value[0] <= 'z' || value[0] >= 'A' && value[0] <= 'Z'
}

func exactAbsoluteDirectory(value string) (string, error) {
	abs, err := exactAbsolutePath(value)
	if err != nil {
		return "", err
	}
	realPath, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(realPath)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("installed path is not a directory")
	}
	return abs, nil
}

func exactAbsolutePath(value string) (string, error) {
	if value == "" || strings.HasPrefix(value, "~") || !filepath.IsAbs(value) {
		return "", errors.New("path must be absolute; home and environment expansion are not performed")
	}
	return filepath.Clean(value), nil
}

func canonicalExactDirectory(value string) (string, error) {
	abs, err := exactAbsolutePath(value)
	if err != nil {
		return "", err
	}
	return canonicalDirectory(abs)
}

func canonicalDirectory(value string) (string, error) {
	if value == "" {
		return "", errors.New("directory is required")
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	realPath, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(realPath)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("path is not a directory")
	}
	return filepath.Clean(realPath), nil
}

func containedRelative(rootPath, filePath string) (string, error) {
	absRoot, err := filepath.Abs(rootPath)
	if err != nil {
		return "", err
	}
	realRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", err
	}
	absFile, err := filepath.Abs(filePath)
	if err != nil {
		return "", err
	}
	canonicalFile, err := canonicalizeFileParent(absFile)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(realRoot, canonicalFile)
	if err != nil {
		return "", err
	}
	relative = filepath.ToSlash(relative)
	if relative == ".." || strings.HasPrefix(relative, "../") {
		return "", errors.New("settings path escapes its project")
	}
	return safeRelative(relative, false)
}

func canonicalizeFileParent(filePath string) (string, error) {
	current := filepath.Dir(filePath)
	suffix := []string{filepath.Base(filePath)}
	for {
		realParent, err := filepath.EvalSymlinks(current)
		if err == nil {
			parts := append([]string{realParent}, suffix...)
			return filepath.Join(parts...), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append([]string{filepath.Base(current)}, suffix...)
		current = parent
	}
}

func pathWithin(parent, child string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
