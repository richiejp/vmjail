package egresscache

import (
	"fmt"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/crane"
)

// Env var names the CLI reads for push auth. Pull of a public package
// needs none; a token (with its optional username) is only required to
// publish. `make update-egress-cache` sets these from `gh auth token`.
const (
	EnvToken = "DAFT_GHCR_TOKEN" //nolint:gosec // G101: env var name, not a credential.
	EnvUser  = "DAFT_GHCR_USER"
)

// Subcommand names, exported so tests reference them without repeating
// the literals (which goconst would flag).
const (
	SubPackPush   = "pack-push"
	SubPullUnpack = "pull-unpack"
)

// Dispatch runs the egress-cache CLI: `pack-push <srcDir> <ref>` packs a
// recorded cache dir and pushes it; `pull-unpack <ref> <destDir>` pulls
// and extracts it. getenv supplies push auth (EnvToken/EnvUser); opts
// pass through to crane (tests inject crane.Insecure for a local
// registry). It returns a usage error for a bad subcommand or arity.
func Dispatch(args []string, getenv func(string) string, opts ...crane.Option) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: daft-egress-cache pack-push <srcDir> <ref> | pull-unpack <ref> <destDir>")
	}
	auth := authFromEnv(getenv)
	switch args[0] {
	case SubPackPush:
		return Push(args[1], args[2], auth, opts...)
	case SubPullUnpack:
		return Pull(args[1], args[2], auth, opts...)
	default:
		return fmt.Errorf("daft-egress-cache: unknown subcommand %q", args[0])
	}
}

// authFromEnv builds a Basic authenticator from EnvToken/EnvUser, or nil
// (anonymous) when no token is set. ghcr accepts the token as the
// password under any username; default to a conventional placeholder.
func authFromEnv(getenv func(string) string) authn.Authenticator {
	token := getenv(EnvToken)
	if token == "" {
		return nil
	}
	user := getenv(EnvUser)
	if user == "" {
		user = "x-access-token"
	}
	return &authn.Basic{Username: user, Password: token}
}
