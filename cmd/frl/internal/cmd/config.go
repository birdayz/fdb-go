package cmd

import (
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
	)
	return c
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
