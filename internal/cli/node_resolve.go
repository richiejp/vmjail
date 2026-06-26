package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1/nodectrlv1connect"
	"github.com/richiejp/darkaifactory/internal/identity"
)

// nodeResolveTimeout bounds the ListNodes lookup used to resolve a
// hostname / fingerprint-prefix selector to a full fingerprint.
const nodeResolveTimeout = 5 * time.Second

// newNodeControlClient dials the orchestrator's NodeControlService over
// HTTP/2 + mTLS with the local daft client identity — the same wiring
// `daft node list` uses, factored so selector resolution can reuse it.
func newNodeControlClient(orchestrator, configDirOverride string) (nodectrlv1connect.NodeControlServiceClient, error) {
	_, id, peers, err := openClientIdentity(configDirOverride, orchestrator)
	if err != nil {
		return nil, err
	}
	tlsCfg, err := identity.ClientTLSConfig(id, peers, "")
	if err != nil {
		return nil, err
	}
	httpc := &http.Client{Transport: &http2.Transport{TLSClientConfig: tlsCfg}}
	return nodectrlv1connect.NewNodeControlServiceClient(httpc, orchestrator), nil
}

// resolveTargetSelector turns the `daft workspace target` argument into
// a full canonical fingerprint to store. An empty selector clears the
// target. A full fingerprint (any accepted spelling) is canonicalised
// WITHOUT a lookup — so a target can be pinned to a node that hasn't
// connected yet. Anything else is resolved against the live registry by
// hostname or fingerprint prefix (one orchestrator dial, bounded).
func resolveTargetSelector(ctx context.Context, opts workspaceClientOpts, selector string) (string, error) {
	if selector == "" {
		return "", nil
	}
	if fp, err := identity.ParseFingerprint(selector); err == nil {
		return fp.String(), nil
	}
	orchestrator, err := resolveOrchestrator(opts.orchestrator, opts.configDir)
	if err != nil {
		return "", err
	}
	client, err := newNodeControlClient(orchestrator, opts.configDir)
	if err != nil {
		return "", err
	}
	rctx, cancel := context.WithTimeout(ctx, nodeResolveTimeout)
	defer cancel()
	return resolveNodeSelector(rctx, client, selector)
}

// resolveNodeSelector matches selector against the live node set: first
// as an exact hostname (the friendly primary key), then as a
// fingerprint-hex prefix. Exactly one match resolves to that node's full
// fingerprint; zero is an error pointing at `daft node list`; many is an
// error listing the candidates so the user can disambiguate with a
// longer prefix or the full fingerprint. A full fingerprint or empty
// selector is handled by resolveTargetSelector before reaching here, but
// is also short-circuited here so direct callers behave the same.
func resolveNodeSelector(
	ctx context.Context,
	client nodectrlv1connect.NodeControlServiceClient,
	selector string,
) (string, error) {
	if selector == "" {
		return "", nil
	}
	if fp, err := identity.ParseFingerprint(selector); err == nil {
		return fp.String(), nil
	}
	resp, err := client.ListNodes(ctx, connect.NewRequest(&nodectrlv1.ListNodesRequest{}))
	if err != nil {
		return "", fmt.Errorf("resolve node %q: list nodes: %w", selector, err)
	}
	nodes := resp.Msg.GetNodes()

	switch byHost := matchHostname(nodes, selector); len(byHost) {
	case 1:
		return byHost[0].GetFingerprint(), nil
	case 0:
		// Not a hostname — fall through to fingerprint-prefix matching.
	default:
		return "", ambiguousSelectorErr(selector, "hostname", byHost)
	}

	switch byPrefix := matchFingerprintPrefix(nodes, selector); len(byPrefix) {
	case 1:
		return byPrefix[0].GetFingerprint(), nil
	case 0:
		return "", fmt.Errorf(
			"no registered node matches %q by hostname or fingerprint prefix (see `daft node list`)",
			selector,
		)
	default:
		return "", ambiguousSelectorErr(selector, "fingerprint prefix", byPrefix)
	}
}

// matchHostname returns the nodes whose (non-empty) hostname equals
// selector exactly.
func matchHostname(nodes []*nodectrlv1.Node, selector string) []*nodectrlv1.Node {
	var out []*nodectrlv1.Node
	for _, n := range nodes {
		if h := n.GetHostname(); h != "" && h == selector {
			out = append(out, n)
		}
	}
	return out
}

// matchFingerprintPrefix returns the nodes whose canonical fingerprint
// hex starts with selector (in any accepted spelling).
func matchFingerprintPrefix(nodes []*nodectrlv1.Node, selector string) []*nodectrlv1.Node {
	var out []*nodectrlv1.Node
	for _, n := range nodes {
		fp, err := identity.ParseFingerprint(n.GetFingerprint())
		if err != nil {
			continue
		}
		if fp.HasHexPrefix(selector) {
			out = append(out, n)
		}
	}
	return out
}

// ambiguousSelectorErr formats an error listing every node a selector
// matched, so the user can pick a longer prefix or the full fingerprint.
func ambiguousSelectorErr(selector, kind string, matches []*nodectrlv1.Node) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%q is an ambiguous %s — matches %d nodes:", selector, kind, len(matches))
	for _, n := range matches {
		fmt.Fprintf(&b, "\n  %s  %s", orDash(n.GetHostname()), n.GetFingerprint())
	}
	b.WriteString("\nuse a longer fingerprint prefix or the full fingerprint")
	return errors.New(b.String())
}
