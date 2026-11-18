package sandboxdcli

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/richiejp/darkaifactory/internal/bundled"
	"github.com/richiejp/darkaifactory/internal/imagestore"
	"github.com/richiejp/darkaifactory/internal/sandbox"
)

func TestLaunchOwnsArtifactLifetime(t *testing.T) {
	for _, command := range []string{"hello command", "run"} {
		for _, fail := range []bool{false, true} {
			t.Run(command+"/"+map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
				f, err := os.CreateTemp(t.TempDir(), "asset")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = f.Close() })
				paths := fakePaths
				paths.Files[0] = f
				extract := func(string) (bundled.Paths, error) { return paths, nil }
				want := errors.New("boot failed")
				boot := func(_ context.Context, _ io.Writer, cfg sandbox.Config) error {
					if cfg.ArtifactFiles[0] != f {
						t.Fatal("artifact descriptor not forwarded")
					}
					if _, err := f.Stat(); err != nil {
						t.Fatalf("closed before boot: %v", err)
					}
					if fail {
						return want
					}
					return nil
				}
				if command == "hello command" {
					err = runHelloWith(t.Context(), io.Discard, helloDeps{cacheDir: func() (string, error) { return "", nil }, extract: extract, boot: boot})
				} else {
					deps := runDepsForTest(&fakeStore{pulledImage: &imagestore.Image{Digest: testDigest}, bundle: &imagestore.Bundle{Path: filepath.Join(t.TempDir(), "bundle")}})
					deps.extract, deps.boot = extract, boot
					err = runRun(t.Context(), io.Discard, runOptions{ref: testImageRef}, deps)
				}
				if fail && !errors.Is(err, want) || !fail && err != nil {
					t.Fatalf("launch: %v", err)
				}
				if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("asset leaked after launch: %v", err)
				}
			})
		}
	}
}
