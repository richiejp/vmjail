package sandboxdcli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/localai-org/darkaifactory/internal/bundled"
	"github.com/localai-org/darkaifactory/internal/observability"
	"github.com/localai-org/darkaifactory/internal/sandbox"
)

// resolvedLogPath is a stand-in absolute observer-log path used by the
// applyObserverLog success/override cases.
const resolvedLogPath = "/state/sb/observer.jsonl"

func TestSandboxesStateRootOverride(t *testing.T) {
	t.Setenv(EnvStateDir, "/srv/daft-state")
	got, err := sandboxesStateRoot()
	if err != nil {
		t.Fatalf("sandboxesStateRoot: %v", err)
	}
	if got != "/srv/daft-state" {
		t.Errorf("root = %q, want the DAFT_STATE_DIR override verbatim", got)
	}
}

func TestSandboxesStateRootRejectsRelativeOverride(t *testing.T) {
	t.Setenv(EnvStateDir, "relative/state")
	if _, err := sandboxesStateRoot(); err == nil {
		t.Fatal("relative DAFT_STATE_DIR should be rejected")
	}
}

func TestSandboxesStateRootXDG(t *testing.T) {
	t.Setenv(EnvStateDir, "")
	t.Setenv("XDG_STATE_HOME", "/xdg/state")
	got, err := sandboxesStateRoot()
	if err != nil {
		t.Fatalf("sandboxesStateRoot: %v", err)
	}
	if want := "/xdg/state/" + stateSubdir; got != want {
		t.Errorf("root = %q, want %q", got, want)
	}
}

func TestSandboxesStateRootHomeFallback(t *testing.T) {
	t.Setenv(EnvStateDir, "")
	// A relative (spec-invalid) XDG_STATE_HOME must be ignored, exactly
	// like an unset one, falling through to ~/.local/state.
	t.Setenv("XDG_STATE_HOME", "not-absolute")
	t.Setenv("HOME", "/home/tester")
	got, err := sandboxesStateRoot()
	if err != nil {
		t.Fatalf("sandboxesStateRoot: %v", err)
	}
	if want := "/home/tester/.local/state/" + stateSubdir; got != want {
		t.Errorf("root = %q, want %q", got, want)
	}
}

func TestObserverLogForIDCreatesDir(t *testing.T) {
	t.Setenv(EnvStateDir, t.TempDir())
	path, err := observerLogForID("abc123")
	if err != nil {
		t.Fatalf("observerLogForID: %v", err)
	}
	if filepath.Base(path) != observability.DefaultLogFilename {
		t.Errorf("log basename = %q, want %q", filepath.Base(path), observability.DefaultLogFilename)
	}
	if filepath.Base(filepath.Dir(path)) != "abc123" {
		t.Errorf("log parent = %q, want id dir abc123", filepath.Dir(path))
	}
	if fi, err := os.Stat(filepath.Dir(path)); err != nil || !fi.IsDir() {
		t.Errorf("id dir not created: stat err=%v", err)
	}
}

func TestObserverLogForIDMkdirFailure(t *testing.T) {
	// A state root that is a regular file makes the per-id MkdirAll
	// fail, which must surface as an error (callers treat it as
	// best-effort and disable the observer).
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	t.Setenv(EnvStateDir, blocker)
	if _, err := observerLogForID("id"); err == nil {
		t.Fatal("expected mkdir failure when the state root is a file")
	}
}

func TestNewSandboxID(t *testing.T) {
	when := time.Date(2026, 5, 31, 12, 0, 5, 0, time.UTC)
	id := newSandboxID(when)
	if !strings.HasPrefix(id, "20260531T120005Z-") {
		t.Errorf("id = %q, want a sortable UTC-timestamp prefix", id)
	}
	if !regexp.MustCompile(`^20260531T120005Z-[0-9a-f]{8}$`).MatchString(id) {
		t.Errorf("id = %q, want <timestamp>-<8 hex>", id)
	}
	// The random suffix disambiguates same-second launches.
	if id2 := newSandboxID(when); id2 == id {
		t.Errorf("two ids for the same instant collided: %q", id)
	}
}

func TestDefaultObserverLog(t *testing.T) {
	root := t.TempDir()
	t.Setenv(EnvStateDir, root)
	path, err := defaultObserverLog()
	if err != nil {
		t.Fatalf("defaultObserverLog: %v", err)
	}
	if !strings.HasPrefix(path, root+string(os.PathSeparator)) {
		t.Errorf("path %q not under state root %q", path, root)
	}
	if filepath.Base(path) != observability.DefaultLogFilename {
		t.Errorf("basename = %q, want %q", filepath.Base(path), observability.DefaultLogFilename)
	}
}

func TestApplyObserverLog(t *testing.T) {
	t.Run("nil resolver leaves config untouched", func(t *testing.T) {
		var buf strings.Builder
		cfg := sandbox.Config{}
		applyObserverLog(&buf, &cfg, nil)
		if cfg.ObserverLogPath != "" || buf.String() != "" {
			t.Errorf("path=%q out=%q, want both empty", cfg.ObserverLogPath, buf.String())
		}
	})
	t.Run("resolver error disables and announces", func(t *testing.T) {
		var buf strings.Builder
		cfg := sandbox.Config{}
		applyObserverLog(&buf, &cfg, func() (string, error) {
			return "", errors.New("state dir broke")
		})
		if cfg.ObserverLogPath != "" {
			t.Errorf("path = %q, want empty on resolver error", cfg.ObserverLogPath)
		}
		if !strings.Contains(buf.String(), "disabled") {
			t.Errorf("out = %q, want a disabled notice", buf.String())
		}
	})
	t.Run("success sets path and announces it", func(t *testing.T) {
		var buf strings.Builder
		cfg := sandbox.Config{}
		applyObserverLog(&buf, &cfg, func() (string, error) {
			return resolvedLogPath, nil
		})
		if cfg.ObserverLogPath != resolvedLogPath {
			t.Errorf("path = %q, want the resolved path", cfg.ObserverLogPath)
		}
		if !strings.Contains(buf.String(), resolvedLogPath) {
			t.Errorf("out = %q, want it to name the path", buf.String())
		}
	})
	t.Run("explicit path wins over default", func(t *testing.T) {
		var buf strings.Builder
		cfg := sandbox.Config{ObserverLogPath: "/explicit.jsonl"}
		called := false
		applyObserverLog(&buf, &cfg, func() (string, error) {
			called = true
			return resolvedLogPath, nil
		})
		if called {
			t.Error("resolver called despite an explicit ObserverLogPath")
		}
		if cfg.ObserverLogPath != "/explicit.jsonl" {
			t.Errorf("path = %q, want the explicit override untouched", cfg.ObserverLogPath)
		}
	})
}

// TestRunHelloWithDefaultsObserverLog proves the hello path threads the
// observerLog dep onto the booted Config.
func TestRunHelloWithDefaultsObserverLog(t *testing.T) {
	const want = "/state/sb-hello/observer.jsonl"
	var bootCfg sandbox.Config
	err := runHelloWith(t.Context(), io.Discard, helloDeps{
		cacheDir:    func() (string, error) { return "/tmp/cache", nil },
		extract:     func(string) (bundled.Paths, error) { return fakePaths, nil },
		boot:        func(_ context.Context, _ io.Writer, cfg sandbox.Config) error { bootCfg = cfg; return nil },
		observerLog: func() (string, error) { return want, nil },
	})
	if err != nil {
		t.Fatalf("runHelloWith: %v", err)
	}
	if bootCfg.ObserverLogPath != want {
		t.Errorf("ObserverLogPath = %q, want %q", bootCfg.ObserverLogPath, want)
	}
}
