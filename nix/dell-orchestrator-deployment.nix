{
  pkgs,
  ...
}:

let
  source = builtins.getFlake "path:/home/rich/go/src/github.com/localai-org/darkaifactory";
in
{
  imports = [
    /etc/nixos/configuration.nix
    ./orchestrator.nix
  ];

  services.darkaifactory-orchestrator = {
    enable = true;
    package = source.packages.${pkgs.system}.daft-orchestrator;
    controlAddress = "127.0.0.1:14443";
    webAddress = "100.124.47.5:14444";
    configDir = "/home/rich/.local/state/daft/cluster/orch";
  };

  networking.firewall.interfaces.tailscale0.allowedTCPPorts = [ 14444 ];
}
