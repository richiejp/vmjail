package a

import (
	"context"
	"os/exec"
)

func bad() {
	_ = exec.Command("false") // want "os/exec.Command has no cancellation path"
}

func good(ctx context.Context) {
	_ = exec.CommandContext(ctx, "true")
	//daft:lint:disable=commandcontext reason="fixture models an intentionally supervised daemon"
	_ = exec.Command("daemon")
}
