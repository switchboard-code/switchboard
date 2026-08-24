package main

import "github.com/switchboard-code/switchboard/internal/terminaltext"

// cliText is the plain-terminal boundary for repository-, session-, provider-,
// and error-authored components. Credential redaction runs on the complete
// component before terminal escaping or any caller-applied display cap.
func cliText(text string) string {
	return terminaltext.Escape(redactCredentialText(text))
}
