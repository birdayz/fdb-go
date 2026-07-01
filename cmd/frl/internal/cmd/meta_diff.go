package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"

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
// summary for additions ("pk: order_id"); Changes carries the per-field
// old→new deltas for modifications. Both renderers consume the same
// Changes list, so the JSON can never under-report what the text shows
// (the old name-only JSON contract hid WHAT changed from jq consumers).
type diffEntry struct {
	Name    string
	Detail  string        // additions only; empty otherwise
	Changes []fieldChange // modifications only; empty otherwise
}

// fieldChange is one attribute delta on a changed entity. Field names
// are stable snake_case identifiers (type, fields, options, predicate,
// added_version, last_modified_version, primary_key, since_version,
// record_type_key) so scripts can select on them.
type fieldChange struct {
	Field string `json:"field"`
	Old   string `json:"old"`
	New   string `json:"new"`
}

// changesDetail renders a Changes list as the human text detail:
// "type VALUE -> COUNT; options (none) -> unique=true".
func changesDetail(changes []fieldChange) string {
	parts := make([]string, len(changes))
	for i, c := range changes {
		parts[i] = fmt.Sprintf("%s %s -> %s", c.Field, c.Old, c.New)
	}
	return strings.Join(parts, "; ")
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
		if detail := changesDetail(e.Changes); detail != "" {
			fmt.Fprintf(b, "  ~ %s: %s\n", e.Name, detail)
		} else {
			fmt.Fprintf(b, "  ~ %s\n", e.Name)
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
		var changes []fieldChange
		if oldPK, newPK := pkFieldsOrUnset(oldT.PrimaryKey), pkFieldsOrUnset(newT.PrimaryKey); oldPK != newPK {
			changes = append(changes, fieldChange{Field: "primary_key", Old: oldPK, New: newPK})
		}
		if oldT.SinceVersion != newT.SinceVersion {
			changes = append(changes, fieldChange{
				Field: "since_version",
				Old:   fmt.Sprintf("%d", oldT.SinceVersion),
				New:   fmt.Sprintf("%d", newT.SinceVersion),
			})
		}
		if oldKey, newKey := recordTypeKeyString(oldT), recordTypeKeyString(newT); oldKey != newKey {
			changes = append(changes, fieldChange{Field: "record_type_key", Old: oldKey, New: newKey})
		}
		if len(changes) > 0 {
			s.Changed = append(s.Changed, diffEntry{Name: name, Changes: changes})
		}
	}
	sortSection(&s)
	return s
}

// recordTypeKeyString renders a record type's key for diff purposes.
// The effective key matters for wire layout, so implicit vs explicit is
// only visible when the value itself differs.
func recordTypeKeyString(rt *recordlayer.RecordType) string {
	return fmt.Sprintf("%v", rt.GetRecordTypeKey())
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
		var changes []fieldChange
		if oldI.Type != newI.Type {
			changes = append(changes, fieldChange{Field: "type", Old: oldI.Type, New: newI.Type})
		}
		oldFields := strings.Join(oldI.RootExpression.FieldNames(), ",")
		newFields := strings.Join(newI.RootExpression.FieldNames(), ",")
		if oldFields != newFields {
			changes = append(changes, fieldChange{Field: "fields", Old: oldFields, New: newFields})
		}
		// Options carry uniqueness ("unique"), allowed-for-query, and
		// every other index knob — a flipped unique with same type+fields
		// used to diff as identical, which is dangerous for a tool whose
		// job is deploy-time sanity (a unique flip changes write behavior).
		if oldOpts, newOpts := optionsString(oldI), optionsString(newI); oldOpts != newOpts {
			changes = append(changes, fieldChange{Field: "options", Old: oldOpts, New: newOpts})
		}
		// Predicate (sparse/filtered index): compare the wire proto, not
		// the Go closure — the proto is what other readers see.
		if !proto.Equal(oldI.GetPredicateProto(), newI.GetPredicateProto()) {
			changes = append(changes, fieldChange{
				Field: "predicate",
				Old:   predicateString(oldI),
				New:   predicateString(newI),
			})
		}
		if oldI.AddedVersion != newI.AddedVersion {
			changes = append(changes, fieldChange{
				Field: "added_version",
				Old:   fmt.Sprintf("%d", oldI.AddedVersion),
				New:   fmt.Sprintf("%d", newI.AddedVersion),
			})
		}
		if oldI.LastModifiedVersion != newI.LastModifiedVersion {
			changes = append(changes, fieldChange{
				Field: "last_modified_version",
				Old:   fmt.Sprintf("%d", oldI.LastModifiedVersion),
				New:   fmt.Sprintf("%d", newI.LastModifiedVersion),
			})
		}
		if len(changes) > 0 {
			s.Changed = append(s.Changed, diffEntry{Name: name, Changes: changes})
		}
	}
	sortSection(&s)
	return s
}

// optionsString renders an index's options map as sorted k=v pairs —
// deterministic across runs so diffs are stable. "(none)" when empty.
func optionsString(idx *recordlayer.Index) string {
	if len(idx.Options) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(idx.Options))
	for k := range idx.Options {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + idx.Options[k]
	}
	return strings.Join(parts, ",")
}

// predicateString renders a filtered index's predicate proto compactly.
// "(none)" for unfiltered indexes.
func predicateString(idx *recordlayer.Index) string {
	p := idx.GetPredicateProto()
	if p == nil {
		return "(none)"
	}
	b, err := prototext.MarshalOptions{}.Marshal(p)
	if err != nil {
		return fmt.Sprintf("(unrenderable: %v)", err)
	}
	return string(b)
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

// sectionJSON's element shapes: added carries {name, detail} (detail is
// the same human summary the text renderer shows), removed is name-only
// (nothing else to say about an entity that's gone), changed carries the
// full per-field old→new list — symmetric with the text output so jq
// consumers can see WHAT changed, not just that something did.
type sectionJSON struct {
	Added   []addedEntryJSON   `json:"added"`
	Removed []string           `json:"removed"`
	Changed []changedEntryJSON `json:"changed"`
}

type addedEntryJSON struct {
	Name   string `json:"name"`
	Detail string `json:"detail,omitempty"`
}

type changedEntryJSON struct {
	Name    string        `json:"name"`
	Changes []fieldChange `json:"changes"`
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

// toSectionJSON converts a diffSection to its JSON shape. Arrays are
// always non-nil so `jq '.indexes.added | length'` is null-safe.
func toSectionJSON(s diffSection) sectionJSON {
	out := sectionJSON{
		Added:   []addedEntryJSON{},
		Removed: []string{},
		Changed: []changedEntryJSON{},
	}
	for _, e := range s.Added {
		out.Added = append(out.Added, addedEntryJSON{Name: e.Name, Detail: e.Detail})
	}
	for _, e := range s.Removed {
		out.Removed = append(out.Removed, e.Name)
	}
	for _, e := range s.Changed {
		out.Changed = append(out.Changed, changedEntryJSON{Name: e.Name, Changes: e.Changes})
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
