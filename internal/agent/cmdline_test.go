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
			name:    testAbsent,
			cmdline: "console=hvc0 reboot=t panic=-1",
			wantOK:  false,
		},
		{
			name:    testHappyPath,
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
			errSubstr: errSubMustBeAbsolute,
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

const (
	testCmdlineShareOnly = "console=hvc0 daft.share=ws:/share"
	testHappyPath        = "happy path"
	testAbsent           = "absent"
	// Error-substring fragments shared between cmdline + overlay
	// tests so goconst's 3-occurrence threshold isn't tripped by
	// having the same matcher in two files.
	errSubMustBeAbsolute = "must be absolute"
	errSubEmptyValue     = "value is empty"
	caseNameEmptyValue   = "empty value"
	caseNameRelativePath = "relative path"
	// testSummaryPath is a sample guest summary path shared by the
	// cmdline + summary-writer tests (goconst 3-occurrence threshold).
	testSummaryPath = "/share/progress.json"
)

//nolint:dupl // same table-test harness as TestParseOpensnoopCmdline by design; the per-parser cases differ.
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
			name:    testAbsent,
			cmdline: testCmdlineShareOnly,
			wantOK:  false,
		},
		{
			name:    testHappyPath,
			cmdline: "console=hvc0 daft.bundle=/share/bundle reboot=t",
			want:    testBundlePath,
			wantOK:  true,
		},
		{
			name:      caseNameEmptyValue,
			cmdline:   "daft.bundle= console=hvc0",
			wantErr:   true,
			errSubstr: errSubEmptyValue,
		},
		{
			name:      caseNameRelativePath,
			cmdline:   "daft.bundle=relative/path",
			wantErr:   true,
			errSubstr: errSubMustBeAbsolute,
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

//nolint:dupl // same table-test harness as TestParseBundleCmdline by design; the per-parser cases differ.
func TestParseOpensnoopCmdline(t *testing.T) {
	tests := []struct {
		name      string
		cmdline   string
		want      string
		wantOK    bool
		wantErr   bool
		errSubstr string
	}{
		{name: testAbsent, cmdline: testCmdlineShareOnly, wantOK: false},
		{
			name:    testHappyPath,
			cmdline: "console=hvc0 daft.observe.opensnoop=/share/opensnoop.log reboot=t",
			want:    "/share/opensnoop.log",
			wantOK:  true,
		},
		{name: caseNameEmptyValue, cmdline: "daft.observe.opensnoop= console=hvc0", wantErr: true, errSubstr: errSubEmptyValue},
		{name: caseNameRelativePath, cmdline: "daft.observe.opensnoop=rel/path", wantErr: true, errSubstr: errSubMustBeAbsolute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := ParseOpensnoopCmdline(tt.cmdline)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseOpensnoopCmdline(%q) = nil err, want error containing %q", tt.cmdline, tt.errSubstr)
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

func TestFormatOpensnoopCmdlineRoundTrips(t *testing.T) {
	for _, p := range []string{"/share/opensnoop.log", "/var/log/daft/openat"} {
		s := FormatOpensnoopCmdline(p)
		got, ok, err := ParseOpensnoopCmdline(s)
		if err != nil || !ok || got != p {
			t.Errorf("round-trip of %q via %q: got=%q ok=%v err=%v", p, s, got, ok, err)
		}
	}
}

// TestExecsnoopCmdline covers the round-trip plus the absent and
// relative-path branches of the parser (same contract as open-snoop).
func TestExecsnoopCmdline(t *testing.T) {
	s := FormatExecsnoopCmdline("/share/execsnoop.log")
	if got, ok, err := ParseExecsnoopCmdline(s); err != nil || !ok || got != "/share/execsnoop.log" {
		t.Errorf("round-trip via %q: got=%q ok=%v err=%v", s, got, ok, err)
	}
	if _, ok, err := ParseExecsnoopCmdline(testCmdlineShareOnly); ok || err != nil {
		t.Errorf("absent token: ok=%v err=%v, want false/nil", ok, err)
	}
	if _, _, err := ParseExecsnoopCmdline("daft.observe.execsnoop=rel/path"); err == nil {
		t.Error("relative path should error")
	}
}

//nolint:dupl // same table-test harness as TestParseOpensnoopCmdline by design; the per-parser cases differ.
func TestParseSummaryCmdline(t *testing.T) {
	tests := []struct {
		name      string
		cmdline   string
		want      string
		wantOK    bool
		wantErr   bool
		errSubstr string
	}{
		{name: testAbsent, cmdline: testCmdlineShareOnly, wantOK: false},
		{
			name:    testHappyPath,
			cmdline: "console=hvc0 daft.observe.summary=" + testSummaryPath + " reboot=t",
			want:    testSummaryPath,
			wantOK:  true,
		},
		{name: caseNameEmptyValue, cmdline: "daft.observe.summary= console=hvc0", wantErr: true, errSubstr: errSubEmptyValue},
		{name: caseNameRelativePath, cmdline: "daft.observe.summary=rel/path", wantErr: true, errSubstr: errSubMustBeAbsolute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := ParseSummaryCmdline(tt.cmdline)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseSummaryCmdline(%q) = nil err, want error containing %q", tt.cmdline, tt.errSubstr)
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

func TestFormatSummaryCmdlineRoundTrips(t *testing.T) {
	for _, p := range []string{"/share/progress.json", "/var/log/daft/summary.json"} {
		s := FormatSummaryCmdline(p)
		got, ok, err := ParseSummaryCmdline(s)
		if err != nil || !ok || got != p {
			t.Errorf("round-trip of %q via %q: got=%q ok=%v err=%v", p, s, got, ok, err)
		}
	}
}

func TestParseVsockTracePortCmdline(t *testing.T) {
	tests := []struct {
		name      string
		cmdline   string
		want      uint32
		wantOK    bool
		wantErr   bool
		errSubstr string
	}{
		{name: testAbsent, cmdline: testCmdlineShareOnly, wantOK: false},
		{name: testHappyPath, cmdline: "console=hvc0 daft.vsock.trace_port=8000", want: 8000, wantOK: true},
		{name: caseNameEmptyValue, cmdline: "daft.vsock.trace_port=", wantErr: true, errSubstr: errSubEmptyValue},
		{name: "non-numeric", cmdline: "daft.vsock.trace_port=hello", wantErr: true, errSubstr: "invalid syntax"},
		{name: "zero", cmdline: "daft.vsock.trace_port=0", wantErr: true, errSubstr: "non-zero"},
		{
			name:      "overflow",
			cmdline:   "daft.vsock.trace_port=99999999999",
			wantErr:   true,
			errSubstr: "value out of range",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := ParseVsockTracePortCmdline(tt.cmdline)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error containing %q, got nil", tt.errSubstr)
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
				t.Errorf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("port = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestFormatVsockTracePortCmdlineRoundTrips(t *testing.T) {
	for _, port := range []uint32{1, 1024, 8000, 65535, 1 << 20} {
		s := FormatVsockTracePortCmdline(port)
		got, ok, err := ParseVsockTracePortCmdline(s)
		if err != nil || !ok || got != port {
			t.Errorf("round-trip of %d via %q: got=%d ok=%v err=%v", port, s, got, ok, err)
		}
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

// TestParsePortForwardCmdlinesHappyPath pins the parse + ordering
// contract that the host-side cmdline-builder relies on. Tokens
// appear in workspace-spec order; ParsePortForwardCmdlines must
// return them likewise.
func TestParsePortForwardCmdlinesHappyPath(t *testing.T) {
	cmdline := "console=ttyS0 daft.port=8000:3000 daft.port=8001:80"
	got, err := ParsePortForwardCmdlines(cmdline)
	if err != nil {
		t.Fatalf("ParsePortForwardCmdlines: %v", err)
	}
	want := []PortForwardSpec{
		{VsockPort: 8000, ContainerPort: 3000},
		{VsockPort: 8001, ContainerPort: 80},
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%+v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestParsePortForwardCmdlinesEmpty pins the no-tokens path: the
// host might pass a cmdline with no daft.port= at all (workspace
// with empty ports list).
func TestParsePortForwardCmdlinesEmpty(t *testing.T) {
	got, err := ParsePortForwardCmdlines("console=ttyS0")
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if got != nil {
		t.Errorf("got = %+v, want nil", got)
	}
}

// TestParsePortForwardCmdlinesRejectsMalformed pins the every-
// rejected-form surface. A typo on the host cmdline must surface
// loudly rather than silently dropping the offending token.
func TestParsePortForwardCmdlinesRejectsMalformed(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"missing colon", "daft.port=80003000", "must be <vsockPort>:<containerPort>"},
		{"empty vsock", "daft.port=:3000", "empty vsock or container port"},
		{"empty container", "daft.port=8000:", "empty vsock or container port"},
		{"non-numeric vsock", "daft.port=foo:3000", "vsock port"},
		{"non-numeric container", "daft.port=8000:bar", "container port"},
		{"zero vsock", "daft.port=0:3000", "vsock port must be non-zero"},
		{"zero container", "daft.port=8000:0", "container port must be non-zero"},
		{"out of range vsock", "daft.port=70000:3000", "vsock port 70000 out of range"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParsePortForwardCmdlines(tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// TestFormatPortForwardCmdlineRoundTrips pins that the host's
// Format + agent's Parse are exact inverses.
func TestFormatPortForwardCmdlineRoundTrips(t *testing.T) {
	in := PortForwardSpec{VsockPort: 8000, ContainerPort: 3000}
	got, err := ParsePortForwardCmdlines(FormatPortForwardCmdline(in))
	if err != nil {
		t.Fatalf("ParsePortForwardCmdlines: %v", err)
	}
	if len(got) != 1 || got[0] != in {
		t.Errorf("round-trip = %+v, want [%+v]", got, in)
	}
}

func FuzzParsePortForwardCmdlines(f *testing.F) {
	for _, seed := range []string{
		"daft.port=8000:3000",
		"daft.port=8000:3000 daft.port=8001:80",
		"daft.port=:",
		"daft.port=abc:def",
		"daft.port=0:0",
		"",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(_ *testing.T, raw string) {
		// Parser must never panic on arbitrary input.
		_, _ = ParsePortForwardCmdlines(raw)
	})
}
