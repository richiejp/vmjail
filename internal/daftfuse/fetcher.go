package daftfuse

import (
	"context"

	"connectrpc.com/connect"

	workspacev1 "github.com/richiejp/darkaifactory/internal/genpb/workspace/v1"
	"github.com/richiejp/darkaifactory/internal/genpb/workspace/v1/workspacev1connect"
)

// NewConnectFetcher adapts a Connect-RPC WorkspaceServiceClient to
// the ChunkFetcher interface. Returned bytes are the server's
// zstd-compressed payload — Mount hands them straight to
// cas.Store.PutCompressed, which verifies the digest before
// committing.
func NewConnectFetcher(client workspacev1connect.WorkspaceServiceClient) ChunkFetcher {
	return &connectFetcher{client: client}
}

type connectFetcher struct {
	client workspacev1connect.WorkspaceServiceClient
}

func (f *connectFetcher) FetchChunk(ctx context.Context, digest string) ([]byte, error) {
	resp, err := f.client.GetChunk(ctx, connect.NewRequest(&workspacev1.GetChunkRequest{
		Digest: digest,
	}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetCompressed(), nil
}
