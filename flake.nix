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
        devShells.default = pkgs.mkShell {
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
          ];

          shellHook = ''
            echo "darkaifactory dev shell ready (version ${version})"
            echo "  make fmt    - format Go code"
            echo "  make lint   - run golangci-lint"
            echo "  make test   - run tests"
            echo "  make cover  - update coverage.txt baseline"
            echo "  make hooks  - install git hooks"
          '';
        };

        packages = rec {
          default = daft;

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
            src = ./.;
            vendorHash = "sha256-pprnK2JKmPuR3Q+F8+vMDEdowlb3oX4BOOzW8NGOqgs=";
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

          # daft-sandboxd is the single-executable sandbox-host. It
          # embeds cloud-hypervisor, the guest kernel, and the
          # initramfs into the binary at build time, so the resulting
          # ELF runs on any Linux distro of the same arch with no
          # external runtime dependencies.
          daft-sandboxd = pkgs.buildGoModule {
            pname = "daft-sandboxd";
            inherit version;
            src = ./.;
            vendorHash = "sha256-pprnK2JKmPuR3Q+F8+vMDEdowlb3oX4BOOzW8NGOqgs=";
            env.CGO_ENABLED = "0";

            # Stage the real artifacts into internal/bundled/assets/
            # before the Go build, so //go:embed picks them up.
            preBuild = ''
              cp ${cloud-hypervisor}/bin/cloud-hypervisor internal/bundled/assets/cloud-hypervisor
              cp ${kernel}/bzImage internal/bundled/assets/bzImage
              cp ${initramfs} internal/bundled/assets/initramfs.cpio.gz
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
          #
          # Hardening posture (matches PLAN.md "Hardening"):
          #   -defaultsh=""    no shell symlink
          #   -base=/dev/null  no default /etc, /bin/sh, etc.
          # Only u-root's `init` busybox-binary and our agent end up in the
          # archive — no interpreters, no diagnostic tools, zero setuid.
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
            # its own module, so run from the u-root checkout.
            cp -r ${./third_party/u-root} u-root-src
            chmod -R u+w u-root-src
            cd u-root-src

            ${u-root}/bin/u-root \
              -o $TMPDIR/initramfs.cpio \
              -defaultsh="" \
              -base=/dev/null \
              -files "${daft-agent}/bin/daft-agent:bin/daft-agent" \
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
