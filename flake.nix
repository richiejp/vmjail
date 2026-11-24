{
  description = "vmjail local microVM sandbox";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    # Submodule contents are NOT pulled into the flake source — at
    # ~1.9 GB across linux + u-root they made every flake eval scan
    # the full tree. Runtime submodules are fetched from their pinned
    # revisions; Linux uses its fixed-hash release tarball so a build
    # neither clones kernel history nor depends on the remote Git HEAD.
  };

  outputs = { self, nixpkgs, flake-utils }:
    (flake-utils.lib.eachDefaultSystem (system:
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
        kernelVersion = "7.3.6";
        linuxSrc = pkgs.fetchurl {
          url = "https://cdn.kernel.org/pub/linux/kernel/v7.x/linux-${kernelVersion}.tar.xz";
          hash = "sha256-gYPUdadqD6/6ktbNUAr0IfWJ39AwCg8QYLuHXhHUVyM=";
        };
        uRootSrc = fetchSubmodule "u-root";
        cloudHypervisorSrc = fetchSubmodule "cloud-hypervisor";
        virtiofsdSrc = fetchSubmodule "virtiofsd";
        crunSrc = fetchSubmodule "crun";

        # goSrc is the project source as Go's tooling sees it. We
        # drop the whole third_party/, vendor/, and agent-skill trees — none is reachable
        # from our Go build, and including it would (a) drag the
        # ~1.9 GB linux + u-root checkouts into every flake eval, and
        # (b) re-introduce the stray Go file in third_party/crun's
        # libocispec/runtime-spec/schema/ that pulls in a module dep
        # we don't want in go.sum. PLAN.md is likewise not a build input.
        # The kernel-config fragment and
        # submodule-revs.json under third_party/ are read by other
        # derivations directly, not via goSrc. Lives in the `let`
        # rather than under `packages` because it is a source, not a
        # derivation, and `nix flake check` type-checks every
        # `packages` output as a derivation.
        goSrc = pkgs.lib.cleanSourceWith {
          src = ./.;
          filter = path: type:
            let name = baseNameOf path;
            in !(
              (type == "directory" && (name == "third_party" || name == "vendor" || name == ".agents"))
              || name == "PLAN.md"
            );
        };

        # The kernel depends on kconfig-verify, but that tiny build-time tool
        # does not depend on the rest of the repository. Giving it the broad
        # goSrc made every unrelated source/test/doc edit invalidate the
        # expensive Linux derivation.
        kconfigVerifySrc = pkgs.lib.fileset.toSource {
          root = ./.;
          fileset = pkgs.lib.fileset.unions [
            ./go.mod
            ./go.sum
            ./cmd/kconfig-verify
            ./internal/kconfig
          ];
        };

        # kconfigVerifySrc intentionally contains only the packages reachable
        # from the kernel's small build-time verifier. buildGoModule vendors
        # that source independently, so it must not reuse the full-project
        # module graph hash below.
        kconfigVerifyVendorHash = "sha256-FUQsIWPumWRthlrJ71jHztjPRlUNTh0pAcbxZq74NuM=";

        # mkVmjailBinary builds one of our cmd/ binaries from the repo's
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
        # preBuild stages embedded assets into the source tree before the
        # Go build (e.g. the orchestrator's bundled build image). Empty by
        # default and omitted from the derivation when empty, so binaries
        # that embed nothing keep their existing build hash.
        mkVmjailBinary = {
          name,
          subPackage,
          description,
          cgoEnabled ? false,
          platforms ? pkgs.lib.platforms.unix,
          preBuild ? "",
          src ? goSrc,
          vendorHash ? "sha256-tfCzB6+pJqC0iW/DAh6GtZt8/hlxVghlgxjmr+2tTH8=",
        }: pkgs.buildGoModule ({
          pname = name;
          inherit version;
          inherit src;
          inherit vendorHash;
          env.CGO_ENABLED = if cgoEnabled then "1" else "0";
          ldflags = [
            "-s" "-w"
            "-X github.com/richiejp/vmjail/internal/version.Version=${version}"
          ];
          subPackages = [ subPackage ];
          meta = with pkgs.lib; {
            inherit description platforms;
            license = licenses.asl20;
            maintainers = [ ];
          };
        } // pkgs.lib.optionalAttrs (preBuild != "") { inherit preBuild; });
      in
      {
        devShells.default =
          let
            # Slow-moving sandbox-host *toolchain* artefacts pinned by
            # the flake. They land on the shell as VMJAIL_* env vars;
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

              # Tier-2 CO-RE eBPF gadget generation (OBSERVABILITY.md
              # "Tier 2"). `make gen-bpf` runs bpf2go (via `go run`,
              # pinned in tools/tools.go) which shells out to clang to
              # compile each gadget's BPF C to an ELF and llvm-strip to
              # trim it; bpftool dumps the kernel BTF to vmlinux.h for
              # CO-RE. Only needed to *re*generate the committed
              # `*_bpfel.go` + `*.o` objects — the normal Go build and
              # `make check` consume the committed output and need none
              # of this. libbpf ships the `bpf/bpf_helpers.h` the C
              # includes.
              clang
              llvm
              bpftools
              libbpf
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
              # store paths, which went stale the moment a baked-in vmjail
              # binary changed. `make integration` now `nix build`s each
              # one fresh into bin/ and injects the paths for that run; a
              # bare `go test -tags=integration` skips with a hint naming
              # the exact `nix build .#<pkg> -o bin/<pkg>` to run. The
              # production vmjail-sandboxd embeds its artefacts via
              # //go:embed, so it never needed these either. See PLAN.md
              # "Dev-shell purification".

              echo "vmjail dev shell ready (version ${version})"
              echo "  make fmt          - format Go code"
              echo "  make lint         - run golangci-lint"
              echo "  make test         - run tests"
              echo "  make integration  - build initramfs + bundle locally, run e2e"
              echo "  make cover        - update coverage.txt baseline"
              echo "  make hooks        - install git hooks"
            '';
          };

        packages = rec {

          default = vmjail;
          vmjail = vmjail-sandboxd;

          # Portable x86_64 Linux distribution. The sandbox executable is
          # static and embeds its VMM, kernel, initramfs, and
          # virtiofsd, so extracting this archive does not require Nix.
          release-archive = pkgs.runCommand "vmjail-${version}-linux-${system}" {
            nativeBuildInputs = [ pkgs.gnutar pkgs.gzip ];
            meta = with pkgs.lib; {
              description = "Portable local sandbox archive";
              license = licenses.asl20;
              platforms = [ "x86_64-linux" ];
            };
          } ''
            root=vmjail-${version}-linux-x86_64
            mkdir -p "$root/bin" $out
            cp ${vmjail}/bin/vmjail ${vmjail-sandboxd}/bin/vmjail-sandboxd "$root/bin/"
            tar --sort=name --mtime=@1 --owner=0 --group=0 --numeric-owner \
              -czf "$out/$root.tar.gz" "$root"
            (cd "$out" && ${pkgs.coreutils}/bin/sha256sum "$root.tar.gz" \
              > "$root.tar.gz.sha256")
          '';

          nixos-module-check =
            let
              nodeSystem = nixpkgs.lib.nixosSystem {
                inherit system;
                modules = [
                  ./nix/node.nix
                  {
                    system.stateVersion = "25.11";
                    services.vmjail-node = {
                      enable = true;
                      package = vmjail-sandboxd;
                      orchestratorAddress = "https://orchestrator.example:14443";
                    };
                  }
                ];
              };
            in
            pkgs.runCommand "vmjail-nixos-module-check" {
              nodeExecStart = nodeSystem.config.systemd.services.vmjail-node.serviceConfig.ExecStart;
            } ''
              test -n "$nodeExecStart"
              touch $out
            '';

          # u-root-src exposes the pinned u-root submodule source as a
          # buildable package so `make integration` can `nix build` it
          # into bin/ (the raw fetchGit result is not a valid flake
          # package output). build-initramfs runs u-root against this
          # tree. (goSrc + mkVmjailBinary live in the outer `let` — they
          # are a source and a function, not derivations, so they can't
          # be `packages` outputs without failing `nix flake check`.)
          u-root-src = pkgs.runCommand "u-root-src" { } "cp -r ${uRootSrc} $out";

          # vmjail-agent runs inside our minimal microVM rootfs which has
          # no libc, so it must be statically linked. (Default cgoEnabled
          # = false on mkVmjailBinary makes this implicit; kept here only
          # for the platform restriction.)
          vmjail-agent = mkVmjailBinary {
            name = "vmjail-agent";
            subPackage = "cmd/vmjail-agent";
            description = "vmjail in-VM sandbox agent";
            platforms = pkgs.lib.platforms.linux;
          };

          # kconfig-verify is a build-time tool: it asserts that every
          # Kconfig symbol our fragments request survived `olddefconfig`
          # (which silently demotes symbols with unmet deps). Used by the
          # kernel derivation's configurePhase below.
          kconfig-verify = mkVmjailBinary {
            name = "kconfig-verify";
            subPackage = "cmd/kconfig-verify";
            description = "vmjail kernel-config survival checker";
            platforms = pkgs.lib.platforms.linux;
            src = kconfigVerifySrc;
            vendorHash = kconfigVerifyVendorHash;
          };

          # vmjail-hello-container is the workload binary in the v0
          # smoke-test OCI bundle. It runs *inside the container*
          # (which runs inside the sandbox VM), prints
          # agent.ContainerBanner, and exits 0.
          vmjail-hello-container = mkVmjailBinary {
            name = "vmjail-hello-container";
            subPackage = "cmd/vmjail-hello-container";
            description = "vmjail hello-world container workload";
            platforms = pkgs.lib.platforms.linux;
          };

          # vmjail-hello-server is the D6b.4 dev-server proof
          # workload: a static HTTP listener with a known body.
          # Baked into the nix-shell-image so the integration test
          # can `vmjail workspace exec` it and then prove the
          # port-forward path end to end.
          vmjail-hello-server = mkVmjailBinary {
            name = "vmjail-hello-server";
            subPackage = "cmd/vmjail-hello-server";
            description = "vmjail hello-world HTTP server (test workload)";
            platforms = pkgs.lib.platforms.linux;
          };

          vmjail-iso-fixture-init = mkVmjailBinary {
            name = "vmjail-iso-fixture-init";
            subPackage = "./cmd/vmjail-iso-fixture";
            description = "Minimal init and SSH-banner endpoint for the nested-ISO test fixture";
            platforms = pkgs.lib.platforms.linux;
          };

          # vmjail-http-fetch is the D6c.2 slice-4 client workload:
          # a tiny HTTPS GET that prints a known-shape sentinel on
          # success. Baked into the nix-shell-image so the
          # integration test can `vmjail workspace exec` it against
          # an httptest TLS server bound to the host's loopback —
          # reached through sandboxnet's DefaultHostLoopbackIP NAT
          # — and prove the container's network egress works.
          vmjail-http-fetch = mkVmjailBinary {
            name = "vmjail-http-fetch";
            subPackage = "cmd/vmjail-http-fetch";
            description = "vmjail HTTPS GET client (test workload)";
            platforms = pkgs.lib.platforms.linux;
          };

          # nix-shell-image is a minimal Nix-built OCI image used by
          # the D6b.3 integration test. It carries dash + coreutils
          # (so /bin/sh and /bin/sleep resolve inside the container)
          # and an Entrypoint of `sleep infinity` so the container
          # stays alive long enough for `vmjail workspace exec` to
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
            name = "vmjail-nix-shell";
            tag = "v1";
            # vmjail-hello-server is the D6b.4 dev-server workload —
            # exec'd via `vmjail workspace exec` after the container
            # is up so the port-forward test has something to GET
            # against. Its /bin/vmjail-hello-server lands here via
            # the same dockerTools buildEnv symlink mechanism as
            # dash and coreutils. vmjail-http-fetch joins the same
            # buildEnv for the D6c.2 slice-4 HTTPS-egress proof.
            contents = [
              pkgs.dash pkgs.coreutils
              vmjail-hello-server vmjail-http-fetch
            ];
            # The entrypoint prints a readiness sentinel on container
            # startup, then execs sleep so the container stays alive.
            # The sentinel travels container stdout → agent → vsock →
            # sandboxd stdout, where the D6b.3 test waits on it via
            # Cluster.WaitForSandboxdOutput before driving
            # `vmjail workspace exec`. Mirrors the role of
            # agent.ContainerBanner for the hello bundle.
            config = {
              Entrypoint = [
                "/bin/dash" "-c"
                "echo vmjail-nix-shell ready; exec /bin/sleep infinity"
              ];
              Env = [ "PATH=/bin" ];
            };
          };

          # Source-blind campaign toolbox. The campaign pushes this tarball
          # into its disposable orchestrator registry before handing control
          # to the tester, so runtime availability never depends on an
          # external mutable tag. Its deliberately short image command serves
          # both persistent-Workspace and preserve-command lifecycle goals.
          blackbox-toolbox-image = pkgs.dockerTools.buildLayeredImage {
            name = "vmjail-blackbox-toolbox";
            tag = "v1";
            contents = with pkgs; [
              bashInteractive bun cacert coreutils curl dash findutils gcc go
              gnugrep gnused
            ];
            config = {
              Entrypoint = [
                "/bin/dash" "-c"
                "echo vmjail-blackbox-short-command"
              ];
              Env = [
                "PATH=/bin"
                "HOME=/tmp"
                "SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt"
              ];
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
              name = "vmjail-nix-tooling";
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
                # "vmjail-nix-shell ready" pattern so the test can
                # wait on a stable string in sandboxd output.
                Entrypoint = [
                  "/bin/dash" "-c"
                  "echo vmjail-nix-tooling ready; exec /bin/sleep infinity"
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

          # vmjail-check-image is the D6c.4 slice-1 self-build image:
          # a NIX-LESS toolchain image that runs `make check` fully
          # offline. It carries the dev-shell's check toolchain (Go +
          # linters + make + git + a POSIX shell + coreutils) baked
          # under /nix/store as plain files — no nix binary, no daemon,
          # no store DB. The project's Go dependencies ride along as a
          # vendor tree (vmjail.goModules, pinned by the same vendorHash
          # as the vmjail binaries) which the self-build test copies into
          # its source mount at /workspace/vendor, so `go build
          # -mod=vendor` resolves every package without the network.
          # The vendor tree is delivered via the mount rather than baked
          # into the image on purpose: go:embed rejects "irregular"
          # files, and routing a vendored embed (e.g. protobuf's
          # editions_defaults.binpb) through dockerTools layering + the
          # imagestore tar-unpack turns it into a hardlink/symlink,
          # which fails `cannot embed irregular file`. A host `cp -r`
          # over virtiofs keeps every file regular.
          #
          # Why nix-less: the prior self-build ran `nix develop` inside
          # the VM, which substituted the multi-GB dev-shell closure
          # from cache.nixos.org on every cold run — the network-bound
          # stall the Tier-2 io-snoop gadget caught (OBSERVABILITY.md /
          # PLAN.md D6c.4). Baking the closure as image content and
          # vendoring the modules removes BOTH the nix substitution and
          # the go-module fetch: `make check` becomes hermetic and
          # network-free. Binaries keep their /nix/store paths (ELF
          # interpreter + RPATH are non-relocatable), so the store dir
          # exists in the guest — but nothing in it is nix-managed.
          #
          # Scope: `make check` only (fmt + lint + test + coverage).
          # `make dev` / `make integration` `nix build` the VM toolchain
          # and fixture images, so a full `make verify` still needs a
          # real nix — that is a separate hermetic-nix image (the
          # nix-tooling-image pattern extended), not this one.
          vmjail-check-image =
            let
              # Mirrors devShells.default's buildInputs MINUS the eBPF
              # compile chain (clang/llvm/bpftool/libbpf): `make check`
              # consumes the committed *_bpfel.go + *.o and needs none
              # of it (see the devShell note). buf/gzip/cpio are
              # likewise build-time-only (proto / initramfs), not check.
              checkToolchain = pkgs.buildEnv {
                name = "vmjail-check-toolchain";
                paths = with pkgs; [
                  go gofumpt golangci-lint gocyclo goconst scc
                  gnumake git bashInteractive dash coreutils
                  gnugrep gnused gawk findutils diffutils which
                  # internal/testtmux drives a dash pane via tmux in a
                  # few non-integration unit tests run by `make test`.
                  tmux
                ];
              };
              # buildEnv only populates /bin, but the conventional
              # interpreter paths a normal container ships also live
              # under /usr/bin and as /bin/sh. Provide them as shims:
              #   * /bin/sh → dash (make runs recipes via /bin/sh; the
              #     self-build wrapper execs /bin/sh).
              #   * /usr/bin/env → coreutils env. The check scripts
              #     (check-submodule-revs.sh, cover-check.sh) start with
              #     `#!/usr/bin/env bash`; without /usr/bin/env the
              #     kernel's binfmt-script handler can't resolve the
              #     shebang and every script exec fails ENOENT (Error
              #     127), aborting `make check` at the first gate.
              pathShims = pkgs.runCommand "vmjail-check-path-shims" { } ''
                mkdir -p $out/bin $out/usr/bin
                ln -s ${pkgs.dash}/bin/dash $out/bin/sh
                ln -s ${pkgs.coreutils}/bin/env $out/usr/bin/env
              '';
            in pkgs.dockerTools.buildLayeredImage {
              name = "vmjail-check";
              tag = "v1";
              contents = [ checkToolchain pathShims ];
              config = {
                # Entrypoint sentinel mirrors the other fixtures so the
                # test can WaitForSandboxdOutput on a stable string.
                Entrypoint = [
                  "/bin/dash" "-c"
                  "echo vmjail-check ready; exec /bin/sleep infinity"
                ];
                # NOTE: crun-exec does NOT inherit these for `workspace
                # exec` (only the entrypoint sees them), so the
                # self-build wrapper re-exports the build env inline.
                # Kept here for interactive `workspace exec … sh` use.
                Env = [
                  "PATH=/bin"
                  "HOME=/workspace/.home"
                  "USER=root"
                  # No C compiler is baked (the vmjail binaries are all
                  # static CGO=0); CGO_ENABLED=0 keeps golangci-lint's
                  # typecheck off the cgo `net` path, which would else
                  # fail to preprocess C.
                  "CGO_ENABLED=0"
                  "GOTOOLCHAIN=local"
                  "GOFLAGS=-mod=vendor"
                  "GOPROXY=off"
                ];
              };
            };

          # vmjail-build-image is the #101 slice-3 builder image: it runs
          # inside a disposable vmjail sandbox and turns a Dockerfile into an
          # OCI image tarball via buildah (daemonless), which the host then
          # ociregistry.Pushes for the workspace to boot.
          #
          # Isolation is `chroot` (Env BUILDAH_ISOLATION): the default `oci`
          # isolation would have buildah spawn a nested OCI runtime +
          # user/mount namespaces for every RUN, which is fragile inside
          # vmjail's already-nested container-in-a-microVM; chroot runs RUN
          # steps in a chroot and is far likelier to work nested. Storage is
          # `vfs` for the spike (no overlay/fuse deps); the perf path
          # (fuse-overlayfs over the existing privileged-fuse CAP_SYS_ADMIN +
          # /dev/fuse grant) is a follow-up. fuse-overlayfs is bundled now so
          # that switch needs no image change.
          vmjail-build-image =
            let
              buildToolchain = pkgs.buildEnv {
                name = "vmjail-build-toolchain";
                paths = with pkgs; [
                  buildah fuse-overlayfs
                  bashInteractive dash coreutils
                  gnutar gzip
                ];
              };
              containersConf = pkgs.runCommand "vmjail-build-containers-conf"
                {
                  policy = pkgs.writeText "policy.json"
                    ''{"default":[{"type":"insecureAcceptAnything"}]}'';
                  storage = pkgs.writeText "storage.conf" ''
                    [storage]
                    driver = "vfs"
                    graphroot = "/var/lib/containers/storage"
                    runroot = "/run/containers/storage"
                  '';
                  registries = pkgs.writeText "registries.conf" ''
                    unqualified-search-registries = []
                  '';
                } ''
                mkdir -p $out/etc/containers $out/bin $out/var $out/etc/ssl/certs
                ln -s ${pkgs.dash}/bin/dash $out/bin/sh
                # buildah's commit writes layer temp under /var/tmp; the
                # container rootfs is read-only, so point it at the writable
                # tmpfs. (Real builds may want a disk-backed writable mount
                # here instead of RAM — fine for the spike.)
                ln -s /tmp $out/var/tmp
                # A system CA bundle at the path Go's crypto/x509 reads
                # first (/etc/ssl/certs/ca-certificates.crt) so buildah's
                # registry-pull TLS has a trust store. A bare nix image ships
                # none, so without this buildah can't validate ANY registry
                # cert. When vmjail's egress proxy is on, installProxyCA
                # overwrites this file with the proxy CA so the MITM leaf is
                # trusted; off, it's the real CA set (cacert) for direct pulls.
                ln -s ${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt \
                  $out/etc/ssl/certs/ca-certificates.crt
                cp $policy $out/etc/containers/policy.json
                cp $storage $out/etc/containers/storage.conf
                cp $registries $out/etc/containers/registries.conf
                printf 'root:100000:65536\n' > $out/etc/subuid
                printf 'root:100000:65536\n' > $out/etc/subgid
              '';
            in
            pkgs.dockerTools.buildLayeredImage {
              name = "vmjail-build";
              tag = "v1";
              # fakeNss provides /etc/passwd (+ group, nsswitch) with a root
              # uid-0 entry. Without it buildah's user.Current() lookup fails
              # ("error determining current user: unknown userid 0") since a
              # bare nix image ships no passwd — the spike's first finding.
              contents = [ buildToolchain containersConf pkgs.fakeNss ];
              config = {
                Entrypoint = [
                  "/bin/dash" "-c"
                  "echo vmjail-build ready; exec /bin/sleep infinity"
                ];
                Env = [
                  "PATH=/bin"
                  "HOME=/root"
                  "BUILDAH_ISOLATION=chroot"
                  "STORAGE_DRIVER=vfs"
                ];
              };
            };

          # d6c1-flake-fixture is the tiny flake the D6c.1 test
          # mounts at /workspace. Its `serve` app's `program`
          # points at python3 inside the nix-tooling-image's
          # closure via store path, so `nix run /workspace#serve`
          # finds the binary without evaluating any other
          # derivation. The fixture is shipped as a writeTextDir
          # output so the test can `cp $VMJAIL_NIX_FLAKE_FIXTURE/
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
          # path as `vmjail.bundle=...` on the kernel cmdline.
          hello-bundle = pkgs.runCommand "vmjail-hello-bundle" {} ''
            mkdir -p $out/rootfs
            cp ${vmjail-hello-container}/bin/vmjail-hello-container $out/rootfs/hello
            chmod +x $out/rootfs/hello
            cp ${./contrib/hello-bundle/config.json} $out/config.json
          '';

          # vmjail-sandboxd is the single-executable sandbox-host. It
          # embeds cloud-hypervisor, the guest kernel, and the
          # initramfs into the binary at build time, so the resulting
          # ELF needs no external VM artifact files. The CLI still requires
          # host KVM, cgroups and namespaces; systemd-run
          # obtains a dedicated delegated scope when necessary.
          vmjail-sandboxd = pkgs.buildGoModule {
            pname = "vmjail-sandboxd";
            inherit version;
            src = goSrc;
            vendorHash = "sha256-tfCzB6+pJqC0iW/DAh6GtZt8/hlxVghlgxjmr+2tTH8=";
            env.CGO_ENABLED = "0";

            # Stage the real artifacts into internal/bundled/assets/
            # before the Go build, so //go:embed picks them up.
            preBuild = ''
              cp ${cloud-hypervisor}/bin/cloud-hypervisor internal/bundled/assets/cloud-hypervisor
              cp ${kernel}/vmlinux internal/bundled/assets/vmlinux
              cp ${initramfs} internal/bundled/assets/initramfs.cpio
              cp ${virtiofsd}/bin/virtiofsd internal/bundled/assets/virtiofsd
            '';

            ldflags = [
              "-s" "-w"
              "-X github.com/richiejp/vmjail/internal/version.Version=${version}"
            ];
            subPackages = [ "cmd/vmjail" "cmd/vmjail-sandboxd" ];

            meta = with pkgs.lib; {
              description = "Local microVM command runner and sandbox host";
              mainProgram = "vmjail";
              license = licenses.asl20;
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
          #   /bin/uinit ->   /bin/vmjail-agent (symlink, set via -uinitcmd)
          #   /bin/vmjail-agent — our statically-linked agent.
          #   /bin/crun       — the OCI runtime the agent execs to launch
          #                     workload containers; static-musl build.
          #
          # Hardening posture (matches PLAN.md "Hardening"):
          #   -defaultsh=""    no shell symlink
          #   -base=/dev/null  no default /etc, /bin/sh, etc.
          # Only u-root's `init` busybox-binary, our agent, and crun end up
          # in the archive — no interpreters, no diagnostic tools, zero
          # setuid (crun is built without the +s helper bits).
          # Keep cpio uncompressed: decoding gzip added about 42 ms to guest boot.
          initramfs = pkgs.runCommand "vmjail-initramfs.cpio" {
            nativeBuildInputs = [ pkgs.go pkgs.cpio ];
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
              -files "${vmjail-agent}/bin/vmjail-agent:bin/vmjail-agent" \
              -files "${crun}/bin/crun:bin/crun" \
              -uinitcmd=/bin/vmjail-agent \
              ./cmds/core/init

            cp $TMPDIR/initramfs.cpio $out
          '';

          # Linux kernel for the sandbox VM, built from the pinned
          # submodule. Config is assembled from the ground up — tinyconfig
          # + kvm_guest + our microvm/vmjail/hardening fragments (see
          # configurePhase) — not from defconfig. Boot the uncompressed ELF
          # directly through PVH; retain bzImage for the legacy ISO fixture.
          kernel = pkgs.stdenv.mkDerivation {
            pname = "linux-vmjail";
            version = kernelVersion;

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
              # what tinyconfig drops + trim), vmjail features, and KSPP
              # hardening last so it wins. allnoconfig sets CONFIG_EXPERT, which
              # is what lets the fragments turn off otherwise-forced symbols
              # (e.g. io_uring). -m merges without running conf; olddefconfig
              # then resolves dependencies and new-symbol defaults.
              make $kmakeTools tinyconfig
              ./scripts/kconfig/merge_config.sh -m -O . .config \
                kernel/configs/kvm_guest.config \
                ${./third_party/kernel-config/microvm-base.config} \
                ${./third_party/kernel-config/vmjail.config} \
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
                ${./third_party/kernel-config/vmjail.config} \
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
              # Match the kernel's own boot-image stripping: retain loadable
              # code/data, PVH notes and BTF, without DWARF/symbol tables.
              llvm-objcopy -R .comment -S vmlinux $out/vmlinux
              llvm-readelf -n $out/vmlinux | grep 'Xen.*0x00000012' > /dev/null
              llvm-readelf -S $out/vmlinux | grep '\.BTF ' > /dev/null
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
              description = "Linux kernel for the vmjail sandbox VM";
              homepage = "https://kernel.org/";
              license = licenses.gpl2Only;
              maintainers = [ ];
              platforms = [ "x86_64-linux" ];
            };
          };

          # Cloud Hypervisor — our v1 VMM, built from our pinned
          # submodule, statically linked against musl so the resulting
          # ELF runs on any Linux distro of the same arch. We bundle it
          # into the sandbox-host single executable (vmjail-sandboxd), so
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
          # binary that drops into the vmjail-sandboxd bundle alongside
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
          # vmjail-agent-as-PID-1.
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

          # iso-qemu-runner-image carries qemu + a launcher that boots a
          # third-party ISO as a NESTED L2 VM: QEMU -accel kvm against
          # the vmjail sandbox's bound /dev/kvm, with slirp user-net
          # hostfwd exposing the ISO's sshd on the container loopback.
          # vmjail's existing port-forward then reaches it from the host
          # (TestClusterNestedISOBoot). Part of the iso-nested-qemu
          # diversion: the ISO is the only untrusted layer, wrapped from
          # outside by the normal vmjail VM (vsock, cgroups, /proc
          # sampler) — vmjail never has to learn to boot ISOs itself.
          iso-qemu-runner-image =
            let
              # qemu_test is the headless "for-vm-tests" build: same
              # KVM + virtio + slirp user-net + x86_64-softmmu we need,
              # but compiled WITHOUT the gtk/sdl/spice/pulse/gstreamer
              # desktop+audio stack qemu_kvm drags in — which a
              # -display none launcher never touches. Cuts the image
              # from ~450 MB to a fraction.
              qemuRunner = pkgs.qemu_test;
              # launch-iso boots the ISO under qemu and blocks until its
              # sshd answers SSH-2.0 on the slirp-forwarded loopback
              # port, then prints VMJAIL_ISO_SSH_READY (the sentinel the
              # test waits on) and stays in the foreground so the
              # host-side forward + SSH assertions have a live target.
              # writeShellScriptBin (not writeShellApplication) on
              # purpose: the readiness probe relies on commands that fail
              # by design, so `set -e` must stay off.
              launchIso = pkgs.writeShellScriptBin "launch-iso" ''
                export PATH=${pkgs.coreutils}/bin:${qemuRunner}/bin:$PATH
                ISO="''${1:?usage: launch-iso <iso-path> [hostport]}"
                HOSTPORT="''${2:-2222}"
                SER="''${QEMU_SERIAL_LOG:-/work/.tmp/qemu-serial.log}"
                mkdir -p "$(dirname "$SER")"
                : > "$SER"

                qemu-system-x86_64 \
                  -accel kvm -cpu host \
                  -m "''${QEMU_MEM_MB:-1536}" -smp "''${QEMU_SMP:-2}" \
                  -cdrom "$ISO" \
                  -netdev "user,id=n,hostfwd=tcp:127.0.0.1:''${HOSTPORT}-:22" \
                  -device virtio-net-pci,netdev=n \
                  -display none -serial "file:$SER" &
                QPID=$!
                echo "vmjail-iso-qemu: launched qemu pid=$QPID iso=$ISO hostfwd=127.0.0.1:''${HOSTPORT}->:22"
                trap 'kill -TERM "$QPID" 2>/dev/null' TERM INT

                # Slirp accepts the host-forward connection before the guest
                # sshd exists, then completes that same connection when the
                # guest starts listening. Keep one bounded connection: rapid
                # reconnects leave stale slirp forwards and delay readiness.
                banner=$(timeout "''${QEMU_READY_SECONDS:-180}" bash -c '
                  until exec 3<>"/dev/tcp/127.0.0.1/$1"; do sleep 0.1; done
                  head -c 40 <&3
                ' _ "''${HOSTPORT}" 2>/dev/null)
                case "$banner" in
                  SSH-2.0*)
                    echo "vmjail-iso-qemu: sshd reachable: ''${banner%%$'\r'*}"
                    echo "VMJAIL_ISO_SSH_READY"
                    ;;
                  *)
                  echo "vmjail-iso-qemu: FATAL sshd not reachable within timeout"
                  echo "--- serial tail ---"; tail -n 40 "$SER" 2>/dev/null || true
                  kill -9 "$QPID" 2>/dev/null
                  exit 1
                    ;;
                esac

                wait "$QPID"
              '';
            in pkgs.dockerTools.buildLayeredImage {
              name = "vmjail-iso-qemu-runner";
              tag = "v1";
              contents = [
                pkgs.bashInteractive pkgs.coreutils pkgs.dash
                launchIso qemuRunner
              ];
              config = {
                Entrypoint = [
                  "${pkgs.bashInteractive}/bin/bash" "-c"
                  "echo vmjail-iso-qemu ready; exec ${pkgs.coreutils}/bin/sleep infinity"
                ];
                Env = [ "PATH=/bin" "HOME=/work" ];
              };
            };

          # iso-ssh-fixture is the slice-1 proof payload for the
          # ISO-nested-QEMU diversion (branch iso-nested-qemu): a
          # deliberately tiny bootable Linux ISO that exposes an SSH banner.
          # It stands in for a third-party "Linux + agent" ISO we
          # cannot rebuild — the integration test boots it under QEMU
          # *inside* a vmjail sandbox VM (nested KVM), forwards its sshd
          # out, and asserts the SSH banner is reachable end-to-end.
          # Pointing the mechanism at a real ISO is a one-line --source
          # swap; this fixture exists only so the chain is CI-provable.
          #
          # The fixture reuses vmjail's small kernel and a u-root initramfs. Its
          # only userspace payload configures eth0 at QEMU slirp's conventional
          # guest address and emits an SSH-2.0 banner on port 22.
          iso-fixture-initramfs = pkgs.runCommand "vmjail-iso-fixture-initramfs.cpio" {
            nativeBuildInputs = [ pkgs.go pkgs.cpio ];
          } ''
            export HOME=$TMPDIR
            export GOCACHE=$TMPDIR/go-cache
            export GOPATH=$TMPDIR/go
            export GOROOT=${pkgs.go}/share/go
            cp -r --no-preserve=mode ${uRootSrc} u-root-src
            cd u-root-src
            ${u-root}/bin/u-root \
              -o $TMPDIR/initramfs.cpio \
              -defaultsh="" \
              -base=/dev/null \
              -files "${vmjail-iso-fixture-init}/bin/vmjail-iso-fixture:bin/vmjail-iso-fixture-init" \
              -uinitcmd=/bin/vmjail-iso-fixture-init \
              ./cmds/core/init
            cp $TMPDIR/initramfs.cpio $out
          '';

          iso-ssh-fixture =
            let
              isolinuxCfg = pkgs.writeText "isolinux.cfg" ''
                DEFAULT vmjail
                PROMPT 0
                TIMEOUT 1
                LABEL vmjail
                  KERNEL /bzImage
                  INITRD /initramfs.cpio
                  APPEND console=ttyS0,115200n8 panic=-1
              '';
            in pkgs.runCommand "vmjail-iso-ssh-fixture" {
              nativeBuildInputs = [ pkgs.xorriso ];
            } ''
              mkdir -p $out/iso image/isolinux
              cp ${kernel}/bzImage image/bzImage
              cp ${iso-fixture-initramfs} image/initramfs.cpio
              cp ${pkgs.syslinux}/share/syslinux/isolinux.bin image/isolinux/
              cp ${pkgs.syslinux}/share/syslinux/ldlinux.c32 image/isolinux/
              cp ${isolinuxCfg} image/isolinux/isolinux.cfg
              xorriso -as mkisofs \
                -o $out/iso/vmjail-iso-ssh-fixture.iso \
                -b isolinux/isolinux.bin \
                -c isolinux/boot.cat \
                -no-emul-boot -boot-load-size 4 -boot-info-table \
                image
            '';
        };
      })) // {
        nixosModules = {
          node = import ./nix/node.nix;
          default = self.nixosModules.node;
        };
      };
}
