package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"fdb.dev/pkg/recordlayer"
)

func newTxCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "tx",
		Short: "Ad-hoc transaction utilities for scripting and debugging",
	}
	c.AddCommand(newTxReadVersionCmd())
	return c
}

// newTxReadVersionCmd fetches the current read version (GRV) from FDB
// and prints it. Doubles as a connection sanity check: if this succeeds,
// the cluster file + network path are working. Read-only — never
// commits anything.
func newTxReadVersionCmd() *cobra.Command {
	var contextName, outputFmt, clusterFile string
	c := &cobra.Command{
		Use:   "read-version",
		Short: "Print the current FDB global read version",
		Example: `  frl tx read-version
  frl tx read-version -o json | jq '.read_version'`,
		Long: "Starts a read-only transaction and returns its global read " +
			"version (GRV) — the FDB equivalent of \"what time is it?\". " +
			"Doubles as a connection smoke-check: if this succeeds, the " +
			"cluster file + network path + coordinators are all reachable. " +
			"No records / metadata / keyspace needed.\n\n" +
			"--output / -o: 'text' (default, bare integer + newline — " +
			"safe for `$(frl tx read-version)`) or 'json' (`{read_version}`).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateOutputFormat(outputFmt, "text", "json"); err != nil {
				return err
			}
			// --cluster-file makes this self-contained — the classic
			// chain: frl tx read-version --cluster-file $(frl fdb up).
			f := storeAddressFlags{contextName: contextName, clusterFile: clusterFile}
			target, err := f.resolve()
			if err != nil {
				return err
			}
			db, err := openDatabase(target.clusterFile())
			if err != nil {
				return err
			}
			rec := recordlayer.NewFDBDatabase(db)
			result, err := rec.Run(cmd.Context(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
				return rtx.Transaction().GetReadVersion().Get()
			})
			if err != nil {
				return fmt.Errorf("read global version: %w", err)
			}
			v, _ := result.(int64)
			if outputFmt == "json" {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{"read_version": v})
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%d\n", v)
			return err
		},
	}
	c.Flags().StringVar(&contextName, "context", "", "context name to use")
	c.Flags().StringVar(&clusterFile, "cluster-file", "", "FDB cluster file; overrides the context's cluster_file — chains with `frl fdb up`")
	c.Flags().StringVarP(&outputFmt, "output", "o", "text", "output format: text or json")
	return c
}
