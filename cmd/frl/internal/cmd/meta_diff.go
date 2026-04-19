package cmd

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/birdayz/fdb-record-layer-go/cmd/frl/internal/meta"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

func newMetaDiffCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "diff <old.pb> <new.pb>",
		Short: "Human-readable diff of two metadata files",
		Long: "Compares two MetaData.pb files and reports added / removed / " +
			"changed record types and indexes. Intended for PR reviews and " +
			"deploy-time sanity checks; pair with `meta evolve-check` in CI " +
			"to catch incompatible evolutions before saveRecordMetaData().",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			oldMeta, err := (&meta.FileSource{Path: args[0]}).Load(cmd.Context())
			if err != nil {
				return fmt.Errorf("load old: %w", err)
			}
			newMeta, err := (&meta.FileSource{Path: args[1]}).Load(cmd.Context())
			if err != nil {
				return fmt.Errorf("load new: %w", err)
			}
			return writeMetaDiff(cmd.OutOrStdout(), oldMeta, newMeta)
		},
	}
	return c
}

// writeMetaDiff renders a compact diff of two RecordMetaData snapshots.
// Output shape (stable, scriptable):
//
//	VERSION: 1 -> 2
//	RECORD TYPES:
//	  + NewType (pk: newfield)
//	  - OldType
//	  ~ Order: pk changed (order_id -> order_id,customer_id)
//	INDEXES:
//	  + Order$new_idx (VALUE on price)
//	  - Order$old_idx
//	  ~ Order$price: type changed (VALUE -> COUNT)
//
// A section is omitted when empty. If nothing changed, prints
// "(metadata is identical)" so scripts can branch on non-empty output.
func writeMetaDiff(out io.Writer, oldMeta, newMeta *recordlayer.RecordMetaData) error {
	var b strings.Builder
	anyChange := false

	if oldMeta.Version() != newMeta.Version() {
		fmt.Fprintf(&b, "VERSION: %d -> %d\n", oldMeta.Version(), newMeta.Version())
		anyChange = true
	}

	typeLines := diffRecordTypes(oldMeta, newMeta)
	if len(typeLines) > 0 {
		b.WriteString("RECORD TYPES:\n")
		for _, line := range typeLines {
			fmt.Fprintf(&b, "  %s\n", line)
		}
		anyChange = true
	}

	indexLines := diffIndexes(oldMeta, newMeta)
	if len(indexLines) > 0 {
		b.WriteString("INDEXES:\n")
		for _, line := range indexLines {
			fmt.Fprintf(&b, "  %s\n", line)
		}
		anyChange = true
	}

	if !anyChange {
		b.WriteString("(metadata is identical)\n")
	}
	_, err := out.Write([]byte(b.String()))
	return err
}

// diffRecordTypes returns sorted +/-/~ lines for record type differences.
// Changes tracked: addition, removal, PK expression change.
func diffRecordTypes(oldMeta, newMeta *recordlayer.RecordMetaData) []string {
	oldTypes := oldMeta.RecordTypes()
	newTypes := newMeta.RecordTypes()
	var lines []string

	for name := range newTypes {
		if _, ok := oldTypes[name]; !ok {
			pk := pkFieldsOrUnset(newTypes[name].PrimaryKey)
			lines = append(lines, fmt.Sprintf("+ %s (pk: %s)", name, pk))
		}
	}
	for name := range oldTypes {
		if _, ok := newTypes[name]; !ok {
			lines = append(lines, fmt.Sprintf("- %s", name))
		}
	}
	for name, oldT := range oldTypes {
		newT, ok := newTypes[name]
		if !ok {
			continue
		}
		oldPK := pkFieldsOrUnset(oldT.PrimaryKey)
		newPK := pkFieldsOrUnset(newT.PrimaryKey)
		if oldPK != newPK {
			lines = append(lines, fmt.Sprintf("~ %s: pk changed (%s -> %s)", name, oldPK, newPK))
		}
	}
	sort.Strings(lines)
	return lines
}

// diffIndexes returns sorted +/-/~ lines. Changes tracked: addition,
// removal, type change, expression-fields change.
func diffIndexes(oldMeta, newMeta *recordlayer.RecordMetaData) []string {
	oldIdx := oldMeta.GetAllIndexes()
	newIdx := newMeta.GetAllIndexes()
	var lines []string

	for name, idx := range newIdx {
		if _, ok := oldIdx[name]; !ok {
			lines = append(lines, fmt.Sprintf("+ %s (%s on %s)",
				name, idx.Type, strings.Join(idx.RootExpression.FieldNames(), ",")))
		}
	}
	for name := range oldIdx {
		if _, ok := newIdx[name]; !ok {
			lines = append(lines, fmt.Sprintf("- %s", name))
		}
	}
	for name, oldI := range oldIdx {
		newI, ok := newIdx[name]
		if !ok {
			continue
		}
		var deltas []string
		if oldI.Type != newI.Type {
			deltas = append(deltas, fmt.Sprintf("type %s -> %s", oldI.Type, newI.Type))
		}
		oldFields := strings.Join(oldI.RootExpression.FieldNames(), ",")
		newFields := strings.Join(newI.RootExpression.FieldNames(), ",")
		if oldFields != newFields {
			deltas = append(deltas, fmt.Sprintf("fields %s -> %s", oldFields, newFields))
		}
		if len(deltas) > 0 {
			lines = append(lines, fmt.Sprintf("~ %s: %s", name, strings.Join(deltas, "; ")))
		}
	}
	sort.Strings(lines)
	return lines
}

// pkFieldsOrUnset returns a human-readable PK representation: comma-joined
// field names, or "(unset)" when the expression is nil or yields no fields.
func pkFieldsOrUnset(ke recordlayer.KeyExpression) string {
	if ke == nil {
		return "(unset)"
	}
	fn := ke.FieldNames()
	if len(fn) == 0 {
		return "(unset)"
	}
	return strings.Join(fn, ",")
}
