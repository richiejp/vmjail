package egresscache

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
)

// TestDispatchRoundTrip drives pack-push then pull-unpack through the
// CLI entrypoint against a local registry, asserting the tree survives.
func TestDispatchRoundTrip(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "index.json"), `{}`)
	ref := registryRef(t) + ":cli"
	noEnv := func(string) string { return "" }

	if err := Dispatch([]string{SubPackPush, src, ref}, noEnv, crane.Insecure); err != nil {
		t.Fatalf("pack-push: %v", err)
	}
	dst := t.TempDir()
	if err := Dispatch([]string{SubPullUnpack, ref, dst}, noEnv, crane.Insecure); err != nil {
		t.Fatalf("pull-unpack: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, "index.json")); err != nil {
		t.Errorf("index.json missing after CLI round-trip: %v", err)
	}
}

// TestDispatchUsage covers the arity + unknown-subcommand errors.
func TestDispatchUsage(t *testing.T) {
	noEnv := func(string) string { return "" }
	for _, args := range [][]string{
		{},
		{SubPackPush, "only-one-arg"},
		{"bogus", "a", "b"},
	} {
		if err := Dispatch(args, noEnv); err == nil {
			t.Errorf("Dispatch(%v): want error", args)
		}
	}
}

// TestAuthFromEnv: a token yields Basic auth (with default/explicit
// user); no token yields anonymous (nil).
func TestAuthFromEnv(t *testing.T) {
	if got := authFromEnv(func(string) string { return "" }); got != nil {
		t.Errorf("no token: got %v, want nil (anonymous)", got)
	}

	env := map[string]string{EnvToken: "tok"}
	a := authFromEnv(func(k string) string { return env[k] })
	basic, ok := a.(*authn.Basic)
	if !ok {
		t.Fatalf("token set: got %T, want *authn.Basic", a)
	}
	if basic.Username != "x-access-token" || basic.Password != "tok" {
		t.Errorf("default user: got %q/%q, want x-access-token/tok", basic.Username, basic.Password)
	}

	env[EnvUser] = "richiejp"
	withUser, ok := authFromEnv(func(k string) string { return env[k] }).(*authn.Basic)
	if !ok {
		t.Fatalf("explicit user: got non-Basic auth")
	}
	if withUser.Username != "richiejp" {
		t.Errorf("explicit user: got %q, want richiejp", withUser.Username)
	}
}
