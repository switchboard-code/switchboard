package lsp

import (
	"context"
	"errors"
	"fmt"

	workspacefs "github.com/switchboard-code/switchboard/internal/workspace"
)

// Keeping the raw document below half the frame cap leaves room for JSON
// escaping and request metadata. In particular, a hostile or accidental huge
// file cannot turn one semantic lookup into unbounded allocation.
const maxDocumentBytes = maxLSPMessageBytes / 2

// readDocumentSnapshot is the only disk-reader used by document sync. The
// authority was bound to the exact workspace directory identity before the
// language server started. Keeping that capability separate from path is what
// makes retained document names safe: an ancestor replaced by a symlink cannot
// turn a later workspace/symbol reconciliation into a host-file read.
//
// The one returned byte slice feeds both didOpen/didChange and position
// resolution.
func readDocumentSnapshot(ctx context.Context, authority *workspacefs.Root, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if authority == nil {
		return nil, fmt.Errorf("language-server workspace authority is unavailable")
	}
	document, err := authority.Read(path, maxDocumentBytes)
	if err != nil {
		switch {
		case errors.Is(err, workspacefs.ErrTooLarge):
			return nil, fmt.Errorf("%s exceeds the language-server document limit of %d bytes: %w", path, maxDocumentBytes, err)
		case errors.Is(err, workspacefs.ErrBinary):
			return nil, fmt.Errorf("%s is not valid UTF-8 text: %w", path, err)
		}
		return nil, fmt.Errorf("reading language-server document %s: %w", path, err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return document.Content, nil
}

func (c *Client) readDocumentSnapshot(ctx context.Context, path string) ([]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("language-server client is unavailable")
	}
	if c.documentRootErr != nil {
		return nil, fmt.Errorf("language-server workspace authority is unavailable: %w", c.documentRootErr)
	}
	return readDocumentSnapshot(ctx, c.documentRoot, path)
}

func documentAuthorityChanged(err error) bool {
	return errors.Is(err, workspacefs.ErrOutsideRoot) ||
		errors.Is(err, workspacefs.ErrStaleLocation) ||
		errors.Is(err, workspacefs.ErrSecureReadUnsupported)
}
