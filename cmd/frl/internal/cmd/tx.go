package cmd

import (
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
	var contextName string
	c := &cobra.Command{
		Use:   "read-version",
		Short: "Print the current FDB global read version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
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
				return fmt.Errorf("GetReadVersion: %w", err)
			}
			v, _ := result.(int64)
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%d\n", v)
			return err
		},
	}
	c.Flags().StringVar(&contextName, "context", "", "context name to use")
	return c
}
