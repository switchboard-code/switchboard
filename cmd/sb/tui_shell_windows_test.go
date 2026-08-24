//go:build windows

package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestWindowsShellInvocationUsesComSpecAndCmdSyntax(t *testing.T) {
	const comspec = `C:\Windows\System32\cmd.exe`
	t.Setenv("COMSPEC", comspec)
	name, args := userShellInvocation("echo switchboard")
	if name != comspec {
		t.Fatalf("shell = %q, want COMSPEC %q", name, comspec)
	}
	if want := []string{"/d", "/s", "/c", "echo switchboard"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
}

func TestWindowsShellRunnerExecutesInWorkspace(t *testing.T) {
	m := testModel(t)
	m.app.workspace = t.TempDir()
	cmd := m.runShell("echo switchboard-windows")
	if cmd == nil {
		t.Fatal("shell command did not launch")
	}
	msg, ok := cmd().(shellDoneMsg)
	if !ok {
		t.Fatal("shell command returned an unexpected message")
	}
	if msg.err != nil || msg.result.kind != shellSucceeded || !strings.Contains(msg.output, "switchboard-windows") {
		t.Fatalf("Windows shell result=%+v err=%v output=%q", msg.result, msg.err, msg.output)
	}
}
