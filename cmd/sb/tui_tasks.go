package main

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/switchboard-code/switchboard/internal/catalog"
	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/delegate"
)

type taskSteerer interface {
	Steer(id, message string) error
}

func cmdTasks(m *tuiModel, args string) tea.Cmd {
	parentID := m.app.loop.Session.ID()
	fields := strings.Fields(args)
	if len(fields) > 0 && fields[0] == "steer" {
		id, message, ok := parseSteerArgs(args)
		if !ok {
			return noticeCmd("warn", "usage: /tasks steer <id> <what to tell it>")
		}
		return steerTask(m, parentID, id, message)
	}
	if len(fields) > 0 {
		if len(fields) != 2 || fields[0] != "cancel" {
			return noticeCmd("warn", "usage: /tasks [cancel <id>] [steer <id> <message>]")
		}
		var found *delegate.TaskSnapshot
		for _, task := range tasksForSession(subagentTasks, parentID) {
			if task.ID == fields[1] {
				copy := task
				found = &copy
				break
			}
		}
		if found == nil {
			return noticeCmd("warn", "no delegate task "+workspaceSanitize(fields[1])+" belongs to this session")
		}
		if err := subagentTasks.Cancel(found.ID); err != nil {
			return noticeCmd("warn", err.Error())
		}
		return noticeCmd("", "cancelling "+found.ID+" "+workspaceSanitize(found.Name)+"; other delegate tasks keep running")
	}

	m.addInfo(renderTasks(tasksForSession(subagentTasks, parentID), subagentTasks.MaxParallel(), parentID))
	return nil
}

// parseSteerArgs splits "steer <id> <message>" without counting offsets. The
// message is everything after the id, kept whole: a correction that arrives
// with its first word eaten is worse than one that does not arrive.
func parseSteerArgs(args string) (id, message string, ok bool) {
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args), "steer"))
	id, message, cut := strings.Cut(rest, " ")
	message = strings.TrimSpace(message)
	if !cut || id == "" || message == "" {
		return "", "", false
	}
	return id, message, true
}

// steerTask sends guidance to a task this session owns. A subagent takes it up
// at its next round boundary, which is the only place a message can be handed
// to a model that is already working.
func steerTask(m *tuiModel, parentID, id, message string) tea.Cmd {
	var found *delegate.TaskSnapshot
	for _, task := range tasksForSession(subagentTasks, parentID) {
		if task.ID == id {
			copied := task
			found = &copied
			break
		}
	}
	if found == nil {
		return noticeCmd("warn", "no delegate task "+workspaceSanitize(id)+" belongs to this session")
	}
	return guardedTaskSteer(m, subagentTasks, found.ID, message)
}

// guardedTaskSteer is the only UI seam that hands new text to a running
// delegate. It uses the same outbound credential gate as a primary steer:
// child sessions and child providers are no less external merely because the
// task was launched by this process.
func guardedTaskSteer(m *tuiModel, target taskSteerer, id, message string) tea.Cmd {
	leaks := credential.ScanPrompt(message)
	send := func(safe string) tea.Cmd {
		if err := target.Steer(id, safe); err != nil {
			return noticeCmd("warn", err.Error())
		}
		return noticeCmd("", "told "+id+"; it reads this when its current round finishes")
	}
	if len(leaks) > 0 {
		return m.openSecretGate(leaks, message, send)
	}
	return send(message)
}

func tasksForSession(manager *delegate.TaskManager, parentID string) []delegate.TaskSnapshot {
	var out []delegate.TaskSnapshot
	for _, task := range manager.List() {
		if task.ParentSessionID == parentID {
			out = append(out, task)
		}
	}
	return out
}

func renderTasks(tasks []delegate.TaskSnapshot, maxParallel int, parentID string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "delegate tasks · session %s · at most %d active\n", workspaceSanitize(parentID), maxParallel)
	if len(tasks) == 0 {
		b.WriteString("\n  none in this session")
		return b.String()
	}
	b.WriteString("\n  id        status      tier    cost          name\n")
	for _, task := range tasks {
		cost := "0 observed"
		if task.CostMicroUSD > 0 {
			cost = catalog.Money(task.CostMicroUSD).String()
		}
		fmt.Fprintf(&b, "  %-9s %-11s %-7s %-13s %s\n",
			workspaceSanitize(task.ID), workspaceSanitize(string(task.Status)),
			workspaceSanitize(task.Tier), cost, workspaceSanitize(task.Name))
		subsession := task.DelegateSessionID
		if subsession == "" {
			subsession = "pending"
		}
		fmt.Fprintf(&b, "    parent %s · delegate %s · %d calls\n",
			workspaceSanitize(task.ParentSessionID), workspaceSanitize(subsession), task.Calls)
		if pending := task.SteersSent - task.SteersApplied; task.SteersSent > 0 {
			// Sent and taken up are different facts while a round is in
			// flight, and "did it get my message" is the question being asked.
			line := fmt.Sprintf("    steered %d time(s)", task.SteersSent)
			if pending > 0 {
				line += fmt.Sprintf(", %d waiting for the current round to finish", pending)
			}
			fmt.Fprintf(&b, "%s\n", line)
		}
		for _, what := range task.Activity {
			fmt.Fprintf(&b, "      %s\n", workspaceSanitize(what))
		}
		if task.Error != "" {
			fmt.Fprintf(&b, "    %s\n", workspaceSanitize(task.Error))
		}
	}
	b.WriteString("\n  /tasks cancel <id> stops one; /tasks steer <id> <message> tells one something mid-task")
	return strings.TrimRight(b.String(), "\n")
}
