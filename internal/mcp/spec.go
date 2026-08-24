package mcp

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/switchboard-code/switchboard/internal/rootedfs"
)

// SpecFileName is the file consulted in ~/.switchboard and in a workspace's
// .switchboard directory. It is its own file rather than a table in
// config.toml because config.toml is regenerated from typed state on every
// TUI settings change, and a hand-maintained server list does not belong in
// a file the program rewrites.
const SpecFileName = "mcp.toml"

const maxSpecFileBytes = 1 << 20

type specEntry struct {
	Command       string            `toml:"command"`
	Args          []string          `toml:"args"`
	Env           map[string]string `toml:"env"`
	CWD           string            `toml:"cwd"`
	RestrictedEnv bool              `toml:"restricted_env"`
	InheritEnv    []string          `toml:"inherit_env"`

	URL               string            `toml:"url"`
	Headers           map[string]string `toml:"headers"`
	HeaderEnv         map[string]string `toml:"header_env"`
	BearerTokenEnvVar string            `toml:"bearer_token_env"`

	StartupTimeoutSeconds float64 `toml:"startup_timeout_seconds"`
	ToolTimeoutSeconds    float64 `toml:"tool_timeout_seconds"`

	EnabledTools  []string `toml:"enabled_tools"`
	DisabledTools []string `toml:"disabled_tools"`
	Required      bool     `toml:"required"`
	Allow         []string `toml:"allow"`
}

// LoadSpecs reads one server file. A missing file is an empty list, not an
// error: most machines and most repositories have none.
func LoadSpecs(path string) ([]Spec, error) {
	return LoadSpecsRooted(filepath.Dir(path), filepath.Base(path))
}

// LoadSpecsRooted binds the declaration to an authority root. Repository
// callers pass the workspace and user callers pass the home directory, so a
// parent-directory rename cannot redirect the read outside that authority.
func LoadSpecsRooted(root, name string) ([]Spec, error) {
	return loadSpecsRootedWithHook(root, name, nil)
}

func loadSpecsRootedWithHook(root, name string, beforeOpen func()) ([]Spec, error) {
	var file struct {
		MCP map[string]specEntry `toml:"mcp"`
	}
	path := filepath.Join(root, filepath.FromSlash(name))
	data, err := rootedfs.ReadFileWithHook(root, name, maxSpecFileBytes, beforeOpen)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	metadata, err := toml.Decode(string(data), &file)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	names := make([]string, 0, len(file.MCP))
	for name := range file.MCP {
		names = append(names, name)
	}
	sort.Strings(names)

	specs := make([]Spec, 0, len(names))
	for _, name := range names {
		e := file.MCP[name]
		startupTimeout, err := secondsDuration(e.StartupTimeoutSeconds)
		if err != nil {
			return nil, fmt.Errorf("%s: mcp server %s: invalid startup timeout", path, name)
		}
		toolTimeout, err := secondsDuration(e.ToolTimeoutSeconds)
		if err != nil {
			return nil, fmt.Errorf("%s: mcp server %s: invalid tool timeout", path, name)
		}
		s := Spec{
			Name:              name,
			Command:           e.Command,
			Args:              e.Args,
			Env:               e.Env,
			CWD:               e.CWD,
			RestrictedEnv:     e.RestrictedEnv,
			InheritEnv:        e.InheritEnv,
			URL:               e.URL,
			Headers:           e.Headers,
			HeaderEnv:         e.HeaderEnv,
			BearerTokenEnvVar: e.BearerTokenEnvVar,
			StartupTimeout:    startupTimeout,
			ToolTimeout:       toolTimeout,
			EnabledTools:      e.EnabledTools,
			EnabledToolsSet:   metadata.IsDefined("mcp", name, "enabled_tools"),
			DisabledTools:     e.DisabledTools,
			DisabledToolsSet:  metadata.IsDefined("mcp", name, "disabled_tools"),
			Required:          e.Required,
			Allow:             e.Allow,
		}
		if err := s.validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		specs = append(specs, s)
	}
	return specs, nil
}

func secondsDuration(seconds float64) (time.Duration, error) {
	if seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds > float64(math.MaxInt64)/float64(time.Second) {
		return 0, fmt.Errorf("invalid duration")
	}
	return time.Duration(seconds * float64(time.Second)), nil
}
