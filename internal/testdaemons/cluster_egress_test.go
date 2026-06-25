//go:build linux && integration

package testdaemons

import "testing"

// TestResolveEgressCache covers the cache-dir/offline decision that
// WithEgressProxy clusters make: a recorded artifact replays OFFLINE
// read-only; its absence falls back to a fresh live-recording temp dir.
// It's in the integration build because the whole package is, but it
// boots nothing — pure branch coverage of the harness wiring.
func TestResolveEgressCache(t *testing.T) {
	t.Run("artifact replays offline", func(t *testing.T) {
		dir, offline := resolveEgressCache("/repo/bin/egress-cache", func() string {
			t.Fatal("mkTemp must not be called when an artifact dir is set")
			return ""
		})
		if dir != "/repo/bin/egress-cache" {
			t.Errorf("dir = %q, want the artifact dir", dir)
		}
		if offline != "1" {
			t.Errorf("offline = %q, want \"1\" for a recorded artifact", offline)
		}
	})

	t.Run("no artifact records live into a temp dir", func(t *testing.T) {
		const temp = "/tmp/test-tempdir"
		dir, offline := resolveEgressCache("", func() string { return temp })
		if dir != temp {
			t.Errorf("dir = %q, want the temp dir %q", dir, temp)
		}
		if offline != "0" {
			t.Errorf("offline = %q, want \"0\" so the proxy records live", offline)
		}
	})
}
