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

func newMetaTypesDescribeCmd() *cobra.Command {
	var contextName, metaFile string
	c := &cobra.Command{
		Use:   "describe <name>",
		Short: "Show full definition of one record type",
		Long: "Prints the record type's primary key, record-type key (for " +
			"multi-type stores that use RecordTypeKeyExpression), since-" +
			"version, proto field count, and every index that touches this " +
			"type (including universal/multi-type ones).\n\n" +
			"Note: FDB-store metadata sources are not yet supported by " +
			"this command; configure `meta_file` in your context or use " +
			"--meta-file.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgCtx, override, err := resolveContextAndOverride(contextName, metaFile)
			if err != nil {
				return err
			}
			if override == nil {
				src, ferr := meta.FromContext(cfgCtx, nil, nil)
				if ferr != nil {
					if errors.Is(ferr, meta.ErrMissingSource) {
						return fmt.Errorf("%w (context %q)", ferr, cfgCtx.GetName())
					}
					return ferr
				}
				override = src
			}
			md, err := override.Load(cmd.Context())
			if err != nil {
				return err
			}
			rt := md.GetRecordType(args[0])
			if rt == nil {
				return fmt.Errorf("record type %q not found — available: %s",
					args[0], strings.Join(sortedRecordTypeNames(md), ", "))
			}
			return writeRecordTypeDescription(cmd.OutOrStdout(), md, rt)
		},
	}
	c.Flags().StringVar(&contextName, "context", "", "context name to use")
	c.Flags().StringVar(&metaFile, "meta-file", "", "path to MetaData.pb; overrides context.metadata")
	return c
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
	pk := "(unset)"
	if rt.PrimaryKey != nil {
		if fields := rt.PrimaryKey.FieldNames(); len(fields) > 0 {
			pk = strings.Join(fields, ",")
		}
	}
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
