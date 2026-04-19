package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"

	configv1 "github.com/birdayz/fdb-record-layer-go/cmd/frl/gen/frl/config/v1"
)

// newConfigCmd houses config-file related subcommands. Phase A only ships
// `frl config schema`, which prints an empty Config message as JSON — enough
// to exercise the generated proto package end-to-end and confirm the
// self-contained buf.gen.yaml + Bazel go_deps wiring works.
func newConfigCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "Manage frl configuration (~/.frl/config.yaml)",
	}
	c.AddCommand(newConfigSchemaCmd())
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
