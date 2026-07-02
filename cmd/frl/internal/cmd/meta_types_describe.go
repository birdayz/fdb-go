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

func newMetaTypesDescribeCmd() *cobra.Command {
	var addr storeAddressFlags
	var outputFmt string
	c := &cobra.Command{
		Use:   "describe <name>",
		Short: "Show full definition of one record type",
		Example: `  frl meta types describe Order
  frl meta types describe Order --meta-file ./meta.pb -o json`,
		ValidArgsFunction: recordTypeNameCompletion,
		Long: "Prints the record type's primary key, record-type key (for " +
			"multi-type stores that use RecordTypeKeyExpression), since-" +
			"version, proto field count, and every index that touches this " +
			"type (including universal/multi-type ones).\n\n" +
			"--output / -o: 'text' (default, key:value lines) or 'json' " +
			"(single object with primary_key, record_type_key, proto_message, " +
			"indexes, multi_type_indexes, universal_indexes).\n\n" +
			"Note: FDB-store metadata sources are not yet supported by " +
			"this command; configure `meta_file` in your context or use " +
			"--meta-file.",
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
			rt, err := lookupRecordType(md, args[0])
			if err != nil {
				return err
			}
			if outputFmt == "json" {
				return writeRecordTypeDescriptionJSON(cmd.OutOrStdout(), md, rt)
			}
			return writeRecordTypeDescription(cmd.OutOrStdout(), md, rt)
		},
	}
	addr.register(c, true)
	c.Flags().StringVarP(&outputFmt, "output", "o", "text", "output format: text or json")
	return c
}

// recordTypeDescription is the JSON shape emitted by meta types describe -o json.
// Fields mirror writeRecordTypeDescription's text output one-to-one.
type recordTypeDescription struct {
	Name             string         `json:"name"`
	PrimaryKey       string         `json:"primary_key"`
	SinceVersion     int            `json:"since_version,omitempty"`
	RecordTypeKey    string         `json:"record_type_key"`
	ProtoMessage     string         `json:"proto_message,omitempty"`
	ProtoFieldCount  int            `json:"proto_field_count,omitempty"`
	Indexes          []indexSummary `json:"indexes,omitempty"`
	MultiTypeIndexes []indexSummary `json:"multi_type_indexes,omitempty"`
	UniversalIndexes []indexSummary `json:"universal_indexes,omitempty"`
}

// indexSummary is a compact representation of an index in JSON output.
type indexSummary struct {
	Name   string   `json:"name"`
	Type   string   `json:"type"`
	Fields []string `json:"fields"`
}

func writeRecordTypeDescriptionJSON(out io.Writer, md *recordlayer.RecordMetaData, rt *recordlayer.RecordType) error {
	pk := pkFieldsOrUnset(rt.PrimaryKey)
	rtk := fmt.Sprintf("(implicit: %d)", rt.RecordTypeIndex)
	if rt.HasExplicitRecordTypeKey() {
		rtk = fmt.Sprintf("%v", rt.GetRecordTypeKey())
	}
	desc := recordTypeDescription{
		Name:             rt.Name,
		PrimaryKey:       pk,
		SinceVersion:     rt.SinceVersion,
		RecordTypeKey:    rtk,
		Indexes:          summariseIndexes(rt.GetIndexes()),
		MultiTypeIndexes: summariseIndexes(rt.GetMultiTypeIndexes()),
		UniversalIndexes: summariseIndexes(md.GetUniversalIndexes()),
	}
	if rt.Descriptor != nil {
		desc.ProtoMessage = string(rt.Descriptor.FullName())
		desc.ProtoFieldCount = rt.Descriptor.Fields().Len()
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(desc)
}

func summariseIndexes(indexes []*recordlayer.Index) []indexSummary {
	if len(indexes) == 0 {
		return nil
	}
	out := make([]indexSummary, len(indexes))
	for i, idx := range indexes {
		out[i] = indexSummary{
			Name:   idx.Name,
			Type:   idx.Type,
			Fields: idx.RootExpression.FieldNames(),
		}
	}
	return out
}

func sortedRecordTypeNames(md *recordlayer.RecordMetaData) []string {
	rts := md.RecordTypes()
	names := make([]string, 0, len(rts))
	for n := range rts {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func writeRecordTypeDescription(out io.Writer, md *recordlayer.RecordMetaData, rt *recordlayer.RecordType) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Name:                   %s\n", rt.Name)
	pk := pkFieldsOrUnset(rt.PrimaryKey)
	fmt.Fprintf(&b, "Primary key:            %s\n", pk)
	if rt.SinceVersion > 0 {
		fmt.Fprintf(&b, "Since version:          %d\n", rt.SinceVersion)
	}
	if rt.HasExplicitRecordTypeKey() {
		fmt.Fprintf(&b, "Record type key:        %v\n", rt.GetRecordTypeKey())
	} else {
		fmt.Fprintf(&b, "Record type key:        (implicit: %d)\n", rt.RecordTypeIndex)
	}
	if rt.Descriptor != nil {
		fmt.Fprintf(&b, "Proto message:          %s (%d fields)\n",
			rt.Descriptor.FullName(), rt.Descriptor.Fields().Len())
	}

	// Indexes split into type-specific vs multi-type so operators can see
	// which ones are shared with other record types at a glance.
	own := rt.GetIndexes()
	multi := rt.GetMultiTypeIndexes()
	if len(own) > 0 {
		fmt.Fprintln(&b, "Indexes:")
		for _, idx := range own {
			fmt.Fprintf(&b, "  %s (%s on %s)\n",
				idx.Name, idx.Type, strings.Join(idx.RootExpression.FieldNames(), ","))
		}
	}
	if len(multi) > 0 {
		fmt.Fprintln(&b, "Multi-type indexes:")
		for _, idx := range multi {
			fmt.Fprintf(&b, "  %s (%s on %s)\n",
				idx.Name, idx.Type, strings.Join(idx.RootExpression.FieldNames(), ","))
		}
	}
	if len(own) == 0 && len(multi) == 0 {
		fmt.Fprintln(&b, "Indexes:                (none)")
	}

	// Universal indexes aren't attached to a specific type but apply
	// across all records; show them for completeness.
	univ := md.GetUniversalIndexes()
	if len(univ) > 0 {
		fmt.Fprintln(&b, "Universal indexes:")
		for _, idx := range univ {
			fmt.Fprintf(&b, "  %s (%s on %s)\n",
				idx.Name, idx.Type, strings.Join(idx.RootExpression.FieldNames(), ","))
		}
	}

	_, err := out.Write([]byte(b.String()))
	return err
}
