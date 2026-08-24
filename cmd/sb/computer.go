package main

// Assembly for the computer-use tool: the platform gate lives here because
// which OS this is and what binaries it carries are surface concerns, not
// the tool suite's.

import (
	"context"
	"os"
	"runtime"

	"github.com/switchboard-code/switchboard/internal/tools"
)

const osascriptExecutable = "/usr/bin/osascript"

// computerExecutable is a fixed system capability, not a PATH-resolved
// extension. A checkout may influence PATH before sb starts; that must not let
// a repository-provided osascript impersonate a macOS system binary.
func computerExecutable() (string, bool) {
	if runtime.GOOS != "darwin" {
		return "", false
	}
	info, err := os.Stat(osascriptExecutable)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", false
	}
	return osascriptExecutable, true
}

// addComputerUse registers the computer tool where the machine can serve
// it: macOS, with osascript present, which every macOS ships. Absent is
// absent, not broken — on any other platform the model never sees the
// tool. Whether the terminal actually holds the Accessibility grant is
// deliberately not probed here: the probe can pop a consent dialog, and
// session assembly is no moment to interrupt the user's screen. The first
// call surfaces the grant state with its remedy, and `sb doctor` probes it
// on request, where a dialog is the point.
func addComputerUse(registry *tools.Registry) {
	binary, ok := computerExecutable()
	if !ok {
		return
	}
	_ = registry.AddExternal(tools.NewComputer(binary))
}

// doctorComputerRow probes the accessibility grant live, macOS only —
// elsewhere the tool is absent by platform and a row would name nothing
// the user can do. An ungranted terminal is a standing, not a failure,
// the astgrep framing: the row names the remedy without a mark. The probe
// may pop the consent dialog; doctor is the one moment that dialog is the
// point rather than an interruption.
func doctorComputerRow(ctx context.Context) (doctorRow, bool) {
	if runtime.GOOS != "darwin" {
		return doctorRow{}, false
	}
	binary, ok := computerExecutable()
	if !ok {
		return doctorRow{label: "computer", detail: "osascript not found; computer use is absent"}, true
	}
	probeCtx, cancel := context.WithTimeout(ctx, doctorProbeTimeout)
	defer cancel()
	if err := tools.ProbeComputer(probeCtx, binary); err != nil {
		return doctorRow{label: "computer",
			detail: redactCredentialTextBeforeTruncate(err.Error(), 90) + "; grant it under System Settings > Privacy & Security > Accessibility"}, true
	}
	return doctorRow{label: "computer", detail: "accessibility granted; the computer tool is in the suite"}, true
}
