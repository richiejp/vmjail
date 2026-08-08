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

    dashboardPasswordHashFile = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Runtime path to a bcrypt dashboard password hash; kept out of the Nix store via a systemd credential.";
    };

    webAuthnRPID = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "WebAuthn relying-party domain; null disables security-key login.";
    };

    webAuthnOrigins = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "Exact public HTTPS origins permitted for WebAuthn ceremonies.";
    };

    configDir = lib.mkOption {
      type = lib.types.str;
      description = "Persistent orchestrator identity, peer, and catalogue directory.";
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.webAuthnRPID == null || cfg.webAuthnOrigins != [ ];
        message = "services.darkaifactory-orchestrator.webAuthnOrigins must be set when webAuthnRPID is enabled";
      }
    ];

    systemd.services.darkaifactory-orchestrator = {
      description = "darkaifactory orchestrator";
      wantedBy = [ "multi-user.target" ];
      wants = [ "network-online.target" ];
      after = [ "network-online.target" ];
      serviceConfig = {
        ExecStart = lib.escapeShellArgs (
          [
            "${cfg.package}/bin/daft-orchestrator"
            "serve"
            "--addr"
            cfg.controlAddress
            "--web-addr"
            cfg.webAddress
            "--config-dir"
            cfg.configDir
          ]
          ++ lib.optionals (cfg.dashboardPasswordHashFile != null) [
            "--web-password-file"
            "/run/credentials/darkaifactory-orchestrator.service/dashboard-password"
          ]
          ++ lib.optionals (cfg.webAuthnRPID != null) (
            [
              "--web-authn-rp-id"
              cfg.webAuthnRPID
            ]
            ++ lib.concatMap (origin: [
              "--web-authn-origin"
              origin
            ]) cfg.webAuthnOrigins
          )
        );
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
      } // lib.optionalAttrs (cfg.dashboardPasswordHashFile != null) {
        LoadCredential = "dashboard-password:${cfg.dashboardPasswordHashFile}";
      };
    };

    environment.systemPackages = [ cfg.package ];
  };
}
