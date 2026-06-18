package cli

import (
	"github.com/localai-org/darkaifactory/internal/localcluster"
)

// discoverLocalCluster returns the running single-box cluster brought up
// by `daft cluster up`, or (nil, false) when none is up. It reads the
// handle (`cluster.json`) at the default state dir and confirms the
// daemons are still alive, so a stale handle (left by a reboot or crash)
// is ignored rather than steering the client at a dead orchestrator.
//
// This is what lets `daft cluster up` then a bare `daft workspace …`
// connect with no env var and no --orchestrator/--config-dir flag: the
// handle records both the URL and the cross-approved client identity dir.
//
// A var (not a plain func) so tests can stub discovery without standing up
// a real cluster.
//
//nolint:gochecknoglobals // test seam, swapped in unit tests.
var discoverLocalCluster = func() (*localcluster.Cluster, bool) {
	dir, err := localcluster.DefaultStateDir()
	if err != nil {
		return nil, false
	}
	c, err := localcluster.Load(dir)
	if err != nil || c == nil || !c.Alive() {
		return nil, false
	}
	return c, true
}

// resolveIdentityDir picks the client identity dir for talking to the
// orchestrator the command will dial. An explicit --config-dir always
// wins. Otherwise, when that orchestrator IS a running local cluster, the
// matching identity is the cluster's cross-approved client dir — the
// user's own XDG identity is not a peer the cluster orchestrator trusts.
// For any other orchestrator it's the user's XDG default.
//
// orchVal is whatever the caller has for the orchestrator: a raw
// --orchestrator flag, an already-resolved URL, or "". The coupling is by
// *value* — the orchestrator we'll dial compared against the discovered
// cluster's URL — not by which precedence layer won, so it stays correct
// whether the caller passes the flag or the post-resolution URL. That
// keeps a user pointed elsewhere (e.g. $DAFT_ORCHESTRATOR=remote) from
// picking up the local cluster's identity while talking to the remote
// host (the URLs differ → XDG default), while a bare invocation against a
// running `daft cluster up` (URLs match) uses the cluster identity.
func resolveIdentityDir(configDirOverride, orchVal string) (string, error) {
	if configDirOverride != "" {
		return configDirOverride, nil
	}
	url, _, err := resolveOrchestratorSource(orchVal, configDirOverride)
	if err != nil {
		return "", err
	}
	if c, ok := discoverLocalCluster(); ok && c.ClientConfigDir != "" && url == c.URL {
		return c.ClientConfigDir, nil
	}
	return resolveClientDir("")
}
