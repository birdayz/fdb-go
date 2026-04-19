package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

func newRecordCountCmd() *cobra.Command {
	var (
		contextName string
		metaFile    string
		recordType  string
	)
	c := &cobra.Command{
		Use:   "count",
		Short: "Count records in the current context's store",
		Long: "Returns the total record count (or per-type count with " +
			"--type). Both forms require the store's metadata to have a " +
			"record_count_key — without one, the record layer has no " +
			"atomic count index to read and this command errors out. " +
			"If you need per-type counts, the metadata's count key must " +
			"be a RecordTypeKeyExpression.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgCtx, override, err := resolveContextAndOverride(contextName, metaFile)
			if err != nil {
				return err
			}
			count, err := withStore(cmd.Context(), cfgCtx, override,
				func(store *recordlayer.FDBRecordStore) (int64, error) {
					if recordType != "" {
						return store.GetSnapshotRecordCountForRecordType(recordType)
					}
					return store.GetRecordCount()
				})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%d\n", count)
			return err
		},
	}
	c.Flags().StringVar(&contextName, "context", "", "context name to use")
	c.Flags().StringVar(&metaFile, "meta-file", "", "path to MetaData.pb; overrides context.metadata")
	c.Flags().StringVar(&recordType, "type", "", "count only this record type (requires RecordTypeKeyExpression count key)")
	return c
}
