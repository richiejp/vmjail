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
