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
		Long: "frl is a kubectl-style CLI for inspecting and operating " +
			"record stores backed by the Go port of the FoundationDB Record " +
			"Layer. Command tree is under active design; Phase A only ships " +
			"the skeleton + `version` subcommand.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(newVersionCmd())
	root.AddCommand(newConfigCmd())
	root.AddCommand(newStoreCmd())
	root.AddCommand(newMetaCmd())
	root.AddCommand(newRecordCmd())

	return root
}
