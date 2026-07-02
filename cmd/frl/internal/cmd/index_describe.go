package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"fdb.dev/pkg/recordlayer"
)

func newIndexDescribeCmd() *cobra.Command {
	var addr storeAddressFlags
	var outputFmt string
	c := &cobra.Command{
		Use:   "describe <name>",
		Short: "Show full definition of one index",
		Example: `  frl index describe Order$price
  frl index describe Order$price --meta-file ./meta.pb
  frl index describe Order$price -o json | jq '.options'`,
		ValidArgsFunction: indexNameCompletion,
		Long: "Prints every field the Record Layer tracks for an index: " +
			"type, key expression (field names), subspace key, unique / " +
			"clear-when-zero options, other options, added and last-modified " +
			"versions, the record types it applies to. Loaded from the " +
			"current context's metadata source — no FDB round-trip needed " +
			"when the source is a file.\n\n" +
			"--output / -o: 'text' (default, key-aligned columns) or 'json' " +
			"(single object suitable for jq).\n\n" +
			"Note: FDB-store metadata sources are not yet supported by this " +
			"command; configure `meta_file` in your context or use --meta-file.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateOutputFormat(outputFmt, "text", "json"); err != nil {
				return err
			}
			target, err := addr.resolve()
			if err != nil {
				return err
			}
			md, err := loadTargetMetadata(cmd.Context(), target)
			if err != nil {
				return err
			}
			idx, err := lookupIndex(md, args[0])
			if err != nil {
				return err
			}
			if outputFmt == "json" {
				return writeIndexDescriptionJSON(cmd.OutOrStdout(), md, idx)
			}
			return writeIndexDescription(cmd.OutOrStdout(), md, idx)
		},
	}
	addr.register(c, true)
	c.Flags().StringVarP(&outputFmt, "output", "o", "text", "output format: text or json")
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

// indexDescribeJSON is the JSON shape emitted by `index describe -o json`.
// Mirrors the text renderer's field set so the two outputs stay informationally
// equivalent — if a field shows up in one, it must show up in the other.
type indexDescribeJSON struct {
	Name                string            `json:"name"`
	Type                string            `json:"type"`
	ExpressionFields    []string          `json:"expression_fields"`
	ColumnSize          int               `json:"column_size"`
	SubspaceKey         string            `json:"subspace_key"`
	RecordTypes         []string          `json:"record_types"`
	Unique              bool              `json:"unique"`
	ClearWhenZero       bool              `json:"clear_when_zero"`
	AddedVersion        int               `json:"added_version"`
	LastModifiedVersion int               `json:"last_modified_version"`
	HasPredicate        bool              `json:"has_predicate"`
	Options             map[string]string `json:"options"`
}

func writeIndexDescriptionJSON(out io.Writer, md *recordlayer.RecordMetaData, idx *recordlayer.Index) error {
	// Options map is copied verbatim; sorting is irrelevant in JSON maps.
	opts := idx.Options
	if opts == nil {
		opts = map[string]string{}
	}
	d := indexDescribeJSON{
		Name:                idx.Name,
		Type:                idx.Type,
		ExpressionFields:    idx.RootExpression.FieldNames(),
		ColumnSize:          idx.RootExpression.ColumnSize(),
		SubspaceKey:         fmt.Sprintf("%v", idx.SubspaceTupleKey()),
		RecordTypes:         recordTypeNames(md, idx),
		Unique:              idx.IsUnique(),
		ClearWhenZero:       idx.IsClearWhenZero(),
		AddedVersion:        idx.AddedVersion,
		LastModifiedVersion: idx.LastModifiedVersion,
		HasPredicate:        idx.Predicate != nil || idx.GetPredicateProto() != nil,
		Options:             opts,
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(d)
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
