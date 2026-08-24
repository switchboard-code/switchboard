//go:build windows

package tools

import "strconv"

func scriptSchemaDescription() string {
	return `One cmd.exe script, used instead of command when you need a pipeline, command chaining, redirection, or %VARIABLE% expansion: "findstr /s /n foo * | more".`
}

func scriptExample() string { return "findstr /s /n foo * | more" }

func describeScript(script string) string { return "cmd.exe /c " + strconv.Quote(script) }
