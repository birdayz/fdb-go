// Package cmd builds the fdb command tree.
package cmd

import (
	"github.com/spf13/cobra"
)

// NewRoot builds the top-level fdb command and registers shell completions.
func NewRoot() *cobra.Command {
	v := readVersion()

	root := &cobra.Command{
		Use:   "fdb",
		Short: "fdb.dev CLI for FoundationDB, Record Layer, and SQL",
		Long: "The fdb.dev CLI manages local FoundationDB development clusters, runs SQL, " +
			"and inspects and manages Record Layer stores.\n\n" +
			"Record Layer commands live under `fdb rl`. SQL, transaction " +
			"utilities, and local development clusters have separate commands.\n\n" +
			"Config lives at ~/.fdb/config.yaml (override via $FDB_CONFIG).\n" +
			"CLI documentation: https://fdb.dev/docs/cli/",
		Example: `  fdb dev up
  fdb sql --database /FRL/myapp --schema main
  fdb rl store info
  fdb rl record scan --type Order --limit 10
  fdb rl index ls -o json | jq -r '.[].name'`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       v.Version,
	}
	// Keep --version and the version subcommand identical, including build info.
	root.SetVersionTemplate("fdb " + v.Version +
		" (" + v.GoVersion + " " + v.GOOS + "/" + v.GOARCH + ")\n")

	root.AddCommand(
		newVersionCmd(),
		newConfigCmd(),
		newRLCmd(),
		newTxCmd(),
		newSQLCmd(),
		newDevCmd(),
	)

	registerCompletions(root)
	return root
}

func newRLCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "rl",
		Short: "Inspect and manage Record Layer stores",
		Example: `  fdb rl store info
  fdb rl record scan --type Order --limit 10
  fdb rl index ls
  fdb rl meta get
  fdb rl status`,
	}
	c.AddCommand(
		newStoreCmd(),
		newMetaCmd(),
		newRecordCmd(),
		newIndexCmd(),
		newKeyspaceCmd(),
		newStatusCmd(),
		newStatsCmd(),
	)
	return c
}

// registerCompletions walks the command tree depth-first and wires up
// context-name / record-type / output-format completions for any command
// that has the matching flag. Commands declaring these flags don't need
// to touch cobra's completion API themselves; commands whose --output
// accepts YAML instead of text flag themselves via AnnotationOutputYAML.
func registerCompletions(c *cobra.Command) {
	registerContextCompletion(c)
	registerRecordTypeCompletion(c)
	registerFormatCompletion(c)
	for _, child := range c.Commands() {
		registerCompletions(child)
	}
}
