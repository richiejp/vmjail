package sandboxdcli

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/localai-org/darkaifactory/internal/observability"
)

// EnvStateDir overrides the per-sandbox state root outright. When
// unset, the root derives from $XDG_STATE_HOME (or ~/.local/state) +
// stateSubdir. The test cluster sets it so e2e launches don't scatter
// observer logs under the developer's real home.
const EnvStateDir = "DAFT_STATE_DIR"

// stateSubdir is the path under the XDG state dir that holds
// per-sandbox observability state. Each sandbox gets a <root>/<id>/
// leaf containing its observer log. Sibling-in-spirit to the config
// dir's "daft/sandboxd" and the image cache's "daft-sandboxd-images",
// kept separate because their size profiles and pruning rules differ.
const stateSubdir = "daft/sandboxes"

// sandboxesStateRoot returns the absolute parent directory under which
// each sandbox gets a per-id subdir. DAFT_STATE_DIR overrides it
// outright; otherwise it is $XDG_STATE_HOME/daft/sandboxes, falling
// back to ~/.local/state/daft/sandboxes per the XDG base-dir spec —
// Go's os package has no UserStateDir, and a relative XDG_STATE_HOME
// is ignored exactly as the spec mandates. The directory is not
// created here; callers mkdir the per-id leaf at launch.
func sandboxesStateRoot() (string, error) {
	if override := os.Getenv(EnvStateDir); override != "" {
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf("daft-sandboxd: %s %q must be absolute", EnvStateDir, override)
		}
		return override, nil
	}
	base := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("daft-sandboxd: locate state dir: %w", err)
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, stateSubdir), nil
}

// observerLogForID resolves the per-sandbox state root and returns the
// observer-log path for id under it (creating the dir). The directory
// persists past VM exit so the trace stays grabbable for post-mortem
// inspection.
func observerLogForID(id string) (string, error) {
	root, err := sandboxesStateRoot()
	if err != nil {
		return "", err
	}
	return observability.DefaultPath(root, id)
}

// newSandboxID mints a sortable per-launch identifier for the debug
// hello/run paths, which have no orchestrator-assigned uid:
// <UTC timestamp>-<4 random hex bytes>. Lexical order tracks launch
// order, so an `observer-tail` surface can pick the most recent. The
// random suffix disambiguates two launches within the same second.
func newSandboxID(now time.Time) string {
	var b [4]byte
	// crypto/rand.Read is documented infallible on Linux; on the off
	// chance it errors we keep the timestamp and drop the suffix
	// rather than fail a launch over a cosmetic id.
	_, _ = rand.Read(b[:])
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

// defaultObserverLog is the production resolver wired into the
// hello/run deps: it mints a fresh sandbox id and returns the
// observer-log path under the per-sandbox state dir.
func defaultObserverLog() (string, error) {
	return observerLogForID(newSandboxID(time.Now()))
}
