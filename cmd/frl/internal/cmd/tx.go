package cmd

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/birdayz/fdb-record-layer-go/cmd/frl/internal/config"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
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
	var contextName, outputFmt string
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
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			cfgCtx, err := config.ResolveContext(cfg, contextName)
			if err != nil {
				if errors.Is(err, config.ErrNoContext) {
					path, _ := config.Path()
					return fmt.Errorf("%w (config: %s)", err, path)
				}
				return err
			}
			db, err := openDatabase(cfgCtx.GetClusterFile())
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
	c.Flags().StringVarP(&outputFmt, "output", "o", "text", "output format: text or json")
	return c
}
