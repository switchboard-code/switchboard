package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
)

// splitExtensionAction keeps the selector as one opaque argument. Native
// identities may be exact paths, and paths may contain whitespace.
func splitExtensionAction(input string) []string {
	input = strings.TrimSpace(input)
	if input == "" {
		return nil
	}
	separator := strings.IndexFunc(input, unicode.IsSpace)
	if separator < 0 {
		return []string{input}
	}
	action := input[:separator]
	selector := strings.TrimSpace(input[separator:])
	if selector == "" {
		return []string{action}
	}
	return []string{action, selector}
}

func (m *tuiModel) startExtensionAction(name, kind string, run func(context.Context, io.Writer) error) tea.Cmd {
	ctx, generation, sourceID, err := m.startOperation(name)
	if err != nil {
		return noticeCmd("warn", err.Error())
	}
	return m.ownOperationCmd(generation, func() tea.Msg {
		result := extensionActionMsg{kind: kind, operation: generation, sourceID: sourceID}
		if err := ctx.Err(); err != nil {
			result.err = err
			return result
		}
		var output strings.Builder
		result.err = run(ctx, &output)
		if err := ctx.Err(); err != nil {
			result.err = err
			return result
		}
		result.output = strings.TrimRight(output.String(), "\n")
		return result
	})
}

func (m *tuiModel) onExtensionAction(msg extensionActionMsg) tea.Cmd {
	if !m.operationMatches(msg.operation, msg.sourceID) {
		return nil
	}
	name := m.operationName
	cancelled := m.operationCancelling || errors.Is(msg.err, context.Canceled)
	m.finishOperation(msg.operation, false)
	switch {
	case cancelled:
		m.addNotice("", name+" cancelled")
	case msg.err != nil:
		m.addNotice("error", msg.err.Error())
	case msg.output != "":
		m.addInfo(msg.output)
	}
	return m.nextQueuedTurn()
}
