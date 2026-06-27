package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"fdb.dev/cmd/frl/internal/meta"
	"fdb.dev/pkg/recordlayer"
)

func newMetaDiffCmd() *cobra.Command {
	var outputFmt string
	c := &cobra.Command{
		Use:   "diff <old.pb> <new.pb>",
		Short: "Human-readable diff of two metadata files",
		Example: `  frl meta diff old.pb new.pb
  frl meta diff old.pb new.pb -o json | jq '.indexes.added'`,
		Long: "Compares two MetaData.pb files and reports added / removed / " +
			"changed record types and indexes. Intended for PR reviews and " +
			"deploy-time sanity checks; pair with `meta evolve-check` in CI " +
			"to catch incompatible evolutions before saveRecordMetaData().\n\n" +
			"--output / -o: 'text' (default) or 'json' (structured object " +
			"with version, record_types.{added,removed,changed}, and " +
			"indexes.{added,removed,changed}).",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateOutputFormat(outputFmt, "text", "json"); err != nil {
				return err
			}
			oldMeta, err := (&meta.FileSource{Path: args[0]}).Load(cmd.Context())
			if err != nil {
				return fmt.Errorf("load old: %w", err)
			}
			newMeta, err := (&meta.FileSource{Path: args[1]}).Load(cmd.Context())
			if err != nil {
				return fmt.Errorf("load new: %w", err)
			}
			if outputFmt == "json" {
				return writeMetaDiffJSON(cmd.OutOrStdout(), oldMeta, newMeta)
			}
			return writeMetaDiff(cmd.OutOrStdout(), oldMeta, newMeta)
		},
	}
	c.Flags().StringVarP(&outputFmt, "output", "o", "text", "output format: text or json")
	return c
}

// diffEntry is the structured unit of change — shared by both text and
// JSON renderers so categories can't drift between the two outputs.
// Name is the entity (record type, index); Detail is a human-readable
// explanation (e.g. "pk: order_id" for additions, "pk changed (old -> new)"
// for modifications). Text-only renderers can render Name + Detail; JSON
// keeps a stable name-only contract on each bucket.
type diffEntry struct {
	Name   string
	Detail string // empty for bare removals
}

// diffSection buckets changes of a given kind (record_types / indexes).
type diffSection struct {
	Added   []diffEntry
	Removed []diffEntry
	Changed []diffEntry
}

// nonEmpty returns true iff the section has any entries across buckets.
func (s diffSection) nonEmpty() bool {
	return len(s.Added) > 0 || len(s.Removed) > 0 || len(s.Changed) > 0
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

	types := diffRecordTypes(oldMeta, newMeta)
	if types.nonEmpty() {
		b.WriteString("RECORD TYPES:\n")
		writeSectionText(&b, types, func(e diffEntry) string {
			if e.Detail == "" {
				return e.Name
			}
			return e.Name + " (" + e.Detail + ")"
		})
		anyChange = true
	}

	indexes := diffIndexes(oldMeta, newMeta)
	if indexes.nonEmpty() {
		b.WriteString("INDEXES:\n")
		writeSectionText(&b, indexes, func(e diffEntry) string {
			if e.Detail == "" {
				return e.Name
			}
			return e.Name + " (" + e.Detail + ")"
		})
		anyChange = true
	}

	if !anyChange {
		b.WriteString("(metadata is identical)\n")
	}
	_, err := out.Write([]byte(b.String()))
	return err
}

// writeSectionText emits the +/-/~ lines for a single diff section.
// addedFmt wraps "name (detail)"; changed entries always render as
// "name: detail" (detail carries the "x changed (a -> b)" description).
func writeSectionText(b *strings.Builder, s diffSection, addedFmt func(diffEntry) string) {
	for _, e := range s.Added {
		fmt.Fprintf(b, "  + %s\n", addedFmt(e))
	}
	for _, e := range s.Removed {
		fmt.Fprintf(b, "  - %s\n", e.Name)
	}
	for _, e := range s.Changed {
		if e.Detail == "" {
			fmt.Fprintf(b, "  ~ %s\n", e.Name)
		} else {
			fmt.Fprintf(b, "  ~ %s: %s\n", e.Name, e.Detail)
		}
	}
}

// diffRecordTypes buckets record-type differences into added / removed /
// changed (PK expression). Each bucket is sorted by name so diffs across
// invocations stay stable. Shared by both text and JSON renderers.
func diffRecordTypes(oldMeta, newMeta *recordlayer.RecordMetaData) diffSection {
	oldTypes := oldMeta.RecordTypes()
	newTypes := newMeta.RecordTypes()
	var s diffSection

	for name := range newTypes {
		if _, ok := oldTypes[name]; !ok {
			s.Added = append(s.Added, diffEntry{
				Name:   name,
				Detail: "pk: " + pkFieldsOrUnset(newTypes[name].PrimaryKey),
			})
		}
	}
	for name := range oldTypes {
		if _, ok := newTypes[name]; !ok {
			s.Removed = append(s.Removed, diffEntry{Name: name})
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
			s.Changed = append(s.Changed, diffEntry{
				Name:   name,
				Detail: fmt.Sprintf("pk changed (%s -> %s)", oldPK, newPK),
			})
		}
	}
	sortSection(&s)
	return s
}

// diffIndexes buckets index differences. Tracks addition, removal, and
// modifications to index type or expression fields.
func diffIndexes(oldMeta, newMeta *recordlayer.RecordMetaData) diffSection {
	oldIdx := oldMeta.GetAllIndexes()
	newIdx := newMeta.GetAllIndexes()
	var s diffSection

	for name, idx := range newIdx {
		if _, ok := oldIdx[name]; !ok {
			s.Added = append(s.Added, diffEntry{
				Name: name,
				Detail: fmt.Sprintf("%s on %s",
					idx.Type, strings.Join(idx.RootExpression.FieldNames(), ",")),
			})
		}
	}
	for name := range oldIdx {
		if _, ok := newIdx[name]; !ok {
			s.Removed = append(s.Removed, diffEntry{Name: name})
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
			s.Changed = append(s.Changed, diffEntry{
				Name:   name,
				Detail: strings.Join(deltas, "; "),
			})
		}
	}
	sortSection(&s)
	return s
}

func sortSection(s *diffSection) {
	sort.Slice(s.Added, func(i, j int) bool { return s.Added[i].Name < s.Added[j].Name })
	sort.Slice(s.Removed, func(i, j int) bool { return s.Removed[i].Name < s.Removed[j].Name })
	sort.Slice(s.Changed, func(i, j int) bool { return s.Changed[i].Name < s.Changed[j].Name })
}

// --- JSON diff ---

// metaDiffJSON is the structured-output shape for `meta diff -o json`.
// Contract: every section is present with empty arrays (not nil) so
// `jq '.indexes.added | length'` is null-safe.
type metaDiffJSON struct {
	Version     *versionChangeJSON `json:"version,omitempty"`
	RecordTypes sectionJSON        `json:"record_types"`
	Indexes     sectionJSON        `json:"indexes"`
}

type versionChangeJSON struct {
	Old int `json:"old"`
	New int `json:"new"`
}

type sectionJSON struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
	Changed []string `json:"changed"`
}

func writeMetaDiffJSON(out io.Writer, oldMeta, newMeta *recordlayer.RecordMetaData) error {
	d := metaDiffJSON{
		RecordTypes: toSectionJSON(diffRecordTypes(oldMeta, newMeta)),
		Indexes:     toSectionJSON(diffIndexes(oldMeta, newMeta)),
	}
	if oldMeta.Version() != newMeta.Version() {
		d.Version = &versionChangeJSON{Old: oldMeta.Version(), New: newMeta.Version()}
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(d)
}

// toSectionJSON extracts just the names out of a diffSection — the JSON
// contract is name-only per bucket; `jq` consumers read names then
// correlate back via their own data. Details are text-only (the text
// renderer shows them inline).
func toSectionJSON(s diffSection) sectionJSON {
	out := sectionJSON{Added: []string{}, Removed: []string{}, Changed: []string{}}
	for _, e := range s.Added {
		out.Added = append(out.Added, e.Name)
	}
	for _, e := range s.Removed {
		out.Removed = append(out.Removed, e.Name)
	}
	for _, e := range s.Changed {
		out.Changed = append(out.Changed, e.Name)
	}
	return out
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
