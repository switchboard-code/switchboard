package agent

import (
	"github.com/switchboard-code/switchboard/internal/credential"
	"github.com/switchboard-code/switchboard/internal/provider"
)

// redactCredentialText removes recognized credential shapes while the text is
// still whole. Callers use it before a later size bound can cut a credential
// below ScanPrompt's deliberately precise length floor.
func redactCredentialText(text string) string {
	leaks := credential.ScanPrompt(text)
	if len(leaks) == 0 {
		return text
	}
	return credential.Redact(text, leaks)
}

// redactSystemBlocks is the last credential boundary before a canonical
// request can reach token accounting, routing, caching, or a provider. It
// always owns the returned slice and canonicalizes text pointers to values, so
// redaction cannot mutate Loop.System or a caller-held block through an alias.
// Non-text blocks are preserved for the adapter to reject with its ordinary
// typed capability error; silently dropping one here would change semantics.
func redactSystemBlocks(blocks []provider.Block) []provider.Block {
	if blocks == nil {
		return nil
	}
	out := make([]provider.Block, len(blocks))
	for i, block := range blocks {
		switch text := block.(type) {
		case provider.Text:
			text.Text = redactCredentialText(text.Text)
			out[i] = text
		case *provider.Text:
			if text != nil {
				out[i] = provider.Text{Text: redactCredentialText(text.Text)}
			}
		default:
			out[i] = block
		}
	}
	return out
}

// redactHistoricalToolResults owns the provider-facing replay copy. Current
// results are already redacted before they enter the session, but a session
// created by an older binary can legitimately contain raw tool output. Resume
// must not turn that durable legacy record into a fresh credential egress.
// User and assistant messages are deliberately untouched: an explicit
// send-as-typed choice remains the user's authority, not a policy this replay
// migration silently narrows.
func redactHistoricalToolResults(messages []provider.Message) []provider.Message {
	out := provider.CloneMessages(messages)
	for i := range out {
		for j, raw := range out[i].Content {
			switch result := raw.(type) {
			case provider.ToolResult:
				result.Content = redactCredentialText(result.Content)
				out[i].Content[j] = result
			case *provider.ToolResult:
				if result != nil {
					copy := *result
					copy.Content = redactCredentialText(copy.Content)
					out[i].Content[j] = copy
				}
			}
		}
	}
	return out
}
