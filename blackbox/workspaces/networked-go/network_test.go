package networked

import (
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestConfiguredEndpoint(t *testing.T) {
	endpoint := os.Getenv("VMJAIL_TEST_ENDPOINT")
	if endpoint == "" {
		t.Fatal("VMJAIL_TEST_ENDPOINT is required")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20)); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %s", response.Status)
	}
}
