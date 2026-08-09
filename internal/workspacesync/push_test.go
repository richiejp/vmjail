package workspacesync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/richiejp/darkaifactory/internal/cas"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/identity"
	"github.com/richiejp/darkaifactory/internal/workspace"
	"golang.org/x/sys/unix"
)

type fakeCatalog struct {
	response *workspacev1.WorkspaceResponse
	err      error
}

func (f fakeCatalog) Get(context.Context, *connect.Request[workspacev1.GetWorkspaceRequest]) (*connect.Response[workspacev1.WorkspaceResponse], error) {
	return connect.NewResponse(f.response), f.err
}

type fakePushClient struct {
	manifest    *workspacev1.PutManifestRequest
	chunks      int
	manifestErr error
	chunkErr    error
}

func (f *fakePushClient) PutManifest(_ context.Context, request *connect.Request[workspacev1.PutManifestRequest]) (*connect.Response[workspacev1.PutManifestResponse], error) {
	f.manifest = request.Msg
	if f.manifestErr != nil {
		return nil, f.manifestErr
	}
	var missing []string
	for _, entry := range request.Msg.GetManifest().GetEntries() {
		for _, chunk := range entry.GetChunks() {
			missing = append(missing, chunk.GetDigest())
		}
	}
	return connect.NewResponse(&workspacev1.PutManifestResponse{MissingDigests: missing}), nil
}

func (f *fakePushClient) PutChunk(context.Context, *connect.Request[workspacev1.PutChunkRequest]) (*connect.Response[workspacev1.PutChunkResponse], error) {
	f.chunks++
	return connect.NewResponse(&workspacev1.PutChunkResponse{}), f.chunkErr
}

func writeProjectConfig(t *testing.T, root, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".daft"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, projectConfigPath), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadProjectConfinesMounts(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "src"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeProjectConfig(t, root, `{"mounts":[{"local":"src","remote":"/workspace"}]}`)
	resolved, mounts, err := loadProject(root)
	if err != nil || resolved != root || len(mounts) != 1 {
		t.Fatalf("loadProject() = %q, %#v, %v", resolved, mounts, err)
	}
	got, err := confinedMount(resolved, mounts[0].Local)
	if err != nil || got != filepath.Join(root, "src") {
		t.Fatalf("confinedMount() = %q, %v", got, err)
	}
}

func TestConfinedMountRejectsLexicalAndSymlinkEscapes(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, local := range []string{"../outside", outside, "escape"} {
		t.Run(strings.ReplaceAll(local, "/", "_"), func(t *testing.T) {
			if _, err := confinedMount(root, local); err == nil {
				t.Fatalf("confinedMount(%q) succeeded", local)
			}
		})
	}
}

func TestLoadProjectRejectsEscapingConfigAndUnboundedDeclaration(t *testing.T) {
	t.Run("config symlink", func(t *testing.T) {
		root := t.TempDir()
		outside := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(outside, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, ".daft"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, projectConfigPath)); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadProject(root); err == nil {
			t.Fatal("loadProject accepted escaping config symlink")
		}
	})
	t.Run("oversize", func(t *testing.T) {
		root := t.TempDir()
		writeProjectConfig(t, root, strings.Repeat(" ", maxConfigBytes+1))
		if _, _, err := loadProject(root); err == nil {
			t.Fatal("loadProject accepted oversized config")
		}
	})
	t.Run("invalid remote", func(t *testing.T) {
		root := t.TempDir()
		writeProjectConfig(t, root, `{"mounts":[{"local":"src","remote":"relative"}]}`)
		if _, _, err := loadProject(root); err == nil {
			t.Fatal("loadProject accepted relative remote mount")
		}
	})
}

func TestLoadProjectReportsInvalidRootsAndDeclarations(t *testing.T) {
	if _, _, err := loadProject(""); err == nil {
		t.Fatal("loadProject accepted empty root")
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadProject(file); err == nil {
		t.Fatal("loadProject accepted file root")
	}
	if _, _, err := loadProject(t.TempDir()); err == nil {
		t.Fatal("loadProject accepted missing config")
	}
	root := t.TempDir()
	writeProjectConfig(t, root, `{`)
	if _, _, err := loadProject(root); err == nil {
		t.Fatal("loadProject accepted malformed JSON")
	}
	writeProjectConfig(t, root, `{"mounts":[{"local":"src","remote":"/`+strings.Repeat("x", maxPathBytes)+`"}]}`)
	if _, _, err := loadProject(root); err == nil {
		t.Fatal("loadProject accepted oversized mount path")
	}
	var declarations strings.Builder
	declarations.WriteString(`{"mounts":[`)
	for i := range maxMounts + 1 {
		if i > 0 {
			declarations.WriteByte(',')
		}
		declarations.WriteString(`{"local":"src","remote":"/workspace"}`)
	}
	declarations.WriteString(`]}`)
	writeProjectConfig(t, root, declarations.String())
	if _, _, err := loadProject(root); err == nil {
		t.Fatal("loadProject accepted too many mounts")
	}
}

func TestPushUploadsConfinedProjectAndClosesRelay(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "src")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "hello.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeProjectConfig(t, root, `{"mounts":[{"local":"src","remote":"/workspace"}]}`)

	cfg, target := validPushConfig(t, root)
	client := &fakePushClient{}
	closed := false
	cfg.DialNode = func(_ context.Context, _ PushConfig, key string, got identity.Fingerprint) (workspace.PushClient, func() error, error) {
		if key != "workspace-id" || got != target {
			t.Fatalf("dial target = %q, %s", key, got)
		}
		return client, func() error { closed = true; return nil }, nil
	}
	result, err := Push(t.Context(), cfg)
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !closed || len(result.Mounts) != 1 || result.Mounts[0].Entries != 1 ||
		client.manifest.GetWorkspace() != workspace.SyncKey("workspace-id", "/workspace") || client.chunks == 0 {
		t.Fatalf("result=%#v closed=%v manifest=%#v chunks=%d", result, closed, client.manifest, client.chunks)
	}
}

func TestPushRejectsInvalidConfigurationAndTargetFailures(t *testing.T) {
	root := t.TempDir()
	writeProjectConfig(t, root, `{"mounts":[{"local":"src","remote":"/workspace"}]}`)
	base, _ := validPushConfig(t, root)
	for name, mutate := range map[string]func(*PushConfig){
		"uuid":         func(c *PushConfig) { c.WorkspaceUUID = "" },
		"orchestrator": func(c *PushConfig) { c.Orchestrator = "" },
		"identity":     func(c *PushConfig) { c.Identity = nil },
		"peers":        func(c *PushConfig) { c.Peers = nil },
		"store":        func(c *PushConfig) { c.Store = nil },
		"catalog":      func(c *PushConfig) { c.Catalog = nil },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base
			mutate(&cfg)
			if _, err := Push(t.Context(), cfg); err == nil {
				t.Fatal("Push accepted invalid configuration")
			}
		})
	}

	for name, response := range map[string]*workspacev1.WorkspaceResponse{
		"missing target": workspaceResponse("workspace-id", ""),
		"bad target":     workspaceResponse("workspace-id", "bad"),
	} {
		t.Run(name, func(t *testing.T) {
			cfg := base
			cfg.Catalog = fakeCatalog{response: response}
			if _, err := Push(t.Context(), cfg); err == nil {
				t.Fatal("Push accepted invalid target")
			}
		})
	}
	cfg := base
	cfg.Catalog = fakeCatalog{response: workspaceResponse("workspace-id", ""), err: errors.New("get failed")}
	if _, err := Push(t.Context(), cfg); err == nil {
		t.Fatal("Push ignored catalog error")
	}
}

func TestPushOmitsGitAndDaftControlTrees(t *testing.T) {
	root := t.TempDir()
	writeProjectConfig(t, root, `{"mounts":[{"local":".","remote":"/workspace"}]}`)
	for _, path := range []string{"kept.txt", ".git/objects/secret", ".daft/private", "nested/.git/config"} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(path), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cfg, _ := validPushConfig(t, root)
	client := &fakePushClient{}
	cfg.DialNode = func(context.Context, PushConfig, string, identity.Fingerprint) (workspace.PushClient, func() error, error) {
		return client, func() error { return nil }, nil
	}
	if _, err := Push(t.Context(), cfg); err != nil {
		t.Fatalf("Push: %v", err)
	}
	entries := client.manifest.GetManifest().GetEntries()
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.GetPath())
	}
	if !slices.Contains(paths, "kept.txt") || slices.ContainsFunc(paths, skipControlPath) {
		t.Fatalf("manifest paths = %v, want kept.txt and no control trees", paths)
	}
}

func TestPushReportsDialAndUploadErrors(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "src")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "file"), []byte("body"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeProjectConfig(t, root, `{"mounts":[{"local":"src","remote":"/workspace"}]}`)
	base, _ := validPushConfig(t, root)
	want := errors.New("failed")
	base.DialNode = func(context.Context, PushConfig, string, identity.Fingerprint) (workspace.PushClient, func() error, error) {
		return nil, nil, want
	}
	if _, err := Push(t.Context(), base); !errors.Is(err, want) {
		t.Fatalf("dial error = %v", err)
	}
	base.DialNode = func(context.Context, PushConfig, string, identity.Fingerprint) (workspace.PushClient, func() error, error) {
		return &fakePushClient{manifestErr: want}, func() error { return nil }, nil
	}
	if _, err := Push(t.Context(), base); !errors.Is(err, want) {
		t.Fatalf("upload error = %v", err)
	}
	base.DialNode = func(context.Context, PushConfig, string, identity.Fingerprint) (workspace.PushClient, func() error, error) {
		return &fakePushClient{chunkErr: want}, func() error { return nil }, nil
	}
	if _, err := Push(t.Context(), base); !errors.Is(err, want) {
		t.Fatalf("chunk upload error = %v", err)
	}
	base.DialNode = nil
	base.Orchestrator = "://bad"
	if _, err := Push(t.Context(), base); err == nil {
		t.Fatal("Push ignored default broker construction error")
	}
}

func TestPushHandlesEmptyProjectsAndInvalidMountContents(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		root := t.TempDir()
		writeProjectConfig(t, root, `{}`)
		cfg, _ := validPushConfig(t, root)
		result, err := Push(t.Context(), cfg)
		if err != nil || result.ProjectDir != root || len(result.Mounts) != 0 {
			t.Fatalf("Push empty = %#v, %v", result, err)
		}
	})
	t.Run("absolute inside root", func(t *testing.T) {
		root := t.TempDir()
		source := filepath.Join(root, "src")
		if err := os.Mkdir(source, 0o700); err != nil {
			t.Fatal(err)
		}
		if got, err := confinedMount(root, source); err != nil || got != source {
			t.Fatalf("confinedMount = %q, %v", got, err)
		}
	})
	t.Run("file mount", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "file"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := confinedMount(root, "file"); err == nil {
			t.Fatal("confinedMount accepted regular file")
		}
	})
	t.Run("empty mount", func(t *testing.T) {
		if _, err := confinedMount(t.TempDir(), ""); err == nil {
			t.Fatal("confinedMount accepted empty path")
		}
	})
	t.Run("push rejects escaping mount", func(t *testing.T) {
		root := t.TempDir()
		writeProjectConfig(t, root, `{"mounts":[{"local":"../outside","remote":"/workspace"}]}`)
		cfg, _ := validPushConfig(t, root)
		cfg.DialNode = func(context.Context, PushConfig, string, identity.Fingerprint) (workspace.PushClient, func() error, error) {
			return &fakePushClient{}, func() error { return nil }, nil
		}
		if _, err := Push(t.Context(), cfg); err == nil {
			t.Fatal("Push accepted escaping mount")
		}
	})
	t.Run("push reports project error", func(t *testing.T) {
		cfg, _ := validPushConfig(t, t.TempDir())
		if _, err := Push(t.Context(), cfg); err == nil {
			t.Fatal("Push ignored missing project config")
		}
	})
	t.Run("unsupported entry", func(t *testing.T) {
		root := t.TempDir()
		source := filepath.Join(root, "src")
		if err := os.Mkdir(source, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mkfifo(filepath.Join(source, "fifo"), 0o600); err != nil {
			t.Fatal(err)
		}
		writeProjectConfig(t, root, `{"mounts":[{"local":"src","remote":"/workspace"}]}`)
		cfg, _ := validPushConfig(t, root)
		cfg.DialNode = func(context.Context, PushConfig, string, identity.Fingerprint) (workspace.PushClient, func() error, error) {
			return &fakePushClient{}, func() error { return nil }, nil
		}
		if _, err := Push(t.Context(), cfg); err == nil {
			t.Fatal("Push accepted unsupported FIFO")
		}
	})
}

func TestDefaultDialRejectsInvalidBrokerURL(t *testing.T) {
	root := t.TempDir()
	writeProjectConfig(t, root, `{}`)
	cfg, target := validPushConfig(t, root)
	cfg.Orchestrator = "://bad"
	if _, _, err := dialNode(t.Context(), cfg, "workspace", target); err == nil {
		t.Fatal("dialNode accepted invalid broker URL")
	}
	cfg.Orchestrator = "https://127.0.0.1:1"
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := dialNode(ctx, cfg, "workspace", target); err == nil {
		t.Fatal("dialNode unexpectedly reached an unavailable broker")
	}
}

func validPushConfig(t *testing.T, root string) (PushConfig, identity.Fingerprint) {
	t.Helper()
	client, err := identity.Generate(identity.RoleClient)
	if err != nil {
		t.Fatal(err)
	}
	node, err := identity.Generate(identity.RoleSandboxHost)
	if err != nil {
		t.Fatal(err)
	}
	peers, err := identity.OpenPeerStore(filepath.Join(t.TempDir(), "peers.json"))
	if err != nil {
		t.Fatal(err)
	}
	store, err := cas.Open(filepath.Join(t.TempDir(), "cas"))
	if err != nil {
		t.Fatal(err)
	}
	return PushConfig{
		Orchestrator: "https://orchestrator", ProjectDir: root, WorkspaceUUID: "workspace-id",
		Identity: client, Peers: peers, Store: store,
		Catalog: fakeCatalog{response: workspaceResponse("workspace-id", node.Fingerprint().String())},
	}, node.Fingerprint()
}

func workspaceResponse(uuid, target string) *workspacev1.WorkspaceResponse {
	return &workspacev1.WorkspaceResponse{Workspace: &workspacev1.Workspace{
		Metadata: &workspacev1.WorkspaceMetadata{Uuid: uuid},
		Spec:     &workspacev1.WorkspaceSpec{TargetSandboxFingerprint: target},
	}}
}
