package scm

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
)

// EntryKind is the porcelain-v2 record class that described a path.
type EntryKind string

const (
	EntryOrdinary  EntryKind = "ordinary"
	EntryRename    EntryKind = "rename"
	EntryUnmerged  EntryKind = "unmerged"
	EntryUntracked EntryKind = "untracked"
	EntryIgnored   EntryKind = "ignored"
)

// PathState is Git's read-only view of one dirty, untracked, ignored, renamed,
// or conflicted path. Paths are repository-root relative and retain arbitrary
// bytes represented by a Go string.
type PathState struct {
	Path         string
	OriginalPath string
	Kind         EntryKind
	XY           string
	Submodule    string
	RenameScore  string

	Tracked   bool
	Untracked bool
	Ignored   bool
	Staged    bool
	Unstaged  bool
	Unmerged  bool
	Renamed   bool
	Copied    bool
	Nested    bool // Git identified the entry as a submodule.
}

// Status reads porcelain-v2 status for the whole worktree.
func (r *Repository) Status(ctx context.Context) ([]PathState, error) {
	return r.StatusPaths(ctx, nil)
}

// StatusPaths reads porcelain-v2 status. Nil paths means the whole worktree;
// otherwise every path is treated literally and must remain inside Root.
func (r *Repository) StatusPaths(ctx context.Context, paths []string) ([]PathState, error) {
	if err := r.executionAllowed(); err != nil {
		return nil, err
	}
	pathspecs, err := r.pathspecs(paths)
	if err != nil {
		return nil, err
	}
	args := []string{"status", "--porcelain=v2", "-z", "--untracked-files=all", "--ignored=matching", "--renames", "--"}
	args = append(args, pathspecs...)
	result := runGit(ctx, r.git, r.Root, maxStatusBytes, args...)
	if result.err != nil {
		return nil, result.commandError("reading Git status")
	}
	if result.stdoutTruncated {
		return nil, fmt.Errorf("%w: Git status exceeded %d bytes", ErrOutputLimit, maxStatusBytes)
	}
	return ParsePorcelainV2Z(result.stdout)
}

// ParsePorcelainV2Z parses `git status --porcelain=v2 -z`. It rejects unknown
// or incomplete records rather than silently dropping state that might make a
// later rollback unsafe.
func ParsePorcelainV2Z(data []byte) ([]PathState, error) {
	if len(data) > maxStatusBytes {
		return nil, fmt.Errorf("%w: Git status exceeded %d bytes", ErrOutputLimit, maxStatusBytes)
	}
	var out []PathState
	for cursor := 0; cursor < len(data); {
		end := bytes.IndexByte(data[cursor:], 0)
		if end < 0 {
			return nil, fmt.Errorf("%w: final record has no NUL terminator", ErrMalformedStatus)
		}
		end += cursor
		record := data[cursor:end]
		cursor = end + 1
		if len(record) == 0 {
			continue
		}

		var state PathState
		switch record[0] {
		case '#':
			continue
		case '1':
			fields := bytes.SplitN(record, []byte{' '}, 9)
			if len(fields) != 9 || string(fields[0]) != "1" || !validTrackedFields(fields[1], fields[2]) {
				return nil, malformedRecord(record)
			}
			state = trackedState(EntryOrdinary, fields[1], fields[2], fields[8])
		case '2':
			fields := bytes.SplitN(record, []byte{' '}, 10)
			if len(fields) != 10 || string(fields[0]) != "2" || !validTrackedFields(fields[1], fields[2]) || !validRenameScore(fields[8]) {
				return nil, malformedRecord(record)
			}
			if cursor >= len(data) {
				return nil, fmt.Errorf("%w: rename record has no original path", ErrMalformedStatus)
			}
			originalEnd := bytes.IndexByte(data[cursor:], 0)
			if originalEnd < 0 {
				return nil, fmt.Errorf("%w: rename original path has no NUL terminator", ErrMalformedStatus)
			}
			originalEnd += cursor
			state = trackedState(EntryRename, fields[1], fields[2], fields[9])
			state.RenameScore = string(fields[8])
			state.Renamed = fields[8][0] == 'R'
			state.Copied = fields[8][0] == 'C'
			state.OriginalPath = string(data[cursor:originalEnd])
			if state.OriginalPath == "" {
				return nil, fmt.Errorf("%w: rename record has an empty original path", ErrMalformedStatus)
			}
			cursor = originalEnd + 1
		case 'u':
			fields := bytes.SplitN(record, []byte{' '}, 11)
			if len(fields) != 11 || string(fields[0]) != "u" || !validTrackedFields(fields[1], fields[2]) {
				return nil, malformedRecord(record)
			}
			state = trackedState(EntryUnmerged, fields[1], fields[2], fields[10])
			state.Unmerged = true
		case '?':
			if len(record) < 3 || record[1] != ' ' {
				return nil, malformedRecord(record)
			}
			state = PathState{Path: string(record[2:]), Kind: EntryUntracked, Untracked: true}
		case '!':
			if len(record) < 3 || record[1] != ' ' {
				return nil, malformedRecord(record)
			}
			state = PathState{Path: string(record[2:]), Kind: EntryIgnored, Ignored: true}
		default:
			return nil, malformedRecord(record)
		}
		if state.Path == "" {
			return nil, fmt.Errorf("%w: record has an empty path", ErrMalformedStatus)
		}
		out = append(out, state)
		if len(out) > maxStatusEntries {
			return nil, fmt.Errorf("%w: Git status exceeded %d entries", ErrOutputLimit, maxStatusEntries)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Path == out[j].Path {
			return out[i].OriginalPath < out[j].OriginalPath
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

func trackedState(kind EntryKind, xy, submodule, path []byte) PathState {
	xyText := string(xy)
	state := PathState{
		Path:      string(path),
		Kind:      kind,
		XY:        xyText,
		Submodule: string(submodule),
		Tracked:   true,
		Nested:    len(submodule) > 0 && submodule[0] == 'S',
	}
	if len(xyText) == 2 {
		state.Staged = xyText[0] != '.'
		state.Unstaged = xyText[1] != '.'
		state.Renamed = strings.ContainsRune(xyText, 'R')
		state.Copied = strings.ContainsRune(xyText, 'C')
	}
	return state
}

func validTrackedFields(xy, submodule []byte) bool {
	if len(xy) != 2 || len(submodule) != 4 {
		return false
	}
	if bytes.Equal(submodule, []byte("N...")) {
		return true
	}
	return submodule[0] == 'S' &&
		(submodule[1] == '.' || submodule[1] == 'C') &&
		(submodule[2] == '.' || submodule[2] == 'M') &&
		(submodule[3] == '.' || submodule[3] == 'U')
}

func validRenameScore(score []byte) bool {
	if len(score) < 2 || len(score) > 4 || (score[0] != 'R' && score[0] != 'C') {
		return false
	}
	value := 0
	for _, digit := range score[1:] {
		if digit < '0' || digit > '9' {
			return false
		}
		value = value*10 + int(digit-'0')
	}
	return value <= 100
}

func malformedRecord(record []byte) error {
	const max = 120
	shown := record
	if len(shown) > max {
		shown = shown[:max]
	}
	return fmt.Errorf("%w: %q", ErrMalformedStatus, shown)
}
