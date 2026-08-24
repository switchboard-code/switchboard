package extensions

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/switchboard-code/switchboard/internal/checkpoint"
	"github.com/switchboard-code/switchboard/internal/fileprivacy"
)

// StateFileName is deliberately separate from config.toml. The TUI rewrites
// config.toml from typed settings, while plugin activation is a security
// decision with its own lifecycle and must not disappear during an unrelated
// settings save.
const StateFileName = "plugins.json"

const maxStateBytes = 1 << 20

var pluginStateBeforePublicationTestHook func()

// Activation is Switchboard's decision about one exact native plugin. A
// native client's enabled or trusted state is never copied here. RealPath is
// part of the identity so an equal namespace installed elsewhere cannot
// inherit the decision.
type Activation struct {
	ID          string    `json:"id"`
	Dialect     Dialect   `json:"dialect"`
	NativeIDs   []string  `json:"native_ids,omitempty"`
	Scope       Scope     `json:"scope"`
	RealPath    string    `json:"real_path"`
	Workspace   string    `json:"workspace,omitempty"`
	Enabled     bool      `json:"enabled"`
	EnabledAt   time.Time `json:"enabled_at,omitempty"`
	TrustDigest string    `json:"executable_trust_digest,omitempty"`
	TrustedAt   time.Time `json:"executable_trusted_at,omitempty"`
}

// ActivationStatus joins persisted state to the plugin bytes discovered for
// this session. Changed is true when executable trust existed but the bounded
// plugin-tree digest no longer matches it.
type ActivationStatus struct {
	Enabled           bool
	ExecutableTrusted bool
	Changed           bool
}

// ActivationReference is a non-secret recovery view of a persisted decision.
// RecoveryToken is a keyed selector over the complete activation identity and
// can safely distinguish otherwise ambiguous stale records without exposing
// the state key or creating an offline path-identity oracle.
type ActivationReference struct {
	Activation    Activation
	RecoveryToken string
}

// State is the persisted activation ledger. Its methods are safe for
// concurrent TUI and assembly diagnostics, though normal mutation happens at
// command boundaries.
type State struct {
	path string

	mu      sync.Mutex
	key     []byte
	records map[string]Activation
}

// StatePath returns the per-user ledger path. A repository cannot edit this
// file and thereby enable or trust itself.
func StatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".switchboard", StateFileName), nil
}

// OpenState opens the default per-user ledger.
func OpenState() (*State, error) {
	path, err := StatePath()
	if err != nil {
		return nil, fmt.Errorf("no home directory for plugin state: %w", err)
	}
	return OpenStateFile(path)
}

// OpenStateFile opens an explicit ledger, primarily for tests. Missing state
// is empty. Malformed, duplicate, or oversized input fails closed rather than
// partially applying security decisions.
func OpenStateFile(path string) (*State, error) {
	directory, err := openPluginStateDirectory(path)
	if err != nil {
		return nil, err
	}
	defer directory.close()
	snapshot, err := readPluginStateSnapshot(directory)
	if err != nil {
		return nil, err
	}
	return decodePluginState(directory.statePath(), snapshot)
}

func decodePluginState(path string, snapshot pluginStateSnapshot) (*State, error) {
	s := &State{path: path, records: make(map[string]Activation)}
	if !snapshot.existed {
		return s, nil
	}
	raw := snapshot.content
	if err := rejectDuplicateJSONKeys(raw); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var file struct {
		Version     int          `json:"version"`
		Key         string       `json:"key,omitempty"`
		Activations []Activation `json:"activations"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if file.Version != 1 && file.Version != 2 {
		return nil, fmt.Errorf("reading %s: unsupported plugin state version %d", path, file.Version)
	}
	if file.Version == 2 {
		key, decodeErr := base64.RawStdEncoding.DecodeString(file.Key)
		if decodeErr != nil || len(key) != sha256.Size {
			return nil, fmt.Errorf("reading %s: plugin recovery key is invalid", path)
		}
		s.key = append([]byte(nil), key...)
	}
	for _, activation := range file.Activations {
		if err := validateActivation(activation); err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		key := activationKey(activation.ID, activation.Dialect, activation.Scope, activation.RealPath, activation.Workspace)
		if _, exists := s.records[key]; exists {
			return nil, fmt.Errorf("reading %s: duplicate plugin activation for %s", path, activation.ID)
		}
		s.records[key] = activation
	}
	return s, nil
}

// Status evaluates one discovered plugin against this ledger. Trust is useful
// only for plugins with executable components, while a stale trust digest is
// still reported for diagnostics.
func (s *State) Status(plugin Plugin, workspace string) ActivationStatus {
	workspace, err := activationWorkspace(plugin.Scope, workspace)
	if err != nil {
		return ActivationStatus{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[activationKey(plugin.ID, plugin.Dialect, plugin.Scope, plugin.RealPath, workspace)]
	if !ok || !record.Enabled {
		return ActivationStatus{}
	}
	changed := record.TrustDigest != "" && record.TrustDigest != plugin.Digest
	return ActivationStatus{
		Enabled:           true,
		ExecutableTrusted: plugin.Executable && record.TrustDigest != "" && !changed,
		Changed:           changed,
	}
}

// Enable exposes an eligible installed plugin to Switchboard on the next
// frozen-zone assembly. It intentionally does not grant executable trust.
// Plain discovered catalog inventory cannot be passed to this method.
func (s *State) Enable(candidate *ActivationCandidate, workspace string) error {
	return s.EnableWithNativeIDsContext(context.Background(), candidate, workspace, nil)
}

// EnableWithNativeIDs records the exact native identities proven by the
// activation source. These identities are provenance, never authority, but
// persist so a later managed deny still constrains the cached copy after its
// native source changes or disappears.
func (s *State) EnableWithNativeIDs(candidate *ActivationCandidate, workspace string, nativeIDs []string) error {
	return s.EnableWithNativeIDsContext(context.Background(), candidate, workspace, nativeIDs)
}

func (s *State) EnableWithNativeIDsContext(ctx context.Context, candidate *ActivationCandidate, workspace string, nativeIDs []string) error {
	plugin, err := candidate.currentPlugin()
	if err != nil {
		return err
	}
	workspace, err = activationWorkspace(plugin.Scope, workspace)
	if err != nil {
		return err
	}
	return s.mutate(ctx, func(latest *State) (bool, error) {
		key := activationKey(plugin.ID, plugin.Dialect, plugin.Scope, plugin.RealPath, workspace)
		before := latest.records[key]
		record := before
		record.ID = plugin.ID
		record.Dialect = plugin.Dialect
		record.NativeIDs = normalizedActivationNativeIDs(nativeIDs)
		if len(record.NativeIDs) == 0 && len(before.NativeIDs) != 0 {
			record.NativeIDs = append([]string(nil), before.NativeIDs...)
		}
		record.Scope = plugin.Scope
		record.RealPath = plugin.RealPath
		record.Workspace = workspace
		record.Enabled = true
		if record.EnabledAt.IsZero() {
			record.EnabledAt = time.Now().UTC()
		}
		latest.records[key] = record
		return true, nil
	})
}

// Disable removes activation and executable trust for one exact plugin.
func (s *State) Disable(plugin Plugin, workspace string) error {
	return s.DisableContext(context.Background(), plugin, workspace)
}

func (s *State) DisableContext(ctx context.Context, plugin Plugin, workspace string) error {
	if err := validatePluginIdentity(plugin); err != nil {
		return err
	}
	workspace, err := activationWorkspace(plugin.Scope, workspace)
	if err != nil {
		return err
	}
	return s.mutate(ctx, func(latest *State) (bool, error) {
		key := activationKey(plugin.ID, plugin.Dialect, plugin.Scope, plugin.RealPath, workspace)
		if _, ok := latest.records[key]; !ok {
			return false, nil
		}
		delete(latest.records, key)
		return true, nil
	})
}

// TrustExecutable grants execution only for the bytes currently discovered.
// A changed digest makes Status fail closed until this method is called again
// after the user has reviewed the update.
func (s *State) TrustExecutable(candidate *ActivationCandidate, workspace string) error {
	return s.TrustExecutableContext(context.Background(), candidate, workspace)
}

func (s *State) TrustExecutableContext(ctx context.Context, candidate *ActivationCandidate, workspace string) error {
	plugin, err := candidate.currentPlugin()
	if err != nil {
		return err
	}
	if !plugin.Executable {
		return errors.New("plugin has no executable components")
	}
	if !validDigest(plugin.Digest) {
		return errors.New("plugin has no valid content digest")
	}
	workspace, err = activationWorkspace(plugin.Scope, workspace)
	if err != nil {
		return err
	}
	return s.mutate(ctx, func(latest *State) (bool, error) {
		key := activationKey(plugin.ID, plugin.Dialect, plugin.Scope, plugin.RealPath, workspace)
		record, ok := latest.records[key]
		if !ok || !record.Enabled {
			return false, errors.New("plugin must be enabled before executable trust is granted")
		}
		record.TrustDigest = plugin.Digest
		record.TrustedAt = time.Now().UTC()
		latest.records[key] = record
		return true, nil
	})
}

// RevokeExecutable withdraws executable trust without disabling prompt-only
// components such as skills.
func (s *State) RevokeExecutable(plugin Plugin, workspace string) error {
	return s.RevokeExecutableContext(context.Background(), plugin, workspace)
}

func (s *State) RevokeExecutableContext(ctx context.Context, plugin Plugin, workspace string) error {
	if err := validatePluginIdentity(plugin); err != nil {
		return err
	}
	workspace, err := activationWorkspace(plugin.Scope, workspace)
	if err != nil {
		return err
	}
	return s.mutate(ctx, func(latest *State) (bool, error) {
		key := activationKey(plugin.ID, plugin.Dialect, plugin.Scope, plugin.RealPath, workspace)
		record, ok := latest.records[key]
		if !ok || record.TrustDigest == "" {
			return false, nil
		}
		record.TrustDigest = ""
		record.TrustedAt = time.Time{}
		latest.records[key] = record
		return true, nil
	})
}

// Activations lists persisted decisions deterministically for diagnostics.
func (s *State) Activations() []Activation {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Activation, 0, len(s.records))
	for _, record := range s.records {
		out = append(out, cloneActivation(record))
	}
	sortActivations(out)
	return out
}

// ActivationsFor returns global activations plus project/local activations
// bound to this exact resolved workspace. A project plugin enabled in one
// checkout must never become inventory merely because another checkout can
// still reach its installation path.
func (s *State) ActivationsFor(workspace string) ([]Activation, error) {
	resolved, err := activationWorkspace(ScopeWorkspace, workspace)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Activation, 0, len(s.records))
	for _, record := range s.records {
		switch record.Scope {
		case ScopeUser, ScopeManaged:
			out = append(out, cloneActivation(record))
		case ScopeWorkspace, ScopeLocal:
			if filepath.Clean(record.Workspace) == resolved {
				out = append(out, cloneActivation(record))
			}
		}
	}
	sortActivations(out)
	return out, nil
}

// ActivationReferencesFor returns the same workspace-filtered decisions as
// ActivationsFor plus stable keyed recovery selectors. Existing version-1
// state is migrated under the mutation lock before selectors are returned, so
// selectors survive restarts without racing another writer.
func (s *State) ActivationReferencesFor(workspace string) ([]ActivationReference, error) {
	resolved, err := activationWorkspace(ScopeWorkspace, workspace)
	if err != nil {
		return nil, err
	}
	if err := s.ensurePersistedRecoveryKey(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var activations []Activation
	for _, record := range s.records {
		switch record.Scope {
		case ScopeUser, ScopeManaged:
			activations = append(activations, cloneActivation(record))
		case ScopeWorkspace, ScopeLocal:
			if filepath.Clean(record.Workspace) == resolved {
				activations = append(activations, cloneActivation(record))
			}
		}
	}
	sortActivations(activations)
	if len(activations) == 0 {
		return nil, nil
	}
	if len(s.key) != sha256.Size {
		return nil, errors.New("plugin recovery key is unavailable")
	}
	references := make([]ActivationReference, 0, len(activations))
	for _, activation := range activations {
		references = append(references, ActivationReference{
			Activation:    activation,
			RecoveryToken: activationRecoveryToken(s.key, activation),
		})
	}
	return references, nil
}

func cloneActivation(activation Activation) Activation {
	activation.NativeIDs = append([]string(nil), activation.NativeIDs...)
	return activation
}

// DisableActivation removes one exact saved decision. It is the recovery path
// when current plugin bytes can no longer be parsed into a Plugin value.
func (s *State) DisableActivation(activation Activation) error {
	return s.DisableActivationContext(context.Background(), activation)
}

func (s *State) DisableActivationContext(ctx context.Context, activation Activation) error {
	if err := validateActivation(activation); err != nil {
		return err
	}
	return s.mutate(ctx, func(latest *State) (bool, error) {
		key := activationKey(activation.ID, activation.Dialect, activation.Scope, activation.RealPath, activation.Workspace)
		if _, ok := latest.records[key]; !ok {
			return false, nil
		}
		delete(latest.records, key)
		return true, nil
	})
}

// RevokeActivationTrust withdraws executable trust for one exact saved
// decision without requiring its current bytes to remain readable.
func (s *State) RevokeActivationTrust(activation Activation) error {
	return s.RevokeActivationTrustContext(context.Background(), activation)
}

func (s *State) RevokeActivationTrustContext(ctx context.Context, activation Activation) error {
	if err := validateActivation(activation); err != nil {
		return err
	}
	return s.mutate(ctx, func(latest *State) (bool, error) {
		key := activationKey(activation.ID, activation.Dialect, activation.Scope, activation.RealPath, activation.Workspace)
		record, ok := latest.records[key]
		if !ok || record.TrustDigest == "" {
			return false, nil
		}
		record.TrustDigest = ""
		record.TrustedAt = time.Time{}
		latest.records[key] = record
		return true, nil
	})
}

func sortActivations(out []Activation) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		if out[i].Workspace != out[j].Workspace {
			return out[i].Workspace < out[j].Workspace
		}
		return out[i].RealPath < out[j].RealPath
	})
}

func validatePluginIdentity(plugin Plugin) error {
	if plugin.ID == "" || plugin.RealPath == "" || plugin.Dialect == "" || plugin.Scope == "" {
		return errors.New("plugin identity is incomplete")
	}
	if hasControl(plugin.ID) {
		return errors.New("plugin ID contains a control character")
	}
	if !filepath.IsAbs(plugin.RealPath) {
		return errors.New("plugin real path must be absolute")
	}
	if plugin.Dialect != DialectCodex && plugin.Dialect != DialectClaude {
		return fmt.Errorf("plugin has unsupported dialect %q", plugin.Dialect)
	}
	switch plugin.Scope {
	case ScopeUser, ScopeManaged, ScopeWorkspace, ScopeLocal:
	default:
		return fmt.Errorf("plugin has unsupported scope %q", plugin.Scope)
	}
	if !strings.HasPrefix(plugin.ID, string(plugin.Dialect)+":") {
		return errors.New("plugin ID does not match its dialect")
	}
	return nil
}

func validateActivation(activation Activation) error {
	if activation.ID == "" || activation.RealPath == "" || activation.Dialect == "" || activation.Scope == "" {
		return errors.New("plugin activation identity is incomplete")
	}
	if hasControl(activation.ID) {
		return errors.New("plugin activation ID contains a control character")
	}
	if !filepath.IsAbs(activation.RealPath) {
		return fmt.Errorf("plugin activation %s has a non-absolute real path", activation.ID)
	}
	if activation.Dialect != DialectCodex && activation.Dialect != DialectClaude {
		return fmt.Errorf("plugin activation %s has unsupported dialect %q", activation.ID, activation.Dialect)
	}
	if !strings.HasPrefix(activation.ID, string(activation.Dialect)+":") {
		return fmt.Errorf("plugin activation %s does not match dialect %s", activation.ID, activation.Dialect)
	}
	seenNativeIDs := make(map[string]struct{}, len(activation.NativeIDs))
	for _, nativeID := range activation.NativeIDs {
		if strings.TrimSpace(nativeID) == "" || hasControl(nativeID) {
			return fmt.Errorf("plugin activation %s has an invalid native ID", activation.ID)
		}
		if _, duplicate := seenNativeIDs[nativeID]; duplicate {
			return fmt.Errorf("plugin activation %s repeats native ID %q", activation.ID, nativeID)
		}
		seenNativeIDs[nativeID] = struct{}{}
	}
	switch activation.Scope {
	case ScopeUser, ScopeManaged:
		if activation.Workspace != "" {
			return fmt.Errorf("global plugin activation %s unexpectedly names a workspace", activation.ID)
		}
	case ScopeWorkspace, ScopeLocal:
		if activation.Workspace == "" || !filepath.IsAbs(activation.Workspace) {
			return fmt.Errorf("project plugin activation %s has no absolute workspace", activation.ID)
		}
	default:
		return fmt.Errorf("plugin activation %s has unsupported scope %q", activation.ID, activation.Scope)
	}
	if activation.TrustDigest != "" && !validDigest(activation.TrustDigest) {
		return fmt.Errorf("plugin activation %s has an invalid trust digest", activation.ID)
	}
	if activation.TrustDigest != "" && !activation.Enabled {
		return fmt.Errorf("plugin activation %s trusts execution while disabled", activation.ID)
	}
	return nil
}

func normalizedActivationNativeIDs(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	deduplicated := out[:0]
	for _, value := range out {
		value = strings.TrimSpace(value)
		if value == "" || hasControl(value) {
			continue
		}
		if len(deduplicated) == 0 || deduplicated[len(deduplicated)-1] != value {
			deduplicated = append(deduplicated, value)
		}
	}
	return deduplicated
}

func validDigest(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	for _, r := range digest {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') {
			continue
		}
		return false
	}
	return true
}

func activationWorkspace(scope Scope, workspace string) (string, error) {
	switch scope {
	case ScopeUser, ScopeManaged:
		return "", nil
	case ScopeWorkspace, ScopeLocal:
		if strings.TrimSpace(workspace) == "" {
			return "", errors.New("project-scoped plugin activation requires a workspace")
		}
		abs, err := filepath.Abs(workspace)
		if err != nil {
			return "", fmt.Errorf("resolving plugin activation workspace: %w", err)
		}
		real, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return "", fmt.Errorf("resolving plugin activation workspace: %w", err)
		}
		info, err := os.Stat(real)
		if err != nil || !info.IsDir() {
			return "", errors.New("plugin activation workspace is not a directory")
		}
		return filepath.Clean(real), nil
	default:
		return "", fmt.Errorf("unsupported plugin scope %q", scope)
	}
}

func activationKey(id string, dialect Dialect, scope Scope, realPath, workspace string) string {
	return strings.Join([]string{id, string(dialect), string(scope), filepath.Clean(realPath), filepath.Clean(workspace)}, "\x00")
}

func activationRecoveryToken(key []byte, activation Activation) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("switchboard/plugin-recovery-selector/v1\x00"))
	for _, value := range []string{
		activation.ID, string(activation.Dialect), string(activation.Scope),
		filepath.Clean(activation.RealPath), filepath.Clean(activation.Workspace),
	} {
		var size [8]byte
		binary.BigEndian.PutUint64(size[:], uint64(len(value)))
		_, _ = mac.Write(size[:])
		_, _ = mac.Write([]byte(value))
	}
	return "saved:" + fmt.Sprintf("%x", mac.Sum(nil))
}

func (s *State) ensureRecoveryKeyLocked() error {
	if len(s.key) == sha256.Size {
		return nil
	}
	key := make([]byte, sha256.Size)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	s.key = key
	return nil
}

func (s *State) ensurePersistedRecoveryKey() error {
	s.mu.Lock()
	ready := len(s.key) == sha256.Size || len(s.records) == 0
	s.mu.Unlock()
	if ready {
		return nil
	}
	return s.mutate(context.Background(), func(latest *State) (bool, error) {
		if len(latest.records) == 0 || len(latest.key) == sha256.Size {
			return false, nil
		}
		if err := latest.ensureRecoveryKeyLocked(); err != nil {
			return false, err
		}
		return true, nil
	})
}

// mutate serializes writers across processes, reloads the latest validated
// state while holding the lock, applies exactly one requested change, and
// only then publishes a complete replacement. The receiver adopts the latest
// snapshot after success so a stale handle cannot resurrect a decision that
// another process already removed.
func (s *State) mutate(ctx context.Context, apply func(*State) (bool, error)) error {
	if s == nil {
		return errors.New("plugin activation state is unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	directory, err := openPluginStateDirectory(s.path)
	if err != nil {
		return err
	}
	defer directory.close()
	lock, err := acquirePluginStateLockInDirectory(ctx, directory)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkpoint.RecoverFilePublicationCleanupBound(
		directory.journalPath, directory.path, directory.journalRoot, directory.root,
	); err != nil {
		return fmt.Errorf("recovering interrupted plugin state publication: %w", err)
	}
	if err := directory.validateLinked(); err != nil {
		return err
	}
	snapshot, err := readPluginStateSnapshot(directory)
	if err != nil {
		return err
	}
	latest, err := decodePluginState(directory.statePath(), snapshot)
	if err != nil {
		return err
	}
	changed, err := apply(latest)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	published := false
	if changed {
		raw, err := latest.encodeState()
		if err != nil {
			return err
		}
		if err := directory.validateLinked(); err != nil {
			return err
		}
		published, err = checkpoint.PublishStandaloneFileCASBound(
			ctx,
			directory.journalPath,
			directory.path,
			directory.journalRoot,
			directory.root,
			directory.statePath(),
			directory.root,
			directory.name,
			snapshot.existed,
			snapshot.mode,
			snapshot.content,
			0o600,
			raw,
			maxStateBytes,
			fileprivacy.Secure,
			pluginStateBeforePublicationTestHook,
		)
		linkedErr := directory.validateLinked()
		if published {
			s.key = append(s.key[:0], latest.key...)
			s.records = cloneActivationMap(latest.records)
		}
		if err != nil || linkedErr != nil {
			return errors.Join(err, linkedErr)
		}
	}
	if !changed {
		if err := directory.validateLinked(); err != nil {
			return err
		}
	}
	if !changed || !published {
		s.key = append(s.key[:0], latest.key...)
		s.records = cloneActivationMap(latest.records)
	}
	return nil
}

func cloneActivationMap(records map[string]Activation) map[string]Activation {
	cloned := make(map[string]Activation, len(records))
	for key, activation := range records {
		cloned[key] = cloneActivation(activation)
	}
	return cloned
}

func (s *State) encodeState() ([]byte, error) {
	if err := s.ensureRecoveryKeyLocked(); err != nil {
		return nil, fmt.Errorf("creating plugin recovery key: %w", err)
	}
	file := struct {
		Version     int          `json:"version"`
		Key         string       `json:"key"`
		Activations []Activation `json:"activations"`
	}{Version: 2, Key: base64.RawStdEncoding.EncodeToString(s.key)}
	for _, record := range s.records {
		file.Activations = append(file.Activations, record)
	}
	sort.Slice(file.Activations, func(i, j int) bool {
		if file.Activations[i].ID != file.Activations[j].ID {
			return file.Activations[i].ID < file.Activations[j].ID
		}
		return file.Activations[i].RealPath < file.Activations[j].RealPath
	})

	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(file); err != nil {
		return nil, err
	}
	if encoded.Len() > maxStateBytes {
		return nil, fmt.Errorf("plugin state exceeds %d bytes", maxStateBytes)
	}
	return append([]byte(nil), encoded.Bytes()...), nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == io.EOF {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("unexpected JSON value after plugin state")
}

func rejectDuplicateJSONKeys(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	var value func() error
	value = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delim {
		case '{':
			seen := make(map[string]struct{})
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return errors.New("JSON object contains a non-string key")
				}
				if _, duplicate := seen[key]; duplicate {
					return fmt.Errorf("JSON object contains duplicate key %q", key)
				}
				seen[key] = struct{}{}
				if err := value(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := value(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return fmt.Errorf("unexpected JSON delimiter %q", delim)
		}
	}
	if err := value(); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}
