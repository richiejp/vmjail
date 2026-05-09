package sandbox

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestHelloIsNotYetWired(t *testing.T) {
	var buf bytes.Buffer
	err := Hello(context.Background(), &buf)
	if !errors.Is(err, ErrNotWired) {
		t.Fatalf("Hello returned %v, want ErrNotWired", err)
	}
	// Stub does not write to the output buffer. Asserting this pins the
	// contract: the integration test in slice 7 expects to see only the
	// agent's banner on the captured stream, not stub chatter.
	if buf.Len() != 0 {
		t.Fatalf("stub Hello wrote %d bytes to output, want 0", buf.Len())
	}
}
