package values

import (
	"sort"
)

// qovRecordLayout is a private immutable snapshot of physical source-window
// metadata that accompanied a QOV's flowed record type. It deliberately lives
// beside, not inside, exactType: RecordType.Legs is execution layout and must
// not enter semantic type equality/hash or leak through QOV.Type().
type qovRecordLayout struct {
	legs   []RecordTypeLeg
	fields []*qovRecordLayout
}

func snapshotQOVRecordLayout(typ Type) *qovRecordLayout {
	record, ok := typ.(*RecordType)
	if !ok || record == nil {
		return nil
	}
	return snapshotQOVRecordLayoutOf(record, make(map[*RecordType]bool))
}

func snapshotQOVRecordLayoutOf(record *RecordType, active map[*RecordType]bool) *qovRecordLayout {
	if record == nil || active[record] {
		return nil
	}
	active[record] = true
	defer delete(active, record)

	layout := &qovRecordLayout{
		legs:   append([]RecordTypeLeg(nil), record.Legs...),
		fields: make([]*qovRecordLayout, len(record.Fields)),
	}
	for i := range record.Fields {
		if nested, ok := record.Fields[i].FieldType.(*RecordType); ok {
			layout.fields[i] = snapshotQOVRecordLayoutOf(nested, active)
		}
	}
	if len(layout.legs) == 0 {
		hasNested := false
		for i := range layout.fields {
			hasNested = hasNested || layout.fields[i] != nil
		}
		if !hasNested {
			return nil
		}
	}
	return layout
}

// physicalFlowedRecordType returns a fresh semantic type copy with the private
// physical source layout restored for purpose-specific migration code. It is
// never returned by QOV.Type/FlowedType and therefore cannot make semantic
// identity depend on layout.
func (q *quantifiedObjectValue) physicalFlowedRecordType() *RecordType {
	if q == nil || q.flowed == nil || q.flowed.code != TypeCodeRecord || q.flowed.anyRecord {
		return nil
	}
	record, _ := q.flowed.thaw().(*RecordType)
	restoreQOVRecordLayout(record, q.sourceLayout)
	return record
}

// PhysicalFlowedRecordTypeOf is physicalFlowedRecordType for a caller OUTSIDE
// this package that is about to RE-MINT a QOV from an existing one, and it
// exists because that re-mint is where the physical layout is otherwise lost.
//
// FlowedType withholds .Legs on purpose — layout must not reach the semantic
// surface, and TestQOVSourceLayoutIsImmutableNonSemanticAndBecomesPhysicalWindows
// pins that. But NewQuantifiedObjectValue snapshots its source layout FROM THE
// TYPE IT IS GIVEN, so re-minting through the public type yields a QOV whose
// layout is silently empty. A merged row then reports no leg boundaries, the
// window derivation emits ONE run spanning the whole concat keyed by the box's
// rightmost leaf (the sourceBinding convention), and an alias-qualified read
// resolves at runOffset+ordinal — inside the FIRST leg. Measured on
// `FOA FULL OUTER FOB`: `FOB.K` read FOA's K, inverting EXISTS and NOT EXISTS.
//
// Returns nil when the value is not a record-typed QOV, so a caller keeps the
// type it already had rather than substituting one.
//
// This is a PHYSICAL accessor, never a semantic one: exact-type identity does
// not consider .Legs, so a type from here compares equal to the same type from
// FlowedType. Use it where the physical row is what is being carried, and
// FlowedType everywhere else.
func PhysicalFlowedRecordTypeOf(v QuantifiedObjectValue) *RecordType {
	q, ok := v.(*quantifiedObjectValue)
	if !ok || q == nil {
		return nil
	}
	return q.physicalFlowedRecordType()
}

// PhysicalCarrierType is the row an OrdinalLayout's carrier describes, WITH its
// leg boundaries — the type to use whenever the PHYSICAL row is what is being
// carried, which is every use of a layout's carrier.
//
// It replaces the `layout.Carrier().FlowedType()` idiom. That spelling reaches
// through a physical object for the SEMANTIC type, which withholds boundaries
// on purpose, and the loss is invisible at the call site: the type is the right
// width with the right fields, and only a later qualified read discovers that
// the row no longer says which source owns which slots. Since
// NewQuantifiedObjectValue snapshots its layout from the type it is handed, the
// idiom silently launders layout out of every value re-minted through it.
//
// Comparisons are unaffected — exact-type identity and RecordType.Equals both
// ignore .Legs — so this is safe wherever the old spelling was used to compare
// as well as where it was used to re-mint.
func PhysicalCarrierType(layout OrdinalLayout) Type {
	if layout == nil {
		return nil
	}
	carrier := layout.Carrier()
	if carrier == nil {
		return nil
	}
	if record := PhysicalFlowedRecordTypeOf(carrier); record != nil {
		return record
	}
	return carrier.FlowedType()
}

// LayoutWithSeedLegs returns layout with its carrier stating the leg boundaries
// the RESULT VALUE knows, for the case where the layout itself cannot know them.
//
// A layout derives boundaries from its own source windows, which works whenever
// it has one window per leg. It does not work for the shape that matters most: a
// merged box row published as ONE window covering the whole concat. There the
// only description of which source owns which slots lives in the seed
// RecordConstructor, and a carrier built without it flows a row whose qualified
// reads resolve into the first leg. So the two halves are joined at the one
// point that holds both — the plan's own admission of (result value, layout).
//
// Layouts that already state boundaries are returned untouched, as is any layout
// whose seed cannot state them exactly, so this only ever ADDS physical
// information. Identity is unaffected: legs are not part of exact-type identity,
// and the layout's alias-free hash is carried over unchanged because nothing
// hashable changed.
func LayoutWithSeedLegs(layout OrdinalLayout, resultValue Value) OrdinalLayout {
	concrete, isConcrete := layout.(*ordinalLayout)
	if !isConcrete || concrete == nil || concrete.carrier == nil {
		return layout
	}
	if concrete.carrier.sourceLayout != nil && len(concrete.carrier.sourceLayout.legs) > 0 {
		return layout
	}
	record, isRecord := concrete.carrier.flowed.thaw().(*RecordType)
	if !isRecord || record == nil {
		return layout
	}
	legs := SeedTilingLegs(resultValue, len(record.Fields))
	if legs == nil {
		return layout
	}
	nested := make([]*qovRecordLayout, len(record.Fields))
	if concrete.carrier.sourceLayout != nil {
		copy(nested, concrete.carrier.sourceLayout.fields)
	}
	carrier := *concrete.carrier
	carrier.sourceLayout = &qovRecordLayout{legs: legs, fields: nested}
	withLegs := *concrete
	withLegs.carrier = &carrier
	return &withLegs
}

// carrierWithWindowLegs returns carrier with the leg boundaries its layout's
// source windows state, as a COPY — the input QOV may be shared, and a layout
// snapshot must never mutate a value someone else already holds.
//
// Only the shape this can describe exactly is expressed: a window whose columns
// are top-level slots of the carrier, contiguous and ascending. Those TILE the
// row, which is what a leg table means. A window that addresses a nested object,
// or one whose columns are scattered, is left out entirely rather than
// approximated — a leg table that is only probably right is worse than none,
// because the readers trust it to resolve a qualified column to a slot.
//
// Returns the carrier unchanged when fewer than two legs can be stated: a
// single-leg row needs no boundaries (every column belongs to the one source),
// and stating one leg would claim a structure the row does not have.
func carrierWithWindowLegs(carrier *quantifiedObjectValue, windows []ordinalWindow) *quantifiedObjectValue {
	if carrier == nil || len(windows) < 2 {
		return carrier
	}
	legs := make([]RecordTypeLeg, 0, len(windows))
	for i := range windows {
		w := &windows[i]
		if w.source == nil {
			return carrier
		}
		// A NESTED source occupies exactly ONE slot of this row, holding its whole
		// leg row. That is a leg the table can state — LegKindNested, width 1 — and
		// stating it matters as much as a flat run: a mixed row (one nested leg
		// beside flat ones) would otherwise yield no boundaries at all, which is
		// the collapsed-run case again.
		if w.objectPath != nil {
			if len(w.objectPath) != 1 || w.objectPath[0] < 0 {
				return carrier
			}
			legs = append(legs, NewRecordTypeLeg(
				LegKindNested, w.source.correlation, w.source.correlation.Name(),
				w.objectPath[0], 1))
			continue
		}
		if len(w.fieldPaths) == 0 {
			return carrier
		}
		start := -1
		for k, path := range w.fieldPaths {
			if len(path) != 1 {
				return carrier // not a top-level slot of this row
			}
			if k == 0 {
				start = path[0]
				continue
			}
			if path[0] != start+k {
				return carrier // not a contiguous run
			}
		}
		if start < 0 {
			return carrier
		}
		legs = append(legs, NewRecordTypeLeg(
			LegKindFlatRun, w.source.correlation, w.source.correlation.Name(),
			start, len(w.fieldPaths)))
	}
	sortRecordTypeLegsByStart(legs)
	for i := 1; i < len(legs); i++ {
		if legs[i].Start < legs[i-1].Start+legs[i-1].Width {
			return carrier // overlapping windows are not a leg table
		}
	}
	withLegs := *carrier
	fieldCount := 0
	if record, isRecord := carrier.flowed.thaw().(*RecordType); isRecord {
		fieldCount = len(record.Fields)
	}
	nested := make([]*qovRecordLayout, fieldCount)
	if carrier.sourceLayout != nil {
		copy(nested, carrier.sourceLayout.fields)
	}
	withLegs.sourceLayout = &qovRecordLayout{legs: legs, fields: nested}
	return &withLegs
}

// SeedTilingLegs recovers the leg boundaries a seed RecordConstructor states,
// for a producer that derives a row TYPE from a result VALUE.
//
// The two describe the same row and only one of them carries boundaries: the
// type is a flat field list, while the seed says which source owns which slots.
// A producer that keeps only the type hands downstream a row that has forgotten
// where its legs start — and that reads not as "no legs" but as ONE run over the
// whole concat, keyed by the box's rightmost leaf, so an alias-qualified column
// resolves inside the FIRST leg.
//
// The windows map is not itself a tiling: finalization both adds per-leaf
// sub-windows beside a box run and replaces a run with a narrower one, so
// entries can overlap (a `C$BOX` run at 0 and its first leaf also at 0). The
// tiling is recovered by taking, at each cursor position, the NARROWEST window
// starting exactly there — leaves tile, their enclosing run does not.
//
// Returns nil unless the result tiles `width` exactly with at least two legs, so
// a shape this cannot describe keeps today's behaviour rather than being given
// boundaries that are only probably right.
func SeedTilingLegs(rv Value, width int) []RecordTypeLeg {
	rc, isRC := rv.(*RecordConstructorValue)
	if !isRC || width <= 0 {
		return nil
	}
	windows, _ := OrdinalSeedLegWindows(rc)
	if len(windows) < 2 {
		return nil
	}
	type candidate struct {
		alias CorrelationIdentifier
		start int
		width int
	}
	candidates := make([]candidate, 0, len(windows))
	for alias, w := range windows {
		if w.Kind != LegKindFlatRun || w.Typ == nil || w.Offset < 0 {
			return nil
		}
		candidates = append(candidates, candidate{alias, w.Offset, len(w.Typ.Fields)})
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].start != candidates[j].start {
			return candidates[i].start < candidates[j].start
		}
		return candidates[i].width < candidates[j].width
	})
	var legs []RecordTypeLeg
	cursor := 0
	for _, c := range candidates {
		if c.start != cursor || c.width <= 0 {
			continue
		}
		legs = append(legs, NewRecordTypeLeg(
			LegKindFlatRun, c.alias, c.alias.Name(), c.start, c.width))
		cursor += c.width
	}
	if cursor != width || len(legs) < 2 {
		return nil
	}
	return legs
}

// WithSeedTilingLegs returns typ carrying the boundaries rv states, or typ
// unchanged when it already has them or the seed cannot state them exactly.
func WithSeedTilingLegs(typ Type, rv Value) Type {
	record, isRecord := typ.(*RecordType)
	if !isRecord || record == nil || len(record.Legs) > 0 {
		return typ
	}
	legs := SeedTilingLegs(rv, len(record.Fields))
	if legs == nil {
		return typ
	}
	withLegs := *record
	withLegs.Legs = legs
	return &withLegs
}

func sortRecordTypeLegsByStart(legs []RecordTypeLeg) {
	for i := 1; i < len(legs); i++ {
		for j := i; j > 0 && legs[j].Start < legs[j-1].Start; j-- {
			legs[j], legs[j-1] = legs[j-1], legs[j]
		}
	}
}

func restoreQOVRecordLayout(record *RecordType, layout *qovRecordLayout) {
	if record == nil || layout == nil {
		return
	}
	record.Legs = append([]RecordTypeLeg(nil), layout.legs...)
	for i := range record.Fields {
		if i >= len(layout.fields) || layout.fields[i] == nil {
			continue
		}
		if nested, ok := record.Fields[i].FieldType.(*RecordType); ok {
			restoreQOVRecordLayout(nested, layout.fields[i])
		}
	}
}
