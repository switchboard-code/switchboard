package main

// MCP servers join the session at assembly time, before the first request,
// because the tool definitions sit in the frozen zone (§6.1). Connection is
// parallel with an independent deadline per server: one server that cannot say
// hello in time is reported and left behind without serially stalling peers.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/switchboard-code/switchboard/internal/mcp"
	"github.com/switchboard-code/switchboard/internal/permission"
	"github.com/switchboard-code/switchboard/internal/tools"
	"github.com/switchboard-code/switchboard/internal/trust"
)

const mcpConnectTimeout = 15 * time.Second

type mcpNote struct {
	level string
	text  string
}

// maxBufferedNotes bounds what accumulates while no surface is listening. A
// chatty server must not grow memory forever into a buffer nobody reads.
const maxBufferedNotes = 200

// mcpState is what the session keeps of its MCP wiring: the live clients for
// /mcp and shutdown, and the notes those clients produce. Notes arrive from
// the connect goroutines and from every client's read loop for as long as
// the session lives, while the surfaces and main append and read their own,
// so every access goes through the mutex here rather than through whatever
// lock a caller happens to hold.
type mcpState struct {
	mu      sync.Mutex
	clients []*mcp.Client
	notes   []mcpNote
	dropped int
	deliver func(mcpNote)
}

// add records a note, or hands it straight to the surface once one attached.
func (s *mcpState) add(n mcpNote) {
	s.mu.Lock()
	d := s.deliver
	if d == nil {
		if len(s.notes) < maxBufferedNotes {
			s.notes = append(s.notes, n)
		} else {
			s.dropped++
		}
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	// Delivery happens outside the lock: the TUI's deliver blocks until the
	// program's event loop consumes it, and holding the lock across that
	// would stall every client's read loop behind one paint.
	d(n)
}

// attach registers where later notes go and returns what buffered before the
// surface existed. If that bounded buffer overflowed, the final returned note
// discloses exactly how many diagnostics could not be retained. A surface that
// is not yet running cannot be delivered to without deadlocking its own setup.
func (s *mcpState) attach(d func(mcpNote)) []mcpNote {
	buffered, dropped := s.attachCounted(d)
	if dropped > 0 {
		buffered = append(buffered, startupNoteOverflowDisclosure(dropped))
	}
	return buffered
}

// attachCounted is the startup-report seam: it keeps the bounded record and
// its loss count separate so the summary can distinguish observed diagnostics
// from retained detail. Callers that only need notes use attach, which appends
// the same disclosure as an ordinary diagnostic.
func (s *mcpState) attachCounted(d func(mcpNote)) ([]mcpNote, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	buffered := s.notes
	dropped := s.dropped
	s.notes = nil
	s.dropped = 0
	s.deliver = d
	return buffered, dropped
}

func startupNoteOverflowDisclosure(dropped int) mcpNote {
	return mcpNote{
		level: "high",
		text: fmt.Sprintf(
			"extensions: %d startup diagnostics were dropped after the %d-note pre-surface buffer filled; /doctor extensions cannot show their text",
			dropped, maxBufferedNotes),
	}
}

func (s *mcpState) clientList() []*mcp.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*mcp.Client(nil), s.clients...)
}

func (s *mcpState) Close() {
	closeMCPClients(s.clientList())
}

// closeMCPClients tears independent transports down concurrently. Each client
// owns a bounded close/reap deadline; serial shutdown would multiply that
// deadline by the number of configured servers and make quitting arbitrarily
// slow for an otherwise independent set of processes and connections.
func closeMCPClients(clients []*mcp.Client) {
	var wg sync.WaitGroup
	for _, client := range clients {
		if client == nil {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			client.Close()
		}()
	}
	wg.Wait()
}

// connectMCP loads the user's servers and, when the workspace is trusted,
// the repository's, connects them, and registers every bridged tool. The
// returned rules carry the config's allow lists into the permission engine.
//
// questions is nil on a surface with no user. It is passed to every client
// rather than consulted here, because what it decides is what each client
// declares at initialize: the elicitation capability is a promise to answer,
// and only a surface with someone to ask may make it.
func connectMCP(ctx context.Context, workspace string, ts *trust.Store, registry *tools.Registry, questions *questionRelay, additional ...mcp.Spec) (*mcpState, []permission.Rule, error) {
	state := &mcpState{}

	var specs []mcp.Spec
	if home, err := os.UserHomeDir(); err == nil {
		userSpecs, err := mcp.LoadSpecsRooted(home, filepath.Join(".switchboard", mcp.SpecFileName))
		if err != nil {
			// Parsing is all-or-nothing, so an error makes it impossible to know
			// whether a required declaration was present. Continuing would turn a
			// malformed sibling entry into a required-server bypass.
			return state, nil, fmt.Errorf("applicable MCP configuration is invalid: %w", err)
		}
		specs = append(specs, userSpecs...)
	}

	repoPath := filepath.Join(workspace, ".switchboard", mcp.SpecFileName)
	if _, err := os.Stat(repoPath); err == nil {
		if ts != nil && ts.Trusted(workspace) {
			repoSpecs, err := mcp.LoadSpecsRooted(workspace, filepath.Join(".switchboard", mcp.SpecFileName))
			if err != nil {
				return state, nil, fmt.Errorf("trusted repository MCP configuration is invalid: %w", err)
			}
			specs = append(specs, repoSpecs...)
		} else {
			// The repository asked for servers and the user has not said yes.
			// Saying so once beats silently ignoring the file, which reads as
			// a bug to whoever wrote it.
			state.add(mcpNote{"warn",
				"this repository declares MCP servers in .switchboard/mcp.toml; they stay off until you run /trust grant"})
		}
	}
	// Native Codex/Claude and trusted plugin adapters arrive only after their
	// own activation, policy, trust, and compatibility gates. Appending them
	// preserves the explicit Switchboard user/repository precedence above while
	// still subjecting every source to the same collision and required-server
	// handling below.
	specs = append(specs, additional...)

	if len(specs) == 0 {
		return state, nil, nil
	}

	// A name collision across the two files is a configuration error, not a
	// race to the registry: the user file wins and the repo's double is named.
	seen := map[string]bool{}
	deduped := specs[:0]
	var requiredConfigurationErrors []string
	for _, s := range specs {
		if seen[s.Name] {
			state.add(mcpNote{"warn",
				fmt.Sprintf("mcp server %s is declared twice; the first declaration wins", s.Name)})
			if s.Required {
				requiredConfigurationErrors = append(requiredConfigurationErrors,
					fmt.Sprintf("required mcp server %s is shadowed by an earlier declaration", s.Name))
			}
			continue
		}
		seen[s.Name] = true
		deduped = append(deduped, s)
	}
	specs = deduped
	if len(requiredConfigurationErrors) > 0 {
		sort.Strings(requiredConfigurationErrors)
		return state, nil, fmt.Errorf("%s", strings.Join(requiredConfigurationErrors, "; "))
	}

	// logf outlives this function: every connected client's read loop keeps
	// calling it for as long as the session runs, which is why it goes
	// through the state's own lock and not one local to this frame.
	logf := func(level, text string) {
		state.add(mcpNote{level, text})
	}

	clients := make([]*mcp.Client, len(specs))
	connectErrors := make([]error, len(specs))
	var wg sync.WaitGroup
	for i, spec := range specs {
		wg.Add(1)
		go func(i int, spec mcp.Spec) {
			defer wg.Done()
			connectCtx, cancel := mcpAssemblyConnectContext(ctx, spec)
			defer cancel()
			var opts []mcp.Option
			if questions != nil {
				opts = append(opts, mcp.WithQuestioner(questions))
			}
			c, err := mcp.Connect(connectCtx, spec, logf, opts...)
			if err != nil {
				connectErrors[i] = err
				logf("error", fmt.Sprintf("mcp server %s did not connect: %v", spec.Name, err))
				return
			}
			clients[i] = c
		}(i, spec)
	}
	wg.Wait()

	var requiredFailures []string
	for i, spec := range specs {
		if spec.Required && clients[i] == nil {
			reason := "connection failed"
			if connectErrors[i] != nil {
				reason = connectErrors[i].Error()
			}
			requiredFailures = append(requiredFailures,
				fmt.Sprintf("required mcp server %s did not connect: %s", spec.Name, reason))
		}
	}
	if len(requiredFailures) > 0 {
		closeMCPClients(clients)
		sort.Strings(requiredFailures)
		return state, nil, fmt.Errorf("%s", strings.Join(requiredFailures, "; "))
	}

	var registrations []mcpToolRegistration
	var connected []*mcp.Client
	for _, c := range clients {
		if c == nil {
			continue
		}
		connected = append(connected, c)
		registrations = append(registrations, mcpRegistrations(c)...)
	}
	state.mu.Lock()
	state.clients = connected
	state.mu.Unlock()

	// Registration, collision resolution, and permission grants are one
	// operation. An allow entry names the server's raw tool identity; deriving
	// rules before the registry chooses among sanitized-name collisions lets a
	// losing identity preapprove the different tool that survived. Keeping the
	// identity beside its bridge until AddExternal succeeds makes that
	// substitution impossible.
	rules, count := registerMCPTools(registry, state, registrations)
	if count > 0 {
		names := make([]string, 0, len(connected))
		for _, c := range connected {
			names = append(names, c.Name())
		}
		sort.Strings(names)
		state.add(mcpNote{"",
			fmt.Sprintf("mcp: %d tools from %s", count, joinAnd(names))})
	}
	return state, rules, nil
}

// mcpAssemblyConnectContext supplies the historical 15-second default only
// when a server did not configure its own startup deadline. mcp.Connect owns
// configured per-server timeouts; wrapping the whole parallel batch in this
// default used to truncate a valid 60-second native setting to 15 seconds.
func mcpAssemblyConnectContext(ctx context.Context, spec mcp.Spec) (context.Context, context.CancelFunc) {
	if spec.StartupTimeout > 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, mcpConnectTimeout)
}

// mcpToolRegistration keeps the identity the server advertised beside the
// provider-safe name and bridge it produced. allowed is scoped to both the raw
// server and raw tool names; it must never be inferred from exposed alone,
// because exposed is intentionally lossy.
type mcpToolRegistration struct {
	server  string
	rawTool string
	exposed string
	tool    tools.Tool
	allowed bool
}

// mcpRegistrations joins a client's raw discovery result to its bridges. Two
// different raw tools from one server that sanitize alike are both left
// bridge-less: letting tools/list order choose a survivor would let the server
// change which identity a standing allow entry names. The identities remain in
// the list so registration can diagnose the collision deterministically.
func mcpRegistrations(c *mcp.Client) []mcpToolRegistration {
	allowed := map[string]bool{}
	for _, name := range c.Spec().Allow {
		allowed[name] = true
	}

	bridged := map[string]tools.Tool{}
	for _, tool := range c.BridgedTools() {
		// BridgedTools already refuses duplicates within one client. Keep this
		// first-wins guard so a future implementation cannot make the join depend
		// on map overwrite order.
		if _, exists := bridged[tool.Name()]; !exists {
			bridged[tool.Name()] = tool
		}
	}

	counts := map[string]int{}
	seenRaw := map[string]bool{}
	var out []mcpToolRegistration
	for _, info := range c.Tools() {
		// A server repeating the same raw definition has not introduced a
		// different permission identity. BridgedTools reports the duplicate; one
		// registration record is enough here.
		if seenRaw[info.Name] {
			continue
		}
		seenRaw[info.Name] = true

		exposed := mcp.Namespaced(c.Name(), info.Name)
		entry := mcpToolRegistration{
			server:  c.Name(),
			rawTool: info.Name,
			exposed: exposed,
			allowed: allowed[info.Name],
		}
		counts[exposed]++
		out = append(out, entry)
	}
	for i := range out {
		if counts[out[i].exposed] == 1 {
			out[i].tool = bridged[out[i].exposed]
		}
	}
	return out
}

// registerMCPTools resolves every exposed-name collision deterministically.
// An intra-server collision has no survivor. Across servers the
// lexicographically first unambiguous raw server/tool identity wins, independent
// of connection completion order. Rules are emitted only after that exact
// bridge registered successfully; skipped identities are permission-inert.
func registerMCPTools(registry *tools.Registry, state *mcpState, registrations []mcpToolRegistration) ([]permission.Rule, int) {
	sort.Slice(registrations, func(i, j int) bool {
		if registrations[i].exposed != registrations[j].exposed {
			return registrations[i].exposed < registrations[j].exposed
		}
		if registrations[i].server != registrations[j].server {
			return registrations[i].server < registrations[j].server
		}
		return registrations[i].rawTool < registrations[j].rawTool
	})

	var rules []permission.Rule
	count := 0
	for start := 0; start < len(registrations); {
		end := start + 1
		for end < len(registrations) && registrations[end].exposed == registrations[start].exposed {
			end++
		}
		group := registrations[start:end]

		// A nil bridge is an identity BridgedTools refused, such as the losing
		// half of an intra-server sanitizer collision or an overlong name.
		winner := -1
		for i := range group {
			if group[i].tool != nil {
				winner = i
				break
			}
		}
		if winner < 0 {
			if len(group) > 1 {
				identities := make([]string, 0, len(group))
				for _, entry := range group {
					identities = append(identities, mcpToolIdentity(entry))
				}
				state.add(mcpNote{"warn", fmt.Sprintf(
					"mcp tools %s map to exposed name %q; none was registered because the raw identity is ambiguous",
					joinAnd(identities), group[0].exposed)})
			}
			start = end
			continue
		}

		chosen := group[winner]
		if err := registry.AddExternal(chosen.tool); err != nil {
			state.add(mcpNote{"warn", err.Error()})
			start = end
			continue
		}
		count++

		if len(group) > 1 {
			identities := make([]string, 0, len(group))
			for _, entry := range group {
				identities = append(identities, mcpToolIdentity(entry))
			}
			state.add(mcpNote{"warn", fmt.Sprintf(
				"mcp tools %s map to exposed name %q; %s was registered and every other identity was skipped",
				joinAnd(identities), chosen.exposed, mcpToolIdentity(chosen))})
		}

		if chosen.allowed {
			rules = append(rules, permission.Rule{
				Decision: permission.Allow,
				Tool:     chosen.exposed,
				Effect:   permission.EffectExternal,
			})
		}
		start = end
	}
	return rules, count
}

func mcpToolIdentity(reg mcpToolRegistration) string {
	return fmt.Sprintf("server %q tool %q", reg.server, reg.rawTool)
}

func joinAnd(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	last := names[len(names)-1]
	rest := names[:len(names)-1]
	out := ""
	for i, n := range rest {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out + " and " + last
}
