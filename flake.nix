{
  description = "darkaifactory - daft CLI";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    # Pull submodule contents into the flake source tree. Nix flakes
    # otherwise ignore submodules; with this set, ./third_party/<sub> is
    # visible to derivations without the user needing `?submodules=1`.
    self.submodules = true;
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        version = pkgs.lib.strings.removeSuffix "\n" (builtins.readFile ./version.txt);
      in
      {
        devShells.default =
          let
            # Slow-moving sandbox-host artefacts pinned by the flake.
            # They land on the shell as DAFT_* env vars; `make
            # integration` reads those vars (with a fall-back to
            # ./result-* for the legacy nix-build path). Their store
            # paths change only when their sources change, so a
            # commit-by-commit iteration doesn't add anything to
            # /nix/store — unlike the per-commit `initramfs` and
            # `hello-bundle` derivations, which now assemble locally
            # under bin/ via Make. See PLAN.md "Dev-shell artefacts"
            # for the rationale.
            inherit (self.packages.${system})
              cloud-hypervisor kernel virtiofsd crun u-root nix-shell-image;
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
            ] ++ [
              # The slow-moving artefacts as build inputs so their
              # store paths are realised when the shell starts and
              # stay alive as long as the shell exists.
              cloud-hypervisor
              kernel
              virtiofsd
              crun
              u-root
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

              # Pin the artefact paths that `make integration` consumes.
              # Updating any of these requires editing the corresponding
              # derivation in flake.nix; the path stays stable across
              # commits otherwise.
              export DAFT_CLOUD_HYPERVISOR="${cloud-hypervisor}/bin/cloud-hypervisor"
              export DAFT_KERNEL="${kernel}/bzImage"
              export DAFT_VIRTIOFSD="${virtiofsd}/bin/virtiofsd"
              export DAFT_CRUN="${crun}/bin/crun"
              export DAFT_UROOT="${u-root}/bin/u-root"
              export DAFT_UROOT_SRC="${./third_party/u-root}"
              # OCI image tarball for D6b.3+ tests: dash + coreutils
              # (no nix binary yet — D6b.4 will swap in a full nix
              # toolchain). The path points at the docker-format
              # tarball; tests load it via go-containerregistry's
              # tarball.ImageFromPath and push to an in-process
              # registry the sandboxd can pull from.
              export DAFT_NIX_IMAGE="${nix-shell-image}"

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

          # goSrc is the project source as Go's tooling sees it: our
          # repo minus third_party/crun, which contains a stray
          # validate.go (under libocispec/runtime-spec/schema/) that
          # references a Go module dep we don't want in our go.sum.
          # crun is a C project; its Go file is upstream test
          # infrastructure, irrelevant to our build.
          goSrc = pkgs.lib.cleanSourceWith {
            src = ./.;
            filter = path: _type:
              let rel = pkgs.lib.removePrefix (toString ./. + "/") (toString path);
              in !(pkgs.lib.hasPrefix "third_party/crun" rel);
          };

          # mkDaftBinary builds one of our two cmd/ binaries from the
          # repo's go.mod. Shared so vendorHash stays defined once —
          # otherwise it drifts the next time go.mod changes.
          #
          # Update vendorHash with `nix build` when go.mod changes; Nix
          # prints the new value in the error.
          # All Go binaries default to CGO_ENABLED=0 so we ship static
          # ELFs that run on any libc. Pure Go has no libc dependency
          # to begin with; CGO=0 just makes that explicit and prevents
          # accidental dynamic-link via a future cgo import.
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
            vendorHash = "sha256-/KsYiRXfsDt8vLvKADlnsdM2e6xI4Mc/4xuLS0UVCvU=";
            env.CGO_ENABLED = if cgoEnabled then "1" else "0";
            ldflags = [
              "-s" "-w"
              "-X github.com/richiejp/darkaifactory/internal/version.Version=${version}"
            ];
            subPackages = [ subPackage ];
            meta = with pkgs.lib; {
              inherit description platforms;
              license = licenses.mit;
              maintainers = [ ];
            };
          };

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
            # dash and coreutils.
            contents = [ pkgs.dash pkgs.coreutils daft-hello-server ];
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
            vendorHash = "sha256-8YS/GSe85WpSdhgNU9PdPUcmPH3GljNT2s0NuF3akXQ=";
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
              "-X github.com/richiejp/darkaifactory/internal/version.Version=${version}"
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
            cp -r --no-preserve=mode ${./third_party/u-root} u-root-src
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
          # submodule with our defconfig fragment merged on top of the
          # x86_64 defconfig. Output: bzImage (suitable for Cloud
          # Hypervisor PVH or bzImage boot).
          kernel = pkgs.stdenv.mkDerivation {
            pname = "linux-daft";
            version = "6.19.14";

            src = ./third_party/linux;

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
            ];

            # Some kernel build steps look for /usr/bin/env or /usr/bin/perl
            # via shebangs; let Nix patch them.
            postPatch = ''
              patchShebangs scripts/
            '';

            configurePhase = ''
              runHook preConfigure
              make x86_64_defconfig
              cat ${./third_party/kernel-config/daft.config} >> .config
              # Fold the fragment options into a coherent .config (sets
              # defaults for any new dependencies, drops obsolete keys).
              make olddefconfig
              runHook postConfigure
            '';

            buildPhase = ''
              runHook preBuild
              make -j$NIX_BUILD_CORES bzImage
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

            src = ./third_party/cloud-hypervisor;

            cargoLock = {
              lockFile = ./third_party/cloud-hypervisor/Cargo.lock;
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

            src = ./third_party/virtiofsd;

            cargoLock = {
              lockFile = ./third_party/virtiofsd/Cargo.lock;
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

            # builtins.path copies the working-tree contents directly,
            # which is what we want here: crun's libocispec submodule has
            # its own deeper submodules (image-spec, runtime-spec, yajl)
            # and Nix flakes' inputs.self.submodules only recurses one
            # level. Using a path src side-steps the flake resolver.
            src = builtins.path {
              name = "crun-source";
              path = ./third_party/crun;
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

            src = ./third_party/u-root;

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
