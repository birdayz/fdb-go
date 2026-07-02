// Package cmd houses the `frl` cobra command tree. NewRoot returns the
// assembled root command with every noun (record, index, store, meta,
// config, keyspace, tx) attached. All shell-completion plumbing is
// wired in one pass by registerCompletions() so new commands that
// declare --context / --meta-file / --output / --type / positional
// args inherit tab-complete behavior without touching cobra's
// completion API directly.
package cmd

import (
	"github.com/spf13/cobra"
)

// NewRoot builds the top-level `frl` command. Every subcommand should be
// attached here (or onto a child command) so a single call to fang.Execute
// in main covers the entire tree.
func NewRoot() *cobra.Command {
	// Resolve version once so `frl --version` / `frl -v` render the same
	// string `frl version` emits — cobra otherwise falls back to
	// "unknown (built from source)" because it doesn't know about our
	// runtime/debug-driven version discovery.
	v := readVersion()

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
		Version:       v.Version,
	}
	// Template matches `frl version` (unqualified + build metadata).
	// Without this, cobra's default `frl version <V>` format drops the
	// Go toolchain / goos / goarch suffix and operators lose the cross-
	// check against `go version -m`.
	root.SetVersionTemplate("frl " + v.Version +
		" (" + v.GoVersion + " " + v.GOOS + "/" + v.GOARCH + ")\n")

	root.AddCommand(newVersionCmd())
	root.AddCommand(newConfigCmd())
	root.AddCommand(newStoreCmd())
	root.AddCommand(newMetaCmd())
	root.AddCommand(newRecordCmd())
	root.AddCommand(newIndexCmd())
	root.AddCommand(newKeyspaceCmd())
	root.AddCommand(newTxCmd())
	root.AddCommand(newSQLCmd())
	root.AddCommand(newFdbCmd())
	root.AddCommand(newStatusCmd())

	// Wire shell-completion helpers across the whole tree in one pass.
	// Every subcommand carrying --context gets its completion function
	// pointed at config.Load(); every --output gets the {text,json[,yaml]}
	// hint.  Doing this centrally keeps new commands completion-aware
	// for free — they just need to declare the flag.
	registerCompletions(root)

	return root
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
