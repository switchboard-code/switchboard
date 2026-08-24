package session

import (
	"fmt"
	"path/filepath"
	"sort"
)

// WorkspaceStateDirs returns the canonical per-workspace state directory
// followed by any historical workspace directories whose published session
// records prove that they belong to the same workspace. Historical symlink
// spellings are admitted only through List's identity gate: either a durable
// canonical workspace binding exists, or both live paths still name the same
// directory object.
//
// Callers that will move mutable state out of a historical directory must
// call ValidateWorkspaceStateDir again after taking that state's own locks.
// The second check closes the discovery-to-migration gap for a symlink that is
// removed or retargeted while startup is in progress.
func (s *Store) WorkspaceStateDirs(workspace string) ([]string, error) {
	workspace, err := cleanAbsoluteWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	canonical, err := s.WorkspaceDir(workspace)
	if err != nil {
		return nil, err
	}
	infos, err := s.List(workspace)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{filepath.Clean(canonical): true}
	var historical []string
	for _, info := range infos {
		if info.Health.ReplayLimit {
			// Inventory may name an over-limit log so Resume Doctor can explain
			// it, but a bounded prefix is not authority for migrating mutable
			// workspace state.
			continue
		}
		dir := filepath.Clean(filepath.Dir(info.Path))
		if seen[dir] {
			continue
		}
		seen[dir] = true
		historical = append(historical, dir)
	}
	sort.Strings(historical)
	return append([]string{canonical}, historical...), nil
}

// ValidateWorkspaceStateDir re-proves that dir is either the canonical state
// directory for workspace or contains a published session whose validated
// identity currently binds it to workspace. It is intentionally read-only so
// a state owner can call it while holding its own migration locks.
func (s *Store) ValidateWorkspaceStateDir(workspace, dir string) error {
	workspace, err := cleanAbsoluteWorkspace(workspace)
	if err != nil {
		return err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	dir = filepath.Clean(dir)
	canonical, err := filepath.Abs(filepath.Join(s.root, workspaceKey(workspace)))
	if err != nil {
		return err
	}
	canonical = filepath.Clean(canonical)
	if dir == canonical {
		return nil
	}
	infos, err := s.List(workspace)
	if err != nil {
		return err
	}
	for _, info := range infos {
		if info.Health.ReplayLimit {
			continue
		}
		if filepath.Clean(filepath.Dir(info.Path)) == dir {
			return nil
		}
	}
	return fmt.Errorf("workspace state directory %q no longer has a published session proving identity with %q", dir, workspace)
}

// WorkspaceAliases returns the canonical workspace followed by historical
// SessionStart spellings whose published logs currently prove the same
// identity. Unlike WorkspaceStateDirs, callers use these values to locate
// state keyed directly from the old path string, such as prompt history.
func (s *Store) WorkspaceAliases(workspace string) ([]string, error) {
	workspace, err := cleanAbsoluteWorkspace(workspace)
	if err != nil {
		return nil, err
	}
	infos, err := s.List(workspace)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{workspace: true}
	var historical []string
	for _, info := range infos {
		if info.Health.ReplayLimit {
			continue
		}
		checked, candidateErr := s.validateCandidate(info.Path, candidateExpectation{id: info.ID, workspace: workspace})
		if candidateErr != nil && !checked.blockedByCorruption {
			// The candidate changed after List's read-only snapshot. Omitting its
			// alias is the fail-closed direction; the mutable state's own locked
			// revalidation repeats this check before migration.
			continue
		}
		alias := checked.start.Workspace
		if alias == "" || seen[alias] {
			continue
		}
		seen[alias] = true
		historical = append(historical, alias)
	}
	sort.Strings(historical)
	return append([]string{workspace}, historical...), nil
}

// ValidateWorkspaceAlias re-proves one historical path spelling. A durable
// workspace_binding remains authoritative after the obsolete alias is gone;
// an unbound alias must still name the same live directory as workspace.
func (s *Store) ValidateWorkspaceAlias(workspace, alias string) error {
	workspace, err := cleanAbsoluteWorkspace(workspace)
	if err != nil {
		return err
	}
	alias, err = cleanAbsoluteWorkspace(alias)
	if err != nil {
		return err
	}
	if alias == workspace {
		return nil
	}
	aliases, err := s.WorkspaceAliases(workspace)
	if err != nil {
		return err
	}
	for _, candidate := range aliases[1:] {
		if candidate == alias {
			return nil
		}
	}
	return fmt.Errorf("historical workspace alias %q no longer has a published session proving identity with %q", alias, workspace)
}
