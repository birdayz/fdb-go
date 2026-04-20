package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"buf.build/go/protoyaml"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	configv1 "github.com/birdayz/fdb-record-layer-go/cmd/frl/gen/frl/config/v1"
	"github.com/birdayz/fdb-record-layer-go/cmd/frl/internal/config"
)

// newConfigCmd houses every config-file subcommand:
//   - init:             scaffold a starter ~/.frl/config.yaml
//   - schema:           dump empty Config as JSON (field discovery)
//   - view:             print effective current context as YAML
//   - path:             print the effective config file path
//   - use-context:      switch current_context
//   - current-context:  print the active context's name
//   - get-contexts:     list all contexts, mark active
func newConfigCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "Manage frl configuration (~/.frl/config.yaml)",
	}
	c.AddCommand(
		newConfigSchemaCmd(),
		newConfigViewCmd(),
		newConfigUseContextCmd(),
		newConfigCurrentContextCmd(),
		newConfigGetContextsCmd(),
		newConfigPathCmd(),
		newConfigInitCmd(),
	)
	return c
}

// newConfigInitCmd scaffolds a minimal ~/.frl/config.yaml at the
// effective config path. Prints the path it wrote so operators can
// immediately open it for editing. Refuses to overwrite an existing
// file — operators get `frl config view` for that.
func newConfigInitCmd() *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:   "init",
		Short: "Create a starter config file at the effective path",
		Example: `  frl config init
  FRL_CONFIG=/etc/frl.yaml frl config init
  frl config init --force    # overwrite existing file`,
		Long: "Creates a starter ~/.frl/config.yaml (or $FRL_CONFIG) with " +
			"one commented-out example context. Refuses to overwrite " +
			"existing files unless --force is set.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := config.Path()
			if err != nil {
				return err
			}
			if !force {
				if _, statErr := os.Stat(path); statErr == nil {
					return fmt.Errorf("refusing to overwrite existing %s — "+
						"use `frl config view` to inspect, or --force to replace", path)
				}
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
			}
			if err := os.WriteFile(path, []byte(initTemplate), 0o600); err != nil {
				return fmt.Errorf("write %s: %w", path, err)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"Wrote starter config to %s — edit it then `frl config use-context <name>`\n",
				path)
			return err
		},
	}
	c.Flags().BoolVar(&force, "force", false, "overwrite existing config file")
	return c
}

// initTemplate is the scaffold written by `frl config init`. Comments
// guide the operator through the two metadata paths (file vs FDBMetaDataStore)
// and point at the full operator-guide.
//
// The `contexts` field MUST be the empty list `[]` rather than a bare
// `contexts:` (which protoyaml reads as a null scalar and refuses,
// because it expects a sequence). That's why the example is written
// inside a YAML comment above an empty-list literal — the file parses
// cleanly before any editing, so `frl config view` / `get-contexts`
// don't error on a fresh init.
const initTemplate = `# frl CLI configuration.
# See cmd/frl/docs/operator-guide.md (in the repo) for wiring walkthroughs.
#
# After editing, switch context with:
#   frl config use-context <name>

current_context: ""

# Example — copy, uncomment, and edit into the list below:
# contexts:
#   - name: local
#     cluster_file: /etc/foundationdb/fdb.cluster
#     keyspace_path: /myapp/orders
#     metadata:
#       # Path A: app-exported meta.pb alongside binaries (most common).
#       meta_file: /etc/myapp/meta.pb
#       # Path B: FDBMetaDataStore persisted in FDB (for schema evolution).
#       # meta_store_keyspace: /myapp/_meta
contexts: []
`

// newConfigPathCmd prints the effective config file path (after
// env-var overrides). Handy for debugging "why isn't my config
// loading" questions — the answer is almost always "wrong path".
func newConfigPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the effective config file path",
		Long: "Prints the path frl is currently reading configuration " +
			"from. Set $FRL_CONFIG to override the default " +
			"(~/.frl/config.yaml). Exits with the path even if the " +
			"file doesn't exist yet — useful for `mkdir -p` / `touch` " +
			"bootstrap scripts.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := config.Path()
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), path)
			return err
		},
	}
}

// newConfigCurrentContextCmd prints the active context's name (or nothing
// + an error if none is set). Matches kubectl / kaf convention.
func newConfigCurrentContextCmd() *cobra.Command {
	var outputFmt string
	c := &cobra.Command{
		Use:   "current-context",
		Short: "Print the active context's name",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateOutputFormat(outputFmt, "text", "json"); err != nil {
				return err
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			name := cfg.GetCurrentContext()
			if name == "" {
				path, _ := config.Path()
				// Quote the YAML key so fang's capitalized banner reads
				// as "No current context set in …" rather than the
				// awkward "Current_context is empty in …".
				return fmt.Errorf("no current context set in %s — run `frl config use-context <name>`", path)
			}
			if outputFmt == "json" {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]string{"name": name})
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), name)
			return err
		},
	}
	c.Flags().StringVarP(&outputFmt, "output", "o", "text", "output format: text or json")
	return c
}

// newConfigGetContextsCmd lists every context, marking the active one
// with '*'. No sorting — order in config.yaml is preserved so operators
// can reason about their file.
func newConfigGetContextsCmd() *cobra.Command {
	var outputFmt string
	c := &cobra.Command{
		Use:   "get-contexts",
		Short: "List all configured contexts",
		Long: "Prints one line per context; '*' marks the active one. Order " +
			"preserves the config.yaml layout.\n\n" +
			"--output / -o: 'text' (default) or 'json' (array of " +
			"{name, active} objects — use `jq 'map(select(.active)) | .[0].name'` " +
			"to script the active context).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateOutputFormat(outputFmt, "text", "json"); err != nil {
				return err
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			current := cfg.GetCurrentContext()
			if outputFmt == "json" {
				return writeContextsJSON(cmd.OutOrStdout(), cfg.GetContexts(), current)
			}
			if len(cfg.GetContexts()) == 0 {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "(no contexts configured)")
				return err
			}
			for _, ctx := range cfg.GetContexts() {
				marker := " "
				if ctx.GetName() == current {
					marker = "*"
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", marker, ctx.GetName()); err != nil {
					return err
				}
			}
			return nil
		},
	}
	c.Flags().StringVarP(&outputFmt, "output", "o", "text", "output format: text or json")
	return c
}

// contextRow is the JSON shape emitted by `config get-contexts -o json`.
type contextRow struct {
	Name   string `json:"name"`
	Active bool   `json:"active"`
}

func writeContextsJSON(out io.Writer, contexts []*configv1.Context, current string) error {
	rows := make([]contextRow, 0, len(contexts))
	for _, ctx := range contexts {
		rows = append(rows, contextRow{
			Name:   ctx.GetName(),
			Active: ctx.GetName() == current,
		})
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}

func newConfigSchemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Print an empty Config message as JSON (schema probe)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// UseProtoNames so the JSON key names match the snake_case keys
			// the operator writes in config.yaml (protoyaml also uses proto
			// names) — otherwise `schema` shows `currentContext` but the
			// file wants `current_context`, and that mismatch is a footgun.
			out, err := protojson.MarshalOptions{
				Multiline:       true,
				Indent:          "  ",
				EmitUnpopulated: true,
				UseProtoNames:   true,
			}.Marshal(&configv1.Config{})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(out))
			return err
		},
	}
}

func newConfigViewCmd() *cobra.Command {
	var contextName, outputFmt string
	c := &cobra.Command{
		Use:   "view",
		Short: "Print the effective context as YAML or JSON",
		// Opt into the yaml completion set — `view` defaults to yaml
		// (matches config.yaml on disk) with json as the alternative.
		Annotations: map[string]string{
			AnnotationOutputYAML: "true",
		},
		Example: `  frl config view
  frl config view --context prod
  frl config view -o json | jq .keyspace_path`,
		Long: "Reads ~/.frl/config.yaml (or $FRL_CONFIG) and prints the " +
			"currently-selected context. Use --context to pick a specific " +
			"one. Missing config file is reported verbatim so the user " +
			"knows they need to run `frl config use-context` first.\n\n" +
			"--output / -o: 'yaml' (default, matches the on-disk file) or " +
			"'json' (protojson with snake_case keys — matches the YAML key " +
			"set so `jq` and `yq` pipelines can be swapped 1:1).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateOutputFormat(outputFmt, "yaml", "json"); err != nil {
				return err
			}
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			ctx, err := config.ResolveContext(cfg, contextName)
			if err != nil {
				if errors.Is(err, config.ErrNoContext) {
					path, _ := config.Path()
					return fmt.Errorf(
						"%w (config: %s) — add contexts to the file or use --context <name>",
						err, path)
				}
				return err
			}
			var bytes []byte
			switch outputFmt {
			case "json":
				// UseProtoNames to stay consistent with `config schema` and
				// the on-disk YAML key set (snake_case).
				bytes, err = protojson.MarshalOptions{
					Multiline:     true,
					Indent:        "  ",
					UseProtoNames: true,
				}.Marshal(ctx)
			default: // "" or "yaml"
				// UseProtoNames so `view` output round-trips through the
				// file format — without it, protoyaml emits
				// `clusterFile`/`keyspacePath` while the on-disk YAML
				// uses `cluster_file`/`keyspace_path`. Operators copying
				// `view` output back into config.yaml would otherwise
				// trip the "unknown field" loader error.
				bytes, err = protoyaml.MarshalOptions{
					Indent:        2,
					UseProtoNames: true,
				}.Marshal(ctx)
			}
			if err != nil {
				return fmt.Errorf("marshal context: %w", err)
			}
			_, err = cmd.OutOrStdout().Write(bytes)
			return err
		},
	}
	c.Flags().StringVar(&contextName, "context", "",
		"context name to show (default: Config.current_context)")
	c.Flags().StringVarP(&outputFmt, "output", "o", "yaml",
		"output format: yaml or json")
	return c
}

func newConfigUseContextCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use-context <name>",
		Short: "Set current_context to <name>",
		Example: `  frl config use-context prod
  frl config use-context local`,
		Args: cobra.ExactArgs(1),
		ValidArgsFunction: func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
			if len(args) > 0 {
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return contextNamesForCompletion(), cobra.ShellCompDirectiveNoFileComp
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			// Validate that <name> actually exists before writing; prevents
			// typos from silently pointing at a nonexistent context.
			if _, err := config.ResolveContext(cfg, name); err != nil {
				// When the resolve fails we want to distinguish two cases:
				//   1) config file is empty / missing all contexts → suggest
				//      editing the file, print its path
				//   2) some contexts exist but `name` isn't one → suggest a
				//      `get-contexts` lookup with the list of candidates
				// Both cases surface a file path so the operator can find it.
				path, _ := config.Path()
				if len(cfg.GetContexts()) == 0 {
					return fmt.Errorf("%w\nno contexts configured in %s — edit the file to add one (see cmd/frl/docs/operator-guide.md)",
						err, path)
				}
				names := make([]string, 0, len(cfg.GetContexts()))
				for _, ctx := range cfg.GetContexts() {
					names = append(names, ctx.GetName())
				}
				return fmt.Errorf("%w\navailable contexts in %s: %s",
					err, path, strings.Join(names, ", "))
			}
			cfg.CurrentContext = name
			if err := config.Save(cfg); err != nil {
				return err
			}
			path, _ := config.Path()
			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"Switched to context %q (%s)\n", name, path)
			return err
		},
	}
}
