//go:build !windows

package tools

import "strconv"

func scriptSchemaDescription() string {
	return `One /bin/sh script, used instead of command when you need a pipe, glob, redirection, or variable: "grep -r foo . | head -20".`
}

func scriptExample() string { return "grep -r foo . | head -20" }

func describeScript(script string) string { return "sh -c " + strconv.Quote(script) }
