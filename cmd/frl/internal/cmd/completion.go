package cmd

import (
	"github.com/spf13/cobra"

	"github.com/birdayz/fdb-record-layer-go/cmd/frl/internal/config"
	"github.com/birdayz/fdb-record-layer-go/cmd/frl/internal/meta"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
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

// registerRecordTypeCompletion wires `--type <TAB>` on commands that
// filter by record type (record scan, record count, …). Candidates are
// the record type names declared in the current context's metadata.
// Silently returns no completions if metadata can't be loaded — shell
// completion must never print errors.
func registerRecordTypeCompletion(c *cobra.Command) {
	if c.Flag("type") == nil {
		return
	}
	_ = c.RegisterFlagCompletionFunc("type",
		func(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
			cfg, err := config.Load()
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			ctxName, _ := cmd.Flags().GetString("context")
			cfgCtx, err := config.ResolveContext(cfg, ctxName)
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			// If --meta-file was passed, respect that override.
			metaFile, _ := cmd.Flags().GetString("meta-file")
			var src meta.Source
			if metaFile != "" {
				src = &meta.FileSource{Path: metaFile}
			} else {
				src, err = meta.FromContext(cfgCtx, nil, nil)
				if err != nil {
					return nil, cobra.ShellCompDirectiveNoFileComp
				}
			}
			md, err := src.Load(cmd.Context())
			if err != nil {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			rts := md.RecordTypes()
			names := make([]string, 0, len(rts))
			for n := range rts {
				names = append(names, n)
			}
			return names, cobra.ShellCompDirectiveNoFileComp
		})
}

// contextNamesForCompletion returns the configured context names, or
// nil on any error. Shared helper for the use-context positional
// completion below.
func contextNamesForCompletion() []string {
	cfg, err := config.Load()
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(cfg.GetContexts()))
	for _, ctx := range cfg.GetContexts() {
		names = append(names, ctx.GetName())
	}
	return names
}

// loadMetaForCompletion loads the current metadata the same way the
// --type completer does — context-aware with --meta-file override.
// Returns nil silently on any error.
func loadMetaForCompletion(cmd *cobra.Command) *recordlayer.RecordMetaData {
	cfg, err := config.Load()
	if err != nil {
		return nil
	}
	ctxName, _ := cmd.Flags().GetString("context")
	cfgCtx, err := config.ResolveContext(cfg, ctxName)
	if err != nil {
		return nil
	}
	metaFile, _ := cmd.Flags().GetString("meta-file")
	var src meta.Source
	if metaFile != "" {
		src = &meta.FileSource{Path: metaFile}
	} else {
		src, err = meta.FromContext(cfgCtx, nil, nil)
		if err != nil {
			return nil
		}
	}
	md, err := src.Load(cmd.Context())
	if err != nil {
		return nil
	}
	return md
}

// indexNameCompletion is a ValidArgsFunction for positional <index name>
// arguments. Wired on `index describe` / `index scan`. Returns all
// known index names from the loaded metadata; silent on error.
func indexNameCompletion(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	md := loadMetaForCompletion(cmd)
	if md == nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	all := md.GetAllIndexes()
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// recordTypeNameCompletion is a ValidArgsFunction for positional
// <record type> arguments on `meta types describe`.
func recordTypeNameCompletion(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	md := loadMetaForCompletion(cmd)
	if md == nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	rts := md.RecordTypes()
	names := make([]string, 0, len(rts))
	for n := range rts {
		names = append(names, n)
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}

// AnnotationOutputYAML is the cobra annotation key that flags a command
// as accepting --output=yaml (instead of the default text|json pair).
// Set via `c.Annotations[AnnotationOutputYAML] = "true"` at command-
// construction time; registerFormatCompletion reads it to pick the
// right completion set. Using an annotation avoids the tree-walker
// needing to know command-name → output-set mappings.
const AnnotationOutputYAML = "frl.completion.output_accepts_yaml"

// registerFormatCompletion wires a value-completer on -o/--output.
// By default the completer suggests `text` / `json`; commands whose
// `--output` accepts YAML instead (currently only `meta get`) mark
// themselves via AnnotationOutputYAML and get `json` / `yaml`.
func registerFormatCompletion(c *cobra.Command) {
	if c.Flag("output") == nil {
		return
	}
	values := []string{"text", "json"}
	if c.Annotations[AnnotationOutputYAML] == "true" {
		values = []string{"json", "yaml"}
	}
	_ = c.RegisterFlagCompletionFunc("output",
		func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
			return values, cobra.ShellCompDirectiveNoFileComp
		})
}
