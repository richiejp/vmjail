{
  config,
  lib,
  ...
}:

let
  cfg = config.services.vmjail-node;
in
{
  options.services.vmjail-node = {
    enable = lib.mkEnableOption "a vmjail sandbox node";

    package = lib.mkOption {
      type = lib.types.package;
      description = "vmjail package containing vmjail-sandboxd.";
    };

    orchestratorAddress = lib.mkOption {
      type = lib.types.str;
      description = "HTTPS address of the vmjail orchestrator.";
      example = "https://orchestrator.example:14443";
    };

    user = lib.mkOption {
      type = lib.types.str;
      default = "vmjail-node";
      description = "System user that owns the node state and process.";
    };

    group = lib.mkOption {
      type = lib.types.str;
      default = "vmjail-node";
      description = "System group for the node service.";
    };

    createUser = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Create the configured system user and group.";
    };

    stateDirectory = lib.mkOption {
      type = lib.types.str;
      default = "vmjail-node";
      description = "StateDirectory name below /var/lib.";
    };

    cacheDirectory = lib.mkOption {
      type = lib.types.str;
      default = "vmjail-node";
      description = "CacheDirectory name below /var/cache.";
    };

    runtimeDirectory = lib.mkOption {
      type = lib.types.str;
      default = "vmjail-node";
      description = "RuntimeDirectory name below /run.";
    };

    networkServices = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ "network-online.target" ];
      description = "Units that must be active before the node starts.";
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = !lib.hasPrefix "/" cfg.stateDirectory;
        message = "services.vmjail-node.stateDirectory must be a systemd directory name, not an absolute path";
      }
      {
        assertion = !lib.hasPrefix "/" cfg.cacheDirectory;
        message = "services.vmjail-node.cacheDirectory must be a systemd directory name, not an absolute path";
      }
      {
        assertion = !lib.hasPrefix "/" cfg.runtimeDirectory;
        message = "services.vmjail-node.runtimeDirectory must be a systemd directory name, not an absolute path";
      }
    ];

    users.groups = lib.mkIf cfg.createUser {
      ${cfg.group} = { };
    };
    users.users = lib.mkIf cfg.createUser {
      ${cfg.user} = {
        isSystemUser = true;
        group = cfg.group;
        extraGroups = [ "kvm" ];
        autoSubUidGidRange = true;
        description = "vmjail sandbox node service account";
      };
    };

    systemd.services.vmjail-node = {
      description = "vmjail sandbox node";
      wantedBy = [ "multi-user.target" ];
      wants = cfg.networkServices;
      after = cfg.networkServices;
      environment = {
        VMJAIL_STATE_DIR = "/var/lib/${cfg.stateDirectory}";
        VMJAIL_STORE_DIR = "/var/lib/${cfg.stateDirectory}/images";
        XDG_CACHE_HOME = "/var/cache/${cfg.cacheDirectory}";
      };
      serviceConfig = {
        ExecStart = lib.escapeShellArgs [
          "${cfg.package}/bin/vmjail-sandboxd"
          "serve"
          "--orchestrator"
          cfg.orchestratorAddress
          "--config-dir"
          "/var/lib/${cfg.stateDirectory}"
          "--vsock-dir"
          "/run/${cfg.runtimeDirectory}/vsock"
        ];
        User = cfg.user;
        Group = cfg.group;
        SupplementaryGroups = [ "kvm" ];
        StateDirectory = cfg.stateDirectory;
        StateDirectoryMode = "0700";
        CacheDirectory = cfg.cacheDirectory;
        CacheDirectoryMode = "0700";
        RuntimeDirectory = cfg.runtimeDirectory;
        RuntimeDirectoryMode = "0700";
        Delegate = true;
        DeviceAllow = [ "/dev/kvm rw" ];
        Restart = "on-failure";
        RestartSec = "2s";
        PrivateTmp = true;
        ProtectHome = true;
        ProtectSystem = "strict";
        RestrictAddressFamilies = [
          "AF_INET"
          "AF_INET6"
          "AF_UNIX"
        ];
        ReadWritePaths = [ "/var/lib/${cfg.stateDirectory}" ];
      };
    };

    environment.systemPackages = [ cfg.package ];
  };
}
