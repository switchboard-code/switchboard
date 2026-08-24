// Package mcp connects Model Context Protocol servers to the tool suite.
//
// The built-in suite stays small on purpose; MCP is the socket the long tail
// plugs into (design principle 5). This package speaks the protocol's client
// side over stdio and Streamable HTTP, and bridges each discovered tool into
// the registry under a namespaced name.
//
// Two constraints shape the code. Discovery happens once, at session
// assembly: tool definitions sit in the frozen zone of the context layout
// (§6.1), so a server that changes its tool list mid-session is deliberately
// not followed. And an MCP tool runs outside whatever sandbox this host
// verified — the server is a process this package started un-confined, acting
// wherever it acts — so every bridged call carries the external effect and
// the permission engine treats it accordingly.
package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/tools"
)

const (
	// Modern MCP is stateless: every request carries its protocol metadata.
	modernProtocolVersion = "2026-07-28"

	// This is the newest initialization-based revision Switchboard implements.
	// 2025-03-26 remains accepted because the existing HTTP transport implements
	// that revision's session shape too.
	legacyProtocolVersion = "2025-06-18"
	olderLegacyVersion    = "2025-03-26"

	modernProbeTimeout = 5 * time.Second

	maxToolListPages = 1_000
	maxListedTools   = 100_000
	maxToolListBytes = 32 << 20

	serverReplyQueueSize = 32
	serverReplyWorkers   = 2
	serverReplyTimeout   = 5 * time.Second
	maxServerReplyID     = 4 << 10
	maxServerMethod      = 1 << 10
)

type listToolsLimits struct {
	pages int
	tools int
	bytes int
}

var defaultListToolsLimits = listToolsLimits{
	pages: maxToolListPages,
	tools: maxListedTools,
	bytes: maxToolListBytes,
}

// Spec describes one configured server. Command starts a stdio server; URL
// reaches a Streamable HTTP one. Exactly one must be set.
type Spec struct {
	Name    string
	Command string
	Args    []string
	Env     map[string]string
	CWD     string

	// RestrictedEnv keeps native declarations from inheriting arbitrary host
	// state. In that mode only a small process baseline, the exact names in
	// InheritEnv, and static Env overrides reach the child. Legacy Switchboard
	// declarations leave this false and retain their non-sensitive ambient env.
	RestrictedEnv bool
	InheritEnv    []string

	URL               string
	Headers           map[string]string
	HeaderEnv         map[string]string
	BearerTokenEnvVar string

	StartupTimeout time.Duration
	ToolTimeout    time.Duration

	EnabledTools     []string
	EnabledToolsSet  bool
	DisabledTools    []string
	DisabledToolsSet bool
	Required         bool

	// Allow lists tool names (the server's own names, not the namespaced
	// form) the user pre-approved in config. Everything else asks.
	Allow []string
}

func (s Spec) validate() error {
	if s.Name == "" {
		return errors.New("mcp server has no name")
	}
	if (s.Command == "") == (s.URL == "") {
		return fmt.Errorf("mcp server %s: exactly one of command and url must be set", s.Name)
	}
	if s.StartupTimeout < 0 || s.ToolTimeout < 0 {
		return fmt.Errorf("mcp server %s: timeouts must not be negative", s.Name)
	}
	seenEnv := make(map[string]string, len(s.Env))
	for _, name := range sortedMapKeys(s.Env) {
		value := s.Env[name]
		if !validEnvironmentName(name) {
			return fmt.Errorf("mcp server %s: invalid environment variable name %q", s.Name, name)
		}
		key := environmentKey(name)
		if prior, duplicate := seenEnv[key]; duplicate {
			return fmt.Errorf("mcp server %s: environment variable %q duplicates %q", s.Name, name, prior)
		}
		seenEnv[key] = name
		if strings.IndexByte(value, 0) >= 0 {
			return fmt.Errorf("mcp server %s: environment variable %q has an invalid value", s.Name, name)
		}
	}
	for _, name := range s.InheritEnv {
		if !validEnvironmentName(name) {
			return fmt.Errorf("mcp server %s: invalid inherited environment variable name %q", s.Name, name)
		}
	}
	if strings.IndexByte(s.CWD, 0) >= 0 {
		return fmt.Errorf("mcp server %s: cwd has an invalid value", s.Name)
	}
	if s.Command != "" {
		if len(s.Headers) > 0 || len(s.HeaderEnv) > 0 || s.BearerTokenEnvVar != "" {
			return fmt.Errorf("mcp server %s: stdio transport has HTTP-only header fields", s.Name)
		}
	} else {
		if s.CWD != "" || len(s.Env) > 0 || len(s.InheritEnv) > 0 || s.RestrictedEnv {
			return fmt.Errorf("mcp server %s: HTTP transport has stdio-only environment fields", s.Name)
		}
		if err := validateHTTPHeaderConfig(s); err != nil {
			return fmt.Errorf("mcp server %s: %w", s.Name, err)
		}
	}
	return nil
}

func validEnvironmentName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

func specSecretValues(spec Spec) []string {
	var secrets []string
	for _, value := range append([]string{spec.Command, spec.CWD, spec.URL}, spec.Args...) {
		if value != "" {
			secrets = append(secrets, value)
		}
	}
	for _, value := range spec.Env {
		if value != "" {
			secrets = append(secrets, value)
		}
	}
	for _, value := range spec.Headers {
		if value != "" {
			secrets = append(secrets, value)
		}
	}
	for _, name := range spec.InheritEnv {
		if value, exists := os.LookupEnv(name); exists && value != "" {
			secrets = append(secrets, value)
		}
	}
	for _, name := range spec.HeaderEnv {
		if value, exists := os.LookupEnv(name); exists && value != "" {
			secrets = append(secrets, value)
		}
	}
	if spec.BearerTokenEnvVar != "" {
		if value, exists := os.LookupEnv(spec.BearerTokenEnvVar); exists && value != "" {
			secrets = append(secrets, value)
		}
	}
	return secrets
}

func specCredentialValues(spec Spec) []string {
	credentials := endpointCredentialValues(spec.URL)
	for name, value := range spec.Env {
		if credentialBearingName(name) && value != "" {
			credentials = append(credentials, value)
		}
	}
	for name, value := range spec.Headers {
		if sensitiveCredentialName(name) && value != "" {
			credentials = append(credentials, value)
		}
	}
	for _, name := range spec.InheritEnv {
		if credentialBearingName(name) {
			if value, exists := os.LookupEnv(name); exists && value != "" {
				credentials = append(credentials, value)
			}
		}
	}
	for header, name := range spec.HeaderEnv {
		if sensitiveCredentialName(header) || credentialBearingName(name) {
			if value, exists := os.LookupEnv(name); exists && value != "" {
				credentials = append(credentials, value)
			}
		}
	}
	if spec.BearerTokenEnvVar != "" {
		if value, exists := os.LookupEnv(spec.BearerTokenEnvVar); exists && value != "" {
			credentials = append(credentials, value)
		}
	}
	return credentials
}

func sensitiveCredentialName(name string) bool {
	return credentialBearingName(name)
}

// credentialBearingName classifies values that must be scrubbed from a
// successful tool result. Unlike the deliberately conservative ambient-env
// filter, it is token-aware: "X-Monkey" and "X-Hockey-Team" are ordinary
// names, while AUTH, SESSION_ID, X-API-Key, and ClientSecret are credentials.
func credentialBearingName(name string) bool {
	lower := strings.ToLower(name)
	tokens := strings.FieldsFunc(lower, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	for _, token := range tokens {
		switch token {
		case "auth", "authorization", "authentication",
			"cookie", "cookies", "credential", "credentials",
			"dsn", "key", "keys", "passwd", "password", "passwords", "pwd",
			"secret", "secrets", "session", "sessions", "token", "tokens":
			return true
		}
	}
	compact := strings.Join(tokens, "")
	switch compact {
	case "apikey", "xapikey", "accesstoken", "authtoken", "bearertoken",
		"clientsecret", "privatekey", "secretkey", "sessionid", "sessionkey",
		"signingkey", "encryptionkey", "refreshtoken", "sshauthsock", "sshagentpid":
		return true
	}
	has := func(candidates ...string) bool {
		for _, token := range tokens {
			for _, candidate := range candidates {
				if token == candidate {
					return true
				}
			}
		}
		return false
	}
	if has("url", "uri") && has("db", "database", "postgres", "postgresql", "mysql", "mariadb", "mongo", "mongodb", "redis", "amqp", "rabbitmq") {
		return true
	}
	return has("connection") && has("string")
}

// ToolInfo is one tool as the server described it.
type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Result mirrors tools.Result without importing it: content for the model,
// and whether the tool itself reports failure.
type Result struct {
	Content string
	IsError bool

	// Images are the picture blocks the server returned. They are carried out
	// rather than flattened into Content because a screenshot is the answer to
	// the call that asked for one, and "[image content omitted]" is not.
	Images []provider.Image

	// RPCError retains the peer's typed code and data when a protocol-level
	// tool refusal is intentionally returned as a model-correctable result.
	// It is nil for ordinary tool results and transport failures.
	RPCError *RPCError
}

// transport carries newline-delimited JSON-RPC messages both ways. Close
// must unblock a pending Recv.
type transport interface {
	Send(ctx context.Context, msg []byte) error
	Recv() ([]byte, error)
	Close() error
}

// Client is one connected server. Methods are safe for concurrent use;
// requests are matched to responses by id, so calls can overlap even though
// the bridge serializes them today.
type Client struct {
	spec Spec
	// secrets is the point-in-time materialization used to start/connect the
	// server. Dynamic HTTP env values are also re-read when sanitizing, but the
	// captured set keeps a later host-env change from exposing an earlier value.
	secrets     []string
	credentials []string

	// logf receives protocol-level notices: a crashed server, a log message
	// the server sent, a request it made that this client refuses. It is the
	// package's only output channel; nothing here writes to a terminal.
	logf func(level, text string)

	transport     transport
	seq           atomic.Int64
	lifetime      context.Context
	cancel        context.CancelFunc
	closeOnce     sync.Once
	closeErr      error
	answerOnce    sync.Once
	answerQueue   chan serverReply
	answerTimeout time.Duration

	mu      sync.Mutex
	pending map[int64]chan rpcResponse
	dead    error // set once the read loop exits; sticky
	closing bool

	// questioner is the surface's user channel, set at assembly by a surface
	// that has one. Its presence is what declares the elicitation capability
	// and what lets a server's question reach a person; nil is the closed
	// state every unattended surface starts in.
	questioner tools.Questioner

	serverName    string
	serverVersion string
	protocol      string
	tools         []ToolInfo
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	Result      json.RawMessage `json:"result"`
	Error       *rpcError       `json:"error"`
	secrets     []string
	credentials []string

	// fatal marks a transport death rather than a server answer, so a dead
	// connection is never mistaken for a tool-level refusal the model could
	// correct.
	fatal error
}

type serverReply struct {
	id     json.RawMessage
	method string

	// params rides along because a request this client answers with more than
	// an empty object needs its contents, and the receive loop is the only
	// place they exist.
	params json.RawMessage
}

// RPCError is a JSON-RPC error returned by an MCP peer. Data stays typed raw
// JSON so protocol-defined payloads such as UnsupportedProtocolVersion's
// supported-version list survive wrapping and errors.As.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
	secrets []string
	rawData json.RawMessage
}

func (e *RPCError) Error() string {
	return redactSecrets(fmt.Sprintf("%s (code %d)", e.Message, e.Code), e.secrets)
}

// Keep the package's existing internal name while making the typed error
// available to callers of this internal package.
type rpcError = RPCError

// ProtocolVersionError reports that discovery proved the peer was modern but
// the two sides share no protocol revision Switchboard implements.
type ProtocolVersionError struct {
	Requested string
	Supported []string
}

func (e *ProtocolVersionError) Error() string {
	if len(e.Supported) == 0 {
		return fmt.Sprintf("mcp server did not advertise a supported protocol version (requested %s)", e.Requested)
	}
	return fmt.Sprintf("mcp server supports %s; switchboard requested %s", strings.Join(e.Supported, ", "), e.Requested)
}

// UnsupportedResultTypeError prevents a modern multi-round-trip or extension
// result from being mistaken for an empty successful tool result.
type UnsupportedResultTypeError struct {
	Method     string
	ResultType string
}

func (e *UnsupportedResultTypeError) Error() string {
	return fmt.Sprintf("%s returned unsupported MCP result type %q", e.Method, e.ResultType)
}

// ToolFilteredError reports a configured admission decision. The request is
// rejected locally and is never sent to the MCP server.
type ToolFilteredError struct {
	Tool string
}

func (e *ToolFilteredError) Error() string {
	return fmt.Sprintf("mcp tool %q is disabled by server configuration", e.Tool)
}

// incoming is the shape every received line is first read into, enough to
// tell a response from a server-initiated request from a notification.
type incoming struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
	Params json.RawMessage `json:"params"`
}

type protocolEra uint8

const (
	eraModern protocolEra = iota + 1
	eraLegacy
)

type negotiation struct {
	era      protocolEra
	protocol string
	discover *discoverResult
	secrets  []string
}

type transportKind uint8

const (
	transportStdio transportKind = iota + 1
	transportHTTP
)

type discoverResult struct {
	ResultType        string                     `json:"resultType"`
	SupportedVersions []string                   `json:"supportedVersions"`
	Capabilities      json.RawMessage            `json:"capabilities"`
	Instructions      string                     `json:"instructions"`
	Meta              map[string]json.RawMessage `json:"_meta"`
}

type implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Option configures a client at construction. Options are applied before the
// read loop starts, so nothing a server sends can race one into place.
type Option func(*Client)

// WithQuestioner grants the elicitation role by supplying the channel it
// resolves against. A client built without one declares no elicitation
// capability and declines the method, which is what every unattended surface
// gets by doing nothing.
func WithQuestioner(q tools.Questioner) Option {
	return func(c *Client) { c.questioner = q }
}

// Connect starts (or reaches) the server, negotiates the modern or legacy
// protocol era, and lists its tools. A successful modern stdio probe becomes
// the live session. Only legacy fallback replaces the probe process: old
// servers are allowed to exit on pre-initialize traffic, and that exit must
// not poison the real session's read loop or sticky failure state.
func Connect(ctx context.Context, spec Spec, logf func(level, text string), opts ...Option) (*Client, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if spec.StartupTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.StartupTimeout)
		defer cancel()
	}
	if logf == nil {
		logf = func(string, string) {}
	}

	if spec.Command != "" {
		return connectStdio(ctx, spec, logf, opts...)
	}

	tr, err := startHTTP(spec)
	if err != nil {
		return nil, err
	}
	c := newClient(spec, tr, logf, opts...)
	decision, err := c.probeModern(ctx, transportHTTP)
	if err != nil {
		c.Close()
		return nil, err
	}
	return finishConnect(ctx, c, decision)
}

func newClient(spec Spec, tr transport, logf func(level, text string), opts ...Option) *Client {
	if logf == nil {
		logf = func(string, string) {}
	}
	initialSecrets := specSecretValues(spec)
	initialCredentials := specCredentialValues(spec)
	if provider, ok := tr.(interface{ secretValues() []string }); ok {
		initialSecrets = append(initialSecrets, provider.secretValues()...)
	}
	if provider, ok := tr.(interface{ credentialValues() []string }); ok {
		initialCredentials = append(initialCredentials, provider.credentialValues()...)
	}
	rawLogf := logf
	logf = func(level, text string) {
		secrets := append(append([]string(nil), initialSecrets...), specSecretValues(spec)...)
		rawLogf(level, redactSecrets(text, secrets))
	}
	lifetime, cancel := context.WithCancel(context.Background())
	c := &Client{
		spec:        spec,
		secrets:     initialSecrets,
		credentials: initialCredentials,
		logf:        logf,
		transport:   tr,
		lifetime:    lifetime,
		cancel:      cancel,
		pending:     map[int64]chan rpcResponse{},
	}
	for _, opt := range opts {
		opt(c)
	}
	go c.readLoop()
	return c
}

func connectStdio(ctx context.Context, spec Spec, logf func(level, text string), opts ...Option) (*Client, error) {
	tr, err := startStdio(spec)
	if err != nil {
		return nil, err
	}
	probe := newClient(spec, tr, logf, opts...)
	probeCtx, cancel := context.WithTimeout(ctx, modernProbeTimeout)
	decision, probeErr := probe.probeModern(probeCtx, transportStdio)
	cancel()

	// A caller cancellation is an instruction to stop, never protocol-era
	// evidence. An internal probe timeout, while the parent remains live, is
	// the stdio binding's documented legacy signal.
	if err := ctx.Err(); err != nil {
		tr.abort(err)
		_ = probe.Close()
		return nil, err
	}
	if probeErr != nil {
		tr.abort(probeErr)
		_ = probe.Close()
		return nil, probeErr
	}
	if decision.era == eraModern {
		return finishConnect(ctx, probe, decision)
	}

	// A legacy server may have rejected pre-initialize traffic, exited, or
	// left unread bytes behind. Retire that process tree before starting the
	// initialization-based session; reusing it would make downgrade stateful.
	tr.abort(errors.New("legacy MCP fallback replaces protocol probe"))
	_ = probe.Close()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	legacyTransport, err := startStdio(spec)
	if err != nil {
		return nil, err
	}
	return finishConnect(ctx, newClient(spec, legacyTransport, logf, opts...), decision)
}

func finishConnect(ctx context.Context, c *Client, decision negotiation) (*Client, error) {
	if decision.era == eraModern {
		c.protocol = decision.protocol
		if t, ok := c.transport.(interface{ setProtocol(string) }); ok {
			t.setProtocol(decision.protocol)
		}
		c.applyDiscovery(decision.discover, decision.secrets)
	} else if err := c.initializeLegacy(ctx, decision.protocol); err != nil {
		c.closeAfterConnectFailure(ctx)
		return nil, err
	}
	if err := c.listTools(ctx); err != nil {
		c.closeAfterConnectFailure(ctx)
		return nil, err
	}
	return c, nil
}

func (c *Client) closeAfterConnectFailure(ctx context.Context) {
	if err := ctx.Err(); err != nil {
		if transport, ok := c.transport.(*stdioTransport); ok {
			transport.abort(err)
		}
	}
	_ = c.Close()
}

func (c *Client) Name() string { return c.spec.Name }
func (c *Client) Spec() Spec   { return c.spec }
func (c *Client) Tools() []ToolInfo {
	return append([]ToolInfo(nil), c.tools...)
}

// ServerLine describes the server for display: what it calls itself, and the
// protocol revision it answered with.
func (c *Client) ServerLine() string {
	name := c.serverName
	if name == "" {
		name = "unnamed server"
	}
	if c.serverVersion != "" {
		name += " " + c.serverVersion
	}
	return fmt.Sprintf("%s, protocol %s", name, c.protocol)
}

// Err reports why the connection died, or nil while it is alive.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dead
}

func (c *Client) probeModern(ctx context.Context, kind transportKind) (negotiation, error) {
	params := json.RawMessage(`{}`)
	raw, secrets, _, err := c.callVersionWithSecrets(ctx, modernProtocolVersion, "server/discover", params)
	if err == nil {
		var discovered discoverResult
		if err := json.Unmarshal(raw, &discovered); err != nil {
			return negotiation{}, fmt.Errorf("server/discover: malformed result: %w", err)
		}
		if err := validateResultType("server/discover", discovered.ResultType, secrets); err != nil {
			return negotiation{}, err
		}
		return negotiationFromDiscovery(&discovered, secrets)
	}

	// Cancellation from the owner is never a downgrade signal. For stdio,
	// probeStdio supplies a child timeout and checks the still-live parent
	// afterward; that timeout is the binding's documented legacy evidence.
	if kind == transportHTTP && ctx.Err() != nil {
		return negotiation{}, ctx.Err()
	}

	var rpcErr *RPCError
	if errors.As(err, &rpcErr) && recognizedModernError(rpcErr.Code) {
		if rpcErr.Code == -32022 {
			return negotiationFromUnsupported(rpcErr)
		}
		return negotiation{}, fmt.Errorf("server/discover: %w", err)
	}

	if kind == transportStdio {
		return negotiation{era: eraLegacy, protocol: legacyProtocolVersion}, nil
	}

	var statusErr *httpStatusError
	if errors.As(err, &statusErr) && statusErr.StatusCode == 400 {
		// Only an unrecognized 400 is protocol-era evidence on Streamable
		// HTTP. Auth failures, rate limits, 5xx responses, and transport
		// failures remain operational errors and never trigger a downgrade.
		return negotiation{era: eraLegacy, protocol: legacyProtocolVersion}, nil
	}
	return negotiation{}, fmt.Errorf("server/discover: %w", err)
}

func recognizedModernError(code int) bool {
	return code == -32020 || code == -32021 || code == -32022
}

func negotiationFromDiscovery(discovered *discoverResult, secrets []string) (negotiation, error) {
	for _, version := range discovered.SupportedVersions {
		if version == modernProtocolVersion {
			return negotiation{era: eraModern, protocol: modernProtocolVersion, discover: discovered, secrets: append([]string(nil), secrets...)}, nil
		}
	}
	if version := preferredLegacyVersion(discovered.SupportedVersions); version != "" {
		return negotiation{era: eraLegacy, protocol: version, secrets: append([]string(nil), secrets...)}, nil
	}
	return negotiation{}, &ProtocolVersionError{
		Requested: modernProtocolVersion,
		Supported: redactStringSlice(discovered.SupportedVersions, secrets),
	}
}

func negotiationFromUnsupported(rpcErr *RPCError) (negotiation, error) {
	var data struct {
		Supported []string `json:"supported"`
	}
	rawData := rpcErr.rawData
	if len(rawData) == 0 {
		rawData = rpcErr.Data
	}
	if len(rawData) == 0 || json.Unmarshal(rawData, &data) != nil {
		return negotiation{}, &ProtocolVersionError{Requested: modernProtocolVersion}
	}
	if version := preferredLegacyVersion(data.Supported); version != "" {
		// The peer explicitly advertised this initialization-based version.
		// This is negotiated downgrade, not heuristic fallback.
		return negotiation{era: eraLegacy, protocol: version}, nil
	}
	return negotiation{}, &ProtocolVersionError{
		Requested: modernProtocolVersion,
		Supported: redactStringSlice(data.Supported, rpcErr.secrets),
	}
}

func (c *Client) applyDiscovery(discovered *discoverResult, secrets []string) {
	if discovered == nil {
		return
	}
	raw := discovered.Meta["io.modelcontextprotocol/serverInfo"]
	var info implementation
	if len(raw) > 0 && json.Unmarshal(raw, &info) == nil {
		c.serverName = redactSecrets(info.Name, secrets)
		c.serverVersion = redactSecrets(info.Version, secrets)
	}
}

// initialize remains as the legacy test seam and always requests the newest
// initialization-based revision this client implements.
func (c *Client) initialize(ctx context.Context) error {
	return c.initializeLegacy(ctx, legacyProtocolVersion)
}

func (c *Client) initializeLegacy(ctx context.Context, requested string) error {
	// A capability is a promise to answer. Elicitation is declared only when a
	// surface supplied the channel that answers it, because a server told this
	// client can ask will ask, and an unattended session has no one to hear it.
	capabilities := map[string]any{}
	if c.questioner != nil {
		capabilities["elicitation"] = map[string]any{}
	}
	params, _ := json.Marshal(map[string]any{
		"protocolVersion": requested,
		"capabilities":    capabilities,
		"clientInfo":      map[string]any{"name": "switchboard", "version": "dev"},
	})
	raw, secrets, _, err := c.callVersionWithSecrets(ctx, "", "initialize", params)
	if err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	var res struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return fmt.Errorf("initialize: malformed result: %w", err)
	}
	if !supportedLegacyVersion(res.ProtocolVersion) {
		return &ProtocolVersionError{Requested: requested, Supported: redactStringSlice([]string{res.ProtocolVersion}, secrets)}
	}
	c.protocol = res.ProtocolVersion
	c.serverName = redactSecrets(res.ServerInfo.Name, secrets)
	c.serverVersion = redactSecrets(res.ServerInfo.Version, secrets)
	if t, ok := c.transport.(interface{ setProtocol(string) }); ok && res.ProtocolVersion != "" {
		t.setProtocol(res.ProtocolVersion)
	}

	note, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"})
	return c.transport.Send(ctx, note)
}

func supportedLegacyVersion(version string) bool {
	return version == legacyProtocolVersion || version == olderLegacyVersion
}

func preferredLegacyVersion(supported []string) string {
	for _, candidate := range [...]string{legacyProtocolVersion, olderLegacyVersion} {
		for _, version := range supported {
			if version == candidate {
				return candidate
			}
		}
	}
	return ""
}

func (c *Client) listTools(ctx context.Context) error {
	return c.listToolsWithLimits(ctx, defaultListToolsLimits)
}

func (c *Client) listToolsWithLimits(ctx context.Context, limits listToolsLimits) error {
	if limits.pages <= 0 || limits.tools <= 0 || limits.bytes <= 0 {
		return errors.New("tools/list: invalid client pagination limits")
	}
	cursor := ""
	seenCursors := map[string]struct{}{"": {}}
	pages := 0
	toolCount := 0
	totalBytes := 0
	var tools []ToolInfo
	for {
		if pages >= limits.pages {
			return fmt.Errorf("tools/list: response exceeds %d pages", limits.pages)
		}
		p := map[string]any{}
		if cursor != "" {
			p["cursor"] = cursor
		}
		params, _ := json.Marshal(p)
		raw, secrets, credentials, err := c.callWithSecrets(ctx, "tools/list", params)
		if err != nil {
			return fmt.Errorf("tools/list: %w", err)
		}
		metadataSecrets := credentials
		if _, ok := c.transport.(*httpTransport); ok {
			// Every value applied to an HTTP request is server-visible and can
			// therefore be laundered back through the successful response. Stdio
			// has no analogous request-header set; restricting it to credentials
			// avoids treating ordinary command arguments as secret metadata.
			metadataSecrets = secrets
		}
		pages++
		if len(raw) > limits.bytes-totalBytes {
			return fmt.Errorf("tools/list: aggregate response exceeds %d bytes", limits.bytes)
		}
		totalBytes += len(raw)
		var res struct {
			ResultType string     `json:"resultType"`
			Tools      []ToolInfo `json:"tools"`
			NextCursor string     `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return fmt.Errorf("tools/list: malformed result: %w", err)
		}
		if err := validateResultType("tools/list", res.ResultType, secrets); err != nil {
			return err
		}
		if len(res.Tools) > limits.tools-toolCount {
			return fmt.Errorf("tools/list: response exceeds %d tools", limits.tools)
		}
		toolCount += len(res.Tools)
		for _, tool := range res.Tools {
			if !c.spec.toolEnabled(tool.Name) {
				continue
			}
			var safe bool
			tool, safe = redactToolMetadata(tool, metadataSecrets)
			if !safe {
				c.logf("warn", fmt.Sprintf("mcp %s: tool skipped: discovery metadata contains request credentials", redactSecrets(c.spec.Name, secrets)))
				continue
			}
			if !c.configureModernHTTPTool(tool, secrets) {
				continue
			}
			tools = append(tools, tool)
		}
		if res.NextCursor == "" {
			c.tools = tools
			return nil
		}
		if _, repeated := seenCursors[res.NextCursor]; repeated {
			return errors.New("tools/list: server repeated cursor")
		}
		seenCursors[res.NextCursor] = struct{}{}
		cursor = res.NextCursor
	}
}

// redactToolMetadata is the successful tools/list boundary. An HTTP server
// sees the request's configured headers and can echo them into discovery
// metadata; those values must not then become frozen provider definitions or
// escape through Tools. Redact the exact request values rather than relying on
// credential-shape scanning: configured credentials may be entirely opaque.
//
// Tool names are both provider-visible and protocol identifiers. Rewriting one
// would make the provider's name disagree with the remote tools/call identity,
// so a credential-bearing name fails closed instead. Descriptions and schemas
// can be cloned and sanitized while preserving every unrelated schema byte.
// The token walk decodes JSON escapes before matching, which covers nested
// values, object keys, and non-string primitives without otherwise normalizing
// the server's schema.
func redactToolMetadata(tool ToolInfo, secrets []string) (ToolInfo, bool) {
	if redacted := redactToolMetadataString(tool.Name, secrets); redacted != tool.Name {
		return ToolInfo{}, false
	}
	tool.Description = redactToolMetadataString(tool.Description, secrets)
	tool.InputSchema = redactToolSchema(tool.InputSchema, secrets)
	return tool, true
}

func redactToolSchema(raw json.RawMessage, secrets []string) json.RawMessage {
	if len(raw) == 0 || len(secrets) == 0 {
		return raw
	}
	out := make([]byte, 0, len(raw))
	for i := 0; i < len(raw); {
		if raw[i] != '"' && !jsonPrimitiveStart(raw[i]) {
			out = append(out, raw[i])
			i++
			continue
		}
		if raw[i] != '"' {
			start := i
			for i < len(raw) && !jsonTokenDelimiter(raw[i]) {
				i++
			}
			token := string(raw[start:i])
			redacted := redactToolMetadataString(token, secrets)
			if redacted == token {
				out = append(out, raw[start:i]...)
				continue
			}
			// Replacing a schema number, boolean, or null with a string can
			// invalidate keyword types (for example, maximum). Once a secret is
			// carried as a non-string primitive, fail the complete schema closed
			// instead of advertising malformed semantics to the provider.
			return json.RawMessage(`{"type":"object","properties":{}}`)
		}

		start := i
		i++
		for i < len(raw) {
			switch raw[i] {
			case '\\':
				i += 2
			case '"':
				i++
				goto stringEnd
			default:
				i++
			}
		}

	stringEnd:
		token := raw[start:i]
		var value string
		if err := json.Unmarshal(token, &value); err != nil {
			// InputSchema arrived inside a successfully decoded JSON response, so
			// this can only happen if the token walk and encoding/json disagree.
			// Do not make that disagreement a raw-metadata escape hatch.
			return json.RawMessage(`{"type":"object","properties":{}}`)
		}
		redacted := redactToolMetadataString(value, secrets)
		if redacted == value {
			out = append(out, token...)
			continue
		}
		encoded, _ := json.Marshal(redacted) // encoding a string cannot fail
		out = append(out, encoded...)
	}
	return json.RawMessage(out)
}

func jsonPrimitiveStart(b byte) bool {
	return b == '-' || b >= '0' && b <= '9' || b == 't' || b == 'f' || b == 'n'
}

func jsonTokenDelimiter(b byte) bool {
	switch b {
	case ' ', '\t', '\r', '\n', ',', ']', '}':
		return true
	default:
		return false
	}
}

func redactToolMetadataString(value string, secrets []string) string {
	redacted := redactSecrets(value, secrets)
	// A configured value can itself equal or occur inside the normal marker.
	// Empty is the only universal replacement that cannot retain a non-empty
	// secret; use it only for that marker-collision edge case.
	for _, secret := range secrets {
		if secret != "" && strings.Contains(redacted, secret) {
			return ""
		}
	}
	return redacted
}

func (c *Client) configureModernHTTPTool(tool ToolInfo, secrets []string) bool {
	transport, ok := c.transport.(*httpTransport)
	if !ok || c.protocol != modernProtocolVersion {
		return true
	}
	bindings, err := parseToolHeaderBindings(tool.InputSchema)
	if err != nil {
		message := fmt.Sprintf("mcp %s: tool %s skipped: invalid x-mcp-header schema: %v", c.spec.Name, tool.Name, err)
		c.logf("warn", redactSecrets(message, secrets))
		return false
	}
	transport.setToolHeaders(tool.Name, bindings)
	return true
}

func (s Spec) toolEnabled(name string) bool {
	for _, disabled := range s.DisabledTools {
		if name == disabled {
			return false
		}
	}
	if s.EnabledToolsSet || len(s.EnabledTools) > 0 {
		for _, enabled := range s.EnabledTools {
			if name == enabled {
				return true
			}
		}
		return false
	}
	return true
}

// Call invokes one tool. A tool-level failure comes back as a Result with
// IsError set, exactly as the built-in suite reports one, so the model can
// read it and correct itself; only a transport-level failure is an error.
func (c *Client) Call(ctx context.Context, tool string, args json.RawMessage) (Result, error) {
	if !c.spec.toolEnabled(tool) {
		return Result{}, &ToolFilteredError{Tool: tool}
	}
	if c.spec.ToolTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.spec.ToolTimeout)
		defer cancel()
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	params, _ := json.Marshal(struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{tool, args})

	raw, secrets, credentials, err := c.callWithSecrets(ctx, "tools/call", params)
	if err != nil {
		var rpcErr *rpcError
		if errors.As(err, &rpcErr) {
			// A protocol-level refusal (unknown tool, invalid params) is
			// something the model can act on; the connection is fine.
			return Result{Content: rpcErr.Message, IsError: true, RPCError: rpcErr}, nil
		}
		return Result{}, err
	}

	var res struct {
		ResultType string `json:"resultType"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			// Data is base64 on the wire for an image or audio block, and
			// mimeType names what it is. Both are decoded here rather than
			// passed along as text, because a caller handed base64 has to
			// guess whether it is a picture or a paragraph.
			Data     string `json:"data"`
			MimeType string `json:"mimeType"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return Result{}, fmt.Errorf("tools/call: malformed result: %w", err)
	}
	if err := validateResultType("tools/call", res.ResultType, secrets); err != nil {
		return Result{}, err
	}

	var b strings.Builder
	var images []provider.Image
	for _, block := range res.Content {
		switch {
		case block.Type == "text":
			b.WriteString(block.Text)
		case block.Type == "image" && block.Data != "":
			data, decodeErr := base64.StdEncoding.DecodeString(block.Data)
			if decodeErr != nil || len(data) == 0 {
				// A block that says image and is not one is named rather than
				// guessed at: handing the model undecodable bytes as a picture
				// is worse than telling it the server sent something broken.
				fmt.Fprintf(&b, "[image content the server sent could not be decoded]")
				continue
			}
			images = append(images, provider.Image{MediaType: block.MimeType, Data: data})
		default:
			fmt.Fprintf(&b, "[%s content omitted]", block.Type)
		}
	}
	// Tool output is model-visible and commonly echoed into logs or persisted
	// transcripts. A server must not be able to exfiltrate a credential that
	// Switchboard supplied on this request merely by returning it successfully.
	redactionValues := credentials
	if res.IsError {
		redactionValues = secrets
	}
	content := redactSecrets(b.String(), redactionValues)
	return Result{Content: content, IsError: res.IsError, Images: images}, nil
}

func validateResultType(method, resultType string, secrets []string) error {
	if resultType == "" || resultType == "complete" {
		return nil
	}
	return &UnsupportedResultTypeError{Method: method, ResultType: redactSecrets(resultType, secrets)}
}

func redactStringSlice(values, secrets []string) []string {
	redacted := make([]string, len(values))
	for i, value := range values {
		redacted[i] = redactSecrets(value, secrets)
	}
	return redacted
}

// call sends one request and waits for its response or the context.
func (c *Client) call(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, error) {
	raw, _, _, err := c.callWithSecrets(ctx, method, params)
	return raw, err
}

func (c *Client) callWithSecrets(ctx context.Context, method string, params json.RawMessage) (json.RawMessage, []string, []string, error) {
	return c.callVersionWithSecrets(ctx, c.protocol, method, params)
}

func (c *Client) callVersion(ctx context.Context, version, method string, params json.RawMessage) (json.RawMessage, error) {
	raw, _, _, err := c.callVersionWithSecrets(ctx, version, method, params)
	return raw, err
}

func (c *Client) callVersionWithSecrets(ctx context.Context, version, method string, params json.RawMessage) (json.RawMessage, []string, []string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	if version == modernProtocolVersion {
		var err error
		params, err = withModernMetadata(params)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("%s: request metadata: %w", method, err)
		}
	}

	id := c.seq.Add(1)
	ch := make(chan rpcResponse, 1)

	c.mu.Lock()
	if c.dead != nil {
		err := c.dead
		c.mu.Unlock()
		return nil, nil, nil, err
	}
	c.pending[id] = ch
	c.mu.Unlock()

	msg, _ := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err := c.transport.Send(ctx, msg); err != nil {
		c.forget(id)
		if ctx.Err() != nil {
			return nil, nil, nil, ctx.Err()
		}
		return nil, nil, nil, err
	}

	select {
	case resp := <-ch:
		if resp.fatal != nil {
			return nil, resp.secrets, resp.credentials, resp.fatal
		}
		if resp.Error != nil {
			return nil, resp.secrets, resp.credentials, resp.Error
		}
		return resp.Result, resp.secrets, resp.credentials, nil
	case <-ctx.Done():
		c.forget(id)
		return nil, nil, nil, ctx.Err()
	}
}

func withModernMetadata(params json.RawMessage) (json.RawMessage, error) {
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(params, &object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, errors.New("params must be a JSON object")
	}

	meta := map[string]json.RawMessage{}
	if raw := object["_meta"]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &meta); err != nil || meta == nil {
			if err == nil {
				err = errors.New("_meta must be a JSON object")
			}
			return nil, err
		}
	}
	meta["io.modelcontextprotocol/protocolVersion"], _ = json.Marshal(modernProtocolVersion)
	meta["io.modelcontextprotocol/clientInfo"], _ = json.Marshal(implementation{Name: "switchboard", Version: "dev"})
	meta["io.modelcontextprotocol/clientCapabilities"] = json.RawMessage(`{}`)
	object["_meta"], _ = json.Marshal(meta)
	return json.Marshal(object)
}

func (c *Client) forget(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// readLoop is the single reader. It dispatches responses by id, answers the
// pings a server is entitled to send, refuses the requests this client does
// not implement, and surfaces the server's log notifications. When the
// transport ends, every pending call fails with the reason.
func (c *Client) readLoop() {
	for {
		line, err := c.transport.Recv()
		if err != nil {
			var requestErr *requestTransportError
			if errors.As(err, &requestErr) {
				var id int64
				if json.Unmarshal(requestErr.ID, &id) == nil {
					c.failRequest(id, requestErr)
					continue
				}
			}
			c.failAndClose(fmt.Errorf("mcp server %s: connection closed: %w", c.spec.Name, err))
			return
		}
		line = sanitizeRPCEnvelope(line, c.secrets)
		var msg incoming
		if err := json.Unmarshal(line, &msg); err != nil {
			c.logf("warn", fmt.Sprintf("mcp %s sent unparseable output; ignoring", c.spec.Name))
			continue
		}
		switch {
		case len(msg.ID) > 0 && msg.Method == "": // response
			id, ok := clientResponseID(msg.ID)
			if !ok {
				c.logf("warn", fmt.Sprintf("mcp %s sent a response with an invalid client request id; ignoring", c.spec.Name))
				continue
			}
			secrets := append([]string(nil), c.secrets...)
			credentials := append([]string(nil), c.credentials...)
			if provider, ok := c.transport.(interface {
				takeResponseSensitive(int64) ([]string, []string)
			}); ok {
				responseSecrets, responseCredentials := provider.takeResponseSensitive(id)
				secrets = append(secrets, responseSecrets...)
				credentials = append(credentials, responseCredentials...)
			}
			msg.Error = sanitizeRPCError(msg.Error, secrets)
			c.mu.Lock()
			ch, ok := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ok {
				ch <- rpcResponse{Result: msg.Result, Error: msg.Error, secrets: secrets, credentials: credentials}
			}
		case len(msg.ID) > 0: // server-initiated request
			if !validServerRequestID(msg.ID) {
				c.logf("warn", fmt.Sprintf("mcp %s sent a request with an invalid JSON-RPC id; ignoring", c.spec.Name))
				continue
			}
			if err := c.enqueueAnswer(msg.ID, msg.Method, msg.Params); err != nil {
				c.failAndClose(fmt.Errorf("mcp server %s: cannot queue server-request response: %w", c.spec.Name, err))
				return
			}
		default: // notification
			c.notified(msg.Method, msg.Params)
		}
	}
}

func clientResponseID(raw json.RawMessage) (int64, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed[0] != '-' && (trimmed[0] < '0' || trimmed[0] > '9') {
		return 0, false
	}
	var id int64
	if json.Unmarshal([]byte(trimmed), &id) != nil {
		return 0, false
	}
	return id, true
}

func validServerRequestID(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || !json.Valid([]byte(trimmed)) {
		return false
	}
	if trimmed[0] == '"' {
		var text string
		return json.Unmarshal([]byte(trimmed), &text) == nil
	}
	if trimmed[0] != '-' && (trimmed[0] < '0' || trimmed[0] > '9') {
		return false
	}
	var number json.Number
	if json.Unmarshal([]byte(trimmed), &number) == nil {
		return true
	}
	return false
}

// failAndClose publishes the transport failure before closing so pending calls
// never wait on process reaping. Stdio aborts its process tree first; this also
// makes Close safe when the fatal read was a framing error rather than EOF.
func (c *Client) failAndClose(err error) {
	c.fail(err)
	if transport, ok := c.transport.(*stdioTransport); ok {
		transport.abort(err)
	}
	_ = c.Close()
}

func (c *Client) failRequest(id int64, err error) {
	c.mu.Lock()
	ch, ok := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ok {
		ch <- rpcResponse{fatal: err}
	}
}

// enqueueAnswer moves server-request replies off the sole receive loop. The
// fixed queue prevents an unbounded peer-controlled backlog; each worker send
// has its own deadline so a hung legacy HTTP response POST cannot stop client
// response dispatch.
func (c *Client) enqueueAnswer(id json.RawMessage, method string, params json.RawMessage) error {
	if len(id) > maxServerReplyID {
		return fmt.Errorf("server-request id exceeds %d bytes", maxServerReplyID)
	}
	if len(method) > maxServerMethod {
		return fmt.Errorf("server-request method exceeds %d bytes", maxServerMethod)
	}
	queue, lifetime, err := c.serverReplyQueue()
	if err != nil {
		return err
	}
	reply := serverReply{
		id:     append(json.RawMessage(nil), id...),
		method: method,
		params: append(json.RawMessage(nil), params...),
	}
	select {
	case queue <- reply:
		return nil
	case <-lifetime.Done():
		return errors.New("client closed")
	default:
		return fmt.Errorf("server-request response queue exceeds %d entries", serverReplyQueueSize)
	}
}

func (c *Client) serverReplyQueue() (chan serverReply, context.Context, error) {
	c.answerOnce.Do(func() {
		c.mu.Lock()
		if c.closing || c.dead != nil {
			c.mu.Unlock()
			return
		}
		if c.lifetime == nil {
			c.lifetime, c.cancel = context.WithCancel(context.Background())
		}
		queue := make(chan serverReply, serverReplyQueueSize)
		c.answerQueue = queue
		lifetime := c.lifetime
		timeout := c.answerTimeout
		if timeout <= 0 {
			timeout = serverReplyTimeout
		}
		c.mu.Unlock()
		for range serverReplyWorkers {
			go c.answerLoop(lifetime, queue, timeout)
		}
	})
	c.mu.Lock()
	queue := c.answerQueue
	lifetime := c.lifetime
	closed := c.closing || c.dead != nil
	c.mu.Unlock()
	if queue == nil || lifetime == nil || closed {
		return nil, nil, errors.New("client closed")
	}
	return queue, lifetime, nil
}

func (c *Client) answerLoop(lifetime context.Context, queue <-chan serverReply, timeout time.Duration) {
	for {
		select {
		case <-lifetime.Done():
			return
		case reply := <-queue:
			if err := c.sendAnswer(lifetime, timeout, reply); err != nil && lifetime.Err() == nil && c.logf != nil {
				c.logf("warn", fmt.Sprintf("mcp %s: failed to answer %s: %v", c.spec.Name, reply.method, err))
			}
		}
	}
}

// sendAnswer replies to a server-initiated request.
//
// Ping gets its empty result. Elicitation is answered when a surface granted
// the role, because a question is interaction rather than an effect and the
// answer channel is a person who can refuse in person (elicit.go). Everything
// else — sampling and roots above all — is declined with method-not-found,
// because each would put this client in a role the user never granted it: a
// sampling request is the server spending the user's model budget.
//
// A schema this client will not answer is an invalid-params error rather than
// method-not-found. The distinction is what a server can act on: the method is
// served, this particular request is not, and a server told otherwise would
// stop asking altogether.
func (c *Client) sendAnswer(lifetime context.Context, timeout time.Duration, reply serverReply) error {
	type errBody struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	// The send deadline is taken after the answer exists, not before. An
	// elicitation blocks on a person, and a five-second timer started here
	// would expire while the dialog was still open: the reply would be
	// composed correctly and then fail to send, every time, for every human.
	var msg []byte
	switch {
	case reply.method == "ping":
		msg, _ = json.Marshal(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Result  map[string]any  `json:"result"`
		}{"2.0", reply.id, map[string]any{}})

	case reply.method == "elicitation/create" && c.questioner != nil:
		// The dialog gets the client's lifetime. It ends when the connection
		// does and not before, because the only honest deadline on a question
		// is how long the session lasts.
		result, err := c.answerElicitation(lifetime, reply.params)
		if err != nil {
			var unsupported *unsupportedElicit
			detail := "switchboard cannot answer this elicitation request"
			if errors.As(err, &unsupported) {
				detail = unsupported.reason
			}
			c.logf("warn", fmt.Sprintf("mcp %s: declining elicitation/create: %s", c.spec.Name, detail))
			msg, _ = json.Marshal(struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      json.RawMessage `json:"id"`
				Error   errBody         `json:"error"`
			}{"2.0", reply.id, errBody{-32602, detail}})
			break
		}
		msg, _ = json.Marshal(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Result  elicitResult    `json:"result"`
		}{"2.0", reply.id, result})

	default:
		msg, _ = json.Marshal(struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Error   errBody         `json:"error"`
		}{"2.0", reply.id, errBody{-32601, "switchboard does not serve " + reply.method}})
	}

	ctx, cancel := context.WithTimeout(lifetime, timeout)
	defer cancel()
	return c.transport.Send(ctx, msg)
}

func (c *Client) notified(method string, params json.RawMessage) {
	switch method {
	case "notifications/message":
		var p struct {
			Level string `json:"level"`
			Data  any    `json:"data"`
		}
		_ = json.Unmarshal(params, &p)
		c.logf("info", fmt.Sprintf("mcp %s: %v", c.spec.Name, p.Data))
	case "notifications/tools/list_changed":
		// Deliberately not followed mid-session: the definitions are in the
		// frozen zone (§6.1). The next Switchboard run will list again.
		c.logf("warn", fmt.Sprintf("mcp %s changed its tool list; the new set applies on the next Switchboard run", c.spec.Name))
	}
}

// fail marks the client dead and drains every waiter.
func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.dead == nil {
		c.dead = err
	}
	waiters := c.pending
	c.pending = map[int64]chan rpcResponse{}
	c.mu.Unlock()

	for _, ch := range waiters {
		ch <- rpcResponse{fatal: err}
	}
}

func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.mu.Lock()
		c.closing = true
		cancel := c.cancel
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		c.closeErr = c.transport.Close()
		c.fail(errors.New("client closed"))
	})
	return c.closeErr
}
