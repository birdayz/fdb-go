// Package cmd houses the `frl` cobra command tree. Phase A ships only the
// root command and a `version` subcommand so the skeleton builds end-to-end
// before the real NOUN VERB hierarchy is designed.
package cmd

import (
	"github.com/spf13/cobra"
)

// NewRoot builds the top-level `frl` command. Every subcommand should be
// attached here (or onto a child command) so a single call to fang.Execute
// in main covers the entire tree.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "frl",
		Short: "Operator CLI for the Go FoundationDB Record Layer",
		Long: "frl is a kaf-style CLI for inspecting record stores backed " +
			"by the Go port of the FoundationDB Record Layer. Read-only in " +
			"v1 — record/index/meta/store introspection plus config and " +
			"escape hatches (tx, keyspace). Writes are deferred to a later " +
			"wave with confirmation + dry-run defaults.\n\n" +
			"Config lives at ~/.frl/config.yaml (override via $FRL_CONFIG).\n" +
			"See `cmd/frl/docs/operator-guide.md` in the repo for the full " +
			"wiring walkthrough (Go + Java apps, both metadata paths).",
		Example: `  frl config use-context prod
  frl store info
  frl record scan --type Order --limit 10
  frl index ls -o json | jq -r '.[].name'
  frl meta evolve-check --old previous.pb --new current.pb`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(newVersionCmd())
	root.AddCommand(newConfigCmd())
	root.AddCommand(newStoreCmd())
	root.AddCommand(newMetaCmd())
	root.AddCommand(newRecordCmd())
	root.AddCommand(newIndexCmd())
	root.AddCommand(newKeyspaceCmd())
	root.AddCommand(newTxCmd())

	return root
}
