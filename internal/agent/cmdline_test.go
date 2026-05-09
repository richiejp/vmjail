package agent

import (
	"strings"
	"testing"
)

func TestParseShareCmdline(t *testing.T) {
	tests := []struct {
		name      string
		cmdline   string
		want      ShareSpec
		wantOK    bool
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "absent",
			cmdline: "console=hvc0 reboot=t panic=-1",
			wantOK:  false,
		},
		{
			name:    "happy path",
			cmdline: "console=hvc0 daft.share=workspace:/share",
			want:    ShareSpec{Tag: testTag, Mountpoint: testMountpoint},
			wantOK:  true,
		},
		{
			name:    "happy path with leading and trailing tokens",
			cmdline: "ro panic=-1 daft.share=ws:/mnt/x console=tty1",
			want:    ShareSpec{Tag: "ws", Mountpoint: "/mnt/x"},
			wantOK:  true,
		},
		{
			name:      "missing colon separator",
			cmdline:   "daft.share=workspace",
			wantErr:   true,
			errSubstr: "want <tag>:<mountpoint>",
		},
		{
			name:      "empty tag",
			cmdline:   "daft.share=:/share",
			wantErr:   true,
			errSubstr: "tag is empty",
		},
		{
			name:      "relative mountpoint",
			cmdline:   "daft.share=ws:share",
			wantErr:   true,
			errSubstr: "must be absolute",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, ok, err := ParseShareCmdline(tt.cmdline)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseShareCmdline(%q) = nil err, want error containing %q", tt.cmdline, tt.errSubstr)
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Fatalf("err = %v, want substring %q", err, tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseShareCmdline(%q) returned unexpected error: %v", tt.cmdline, err)
			}
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && spec != tt.want {
				t.Fatalf("spec = %+v, want %+v", spec, tt.want)
			}
		})
	}
}

func TestFormatShareCmdlineRoundTrips(t *testing.T) {
	for _, spec := range []ShareSpec{
		{Tag: "ws", Mountpoint: testMountpoint},
		{Tag: testTag, Mountpoint: "/mnt/work"},
	} {
		s := FormatShareCmdline(spec)
		got, ok, err := ParseShareCmdline(s)
		if err != nil || !ok {
			t.Fatalf("round-trip of %+v via %q: ok=%v err=%v", spec, s, ok, err)
		}
		if got != spec {
			t.Errorf("round-trip: got %+v, want %+v", got, spec)
		}
	}
}

func TestParseBundleCmdline(t *testing.T) {
	tests := []struct {
		name      string
		cmdline   string
		want      string
		wantOK    bool
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "absent",
			cmdline: "console=hvc0 daft.share=ws:/share",
			wantOK:  false,
		},
		{
			name:    "happy path",
			cmdline: "console=hvc0 daft.bundle=/share/bundle reboot=t",
			want:    testBundlePath,
			wantOK:  true,
		},
		{
			name:      "empty value",
			cmdline:   "daft.bundle= console=hvc0",
			wantErr:   true,
			errSubstr: "value is empty",
		},
		{
			name:      "relative path",
			cmdline:   "daft.bundle=relative/path",
			wantErr:   true,
			errSubstr: "must be absolute",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := ParseBundleCmdline(tt.cmdline)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseBundleCmdline(%q) = nil err, want error containing %q", tt.cmdline, tt.errSubstr)
				}
				if !strings.Contains(err.Error(), tt.errSubstr) {
					t.Fatalf("err = %v, want substring %q", err, tt.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Fatalf("path = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatBundleCmdlineRoundTrips(t *testing.T) {
	for _, p := range []string{testBundlePath, "/mnt/work/oci"} {
		s := FormatBundleCmdline(p)
		got, ok, err := ParseBundleCmdline(s)
		if err != nil || !ok || got != p {
			t.Errorf("round-trip of %q via %q: got=%q ok=%v err=%v", p, s, got, ok, err)
		}
	}
}

// FuzzParseBundleCmdline mirrors FuzzParseShareCmdline — guards the
// bundle-path parser against panics on arbitrary kernel-cmdline-shaped
// input. Same threat model as the share parser: untrusted bytes from
// the cmdline.
//
// Run actively with:
//
//	go test -run='^$' -fuzz=FuzzParseBundleCmdline -fuzztime=30s ./internal/agent
func FuzzParseBundleCmdline(f *testing.F) {
	seeds := []string{
		"",
		"daft.bundle=/share/bundle",
		"console=hvc0 daft.bundle=/x",
		"daft.bundle=",
		"daft.bundle=/",
		"daft.bundle=relative",
		"daft.bundle=/" + strings.Repeat("a", 4096),
		"daft.bundle=\x00",
		"daft.bundle=日本語",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		_, _, _ = ParseBundleCmdline(raw)
	})
}

// FuzzParseShareCmdline guards the cmdline parser against panics on
// arbitrary kernel-cmdline-shaped input. The kernel cmdline is
// bytes-shaped untrusted input from the host (someone could pass
// pathological values via DAFT_* env or a malicious controller); the
// parser must never panic.
//
// Run actively with:
//
//	go test -run='^$' -fuzz=FuzzParseShareCmdline -fuzztime=30s ./internal/agent
func FuzzParseShareCmdline(f *testing.F) {
	seeds := []string{
		"",
		"daft.share=ws:/share",
		"console=hvc0 daft.share=ws:/share reboot=t",
		"daft.share=",
		"daft.share=:",
		"daft.share=:/",
		"daft.share=ws:",
		"daft.share=ws:relative/path",
		"daft.share=" + strings.Repeat("a", 4096) + ":/x",
		"daft.share=ws:/" + strings.Repeat("a", 4096),
		"daft.share=ws:/share daft.share=other:/mnt", // first token wins
		"daft.share=日本語:/mnt",
		"daft.share=\x00:/mnt",
		"daft.share=ws:\t/mnt",
		// adjacent equals
		"daft.share==:/x",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		_, _, _ = ParseShareCmdline(raw)
	})
}
