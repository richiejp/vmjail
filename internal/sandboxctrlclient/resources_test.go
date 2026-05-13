package sandboxctrlclient

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeMeminfo(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "meminfo")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write meminfo: %v", err)
	}
	return p
}

func TestReadMemTotalKB(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    uint64
		wantErr string
	}{
		{
			name:    "happy_path",
			content: "MemTotal:       16313800 kB\nMemFree:         1234 kB\n",
			want:    16313800,
		},
		{
			name:    "first_line",
			content: "MemTotal: 4096 kB\n",
			want:    4096,
		},
		{
			name:    "missing_memtotal",
			content: "MemFree: 100 kB\n",
			wantErr: "no MemTotal entry",
		},
		{
			name:    "empty_memtotal_line",
			content: "MemTotal:\n",
			wantErr: "empty MemTotal line",
		},
		{
			name:    "non_numeric_memtotal",
			content: "MemTotal: lots kB\n",
			wantErr: "parse MemTotal",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := writeMeminfo(t, tc.content)
			got, err := readMemTotalKB(p)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("err = %v, want substring %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("readMemTotalKB: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestReadMemTotalKBMissingFile(t *testing.T) {
	_, err := readMemTotalKB("/does/not/exist/meminfo")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestDetectResources(t *testing.T) {
	saved := procMeminfoPath
	t.Cleanup(func() { procMeminfoPath = saved })

	procMeminfoPath = writeMeminfo(t, "MemTotal: 8192 kB\n")
	res, err := DetectResources()
	if err != nil {
		t.Fatalf("DetectResources: %v", err)
	}
	if res.MemoryBytes != 8192*1024 {
		t.Errorf("MemoryBytes = %d, want %d", res.MemoryBytes, 8192*1024)
	}
	if int(res.CPUs) != runtime.NumCPU() {
		t.Errorf("CPUs = %d, want %d", res.CPUs, runtime.NumCPU())
	}
}

func TestDetectResourcesFailsWithoutMeminfo(t *testing.T) {
	saved := procMeminfoPath
	t.Cleanup(func() { procMeminfoPath = saved })

	procMeminfoPath = "/does/not/exist/meminfo"
	if _, err := DetectResources(); err == nil {
		t.Fatal("expected error when meminfo missing")
	}
}
