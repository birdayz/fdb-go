package cmd

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
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
			"has nothing to read and this command errors out. A per-type " +
			"count reads the record_count_key when it is the record type " +
			"key (RecordTypeKeyExpression), and otherwise a COUNT index on " +
			"that type or a universal COUNT index grouped by record type, " +
			"as the Java record layer's getSnapshotRecordCountForRecordType " +
			"does.\n\n" +
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
						// "not found — available: A, B, C" instead of the
						// record layer's "Unknown record type X".
						if err := validateRecordType(store.GetRecordMetaData(), recordType); err != nil {
							return 0, err
						}
						return recordTypeCount(store, recordType)
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
					return fmt.Errorf("record counting is not enabled for this store — add a record_count_key to the metadata (RecordMetaDataBuilder.SetRecordCountKey) or a universal COUNT index, and redeploy")
				}
				// The per-type count found no index to read: Java's
				// RecordCoreException "Require a COUNT index on X".
				if recordType != "" && errors.As(err, new(*recordlayer.RecordCoreError)) {
					return fmt.Errorf("counting %s records needs a COUNT index on %s, a universal COUNT index grouped by record type, or a record_count_key that is the record type key: %w", recordType, recordType, err)
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
	c.Flags().StringVar(&recordType, "type", "", "count only this record type (from a record-type count key or a COUNT index)")
	c.Flags().StringVarP(&outputFmt, "output", "o", "text", "output format: text or json")
	return c
}

// recordCountResult is the typed JSON shape of `record count -o json` —
// consistent with the rest of frl's structured output (no ad-hoc maps).
type recordCountResult struct {
	Count      int64  `json:"count"`
	RecordType string `json:"record_type"`
}

// recordTypeCount is a record type's count. A record_count_key that IS the record
// type key keeps one counter per type, read at the type's key as Java's
// getSnapshotRecordCount(recordType(), value) reads it; otherwise the count comes
// from a COUNT index, as Java's getSnapshotRecordCountForRecordType takes it.
func recordTypeCount(store *recordlayer.FDBRecordStore, recordType string) (int64, error) {
	md := store.GetRecordMetaData()
	if _, byType := md.GetRecordCountKey().(*recordlayer.RecordTypeKeyExpression); byType {
		return store.GetSnapshotRecordCount(tuple.Tuple{md.GetRecordType(recordType).GetRecordTypeKey()})
	}
	return store.GetSnapshotRecordCountForRecordType(recordType)
}
