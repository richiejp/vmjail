//go:build !linux

package agent

// defaultAttachDeps on non-Linux falls back to the
// unavailableAttachExec stub — PTY allocation + crun are Linux-
// kernel features and the agent only ships for Linux (see
// flake.nix). The package compiles cross-platform so editor
// tooling and `go vet ./...` work for developers on macOS.
func defaultAttachDeps() attachDeps {
	return attachDeps{
		listenVsock: vsockListenGuest,
		exec:        unavailableAttachExec{},
	}
}

func defaultJobDeps() jobDeps {
	return jobDeps{listenVsock: vsockListenGuest, exec: unavailableJobExec{}}
}
