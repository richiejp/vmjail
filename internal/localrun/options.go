// Package localrun launches host commands in a local microVM.
package localrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/richiejp/vmjail/internal/cli"
	"github.com/spf13/cobra"
)

type Options struct {
	Mode, Root, Cwd, Hostname string
	User, Group               uint32
	Command                   []string

	ReadOnly, Writable, Tmpfs []string
	Env                       []string
	KeepEnv                   bool

	Memory                             string
	CPUs                               uint
	Time, AddressSpace, CPUTime, Files uint64

	Network    bool
	Port       uint16
	ObserveDir string
}

// ExitError preserves the workload's exit status independently of setup errors.
type ExitError struct{ Code int }

func (e *ExitError) Error() string { return fmt.Sprintf("workload exited with status %d", e.Code) }

type RunFunc func(context.Context, Options, io.Reader, io.Writer, io.Writer) error

func NewCommand(in io.Reader, out, errout io.Writer, run RunFunc) *cobra.Command {
	o := Options{}
	cmd := &cobra.Command{
		Use:           "vmjail [options] -- command [args...]",
		Short:         "Run a host command inside a local microVM",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			o.Command = args
			if err := o.validate(); err != nil {
				return err
			}

			return run(cmd.Context(), o, in, out, errout)
		},
	}

	cmd.SetIn(in)
	cmd.SetOut(out)
	cmd.SetErr(errout)

	f := cmd.Flags()
	f.SetInterspersed(false)
	f.StringVarP(&o.Mode, "mode", "M", "o", "o: once; r: rerun; l: TCP listener (one VM per connection)")
	f.StringVarP(&o.Root, "chroot", "c", "", "host root directory; default is an empty root with explicit mounts")
	f.StringVar(&o.Cwd, "cwd", "/", "working directory inside the VM")
	f.StringVar(&o.Hostname, "hostname", "VMJAIL", "guest hostname")
	f.Uint32VarP(&o.User, "user", "u", 0, "guest numeric UID")
	f.Uint32VarP(&o.Group, "group", "g", 0, "guest numeric GID")

	f.StringArrayVarP(&o.ReadOnly, "bindmount_ro", "R", nil, "read-only host SRC[:DST]")
	f.StringArrayVarP(&o.Writable, "bindmount", "B", nil, "writable host SRC[:DST]")
	f.StringArrayVarP(&o.Tmpfs, "tmpfsmount", "T", nil, "guest tmpfs destination")
	f.StringArrayVarP(&o.Env, "env", "E", nil, "guest NAME=VALUE")
	f.BoolVar(&o.KeepEnv, "keep_env", false, "inherit host environment")

	f.Uint64VarP(&o.Time, "time_limit", "t", 600, "wall time per VM in seconds; 0 disables")
	f.Uint64Var(&o.AddressSpace, "rlimit_as", 0, "guest process address space in MiB; 0 inherits")
	f.Uint64Var(&o.CPUTime, "rlimit_cpu", 0, "guest process CPU seconds; 0 inherits")
	f.Uint64Var(&o.Files, "rlimit_nofile", 0, "guest process open file limit; 0 inherits")
	f.UintVar(&o.CPUs, "max_cpus", 1, "number of virtual CPUs")

	f.StringVar(&o.ObserveDir, "observe-dir", "", "retain host resource logs and guest IO/process traces beneath this directory")
	f.StringVar(&o.Memory, "memory", "512M", "guest RAM (VM overhead is separate from process limits)")
	f.BoolVar(&o.Network, "use_pasta", false, "enable userspace networking (vmjail backend, not pasta)")
	f.Uint16Var(&o.Port, "port", 0, "TCP listener port for -Ml (loopback only)")

	cmd.AddCommand(cli.NewVersionCommand("vmjail"))

	return cmd
}

func (o Options) validate() error {
	if o.Mode != "o" && o.Mode != "r" && o.Mode != "l" {
		return errors.New("supported modes: -Mo, -Mr, -Ml; direct host exec is not available across a VM")
	}
	if len(o.Command) == 0 {
		return errors.New("a command after -- is required")
	}
	if !filepath.IsAbs(o.Cwd) || (o.Root != "" && !filepath.IsAbs(o.Root)) {
		return errors.New("chroot and cwd must be absolute")
	}
	if o.CPUs == 0 || o.CPUs > 256 || o.Time > 86400*365 || o.AddressSpace > 1<<40 {
		return errors.New("invalid CPU, time or address-space limit")
	}
	if (o.Mode == "l") != (o.Port != 0) {
		return errors.New("--port is required only with -Ml")
	}
	for _, env := range o.Env {
		if !strings.Contains(env, "=") || strings.HasPrefix(env, "=") {
			return fmt.Errorf("invalid environment entry %q", env)
		}
	}

	return nil
}

func environment(o Options) []string {
	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/", "TERM=" + os.Getenv("TERM")}
	if o.KeepEnv {
		env = os.Environ()
	}

	return append(env, o.Env...)
}
