package cascades

import (
	"math"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// This file ports the ONE numeric consequence of Java's CardinalitiesProperty
// (properties/cardinality.go, plan_properties.go's computeCardinalities) that
// the concrete join-ordering cost walk (concretePlanCost/combineConcreteCost
// in planning_cost_model.go) was missing: a join's output can never exceed
// the size of the table it probes, and this bound holds REGARDLESS of drive
// order — it is a property of the logical join, not of which side is outer.
//
// Ported CardinalitiesProperty (properties/cardinality.go, already 1:1 with
// Java) proves a per-execution probe is AtMostOne ONLY when every column of a
// UNIQUE index or the full primary key is equality-bound — a plain full scan
// or a NON-UNIQUE secondary-index equality (the FK-fan-out shape every hop of
// TestFDB_MultiwayJoinOrder_Nway's forward direction uses) is
// UnknownMaxCardinality, exactly like Java. That is proven correct by
// re-reading Java's PlanningCostModel.compare (lines 274-307): its ONLY
// join-order criterion evaluates cardinalities().evaluate(outer) — the
// OUTER's OWN max, not a per-hop compounded product — and on this exact
// unfiltered FK-chain shape (the driving root is necessarily an unbound full
// scan in EITHER direction) that evaluates to UnknownMaxCardinality for BOTH
// candidate orderings, so Java's own criterion abstains here too. A faithful
// CardinalitiesProperty port does not, and structurally cannot, discriminate
// this case — it carries no statistics by design (see CardinalitiesProperty's
// Java class: no StatisticsProvider anywhere in it).
//
// So the fix below is a Go-only, statistics-aware EXTENSION beyond
// CardinalitiesProperty (permitted by CLAUDE.md's "read-side query surface
// MAY go beyond Java" — nothing here touches the wire), built on a fact
// CardinalitiesProperty's own reasoning already establishes structurally: a
// plain scan/index access of table T never returns the same physical T row
// twice within one execution. Chain that fact through a left-deep FlatMap
// probe sequence and by induction every hop's bind value is UNIQUE across
// the accumulated outer — which is exactly the soundness precondition a
// naive "probe sum ≤ table size" cap needs (and lacks in general: 1000 outer
// rows sharing one non-PK value, probing a 500-row table for 3 matching rows
// each, gives a true output of 3000 — summed, not capped — because the outer
// is NOT unique on that bind column; see fkChainCardinalityCap's doc comment
// for the precise condition that rules this counter-example out).
//
// pkThread is deliberately narrow: it fires only when EVERY hop of a chain
// binds via the immediately-preceding hop's OWN primary key, read off that
// hop's OWN scan/index leaf (never re-derived, never guessed) — the exact
// shape a foreign-key equi-join chain has. Any other join shape leaves
// pkThread{ok:false} and the existing selectivity-based FlatMapCost estimate
// is used unclamped, unchanged from before this file existed.

// pkThread describes a concrete plan subtree whose output rows are in exact
// 1:1 correspondence with DISTINCT rows of one base table (recordType),
// reached without ever losing that table's own row identity — no join,
// filter, or projection along the way can have introduced a duplicate of any
// single physical row of recordType. pkValues are that table's OWN
// (unaliased, leaf-relative — FieldValue.Child == nil at the root) primary
// key Values, exactly as GetPrimaryKeyValues()/GetCommonPrimaryKeyValues()
// stamp them.
type pkThread struct {
	recordType string
	pkValues   []values.Value
	pkTypes    []values.Type
	ok         bool
}

// computePKThread walks a concrete RecordQueryPlan subtree and proves (or
// fails to prove) that its output is pkThread-identified by some base table.
// Fails closed (ok=false) on anything it does not specifically recognize as
// identity-preserving — under-applying the cap is always safe (the caller
// simply keeps today's selectivity estimate), over-applying it is not, so
// every arm below is a plan shape that PROVABLY cannot introduce, lose track
// of, or duplicate the underlying table's rows.
func computePKThread(p plans.RecordQueryPlan) pkThread {
	switch pl := p.(type) {
	case *plans.RecordQueryScanPlan:
		rts := pl.GetRecordTypes()
		pk := pl.GetPrimaryKeyValues()
		if len(rts) != 1 || len(pk) == 0 {
			return pkThread{}
		}
		pkTypes, aligned := alignPKTypesByPosition(pk, pl.GetKeyComponentTypes())
		if !aligned {
			return pkThread{}
		}
		return pkThread{recordType: rts[0], pkValues: pk, pkTypes: pkTypes, ok: true}

	case *plans.RecordQueryIndexPlan:
		rts := pl.GetRecordTypes()
		pk := pl.GetCommonPrimaryKeyValues()
		if len(rts) != 1 || len(pk) == 0 {
			return pkThread{}
		}
		// A fan-out index (ProducesDistinctRecords()==false — either it is
		// KNOWN to createDuplicates, or the signal was never stamped at all,
		// Java's own empty-candidate default) can emit the SAME physical
		// record/PK more than once. The partition argument this whole file
		// rests on — each row of the probed table matches at most one outer
		// row — needs the leaf's OWN rows to be distinct in the first place;
		// a duplicating leaf breaks that before the thread even starts, so
		// fail closed rather than assume a scalar index.
		if !pl.ProducesDistinctRecords() {
			return pkThread{}
		}
		pkTypes, aligned := alignIndexPKTypesByCoordinate(
			pk, len(pl.GetPKColumnNames()), pl.GetPrimaryKeyComponentTypes(),
		)
		if !aligned {
			return pkThread{}
		}
		return pkThread{recordType: rts[0], pkValues: pk, pkTypes: pkTypes, ok: true}

	case *plans.RecordQueryFetchFromPartialRecordPlan:
		return pkThreadFromSingleChild(pl.GetChildren())
	case *plans.RecordQueryTypeFilterPlan:
		return pkThreadFromSingleChild(pl.GetChildren())
	case *plans.RecordQueryPredicatesFilterPlan:
		return pkThreadFromSingleChild(pl.GetChildren())
	case *plans.RecordQueryFilterPlan:
		return pkThreadFromSingleChild(pl.GetChildren())
	case *plans.RecordQueryMapPlan:
		return computeMapPKThread(pl)
	case *plans.RecordQueryProjectionPlan:
		return computeProjectionPKThread(pl)
	case *plans.RecordQueryInMemorySortPlan:
		return pkThreadFromSingleChild(pl.GetChildren())

	case *plans.RecordQueryFlatMapPlan:
		return computeFlatMapPKThread(pl)

	default:
		return pkThread{}
	}
}

// alignPKTypesByPosition aligns a primary scan's authoritative physical type
// vector with its structural PK values. Producers use one of two explicit
// topologies: a full-coordinate vector (including a RecordTypeValue prefix),
// or a visible-column vector that omits that per-record-type constant. Any
// other arity fails closed rather than silently shifting a later key type onto
// the wrong component.
func alignPKTypesByPosition(pk []values.Value, physicalTypes []values.Type) ([]values.Type, bool) {
	variableCount := 0
	for _, component := range pk {
		if _, isRecordType := component.(*values.RecordTypeValue); !isRecordType {
			variableCount++
		}
	}
	fullCoordinates := len(physicalTypes) == len(pk)
	visibleCoordinates := len(physicalTypes) == variableCount
	if !fullCoordinates && !visibleCoordinates {
		return nil, false
	}
	aligned := make([]values.Type, len(pk))
	visiblePosition := 0
	for i, component := range pk {
		if _, isRecordType := component.(*values.RecordTypeValue); isRecordType {
			aligned[i] = values.UnknownType
			continue
		}
		position := visiblePosition
		if fullCoordinates {
			position = i
		}
		aligned[i] = physicalTypeAt(physicalTypes, position)
		visiblePosition++
	}
	return aligned, true
}

// alignIndexPKTypesByCoordinate aligns an index scan's structural common
// primary-key Values with its authoritative visible PK type vector. Both are
// translations of the same primary-key expression and therefore share
// primary-key component order. That coordinate is the identity: Field is only
// a display spelling and is deliberately never read here. This matters for a
// layout that exposes duplicate display names with distinct baked ordinals;
// a name-keyed map would collapse the two components and could attach a
// FLOAT/DOUBLE proof to the wrong physical key coordinate.
//
// The independently-derived visible column count participates only as an arity
// witness for the type vector; no display name crosses this boundary. A
// leading RecordTypeValue is the one structural coordinate omitted from both
// visible metadata vectors and is preserved as an Unknown placeholder.
// Non-flat or misplaced structural components fail closed.
func alignIndexPKTypesByCoordinate(
	pk []values.Value,
	visiblePKColumnCount int,
	physicalTypes []values.Type,
) ([]values.Type, bool) {
	if visiblePKColumnCount <= 0 || visiblePKColumnCount != len(physicalTypes) {
		return nil, false
	}
	aligned := make([]values.Type, len(pk))
	visiblePosition := 0
	for i, component := range pk {
		if _, isRecordType := component.(*values.RecordTypeValue); isRecordType {
			if i != 0 {
				return nil, false
			}
			aligned[i] = values.UnknownType
			continue
		}
		field, ok := component.(*values.FieldValue)
		if !ok || field.Child != nil {
			return nil, false
		}
		if visiblePosition >= len(physicalTypes) {
			return nil, false
		}
		aligned[i] = physicalTypeAt(physicalTypes, visiblePosition)
		visiblePosition++
	}
	if visiblePosition != len(physicalTypes) {
		return nil, false
	}
	return aligned, true
}

func physicalTypeAt(types []values.Type, position int) values.Type {
	if position < 0 || position >= len(types) || types[position] == nil {
		return values.UnknownType
	}
	return types[position]
}

func pkThreadFromSingleChild(children []plans.RecordQueryPlan) pkThread {
	if len(children) != 1 {
		return pkThread{}
	}
	return computePKThread(children[0])
}

// namedValue pairs an output-row field name with the Value that computes it —
// the normalized shape pkThreadThroughFields needs from either a
// RecordConstructorValue's Fields (RecordQueryMapPlan.resultValue,
// RecordQueryFlatMapPlan.resultValue) or a RecordQueryProjectionPlan's
// parallel projections/aliases slices.
type namedValue struct {
	name  string
	value values.Value
}

// computeMapPKThread extends a proven child pkThread across a
// RecordQueryMapPlan's output-shaping resultValue. A Map can REPLACE, DROP, or
// RENAME the tracked PK field (project a constant as ID, and every outer row
// binds the same value — silently breaking the partition argument the whole
// file rests on), so the child's thread survives only when resultValue
// provably preserves every tracked PK value: see pkThreadThroughResultValue.
func computeMapPKThread(pl *plans.RecordQueryMapPlan) pkThread {
	childThread := pkThreadFromSingleChild(pl.GetChildren())
	return pkThreadThroughResultValue(childThread, pl.GetInnerQuantifier().GetAlias(),
		singleChildRowLayout(pl.GetChildren()), pl.GetResultValue())
}

// computeProjectionPKThread is computeMapPKThread's analogue for
// RecordQueryProjectionPlan, whose output shape is a parallel
// projections/aliases pair rather than a single RecordConstructorValue.
// IsIdentity() (the projection passes every column through unchanged) is the
// same full-passthrough fast path pkThreadThroughResultValue recognizes for a
// bare QuantifiedObjectValue resultValue; the general case defers to
// pkThreadThroughFields with each projection's OWN output-name authority
// (values.OutputColumnName — the exact name executeProjection keys the slot
// under), so the check matches names the same way any downstream reader must.
func computeProjectionPKThread(pl *plans.RecordQueryProjectionPlan) pkThread {
	childThread := pkThreadFromSingleChild(pl.GetChildren())
	if !childThread.ok {
		return pkThread{}
	}
	if len(childThread.pkTypes) != len(childThread.pkValues) {
		return pkThread{}
	}
	if pl.IsIdentity() {
		return childThread
	}
	childAlias := pl.GetInnerQuantifier().GetAlias()
	projections := pl.GetProjections()
	aliases := pl.GetAliases()
	fields := make([]namedValue, len(projections))
	for i, v := range projections {
		alias := ""
		if i < len(aliases) {
			alias = aliases[i]
		}
		fields[i] = namedValue{name: values.OutputColumnName(v, alias), value: v}
	}
	return pkThreadThroughFields(childThread, childAlias, singleChildRowLayout(pl.GetChildren()), fields)
}

// pkThreadThroughResultValue re-roots childThread (a proven pkThread whose
// rows are read under childAlias) through an output-shaping resultValue —
// RecordQueryMapPlan.resultValue or RecordQueryFlatMapPlan.resultValue. Fails
// closed unless resultValue provably preserves every tracked PK value:
//
//   - resultValue IS childAlias's entire row, unchanged (a bare
//     QuantifiedObjectValue over childAlias) — every field, PK included,
//     passes through untouched, so childThread survives verbatim; or
//   - resultValue is a RecordConstructorValue in which every PK field has
//     some named slot that is a DIRECT, uncomputed read of that same field
//     off childAlias (see correlatedFieldOf) — never a renamed-without-
//     tracking, computed, or constant slot.
//
// Anything else (a bare computed Value, a RecordConstructor missing a PK
// field, nil) declines: under-applying the cap is always safe, so this
// function is deliberately conservative rather than clever.
func pkThreadThroughResultValue(
	childThread pkThread,
	childAlias values.CorrelationIdentifier,
	childLayout values.Type,
	resultValue values.Value,
) pkThread {
	if !childThread.ok {
		return pkThread{}
	}
	if qov, ok := resultValue.(*values.QuantifiedObjectValue); ok && qov.Correlation == childAlias {
		return childThread
	}
	rc, ok := resultValue.(*values.RecordConstructorValue)
	if !ok {
		return pkThread{}
	}
	fields := make([]namedValue, len(rc.Fields))
	for i, f := range rc.Fields {
		fields[i] = namedValue{name: f.Name, value: f.Value}
	}
	return pkThreadThroughFields(childThread, childAlias, childLayout, fields)
}

// pkThreadThroughFields is the shared engine behind pkThreadThroughResultValue
// and computeProjectionPKThread: given a proven childThread and a set of
// named output slots computed over childAlias's row, it re-derives childThread
// under the NEW output names — or fails closed the moment any tracked PK
// component cannot be proven to survive.
//
// A values.RecordTypeValue component is carried through UNCHANGED rather than
// matched against a field slot: per innerFullyBindsThread's doc comment it is
// a per-thread CONSTANT (the record type never changes under a Map/Projection
// over the same underlying row), never a discriminating column, so no field
// slot needs to reproduce it for the thread to remain sound.
//
// Every other PK component must be a flat, leaf-relative FieldValue whose name
// resolves in the CHILD's row layout (leafFieldIdentity), and some output slot
// must be a DIRECT reference to that same COLUMN off childAlias
// (correlatedFieldIdentity, matched by identity rather than by spelling) — a
// renamed, computed, or constant slot does not count, and a missing match fails
// the WHOLE thread closed (not just that one component): a partial PK is not a
// PK.
//
// childLayout is the row childAlias binds — the child plan's own output type.
// It is the frontier both sides are stated in, so the tracked PK column and the
// slot reading it are compared as ordinals in ONE layout.
func pkThreadThroughFields(
	childThread pkThread,
	childAlias values.CorrelationIdentifier,
	childLayout values.Type,
	fields []namedValue,
) pkThread {
	if !childThread.ok {
		return pkThread{}
	}
	if len(childThread.pkTypes) != len(childThread.pkValues) {
		return pkThread{}
	}
	frontier := values.OrdinalDomainOfType(childLayout)
	if !frontier.IsKnown() {
		return pkThread{} // no declared column order to state the mapping in
	}
	newPK := make([]values.Value, 0, len(childThread.pkValues))
	newPKTypes := make([]values.Type, 0, len(childThread.pkTypes))
	for i, pv := range childThread.pkValues {
		if _, isRecordType := pv.(*values.RecordTypeValue); isRecordType {
			newPK = append(newPK, pv)
			newPKTypes = append(newPKTypes, childThread.pkTypes[i])
			continue
		}
		wanted, ok := leafFieldIdentity(pv, childLayout)
		if !ok {
			return pkThread{} // a non-flat-FieldValue PK component — fail closed
		}
		outName, found := findDirectFieldMapping(fields, childAlias, wanted.WithCorrelation(childAlias), frontier)
		if !found {
			return pkThread{}
		}
		// The re-rooted component is a name carrier under the OUTPUT name —
		// the slot's own naming authority, which is what the NEXT hop's layout
		// declares. It is resolved against that layout when it is next
		// consulted, never compared as a name.
		newPK = append(newPK, &values.FieldValue{Field: outName})
		newPKTypes = append(newPKTypes, childThread.pkTypes[i])
	}
	return pkThread{
		recordType: childThread.recordType,
		pkValues:   newPK,
		pkTypes:    newPKTypes,
		ok:         true,
	}
}

// findDirectFieldMapping searches fields for a slot that is a DIRECT,
// uncomputed read of the wanted COLUMN off childAlias — i.e.
// correlatedFieldIdentity(v, frontier) == wanted — and returns that slot's own
// output name. Multiple slots may qualify (a column projected twice under
// different names); any one witness is sufficient to prove the PK component
// survives.
//
// The LEG is judged by values.SameLeg and then canonicalized onto the key, so
// the `key != wanted` comparison that follows decides the COLUMN (domain +
// ordinal) and nothing else. Without the leg check the canonicalization would
// re-stamp any slot as childAlias's and a projection of some OTHER leg's
// same-ordinal column would be accepted as the tracked PK component surviving
// — a pkThread carried forward over a row that no longer identifies it.
func findDirectFieldMapping(
	fields []namedValue,
	childAlias values.CorrelationIdentifier,
	wanted values.ColumnIdentity,
	frontier values.OrdinalDomain,
) (string, bool) {
	for _, f := range fields {
		key, ok := correlatedFieldIdentity(f.value, frontier)
		if !ok || !values.SameLeg(key.Correlation, childAlias) {
			continue
		}
		if key.WithCorrelation(childAlias) != wanted {
			continue
		}
		return f.name, true
	}
	return "", false
}

// computeFlatMapPKThread extends a proven outer pkThread across one more
// FlatMap hop: if the inner leg's search fully equality-binds every column of
// the outer's established primary key (correlated to the FlatMap's own outer
// alias — see innerFullyBindsThread), the inner leg's rows are each matched
// by AT MOST one outer row's bind value (equality partitions inner's table by
// that value), so the whole FlatMap's output is a duplicate-free subset of
// inner's own table — a new, valid pkThread rooted at inner's table.
func computeFlatMapPKThread(fm *plans.RecordQueryFlatMapPlan) pkThread {
	outerThread := computePKThread(fm.GetOuter())
	if !outerThread.ok {
		return pkThread{}
	}
	if !innerFullyBindsThread(fm, outerThread) {
		return pkThread{}
	}
	innerThread := computePKThread(fm.GetInner())
	if !innerThread.ok {
		return pkThread{}
	}
	// fm's OWN resultValue defines what the FlatMap actually emits — it can
	// merge outer and inner fields, rename them, or drop the inner's PK
	// entirely, exactly like a Map/Projection's resultValue can. Re-root
	// innerThread through it rather than assuming the FlatMap's output is
	// interchangeable with the bare inner leg's own identity.
	return pkThreadThroughResultValue(innerThread, fm.GetInnerAlias(),
		planRowLayout(fm.GetInner()), fm.GetResultValue())
}

// innerFullyBindsThread reports whether fm's inner leg's own scan/index
// comparisons bind EVERY VARIABLE component of outerThread's primary key via
// a single equality comparison each, correlated to fm's OWN outer alias.
// This is the exact condition under which each inner execution is keyed to
// one distinct outer row: outerThread already proves outer's rows are in 1:1
// correspondence with distinct primary-key values of outerThread.recordType,
// so an inner search that demands equality on ALL of those columns can only
// ever be satisfied by rows sharing exactly one such value — no two distinct
// outer executions can probe overlapping inner rows.
//
// "VARIABLE" excludes values.RecordTypeValue components: TranslatePrimaryKeyToValues
// (primary_key_translation.go) prepends one whenever a table's declared
// PRIMARY KEY compiles to Concat(RecordTypeKey(), Field(...)) — the normal
// shape for every table in a non-intermingled multi-type SQL schema. Within a
// SINGLE pkThread every row already shares one record type (that's
// outerThread.recordType, established by GetRecordTypes() having length 1 at
// every leaf computePKThread walked through), so that component's value is a
// per-thread CONSTANT — it can never distinguish one outer row from another,
// and demanding an explicit correlated equality on it would be requiring the
// inner search to bind something that carries no row identity. The real
// discriminating columns are the FieldValue components; only those must be
// explicitly bound. Any OTHER non-FieldValue, non-RecordTypeValue shape
// (nested/computed) still fails closed exactly as before — RecordTypeValue is
// the sole exception because its constancy is structurally provable from
// outerThread itself, not assumed.
//
// The match is by column IDENTITY — (correlation, domain, ordinal), RFC-197's
// triple — resolved against the OUTER LEG's own output row layout, which is
// the row every comparand correlated to fm's outer alias reads. The outer
// thread's PK components are metadata name carriers with no ordinal of their
// own (primary_key_translation.go: "the flat field-reference model", never
// evaluated), so they are resolved against that same layout once, at the
// boundary. Two same-leaf-named columns reached through different quantifiers
// no longer key one slot, and an ordinal is never compared across layouts.
func innerFullyBindsThread(fm *plans.RecordQueryFlatMapPlan, outerThread pkThread) bool {
	binding, ok := scanBindingOfLeaf(fm.GetInner())
	if !ok || len(binding.comparisons) == 0 {
		return false
	}
	if len(outerThread.pkTypes) != len(outerThread.pkValues) {
		return false
	}
	outerLayout := planRowLayout(fm.GetOuter())
	frontier := values.OrdinalDomainOfType(outerLayout)
	if !frontier.IsKnown() {
		return false // no declared column order to state the proof in — fail closed
	}
	wantKeys := make(map[values.ColumnIdentity]values.Type, len(outerThread.pkValues))
	for i, pv := range outerThread.pkValues {
		if _, isRecordType := pv.(*values.RecordTypeValue); isRecordType {
			continue // per-thread constant — never a discriminating column, see doc comment
		}
		key, ok := leafFieldIdentity(pv, outerLayout)
		if !ok {
			return false // a non-flat-FieldValue PK component — fail closed
		}
		key = key.WithCorrelation(fm.GetOuterAlias())
		if _, duplicate := wantKeys[key]; duplicate {
			return false
		}
		wantKeys[key] = outerThread.pkTypes[i]
	}
	if len(wantKeys) == 0 {
		return false
	}

	bound := make(map[values.ColumnIdentity]bool, len(wantKeys))
	for i, cr := range binding.comparisons {
		if cr == nil || !cr.IsEquality() {
			continue
		}
		eq := cr.GetEqualityComparison()
		if eq == nil || eq.Type != predicates.ComparisonEquals || eq.Operand == nil {
			continue
		}
		// The LEG is judged here and canonicalized onto the key, so the
		// wantKeys lookup that follows decides only WHICH COLUMN. Dropping the
		// leg check admits a comparand correlated to some OTHER quantifier: in
		// a self-join the two legs share a layout, so an equality on the
		// INNER's own copy of the outer PK column agrees on domain and ordinal
		// and would be counted as binding the outer thread — a 1:1 claim the
		// inner search does not make, and the FK-chain cap would then divide a
		// real fan-out away.
		key, ok := correlatedFieldIdentity(eq.Operand, frontier)
		if !ok || !values.SameLeg(key.Correlation, fm.GetOuterAlias()) {
			continue
		}
		key = key.WithCorrelation(fm.GetOuterAlias())
		sourceType, wanted := wantKeys[key]
		if wanted && properties.LogicalEqualityProjectionInjective(
			sourceType, physicalTypeAt(binding.physicalTypes, i),
		) {
			bound[key] = true
		}
	}
	return len(bound) == len(wantKeys)
}

// leafFieldIdentity returns the IDENTITY of the column a leaf-relative
// primary-key Value names, resolved against the layout the tracked rows
// actually have.
//
// A pkThread's PK components are METADATA name carriers by design: a LAZY,
// Child==nil FieldValue, exactly as GetPrimaryKeyValues() /
// GetCommonPrimaryKeyValues() stamp a simple, non-nested key column
// (primary_key_translation.go's "the flat field-reference model", never
// evaluated), or the OUTPUT name a re-rooting hop assigned. They carry no
// ordinal of their own — so this is the boundary resolution of RFC-197 item 1
// applied at the point of use: the metadata name is resolved ONCE against the
// stated row layout and dies there, and every comparison downstream is between
// ordinals.
//
// It replaces `leafFieldName`, which returned the bare string and let its
// callers key sets by it (RFC-197 item 2 — the escape). Composite/nested PK
// shapes (Child != nil, i.e. a RecordTypeKey-prefixed or nested structural PK)
// are not a flat single field and fail closed, as does a name the layout does
// not declare or a layout with no declared column order.
func leafFieldIdentity(v values.Value, layout values.Type) (values.ColumnIdentity, bool) {
	fv, ok := v.(*values.FieldValue)
	if !ok || fv.Child != nil {
		return values.ColumnIdentity{}, false
	}
	return values.OrdinalOfNameIn(layout, fv.Field)
}

// correlatedFieldIdentity returns the IDENTITY of the column a comparand
// reads, when the comparand is a BARE column reference off a source
// QuantifiedObjectValue, stated in the caller's frontier layout.
//
// It replaces `correlatedFieldOf`, which handed the leaf name back as a bare
// string for the caller to match against a name-keyed set (RFC-197 item 2).
// Anything else (a literal, a nested/computed expression, a differently-shaped
// correlation) fails closed — including a fused baked multi-accessor path,
// where FieldValue.Field is only the LEAF name and a nested `outer.address.id`
// would otherwise impersonate a real top-level "ID" column. That case now
// declines structurally rather than by a hand-written guard: values.OrdinalIn
// refuses a multi-accessor path because its root ordinal addresses the OUTER
// step. plans/cost.go's correlatedInnerFieldKey is the same shape in the
// unique-key join-cost proof this function mirrors; keep both in lockstep.
//
// This also closes the incomparability the old name match existed to work
// around. A real search comparand is a BAKED FieldValue (an ordinal path
// against its own layout) while a declared primary key is a LAZY name carrier,
// so values.EqualsWithoutChildren treats them as unequal BY CONTRACT — an
// ordinal-vs-name comparison, not a value comparison. Resolving the metadata
// name against the same stated layout the comparand's ordinal indexes makes
// the two sides comparable in the element that is actually the identity,
// instead of in the one that merely looks like it.
func correlatedFieldIdentity(v values.Value, frontier values.OrdinalDomain) (values.ColumnIdentity, bool) {
	fv, isField := v.(*values.FieldValue)
	if !isField {
		return values.ColumnIdentity{}, false
	}
	return fv.CorrelatedIdentityIn(frontier)
}

// planRowLayout is a plan's output row layout — the frontier the identities
// above are stated in. Nil-safe: a missing leg has no layout, and the
// resolution then declines rather than panicking.
//
// A FlatMap needs its own arm because RecordQueryFlatMapPlan.GetResultType()
// answers UnknownType unconditionally: the plan does not compute a row type
// from its resultValue. Every hop of an FK chain past the first has a FlatMap
// as its OUTER, so taking that answer at face value would fail the identity
// proof closed for exactly the multi-hop shape this whole file exists for —
// the cap would still fire on hop 1 and silently stop firing on hops 2..n,
// which is the order-dependent estimate fkChainCardinalityCap was written to
// remove.
//
// The derivation is the resultValue's, because the resultValue is what shapes
// the emitted row: a bare QuantifiedObjectValue over one of the two aliases
// emits THAT leg's row unchanged, so the FlatMap's layout is that leg's. Any
// other resultValue (a RecordConstructor merging both legs, a computed value)
// produces a row this file cannot name a layout for, and declines — the same
// fail-closed answer, reached deliberately rather than by omission.
func planRowLayout(p plans.RecordQueryPlan) values.Type {
	switch pl := p.(type) {
	case nil:
		return nil
	case *plans.RecordQueryFlatMapPlan:
		if pl == nil {
			return nil
		}
		qov, ok := pl.GetResultValue().(*values.QuantifiedObjectValue)
		if !ok {
			return nil
		}
		switch qov.Correlation {
		case pl.GetInnerAlias():
			return planRowLayout(pl.GetInner())
		case pl.GetOuterAlias():
			return planRowLayout(pl.GetOuter())
		}
		return nil
	}
	return p.GetResultType()
}

// singleChildRowLayout is planRowLayout for the sole child of a one-input
// plan, mirroring pkThreadFromSingleChild's shape check: anything but exactly
// one child has no single row layout to speak about.
func singleChildRowLayout(children []plans.RecordQueryPlan) values.Type {
	if len(children) != 1 {
		return nil
	}
	return planRowLayout(children[0])
}

type leafScanBinding struct {
	comparisons   []*predicates.ComparisonRange
	physicalTypes []values.Type
}

// scanBindingOfLeaf resolves p down through the same identity-preserving
// wrappers as the old scanComparisonsOfLeaf walk and returns the bottommost
// scan/index leaf's comparisons together with their aligned authoritative
// physical key types. Keeping those parallel vectors in one result prevents a
// correlated disjointness proof from reconstructing the target domain from
// the comparand or from a display name.
func scanBindingOfLeaf(p plans.RecordQueryPlan) (leafScanBinding, bool) {
	switch pl := p.(type) {
	case *plans.RecordQueryScanPlan:
		if pl == nil {
			return leafScanBinding{}, false
		}
		return leafScanBinding{
			comparisons: pl.GetScanComparisons(), physicalTypes: pl.GetKeyComponentTypes(),
		}, true
	case *plans.RecordQueryIndexPlan:
		if pl == nil {
			return leafScanBinding{}, false
		}
		// The cap sums one partition of the inner table per distinct outer
		// primary key. A fan-out index can emit the same physical inner record
		// under multiple indexed values, so two outer probes can overlap even
		// though their bind values differ. That breaks the partition proof before
		// this index ever becomes a later hop's outer thread. Require the same
		// explicit duplicate-free signal computePKThread requires; an unstamped
		// hand-built/stale plan fails closed as Java's empty-candidate default.
		if !pl.ProducesDistinctRecords() {
			return leafScanBinding{}, false
		}
		return leafScanBinding{
			comparisons: pl.GetScanComparisons(), physicalTypes: pl.GetKeyComponentTypes(),
		}, true
	case *plans.RecordQueryFetchFromPartialRecordPlan:
		return scanBindingOfSingleChild(pl.GetChildren())
	case *plans.RecordQueryTypeFilterPlan:
		return scanBindingOfSingleChild(pl.GetChildren())
	default:
		return leafScanBinding{}, false
	}
}

func scanBindingOfSingleChild(children []plans.RecordQueryPlan) (leafScanBinding, bool) {
	if len(children) != 1 {
		return leafScanBinding{}, false
	}
	return scanBindingOfLeaf(children[0])
}

// fkChainCardinalityCap returns a sound, PROVABLE upper bound on fm's total
// output cardinality — the size of the table its inner leg probes — and
// whether that bound could be proven at all. It fires exactly when fm's
// inner leg equality-binds the FULL primary key the outer chain has already
// established itself to be threaded through (computeFlatMapPKThread's
// precondition), which is precisely the condition under which "every probe's
// results, summed over all outer rows, cannot exceed the probed table's own
// row count" is sound (see the file doc comment for why the general form of
// this cap is unsound and what recovers soundness here).
func fkChainCardinalityCap(fm *plans.RecordQueryFlatMapPlan, stats properties.StatisticsProvider) (float64, bool) {
	outerThread := computePKThread(fm.GetOuter())
	if !outerThread.ok {
		return 0, false
	}
	if !innerFullyBindsThread(fm, outerThread) {
		return 0, false
	}
	innerRecordType, ok := singleLeafRecordType(fm.GetInner())
	if !ok {
		return 0, false
	}
	if stats == nil {
		stats = properties.DefaultStatistics{}
	}
	return stats.RecordTypeCardinality(innerRecordType), true
}

// fkChainCappedInnerCost derives a CPU-consistent average inner Cost for a
// FlatMap hop once fkChainCardinalityCap has proven the hop emits at most cap
// rows across all outer probes. fixedPerExecutionCPU is the part of the inner
// CPU paid every time FlatMap opens that inner, even when the probe returns no
// row: an isolated point-probe GetRange and every additional physical range
// seek are the two leaf examples. Only the remaining row-dependent CPU scales
// with the reduced average result cardinality.
//
// FlatMap executes the inner once per emitted outer row. Therefore a binding
// cap changes the estimated rows returned by one execution from innerCard to
// cap/outerCard, but it does not change the number of executions. Scaling the
// entire CPU by that ratio would erase real startup work: 1,000 distinct outer
// keys probing a 20-row table still open 1,000 cursors/ranges even though only
// 20 probes can return a row. Java's model likewise uses the FlatMap outer
// cardinality to reason about how often the inner is executed; this numeric
// statistics cap is a Go extension, not permission to reduce that count.
//
// Callers that cannot structurally derive the fixed component pass inner.CPU.
// That still applies the proven cardinality cap while conservatively preserving
// all CPU. A negative or oversized fixed component is clamped to [0,inner.CPU]
// so the helper remains safe for hand-built plans and direct property tests.
func fkChainCappedInnerCost(
	outer, inner properties.Cost,
	cap float64,
	fixedPerExecutionCPU float64,
) (properties.Cost, bool) {
	outerCard := outer.Cardinality
	if outerCard <= 0 || inner.Cardinality <= 0 ||
		math.IsNaN(outerCard) || math.IsInf(outerCard, 0) ||
		math.IsNaN(inner.Cardinality) || math.IsInf(inner.Cardinality, 0) ||
		math.IsNaN(inner.CPU) || math.IsInf(inner.CPU, 0) || inner.CPU < 0 ||
		math.IsNaN(cap) || math.IsInf(cap, 0) || cap < 0 {
		return properties.Cost{}, false
	}
	uncappedTotal := properties.SaturatingHeuristicMultiply(outerCard, inner.Cardinality)
	if cap >= uncappedTotal {
		return properties.Cost{}, false // not binding — inner's own estimate already satisfies the bound
	}
	averageCardinality := cap / outerCard
	r := averageCardinality / inner.Cardinality
	if math.IsNaN(fixedPerExecutionCPU) || math.IsInf(fixedPerExecutionCPU, 0) {
		fixedPerExecutionCPU = inner.CPU
	}
	if fixedPerExecutionCPU < 0 {
		fixedPerExecutionCPU = 0
	}
	if fixedPerExecutionCPU > inner.CPU {
		fixedPerExecutionCPU = inner.CPU
	}
	variableCPU := inner.CPU - fixedPerExecutionCPU
	return properties.Cost{
		Cardinality: averageCardinality,
		CPU: properties.SaturatingHeuristicAdd(
			fixedPerExecutionCPU,
			properties.SaturatingHeuristicMultiply(variableCPU, r),
		),
	}, true
}

// singleLeafRecordType resolves p down to its bottommost scan/index leaf
// (through the same wrappers computePKThread sees through) and returns that
// leaf's single record type.
func singleLeafRecordType(p plans.RecordQueryPlan) (string, bool) {
	switch pl := p.(type) {
	case *plans.RecordQueryScanPlan:
		rts := pl.GetRecordTypes()
		if len(rts) != 1 {
			return "", false
		}
		return rts[0], true
	case *plans.RecordQueryIndexPlan:
		rts := pl.GetRecordTypes()
		if len(rts) != 1 {
			return "", false
		}
		return rts[0], true
	case *plans.RecordQueryFetchFromPartialRecordPlan:
		return singleLeafRecordTypeFromChildren(pl.GetChildren())
	case *plans.RecordQueryTypeFilterPlan:
		return singleLeafRecordTypeFromChildren(pl.GetChildren())
	case *plans.RecordQueryPredicatesFilterPlan:
		return singleLeafRecordTypeFromChildren(pl.GetChildren())
	case *plans.RecordQueryFilterPlan:
		return singleLeafRecordTypeFromChildren(pl.GetChildren())
	default:
		return "", false
	}
}

func singleLeafRecordTypeFromChildren(children []plans.RecordQueryPlan) (string, bool) {
	if len(children) != 1 {
		return "", false
	}
	return singleLeafRecordType(children[0])
}
