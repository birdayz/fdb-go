package cmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"fdb.dev/pkg/recordlayer"
)

func newRecordCountCmd() *cobra.Command {
	var (
		addr       storeAddressFlags
		recordType string
		outputFmt  string
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
			target, err := addr.resolve()
			if err != nil {
				return err
			}
			count, err := withStore(cmd.Context(), target,
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
				// The record layer's internal wording ("recordCountKey is
				// nil") tells an operator nothing actionable — say what to
				// add and where.
				if strings.Contains(err.Error(), "recordCountKey is nil") {
					return fmt.Errorf("record counting is not enabled for this store — add a record_count_key to the metadata (RecordMetaDataBuilder.SetRecordCountKey) and redeploy; per-type counts additionally need a RecordTypeKeyExpression count key")
				}
				return err
			}
			if outputFmt == "json" {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(recordCountResult{
					Count:      count,
					RecordType: recordType, // "" for store-wide
				})
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%d\n", count)
			return err
		},
	}
	addr.register(c, true)
	c.Flags().StringVar(&recordType, "type", "", "count only this record type (requires RecordTypeKeyExpression count key)")
	c.Flags().StringVarP(&outputFmt, "output", "o", "text", "output format: text or json")
	return c
}

// recordCountResult is the typed JSON shape of `record count -o json` —
// consistent with the rest of frl's structured output (no ad-hoc maps).
type recordCountResult struct {
	Count      int64  `json:"count"`
	RecordType string `json:"record_type"`
}
