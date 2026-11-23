//go:build linux

package nsjail

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/richiejp/vmjail/internal/cli"

	"github.com/richiejp/vmjail/internal/agent"
	"github.com/richiejp/vmjail/internal/bundled"
	"github.com/richiejp/vmjail/internal/ocibundle"
	"github.com/richiejp/vmjail/internal/sandbox"
	"golang.org/x/sys/unix"
)

//nolint:gochecknoglobals // Process and artifact boundaries allow lifecycle fault injection.
var (
	runCommand    = runOnce
	serveVM       = sandbox.Serve
	loadArtifacts = artifacts
	hasBundle     = bundled.IsBundled
	extractBundle = bundled.Extract
	listenTCP     = net.Listen
)

func Run(ctx context.Context, o Options, in io.Reader, out, errout io.Writer) error {
	if o.Mode == "l" {
		return listen(ctx, o, errout)
	}

	if o.Mode == "r" {
		if _, ok := in.(*os.File); !ok {
			return errors.New("rerun mode requires file-backed stdin")
		}
	}

	//vmjail:lint:disable=forloopbound reason="once exits after one VM; rerun stops on signal or infrastructure failure"
	for {
		err := runCommand(ctx, o, in, out, errout)
		if o.Mode != "r" || ctx.Err() != nil {
			return err
		}

		var exit *cli.ExitError
		if err != nil && !errors.As(err, &exit) {
			return err
		}
	}
}

func listen(ctx context.Context, o Options, errout io.Writer) error {
	ln, err := listenTCP("tcp", fmt.Sprintf("127.0.0.1:%d", o.Port))
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()

	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()

	// Sequential connections bound VM concurrency and memory usage for this experiment.
	//vmjail:lint:disable=forloopbound reason="listener closes on context cancellation"
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}

		runErr := runCommand(ctx, o, conn, conn, conn)
		_ = conn.Close()

		if runErr != nil {
			if _, err := fmt.Fprintln(errout, runErr); err != nil {
				return err
			}
		}
	}
}

func artifacts() (sandbox.Config, func(), error) {
	embedded := hasBundle()
	cfg, err := sandbox.ConfigFromEnv(!embedded)
	if err != nil {
		return cfg, nil, err
	}
	if !embedded {
		return cfg, func() {}, nil
	}

	paths, err := extractBundle("")
	if err != nil {
		return cfg, nil, err
	}

	cfg.ApplyArtifactDefaults(paths)
	return cfg, paths.Close, nil
}

func prepare(o Options, cfg *sandbox.Config) (func(), error) {
	table, err := readMounts()
	if err != nil {
		return nil, err
	}

	work, err := os.MkdirTemp("", "vmjail-local-")
	if err != nil {
		return nil, err
	}

	cleanup := func() {
		if err := unix.Unmount(work, unix.MNT_DETACH); err != nil {
			return
		}
		_ = os.RemoveAll(work)
	}

	if err = mountFS(fsTmpfs, work, fsTmpfs, unix.MS_NODEV|unix.MS_NOSUID, "mode=0700,size=16m"); err != nil {
		_ = os.Remove(work) // No mount exists when mount failed.
		return nil, err
	}

	fail := func(err error) (func(), error) { cleanup(); return nil, err }

	root := o.Root
	if root == "" {
		root = filepath.Join(work, "empty")
		// #nosec G301 -- the empty guest root must be searchable by the selected guest UID.
		if err = os.Mkdir(root, 0o755); err != nil {
			return fail(err)
		}
	} else {
		root, err = filepath.EvalSymlinks(root)
		if err != nil {
			return fail(err)
		}
	}

	view := filepath.Join(work, "root")
	if err = filteredView(root, view, table, true); err != nil {
		return fail(err)
	}

	if o.Root != "" && beneath(work, root) {
		rel, err := filepath.Rel(root, work)
		if err != nil {
			return fail(err)
		}
		if _, err := maskDirectory(filepath.Join(view, rel), view); err != nil {
			return fail(err)
		}
	}

	cfg.Shares = []*sandbox.ShareConfig{{
		HostDir:    view,
		Tag:        "hostroot",
		Mountpoint: hostRootMount,
		ReadOnly:   true,
		Overlay:    true,
	}}
	if err := configureShares(o, cfg, work, table); err != nil {
		return fail(err)
	}

	if err := configureObservation(o, cfg, work, table); err != nil {
		return fail(err)
	}

	return cleanup, nil
}

func configureShares(o Options, cfg *sandbox.Config, work string, table []mountEntry) error {
	mounts := guestMounts()
	for i, value := range slices.Concat(o.ReadOnly, o.Writable) {
		b, err := parseBind(value, i < len(o.ReadOnly))
		if err != nil {
			return err
		}

		tag := fmt.Sprintf("mount%d", i)
		share, mount, err := stageBind(b, work, tag, table, o.User, o.Group)
		if err != nil {
			return err
		}

		cfg.Shares = append(cfg.Shares, share)
		mounts = append(mounts, mount)
	}

	for _, dst := range o.Tmpfs {
		if !filepath.IsAbs(dst) {
			return fmt.Errorf("tmpfs destination must be absolute")
		}
		mounts = append(mounts, ocibundle.Mount{
			Destination: dst, Type: fsTmpfs, Source: fsTmpfs,
			Options: []string{mountNoDev, mountNoSuid, modeSticky, "size=128m"},
		})
	}

	bundle := filepath.Join(work, "bundle")
	if err := os.Mkdir(bundle, 0o700); err != nil {
		return err
	}

	if o.Network {
		// #nosec G306 -- the gateway resolver address must be readable by non-root guest processes.
		if err := os.WriteFile(filepath.Join(bundle, "resolv.conf"), []byte("nameserver "+agent.DefaultGuestGw+"\n"), 0o644); err != nil {
			return err
		}
		mounts = append(mounts, ocibundle.Mount{
			Destination: "/etc/resolv.conf", Type: mountBind, Source: commandBundle + "/resolv.conf",
			Options: []string{mountBind, "ro", mountNoDev, mountNoSuid},
		})
	}

	if err := writeSpec(filepath.Join(bundle, "config.json"), o, mounts); err != nil {
		return err
	}

	cfg.Shares = append(cfg.Shares, &sandbox.ShareConfig{
		HostDir:    bundle,
		Tag:        "config",
		Mountpoint: commandBundle,
		ReadOnly:   true,
	})
	cfg.Bundle = commandBundle
	cfg.HostCommand = true
	cfg.Memory = o.Memory
	cfg.CPUs = int(o.CPUs)

	return nil
}

// stageBind stages one export directory so either a tree or a regular file
// can be mounted within the guest.
func stageBind(b bind, work, tag string, table []mountEntry, uid, gid uint32) (*sandbox.ShareConfig, ocibundle.Mount, error) {
	dir := filepath.Join(work, tag)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, ocibundle.Mount{}, err
	}

	if err := filteredView(b.source, filepath.Join(dir, "data"), table, b.readonly); err != nil {
		return nil, ocibundle.Mount{}, err
	}

	guest := "/host" + tag
	options := []string{"rbind", mountNoDev, mountNoSuid}
	if b.readonly {
		options = append(options, "ro")
	}

	share := &sandbox.ShareConfig{
		HostDir:    dir,
		Tag:        tag,
		Mountpoint: guest,
		ReadOnly:   b.readonly,
		Translate:  &sandbox.ShareTranslate{UID: int(uid), GID: int(gid)},
	}
	mount := ocibundle.Mount{Destination: b.target, Type: mountBind, Source: guest + "/data", Options: options}
	return share, mount, nil
}

func runOnce(ctx context.Context, o Options, in io.Reader, out, errout io.Writer) error {
	if o.Time > 0 {
		var cancel context.CancelFunc
		// #nosec G115 -- validate bounds Time to one year.
		ctx, cancel = context.WithTimeout(ctx, time.Duration(o.Time)*time.Second)
		defer cancel()
	}

	cfg, closeAssets, err := loadArtifacts()
	if err != nil {
		return err
	}
	defer closeAssets()

	cleanup, err := prepare(o, &cfg)
	if err != nil {
		return err
	}
	defer cleanup()

	return runConfigured(ctx, o, cfg, in, out, errout)
}

func runConfigured(ctx context.Context, o Options, cfg sandbox.Config, in io.Reader, out, errout io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	vsock, cleanup, err := sandbox.NewStandaloneVsock()
	if err != nil {
		return err
	}
	defer cleanup()

	vsock.Net = o.Network
	cfg.Vsock = vsock

	sock := fmt.Sprintf("%s_%d", cfg.Vsock.HostSocket, agent.DefaultCommandVsockPort)
	ln, err := net.ListenUnix(unixNetwork, &net.UnixAddr{Name: sock, Net: unixNetwork})
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }() // ListenUnix unlinks its socket on close.
	if err := ln.SetDeadline(time.Now().Add(45 * time.Second)); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()

	logDir := ""
	if cfg.ObserverLogPath != "" {
		logDir = filepath.Dir(cfg.ObserverLogPath)
	}
	log, err := os.CreateTemp(logDir, "vmjail-console-*.log")
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }()
	if cfg.ObserverLogPath == "" {
		cfg.ObserverLogPath = log.Name() + ".resources.jsonl"
	}

	done := make(chan struct{})
	var serveErr error
	go func() {
		serveErr = serveVM(ctx, log, cfg)
		close(done)
		_ = ln.Close()
	}()
	defer func() {
		cancel()
		<-done
	}()

	conn, err := ln.AcceptUnix()
	if err != nil {
		select {
		case <-done:
			if serveErr != nil {
				err = errors.Join(err, serveErr)
			}
		default:
		}

		return fmt.Errorf("guest startup: %w (console: %s)", err, log.Name())
	}

	err = cli.RelayCommand(ctx, conn, in, out, errout)
	if ctx.Err() != nil {
		return fmt.Errorf("VM deadline/cancellation: %w (console: %s)", ctx.Err(), log.Name())
	}

	var workload *cli.ExitError
	if err != nil && !errors.As(err, &workload) {
		return fmt.Errorf("command transport: %w (console: %s)", err, log.Name())
	}

	return err
}
