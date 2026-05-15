// Package sandboxexec is the sandbox-host-side executor: it
// receives Dispatch events from the orchestrator's
// NodeControlService and runs the corresponding workload by
// driving the existing internal/sandbox boot pipeline.
//
// When spec.image_ref is set, the executor pulls the image into
// the local imagestore, bundles it, and boots a VM that runs the
// container (via internal/sandboxboot). When image_ref is empty,
// the executor falls back to a bare Hello VM — useful for the
// smoke-test path the orchestrator uses to verify a sandbox is
// alive.
package sandboxexec

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/richiejp/darkaifactory/internal/assert"
	nodectrlv1 "github.com/richiejp/darkaifactory/internal/genpb/nodectrl/v1"
	sandboxv1 "github.com/richiejp/darkaifactory/internal/genpb/sandbox/v1"
	"github.com/richiejp/darkaifactory/internal/sandbox"
	"github.com/richiejp/darkaifactory/internal/sandboxboot"
	"github.com/richiejp/darkaifactory/internal/trace"
)

// Assembler is the seam through which the Executor turns an OCI
// image reference into the bootable sandbox.Config + a deferred
// unlock for the bundle lock. Production wires
// sandboxboot.BuildBundleConfig; tests pass fakes that don't
// touch the imagestore.
type Assembler func(ctx context.Context, ref string) (sandbox.Config, func(), error)

// Runner is the function the Executor calls to boot a VM. The
// default in production is sandbox.Hello; tests inject a fake so
// they don't need /dev/kvm.
type Runner func(ctx context.Context, w io.Writer, cfg sandbox.Config) error

// Executor implements nodectrlclient.DispatchHandler. One
// Executor per sandboxd process — concurrent dispatches share the
// same baseline config and stdout writer.
type Executor struct {
	base      sandbox.Config
	stdout    io.Writer
	run       Runner
	assembler Assembler
}

// Config bundles the Executor's dependencies. The baseline
// sandbox.Config is the one resolved at startup from DAFT_*
// (used for the bare-Hello fallback when image_ref is empty);
// Assembler is the image-ref → bootable-config seam used when
// image_ref is set. Runner is the function that actually boots
// the VM — production wiring uses sandbox.Hello.
type Config struct {
	Base      sandbox.Config
	Stdout    io.Writer
	Runner    Runner
	Assembler Assembler
}

// New builds an Executor. Stdout is required; Runner defaults to
// sandbox.Hello when nil; Assembler defaults to a wrapper around
// sandboxboot.BuildBundleConfig with the deps the caller passes
// via the production helper (see ProductionAssembler below).
func New(cfg Config) (*Executor, error) {
	if cfg.Stdout == nil {
		return nil, errors.New("sandboxexec: nil stdout writer")
	}
	if cfg.Runner == nil {
		cfg.Runner = sandbox.Hello
	}
	return &Executor{
		base:      cfg.Base,
		stdout:    cfg.Stdout,
		run:       cfg.Runner,
		assembler: cfg.Assembler,
	}, nil
}

// ProductionAssembler returns an Assembler backed by
// sandboxboot.BuildBundleConfig. Threading the deps through here
// keeps the wiring in one place — `daft-sandboxd serve`
// constructs one and hands it to New.
func ProductionAssembler(deps sandboxboot.Deps) Assembler {
	return func(ctx context.Context, ref string) (sandbox.Config, func(), error) {
		return sandboxboot.BuildBundleConfig(ctx, ref, deps)
	}
}

// HandleSandboxEvent is the SandboxEventHandler entrypoint. ADDED
// events trigger a VM boot for the embedded Sandbox.spec; other
// types are ignored for now (MODIFIED is unused in v1 since Sandbox
// specs are immutable; DELETED teardown wiring lands in B5).
//
// When spec.image_ref is non-empty, the executor delegates to its
// Assembler to pull the image and build the bootable sandbox.Config,
// then runs the VM with that config. When image_ref is empty (or no
// Assembler is configured), the executor falls back to the bare
// baseline Hello — the smoke-test path.
//
// Spec fields that don't yet have an executor consumer (mounts[],
// env, ports, gpu_request, isolation) are recorded as span
// attributes but otherwise ignored. Wiring them lands in later
// slices alongside Workspace + ingress.
//
// v1 invokes the runner synchronously, which blocks the
// nodectrlclient receive loop for the duration of the VM boot
// (~hundreds of ms warm, multiple seconds cold). Becomes
// non-blocking once a real concurrency cap and the heartbeat-based
// reconciler (B4d/B5) land.
func (e *Executor) HandleSandboxEvent(
	ctx context.Context,
	evt *nodectrlv1.SandboxWatchEvent,
) error {
	assert.True(evt != nil,
		"SandboxEventHandler must never be called with a nil event — nodectrlclient filters them upstream")
	if evt.GetType() != sandboxv1.EventType_EVENT_TYPE_ADDED {
		// MODIFIED/DELETED handling lands with the reconciler.
		return nil
	}
	sb := evt.GetSandbox()
	assert.True(sb != nil,
		"SandboxWatchEvent ADDED without a Sandbox payload is an orchestrator bug — the wire schema makes the field optional, but every ADDED code path sets it")
	uid := sb.GetMetadata().GetUid()
	spec := sb.GetSpec()
	assert.True(spec != nil,
		"Sandbox without a Spec is an orchestrator bug — Launch always composes one before publishing")
	ctx, span := trace.Start(ctx, "sandboxexec.sandbox_added",
		trace.WithAttributes(
			trace.KV{Key: "sandbox.uid", Value: uid},
			trace.KV{Key: "spec.image_ref", Value: spec.GetImageRef()},
			trace.KV{Key: "spec.workspace_uuid", Value: spec.GetWorkspaceUuid()},
		))
	defer span.End()

	_, _ = fmt.Fprintf(e.stdout,
		"daft-sandboxd: sandbox %s spec.workspace=%s image=%s\n",
		uid, spec.GetWorkspaceUuid(), spec.GetImageRef())

	cfg, unlock, err := e.resolveBootConfig(ctx, spec.GetImageRef())
	if err != nil {
		return trace.RecordError(ctx, fmt.Errorf("sandboxexec: sandbox %s: %w", uid, err))
	}
	defer unlock()

	if err := e.run(ctx, e.stdout, cfg); err != nil {
		return trace.RecordError(ctx, fmt.Errorf("sandboxexec: sandbox %s: %w", uid, err))
	}
	return nil
}

// resolveBootConfig picks between the baseline Hello config and
// the per-dispatch bundled config. unlock is always non-nil so
// callers can defer it unconditionally.
func (e *Executor) resolveBootConfig(ctx context.Context, imageRef string) (sandbox.Config, func(), error) {
	if imageRef == "" || e.assembler == nil {
		return e.base, func() {}, nil
	}
	return e.assembler(ctx, imageRef)
}
