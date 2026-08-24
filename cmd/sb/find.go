package main

// /find and `sb find`: search the workspace's recorded sessions by content.
// "Which session did I fix the runner race in" is a question the picker's
// first-words labels cannot answer once the day is long; this greps what
// was actually said — the user's prompts and the model's answers — and
// hands back the ids /resume takes. Read-only over the logs, the open one
// included, the same posture as sb cost.

import (
	"fmt"
	"io"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/provider"
	"github.com/switchboard-code/switchboard/internal/session"
)

const findMaxSessions = 20

// findAllLines searches every workspace the store holds, the workspace
// resolved from each log's own header, and says where each match lives -
// the cross-workspace question is "which project was that", so the
// project is the answer's first word.
func findAllLines(store *session.Store, query string) []string {
	byWorkspace, err := store.ListAll()
	if err != nil {
		return []string{"  " + err.Error()}
	}
	if len(byWorkspace) == 0 {
		return []string{"  no sessions recorded anywhere yet"}
	}
	workspaces := make([]string, 0, len(byWorkspace))
	for ws := range byWorkspace {
		workspaces = append(workspaces, ws)
	}
	sort.Strings(workspaces)

	needle := strings.ToLower(query)
	var lines []string
	matched, scanned := 0, 0
	for _, ws := range workspaces {
		for _, info := range byWorkspace[ws] {
			scanned++
			state, err := session.ReadState(info.Path)
			if err != nil {
				continue
			}
			hits, snippet := searchMessages(state.Messages, needle)
			if hits == 0 {
				continue
			}
			matched++
			if matched > findMaxSessions {
				lines = append(lines, fmt.Sprintf("  … more sessions match; a narrower query sees past the first %d", findMaxSessions))
				return append(lines, "  sb -workspace <path> then /resume <id> picks one up")
			}
			word := "match"
			if hits > 1 {
				word = "matches"
			}
			lines = append(lines,
				fmt.Sprintf("  %s", ws),
				fmt.Sprintf("    %s  %s  %d %s", state.ID, info.Modified.Local().Format("Jan 2 15:04"), hits, word),
				"      "+snippet)
		}
	}
	if matched == 0 {
		return []string{fmt.Sprintf("  nothing in %d sessions across %d workspaces says %q", scanned, len(byWorkspace), query)}
	}
	return append(lines, "  sb -workspace <path> then /resume <id> picks one up")
}

func findLines(store *session.Store, workspace, query string) []string {
	infos, err := store.List(workspace)
	if err != nil {
		return []string{"  " + err.Error()}
	}
	if len(infos) == 0 {
		return []string{"  no sessions recorded for this workspace yet"}
	}

	needle := strings.ToLower(query)
	var lines []string
	matched := 0
	for _, info := range infos {
		state, err := session.ReadState(info.Path)
		if err != nil {
			continue
		}
		hits, snippet := searchMessages(state.Messages, needle)
		if hits == 0 {
			continue
		}
		matched++
		if matched > findMaxSessions {
			lines = append(lines, fmt.Sprintf("  … more sessions match; a narrower query sees past the first %d", findMaxSessions))
			break
		}
		opening, _ := session.ReadOpeningSummary(info.Path)
		label := safeOpeningText(opening)
		if label == "" {
			if opening.Found {
				label = "(authored wording unavailable for this legacy session)"
			} else {
				label = "(no prompt recorded)"
			}
		}
		word := "match"
		if hits > 1 {
			word = "matches"
		}
		lines = append(lines,
			fmt.Sprintf("  %s  %s  %d %s", state.ID, info.Modified.Local().Format("Jan 2 15:04"), hits, word),
			"    "+redactCredentialTextBeforeTruncate(label, 70),
			"    "+snippet)
	}
	if matched == 0 {
		return []string{fmt.Sprintf("  nothing in %d sessions says %q", len(infos), query)}
	}
	lines = append(lines, "  /resume <id> picks one up")
	return lines
}

// searchMessages counts case-insensitive hits across what was said — user
// and assistant text, not tool payloads, because the question is about the
// conversation — and returns the first matching line as the snippet.
func searchMessages(messages []provider.Message, needle string) (int, string) {
	hits := 0
	snippet := ""
	for _, msg := range messages {
		if msg.Role != provider.RoleUser && msg.Role != provider.RoleAssistant {
			continue
		}
		var texts []string
		switch msg.Role {
		case provider.RoleAssistant:
			for _, block := range msg.Content {
				if text, ok := block.(provider.Text); ok {
					texts = append(texts, text.Text)
				}
			}
		case provider.RoleUser:
			// Search user-authored words, not provider-expanded files, shell
			// output, carried synthetic context, or machine injections. A verified
			// user steer remains authored conversation despite its injected wire
			// position.
			if msg.Synthetic || (msg.Injected && !msg.UserSteer) || (!msg.Injected && !session.OpensTurn(msg)) {
				continue
			}
			authored, known := msg.AuthoredProjection()
			if !known {
				continue
			}
			texts = append(texts, authored)
		}
		for _, text := range texts {
			for _, line := range strings.Split(text, "\n") {
				n := strings.Count(strings.ToLower(line), needle)
				if n == 0 {
					continue
				}
				hits += n
				if snippet == "" {
					snippet = redactCredentialTextBeforeTruncate(strings.TrimSpace(line), 70)
				}
			}
		}
	}
	return hits, snippet
}

func cmdFind(m *tuiModel, args string) tea.Cmd {
	query := strings.TrimSpace(args)
	if query == "" {
		return noticeCmd("error", "/find takes text to search this workspace's sessions for; /find all <text> spans workspaces")
	}
	if rest, ok := strings.CutPrefix(query, "all "); ok {
		rest = strings.TrimSpace(rest)
		if rest == "" {
			return noticeCmd("error", "/find all takes the text to search every workspace for")
		}
		m.addInfo(fmt.Sprintf("sessions saying %q, every workspace\n", rest) +
			strings.Join(findAllLines(m.app.store, rest), "\n"))
		return nil
	}
	m.addInfo(fmt.Sprintf("sessions saying %q\n", query) +
		strings.Join(findLines(m.app.store, m.app.workspace, query), "\n"))
	return nil
}

func runFindCLI(w io.Writer, store *session.Store, workspace, query string) error {
	query = strings.TrimSpace(query)
	if query == "" {
		return fmt.Errorf("sb find takes text to search this workspace's sessions for; sb find all <text> spans workspaces")
	}
	if rest, ok := strings.CutPrefix(query, "all "); ok {
		for _, line := range findAllLines(store, strings.TrimSpace(rest)) {
			fmt.Fprintln(w, cliText(strings.TrimRight(line, " ")))
		}
		return nil
	}
	for _, line := range findLines(store, workspace, query) {
		fmt.Fprintln(w, cliText(strings.TrimRight(line, " ")))
	}
	return nil
}
