package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"fdb.dev/pkg/recordlayer"
)

func newIndexScanCmd() *cobra.Command {
	var (
		contextName string
		metaFile    string
		limit       int
		reverse     bool
	)
	c := &cobra.Command{
		Use:   "scan <name>",
		Short: "Scan index entries",
		Example: `  frl index scan Order$price --limit 10
  frl index scan Order$price --reverse --limit 5
  frl index scan Order$price | jq -s 'map(.primary_key)'`,
		ValidArgsFunction: indexNameCompletion,
		Long: "Cursor-backed scan over an index's entries in the current " +
			"context's store. Entries are emitted as newline-delimited JSON " +
			"envelopes with the indexed values, reconstructed primary key, " +
			"and (for non-VALUE indexes) the index value tuple.\n\n" +
			"Only plain index scans are supported here — TEXT, BITMAP_VALUE, " +
			"VECTOR, and TIME_WINDOW_LEADERBOARD indexes require type-specific " +
			"scan modes which aren't wired into `frl` yet.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgCtx, override, err := resolveContextAndOverride(contextName, metaFile)
			if err != nil {
				return err
			}
			name := args[0]
			return withStoreE(cmd.Context(), cfgCtx, override,
				func(store *recordlayer.FDBRecordStore) error {
					idx, err := lookupIndex(store.GetRecordMetaData(), name)
					if err != nil {
						return err
					}
					return scanIndexAndRender(cmd.Context(), cmd.OutOrStdout(), store, idx, limit, reverse)
				})
		},
	}
	c.Flags().StringVar(&contextName, "context", "", "context name to use")
	c.Flags().StringVar(&metaFile, "meta-file", "", "path to MetaData.pb; overrides context.metadata")
	c.Flags().IntVar(&limit, "limit", defaultScanLimit, "max entries to return; 0 means unlimited")
	c.Flags().BoolVar(&reverse, "reverse", false, "scan in reverse order")
	return c
}

// scanIndexAndRender drives a record-layer index cursor and emits one
// JSON envelope per line. Envelope shape:
//
//	{"index":"name","key":[...],"primary_key":"pk","value":[...]}
//
// Unused fields collapse to empty — value is typically empty for VALUE
// indexes. Keeps the output machine-friendly without exploding per-type.
func scanIndexAndRender(
	ctx context.Context,
	out io.Writer,
	store *recordlayer.FDBRecordStore,
	idx *recordlayer.Index,
	limit int,
	reverse bool,
) error {
	scanProps := recordlayer.ForwardScan()
	if reverse {
		scanProps = recordlayer.ReverseScan()
	}
	if limit > 0 {
		scanProps.ExecuteProperties.ReturnedRowLimit = limit
	}
	cursor := store.ScanIndex(idx, recordlayer.TupleRangeAll, nil, scanProps)
	defer cursor.Close()

	for {
		result, err := cursor.OnNext(ctx)
		if err != nil {
			return fmt.Errorf("index scan: %w", err)
		}
		if !result.HasNext() {
			return nil
		}
		entry := result.GetValue()
		if err := writeIndexEntryAsJSON(out, entry); err != nil {
			return err
		}
	}
}

func writeIndexEntryAsJSON(out io.Writer, e *recordlayer.IndexEntry) error {
	// Key, Value, PrimaryKey all render via formatPK — a stable
	// comma-separated form. Suitable for machine consumption (grep,
	// awk, cut); structured json-of-tuples would add type ambiguity
	// without adding information.
	//
	// json.Marshal on the strings (not fmt.Sprintf %q) because %q emits
	// Go's escape syntax (\x00 for NULs) which isn't valid JSON. See
	// record.go:writeRecordAsJSON for the same fix.
	name, _ := json.Marshal(e.Index.Name)
	values, _ := json.Marshal(formatPK(e.IndexValues()))
	pk, _ := json.Marshal(formatPK(e.PrimaryKey()))
	value, _ := json.Marshal(formatPK(e.Value))
	_, err := fmt.Fprintf(out,
		`{"index":%s,"index_values":%s,"primary_key":%s,"value":%s}`+"\n",
		name, values, pk, value)
	return err
}
