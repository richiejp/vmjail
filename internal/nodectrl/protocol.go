package nodectrl

import "fmt"

// ProtocolVersion{Major,Minor,Patch} is the wire-protocol version
// this build speaks on the NodeControlService stream. It is
// distinct from the binary's build version (which identifies the
// release for operator triage) — this gates the bytes on the wire.
//
// Policy:
//   - Major: incremented on incompatible wire changes. Peers with a
//     different major are refused.
//   - Minor: incremented on backwards-compatible additions (new
//     fields, new optional event variants). Mismatches are informational.
//   - Patch: incremented on documentation / non-wire fixes.
//
// We start at 1.0.0 so that a peer omitting ProtocolVersion (or
// pinning ourselves to a future major=0 build) is immediately
// rejected — proto3 zero-values make "field missing" and
// "major=0" indistinguishable, so anchoring at 1 keeps the
// handshake meaningful from day one. Wire breaks bump the major.
const (
	ProtocolVersionMajor uint32 = 1
	ProtocolVersionMinor uint32 = 1
	ProtocolVersionPatch uint32 = 0
)

// CheckProtocolCompatibility returns nil if a peer reporting the
// given (major, minor, patch) is wire-compatible with this build,
// or a descriptive error otherwise. Currently only major must match;
// minor/patch differences are accepted silently. Future minor-gated
// features should consult the peer's reported minor explicitly
// rather than relying on this helper.
func CheckProtocolCompatibility(major, minor, patch uint32) error {
	if major != ProtocolVersionMajor {
		return fmt.Errorf(
			"nodectrl: incompatible protocol major version: peer=%d.%d.%d, local=%d.%d.%d",
			major, minor, patch,
			ProtocolVersionMajor, ProtocolVersionMinor, ProtocolVersionPatch,
		)
	}
	return nil
}
