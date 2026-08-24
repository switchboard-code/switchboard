// Package tools implements the built-in tool suite.
//
// The suite stays small on purpose. Everything beyond it arrives over MCP,
// because a one-person project cannot build the long tail but can build the
// socket the long tail plugs into (design principle 5). Phase 0 shipped the
// four tools §19.2 names — read, write, edit, and exec — glob and grep
// joined them so a model can search a tree without shelling out to whatever
// this host happens to have installed, and websearch and webfetch joined so
// it can reach current documentation, under the egress posture web.go
// documents.
package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/switchboard-code/switchboard/internal/execution"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/provider"
)

type Result struct {
	Content string
	IsError bool
}

func errorf(format string, args ...any) (Result, error) {
	return Result{Content: fmt.Sprintf(format, args...), IsError: true}, nil
}

// Plan is a validated tool call that has not run yet. Splitting validation from
// execution lets the caller check policy against the real arguments, so a
// prompt names the actual path or command rather than a raw JSON blob.
type Plan struct {
	Request permission.Request
	Run     func(ctx context.Context) (Result, error)
}

type Tool interface {
	Name() string
	Description() string
	Schema() json.RawMessage

	// ParallelSafe reports whether concurrent calls to this tool can be issued
	// together. Only reads qualify in this suite.
	ParallelSafe() bool

	// Plan parses and validates input. A returned error is a malformed call,
	// which the loop reports back to the model as a tool error so it can
	// correct itself (§10.3).
	Plan(input json.RawMessage) (Plan, error)
}

// ParallelBatchTool is the narrow exception to ParallelSafe's read-only
// rule. Tools with the same non-empty key may overlap with each other, but
// never with ordinary parallel-safe reads or with a different keyed tool.
//
// Delegate uses this because two independent subagents may work concurrently,
// while either one may issue writes internally. Treating delegate as generally
// parallel-safe would let an opaque write race a top-level read in the same
// provider batch and make call order observable.
type ParallelBatchTool interface {
	ParallelBatchKey() string
}

// Registry holds the tool suite for one workspace.
type Registry struct {
	root     string
	rootInfo os.FileInfo

	// displayRoot retains the launch spelling (notably a Windows 8.3 path),
	// while displayPath remembers case-only caller spellings by canonical path.
	// Both are presentation-only: root remains the authority and every state,
	// checkpoint, and filesystem key uses resolve's canonical result.
	displayRoot string
	displayMu   sync.RWMutex
	displayPath map[string]string

	capability execution.Capability
	execution  *execution.Controller
	versions   *fileVersions
	todos      *todoState
	tools      map[string]Tool
	order      []string

	// checkpoints captures and durably publishes every write and edit. Product
	// assembly must set a recorder with an active turn scope; mutating tools fail
	// closed when it is absent or lacks the exact-state publication contract.
	checkpoints Checkpointer

	// images queues what an external tool returned as a picture, for delivery
	// at a round boundary. A branch shares it for the reason it shares the
	// process set: the pictures answer a question this session asked.
	images *toolImages

	// background owns the commands exec started and left running. It is the
	// session's, set at assembly, and stopped when the session ends: a set
	// that outlived its session would be a handle to processes nobody is left
	// to reap. A branch and a subagent share the primary's, because a process
	// started under one is still this program's to stop.
	background *execution.BackgroundSet

	// questioner, when non-nil, is the surface the ask tool resolves
	// questions against. Set at assembly, only by surfaces with a user
	// attached; nil means the tool refuses with the reason.
	questioner Questioner
}

// Checkpointer is what the registry needs from a checkpoint recorder. The
// interface lives here so tools does not import the recorder's package.
type Checkpointer interface {
	Record(abs string)
}

// SetCheckpoints wires the durable recorder in at assembly time. Write and edit
// refuse to mutate until it supports their exact-state publication contract and
// has an active turn scope.
func (r *Registry) SetCheckpoints(c Checkpointer) { r.checkpoints = c }

// ForgetVersions drops the recorded read state for paths whose contents
// changed outside a tool call — an undo. The next write or edit refuses
// until the model re-reads, which is the read-before-write contract doing
// exactly its job.
func (r *Registry) ForgetVersions(paths []string) {
	for _, p := range paths {
		r.versions.forget(p)
	}
}

// ForgetAllVersions drops every recorded read. It belongs to a session
// swap — /clear, /compact, /fork, an in-place /resume — because those
// replace the context the reads lived in, and the resume rationale on
// fileVersions applies unchanged: the agent's knowledge of a file came from
// a context that no longer exists, so it must read again before it may
// overwrite. A registry that remembered reads across the swap would let a
// fresh context write files it has never seen.
func (r *Registry) ForgetAllVersions() { r.versions.forgetAll() }

// ReadPaths lists the files the model has read this session, absolute and
// sorted. It is the same evidence the stale check and the drift sweep use: a
// surface asking what the session has touched should not get a second, subtly
// different answer.
func (r *Registry) ReadPaths() []string {
	r.versions.mu.Lock()
	defer r.versions.mu.Unlock()
	out := make([]string, 0, len(r.versions.seen))
	for path := range r.versions.seen {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

// recordUndo is called by mutating tools before they touch a file.
func (r *Registry) recordUndo(abs string) {
	if r.checkpoints != nil {
		r.checkpoints.Record(abs)
	}
}

// NewRegistry binds the suite to a workspace and to whatever confinement this
// host provides. The capability is carried rather than re-detected so that the
// wrapper the exec tool applies is the same one the permission engine consulted
// when it decided whether approval was needed.
func NewRegistry(workspace string, capability execution.Capability) (*Registry, error) {
	// Preserve the original constructor for embedders: a verified capability is
	// active. Product assembly uses NewRegistryWithExecution so sandbox-off is
	// explicit and the controller is shared with the permission engine.
	controller, _ := execution.NewController(capability, execution.SandboxAuto)
	return NewRegistryWithExecution(workspace, controller)
}

func NewRegistryWithExecution(workspace string, controller *execution.Controller) (*Registry, error) {
	if controller == nil {
		controller = execution.NewDefaultController(execution.Capability{})
	}
	abs, err := filepath.Abs(workspace)
	if err != nil {
		return nil, err
	}
	// The root is resolved once so that every later containment check compares
	// resolved paths against a resolved root.
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolving workspace %s: %w", workspace, err)
	}
	rootInfo, err := bindWorkspaceRootIdentity(root)
	if err != nil {
		return nil, fmt.Errorf("binding workspace %s: %w", workspace, err)
	}

	r := &Registry{
		root:        root,
		displayRoot: filepath.Clean(abs),
		rootInfo:    rootInfo,
		displayPath: map[string]string{},
		capability:  controller.Capability(),
		execution:   controller,
		versions:    newFileVersions(),
		background:  execution.NewBackgroundSet(),
		images:      &toolImages{},
		todos:       &todoState{},
		tools:       map[string]Tool{},
	}
	r.add(&readTool{r})
	r.add(&writeTool{r})
	r.add(&editTool{r})
	r.add(&execTool{r})
	r.add(&procTool{r})
	r.add(&globTool{r})
	r.add(&grepTool{r})
	r.add(&todoTool{r})
	r.add(&askTool{r})
	client := newWebClient()
	r.add(&websearchTool{client: client, endpoint: ddgEndpoint})
	r.add(&webfetchTool{client: client})
	return r, nil
}

// Execution returns the controller shared with branches and delegates.
func (r *Registry) Execution() *execution.Controller { return r.execution }

// StopBackgroundCommands ends everything exec left running and refuses more.
// The surface calls it on the way out: that is the last moment this program
// can be sure those processes are still its own to signal.
func (r *Registry) StopBackgroundCommands() {
	if r.background != nil {
		r.background.StopAll()
	}
}

func (r *Registry) add(t Tool) {
	r.tools[t.Name()] = t
	r.order = append(r.order, t.Name())
	sort.Strings(r.order)
}

// AddExternal registers a tool provided from outside the suite — an MCP
// server's, bridged. It exists for session assembly only: the definitions
// sit in the frozen zone of the context layout (§6.1), so the set must be
// complete before the first request goes out and must not change after.
func (r *Registry) AddExternal(t Tool) error {
	if _, exists := r.tools[t.Name()]; exists {
		return fmt.Errorf("tool %s is already registered", t.Name())
	}
	// A tool that returns pictures needs somewhere to put them, and the
	// registry is the thing that knows whether the bound rung can see one.
	// Wiring it here rather than at construction keeps the bridge from having
	// to be handed a registry it does not otherwise need.
	if sink, ok := t.(interface{ setImageSink(*Registry) }); ok {
		sink.setImageSink(r)
	}
	r.add(t)
	return nil
}

// CoreNames lists the built-in suite, sorted. It exists so assembly can
// validate a configured tool grant — a named agent's — without building a
// registry; the test tying it to NewRegistry is what keeps the two honest.
func CoreNames() []string {
	return []string{"ask", "edit", "exec", "glob", "grep", "proc", "read", "todo", "webfetch", "websearch", "write"}
}

// Restrict narrows the registry to the named tools. Session assembly only,
// for the same frozen-zone reason as AddExternal: a suite that shrinks after
// the first request would invalidate the cached prefix. It can only narrow —
// a name the registry does not hold is an error, never an addition.
func (r *Registry) Restrict(names []string) error {
	keep := map[string]bool{}
	for _, name := range names {
		if _, ok := r.tools[name]; !ok {
			return fmt.Errorf("tool %s is not in the suite", name)
		}
		keep[name] = true
	}
	kept := r.order[:0]
	for _, name := range r.order {
		if keep[name] {
			kept = append(kept, name)
		} else {
			delete(r.tools, name)
		}
	}
	r.order = kept
	return nil
}

func (r *Registry) Root() string { return r.root }

// Resolve and Display expose workspace containment and the relative
// rendering to first-party tools assembled outside this package — the LSP
// pair — so an external tool answers with the same paths and refuses the
// same escapes as the built-in suite.
func (r *Registry) Resolve(path string) (string, error) { return r.resolve(path) }
func (r *Registry) Display(abs string) string           { return r.display(abs) }

func (r *Registry) Get(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Definitions renders the suite for a provider request. The order is
// deterministic because tool definitions sit in the frozen zone of the context
// layout, and a set that reshuffles between requests would invalidate the
// cached prefix on every turn (§6.1).
func (r *Registry) Definitions() []provider.ToolDefinition {
	defs := make([]provider.ToolDefinition, 0, len(r.order))
	for _, name := range r.order {
		t := r.tools[name]
		defs = append(defs, provider.ToolDefinition{
			Name:        t.Name(),
			Description: t.Description(),
			Schema:      t.Schema(),
		})
	}
	return defs
}

// fileVersions records the content hash of every file the agent has read, which
// is what lets a write detect that something else changed the file in between.
//
// The map starts empty on resume. That is correct rather than unfortunate: the
// agent's knowledge of a file came from a context that no longer exists, so it
// must read again before it may overwrite.
type fileVersions struct {
	mu   sync.Mutex
	seen map[string]string

	// stamps is what the file looked like on disk when it was read: size and
	// modification time, so a drift sweep can rule a file out with a stat
	// instead of a read. It is a cheap gate and never evidence on its own —
	// a stamp that differs sends the file to be hashed, and the hash decides.
	stamps map[string]readStamp

	// reported remembers what drift has already been announced, so the same
	// change is not reported at every round boundary. Deliberately separate
	// from seen: seen is what the model was shown and is write and edit's
	// evidence, and a reporter that refreshed it would disarm the refusal
	// that catches this at the point it matters most.
	reported map[string]string

	// whole records the hash of content the model received complete: a full
	// read, uncapped. It backs the read tool's re-injection skip (§6.7) and
	// is deliberately narrower than seen — a partial read updates seen for
	// the stale check while proving nothing about what the context holds, so
	// it must never arm the skip.
	whole map[string]string
}

// newFileVersions is the one place these maps are made, because there are two
// construction sites and a forgotten map here is a nil-map panic on the first
// read rather than an obvious mistake at the call.
func newFileVersions() *fileVersions {
	return &fileVersions{
		seen:     map[string]string{},
		whole:    map[string]string{},
		stamps:   map[string]readStamp{},
		reported: map[string]string{},
	}
}

func (v *fileVersions) record(path, hash string, info os.FileInfo) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.seen[path] = hash
	delete(v.reported, path)
	if info != nil && !info.IsDir() {
		v.stamps[path] = readStamp{size: info.Size(), modTime: info.ModTime()}
	} else {
		delete(v.stamps, path)
	}
}

func (v *fileVersions) get(path string) (string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	h, ok := v.seen[path]
	return h, ok
}

func (v *fileVersions) forget(path string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.seen, path)
	delete(v.whole, path)
	delete(v.stamps, path)
	delete(v.reported, path)
}

func (v *fileVersions) forgetAll() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.seen = map[string]string{}
	v.whole = map[string]string{}
	v.stamps = map[string]readStamp{}
	v.reported = map[string]string{}
}

func (v *fileVersions) recordWhole(path, hash string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.whole[path] = hash
}

func (v *fileVersions) getWhole(path string) (string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	hash, ok := v.whole[path]
	return hash, ok
}

func hashContent(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

var errOutsideWorkspace = errors.New("path is outside the workspace")

// resolve maps a tool-supplied path to an absolute path inside the workspace.
//
// Symlinks are followed before the containment check, because a link inside the
// workspace pointing at /etc is otherwise a boundary that only looks like one.
// Paths that do not exist yet resolve their longest existing ancestor, so
// creating a file through a symlinked directory is checked the same way.
func (r *Registry) resolve(p string) (string, error) {
	if p == "" {
		return "", errors.New("path is required")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(r.root, p)
	}

	resolved, err := resolveExistingPrefix(filepath.Clean(p))
	if err != nil {
		return "", err
	}

	rel, err := filepath.Rel(r.root, resolved)
	if err != nil {
		return "", errOutsideWorkspace
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", errOutsideWorkspace, p)
	}
	r.rememberCaseOnlyDisplay(resolved, filepath.Clean(p))
	return resolved, nil
}

func resolveExistingPrefix(p string) (string, error) {
	var trailing []string
	cur := p
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			parts := append([]string{resolved}, trailing...)
			return filepath.Join(parts...), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		trailing = append([]string{filepath.Base(cur)}, trailing...)
		cur = parent
	}
}

// display renders a path relative to the workspace for prompts and messages.
func (r *Registry) display(abs string) string {
	clean := filepath.Clean(abs)
	r.displayMu.RLock()
	remembered, ok := r.displayPath[clean]
	r.displayMu.RUnlock()
	if ok {
		return remembered
	}
	rel, relErr := filepath.Rel(r.root, abs)
	if relErr == nil && pathIsWithinRoot(rel) {
		return filepath.ToSlash(rel)
	}
	// EvalSymlinks may canonicalize the launch spelling (including a Windows
	// 8.3 name) stored in root. Keep that exact logical root for presentation so
	// language-server paths using the launch spelling remain workspace-relative.
	// This fallback is lexical: rendering an untrusted external path must not
	// probe a UNC share, automount, or any other filesystem authority.
	if r.displayRoot != "" {
		if displayRel, err := filepath.Rel(r.displayRoot, abs); err == nil && pathIsWithinRoot(displayRel) {
			return filepath.ToSlash(displayRel)
		}
	}
	if relErr == nil {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(abs)
}

func (r *Registry) rememberCaseOnlyDisplay(resolved, requested string) {
	if runtime.GOOS != "windows" || resolved == requested || !strings.EqualFold(resolved, requested) {
		return
	}
	rel, err := filepath.Rel(r.root, requested)
	if err != nil || !pathIsWithinRoot(rel) {
		return
	}
	r.displayMu.Lock()
	if r.displayPath == nil {
		r.displayPath = map[string]string{}
	}
	r.displayPath[filepath.Clean(resolved)] = filepath.ToSlash(rel)
	r.displayMu.Unlock()
}

func pathIsWithinRoot(rel string) bool {
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
