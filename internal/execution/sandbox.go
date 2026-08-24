package execution

// NetworkAccess is how much network a confined command gets.
//
// There is no "nothing at all" option. A test suite that stands up a fixture
// server on an ephemeral loopback port is the single most common thing an
// agent runs, and denying that makes the sandbox unusable for the work. The
// On Linux a private network namespace denies egress off the machine. macOS
// Seatbelt can restrict sockets to host loopback but cannot isolate local
// services, so proxy environment is stripped and localhost services remain an
// explicit trust boundary. Callers must not describe Loopback as "no network."
type NetworkAccess string

const (
	NetworkLoopback NetworkAccess = "loopback"
	NetworkFull     NetworkAccess = "full"
)

// Policy is the per-command confinement request.
type Policy struct {
	Workspace string
	Network   NetworkAccess

	// readOnlyRoots are exact, identity-derived toolchain roots that sit below
	// a home directory the sandbox otherwise hides. They are populated only by
	// this package after resolving the executable outside workspace authority;
	// callers cannot widen the sandbox with an environment variable.
	readOnlyRoots []string
}

// Confinement wraps a command so the operating system confines it.
//
// A non-nil *Confinement is itself the evidence that the self-test passed on
// this host. There is deliberately no exported boolean beside it that could
// say "verified" while the wrap is missing, because that combination is how a
// harness ends up running automatic execution unconfined while reporting that
// it is contained (design principle 4).
type Confinement struct {
	mechanism Mechanism
	wrap      func(Policy, []string) ([]string, error)
}

func (c *Confinement) Mechanism() Mechanism {
	if c == nil {
		return MechanismNone
	}
	return c.mechanism
}

func (c *Confinement) apply(p Policy, argv []string) ([]string, error) {
	return c.wrap(p, argv)
}
