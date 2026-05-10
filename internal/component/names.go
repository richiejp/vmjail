// Package component holds the canonical identifier strings for
// every daft binary. Tracing, log prefixes, and orchestrator-side
// peer attribution stamp these strings into spans / messages, so a
// single source of truth keeps the wire data uniform across all
// producers and avoids cross-package import cycles when one package
// (e.g. orchestrator) wants to reason about another's identifier
// (e.g. the daft client).
package component

const (
	// Client is the daft user-host CLI.
	Client = "daft"
	// Orchestrator is the central daft-orchestrator service.
	Orchestrator = "daft-orchestrator"
	// Sandboxd is the per-sandbox-host VMM controller.
	Sandboxd = "daft-sandboxd"
	// Agent is the in-VM agent that runs containers under crun.
	Agent = "daft-agent"
)
