package main

// Prompt history that survives the session, per workspace. Muscle memory is
// the whole feature: up-arrow reaches last week's prompt in this repository,
// ctrl+r searches it incrementally, and none of it is shared across projects,
// because the prompt that made sense in one repository is noise in another.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/fileprivacy"
	"github.com/switchboard-code/switchboard/internal/terminaltext"
)

const historyKeep = 500

func historyPath(workspace string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(workspace))
	return filepath.Join(home, ".switchboard", "history", hex.EncodeToString(sum[:8])+".hist"), nil
}

// loadHistory reads the workspace's prompt history, oldest first. Each new
// entry is one JSON string, so newlines, backslashes, tabs, and Unicode round
// trip without growing a second escaping language. The old \n-only format is
// still accepted and is rewritten on the next read.
func loadHistory(workspace string) []string {
	path, err := historyPath(workspace)
	if err != nil {
		return nil
	}
	data, snapshot, err := readHistoryFile(path, nil)
	if err != nil {
		return nil
	}
	out, rewrite := decodeHistory(data, snapshot.ownerOnly)
	// A pre-fix history may already hold a recognized credential. Loading it
	// is the first safe opportunity to scrub that durable copy. The rewrite is
	// best-effort for the same reason history itself is: losing history cannot
	// stop the workbench from opening.
	if rewrite {
		_ = rewriteHistoryIfUnchanged(path, out, snapshot, nil)
	}
	return out
}

// decodeHistory is the semantic reader shared by ordinary loading and
// workspace-key migration. Equivalence is measured after the same legacy
// decoding, credential redaction, empty-entry removal, and retention bound the
// user-facing history applies, so formatting differences cannot manufacture a
// conflict and a raw legacy credential is never copied to the canonical file.
func decodeHistory(data []byte, ownerOnly bool) ([]string, bool) {
	var out []string
	rewrite := !ownerOnly
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var prompt string
		if err := json.Unmarshal([]byte(line), &prompt); err != nil {
			// Legacy history escaped only newlines. Preserve its historical
			// reading rather than rejecting a person's existing prompts.
			prompt = strings.ReplaceAll(line, "\\n", "\n")
			rewrite = true
		}
		safe := historySafe(prompt)
		if safe != prompt {
			rewrite = true
		}
		if safe != "" {
			out = append(out, safe)
		}
	}
	if len(out) > historyKeep {
		out = out[len(out)-historyKeep:]
		rewrite = true
	}
	return out, rewrite
}

// appendHistory writes one prompt through to disk. Recognized credentials are
// always redacted here, independent of what the outbound secret gate decides:
// that gate grants a provider send and a session-log copy, not a second secret
// archive in prompt history. Failure is silent because history is a
// convenience, and a warning about it would outweigh it.
func appendHistory(workspace, prompt string) {
	prompt = historySafe(prompt)
	if prompt == "" {
		return
	}
	path, err := historyPath(workspace)
	if err != nil {
		return
	}
	if err := fileprivacy.EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return
	}
	_ = appendHistoryPrompt(path, prompt, nil)
}

func historySafe(prompt string) string {
	if leaks := credential.ScanPrompt(prompt); len(leaks) > 0 {
		return credential.Redact(prompt, leaks)
	}
	return prompt
}

// rememberPrompt is the one in-memory and durable history seam for sends and
// steers. It deliberately remembers the redacted spelling even when the user
// explicitly authorizes sending the original to a provider.
func (m *tuiModel) rememberPrompt(prompt string) {
	prompt = historySafe(prompt)
	if prompt == "" {
		return
	}
	if len(m.history) == 0 || m.history[len(m.history)-1] != prompt {
		m.history = append(m.history, prompt)
		appendHistory(m.app.workspace, prompt)
	}
	if len(m.history) > historyKeep {
		m.history = m.history[len(m.history)-historyKeep:]
		// Trim the durable copy through its conditional atomic rewrite. If
		// another process appends between read and publish, loadHistory refuses
		// the stale rewrite and the newer prompt wins.
		_ = loadHistory(m.app.workspace)
	}
	m.resetHistoryNavigation()
}

// --- reverse search ----------------------------------------------------------

// startHistorySearch is ctrl+r at the prompt: incremental search over this
// workspace's history, newest match first, ctrl+r again for the next older.
func (m *tuiModel) startHistorySearch() {
	m.histSearch = true
	m.histQuery = ""
	m.histMatch = -1
}

// historySearchKey handles one keypress while searching. It returns false
// when the search is over and the key should fall through to normal handling.
func (m *tuiModel) historySearchKey(msg tea.KeyMsg) bool {
	switch msg.String() {
	case "esc", "ctrl+c":
		m.histSearch = false
		return true
	case "enter":
		if hit := m.historyMatch(m.histMatch); hit != "" {
			m.ta.SetValue(hit)
			m.ta.CursorEnd()
			m.resetHistoryNavigation()
			m.growInput()
		}
		m.histSearch = false
		return true
	case "ctrl+r":
		if next := m.historyMatch(m.histMatch + 1); next != "" {
			m.histMatch++
		}
		return true
	case "backspace":
		if m.histQuery != "" {
			// By rune, not by byte: deleting the last byte of a multi-byte
			// character would leave the query invalid UTF-8.
			runes := []rune(m.histQuery)
			m.histQuery = string(runes[:len(runes)-1])
			m.histMatch = m.firstMatch()
		}
		return true
	}
	if msg.Type == tea.KeyRunes {
		m.histQuery += string(msg.Runes)
		m.histMatch = m.firstMatch()
		return true
	}
	return true // swallow everything else; search owns the keyboard
}

func (m *tuiModel) firstMatch() int {
	if m.historyMatch(0) != "" {
		return 0
	}
	return -1
}

// historyMatch returns the nth newest history entry containing the query,
// or "" when there is no such match.
func (m *tuiModel) historyMatch(n int) string {
	if n < 0 {
		return ""
	}
	query := strings.ToLower(m.histQuery)
	seen := 0
	for i := len(m.history) - 1; i >= 0; i-- {
		if query == "" || strings.Contains(strings.ToLower(m.history[i]), query) {
			if seen == n {
				return m.history[i]
			}
			seen++
		}
	}
	return ""
}

func (m *tuiModel) historySearchView() string {
	hit := m.historyMatch(m.histMatch)
	line := "(reverse-search) " + terminaltext.Escape(m.histQuery)
	if hit != "" {
		line += m.th.dim.Render("  → " + terminaltext.Escape(firstLine(strings.ReplaceAll(hit, "\n", " "))))
	} else if m.histQuery != "" {
		line += m.th.dim.Render("  no match")
	}
	return m.th.accent.Render(line)
}
