package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/omniswitch-dev/omniswitch/internal/gatewayconfig"
	"github.com/spf13/cobra"
)

// cliAction is what main should do once command-line arguments have been
// dispatched.
type cliAction int

const (
	// actionServe starts the gateway server.
	actionServe cliAction = iota
	// actionExit means the command already produced its output; main should
	// exit with the returned status code without starting a server.
	actionExit
)

const helpText = `OmniSwitch AI Gateway

Usage:
  omniswitch [command]

Commands:
  serve             Start the gateway server (default when no command is given)
  validate-config   Validate a gateway config file and exit (defaults to $OMNISWITCH_CONFIG)
  migrate           Inspect or apply schema migrations (status, up [--dry-run])
  version           Print version information
  help              Show this help text

Flags:
  -h, --help        Show this help text
  -v, --version     Print version information

Environment:
  OPENAI_API_KEY, ANTHROPIC_API_KEY, and friends configure providers.
  See docs/CONFIGURATION.md and docs/CLI.md for the full reference.
`

// dispatch interprets os.Args[1:] and returns what main should do next. It
// never starts the server or binds a port itself, so --help and --version
// are safe to run even when the configured listen address is already in
// use. stdout/stderr are injected for testability.
func dispatch(args []string, stdout, stderr io.Writer, env func(string) string) (cliAction, int) {
	if len(args) == 0 {
		return actionServe, 0
	}

	switch args[0] {
	case "serve":
		if len(args) > 1 {
			fmt.Fprintf(stderr, "omniswitch serve: unexpected argument %q\n", args[1])
			return actionExit, 2
		}
		return actionServe, 0
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, versionString())
		return actionExit, 0
	case "help", "--help", "-h":
		fmt.Fprint(stdout, helpText)
		return actionExit, 0
	case "migrate":
		return actionExit, runMigrateCLI(args[1:], stdout, stderr)
	case "validate-config":
		path := ""
		if len(args) > 1 {
			path = args[1]
		} else if env != nil {
			path = env("OMNISWITCH_CONFIG")
		}
		path = strings.TrimSpace(path)
		if path == "" {
			fmt.Fprintln(stderr, "omniswitch validate-config: a config path is required (pass one, or set OMNISWITCH_CONFIG)")
			return actionExit, 2
		}
		if _, err := gatewayconfig.LoadFile(path); err != nil {
			fmt.Fprintf(stderr, "invalid config %s: %v\n", path, err)
			return actionExit, 1
		}
		fmt.Fprintf(stdout, "OK: %s\n", path)
		return actionExit, 0
	default:
		fmt.Fprintf(stderr, "omniswitch: unknown command %q\n\n", args[0])
		fmt.Fprint(stderr, helpText)
		return actionExit, 1
	}
}

func versionString() string {
	return fmt.Sprintf("omniswitch version %s (commit %s, built %s)", version, commit, date)
}

func runMigrateCLI(args []string, stdout, stderr io.Writer) int {
	// Preserve the existing top-level dispatch while using Cobra for the nested
	// migration commands, argument validation, help, and --dry-run flag.
	root := &cobra.Command{Use: "omniswitch", SilenceUsage: true, SilenceErrors: true}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.CompletionOptions.DisableDefaultCmd = true
	migrate := &cobra.Command{
		Use: "migrate", Short: "Inspect and apply schema migrations", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	status := &cobra.Command{
		Use: "status", Short: "Print schema versions without changing the schema", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runMigrationCommand(cmd, false) },
	}
	var dryRun bool
	up := &cobra.Command{
		Use: "up", Short: "Apply pending migrations, or list them with --dry-run", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runMigrationCommand(cmd, dryRun) },
	}
	up.Flags().BoolVar(&dryRun, "dry-run", false, "List pending migrations without applying them")
	migrate.AddCommand(status, up)
	root.AddCommand(migrate)
	root.SetArgs(append([]string{"migrate"}, args...))
	if err := root.ExecuteContext(context.Background()); err != nil {
		fmt.Fprintf(stderr, "omniswitch migrate: %v\n", err)
		return 1
	}
	return 0
}

func runMigrationCommand(cmd *cobra.Command, dryRun bool) error {
	settings, err := loadRuntimeSettings()
	if err != nil {
		return fmt.Errorf("load gateway settings: %w", err)
	}
	st, err := openConfiguredStore(settings, false)
	if err != nil {
		return fmt.Errorf("open migration store: %w", err)
	}
	defer st.Close()
	status, err := st.SchemaStatus(cmd.Context())
	if err != nil {
		return err
	}
	if cmd.Name() == "status" {
		fmt.Fprintf(cmd.OutOrStdout(), "Driver: %s\nCurrent version: %d\nLatest version: %d\nPending migrations: %d\n", st.Driver(), status.Current, status.Latest, status.Pending)
		return nil
	}
	pending, err := st.PendingMigrations(cmd.Context())
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		fmt.Fprintf(cmd.OutOrStdout(), "No pending migrations (driver %s, version %d).\n", st.Driver(), status.Current)
		return nil
	}
	if dryRun {
		fmt.Fprintf(cmd.OutOrStdout(), "Pending migrations (%s):\n", st.Driver())
		for _, migration := range pending {
			fmt.Fprintf(cmd.OutOrStdout(), "  %d  %s\n", migration.Version, migration.Name)
		}
		return nil
	}
	if err := st.Migrate(cmd.Context()); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	status, err = st.SchemaStatus(cmd.Context())
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Schema is up to date (driver %s, version %d, pending %d).\n", st.Driver(), status.Current, status.Pending)
	return nil
}
