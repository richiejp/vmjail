// Package cloudhypervisor implements the bounded subset of the pinned Cloud
// Hypervisor HTTP-over-Unix API needed for workspace-disk lifecycle control.
package cloudhypervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// Client controls one Cloud Hypervisor process through its Unix API socket.
type Client struct {
	socket string
	http   *http.Client
}

// NewClient constructs a client for an absolute Unix-socket path.
func NewClient(socket string) (*Client, error) {
	if !strings.HasPrefix(socket, "/") {
		return nil, fmt.Errorf("cloudhypervisor: API socket %q must be absolute", socket)
	}
	dialer := &net.Dialer{}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{
		socket: socket,
		http:   &http.Client{Transport: transport},
	}, nil
}

// Pause freezes all guest vCPUs after Cloud Hypervisor has drained the request.
func (c *Client) Pause(ctx context.Context) error {
	return c.put(ctx, "vm.pause", nil)
}

// Resume restarts a previously paused VM.
func (c *Client) Resume(ctx context.Context) error {
	return c.put(ctx, "vm.resume", nil)
}

// ResizeDisk exposes desiredSize bytes on the named virtio-blk device. The
// caller must grow the qcow2 virtual size first and run the guest filesystem
// grow operation after this request succeeds.
func (c *Client) ResizeDisk(ctx context.Context, id string, desiredSize uint64) error {
	if id == "" {
		return errors.New("cloudhypervisor: empty disk id")
	}
	if desiredSize == 0 {
		return errors.New("cloudhypervisor: zero desired disk size")
	}
	body, err := json.Marshal(struct {
		ID          string `json:"id"`
		DesiredSize uint64 `json:"desired_size"`
	}{ID: id, DesiredSize: desiredSize})
	if err != nil {
		return fmt.Errorf("cloudhypervisor: encode resize request: %w", err)
	}
	return c.put(ctx, "vm.resize-disk", body)
}

func (c *Client) put(ctx context.Context, command string, body []byte) error {
	if c == nil || c.http == nil {
		return errors.New("cloudhypervisor: nil client")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut,
		"http://localhost/api/v1/"+command, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("cloudhypervisor: build %s request: %w", command, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cloudhypervisor: %s via %s: %w", command, c.socket, err)
	}
	const maxErrorBody = 4 << 10
	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody+1))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return fmt.Errorf("cloudhypervisor: read %s response: %w", command, readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("cloudhypervisor: close %s response: %w", command, closeErr)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if len(responseBody) > maxErrorBody {
			responseBody = responseBody[:maxErrorBody]
		}
		return fmt.Errorf("cloudhypervisor: %s returned %s: %s",
			command, resp.Status, strings.TrimSpace(string(responseBody)))
	}
	return nil
}
