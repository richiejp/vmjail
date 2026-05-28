{
  description = "darkaifactory - daft CLI";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    # Submodule contents are NOT pulled into the flake source — at
    # ~1.9 GB across linux + u-root they made every flake eval scan
    # the full tree. Each submodule is fetched from upstream via
    # builtins.fetchGit, driven by third_party/submodule-revs.json
    # (url + rev per submodule). A pre-commit hook keeps the JSON
    # in sync with `git submodule status` + `.gitmodules`. fetchGit
    # is content-addressed by rev, so the second eval is
    # cache-instant; first eval on a fresh machine clones once.
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        version = pkgs.lib.strings.removeSuffix "\n" (builtins.readFile ./version.txt);

        # Submodule revs + upstream URLs come from a JSON file the
        # pre-commit hook regenerates from `git submodule status`
        # and `.gitmodules` — see
        # scripts/hooks/check-submodule-revs.sh. fetchSubmodule
        # clones each submodule from its upstream URL at the pinned
        # rev; Nix caches the result by rev so subsequent evals are
        # instant. Local-only submodule patches are not supported
        # in this flow — push the rev before bumping.
        submoduleRevs = builtins.fromJSON (builtins.readFile ./third_party/submodule-revs.json);
        fetchSubmodule = name: builtins.fetchGit {
          inherit (submoduleRevs.${name}) url rev;
        };
        linuxSrc = fetchSubmodule "linux";
        uRootSrc = fetchSubmodule "u-root";
        cloudHypervisorSrc = fetchSubmodule "cloud-hypervisor";
        virtiofsdSrc = fetchSubmodule "virtiofsd";
        crunSrc = fetchSubmodule "crun";

        # goSrc is the project source as Go's tooling sees it. We
        # drop the whole third_party/ tree — none of it is reachable
        # from our Go build, and including it would (a) drag the
        # ~1.9 GB linux + u-root checkouts into every flake eval, and
        # (b) re-introduce the stray Go file in third_party/crun's
        # libocispec/runtime-spec/schema/ that pulls in a module dep
        # we don't want in go.sum. The kernel-config fragment and
        # submodule-revs.json under third_party/ are read by other
        # derivations directly, not via goSrc. Lives in the `let`
        # rather than under `packages` because it is a source, not a
        # derivation, and `nix flake check` type-checks every
        # `packages` output as a derivation.
        goSrc = pkgs.lib.cleanSourceWith {
          src = ./.;
          filter = path: _type:
            let rel = pkgs.lib.removePrefix (toString ./. + "/") (toString path);
            in !(pkgs.lib.hasPrefix "third_party/" rel);
        };

        # mkDaftBinary builds one of our cmd/ binaries from the repo's
        # go.mod. Shared so vendorHash stays defined once — otherwise
        # it drifts the next time go.mod changes.
        #
        # Update vendorHash with `nix build` when go.mod changes; Nix
        # prints the new value in the error.
        # All Go binaries default to CGO_ENABLED=0 so we ship static
        # ELFs that run on any libc. Pure Go has no libc dependency
        # to begin with; CGO=0 just makes that explicit and prevents
        # accidental dynamic-link via a future cgo import. Lives in the
        # `let` rather than under `packages` because it is a function,
        # not a derivation (see goSrc note re: `nix flake check`).
        mkDaftBinary = {
          name,
          subPackage,
          description,
          cgoEnabled ? false,
          platforms ? pkgs.lib.platforms.unix,
        }: pkgs.buildGoModule {
          pname = name;
          inherit version;
          src = goSrc;
          vendorHash = "sha256-dxtYE59zlJS56XoPExikTezIQGesl5YWTNESBmPcy8M=";
          env.CGO_ENABLED = if cgoEnabled then "1" else "0";
          ldflags = [
            "-s" "-w"
            "-X github.com/localai-org/darkaifactory/internal/version.Version=${version}"
          ];
          subPackages = [ subPackage ];
          meta = with pkgs.lib; {
            inherit description platforms;
            license = licenses.mit;
            maintainers = [ ];
          };
        };
      in
      {
        devShells.default =
          let
            # Slow-moving sandbox-host *toolchain* artefacts pinned by
            # the flake. They land on the shell as DAFT_* env vars;
            # `make integration` reads those vars. Their store paths
            # change only when their (upstream) sources change, so they
            # don't churn /nix/store commit-by-commit — unlike the
            # per-commit `initramfs`/`hello-bundle` (assembled under
            # bin/ via Make) and the test-fixture OCI images
            # (`nix build`-ed by `make integration`, NOT inherited here,
            # so shell entry no longer realises them). See PLAN.md
            # "Dev-shell artefacts" / "Dev-shell purification".
            inherit (self.packages.${system})
              cloud-hypervisor kernel virtiofsd crun u-root;
          in
          pkgs.mkShell {
            buildInputs = with pkgs; [
              go
              gopls
              gotools
              go-tools

              # Formatting
              gofumpt

              # Metrics & linting (mirrors the linters configured in .golangci.yml)
              golangci-lint
              gocyclo
              goconst
              scc

              # Protobuf / Connect-RPC codegen. The codegen plugins
              # (protoc-gen-go, protoc-gen-connect-go) are pinned via
              # tools/tools.go and invoked through `go run`, so only
              # buf itself needs to be in the dev shell. `make proto`
              # is the entry point.
              buf

              # `make build-initramfs` calls u-root + gzip + cpio.
              gzip
              cpio

              # internal/testtmux drives a real CLI through a tmux
              # pane running dash. Required by the D6 black-box
              # workflow tests; running unit tests outside the dev
              # shell without these is unsupported by design (same
              # convention as the KVM-required integration tests).
              tmux
              dash
            ];

            shellHook = ''
              # Normalize TMPDIR back to /tmp. `nix develop -c <cmd>`
              # nests a fresh /tmp/nix-shell.X for every invocation,
              # and a few of our unit tests use Unix sockets via
              # t.TempDir() — the Linux sockaddr_un path limit is
              # 108 bytes, which two nested levels of nix-shell
              # TMPDIR + the test's own subdir blow through. The dev
              # shell isn't a Nix build sandbox; /tmp is fine.
              export TMPDIR=/tmp

              # NOTE: the dev shell exports NO sandbox artefacts. The VM
              # toolchain (cloud-hypervisor, kernel, virtiofsd, crun,
              # u-root) and the test-fixture OCI images are not developer
              # tools — nobody runs the kernel at a prompt; they're
              # dependencies *of* the sandbox. Exporting them here forced
              # a build of every artefact at shell entry and froze their
              # store paths, which went stale the moment a baked-in daft
              # binary changed. `make integration` now `nix build`s each
              # one fresh into bin/ and injects the paths for that run; a
              # bare `go test -tags=integration` skips with a hint naming
              # the exact `nix build .#<pkg> -o bin/<pkg>` to run. The
              # production daft-sandboxd embeds its artefacts via
              # //go:embed, so it never needed these either. See PLAN.md
              # "Dev-shell purification".

              echo "darkaifactory dev shell ready (version ${version})"
              echo "  make fmt          - format Go code"
              echo "  make lint         - run golangci-lint"
              echo "  make test         - run tests"
              echo "  make integration  - build initramfs + bundle locally, run e2e"
              echo "  make cover        - update coverage.txt baseline"
              echo "  make hooks        - install git hooks"
            '';
          };

        packages = rec {
          default = daft;

          # u-root-src exposes the pinned u-root submodule source as a
          # buildable package so `make integration` can `nix build` it
          # into bin/ (the raw fetchGit result is not a valid flake
          # package output). build-initramfs runs u-root against this
          # tree. (goSrc + mkDaftBinary live in the outer `let` — they
          # are a source and a function, not derivations, so they can't
          # be `packages` outputs without failing `nix flake check`.)
          u-root-src = pkgs.runCommand "u-root-src" { } "cp -r ${uRootSrc} $out";

          daft = mkDaftBinary {
            name = "daft";
            subPackage = "cmd/daft";
            description = "darkaifactory daft CLI";
          };

          # daft-agent runs inside our minimal microVM rootfs which has
          # no libc, so it must be statically linked. (Default cgoEnabled
          # = false on mkDaftBinary makes this implicit; kept here only
          # for the platform restriction.)
          daft-agent = mkDaftBinary {
            name = "daft-agent";
            subPackage = "cmd/daft-agent";
            description = "darkaifactory in-VM sandbox agent";
            platforms = pkgs.lib.platforms.linux;
          };

          # daft-orchestrator is the central control-plane binary:
          # mTLS-authenticated Connect-RPC over HTTP/2, with TOFU
          # peer approvals managed via internal/identity. v0 ships
          # a single Health.Ping endpoint to prove the trust path
          # works end-to-end; the registry handler, port forwarder,
          # and terminal stream join the same listener later.
          daft-orchestrator = mkDaftBinary {
            name = "daft-orchestrator";
            subPackage = "cmd/daft-orchestrator";
            description = "darkaifactory orchestrator control-plane";
          };

          # kconfig-verify is a build-time tool: it asserts that every
          # Kconfig symbol our fragments request survived `olddefconfig`
          # (which silently demotes symbols with unmet deps). Used by the
          # kernel derivation's configurePhase below.
          kconfig-verify = mkDaftBinary {
            name = "kconfig-verify";
            subPackage = "cmd/kconfig-verify";
            description = "darkaifactory kernel-config survival checker";
            platforms = pkgs.lib.platforms.linux;
          };

          # daft-hello-container is the workload binary in the v0
          # smoke-test OCI bundle. It runs *inside the container*
          # (which runs inside the sandbox VM), prints
          # agent.ContainerBanner, and exits 0.
          daft-hello-container = mkDaftBinary {
            name = "daft-hello-container";
            subPackage = "cmd/daft-hello-container";
            description = "darkaifactory hello-world container workload";
            platforms = pkgs.lib.platforms.linux;
          };

          # daft-hello-server is the D6b.4 dev-server proof
          # workload: a static HTTP listener with a known body.
          # Baked into the nix-shell-image so the integration test
          # can `daft workspace exec` it and then prove the
          # port-forward path end to end.
          daft-hello-server = mkDaftBinary {
            name = "daft-hello-server";
            subPackage = "cmd/daft-hello-server";
            description = "darkaifactory hello-world HTTP server (test workload)";
            platforms = pkgs.lib.platforms.linux;
          };

          # daft-http-fetch is the D6c.2 slice-4 client workload:
          # a tiny HTTPS GET that prints a known-shape sentinel on
          # success. Baked into the nix-shell-image so the
          # integration test can `daft workspace exec` it against
          # an httptest TLS server bound to the host's loopback —
          # reached through sandboxnet's DefaultHostLoopbackIP NAT
          # — and prove the container's network egress works.
          daft-http-fetch = mkDaftBinary {
            name = "daft-http-fetch";
            subPackage = "cmd/daft-http-fetch";
            description = "darkaifactory HTTPS GET client (test workload)";
            platforms = pkgs.lib.platforms.linux;
          };

          # nix-shell-image is a minimal Nix-built OCI image used by
          # the D6b.3 integration test. It carries dash + coreutils
          # (so /bin/sh and /bin/sleep resolve inside the container)
          # and an Entrypoint of `sleep infinity` so the container
          # stays alive long enough for `daft workspace exec` to
          # crun-exec into it. dockerTools.buildLayeredImage produces
          # a docker-format tarball; the test loads it with
          # go-containerregistry's tarball.ImageFromPath, pushes to
          # an in-process registry, and points the workspace at that
          # registry ref.
          #
          # D6b.4 will replace this with a nix-tooling image so the
          # dev-server proof can actually invoke `nix develop` /
          # `nix run` inside the sandbox.
          nix-shell-image = pkgs.dockerTools.buildLayeredImage {
            name = "daft-nix-shell";
            tag = "v1";
            # daft-hello-server is the D6b.4 dev-server workload —
            # exec'd via `daft workspace exec` after the container
            # is up so the port-forward test has something to GET
            # against. Its /bin/daft-hello-server lands here via
            # the same dockerTools buildEnv symlink mechanism as
            # dash and coreutils. daft-http-fetch joins the same
            # buildEnv for the D6c.2 slice-4 HTTPS-egress proof.
            contents = [
              pkgs.dash pkgs.coreutils
              daft-hello-server daft-http-fetch
            ];
            # The entrypoint prints a readiness sentinel on container
            # startup, then execs sleep so the container stays alive.
            # The sentinel travels container stdout → agent → vsock →
            # sandboxd stdout, where the D6b.3 test waits on it via
            # Cluster.WaitForSandboxdOutput before driving
            # `daft workspace exec`. Mirrors the role of
            # agent.ContainerBanner for the hello bundle.
            config = {
              Entrypoint = [
                "/bin/dash" "-c"
                "echo daft-nix-shell ready; exec /bin/sleep infinity"
              ];
              Env = [ "PATH=/bin" ];
            };
          };

          # nix-tooling-image is the D6c.1 hermetic-nix-CLI image.
          # It carries pkgs.nix + python3 + bashInteractive +
          # coreutils + cacert, plus an initialized /nix/var/nix/db
          # populated with the closure so `nix` can resolve the
          # store paths the test's flake fixture references via
          # `builtins.storePath`. /etc/nix/nix.conf enables the
          # nix-command + flakes experimental features and disables
          # all substituters — the container has no network, every
          # path the test needs is already in /nix/store.
          #
          # ~500MB uncompressed (most of which is python3 + nix
          # itself). dockerTools chunks the closure across many
          # small layers so the test-side load + push to the in-
          # process registry stays under a second on warm runs.
          nix-tooling-image =
            let
              pyEnv = pkgs.buildEnv {
                # pkgs.python3 by itself lands under
                # /nix/store/<hash>-python3-<v> with the interpreter
                # at .../bin/python3. The serve app references
                # python3 directly via store path, so no env wrap
                # is needed for the flake — but we still include it
                # here as a "contents" root so its closure is in
                # the image and registered with the nix db.
                name = "d6c1-py-env";
                paths = [ pkgs.python3 ];
              };
              closure = pkgs.closureInfo {
                rootPaths = [
                  pkgs.nix pkgs.bashInteractive pkgs.coreutils
                  pkgs.dash pkgs.cacert pyEnv pkgs.python3
                  pkgs.curl
                ];
              };
            in pkgs.dockerTools.buildLayeredImage {
              name = "daft-nix-tooling";
              tag = "v1";
              # pkgs.curl is on board so the D6c.4 narinfo probe
              # (TestClusterWorkspace404NarinfoProbe) can drive an
              # HTTPS GET through the egress proxy without involving
              # nix. Useful diagnostic any time the nix↔proxy edge
              # behaves suspiciously.
              contents = [
                pkgs.dash pkgs.coreutils pkgs.bashInteractive
                pkgs.nix pkgs.cacert pkgs.python3 pkgs.curl
              ];
              # fakeRootCommands runs inside a proot chroot rooted
              # at the assembled image; /nix/store reflects the
              # image's view, so paths registered here resolve
              # correctly at runtime inside the container.
              enableFakechroot = true;
              fakeRootCommands = ''
                mkdir -p /etc/nix /nix/var/nix/db \
                  /nix/var/nix/profiles /nix/var/nix/gcroots \
                  /tmp /etc/ssl/certs
                cat > /etc/nix/nix.conf <<'CONF'
                experimental-features = nix-command flakes
                substituters =
                require-sigs = false
                build-users-group =
                sandbox = false
                # rootfs is mounted read-only and the container
                # has no CAP_SYS_ADMIN, so nix can't mount-remount
                # /nix/store rw. read-only-local-store tells nix
                # to treat the store as immutable and skip the
                # remount; fine for D6c.1 since everything the
                # flake references is already in the store.
                read-only-local-store = true
                CONF
                ${pkgs.nix}/bin/nix-store --load-db < ${closure}/registration
                ln -sf ${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt \
                  /etc/ssl/certs/ca-bundle.crt
              '';
              config = {
                # Entrypoint sentinel mirrors nix-shell-image's
                # "daft-nix-shell ready" pattern so the test can
                # wait on a stable string in sandboxd output.
                Entrypoint = [
                  "/bin/dash" "-c"
                  "echo daft-nix-tooling ready; exec /bin/sleep infinity"
                ];
                Env = [
                  "PATH=/bin"
                  "HOME=/tmp"
                  "USER=root"
                  "NIX_PAGER=cat"
                  "NIX_SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt"
                ];
              };
            };

          # d6c1-flake-fixture is the tiny flake the D6c.1 test
          # mounts at /workspace. Its `serve` app's `program`
          # points at python3 inside the nix-tooling-image's
          # closure via store path, so `nix run /workspace#serve`
          # finds the binary without evaluating any other
          # derivation. The fixture is shipped as a writeTextDir
          # output so the test can `cp $DAFT_NIX_FLAKE_FIXTURE/
          # flake.nix mountSrc/`.
          d6c1-flake-fixture = pkgs.writeTextDir "flake.nix" ''
            {
              description = "D6c.1 hermetic dev shell";
              outputs = { self }: {
                apps.x86_64-linux.serve = {
                  type = "app";
                  program = "${pkgs.python3}/bin/python3";
                };
              };
            }
          '';

          # hello-bundle is the OCI runtime bundle the agent feeds to
          # crun: a directory containing config.json + rootfs/ with
          # one statically-linked /hello binary. The host shares the
          # bundle dir into the guest via virtiofs and surfaces its
          # path as `daft.bundle=...` on the kernel cmdline.
          hello-bundle = pkgs.runCommand "daft-hello-bundle" {} ''
            mkdir -p $out/rootfs
            cp ${daft-hello-container}/bin/daft-hello-container $out/rootfs/hello
            chmod +x $out/rootfs/hello
            cp ${./contrib/hello-bundle/config.json} $out/config.json
          '';

          # daft-sandboxd is the single-executable sandbox-host. It
          # embeds cloud-hypervisor, the guest kernel, and the
          # initramfs into the binary at build time, so the resulting
          # ELF runs on any Linux distro of the same arch with no
          # external runtime dependencies.
          daft-sandboxd = pkgs.buildGoModule {
            pname = "daft-sandboxd";
            inherit version;
            src = goSrc;
            vendorHash = "sha256-dxtYE59zlJS56XoPExikTezIQGesl5YWTNESBmPcy8M=";
            env.CGO_ENABLED = "0";

            # Stage the real artifacts into internal/bundled/assets/
            # before the Go build, so //go:embed picks them up.
            preBuild = ''
              cp ${cloud-hypervisor}/bin/cloud-hypervisor internal/bundled/assets/cloud-hypervisor
              cp ${kernel}/bzImage internal/bundled/assets/bzImage
              cp ${initramfs} internal/bundled/assets/initramfs.cpio.gz
              cp ${virtiofsd}/bin/virtiofsd internal/bundled/assets/virtiofsd
            '';

            ldflags = [
              "-s" "-w"
              "-X github.com/localai-org/darkaifactory/internal/version.Version=${version}"
            ];
            subPackages = [ "cmd/daft-sandboxd" ];

            meta = with pkgs.lib; {
              description = "darkaifactory sandbox-host (single-executable, statically linked)";
              license = licenses.mit;
              maintainers = [ ];
              platforms = platforms.linux;
            };
          };

          # Sandbox-VM initramfs, assembled by u-root.
          #
          # Layout:
          #   /init           — u-root's pure-Go init (cmds/core/init);
          #                     mounts /proc, /sys, devtmpfs, cgroup2, etc.,
          #                     reaps zombies, and execs /bin/uinit.
          #   /bin/uinit ->   /bin/daft-agent (symlink, set via -uinitcmd)
          #   /bin/daft-agent — our statically-linked agent.
          #   /bin/crun       — the OCI runtime the agent execs to launch
          #                     workload containers; static-musl build.
          #
          # Hardening posture (matches PLAN.md "Hardening"):
          #   -defaultsh=""    no shell symlink
          #   -base=/dev/null  no default /etc, /bin/sh, etc.
          # Only u-root's `init` busybox-binary, our agent, and crun end up
          # in the archive — no interpreters, no diagnostic tools, zero
          # setuid (crun is built without the +s helper bits).
          initramfs = pkgs.runCommand "daft-initramfs.cpio.gz" {
            nativeBuildInputs = [ pkgs.go pkgs.gzip pkgs.cpio ];
          } ''
            export HOME=$TMPDIR
            export GOCACHE=$TMPDIR/go-cache
            export GOPATH=$TMPDIR/go
            # u-root spawns `go` to compile init; it locates the toolchain
            # via $GOROOT/bin/go, so spell GOROOT out explicitly. (Nix's
            # `go` package puts the toolchain under share/go.)
            export GOROOT=${pkgs.go}/share/go
            # u-root needs to resolve cmds/core/init's package path inside
            # its own module, so run from a writable copy of the checkout.
            # --no-preserve=mode lands the files writable so we don't need
            # a follow-up chmod -R u+w.
            cp -r --no-preserve=mode ${uRootSrc} u-root-src
            cd u-root-src

            ${u-root}/bin/u-root \
              -o $TMPDIR/initramfs.cpio \
              -defaultsh="" \
              -base=/dev/null \
              -files "${daft-agent}/bin/daft-agent:bin/daft-agent" \
              -files "${crun}/bin/crun:bin/crun" \
              -uinitcmd=/bin/daft-agent \
              ./cmds/core/init

            gzip < $TMPDIR/initramfs.cpio > $out
          '';

          # Linux kernel for the sandbox VM, built from the pinned
          # submodule. Config is assembled from the ground up — tinyconfig
          # + kvm_guest + our microvm/daft/hardening fragments (see
          # configurePhase) — not from defconfig. Output: bzImage (suitable
          # for Cloud Hypervisor PVH or bzImage boot).
          kernel = pkgs.stdenv.mkDerivation {
            pname = "linux-daft";
            version = "7.0.10";

            src = linuxSrc;

            nativeBuildInputs = with pkgs; [
              bc
              bison
              cpio
              elfutils
              flex
              gmp
              libmpc
              mpfr
              openssl
              pahole
              perl
              rsync
              zstd
              python3
              # LLVM toolchain for the *target* kernel: enables KCFI
              # (CONFIG_CFI, forward-edge control-flow integrity — gcc can't do
              # this) and trivial cross-compile (one clang targets every arch).
              # Use the UNWRAPPED clang: the Nix cc-wrapper injects
              # -nostdlibinc and mishandles the kernel's --target triple, which
              # breaks the build; the kernel target compile is freestanding
              # (-nostdinc/-ffreestanding) and only needs clang's own
              # resource-dir headers, so the wrapper buys us nothing here.
              llvmPackages.clang-unwrapped
              lld
              llvm
              # Our build-time checker (resolves to the flake package via the
              # packages `rec` scope, not pkgs); see configurePhase.
              kconfig-verify
            ];

            # Build the *target* kernel with the LLVM toolchain (clang +
            # ld.lld + llvm-* binutils) to enable KCFI (CONFIG_CFI) and get
            # trivial cross-compile, while leaving the *host* tools (objtool,
            # modpost, fixdep) on gcc. A blanket LLVM=1 would also force
            # HOSTCC=clang, and the Nix clang wrapper misbehaves on the
            # kernel's `--target=x86_64-linux-gnu` (it expects the
            # `-unknown-linux-gnu` triple), dropping nativeBuildInput include
            # paths so host tools can't find e.g. <gelf.h>. The gcc host
            # wrapper has no such issue. $kmakeTools is expanded into every
            # `make` in the phases below.
            kmakeTools = "CC=clang LD=ld.lld AR=llvm-ar NM=llvm-nm STRIP=llvm-strip OBJCOPY=llvm-objcopy OBJDUMP=llvm-objdump READELF=llvm-readelf HOSTCC=gcc";

            # The Nix cc-wrapper injects hardening/stack-protector flags that
            # collide with the kernel's own flag management (gcc host tools);
            # the kernel sets exactly the flags it wants via Kconfig.
            hardeningDisable = [ "all" ];

            # Some kernel build steps look for /usr/bin/env or /usr/bin/perl
            # via shebangs; let Nix patch them.
            postPatch = ''
              patchShebangs scripts/
            '';

            configurePhase = ''
              runHook preConfigure
              # Build the config from the ground up rather than from a
              # bloated defconfig: tinyconfig (allnoconfig + tiny.config) is
              # the minimal base, then merge_config.sh folds the fragments in
              # order — upstream KVM-guest essentials, our microvm base (re-add
              # what tinyconfig drops + trim), daft features, and KSPP
              # hardening last so it wins. allnoconfig sets CONFIG_EXPERT, which
              # is what lets the fragments turn off otherwise-forced symbols
              # (e.g. io_uring). -m merges without running conf; olddefconfig
              # then resolves dependencies and new-symbol defaults.
              make $kmakeTools tinyconfig
              ./scripts/kconfig/merge_config.sh -m -O . .config \
                kernel/configs/kvm_guest.config \
                ${./third_party/kernel-config/microvm-base.config} \
                ${./third_party/kernel-config/daft.config} \
                ${./third_party/kernel-config/microvm-hardening.config}
              make $kmakeTools olddefconfig
              # `make olddefconfig` has been observed to silently drop
              # CONFIG_TUN between otherwise-identical builds (same
              # source, same fragment, different output) — likely a
              # symptom of a dependency that defaults to n. Re-assert
              # it explicitly and refold; without TUN the in-VM agent
              # can't open /dev/net/tun and every egress test fails
              # with ENETUNREACH.
              ./scripts/config --enable TUN
              make $kmakeTools olddefconfig
              # Fail the build if olddefconfig silently demoted any symbol our
              # fragments requested (unmet deps after a toolchain/kernel bump —
              # an explicit CONFIG_X=y is only a request). This subsumes the
              # old CONFIG_TUN grep guard and also covers CFI/FineIBT, the KSPP
              # hardening set, FILE_LOCKING, KVM, etc. — the kind of silent
              # drop that otherwise only surfaces as a runtime e2e failure.
              kconfig-verify .config \
                ${./third_party/kernel-config/microvm-base.config} \
                ${./third_party/kernel-config/daft.config} \
                ${./third_party/kernel-config/microvm-hardening.config}
              runHook postConfigure
            '';

            buildPhase = ''
              runHook preBuild
              make $kmakeTools -j$NIX_BUILD_CORES bzImage
              runHook postBuild
            '';

            installPhase = ''
              runHook preInstall
              mkdir -p $out
              cp arch/x86/boot/bzImage $out/bzImage
              cp .config $out/config
              cp System.map $out/System.map
              runHook postInstall
            '';

            # The kernel build determinism story is not great — timestamps
            # and the like leak in. Don't pretend otherwise.
            enableParallelBuilding = true;
            dontStrip = true;
            dontPatchELF = true;

            meta = with pkgs.lib; {
              description = "Linux kernel for the daft sandbox VM";
              homepage = "https://kernel.org/";
              license = licenses.gpl2Only;
              maintainers = [ ];
              platforms = [ "x86_64-linux" ];
            };
          };

          # Cloud Hypervisor — our v1 VMM, built from our pinned
          # submodule, statically linked against musl so the resulting
          # ELF runs on any Linux distro of the same arch. We bundle it
          # into the sandbox-host single executable (daft-sandboxd), so
          # static linking is load-bearing for portability.
          cloud-hypervisor = pkgs.pkgsStatic.rustPlatform.buildRustPackage {
            pname = "cloud-hypervisor";
            version = "51.1";

            src = cloudHypervisorSrc;

            cargoLock = {
              lockFile = "${cloudHypervisorSrc}/Cargo.lock";
              outputHashes = {
                "micro_http-0.1.0" = "sha256-7/4qOdb983JD70ZgpZv/fa5HFWMSQk5JseRpPQ0OU0k=";
              };
            };

            # The workspace ships several binaries; we only need the main
            # cloud-hypervisor for boot. ch-remote is useful for poking at
            # a running VM from the shell — keep it too.
            cargoBuildFlags = [ "--bin" "cloud-hypervisor" "--bin" "ch-remote" ];

            # Cloud Hypervisor's tests assume a fully-featured KVM host
            # plus root and a network. None of that is available in the
            # Nix build sandbox.
            doCheck = false;

            meta = with pkgs.lib; {
              description = "Cloud Hypervisor microVM monitor (static-musl)";
              homepage = "https://www.cloudhypervisor.org/";
              license = with licenses; [ asl20 bsd3 ];
              maintainers = [ ];
              platforms = [ "x86_64-linux" "aarch64-linux" ];
            };
          };

          # virtiofsd — vhost-user virtio-fs daemon, built from the
          # pinned submodule. Spawned by the sandbox controller alongside
          # cloud-hypervisor to back `--fs tag=...,socket=...` shares;
          # the in-VM kernel mounts the share via virtiofs.
          #
          # Statically linked against musl via pkgsStatic. The
          # capng/libseccomp Rust bindings read $LIBCAPNG_LINK_TYPE /
          # $LIBSECCOMP_LINK_TYPE — we set them to "static" and feed
          # the static .a paths so rustc embeds the archives rather
          # than the shared libs. The result is a fully self-contained
          # binary that drops into the daft-sandboxd bundle alongside
          # cloud-hypervisor and crun.
          virtiofsd = pkgs.pkgsStatic.rustPlatform.buildRustPackage {
            pname = "virtiofsd";
            version = "1.9.0";

            src = virtiofsdSrc;

            cargoLock = {
              lockFile = "${virtiofsdSrc}/Cargo.lock";
            };

            nativeBuildInputs = [ pkgs.pkg-config ];
            buildInputs = with pkgs.pkgsStatic; [ libseccomp libcap_ng ];

            env.LIBCAPNG_LINK_TYPE = "static";
            env.LIBCAPNG_LIB_PATH = "${pkgs.pkgsStatic.libcap_ng}/lib";

            doCheck = false;

            meta = with pkgs.lib; {
              description = "virtio-fs vhost-user device daemon";
              homepage = "https://virtio-fs.gitlab.io/";
              license = with licenses; [ asl20 bsd3 ];
              maintainers = [ ];
              platforms = [ "x86_64-linux" "aarch64-linux" ];
            };
          };

          # crun — OCI container runtime, built from the pinned submodule
          # and statically linked against musl. The agent execs this
          # inside the VM to run the workload container; static linking
          # means we just drop the binary into the initramfs and trust
          # nothing else.
          #
          # We disable every optional integration (systemd, CRIU,
          # libkrun, wasm, python/lua bindings) — the VM is the
          # security boundary and we don't want extra C deps in the
          # initramfs. Capabilities + seccomp + bpf stay on (they're
          # the basic isolation primitives crun configures via cgroup
          # v2 + namespaces).
          crun = pkgs.pkgsStatic.stdenv.mkDerivation {
            pname = "crun";
            version = "1.9.2";

            # crun has deeper nested submodules (libocispec → image-spec
            # / runtime-spec / yajl). fetchGit with submodules=true
            # recurses through them at the pinned rev.
            src = builtins.fetchGit {
              inherit (submoduleRevs."crun") url rev;
              submodules = true;
            };

            nativeBuildInputs = with pkgs; [
              autoreconfHook
              pkg-config
              python3
              go-md2man
              git
            ];
            buildInputs = with pkgs.pkgsStatic; [
              libcap
              libseccomp
              yajl
              # musl doesn't ship glibc's argp_parse; crun's CLI parser
              # needs it, so link the standalone implementation.
              argp-standalone
            ];

            configureFlags = [
              "--disable-systemd"
              "--disable-criu"
              "--disable-shared"
              "--enable-embedded-yajl"
              "--disable-dl"
            ];

            # crun ships a vendored libocispec via submodule; the
            # nested submodule is checked in here as third_party/crun/
            # libocispec, but autoreconf runs from the crun root.
            #
            # The Makefile generates git-version.h from `git describe`,
            # which doesn't work in the Nix sandbox (no .git here);
            # pre-write the tarball variant the Makefile falls back to,
            # so the generated header is just a static version string.
            postPatch = ''
              patchShebangs build-aux libocispec/build-aux 2>/dev/null || true
              echo '#define GIT_VERSION "1.9.2"' > .tarball-git-version.h
            '';

            # The build links libcrun.a into crun, then the final crun
            # binary against pkgsStatic libcap / libseccomp / yajl. With
            # --disable-shared and pkgsStatic libs the resulting ELF is
            # statically linked.
            #
            # crun's tree includes a fuzzer test (tests_libcrun_fuzzer.c)
            # that fails to build on gcc 15 due to a sig_chld function
            # pointer mismatch. We don't run any of crun's tests — they
            # need a privileged Linux host — so allow the incompatible
            # pointer types error as a warning. Affects only test
            # builds; the crun binary itself doesn't include them.
            env.NIX_CFLAGS_COMPILE = "-Wno-error=incompatible-pointer-types";

            doCheck = false;

            meta = with pkgs.lib; {
              description = "OCI container runtime (static-musl)";
              homepage = "https://github.com/containers/crun";
              license = licenses.gpl2Plus;
              maintainers = [ ];
              platforms = platforms.linux;
            };
          };

          # u-root command, built from our pinned submodule. Used to
          # assemble future-slice initramfs variants once we move off
          # daft-agent-as-PID-1.
          u-root = pkgs.buildGoModule {
            pname = "u-root";
            version = "0.16.0";

            src = uRootSrc;

            # u-root checks in its vendored dependencies, so we don't need
            # Nix to pre-fetch a module cache for it.
            vendorHash = null;

            env.CGO_ENABLED = "0";

            subPackages = [ "." ];

            # u-root's tests assume a writable HOME, network access, and
            # other host services we don't want to exercise in a Nix
            # sandbox. We're vendoring it as a build tool, not a library
            # of ours, so its own test suite isn't our concern here.
            doCheck = false;

            meta = with pkgs.lib; {
              description = "u-root: a Go-based root file system / initramfs builder";
              homepage = "https://u-root.org";
              license = licenses.bsd3;
              maintainers = [ ];
              platforms = platforms.unix;
            };
          };
        };
      });
}
