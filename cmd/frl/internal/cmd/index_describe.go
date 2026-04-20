package cmd

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/birdayz/fdb-record-layer-go/cmd/frl/internal/meta"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

func newIndexDescribeCmd() *cobra.Command {
	var contextName, metaFile string
	c := &cobra.Command{
		Use:   "describe <name>",
		Short: "Show full definition of one index",
		Example: `  frl index describe Order$price
  frl index describe Order$price --meta-file ./meta.pb`,
		ValidArgsFunction: indexNameCompletion,
		Long: "Prints every field the Record Layer tracks for an index: " +
			"type, key expression (field names), subspace key, unique / " +
			"clear-when-zero options, other options, added and last-modified " +
			"versions, the record types it applies to. Loaded from the " +
			"current context's metadata source — no FDB round-trip needed " +
			"when the source is a file.\n\n" +
			"Note: FDB-store metadata sources are not yet supported by this " +
			"command; configure `meta_file` in your context or use --meta-file.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgCtx, override, err := resolveContextAndOverride(contextName, metaFile)
			if err != nil {
				return err
			}
			if override == nil {
				src, err := meta.FromContext(cfgCtx, nil, nil)
				if err != nil {
					if errors.Is(err, meta.ErrMissingSource) {
						return fmt.Errorf("%w (context %q)", err, cfgCtx.GetName())
					}
					return err
				}
				override = src
			}
			md, err := override.Load(cmd.Context())
			if err != nil {
				return err
			}
			idx := md.GetIndex(args[0])
			if idx == nil {
				return fmt.Errorf("index %q not found — available: %s",
					args[0], strings.Join(sortedIndexNames(md), ", "))
			}
			return writeIndexDescription(cmd.OutOrStdout(), md, idx)
		},
	}
	c.Flags().StringVar(&contextName, "context", "", "context name to use")
	c.Flags().StringVar(&metaFile, "meta-file", "", "path to MetaData.pb; overrides context.metadata")
	return c
}

// sortedIndexNames returns all index names in alphabetical order — used
// in the "not found" error so operators can see what IS available.
func sortedIndexNames(md *recordlayer.RecordMetaData) []string {
	all := md.GetAllIndexes()
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// writeIndexDescription renders an `Index` as key: value lines. Field
// order matches the struct's natural reading order (identity first,
// then behavior, then metadata). Options are sorted by key.
func writeIndexDescription(out io.Writer, md *recordlayer.RecordMetaData, idx *recordlayer.Index) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Name:                   %s\n", idx.Name)
	fmt.Fprintf(&b, "Type:                   %s\n", idx.Type)
	fmt.Fprintf(&b, "Expression fields:      %s\n", strings.Join(idx.RootExpression.FieldNames(), ","))
	fmt.Fprintf(&b, "Column size:            %d\n", idx.RootExpression.ColumnSize())
	fmt.Fprintf(&b, "Subspace key:           %v\n", idx.SubspaceTupleKey())
	fmt.Fprintf(&b, "Record types:           %s\n", strings.Join(recordTypeNames(md, idx), ","))
	fmt.Fprintf(&b, "Unique:                 %t\n", idx.IsUnique())
	fmt.Fprintf(&b, "Clear-when-zero:        %t\n", idx.IsClearWhenZero())
	fmt.Fprintf(&b, "Added version:          %d\n", idx.AddedVersion)
	fmt.Fprintf(&b, "Last modified version:  %d\n", idx.LastModifiedVersion)
	if idx.Predicate != nil || idx.GetPredicateProto() != nil {
		fmt.Fprintln(&b, "Predicate:              (filtered index — see metadata proto)")
	}
	if len(idx.Options) > 0 {
		fmt.Fprintln(&b, "Options:")
		keys := make([]string, 0, len(idx.Options))
		for k := range idx.Options {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(&b, "  %s = %s\n", k, idx.Options[k])
		}
	}
	_, err := out.Write([]byte(b.String()))
	return err
}
