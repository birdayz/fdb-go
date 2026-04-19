package cmd

import (
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
	var contextName, metaFile string
	var noFDB bool
	c := &cobra.Command{
		Use:   "ls",
		Short: "List indexes with state",
		Long: "Opens the current context's store and prints one row per " +
			"index: name, type, current state (readable / write-only / " +
			"disabled / readable-unique-pending), the record types it " +
			"applies to, and the metadata version that last touched it. " +
			"--no-fdb skips opening the store and shows STATE as '—' " +
			"so that `--meta-file` can be used as a pure offline lister.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgCtx, override, err := resolveContextAndOverride(contextName, metaFile)
			if err != nil {
				return err
			}
			if noFDB {
				if override == nil {
					src, err := meta.FromContext(cfgCtx, nil, nil)
					if err != nil {
						return err
					}
					override = src
				}
				md, err := override.Load(cmd.Context())
				if err != nil {
					return err
				}
				return writeIndexList(cmd.OutOrStdout(), md, nil)
			}
			return withStoreE(cmd.Context(), cfgCtx, override,
				func(store *recordlayer.FDBRecordStore) error {
					return writeIndexList(cmd.OutOrStdout(),
						store.GetRecordMetaData(),
						func(name string) string { return store.GetIndexState(name).String() })
				})
		},
	}
	c.Flags().StringVar(&contextName, "context", "", "context name to use")
	c.Flags().StringVar(&metaFile, "meta-file", "", "path to MetaData.pb; overrides context.metadata")
	c.Flags().BoolVar(&noFDB, "no-fdb", false, "render from metadata only; skip opening the store")
	return c
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
