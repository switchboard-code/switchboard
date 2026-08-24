package mcppolicy

import (
	"fmt"
	"os/user"
	"path"
	"path/filepath"
	"strings"
)

// ResolvePaths returns official native policy paths for macOS, Linux/WSL, and
// Windows. It performs no filesystem access.
func ResolvePaths(options Options) (Paths, error) {
	goos := platform(options)
	home, err := absoluteCleanPlatform(goos, options.HomeDir)
	if err != nil {
		return Paths{}, fmt.Errorf("native MCP policy home: %w", err)
	}
	workspace, err := absoluteCleanPlatform(goos, options.Workspace)
	if err != nil {
		return Paths{}, fmt.Errorf("native MCP policy workspace: %w", err)
	}

	codexDir := options.CodexConfigDir
	if strings.TrimSpace(codexDir) == "" {
		codexDir = joinPlatform(goos, home, ".codex")
	} else if codexDir, err = absoluteCleanPlatform(goos, codexDir); err != nil {
		return Paths{}, fmt.Errorf("native MCP policy Codex config directory: %w", err)
	}
	claudeDir := options.ClaudeConfigDir
	if strings.TrimSpace(claudeDir) == "" {
		claudeDir = joinPlatform(goos, home, ".claude")
	} else if claudeDir, err = absoluteCleanPlatform(goos, claudeDir); err != nil {
		return Paths{}, fmt.Errorf("native MCP policy Claude config directory: %w", err)
	}

	result := Paths{
		CodexAuth:             joinPlatform(goos, codexDir, "auth.json"),
		ClaudeRemoteSettings:  joinPlatform(goos, claudeDir, "remote-settings.json"),
		ClaudeState:           joinPlatform(goos, home, ".claude.json"),
		ClaudeUserSettings:    joinPlatform(goos, claudeDir, "settings.json"),
		ClaudeProjectSettings: joinPlatform(goos, workspace, ".claude", "settings.json"),
		ClaudeLocalSettings:   joinPlatform(goos, workspace, ".claude", "settings.local.json"),
	}
	if strings.TrimSpace(options.ClaudeConfigDir) != "" {
		// Claude Code and the command runtime place project state beside the
		// configured settings directory when CLAUDE_CONFIG_DIR is overridden.
		result.ClaudeState = joinPlatform(goos, claudeDir, ".claude.json")
	}

	switch goos {
	case "windows":
		programData := options.ProgramData
		if strings.TrimSpace(programData) == "" {
			programData = `C:\ProgramData`
		} else if programData, err = absoluteCleanPlatform(goos, programData); err != nil {
			return Paths{}, fmt.Errorf("native MCP policy ProgramData: %w", err)
		}
		programFiles := options.ProgramFiles
		if strings.TrimSpace(programFiles) == "" {
			programFiles = `C:\Program Files`
		} else if programFiles, err = absoluteCleanPlatform(goos, programFiles); err != nil {
			return Paths{}, fmt.Errorf("native MCP policy ProgramFiles: %w", err)
		}
		result.CodexRequirements = joinPlatform("windows", programData, "OpenAI", "Codex", "requirements.toml")
		claudeSystem := joinPlatform("windows", programFiles, "ClaudeCode")
		result.ClaudeManagedSettings = joinPlatform("windows", claudeSystem, "managed-settings.json")
		result.ClaudeManagedDropIns = joinPlatform("windows", claudeSystem, "managed-settings.d")
		result.ClaudeManagedMCP = joinPlatform("windows", claudeSystem, "managed-mcp.json")
	case "darwin":
		result.CodexRequirements = "/etc/codex/requirements.toml"
		claudeSystem := "/Library/Application Support/ClaudeCode"
		result.ClaudeManagedSettings = joinPlatform(goos, claudeSystem, "managed-settings.json")
		result.ClaudeManagedDropIns = joinPlatform(goos, claudeSystem, "managed-settings.d")
		result.ClaudeManagedMCP = joinPlatform(goos, claudeSystem, "managed-mcp.json")
		name := strings.TrimSpace(options.UserName)
		if name == "" {
			if current, currentErr := user.Current(); currentErr == nil {
				name = current.Username
			}
		}
		result.CodexMDM = managedPreferencePaths(name, "com.openai.codex")
		result.ClaudeMDM = managedPreferencePaths(name, "com.anthropic.claudecode")
	default:
		result.CodexRequirements = "/etc/codex/requirements.toml"
		claudeSystem := "/etc/claude-code"
		result.ClaudeManagedSettings = joinPlatform(goos, claudeSystem, "managed-settings.json")
		result.ClaudeManagedDropIns = joinPlatform(goos, claudeSystem, "managed-settings.d")
		result.ClaudeManagedMCP = joinPlatform(goos, claudeSystem, "managed-mcp.json")
	}
	return result, nil
}

func managedPreferencePaths(userName, domain string) []string {
	paths := []string{path.Join("/Library/Managed Preferences", domain+".plist")}
	if userName != "" && !strings.ContainsAny(userName, `/\\`) {
		paths = append([]string{path.Join("/Library/Managed Preferences", userName, domain+".plist")}, paths...)
	}
	return paths
}

func absoluteClean(value string) (string, error) {
	return absoluteCleanPlatform("", value)
}

func absoluteCleanPlatform(goos, value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("path is empty")
	}
	if goos == "windows" {
		converted := strings.ReplaceAll(value, `\`, "/")
		if strings.HasPrefix(converted, "//") {
			components := strings.Split(strings.TrimPrefix(converted, "//"), "/")
			if len(components) < 2 || components[0] == "" || components[1] == "" {
				return "", fmt.Errorf("path is not an absolute Windows UNC path")
			}
			cleaned := path.Clean("/" + strings.TrimPrefix(converted, "//"))
			return `\\` + strings.ReplaceAll(strings.TrimPrefix(cleaned, "/"), "/", `\`), nil
		}
		if len(converted) < 3 || converted[1] != ':' || converted[2] != '/' ||
			((converted[0] < 'A' || converted[0] > 'Z') && (converted[0] < 'a' || converted[0] > 'z')) {
			return "", fmt.Errorf("path is not an absolute Windows path")
		}
		return strings.ReplaceAll(path.Clean(converted), "/", `\`), nil
	}
	// ResolvePaths describes the requested target platform, which may differ
	// from the host running discovery or its tests. POSIX targets therefore use
	// path rather than filepath so a Windows host cannot rewrite /home into a
	// drive-qualified path or introduce backslashes into Linux/macOS policy
	// surfaces.
	if goos != "" {
		if !path.IsAbs(value) {
			return "", fmt.Errorf("path is not absolute")
		}
		return path.Clean(value), nil
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("path cannot be made absolute")
	}
	return filepath.Clean(abs), nil
}

func joinPlatform(goos string, elements ...string) string {
	if goos == "" {
		return filepath.Join(elements...)
	}
	if goos != "windows" {
		return path.Join(elements...)
	}
	if len(elements) == 0 {
		return ""
	}
	converted := make([]string, 0, len(elements))
	unc := false
	for index, element := range elements {
		element = strings.ReplaceAll(element, `\`, "/")
		if index == 0 && strings.HasPrefix(element, "//") {
			unc = true
			element = strings.TrimPrefix(element, "//")
		}
		if index > 0 {
			element = strings.Trim(element, "/")
		}
		converted = append(converted, element)
	}
	joined := strings.ReplaceAll(path.Join(converted...), "/", `\`)
	if unc {
		return `\\` + strings.TrimPrefix(joined, `\`)
	}
	return joined
}
