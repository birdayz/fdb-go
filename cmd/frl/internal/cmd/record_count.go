package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

func newRecordCountCmd() *cobra.Command {
	var (
		contextName string
		metaFile    string
		recordType  string
		outputFmt   string
	)
	c := &cobra.Command{
		Use:   "count",
		Short: "Count records in the current context's store",
		Example: `  frl record count
  frl record count --type Order
  frl record count -o json | jq '.count'`,
		Long: "Returns the total record count (or per-type count with " +
			"--type). Both forms require the store's metadata to have a " +
			"record_count_key — without one, the record layer has no " +
			"atomic count index to read and this command errors out. " +
			"If you need per-type counts, the metadata's count key must " +
			"be a RecordTypeKeyExpression.\n\n" +
			"--output / -o: 'text' (default, bare integer) or 'json' " +
			"({count, record_type}). record_type is empty for store-wide counts.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateOutputFormat(outputFmt, "text", "json"); err != nil {
				return err
			}
			cfgCtx, override, err := resolveContextAndOverride(contextName, metaFile)
			if err != nil {
				return err
			}
			count, err := withStore(cmd.Context(), cfgCtx, override,
				func(store *recordlayer.FDBRecordStore) (int64, error) {
					if recordType != "" {
						// Up-front type validation so a typo surfaces as
						// "not found — available: A, B, C" instead of
						// whatever internal error the record layer returns
						// (which varies between "unknown record type" and
						// "requires RecordTypeKeyExpression" depending on
						// whether the count_key shape is wrong too).
						if err := validateRecordType(store.GetRecordMetaData(), recordType); err != nil {
							return 0, err
						}
						return store.GetSnapshotRecordCountForRecordType(recordType)
					}
					return store.GetRecordCount()
				})
			if err != nil {
				return err
			}
			if outputFmt == "json" {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"count":       count,
					"record_type": recordType, // "" for store-wide
				})
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%d\n", count)
			return err
		},
	}
	c.Flags().StringVar(&contextName, "context", "", "context name to use")
	c.Flags().StringVar(&metaFile, "meta-file", "", "path to MetaData.pb; overrides context.metadata")
	c.Flags().StringVar(&recordType, "type", "", "count only this record type (requires RecordTypeKeyExpression count key)")
	c.Flags().StringVarP(&outputFmt, "output", "o", "text", "output format: text or json")
	return c
}
