package cmd

import (
	"encoding/json"
	"errors"
	"fmt"

	"buf.build/go/protoyaml"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	configv1 "github.com/birdayz/fdb-record-layer-go/cmd/frl/gen/frl/config/v1"
	"github.com/birdayz/fdb-record-layer-go/cmd/frl/internal/config"
)

// newConfigCmd houses config-file subcommands. v1 surface:
//   - schema:       dump empty Config as JSON (schema probe)
//   - view:         print effective current context as YAML
//   - use-context:  switch current_context
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
	)
	return c
}

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
	return &cobra.Command{
		Use:   "current-context",
		Short: "Print the active context's name",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			name := cfg.GetCurrentContext()
			if name == "" {
				path, _ := config.Path()
				return fmt.Errorf("current_context is empty in %s — run `frl config use-context <name>`", path)
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), name)
			return err
		},
	}
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
			switch outputFmt {
			case "", "text", "json":
			default:
				return fmt.Errorf("invalid --output %q: want text or json", outputFmt)
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

func writeContextsJSON(out interface{ Write([]byte) (int, error) }, contexts []*configv1.Context, current string) error {
	rows := make([]contextRow, 0, len(contexts))
	for _, ctx := range contexts {
		rows = append(rows, contextRow{
			Name:   ctx.GetName(),
			Active: ctx.GetName() == current,
		})
	}
	enc := json.NewEncoder(&writerAdapter{out})
	enc.SetIndent("", "  ")
	return enc.Encode(rows)
}

func newConfigSchemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Print an empty Config message as JSON (schema probe)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := protojson.MarshalOptions{
				Multiline:       true,
				Indent:          "  ",
				EmitUnpopulated: true,
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
	var contextName string
	c := &cobra.Command{
		Use:   "view",
		Short: "Print the effective context as YAML",
		Long: "Reads ~/.frl/config.yaml (or $FRL_CONFIG) and prints the " +
			"currently-selected context. Use --context to pick a specific " +
			"one. Missing config file is reported verbatim so the user " +
			"knows they need to run `frl config use-context` first.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
			out, err := protoyaml.MarshalOptions{Indent: 2}.Marshal(ctx)
			if err != nil {
				return fmt.Errorf("marshal context: %w", err)
			}
			_, err = cmd.OutOrStdout().Write(out)
			return err
		},
	}
	c.Flags().StringVar(&contextName, "context", "",
		"context name to show (default: Config.current_context)")
	return c
}

func newConfigUseContextCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use-context <name>",
		Short: "Set current_context to <name>",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			// Validate that <name> actually exists before writing; prevents
			// typos from silently pointing at a nonexistent context.
			if _, err := config.ResolveContext(cfg, name); err != nil {
				return err
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
