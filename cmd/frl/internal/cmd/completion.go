package cmd

import (
	"github.com/spf13/cobra"

	"github.com/birdayz/fdb-record-layer-go/cmd/frl/internal/config"
)

// registerContextCompletion wires a ValidArgsFunction to the `--context`
// flag of the given command so `frl … --context <TAB>` completes from
// the names in ~/.frl/config.yaml. No-op if the command doesn't define
// a --context flag.
//
// Any error while loading the config results in no completions (rather
// than printing the error into the shell) — silent failure is the shell-
// completion convention, users see nothing break.
func registerContextCompletion(c *cobra.Command) {
	if c.Flag("context") == nil {
		return
	}
	_ = c.RegisterFlagCompletionFunc("context",
		func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
			cfg, err := config.Load()
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			names := make([]string, 0, len(cfg.GetContexts()))
			for _, ctx := range cfg.GetContexts() {
				names = append(names, ctx.GetName())
			}
			return names, cobra.ShellCompDirectiveNoFileComp
		})
}

// registerFormatCompletion wires a value-completer on -o/--output so
// the shell suggests 'text' / 'json' (and 'yaml' where supported).
// yamlToo=true for commands that accept yaml as a third option.
func registerFormatCompletion(c *cobra.Command, yamlToo bool) {
	if c.Flag("output") == nil {
		return
	}
	values := []string{"text", "json"}
	if yamlToo {
		values = []string{"json", "yaml"} // `meta get` has no text mode
	}
	_ = c.RegisterFlagCompletionFunc("output",
		func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
			return values, cobra.ShellCompDirectiveNoFileComp
		})
}
