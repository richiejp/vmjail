{
  description = "darkaifactory - daft CLI";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";
    flake-utils.url = "github:numtide/flake-utils";
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

        packages.default = pkgs.buildGoModule {
          pname = "daft";
          inherit version;

          src = ./.;

          # vendorHash will need updating once go.mod has dependencies. Until
          # then, the project has no external deps so vendoring is trivial.
          vendorHash = null;

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
      });
}
