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

          daft = pkgs.buildGoModule {
            pname = "daft";
            inherit version;

            src = ./.;

            # vendorHash pins the Go module dependency closure for
            # reproducible builds. Update with `nix build` when go.mod
            # changes — Nix prints the new hash in the error.
            vendorHash = "sha256-pprnK2JKmPuR3Q+F8+vMDEdowlb3oX4BOOzW8NGOqgs=";

            # Embed the version string into the binary at build time.
            ldflags = [
              "-s" "-w"
              "-X github.com/richiejp/darkaifactory/internal/version.Version=${version}"
            ];

            subPackages = [ "cmd/daft" ];

            meta = with pkgs.lib; {
              description = "darkaifactory daft CLI";
              license = licenses.mit;
              maintainers = [ ];
              platforms = platforms.unix;
            };
          };

          # daft-agent is the in-VM agent. It must run inside our minimal
          # microVM rootfs which has no libc, so build statically with
          # CGO_ENABLED=0. Pure Go has no runtime libc dependency.
          daft-agent = pkgs.buildGoModule {
            pname = "daft-agent";
            inherit version;

            src = ./.;
            vendorHash = "sha256-pprnK2JKmPuR3Q+F8+vMDEdowlb3oX4BOOzW8NGOqgs=";

            env.CGO_ENABLED = "0";

            ldflags = [
              "-s" "-w"
              "-X github.com/richiejp/darkaifactory/internal/version.Version=${version}"
            ];

            subPackages = [ "cmd/daft-agent" ];

            meta = with pkgs.lib; {
              description = "darkaifactory in-VM sandbox agent";
              license = licenses.mit;
              maintainers = [ ];
              platforms = platforms.linux;
            };
          };

          # Initramfs cpio for the sandbox VM. First-pass minimum:
          # daft-agent acts as /init directly. PLAN.md's longer-term
          # plan is u-root's `cmds/core/init` with daft-agent as
          # -uinitcmd; that lands in a follow-up slice. The current
          # arrangement is enough to validate the boot path end-to-end:
          # kernel decompresses, jumps to /init, which is daft-agent,
          # which prints its banner to the serial console.
          initramfs = pkgs.runCommand "daft-initramfs.cpio.gz" {
            nativeBuildInputs = with pkgs; [ cpio gzip ];
          } ''
            mkdir -p root
            cp ${daft-agent}/bin/daft-agent root/init
            chmod +x root/init
            (cd root && find . -print0 | cpio --null --create --format=newc) \
              | gzip -9 > $out
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

          # Cloud Hypervisor — our v1 VMM, built from our pinned submodule
          # rather than nixpkgs. Static-musl is the eventual goal per
          # PLAN.md; for the first pass we accept the dynamic build that
          # cargo+rustc produce by default and tighten in a later slice.
          cloud-hypervisor = pkgs.rustPlatform.buildRustPackage {
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

            nativeBuildInputs = with pkgs; [ pkg-config ];

            meta = with pkgs.lib; {
              description = "Cloud Hypervisor microVM monitor";
              homepage = "https://www.cloudhypervisor.org/";
              license = with licenses; [ asl20 bsd3 ];
              maintainers = [ ];
              platforms = [ "x86_64-linux" "aarch64-linux" ];
            };
          };

          # u-root command, built from our pinned submodule rather than
          # nixpkgs. We use it in a later slice to assemble the initramfs
          # cpio that boots the sandbox VM. The vendorHash pins the Go
          # module dependency closure for reproducible builds.
          u-root = pkgs.buildGoModule {
            pname = "u-root";
            version = "0.16.0";

            src = ./third_party/u-root;

            # u-root checks in its vendored dependencies, so we don't need
            # Nix to pre-fetch a module cache for it.
            vendorHash = null;

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
