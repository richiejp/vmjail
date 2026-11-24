# Codex executor inside vmjail

**Experimental.** The executor protocol and teardown pass the credential-free
smoke test. A live model-driven session still requires authenticating the
separate profile and remains to be tested.

Codex 0.167.0 can launch a named execution environment over stdio. The host
runs the Codex harness; vmjail runs `codex exec-server --listen stdio` inside a
VM. Commands within that executor share one VM, including temporary files and
running processes. No Codex fork, shell replacement or TCP listener is needed.

The [environment template](../../contrib/codex/environments.toml) targets NixOS.
Replace its executable and project paths before use. The vmjail executable must
include embedded assets, for example the output of `nix build .#vmjail`.
The Codex path must name the same version used by the host harness. Both Nix
store paths must remain available; retain their build outputs as GC roots for
long-lived use.

## Separate profile

Put the template in a separate Codex home as `environments.toml`. Put these
settings in that directory's `config.toml`:

```toml
approval_policy = "on-request"
sandbox_mode = "workspace-write"

[sandbox_workspace_write]
network_access = true
```

For the prepared profile on this machine, launch:

```sh
CODEX_HOME="$HOME/.codex-vmjail" nix run github:numtide/nix-ai-tools#codex
```

The prepared profile contains no login credentials. Authenticate that profile
before a model-driven session. The host login and other host configuration are
not automatically copied. This configuration does not change the default
`~/.codex` profile or a currently running Codex session.

`default = "vmjail"` selects the executor for new threads. `include_local = false`
removes the ordinary host executor from the environment list. Explicit
app-server thread requests can select `environments = [{environmentId:
"vmjail", cwd: "/absolute/project/path"}]`.

## Files, networking and lifetime

The template starts with an empty guest root and exports only `/nix/store`,
`/run/current-system/sw`, and `/bin` read-only. The selected project is writable
at its original absolute path. It supplies a guest-only `/codex` tmpfs for the
executor's home, alongside guest-owned `/dev`, `/proc`, `/sys` and `/tmp`.
The host home directory and its authentication files are not mounted.

Project writes immediately affect the host. Toolchain additions outside the
listed paths need explicit mounts. A complete development environment, Nix
build daemon access, host sockets, and nested KVM are not provided by this
profile. Secrets stored inside the project or publicly readable Nix store are
still visible to its commands.

Guest networking is enabled at the user's request: `--use_pasta` enables
vmjail's userspace network backend and `network_access = true` permits network
access under Codex's workspace-write policy. Host Codex talks to its model
provider separately; that connection does not originate in the VM.

`-t 0` disables the default ten-minute VM deadline for the persistent executor.
The host harness owns the stdio connection and VM process lifetime. VM memory
and CPU defaults remain 512 MiB and one vCPU; configure `--memory` and
`--max_cpus` for larger builds.

The integration moves executor-owned command and filesystem operations into
the VM. It does not move the host harness, independently configured MCP servers
or host hooks. Codex's standalone app-server `command/exec` method targets its
local environment in 0.167.0; with this profile it returns `local environment is
not configured`. Use an agent thread selecting `vmjail`.

## Verification

Run the credential-free protocol smoke test (Python 3.11+, KVM and cgroup
permissions required):

```sh
python3 scripts/smoke-codex.py "$HOME/.codex-vmjail" --workspace "$PWD"
```

It checks initialization, a guest kernel different from the host, exit status
7, direct workspace writes, temporary state retained across two commands, and
clean shutdown. All checks pass with the default Linux 7.3.6 kernel.
Separately, the installed app-server has successfully initialized a thread
selecting the VM environment with `workspace-write` policy. A live model-driven
turn remains to be tested after the separate profile is authenticated.

Protocol and configuration references: upstream
[exec-server](https://github.com/openai/codex/blob/rust-v0.167.0/codex-rs/exec-server/README.md),
[environment configuration](https://github.com/openai/codex/blob/rust-v0.167.0/codex-rs/exec-server/src/environment_toml.rs),
and [process protocol](https://github.com/openai/codex/blob/rust-v0.167.0/codex-rs/exec-server-protocol/src/protocol.rs).

## Resolved teardown fault (2026-11-23)

With the restricted mount template, both command RPCs finish and project writes
reach the host. Closing executor stdin then triggers this guest fault:

```text
VFS: Busy inodes after unmount of virtiofs (virtiofs)
Oops: invalid opcode
Comm: codex-child-rea
RIP: generic_shutdown_super+0x121/0x130
kill_anon_super -> virtio_kill_sb -> deactivate_locked_super
cleanup_mnt -> task_work_run -> do_exit
Fixing recursive fault but reboot is needed!
```

Linux 7.0.10 could not exit cleanly: the smoke test waited 30 seconds,
terminated the launcher, and failed on exit 125. Removing the separate `/bin`
export reproduced the same failure.

Staged kernel tracing showed that automatic virtiofs submount teardown raced
asynchronous FUSE RELEASE completion. The final Codex thread released its mount
namespace during `do_exit`; Codex itself did not issue an unmount. The trace
caught retained `libpthread.so.0` and `libdl.so.2` inodes immediately before
the assertion. This is the issue fixed upstream by Linux commit
[`06b41351779e`](https://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git/commit/?id=06b41351779e9289e8785694ade9042ae85e41ea).

The patched 7.0.10 comparison kernel passed four consecutive smoke runs while
the unpatched control reproduced the oops. vmjail now defaults to Linux 7.3.6,
which contains the released fix; the full KVM suite and the Codex teardown
smoke test pass on that kernel.

The profile has no credentials and uses `workspace-write`. Neither host login
reuse nor a broader permissions profile was installed. The original host Codex
configuration remains unchanged.
