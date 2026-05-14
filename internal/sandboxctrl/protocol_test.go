package sandboxctrl

import (
	"strings"
	"testing"
)

func TestCheckProtocolCompatibilityAcceptsMatchingMajor(t *testing.T) {
	cases := []struct {
		name                string
		major, minor, patch uint32
	}{
		{"exact", ProtocolVersionMajor, ProtocolVersionMinor, ProtocolVersionPatch},
		{"newer-minor", ProtocolVersionMajor, ProtocolVersionMinor + 1, 0},
		{"older-minor", ProtocolVersionMajor, 0, 0},
		{"different-patch", ProtocolVersionMajor, ProtocolVersionMinor, ProtocolVersionPatch + 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := CheckProtocolCompatibility(tc.major, tc.minor, tc.patch); err != nil {
				t.Errorf("err = %v, want nil for %d.%d.%d", err, tc.major, tc.minor, tc.patch)
			}
		})
	}
}

func TestCheckProtocolCompatibilityRejectsMismatchedMajor(t *testing.T) {
	err := CheckProtocolCompatibility(ProtocolVersionMajor+1, 0, 0)
	if err == nil {
		t.Fatal("expected error for mismatched major")
	}
	if !strings.Contains(err.Error(), "incompatible protocol major") {
		t.Errorf("err = %v, want 'incompatible protocol major' substring", err)
	}
}

// TestCheckProtocolCompatibilityRejectsZeroValue pins the
// invariant that an all-zero peer triple (the proto3 default when
// the field is unset) is refused — without this, omitting
// ProtocolVersion would silently bypass the handshake.
func TestCheckProtocolCompatibilityRejectsZeroValue(t *testing.T) {
	if err := CheckProtocolCompatibility(0, 0, 0); err == nil {
		t.Fatal("expected error for zero-value peer (major=0)")
	}
}
