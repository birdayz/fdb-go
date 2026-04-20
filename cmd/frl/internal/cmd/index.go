package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/birdayz/fdb-record-layer-go/cmd/frl/internal/meta"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

func newIndexCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "index",
		Short: "Inspect indexes on the current context's store",
	}
	c.AddCommand(
		newIndexLsCmd(),
		newIndexDescribeCmd(),
		newIndexScanCmd(),
	)
	return c
}

func newIndexLsCmd() *cobra.Command {
	var contextName, metaFile, outputFmt string
	var noFDB bool
	c := &cobra.Command{
		Use:   "ls",
		Short: "List indexes with state",
		Example: `  frl index ls
  frl index ls -o json
  frl index ls --meta-file ./meta.pb --no-fdb    # offline render`,
		Long: "Opens the current context's store and prints one row per " +
			"index: name, type, current state (readable / write-only / " +
			"disabled / readable-unique-pending), the record types it " +
			"applies to, and the metadata version that last touched it. " +
			"--no-fdb skips opening the store and shows STATE as '—' " +
			"so that `--meta-file` can be used as a pure offline lister.\n\n" +
			"--output / -o: 'text' (default, tabwriter-aligned) or 'json' " +
			"(array of {name, type, state, record_types, last_modified_version}).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateOutputFormat(outputFmt, "text", "json"); err != nil {
				return err
			}
			cfgCtx, override, err := resolveContextAndOverride(contextName, metaFile)
			if err != nil {
				return err
			}
			if noFDB {
				if override == nil {
					src, err := meta.FromContext(cfgCtx, nil, nil)
					if err != nil {
						return fmt.Errorf("--no-fdb requires a file metadata source: %w "+
							"(add `meta_file` to the context or pass --meta-file)", err)
					}
					override = src
				}
				md, err := override.Load(cmd.Context())
				if err != nil {
					return err
				}
				return renderIndexList(cmd.OutOrStdout(), md, nil, outputFmt)
			}
			return withStoreE(cmd.Context(), cfgCtx, override,
				func(store *recordlayer.FDBRecordStore) error {
					return renderIndexList(cmd.OutOrStdout(),
						store.GetRecordMetaData(),
						func(name string) string { return store.GetIndexState(name).String() },
						outputFmt)
				})
		},
	}
	c.Flags().StringVar(&contextName, "context", "", "context name to use")
	c.Flags().StringVar(&metaFile, "meta-file", "", "path to MetaData.pb; overrides context.metadata")
	c.Flags().BoolVar(&noFDB, "no-fdb", false, "render from metadata only; skip opening the store")
	c.Flags().StringVarP(&outputFmt, "output", "o", "text", "output format: text or json")
	return c
}

// renderIndexList dispatches on outputFmt. Keeps the text/tabwriter
// renderer in writeIndexList for backwards-compatibility and because
// nothing else prints a table; JSON rendering lives in its own helper
// so the shape is easy to evolve.
func renderIndexList(out io.Writer, md *recordlayer.RecordMetaData, stateFn func(string) string, outputFmt string) error {
	if outputFmt == "json" {
		return writeIndexListJSON(out, md, stateFn)
	}
	return writeIndexList(out, md, stateFn)
}

// writeIndexListJSON renders the index catalog as a JSON array. Shape:
//
//	[
//	  {
//	    "name": "Order$price",
//	    "type": "value",
//	    "state": "readable",        # or "—" when stateFn is nil
//	    "record_types": ["Order"],  # or ["*"] for universal indexes
//	    "last_modified_version": 1
//	  },
//	  ...
//	]
//
// Sorted by name for stable diff-across-invocations (same as text mode).
func writeIndexListJSON(out io.Writer, md *recordlayer.RecordMetaData, stateFn func(string) string) error {
	all := md.GetAllIndexes()
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)

	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	rows := make([]indexListRow, 0, len(names))
	for _, name := range names {
		idx := all[name]
		state := "—"
		if stateFn != nil {
			state = stateFn(name)
		}
		rows = append(rows, indexListRow{
			Name:                name,
			Type:                idx.Type,
			State:               state,
			RecordTypes:         recordTypeNames(md, idx),
			LastModifiedVersion: idx.LastModifiedVersion,
		})
	}
	return enc.Encode(rows)
}

// indexListRow is the JSON row shape emitted by `index ls -o json`.
// Field names are snake_case to match the Go proto/JSON convention
// across the rest of frl's structured output.
type indexListRow struct {
	Name                string   `json:"name"`
	Type                string   `json:"type"`
	State               string   `json:"state"`
	RecordTypes         []string `json:"record_types"`
	LastModifiedVersion int      `json:"last_modified_version"`
}

// writeIndexList renders a tabwriter-aligned table of every index in md,
// tagged with state returned by stateFn. If stateFn is nil (metadata-only
// mode) every row shows "—" for state. Rows are sorted by name so diffs
// across invocations are stable.
func writeIndexList(out io.Writer, md *recordlayer.RecordMetaData, stateFn func(string) string) error {
	all := md.GetAllIndexes()
	if len(all) == 0 {
		_, err := fmt.Fprintln(out, "(no indexes in metadata)")
		return err
	}
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tTYPE\tSTATE\tRECORD TYPES\tLAST MODIFIED")
	for _, name := range names {
		idx := all[name]
		state := "—"
		if stateFn != nil {
			state = stateFn(name)
		}
		types := recordTypeNames(md, idx)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n",
			name, idx.Type, state, strings.Join(types, ","), idx.LastModifiedVersion)
	}
	return tw.Flush()
}

// recordTypeNames returns the sorted names of record types this index
// applies to. For universal indexes (no explicit types), returns ["*"].
func recordTypeNames(md *recordlayer.RecordMetaData, idx *recordlayer.Index) []string {
	rts := md.RecordTypesForIndex(idx)
	if len(rts) == 0 {
		return []string{"*"}
	}
	names := make([]string, len(rts))
	for i, rt := range rts {
		names[i] = rt.Name
	}
	sort.Strings(names)
	return names
}
