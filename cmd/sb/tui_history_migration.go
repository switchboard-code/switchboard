package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/switchboard-code/switchboard/internal/session"
)

var errHistoryMigrationConflict = errors.New("prompt history migration conflict")

type historyMigrationSource struct {
	workspace string
	path      string
}

type historyMigrationHooks struct {
	beforeRemove func(path string)
}

type historyMigrationImage struct {
	path         string
	entries      []string
	canonical    []byte
	snapshot     historySnapshot
	needsRewrite bool
}

func workspaceHistoryMigrationSources(store *session.Store, workspace string) (string, []historyMigrationSource, error) {
	workspace, err := filepath.Abs(workspace)
	if err != nil {
		return "", nil, err
	}
	workspace = filepath.Clean(workspace)
	canonicalPath, err := historyPath(workspace)
	if err != nil {
		return "", nil, err
	}
	if store == nil {
		return canonicalPath, nil, nil
	}
	aliases, err := store.WorkspaceAliases(workspace)
	if err != nil {
		return "", nil, err
	}
	seen := map[string]bool{filepath.Clean(canonicalPath): true}
	var sources []historyMigrationSource
	for _, alias := range aliases {
		if alias == workspace {
			continue
		}
		path, pathErr := historyPath(alias)
		if pathErr != nil {
			return "", nil, pathErr
		}
		path = filepath.Clean(path)
		if seen[path] {
			continue
		}
		seen[path] = true
		sources = append(sources, historyMigrationSource{workspace: alias, path: path})
	}
	return canonicalPath, sources, nil
}

func migrateWorkspaceHistory(store *session.Store, workspace string) error {
	canonicalPath, sources, err := workspaceHistoryMigrationSources(store, workspace)
	if err != nil || len(sources) == 0 {
		return err
	}
	return migrateWorkspaceHistorySources(store, workspace, canonicalPath, sources, historyMigrationHooks{})
}

// migrateWorkspaceHistorySources is split from discovery so tests can prove
// the discovery-to-lock race: a symlink removed or retargeted after source
// enumeration is revalidated only after every source and destination sidecar
// is locked, and no file is touched when that proof fails.
func migrateWorkspaceHistorySources(store *session.Store, workspace, canonicalPath string, sources []historyMigrationSource, hooks historyMigrationHooks) error {
	if store == nil {
		return errors.New("prompt history migration has no session identity store")
	}
	canonicalPath = filepath.Clean(canonicalPath)
	paths := []string{canonicalPath}
	seen := map[string]bool{canonicalPath: true}
	normalized := make([]historyMigrationSource, 0, len(sources))
	for _, source := range sources {
		source.path = filepath.Clean(source.path)
		if source.workspace == "" || seen[source.path] {
			continue
		}
		seen[source.path] = true
		normalized = append(normalized, source)
		paths = append(paths, source.path)
	}
	sources = normalized
	if len(sources) == 0 {
		return nil
	}
	return withHistoryLocks(paths, true, func(parents map[string]os.FileInfo) error {
		canonical, err := readHistoryMigrationImage(canonicalPath, parents[canonicalPath])
		if err != nil {
			return err
		}
		baseline := canonical
		var historical []*historyMigrationImage
		for _, source := range sources {
			source.path = filepath.Clean(source.path)
			exists, err := historyDataExists(source.path)
			if err != nil {
				return err
			}
			if !exists {
				continue
			}
			if err := store.ValidateWorkspaceAlias(workspace, source.workspace); err != nil {
				return fmt.Errorf("revalidating prompt history workspace identity for %s: %w", source.path, err)
			}
			image, err := readHistoryMigrationImage(source.path, parents[source.path])
			if err != nil {
				return err
			}
			if image == nil {
				continue
			}
			historical = append(historical, image)
			if baseline == nil {
				baseline = image
				continue
			}
			if !bytes.Equal(baseline.canonical, image.canonical) {
				return fmt.Errorf("%w: refusing to choose between non-equivalent histories %s and %s", errHistoryMigrationConflict, baseline.path, image.path)
			}
		}
		if len(historical) == 0 {
			return nil
		}

		entries := append([]string(nil), baseline.entries...)
		switch {
		case canonical == nil:
			if err := createHistoryLocked(canonicalPath, entries, parents[canonicalPath]); err != nil {
				return fmt.Errorf("publishing migrated prompt history %s: %w", canonicalPath, err)
			}
		case canonical.needsRewrite:
			if err := rewriteHistoryLocked(canonicalPath, entries, canonical.snapshot, parents[canonicalPath], nil); err != nil {
				return fmt.Errorf("publishing private normalized prompt history %s: %w", canonicalPath, err)
			}
		}
		// The canonical bytes and their owner-only ACL are durable before any
		// legacy name is removed. Cleanup rechecks descriptor identity and bytes;
		// a replacement loses the race rather than being deleted.
		for _, image := range historical {
			var beforeRemove func()
			if hooks.beforeRemove != nil {
				path := image.path
				beforeRemove = func() { hooks.beforeRemove(path) }
			}
			if err := removeHistoryIfUnchangedLocked(image.path, image.snapshot, parents[image.path], beforeRemove); err != nil {
				return fmt.Errorf("removing migrated prompt history %s: %w", image.path, err)
			}
		}
		return nil
	})
}

func historyDataExists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return false, fmt.Errorf("prompt history %s is not a regular file", path)
		}
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func readHistoryMigrationImage(path string, parent os.FileInfo) (*historyMigrationImage, error) {
	exists, err := historyDataExists(path)
	if err != nil || !exists {
		return nil, err
	}
	data, snapshot, err := readHistoryFileLocked(path, parent, nil)
	if err != nil {
		return nil, err
	}
	entries, needsRewrite := decodeHistory(data, snapshot.ownerOnly)
	canonical, err := encodeHistory(entries)
	if err != nil {
		return nil, err
	}
	return &historyMigrationImage{
		path: path, entries: entries, canonical: canonical, snapshot: snapshot, needsRewrite: needsRewrite,
	}, nil
}
