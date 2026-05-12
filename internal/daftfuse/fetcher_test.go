package daftfuse_test

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect"

	"github.com/richiejp/darkaifactory/internal/daftfuse"
	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
)

// stubWorkspaceClient implements WorkspaceServiceClient for the
// NewConnectFetcher tests. GetManifest is unused — we only care
// about GetChunk for ChunkFetcher's contract.
type stubWorkspaceClient struct {
	workspacev1connect.UnimplementedWorkspaceServiceHandler

	wantDigest string
	payload    []byte
	err        error
}

func (s *stubWorkspaceClient) GetManifest(_ context.Context, _ *connect.Request[workspacev1.GetManifestRequest]) (*connect.Response[workspacev1.GetManifestResponse], error) {
	return nil, errors.New("not used")
}

func (s *stubWorkspaceClient) GetChunk(_ context.Context, req *connect.Request[workspacev1.GetChunkRequest]) (*connect.Response[workspacev1.GetChunkResponse], error) {
	if s.err != nil {
		return nil, s.err
	}
	if req.Msg.GetDigest() != s.wantDigest {
		return nil, errors.New("unexpected digest")
	}
	return connect.NewResponse(&workspacev1.GetChunkResponse{Compressed: s.payload}), nil
}

func TestNewConnectFetcherForwardsDigest(t *testing.T) {
	stub := &stubWorkspaceClient{wantDigest: "deadbeef", payload: []byte("zstd-bytes")}
	f := daftfuse.NewConnectFetcher(stub)
	got, err := f.FetchChunk(context.Background(), "deadbeef")
	if err != nil {
		t.Fatalf("FetchChunk: %v", err)
	}
	if string(got) != "zstd-bytes" {
		t.Errorf("got = %q", got)
	}
}

func TestNewConnectFetcherPropagatesError(t *testing.T) {
	stub := &stubWorkspaceClient{err: errors.New("rpc broken")}
	f := daftfuse.NewConnectFetcher(stub)
	if _, err := f.FetchChunk(context.Background(), "x"); err == nil {
		t.Error("expected error from fetcher")
	}
}
