package cascades

import (
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
		return pkThread{recordType: rts[0], pkValues: pk, ok: true}

	case *plans.RecordQueryIndexPlan:
		rts := pl.GetRecordTypes()
		pk := pl.GetCommonPrimaryKeyValues()
		if len(rts) != 1 || len(pk) == 0 {
			return pkThread{}
		}
		return pkThread{recordType: rts[0], pkValues: pk, ok: true}

	case *plans.RecordQueryFetchFromPartialRecordPlan:
		return pkThreadFromSingleChild(pl.GetChildren())
	case *plans.RecordQueryTypeFilterPlan:
		return pkThreadFromSingleChild(pl.GetChildren())
	case *plans.RecordQueryPredicatesFilterPlan:
		return pkThreadFromSingleChild(pl.GetChildren())
	case *plans.RecordQueryFilterPlan:
		return pkThreadFromSingleChild(pl.GetChildren())
	case *plans.RecordQueryMapPlan:
		return pkThreadFromSingleChild(pl.GetChildren())
	case *plans.RecordQueryProjectionPlan:
		return pkThreadFromSingleChild(pl.GetChildren())
	case *plans.RecordQueryInMemorySortPlan:
		return pkThreadFromSingleChild(pl.GetChildren())

	case *plans.RecordQueryFlatMapPlan:
		return computeFlatMapPKThread(pl)

	default:
		return pkThread{}
	}
}

func pkThreadFromSingleChild(children []plans.RecordQueryPlan) pkThread {
	if len(children) != 1 {
		return pkThread{}
	}
	return computePKThread(children[0])
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
	return computePKThread(fm.GetInner())
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
// The match is by FIELD NAME + correlation identity, not
// values.ValuesStructurallyEqual: a real search comparand off a planned
// scan/index is a BAKED FieldValue (Resolved != nil, an ordinal path against
// THAT plan's own column layout), while a plan's declared primary key
// (GetPrimaryKeyValues/GetCommonPrimaryKeyValues) is a LAZY, name-only
// FieldValue by design (primary_key_translation.go: "the flat field-reference
// model", never evaluated). values.EqualsWithoutChildren's FieldValue arm
// treats baked-vs-lazy as UNEQUAL BY CONTRACT (it's an ordinal-vs-name
// comparison, not a value comparison) — the two sides here are NEVER
// comparable that way. What actually answers "does this comparand read
// outerAlias's declared PK column" is the field name plus the correlation the
// comparand's Child resolves to, both of which are populated on baked and
// lazy nodes alike (Field is display-name metadata; Resolved doesn't gate
// it).
func innerFullyBindsThread(fm *plans.RecordQueryFlatMapPlan, outerThread pkThread) bool {
	comps := scanComparisonsOfLeaf(fm.GetInner())
	if comps == nil {
		return false
	}
	wantFields := make(map[string]bool, len(outerThread.pkValues))
	for _, pv := range outerThread.pkValues {
		if _, isRecordType := pv.(*values.RecordTypeValue); isRecordType {
			continue // per-thread constant — never a discriminating column, see doc comment
		}
		name, ok := leafFieldName(pv)
		if !ok {
			return false // a non-flat-FieldValue PK component — fail closed
		}
		wantFields[name] = true
	}
	if len(wantFields) == 0 {
		return false
	}

	bound := make(map[string]bool, len(wantFields))
	for _, cr := range comps {
		if cr == nil || !cr.IsEquality() {
			continue
		}
		eq := cr.GetEqualityComparison()
		if eq == nil || eq.Type != predicates.ComparisonEquals || eq.Operand == nil {
			continue
		}
		field, corr, ok := correlatedFieldOf(eq.Operand)
		if !ok || corr != fm.GetOuterAlias() {
			continue
		}
		if wantFields[field] {
			bound[field] = true
		}
	}
	return len(bound) == len(wantFields)
}

// leafFieldName returns the field name of a leaf-relative primary-key Value
// (Child==nil at the root, exactly as GetPrimaryKeyValues() /
// GetCommonPrimaryKeyValues() stamp a simple, non-nested key column — see
// primary_key_translation.go's "base of every leaf is nil" invariant).
// Composite/nested PK shapes (Child != nil, i.e. a RecordTypeKey-prefixed or
// nested structural PK) are not a flat single field and fail closed.
func leafFieldName(v values.Value) (string, bool) {
	fv, ok := v.(*values.FieldValue)
	if !ok || fv.Child != nil {
		return "", false
	}
	return fv.Field, true
}

// correlatedFieldOf returns the field name and root correlation of a
// comparand shaped like FieldValue{Field, Child: QuantifiedObjectValue} — the
// shape a correlated column reference takes, baked or lazy alike. Anything
// else (a literal, a nested/computed expression, a differently-shaped
// correlation) fails closed.
func correlatedFieldOf(v values.Value) (field string, corr values.CorrelationIdentifier, ok bool) {
	fv, isField := v.(*values.FieldValue)
	if !isField {
		return "", values.CorrelationIdentifier{}, false
	}
	qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
	if !isQOV {
		return "", values.CorrelationIdentifier{}, false
	}
	return fv.Field, qov.Correlation, true
}

// scanComparisonsOfLeaf resolves p down through the same identity-preserving
// wrappers computePKThread already knows how to see through, and returns the
// bottommost scan/index leaf's own ScanComparisons. nil means p is not (or
// does not resolve to) a bare scan/index leaf.
func scanComparisonsOfLeaf(p plans.RecordQueryPlan) []*predicates.ComparisonRange {
	switch pl := p.(type) {
	case *plans.RecordQueryScanPlan:
		return pl.GetScanComparisons()
	case *plans.RecordQueryIndexPlan:
		return pl.GetScanComparisons()
	case *plans.RecordQueryFetchFromPartialRecordPlan:
		return scanComparisonsOfSingleChild(pl.GetChildren())
	case *plans.RecordQueryTypeFilterPlan:
		return scanComparisonsOfSingleChild(pl.GetChildren())
	default:
		return nil
	}
}

func scanComparisonsOfSingleChild(children []plans.RecordQueryPlan) []*predicates.ComparisonRange {
	if len(children) != 1 {
		return nil
	}
	return scanComparisonsOfLeaf(children[0])
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

// fkChainCappedInnerCost derives a CPU-consistent inner Cost for a FlatMap hop
// once fkChainCardinalityCap has proven the hop's total output cannot exceed
// `cap` rows across all of outer's probes. Returns the derived Cost and
// whether the cap actually binds (ok=false means the caller should use
// inner unchanged — the proven bound does not improve on inner's own
// selectivity-based estimate here).
//
// DERIVATION (not a picked constant): FlatMapCost re-runs inner once per
// outer row, so the total rows examined across the whole hop is
// outerCard * inner.Cardinality, where inner.Cardinality is scanLikeCost's
// flat-selectivity GUESS at how many rows one probe returns. The cap proves a
// hard ceiling on that SAME total: cap. If cap < outerCard*inner.Cardinality,
// the guess is provably too high, and the bound implies a corrected average
// rows-EXAMINED-per-probe of cap/outerCard (NOT inner.Cardinality) — the same
// arithmetic fkChainCardinalityCap's own soundness argument already
// establishes, one level down: total ≤ cap, spread evenly over outerCard
// identically-shaped probes (every probe binds the same way, off the same
// index, so there is no basis to assume any one probe cheaper or costlier
// than another).
//
// scanLikeCost's CPU is an EXACTLY LINEAR, zero-intercept function of the
// leaf's own Cardinality (CPU = card*ScanCPU*multiplier — see scanLikeCost),
// and every wrapper this cap sees through (Fetch/TypeFilter/PredicatesFilter,
// see scanComparisonsOfLeaf's siblings) is itself linear with no additive
// constant independent of its child's cardinality (FetchCost, TypeFilterCost,
// FilterCost — cost_formulas.go). So inner.CPU, taken as a whole, is an exactly
// linear function of inner.Cardinality with no offset: scaling BOTH
// inner.Cardinality and inner.CPU by the identical ratio
// r = (cap/outerCard) / inner.Cardinality reproduces exactly the Cost a
// probe returning cap/outerCard rows would have had, rather than crediting
// the hop with the proven (smaller) row count while still charging it for
// scanning the disproven (larger) one — the same "cardinality and CPU must
// describe the same execution" property FlatMapCost's own doc comment
// already requires of NestedLoopJoinCost vs FlatMapCost.
//
// FlatMapCost's own outerCard fallback (LeafScanCardinality when
// outer.Cardinality is 0) is mirrored here so the ratio is computed against
// the SAME outerCard FlatMapCost will actually use.
func fkChainCappedInnerCost(outer, inner properties.Cost, cap float64) (properties.Cost, bool) {
	outerCard := outer.Cardinality
	if outerCard <= 0 {
		outerCard = properties.LeafScanCardinality
	}
	if inner.Cardinality <= 0 {
		return properties.Cost{}, false
	}
	uncappedTotal := outerCard * inner.Cardinality
	if cap >= uncappedTotal {
		return properties.Cost{}, false // not binding — inner's own estimate already satisfies the bound
	}
	r := (cap / outerCard) / inner.Cardinality
	return properties.Cost{
		Cardinality: inner.Cardinality * r,
		CPU:         inner.CPU * r,
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
