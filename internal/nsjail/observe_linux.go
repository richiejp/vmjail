package nsjail

import (
	"os"
	"path/filepath"

	"github.com/richiejp/vmjail/internal/sandbox"
)

func configureObservation(o Options, cfg *sandbox.Config, work string, table []mountEntry) error {
	if o.ObserveDir == "" {
		return nil
	}

	parent, err := filepath.Abs(o.ObserveDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(parent, "run-")
	if err != nil {
		return err
	}

	view := filepath.Join(work, "observe")
	if err := filteredView(dir, view, table, false); err != nil {
		return err
	}

	cfg.Shares = append(cfg.Shares, &sandbox.ShareConfig{HostDir: view, Tag: "observe", Mountpoint: "/observe"})
	cfg.ObserverLogPath = filepath.Join(dir, "resources.jsonl")
	cfg.OpensnoopLogPath = "/observe/opensnoop.jsonl"
	cfg.ExecsnoopLogPath = "/observe/execsnoop.jsonl"
	cfg.SleepsnoopLogPath = "/observe/sleepsnoop.jsonl"
	cfg.IosnoopLogPath = "/observe/iosnoop.jsonl"
	cfg.ObserveSummaryPath = "/observe/summary.json"
	cfg.SyscallListPath = "/observe/syscalls.txt"

	return nil
}
