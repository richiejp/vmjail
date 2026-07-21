package agent

import (
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
	"time"
)

// PostStartReportPrefix tags post-start lifecycle output on the shared
// console, so a user (and the observer trace) can tell the resolved
// devcontainer postStartCommand's output from the container's own.
const PostStartReportPrefix = "post-start: "

// postStartReadyTimeout bounds how long the runner waits for the
// container to reach crun's "running" state before giving up. Generous:
// a heavy entrypoint (e.g. a dev image's init) can take a few seconds to
// settle, and a never-running container means the commands can't run
// anyway — so this only caps a wedged boot, it isn't a tight SLA. A var
// (not const) so tests can shrink it.
//
//nolint:gochecknoglobals // test-tunable readiness bound; never mutated in production.
var postStartReadyTimeout = 30 * time.Second

// postStartPollInterval is how often the runner re-checks container state
// while waiting for "running". A var so tests can shrink it.
//
//nolint:gochecknoglobals // test-tunable poll cadence; never mutated in production.
var postStartPollInterval = 200 * time.Millisecond

// containerRunning reports whether the daft container is in crun's
// "running" state. Production shells `crun state <id>` and reads the
// status field; a package var so tests can stub readiness without crun.
//
//nolint:gochecknoglobals // test seam for the crun state probe; never mutated in production.
var containerRunning = func(ctx context.Context) bool {
	// #nosec G204 — ContainerRuntime is a package var only host code can
	// swap; ContainerID is a constant. Same trust model as RunContainer.
	out, err := exec.CommandContext(ctx, ContainerRuntime, "state", ContainerID).Output()
	if err != nil {
		return false
	}
	var st struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(out, &st) != nil {
		return false
	}
	return st.Status == "running"
}

// startPostStart runs the resolved devcontainer postStartCommand(s) inside
// the container once it is up, then returns — it spawns its work on a
// detached goroutine so it never blocks the boot before runContainer (the
// same spawn-and-return contract as startAttachListener). It is a no-op
// when there is no bundle (nothing to exec into) or no post-start command.
//
// postStart runs every container start (devcontainer semantics). A failing
// command is logged, not fatal: the container keeps running, matching how a
// failed share or container is non-fatal to the agent.
func startPostStart(ctx context.Context, w io.Writer, d runDeps) {
	cmds := d.cfg.GetPostStartCommands()
	bundle := d.cfg.GetBundle()
	if len(cmds) == 0 || bundle == "" {
		return
	}
	// Exec'd processes inherit the container's env + user, exactly as the
	// attach path does, so postStart sees the same environment (incl. the
	// egress-proxy vars) as the entrypoint.
	proc := readContainerProcess(w, bundle)
	go runPostStart(ctx, w, d, proc)
}

// runPostStart waits for the container to be running, then crun-execs each
// post-start command in order, streaming its output to w. It reuses the
// attach exec primitive (d.attach.exec) — the same crun-exec path
// `daft workspace exec` uses — so there's one way to run a command in the
// container.
func runPostStart(ctx context.Context, w io.Writer, d runDeps, cp containerProcess) {
	if !awaitContainerRunning(ctx, w, PostStartReportPrefix,
		"container did not reach running state; skipping postStartCommand") {
		return
	}
	for _, cmd := range d.cfg.GetPostStartCommands() {
		argv := cmd.GetArgv()
		if len(argv) == 0 {
			continue
		}
		runOnePostStart(ctx, w, d, cp, argv)
	}
}

// awaitContainerRunning polls until the container reaches "running" or the
// deadline passes. Returns false (and reports) on timeout/cancel so the
// caller skips the commands rather than exec into a container that never
// came up.
func awaitContainerRunning(ctx context.Context, w io.Writer, reportPrefix, timeoutMessage string) bool {
	deadline := time.Now().Add(postStartReadyTimeout)
	tick := time.NewTicker(postStartPollInterval)
	defer tick.Stop()
	//daft:lint:disable=forloopbound reason="bounded by the postStartReadyTimeout deadline + ctx.Done() below"
	for {
		if containerRunning(ctx) {
			return true
		}
		if time.Now().After(deadline) {
			writeReport(w, reportPrefix, timeoutMessage)
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
		}
	}
}

// runOnePostStart execs one command and streams its combined output. A
// start error or non-zero exit is reported but never propagated — the
// container stays up regardless (devcontainer postStart-failure semantics).
func runOnePostStart(ctx context.Context, w io.Writer, d runDeps, cp containerProcess, argv []string) {
	writeReport(w, PostStartReportPrefix, "running: "+strings.Join(argv, " "))
	proc, err := d.attach.exec.Start(ctx, AttachExecRequest{
		Command: argv,
		Env:     cp.Env,
		User:    cp.User,
	})
	if err != nil {
		writeReport(w, PostStartReportPrefix, "start: "+err.Error())
		return
	}
	// Stream output until the process closes its (pty) stream, then reap.
	prefixCopy(w, proc, PostStartReportPrefix)
	code, werr := proc.Wait()
	_ = proc.Close()
	switch {
	case werr != nil:
		writeReport(w, PostStartReportPrefix, "wait: "+werr.Error())
	case code != 0:
		writeReport(w, PostStartReportPrefix,
			"command exited non-zero (continuing): "+strings.Join(argv, " "))
	}
}
