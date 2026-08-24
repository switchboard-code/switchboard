package credential

import (
	"context"
	"os/exec"

	"github.com/switchboard-code/switchboard/internal/execution"
)

// runCredentialCommand owns the complete subprocess lifetime. Credential
// helpers may start descendants that retain stdout, stderr, or stdin after the
// direct child exits; exec.CommandContext can kill only that direct child and
// then wait forever for the retained pipe. The shared execution boundary puts
// the child in its own process group where supported and bounds cancellation,
// pipe cleanup, and reaping on every platform.
func runCredentialCommand(ctx context.Context, cmd *exec.Cmd) error {
	return execution.RunProcess(ctx, cmd)
}
