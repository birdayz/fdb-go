package cmd

import (
	"encoding/json"
	"errors"
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
			"--type). The store-wide count reads the metadata's " +
			"record_count_key when there is one, and otherwise falls back " +
			"to a universal COUNT index; without either, the record layer " +
			"has nothing to read and this command errors out. Per-type " +
			"counts require the metadata's count key to be a " +
			"RecordTypeKeyExpression.\n\n" +
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
				// Neither source of a count is available. A store-wide count
				// falls back to a universal COUNT index when there is no
				// record_count_key, so "no count key" alone is not the
				// diagnosis — the actionable message names both sources.
				// Matched on the error TYPE, not its wording.
				if errors.As(err, new(*recordlayer.AggregateFunctionNotSupportedError)) {
					return fmt.Errorf("record counting is not enabled for this store — add a record_count_key to the metadata (RecordMetaDataBuilder.SetRecordCountKey) or a universal COUNT index, and redeploy; per-type counts additionally need a RecordTypeKeyExpression count key")
				}
				// The per-type path still rejects a missing count key up front,
				// and its internal wording tells an operator nothing actionable.
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
