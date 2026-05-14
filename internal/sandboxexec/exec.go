// Package sandboxexec is the sandbox-host-side executor: it
// receives Dispatch events from the orchestrator's
// SandboxControlService and runs the corresponding workload by
// driving the existing internal/sandbox boot pipeline.
//
// Slice A4b lands the seam — every Dispatch boots a bare Hello
// VM (the SandboxSpec's image_ref and mounts are recorded but do
// not yet drive the in-VM container). Slice A5 will deprecate
// `daft-sandboxd run` and route its image-pull + bundle-share
// pipeline through this executor.
package sandboxexec

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/richiejp/darkaifactory/internal/assert"
	sandboxctrlv1 "github.com/richiejp/darkaifactory/internal/genpb/sandboxctrl/v1"
	"github.com/richiejp/darkaifactory/internal/sandbox"
	"github.com/richiejp/darkaifactory/internal/trace"
)

// Runner is the function the Executor calls to boot a VM. The
// default in production is sandbox.Hello; tests inject a fake so
// they don't need /dev/kvm.
type Runner func(ctx context.Context, w io.Writer, cfg sandbox.Config) error

// Executor implements sandboxctrlclient.DispatchHandler. One
// Executor per sandboxd process — concurrent dispatches share the
// same baseline config and stdout writer.
type Executor struct {
	base   sandbox.Config
	stdout io.Writer
	run    Runner
}

// New builds an Executor with the baseline sandbox.Config the
// sandbox-host's `serve` command resolved at startup (paths to
// cloud-hypervisor, kernel, initramfs). stdout receives the VM's
// captured banner per dispatch. runner is optional — nil uses the
// production sandbox.Hello.
func New(base sandbox.Config, stdout io.Writer, runner Runner) (*Executor, error) {
	if stdout == nil {
		return nil, errors.New("sandboxexec: nil stdout writer")
	}
	if runner == nil {
		runner = sandbox.Hello
	}
	return &Executor{base: base, stdout: stdout, run: runner}, nil
}

// HandleDispatch is the DispatchHandler entrypoint. The spec
// fields that don't yet have an executor consumer (image_ref,
// mounts, env, ports, gpu_request, isolation) are recorded as
// span attributes but otherwise ignored — Slice A5 will wire the
// image-pull / container pipeline here.
func (e *Executor) HandleDispatch(
	ctx context.Context,
	d *sandboxctrlv1.Dispatch,
) error {
	assert.True(d != nil,
		"DispatchHandler must never be called with a nil Dispatch — sandboxctrlclient filters them upstream")
	spec := d.GetSpec()
	assert.True(spec != nil,
		"Dispatch event without a Spec is an orchestrator bug — the wire schema makes Spec optional, but every code path on the orchestrator side sets it before pushing")
	ctx, span := trace.Start(ctx, "sandboxexec.dispatch",
		trace.WithAttributes(
			trace.KV{Key: "dispatch.id", Value: d.GetDispatchId()},
			trace.KV{Key: "spec.image_ref", Value: spec.GetImageRef()},
			trace.KV{Key: "spec.workspace_uuid", Value: spec.GetWorkspaceUuid()},
		))
	defer span.End()

	_, _ = fmt.Fprintf(e.stdout,
		"daft-sandboxd: dispatch %s spec.workspace=%s image=%s\n",
		d.GetDispatchId(), spec.GetWorkspaceUuid(), spec.GetImageRef())

	if err := e.run(ctx, e.stdout, e.base); err != nil {
		return trace.RecordError(ctx, fmt.Errorf("sandboxexec: dispatch %s: %w", d.GetDispatchId(), err))
	}
	return nil
}
