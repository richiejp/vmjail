package cli

import (
	"strings"
	"testing"
)

// TestParsePortMappingArg covers the CLI's first-line parser.
func TestParsePortMappingArg(t *testing.T) {
	for _, tc := range []struct {
		in        string
		wantLocal uint32
		wantCont  uint32
		wantErr   string
	}{
		{"8080:80", 8080, 80, ""},
		{"22:22", 22, 22, ""},
		{"65535:1", 65535, 1, ""},
		{"", 0, 0, "expected <local>:<container>"},
		{"8080", 0, 0, "expected <local>:<container>"},
		{"8080:", 0, 0, "empty container port"},
		{":80", 0, 0, "empty local port"},
		{"0:80", 0, 0, "must be non-zero"},
		{"80:0", 0, 0, "must be non-zero"},
		{"8080:80:90", 0, 0, "too many ':'"},
		{"99999:80", 0, 0, "out of range"},
		{"foo:80", 0, 0, "bad local port"},
	} {
		t.Run(tc.in, func(t *testing.T) {
			local, cont, err := ParsePortMappingArg(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("err = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if local != tc.wantLocal || cont != tc.wantCont {
				t.Errorf("got (%d, %d), want (%d, %d)", local, cont, tc.wantLocal, tc.wantCont)
			}
		})
	}
}
