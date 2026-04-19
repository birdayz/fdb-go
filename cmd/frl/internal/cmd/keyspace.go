package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newKeyspaceCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "keyspace",
		Short: "Navigate FDB keyspace paths used by record stores",
	}
	c.AddCommand(newKeyspaceResolveCmd())
	return c
}

// newKeyspaceResolveCmd prints the FDB byte prefix a logical path maps
// to. Useful for matching against `fdbcli getrange` output or debugging
// why a store isn't where an operator expected. Pure computation — no
// FDB connection needed.
func newKeyspaceResolveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resolve <path>",
		Short: "Print the FDB byte prefix for a logical keyspace path",
		Long: "Resolves a slash-separated path (e.g. /myapp/prod/orders) " +
			"to its FDB subspace byte prefix, packed as a tuple of strings. " +
			"Matches the addressing convention `frl` itself uses — apps with " +
			"typed keyspaces (int / UUID components) will see different " +
			"bytes and should reach for `fdbcli` instead.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ss, err := parseKeyspacePath(args[0])
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%x\n", ss.Bytes())
			return err
		},
	}
}
