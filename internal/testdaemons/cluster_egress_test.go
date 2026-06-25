//go:build linux && integration

package testdaemons

import "testing"

// TestResolveEgressCache covers the cache-dir/offline decision that
// WithEgressProxy clusters make across its three modes: a recorded
// artifact replays OFFLINE read-only (and wins over a record dir); a
// record dir records online into a persistent path; neither falls back
// to a fresh live-recording temp dir. It's in the integration build
// because the whole package is, but it boots nothing — pure branch
// coverage of the harness wiring.
func TestResolveEgressCache(t *testing.T) {
	noTemp := func() string {
		t.Fatal("mkTemp must not be called when a cache dir is set")
		return ""
	}

	t.Run("artifact replays offline", func(t *testing.T) {
		dir, offline := resolveEgressCache("/repo/bin/egress-cache", "", noTemp)
		if dir != "/repo/bin/egress-cache" || offline != "1" {
			t.Errorf("got (%q,%q), want artifact dir + offline \"1\"", dir, offline)
		}
	})

	t.Run("artifact wins over record dir", func(t *testing.T) {
		dir, offline := resolveEgressCache("/repo/bin/egress-cache", "/record", noTemp)
		if dir != "/repo/bin/egress-cache" || offline != "1" {
			t.Errorf("got (%q,%q), want the artifact dir to win + offline \"1\"", dir, offline)
		}
	})

	t.Run("record dir records online", func(t *testing.T) {
		dir, offline := resolveEgressCache("", "/record", noTemp)
		if dir != "/record" || offline != "0" {
			t.Errorf("got (%q,%q), want record dir + online \"0\"", dir, offline)
		}
	})

	t.Run("neither records live into a temp dir", func(t *testing.T) {
		const temp = "/tmp/test-tempdir"
		dir, offline := resolveEgressCache("", "", func() string { return temp })
		if dir != temp || offline != "0" {
			t.Errorf("got (%q,%q), want temp dir %q + online \"0\"", dir, offline, temp)
		}
	})
}
