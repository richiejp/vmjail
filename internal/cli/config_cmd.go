package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// configScope binds a get/set command pair to one config file:
// where it lives (pathFn, evaluated at run time so flags are already
// parsed) and which keys it accepts.
type configScope struct {
	name    string
	allowed []string
	pathFn  func() (string, error)
}

// newGlobalsCmd builds `daft globals`, the per-user config scope.
func newGlobalsCmd() *cobra.Command {
	var configDir string
	cmd := &cobra.Command{
		Use:   "globals",
		Short: "Get or set per-user default settings",
		Long: "Per-user defaults stored in $XDG_CONFIG_HOME/daft/client/config.json.\n" +
			"They apply to every command unless overridden by a project-local\n" +
			"./.daft/config.json (see `daft workspace set`), the " + orchestratorEnvVar + "\n" +
			"env var, or an explicit flag.\n\n" +
			"Settable keys: " + strings.Join(globalConfigKeys(), ", "),
	}
	cmd.PersistentFlags().StringVar(&configDir, configDirFlag, "",
		"override the daft client config dir (default: $XDG_CONFIG_HOME/daft/client)")
	scope := configScope{
		name:    "globals",
		allowed: globalConfigKeys(),
		pathFn:  func() (string, error) { return globalConfigPath(configDir) },
	}
	cmd.AddCommand(
		newConfigGetCmd(scope),
		newConfigSetCmd(scope),
		newConfigResolveCmd(&configDir),
	)
	return cmd
}

// newConfigResolveCmd prints the *effective* config — the value each
// command would actually use — and the layer it came from, walking
// the full precedence chain rather than a single scope's file.
func newConfigResolveCmd(configDir *string) *cobra.Command {
	return &cobra.Command{
		Use:           "resolve",
		Short:         "Print the effective orchestrator + workspace name and where each came from",
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			orch, orchSrc, err := resolveOrchestratorSource("", *configDir)
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "orchestrator = %s (source: %s)\n", orch, orchSrc)
			name, nameSrc, err := resolveWorkspaceNameSource()
			if err != nil {
				// A name failure (e.g. cwd is "/") is non-fatal for
				// resolve: still report the orchestrator we found.
				_, _ = fmt.Fprintf(out, "name         = <unset> (%v)\n", err)
				return nil
			}
			_, _ = fmt.Fprintf(out, "name         = %s (source: %s)\n", name, nameSrc)
			return nil
		},
	}
}

// newWorkspaceConfigCmds builds the dir-local `get`/`set` pair hung
// under `daft workspace`. They edit ./.daft/config.json, which holds
// per-project defaults (orchestrator URL, workspace name).
func newWorkspaceConfigCmds() []*cobra.Command {
	scope := configScope{
		name:    workspaceCmdName,
		allowed: dirLocalConfigKeys(),
		pathFn:  dirLocalConfigPath,
	}
	return []*cobra.Command{newConfigGetCmd(scope), newConfigSetCmd(scope)}
}

// newConfigGetCmd prints one value, or every key when called bare.
func newConfigGetCmd(scope configScope) *cobra.Command {
	return &cobra.Command{
		Use:           "get [key]",
		Short:         "Print a " + scope.name + " config value (bare: print all)",
		Args:          cobra.MaximumNArgs(1),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := scope.pathFn()
			if err != nil {
				return err
			}
			c, err := loadFileConfig(path)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(args) == 0 {
				for _, k := range scope.allowed {
					v, _ := c.get(k)
					_, _ = fmt.Fprintf(out, "%s = %s\n", k, v)
				}
				return nil
			}
			key := args[0]
			if !knownConfigKey(scope.allowed, key) {
				return unknownKeyErr(scope, "get", key)
			}
			v, _ := c.get(key)
			_, _ = fmt.Fprintln(out, v)
			return nil
		},
	}
}

// newConfigSetCmd writes one key into the scope's config file.
func newConfigSetCmd(scope configScope) *cobra.Command {
	return &cobra.Command{
		Use:           "set <key> <value>",
		Short:         "Set a " + scope.name + " config value",
		Args:          cobra.ExactArgs(2),
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			key, value := args[0], args[1]
			if !knownConfigKey(scope.allowed, key) {
				return unknownKeyErr(scope, "set", key)
			}
			if err := validateConfigValue(key, value); err != nil {
				return fmt.Errorf("daft %s set: %w", scope.name, err)
			}
			path, err := scope.pathFn()
			if err != nil {
				return err
			}
			c, err := loadFileConfig(path)
			if err != nil {
				return err
			}
			c.set(key, value)
			if err := saveFileConfig(path, c); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s = %s (%s)\n", key, value, path)
			return nil
		},
	}
}

func unknownKeyErr(scope configScope, verb, key string) error {
	return fmt.Errorf("daft %s %s: unknown key %q (valid: %s)",
		scope.name, verb, key, strings.Join(scope.allowed, ", "))
}
