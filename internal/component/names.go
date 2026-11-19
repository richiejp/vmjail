// Package component holds the canonical identifier strings for
// every vmjail binary. Tracing, log prefixes, and orchestrator-side
// peer attribution stamp these strings into spans / messages, so a
// single source of truth keeps the wire data uniform across all
// producers and avoids cross-package import cycles when one package
// (e.g. orchestrator) wants to reason about another's identifier
// (e.g. the vmjail client).
package component

const (
	// Client is the vmjail user-host CLI.
	Client = "vmjail"
	// Orchestrator is the central vmjail-orchestrator service.
	Orchestrator = "vmjail-orchestrator"
	// Sandboxd is the per-sandbox-host VMM controller.
	Sandboxd = "vmjail-sandboxd"
	// Agent is the in-VM agent that runs containers under crun.
	Agent = "vmjail-agent"
)
