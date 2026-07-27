package cloudhypervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

const testAPISocket = "/api.sock"

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type failingBody struct {
	readErr  error
	closeErr error
}

func (b failingBody) Read([]byte) (int, error) { return 0, b.readErr }
func (b failingBody) Close() error             { return b.closeErr }

func TestClientCommands(t *testing.T) {
	type received struct {
		path string
		body map[string]any
	}
	requests := make(chan received, 3)
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			t.Errorf("decode body: %v", err)
		}
		requests <- received{path: r.URL.Path, body: body}
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Status:     "204 No Content",
			Body:       io.NopCloser(bytes.NewReader(nil)),
		}, nil
	})
	client := &Client{socket: testAPISocket, http: &http.Client{Transport: transport}}
	ctx := context.Background()
	if err := client.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.ResizeDisk(ctx, "workspace", 24<<30); err != nil {
		t.Fatal(err)
	}
	if err := client.Resume(ctx); err != nil {
		t.Fatal(err)
	}

	wantPaths := []string{"/api/v1/vm.pause", "/api/v1/vm.resize-disk", "/api/v1/vm.resume"}
	for i, want := range wantPaths {
		got := <-requests
		if got.path != want {
			t.Errorf("request %d path = %q, want %q", i, got.path, want)
		}
		if i == 1 {
			if got.body["id"] != "workspace" || got.body["desired_size"] != float64(24<<30) {
				t.Errorf("resize body = %#v", got.body)
			}
		}
	}
}

func TestClientRejectsInvalidInputs(t *testing.T) {
	if _, err := NewClient("relative.sock"); err == nil {
		t.Fatal("relative socket accepted")
	}
	client, err := NewClient("/missing.sock")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.ResizeDisk(context.Background(), "", 1); err == nil {
		t.Fatal("empty disk id accepted")
	}
	if err := client.ResizeDisk(context.Background(), "workspace", 0); err == nil {
		t.Fatal("zero size accepted")
	}
}

func TestClientBoundsErrorBody(t *testing.T) {
	client := &Client{socket: testAPISocket, http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Status:     "500 Internal Server Error",
			Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", 8<<10))),
		}, nil
	})}}
	err := client.Pause(context.Background())
	if err == nil {
		t.Fatal("server error accepted")
	}
	if len(err.Error()) > 4300 {
		t.Fatalf("error response was not bounded: %d bytes", len(err.Error()))
	}
}

func TestNilClient(t *testing.T) {
	var client *Client
	if err := client.Pause(context.Background()); err == nil {
		t.Fatal("nil client accepted")
	}
}

func TestClientTransportAndResponseErrors(t *testing.T) {
	client, err := NewClient("/definitely/missing/cloud-hypervisor.sock")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Pause(context.Background()); err == nil {
		t.Fatal("dial error ignored")
	}
	if err := client.Pause(nil); err == nil { //nolint:staticcheck // Exercises invalid caller context handling.
		t.Fatal("nil context accepted")
	}

	for _, tc := range []struct {
		name string
		body failingBody
	}{
		{name: "read", body: failingBody{readErr: errors.New("read failed")}},
		{name: "close", body: failingBody{readErr: io.EOF, closeErr: errors.New("close failed")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &Client{socket: testAPISocket, http: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusNoContent, Body: tc.body}, nil
			})}}
			if err := client.Resume(context.Background()); err == nil {
				t.Fatalf("response %s error ignored", tc.name)
			}
		})
	}
}
