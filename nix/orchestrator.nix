{
  config,
  lib,
  ...
}:

let
  cfg = config.services.darkaifactory-orchestrator;
in
{
  options.services.darkaifactory-orchestrator = {
    enable = lib.mkEnableOption "the darkaifactory orchestrator";

    package = lib.mkOption {
      type = lib.types.package;
      description = "darkaifactory package containing daft-orchestrator.";
    };

    user = lib.mkOption {
      type = lib.types.str;
      description = "User that owns the orchestrator state and process.";
    };

    controlAddress = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1:14443";
      description = "mTLS control-plane listen address.";
    };

    webAddress = lib.mkOption {
      type = lib.types.str;
      description = "Private read-only dashboard listen address.";
    };

    configDir = lib.mkOption {
      type = lib.types.str;
      description = "Persistent orchestrator identity, peer, and catalogue directory.";
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.services.darkaifactory-orchestrator = {
      description = "darkaifactory orchestrator";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      serviceConfig = {
        ExecStart = lib.escapeShellArgs [
          "${cfg.package}/bin/daft-orchestrator"
          "serve"
          "--addr"
          cfg.controlAddress
          "--web-addr"
          cfg.webAddress
          "--config-dir"
          cfg.configDir
        ];
        User = cfg.user;
        Restart = "on-failure";
        RestartSec = "2s";
        NoNewPrivileges = true;
        PrivateTmp = true;
        ProtectSystem = "strict";
        ProtectHome = "read-only";
        ReadWritePaths = [ cfg.configDir ];
        RestrictAddressFamilies = [
          "AF_INET"
          "AF_INET6"
        ];
      };
    };

    environment.systemPackages = [ cfg.package ];
  };
}
