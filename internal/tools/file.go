package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"

	"github.com/switchboard-code/switchboard/internal/permission"
)

const maxReadBytes = 256 << 10

type readTool struct{ r *Registry }

func (t *readTool) Name() string { return "read" }

func (t *readTool) Description() string {
	return "Read a UTF-8 text file from the workspace. Returns the file's exact bytes with " +
		"no line numbers or other decoration, so text taken from a read can be pasted " +
		"straight into edit's old_string. Use offset and limit, counted in lines, for " +
		"large files."
}

func (t *readTool) ParallelSafe() bool { return true }

func (t *readTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File path, absolute or relative to the workspace root."},
    "offset": {"type": "integer", "description": "First line to return, 1-based. Defaults to the start of the file."},
    "limit": {"type": "integer", "description": "How many lines to return. Defaults to the rest of the file."}
  },
  "required": ["path"]
}`)
}

type readInput struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

func (t *readTool) Plan(input json.RawMessage) (Plan, error) {
	var in readInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Plan{}, fmt.Errorf("read: %w", err)
	}
	abs, err := t.r.resolve(in.Path)
	if err != nil {
		return Plan{}, err
	}

	return Plan{
		Request: permission.Request{Tool: t.Name(), Effect: permission.EffectRead, Path: t.r.display(abs)},
		Run: func(context.Context) (Result, error) {
			return t.read(abs, in)
		},
	}, nil
}

func (t *readTool) read(abs string, in readInput) (Result, error) {
	return t.readWithHook(abs, in, nil)
}

func (t *readTool) readWithHook(abs string, in readInput, beforeOpen func()) (Result, error) {
	root, relative, err := t.r.openResolvedWorkspace(abs)
	if err != nil {
		return errorf("cannot read %s: %v", t.r.display(abs), err)
	}
	defer root.Close()
	data, info, err := readRegularWorkspaceFile(root, relative, t.r.display(abs), maxWorkspaceFileBytes, beforeOpen)
	if err != nil {
		return errorf("cannot read %s: %v", t.r.display(abs), err)
	}
	// A root handle follows its directory across a rename. Revalidate the
	// canonical workspace pathname after the complete read so bytes from a
	// root moved outside the workspace cannot become a tool result.
	if err := t.r.verifyWorkspaceRoot(root); err != nil {
		return errorf("cannot read %s: workspace changed while it was read", t.r.display(abs))
	}

	// The hash covers the whole file even when only a slice is returned. A
	// partial read still tells the agent what version it saw, and a write must
	// be checked against the file as a whole.
	current := hashContent(data)
	t.r.versions.record(abs, current, info)

	if len(data) > maxReadBytes && in.Limit == 0 {
		return errorf("%s is %d bytes, over the %d byte limit for a whole-file read; "+
			"use offset and limit", t.r.display(abs), len(data), maxReadBytes)
	}

	content := string(data)
	if in.Offset <= 0 && in.Limit <= 0 {
		// §6.7's re-injection skip: content the context already holds
		// complete and unchanged is answered with a marker, not repeated.
		// Only a full, uncapped read arms this — the whole map, not the
		// stale-check hash above — and mutation, external change, /undo,
		// and every session swap disarm it, so the skip can never stand in
		// for content the model does not actually have.
		if prior, ok := t.r.versions.getWhole(abs); ok && prior == current {
			return Result{Content: fmt.Sprintf("%s is unchanged since you last read it in this session; "+
				"the content you already have is still current, so it is not repeated. "+
				"Use offset and limit if you need a slice shown again.", t.r.display(abs))}, nil
		}
		t.r.versions.recordWhole(abs, current)
		if content == "" {
			return Result{Content: fmt.Sprintf("%s is empty", t.r.display(abs))}, nil
		}
		return Result{Content: content}, nil
	}

	lines := strings.Split(content, "\n")
	start := max(in.Offset-1, 0)
	if start >= len(lines) {
		return errorf("%s has %d lines; offset %d is past the end", t.r.display(abs), len(lines), in.Offset)
	}
	end := len(lines)
	if in.Limit > 0 && start+in.Limit < end {
		end = start + in.Limit
	}

	var b strings.Builder
	fmt.Fprintf(&b, "[lines %d-%d of %d]\n", start+1, end, len(lines))
	b.WriteString(strings.Join(lines[start:end], "\n"))
	return Result{Content: b.String()}, nil
}

type writeTool struct{ r *Registry }

func (t *writeTool) Name() string { return "write" }

func (t *writeTool) Description() string {
	return "Write a whole file, creating it or replacing its contents. An existing file must " +
		"have been read first in this session, and the write fails if it changed since " +
		"that read. Prefer edit for changing part of a file."
}

func (t *writeTool) ParallelSafe() bool { return false }

func (t *writeTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File path, absolute or relative to the workspace root."},
    "content": {"type": "string", "description": "The complete new contents of the file."}
  },
  "required": ["path", "content"]
}`)
}

type writeInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (t *writeTool) Plan(input json.RawMessage) (Plan, error) {
	var in writeInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Plan{}, fmt.Errorf("write: %w", err)
	}
	abs, err := t.r.resolve(in.Path)
	if err != nil {
		return Plan{}, err
	}

	return Plan{
		Request: permission.Request{Tool: t.Name(), Effect: permission.EffectWrite, Path: t.r.display(abs)},
		Run: func(ctx context.Context) (Result, error) {
			tx, res, ok := t.r.prepareFileMutation(abs, true)
			if !ok {
				return res, nil
			}
			defer tx.close()

			content := []byte(in.Content)
			mode := fs.FileMode(0o644)
			if tx.before.existed {
				mode = tx.before.mode
				if string(tx.before.content) == in.Content {
					return Result{Content: fmt.Sprintf("%s already has the requested contents; no changes made", t.r.display(abs))}, nil
				}
			}
			if err := tx.publish(ctx, content, mode, nil); err != nil {
				return errorf("cannot write %s: %v", t.r.display(abs), err)
			}
			return Result{Content: fmt.Sprintf("wrote %s (%d bytes)", t.r.display(abs), len(content))}, nil
		},
	}, nil
}

type editTool struct{ r *Registry }

func (t *editTool) Name() string { return "edit" }

func (t *editTool) Description() string {
	return "Replace an exact string in a file. old_string must appear exactly once unless " +
		"replace_all is set, and must match the file byte for byte including indentation. " +
		"The file must have been read first in this session, and the edit fails if it " +
		"changed since that read."
}

func (t *editTool) ParallelSafe() bool { return false }

func (t *editTool) Schema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "File path, absolute or relative to the workspace root."},
    "old_string": {"type": "string", "description": "Exact text to replace, including surrounding context to make it unique."},
    "new_string": {"type": "string", "description": "Replacement text. Use an empty string to delete."},
    "replace_all": {"type": "boolean", "description": "Replace every occurrence instead of requiring exactly one."}
  },
  "required": ["path", "old_string", "new_string"]
}`)
}

type editInput struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func (t *editTool) Plan(input json.RawMessage) (Plan, error) {
	var in editInput
	if err := json.Unmarshal(input, &in); err != nil {
		return Plan{}, fmt.Errorf("edit: %w", err)
	}
	if in.OldString == "" {
		return Plan{}, fmt.Errorf("edit: old_string is empty; use write to create a file")
	}
	if in.OldString == in.NewString {
		return Plan{}, fmt.Errorf("edit: old_string and new_string are identical")
	}
	abs, err := t.r.resolve(in.Path)
	if err != nil {
		return Plan{}, err
	}

	return Plan{
		Request: permission.Request{Tool: t.Name(), Effect: permission.EffectWrite, Path: t.r.display(abs)},
		Run: func(ctx context.Context) (Result, error) {
			return t.edit(ctx, abs, in)
		},
	}, nil
}

func (t *editTool) edit(ctx context.Context, abs string, in editInput) (Result, error) {
	tx, res, ok := t.r.prepareFileMutation(abs, false)
	if !ok {
		return res, nil
	}
	defer tx.close()
	content := string(tx.before.content)

	count := strings.Count(content, in.OldString)
	switch {
	case count == 0:
		return errorf("old_string was not found in %s. Read the file again: it must match "+
			"byte for byte, including indentation and line endings.", t.r.display(abs))
	case count > 1 && !in.ReplaceAll:
		return errorf("old_string appears %d times in %s. Add surrounding context to make it "+
			"unique, or set replace_all.", count, t.r.display(abs))
	}

	updated := content
	if in.ReplaceAll {
		updated = strings.ReplaceAll(content, in.OldString, in.NewString)
	} else {
		updated = strings.Replace(content, in.OldString, in.NewString, 1)
	}

	if updated == content {
		return Result{Content: fmt.Sprintf("%s already has the requested contents; no changes made", t.r.display(abs))}, nil
	}
	if err := tx.publish(ctx, []byte(updated), tx.before.mode, nil); err != nil {
		return errorf("cannot write %s: %v", t.r.display(abs), err)
	}

	replaced := 1
	if in.ReplaceAll {
		replaced = count
	}
	return Result{Content: fmt.Sprintf("edited %s (%d replacement(s))", t.r.display(abs), replaced)}, nil
}
