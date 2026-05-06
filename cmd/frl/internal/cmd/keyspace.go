package cmd

import (
	"encoding/json"
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
	var outputFmt string
	c := &cobra.Command{
		Use:   "resolve <path>",
		Short: "Print the FDB byte prefix for a logical keyspace path",
		Example: `  frl keyspace resolve /myapp/prod/orders
  frl keyspace resolve /myapp/prod -o json | jq -r .prefix_hex`,
		Long: "Resolves a slash-separated path (e.g. /myapp/prod/orders) " +
			"to its FDB subspace byte prefix, packed as a tuple of strings. " +
			"Matches the addressing convention `frl` itself uses — apps with " +
			"typed keyspaces (int / UUID components) will see different " +
			"bytes and should reach for `fdbcli` instead.\n\n" +
			"--output / -o: 'text' (default, bare hex) or 'json' " +
			"({path, prefix_hex, prefix_len}).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateOutputFormat(outputFmt, "text", "json"); err != nil {
				return err
			}
			ss, err := parseKeyspacePath(args[0])
			if err != nil {
				return err
			}
			if outputFmt == "json" {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"path":       args[0],
					"prefix_hex": fmt.Sprintf("%x", ss.Bytes()),
					"prefix_len": len(ss.Bytes()),
				})
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%x\n", ss.Bytes())
			return err
		},
	}
	c.Flags().StringVarP(&outputFmt, "output", "o", "text", "output format: text or json")
	return c
}
