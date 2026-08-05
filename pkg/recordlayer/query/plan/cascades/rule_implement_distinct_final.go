package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/matching"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// Go extension: Java's fdb-relational 4.11.1.0 rejects SELECT DISTINCT for most
// query shapes; Go supports it broadly via this rule and the hash-distinct executor.
//
// ImplementDistinctFinalRule is the PLANNING-phase rule for
// LogicalDistinctExpression. Ports Java's ImplementDistinctRule
// (ImplementationCascadesRule<LogicalDistinctExpression>).
//
// For each physical FinalMember, the rule checks two sources of
// distinctness information:
//
//  1. Physical-level DistinctRecordsProperty — per-member, matching
//     Java's ImplementDistinctRule 1:1. A scan on a unique index, an
//     identity-mapping MapPlan, etc. propagate distinctness through
//     the property system.
//  2. Logical-level PRIMARY KEY column coverage — Go extension,
//     fallback. If the projected columns cover a primary key, ALL physical
//     plans are guaranteed distinct (equivalent to Java's "strictlySorted"
//     path where all partition members get elided). Primary-key uniqueness
//     is a storage invariant, so this license is unconditional.
//  3. Logical-level SECONDARY UNIQUE index coverage — see
//     secondaryUniqueEliminationProof. Unlike 1 and 2 this license rests on
//     MUTABLE STORE STATE, so the yielded plan carries the proving index's
//     name as a stamp and the index becomes a plan dependency revalidated in
//     every execution transaction.
//
// The order matters and is enforced in yieldElidedOrWrapped: 1, then 2, then 3,
// and ONLY 3 stamps. An elision the property path or the primary key already
// justified must not record a dependency its correctness does not rest on.
//
// When any check passes, the Distinct operator is elided and the
// inner plan is yielded directly. Otherwise, the inner is wrapped
// with RecordQueryDistinctPlan.
//
// This rule subsumes the former DistinctOnUniqueElimRule (which ran
// during EXPLORE). Moving the elimination check to PLANNING matches
// Java's architecture: ImplementDistinctRule is an
// ImplementationCascadesRule, not an exploration rule.
type ImplementDistinctFinalRule struct {
	matcher matching.BindingMatcher
}

func NewImplementDistinctFinalRule() *ImplementDistinctFinalRule {
	return &ImplementDistinctFinalRule{
		matcher: NewExpressionMatcher[*expressions.LogicalDistinctExpression]("logical_distinct_final"),
	}
}

func (r *ImplementDistinctFinalRule) Matcher() matching.BindingMatcher { return r.matcher }

func (r *ImplementDistinctFinalRule) OnMatch(call *ImplementationRuleCall) {
	d := call.Bindings.Get(r.matcher).(*expressions.LogicalDistinctExpression)
	qs := d.GetQuantifiers()
	if len(qs) == 0 {
		return
	}
	innerRef := qs[0].GetRangesOver()
	if innerRef == nil {
		return
	}

	// Check if projected columns cover a unique key (PK or unique index).
	// If so, ALL plans are distinct regardless of their physical
	// properties. This must check the Projection expression specifically,
	// not just innerRef.Get() which might return a Filter or other
	// expression merged into the same Reference during REWRITING.
	pkDistinct := false
	var proof secondaryUniqueProof
	if call.Context != nil {
		for _, m := range innerRef.Members() {
			if proj, ok := m.(*expressions.LogicalProjectionExpression); ok {
				pkDistinct = distinctEliminatedByUniqueKey(proj, call.Context)
				if !pkDistinct {
					proof = secondaryUniqueEliminationProof(proj, call.Context)
				}
				break
			}
		}
	}

	// Java-aligned: use PlanPartitions filtered by StoredRecord. Each
	// partition is evaluated independently — if its plans are already
	// distinct (PK-level), the Distinct is absorbed; otherwise wrapped.
	// Mirrors Java's ImplementDistinctRule which uses
	// filterPlanPartitions(StoredRecordProperty.storedRecord()).
	partitions := ToPlanPartitions(innerRef)

	handled := false
	for _, partition := range partitions {
		if !partition.GetPartitionPropertiesMap().GetBool(properties.PropStoredRecord) {
			continue
		}
		handled = true

		for _, expr := range partition.GetExpressions() {
			r.yieldElidedOrWrapped(call, expr,
				partition.IsDistinct() && expressionRecordIdentityMatchesLogicalEquality(expr),
				pkDistinct, proof)
		}
	}

	// Logical-level fallback: if projected columns cover a unique key,
	// ALL physical plans are guaranteed distinct. This fires when no
	// StoredRecord partitions were found (e.g. unit tests without full
	// PLANNING, or when the fallback partitioner doesn't set properties).
	if !handled {
		allDistinct := false
		var fallbackProof secondaryUniqueProof
		if call.Context != nil {
			innerExpr := innerRef.Get()
			allDistinct = distinctEliminatedByUniqueKey(innerExpr, call.Context)
			if !allDistinct {
				fallbackProof = secondaryUniqueEliminationProof(innerExpr, call.Context)
			}
		}

		for _, m := range innerRef.AllMembers() {
			if _, ok := m.(physicalPlanExpression); !ok {
				continue
			}
			// No PropDistinctRecords partition exists on this path, so the
			// property license cannot be consulted; the PK arm is the first
			// license available and the secondary-UNIQUE arm the last.
			r.yieldElidedOrWrapped(call, m, false, allDistinct, fallbackProof)
		}
	}
}

// yieldElidedOrWrapped applies the rule's LICENSE ORDER and is the sole site
// that decides whether an elided plan carries a §5.2 proof stamp.
//
// The order is not a tidiness preference, it is a correctness requirement.
// PropDistinctRecords — Java's own mechanism, the only thing
// ImplementDistinctRule reads — licenses the elision on its own for many plans,
// and so does primary-key coverage. Both are unconditional: a PK is a storage
// invariant, and record distinctness is a property of the plan itself. Neither
// depends on an index's mutable state, so neither may stamp. Stamping a plan
// whose elision one of them already justified would record a dependency the
// plan's correctness does not rest on, and transitioning that index would then
// fail a statement that would have been correct regardless — the over-scoping
// this design rejects, arrived at from the other direction.
//
// So a stamp is written only when the secondary-UNIQUE proof is the SOLE
// license. When the yielded plan cannot carry a stamp, the elision is DECLINED
// and the physical distinct is kept: a proof whose dependency cannot be recorded
// is a proof that survives an index transition unguarded, which is worse than
// the redundant operator it would have removed.
//
// The last arm is R3, and it is a FLOOR rather than a fallback that can fail.
// When clauses 1-7 hold but the exempt set could not be proved empty, the
// operator is kept and NARROWED: only rows carrying an exempt key component
// enter the seen-set. That needs no fact about the stream at all, so it always
// applies where a qualifying index exists — and it still records the dependency,
// because its soundness rests on the index's uniqueness exactly as the elision's
// does.
func (r *ImplementDistinctFinalRule) yieldElidedOrWrapped(
	call *ImplementationRuleCall,
	expr expressions.RelationalExpression,
	propertyLicensed bool,
	pkLicensed bool,
	proof secondaryUniqueProof,
) {
	if propertyLicensed || pkLicensed {
		call.YieldFinalExpression(expr)
		return
	}
	if proof.FullElision {
		if stamped := stampDistinctProof(expr, proof.IndexName); stamped != nil {
			call.YieldFinalExpression(stamped)
			return
		}
	}
	w := newPhysicalDistinctFor(call, expr)
	if w == nil {
		return
	}
	if proof.IndexName != "" {
		if distinct, ok := w.(*plans.RecordQueryDistinctPlan); ok {
			w = distinct.WithNarrowedDedup(
				proof.IndexName, exemptSlotsFor(distinct.GetInner(), proof))
		}
	}
	call.YieldFinalExpression(w)
}

// exemptSlotsFor maps the proving index's key columns onto positions in the
// DEDUP KEY's slot order — the order distinctKey packs, which distinctKeyColumns
// is the authority for ("distinctKey packs exactly these positional slots").
//
// Resolving against that list rather than re-deriving a position is what keeps
// the executor's test aimed at the right slots: a projection may carry columns
// the index does not key, and testing those would retain rows the index already
// proves unique.
//
// nil is returned whenever ANY key column has no statable position — an
// expression-valued projection, a lazy or foreign-domain reference, a
// non-projection inner whose schema the walk cannot line up. nil means "test
// every slot", which OVER-approximates the exempt set: more rows are
// deduplicated than necessary, which costs performance and cannot drop a row.
// Under-approximating would drop rows, so the unstatable case has exactly one
// safe direction and this is it.
func exemptSlotsFor(inner plans.RecordQueryPlan, proof secondaryUniqueProof) []int {
	if inner == nil || len(proof.KeyOrdinals) == 0 || !proof.Layout.IsKnown() {
		return nil
	}
	slotColumns := distinctKeyColumns(inner)
	if len(slotColumns) == 0 {
		return nil
	}
	positionOfOrdinal := map[int]int{}
	for position, column := range slotColumns {
		fv, isField := column.(*values.FieldValue)
		if !isField {
			continue
		}
		ord, stated := fv.OrdinalIn(proof.Layout)
		if !stated {
			continue
		}
		positionOfOrdinal[ord] = position
	}
	slots := make([]int, 0, len(proof.KeyOrdinals))
	for _, keyOrdinal := range proof.KeyOrdinals {
		position, found := positionOfOrdinal[keyOrdinal]
		if !found {
			return nil
		}
		slots = append(slots, position)
	}
	return slots
}

// stampDistinctProof returns a copy of a physical expression carrying the name
// of the secondary UNIQUE index that licensed eliding a DISTINCT over it, or nil
// when the expression is not a plan that can carry one. Asking for the
// CAPABILITY (plans.DistinctProofStampable) rather than switching on concrete
// plan types is what keeps a new plan type from silently becoming an unguarded
// elision: a type that cannot carry the proof declines the elision instead.
func stampDistinctProof(
	expr expressions.RelationalExpression, indexName string,
) expressions.RelationalExpression {
	stampable, ok := expr.(plans.DistinctProofStampable)
	if !ok {
		return nil
	}
	stamped, ok := stampable.WithDistinctProofIndexName(indexName).(expressions.RelationalExpression)
	if !ok {
		return nil
	}
	return stamped
}

// distinctEliminatedByUniqueKey checks whether the projected column set
// covers all columns of a PRIMARY KEY, making the DISTINCT operator
// redundant. Secondary unique indexes are not admitted as proofs; the
// reasoning is at the bottom of this function, where they are declined.
func distinctEliminatedByUniqueKey(
	innerExpr expressions.RelationalExpression,
	ctx PlanContext,
) bool {
	// The proof is stated in ONE layout — the scan row both the projected
	// references and the metadata key columns address. An unstatable layout (a
	// multi-record-type scan degrades to unknown, a physical inner has no scan
	// expression here) fails closed: the per-FinalMember PropDistinctRecords
	// check is the primary path and a declined elision costs a DISTINCT that was
	// redundant, never a duplicate row.
	layoutType, layout := scanRowLayout(innerExpr)
	if !layout.IsKnown() {
		return false
	}
	projectedOrds, statable := collectProjectedOrdinals(innerExpr, layout)
	if !statable {
		return false
	}

	recordTypes := findRecordTypes(innerExpr)
	if len(recordTypes) == 0 {
		return false
	}

	// A visible PK is globally unique only inside one record type. In a
	// multi-type stream, A/1 and B/1 are different physical primary keys but
	// collide after projecting the visible ID coordinate.
	if len(recordTypes) == 1 {
		pkCols := ctx.GetPrimaryKeyColumns(recordTypes[0])
		pkTypes := physicalTypesFromFlatRow(layoutType, pkCols, nil)
		if properties.TupleKeyUniquenessMatchesLogicalEquality(pkTypes, len(pkCols)) &&
			uniqueKeysCovered(pkCols, layoutType, projectedOrds) {
			return true
		}
	}

	// A SECONDARY index's `unique` flag is a separate proof with a separate
	// admission predicate and a plan-carried dependency, because it rests on
	// mutable STORE state rather than on a storage invariant. It lives in
	// secondaryUniqueEliminationProof, which the rule consults only after this
	// one declines.
	return false
}

// secondaryUniqueEliminationProof returns the name of a secondary UNIQUE index
// that proves a DISTINCT over innerExpr's projection redundant, or "" when none
// does. Every clause below fails CLOSED, and the conjunction is the whole
// admission predicate — there is no second authority anywhere.
//
// A secondary `unique` flag states an INTENT recorded in metadata; whether the
// stored rows honour it is a property of the STORE, and the two part company in
// exactly one state. A unique index whose build completed over violating data
// lands in READABLE_UNIQUE_PENDING: fully populated, scannable, carrying a
// `unique` flag the data contradicts — "safe to consider READABLE for queries as
// long as uniqueness is not assumed" (IndexState.java:59-66).
//
// Nothing here checks for that state, and that is the design rather than an
// oversight. Java's planner contains ZERO checks of READABLE_UNIQUE_PENDING; its
// safety is structural, because MetaDataPlanContext filters the index list
// through RecordStoreState::isReadable (MetaDataPlanContext.java:96-103 — exact
// equality with READABLE, RecordStoreState.java:172-174) BEFORE any match
// candidate is created, so every Java site trusting MatchCandidate.isUnique() is
// safe without asking. A bolted-on `if state == READABLE_UNIQUE_PENDING` here
// would be a second authority on a question the candidate filter already answers,
// and it would diverge the moment the candidate-set structure changed.
//
// Which is why the operational route is part of the predicate and not an
// implementation detail: the candidates come from ctx.GetMatchCandidates() and
// from nowhere else. That list is the one the readable-index filter already
// pruned. An implementation reaching instead for the metadata's whole index list,
// or for the store's GetReadableIndexes (which by its own contract INCLUDES
// READABLE_UNIQUE_PENDING), would satisfy every word written here and
// reintroduce the exact bug the arm was declined for.
//
// The result is a THREE-WAY answer, not a boolean, because clause 8 has three
// outcomes rather than two. Clauses 1-7 are about the index and the projection
// and either hold or do not. Clause 8 asks whether the index's EXEMPT SET is
// empty on this stream, and when it is not, the right response is not to walk
// away — it is R3, which keeps the operator and narrows it to exactly that
// exempt subset. So a proof with IndexName set and FullElision false is a
// positive result: the caller narrows rather than elides, and records the same
// dependency either way.
func secondaryUniqueEliminationProof(
	innerExpr expressions.RelationalExpression,
	ctx PlanContext,
) secondaryUniqueProof {
	// Clause 1, first half. "It is a match candidate, therefore the store called
	// it READABLE" holds only on planning runs that ASKED. Go's filter is fed by
	// the SELECT and DML generator paths; every other entry point — the
	// metadata-only harness, offline tools, rule unit tests — leaves the
	// configuration at its zero value, which permits every index to be scanned
	// and asserts nothing about any index's state. Java needs no equivalent
	// because its filter has no unstated callers.
	//
	// The demand is therefore for AFFIRMATIVE evidence, not for the absence of
	// contrary evidence: a run that never consulted a store may still plan an
	// index scan, but it may not prove anything from an index's declared
	// uniqueness. That makes every unwritten planning path safe by construction
	// rather than safe by everyone remembering to thread a config field.
	if !ctx.GetPlannerConfiguration().ReadableIndexes.IndexStatesEstablished() {
		return secondaryUniqueProof{}
	}

	// The proof is stated in ONE layout — the scan row that both the projected
	// references and the metadata key columns address (RFC-197's boundary rule:
	// resolve names once, compare ordinals). An unstatable layout fails closed.
	layoutType, layout := scanRowLayout(innerExpr)
	if !layout.IsKnown() {
		return secondaryUniqueProof{}
	}
	projectedOrds, statable := collectProjectedOrdinals(innerExpr, layout)
	if !statable {
		return secondaryUniqueProof{}
	}

	// Clause 7. A secondary key is unique only WITHIN a record type: A's and B's
	// unique keys collide the moment a shared visible coordinate is projected.
	// The same restriction the primary-key arm carries.
	recordTypes := findRecordTypes(innerExpr)
	if len(recordTypes) != 1 {
		return secondaryUniqueProof{}
	}
	recordType := recordTypes[0]

	// R2's evidence, gathered once for all candidates: the scan-row ordinals on
	// which a NULL-rejecting conjunct sits strictly between the scan and this
	// DISTINCT. Empty on an unfiltered query, which leaves clause 8 exactly as
	// strict as R1 alone.
	nullRejected := nullRejectedOrdinals(innerExpr, layout)

	// R3's carrier. The FIRST candidate to satisfy clauses 1-7 is remembered
	// here; if a later one also discharges clause 8 the full elision wins, which
	// is why the loop keeps going rather than returning the residual on sight.
	var residual secondaryUniqueProof

	for _, candidate := range ctx.GetMatchCandidates() {
		// Clause 1, second half.
		if candidate == nil {
			continue
		}
		// Clause 2 — the declared intent.
		if !candidate.IsUnique() {
			continue
		}
		// Clause 7, candidate side: a candidate spanning several record types
		// makes no statement about this stream's single type.
		candidateTypes := candidate.GetRecordTypes()
		if len(candidateTypes) != 1 || candidateTypes[0] != recordType {
			continue
		}
		// Clauses 3 and 4. The authority is the shared cardinality gate, NOT
		// createsDuplicates directly: createsDuplicates answers only the fan-out
		// question, while the gate additionally refuses a candidate whose
		// traversal was rejected (scalar nesting, unsupported FAN_OUT shape,
		// inconsistent metadata) and a SPARSE candidate carrying a stored
		// predicate. Reaching for the narrow signal where the codebase already
		// has the affirmative one is how a second admission authority is born.
		//
		// Fan-out (clause 3): UNIQUE on a fan-out index constrains index
		// ENTRIES, and a record whose repeated field is empty produces no entry
		// at all, so the key says nothing about base-row distinctness. This
		// covers the nested-under-a-repeated-parent case a leaf's own FanType
		// does not reveal.
		//
		// Sparseness (clause 4) is the one failure whose consequence is WRONG
		// ROWS and whose cause is invisible in the index's key definition: a
		// WHERE-filtered index omits every record its stored predicate rejects,
		// so its UNIQUE declaration constrains only the rows the predicate
		// ADMITS and says nothing whatever about the excluded rows, which may
		// hold arbitrarily many duplicates of an admitted value.
		if !candidatePreservesBaseRecordCardinality(candidate) {
			continue
		}
		// Clause 5. The key columns must BE the columns, not a derived value:
		// a unique index over f(col) says nothing about col. plainFieldColumns
		// refuses any function-keyed component and any root key expression whose
		// top-level scalar fields do not match the declared column names.
		keyColumns, plain := candidatePlainFieldColumnsForShortcut(candidate)
		if !plain || len(keyColumns) == 0 {
			continue
		}
		// Clause 6. Every key column must be projected as a BARE, top-level
		// field, resolved to an ordinal in THIS layout. Two distinct failures
		// are prevented: a many-to-one projection (f(pk)) being credited as
		// injective, and a same-named column from a DIFFERENT source being
		// credited as covering this one's key.
		if !uniqueKeysCovered(keyColumns, layoutType, projectedOrds) {
			continue
		}
		// Clause 8. A unique STORAGE key is not yet a unique LOGICAL row, and it
		// fails in two opposite directions:
		//
		//   NULL — under NULLS DISTINCT the uniqueness check is skipped when an
		//   indexed component is NULL, so one NULL prefix holds arbitrarily many
		//   entries distinguished only by their appended primary key. A UNIQUE
		//   index on a nullable column permits (NULL),(NULL),(NULL) while
		//   SELECT DISTINCT col must return one row.
		//
		//   FLOAT/DOUBLE — FDB preserves distinct raw NaN sign and payload
		//   encodings, so two tuple keys the index happily holds are ONE logical
		//   value to values.CompareFloat64 (faithful to Double.compare). Storage
		//   identity finer than logical equality means eliding emits two rows
		//   where DISTINCT emits one.
		//
		// Together those two are the index's EXEMPT SET, and the proof this
		// clause needs is that the exempt set is EMPTY ON THIS STREAM. That is a
		// fact about the ROWS ARRIVING, not about the catalog, and reading it as
		// a catalog question is what made this arm inert: the SQL DDL rejects a
		// NOT NULL scalar column, so every SQL-expressible secondary unique index
		// has a nullable key and the catalog-wide form is false for all of them.
		//
		// Two routes discharge it here, in strength order.
		//
		// R1, the catalog route: every key component declared NOT NULL and none
		// FLOAT/DOUBLE, over the AUTHORITATIVE physical key component types — an
		// Unknown type fails closed, because a visible literal cannot substitute
		// for missing metadata. Strongest and cheapest, and it stays unrelaxed:
		// it is what fires for a record-layer API caller whose key CAN be NOT
		// NULL. An arm that only fires once this predicate is widened is a wrong
		// arm, not a too-strict predicate.
		//
		// R2, the predicate route: a NULL-rejecting conjunct on every key column,
		// strictly between the scan and this DISTINCT, empties the NULL half of
		// the exempt set on this stream even though the catalog permits NULLs.
		// The FLOAT/DOUBLE half is NOT overridable and is not overridden — a
		// predicate that rejects NULL admits every NaN encoding.
		//
		// R2's soundness rests entirely on three-valued semantics: the row whose
		// key column is NULL evaluates the conjunct to UNKNOWN and is DROPPED by
		// the filter executor, not passed through as TRUE.
		//
		// The THIRD outcome — R3 — is what happens when neither route
		// discharges it. The obligation is not abandoned; it is redirected. The
		// index still guarantees at most one row per NON-exempt key value, so
		// the operator is kept and narrowed to dedup exactly the exempt subset.
		// That claim needs nothing about the stream, so this candidate is
		// remembered unconditionally and used if no other candidate proves
		// better.
		if !properties.SecondaryUniqueKeyEnforcedOnStream(
			keyComponentTypesOf(candidate), len(keyColumns),
			nullRejectionPerKeyColumn(keyColumns, layoutType, nullRejected),
		) {
			if residual.IndexName == "" {
				residual = secondaryUniqueProof{
					IndexName:   candidate.CandidateName(),
					KeyOrdinals: keyColumnOrdinals(keyColumns, layoutType),
					Layout:      layout,
				}
			}
			continue
		}
		return secondaryUniqueProof{
			IndexName:   candidate.CandidateName(),
			FullElision: true,
			KeyOrdinals: keyColumnOrdinals(keyColumns, layoutType),
			Layout:      layout,
		}
	}
	return residual
}

// secondaryUniqueProof is clause 8's three-way answer.
//
//	IndexName == ""                    — no qualifying index; the operator is
//	                                     the ordinary full distinct.
//	IndexName != "", FullElision       — R1 or R2 proved the exempt set EMPTY on
//	                                     this stream; the operator is removed.
//	IndexName != "", !FullElision      — R3: the exempt set could not be proved
//	                                     empty, so the operator is kept and
//	                                     NARROWED to dedup exactly that subset.
//
// The dependency is recorded in BOTH positive cases and for the same reason.
// R3 reads as unconditional because it removes nothing, and that reading is
// wrong: withdraw the index's uniqueness and a non-exempt row can duplicate
// another non-exempt row, which is precisely what the narrowed seen-set no
// longer catches.
type secondaryUniqueProof struct {
	IndexName   string
	FullElision bool
	// KeyOrdinals are the proving index's key columns as ordinals in Layout —
	// resolved here, at the layer whose right it is to name them (RFC-197), so
	// the executor's exempt test can be aimed at exactly those slots instead of
	// at the whole row.
	KeyOrdinals []int
	Layout      values.OrdinalDomain
}

// keyColumnOrdinals resolves an index's key column NAMES into the scan row's
// layout, once. A name the layout does not declare drops the whole list: a
// partial mapping would aim the exempt test at some of the key columns and
// silently exclude the rest, which under-approximates the exempt set and drops
// rows. Returning nil instead makes the executor test every slot.
func keyColumnOrdinals(keyColumns []string, layout values.Type) []int {
	ordinals := make([]int, 0, len(keyColumns))
	for _, col := range keyColumns {
		id, resolved := values.OrdinalOfNameIn(layout, col)
		if !resolved {
			return nil
		}
		ordinals = append(ordinals, id.Ordinal)
	}
	return ordinals
}

// nullRejectionPerKeyColumn projects a set of NULL-rejected scan-row ordinals
// onto an index's KEY COLUMN ORDER, which is the order the physical key
// component types arrive in and therefore the only order the gate can consume.
//
// Each key column needs its OWN conjunct — the result is positional and the
// gate's loop is a conjunction over it. A composite UNIQUE (a, b) with only `a`
// constrained still admits (1, NULL), (1, NULL), so partial coverage must leave
// the uncovered position false rather than being rounded up to "the filter
// rejects NULL".
//
// A key column the layout cannot resolve yields false for that position, which
// declines the proof. That is the fail-closed direction: the alternative would
// be crediting a name the layout never declared.
func nullRejectionPerKeyColumn(
	keyColumns []string, layout values.Type, nullRejectedOrds map[int]struct{},
) []bool {
	if len(nullRejectedOrds) == 0 {
		return nil
	}
	rejected := make([]bool, len(keyColumns))
	for i, col := range keyColumns {
		id, resolved := values.OrdinalOfNameIn(layout, col)
		if !resolved {
			continue
		}
		if _, ok := nullRejectedOrds[id.Ordinal]; ok {
			rejected[i] = true
		}
	}
	return rejected
}

// keyComponentTypesOf reads a candidate's authoritative physical key component
// types through the capability, so a candidate kind that cannot state them
// yields nil and fails clause 8 closed rather than being judged on a declared
// SQL type or a Go carrier.
func keyComponentTypesOf(candidate MatchCandidate) []values.Type {
	typed, ok := candidate.(interface{ GetKeyComponentTypes() []values.Type })
	if !ok {
		return nil
	}
	return typed.GetKeyComponentTypes()
}

// collectProjectedOrdinals returns the ORDINALS, in the scan row's layout, of
// the columns each projected as a BARE, top-level field reference. Only a
// projected value that IS itself a FieldValue counts: such a projection is the
// column value verbatim, so it is injective in that column, and the DISTINCT may
// be elided when the bare columns collectively cover a unique key. A projected
// value that references a key column only from INSIDE an expression — id/3,
// f(id), a+b — is NOT injective over that column (f(pk) maps many pks to one
// output), so it contributes NOTHING: crediting its buried FieldValue would
// wrongly elide DISTINCT over a many-to-one projection and emit duplicates.
//
// ok=false means the projected set could not be established and the caller must
// NOT elide: a bare reference whose ordinal does not provably index THIS layout
// (lazy, fused, a foreign domain) states no column here, and crediting it by
// the name it happens to render is how a projection of some other source's
// same-named column would be counted as covering this one's key (RFC-197).
//
// A nil map with ok=true means the inner expression is not a projection: all
// columns available (full row).
func collectProjectedOrdinals(
	expr expressions.RelationalExpression,
	layout values.OrdinalDomain,
) (map[int]struct{}, bool) {
	proj, isProj := expr.(*expressions.LogicalProjectionExpression)
	if !isProj {
		return nil, true
	}

	ords := make(map[int]struct{})
	for _, v := range proj.GetProjectedValues() {
		// TOP-LEVEL type assertion only — a FieldValue nested inside an
		// ArithmeticValue/function is deliberately not unwrapped here.
		fv, isFV := v.(*values.FieldValue)
		if !isFV {
			continue
		}
		ord, stated := fv.OrdinalIn(layout)
		if !stated {
			return nil, false
		}
		ords[ord] = struct{}{}
	}
	return ords, true
}

// scanRowLayout is the row layout the DISTINCT-elimination proof is stated in:
// the flowed type of the FullUnorderedScanExpression under the transparent
// logical operators, which is the row both the projected references and the
// metadata key columns address. A multi-record-type scan has no single column
// order, so OrdinalDomainOfType yields the unknown token and every check below
// fails closed.
func scanRowLayout(expr expressions.RelationalExpression) (values.Type, values.OrdinalDomain) {
	scan := findScanExpression(expr)
	if scan == nil {
		return nil, values.OrdinalDomain{}
	}
	t := scan.GetFlowedType()
	return t, values.OrdinalDomainOfType(t)
}

// findRecordTypes walks down through transparent LOGICAL operators
// (projection, filter, sort, distinct, unique, type-filter) to find a
// FullUnorderedScanExpression and returns its record types.
//
// Intentionally handles only logical expressions. During PLANNING,
// innerRef.Get() returns physical wrappers which this function does
// not match, so distinctEliminatedByUniqueKey returns false and the
// per-FinalMember PropDistinctRecords property check (the primary
// path) handles distinctness. This function is a fallback for tests
// and scenarios where the inner Reference contains only logical
// expressions (no PLANNING phase ran).
func findRecordTypes(expr expressions.RelationalExpression) []string {
	switch e := expr.(type) {
	case *expressions.FullUnorderedScanExpression:
		return e.GetRecordTypes()
	case *expressions.LogicalProjectionExpression:
		return findRecordTypesViaQuantifier(e.GetInner())
	case *expressions.LogicalFilterExpression:
		return findRecordTypesViaQuantifier(e.GetInner())
	case *expressions.LogicalSortExpression:
		return findRecordTypesViaQuantifier(e.GetInner())
	case *expressions.LogicalDistinctExpression:
		return findRecordTypesViaQuantifier(e.GetInner())
	case *expressions.LogicalUniqueExpression:
		return findRecordTypesViaQuantifier(e.GetInner())
	case *expressions.LogicalTypeFilterExpression:
		return findRecordTypesViaQuantifier(e.GetInner())
	}
	return nil
}

func findRecordTypesViaQuantifier(q expressions.Quantifier) []string {
	ref := q.GetRangesOver()
	if ref == nil {
		return nil
	}
	return findRecordTypes(ref.Get())
}

// uniqueKeysCovered reports whether every column in uniqueKeyCols
// appears in projectedCols. If projectedCols is nil, all columns are
// considered available (no projection = full row).
// uniqueKeysCovered reports whether every column of a unique key is projected.
//
// The key columns arrive as METADATA NAMES — a primary key's column list, an
// index definition's own columns — and that is the layer whose right it is to
// name them. Each is resolved ONCE against the scan row's layout and dies there
// (RFC-197's boundary rule); the coverage test itself is over ordinals, so a
// projection of some other source's same-named column can no longer be credited
// as covering this one's key. A key column the layout does not declare fails
// closed: no elision.
//
// A nil projected set means the full row is available.
func uniqueKeysCovered(uniqueKeyCols []string, layout values.Type, projectedOrds map[int]struct{}) bool {
	if projectedOrds == nil {
		return true
	}
	for _, col := range uniqueKeyCols {
		id, resolved := values.OrdinalOfNameIn(layout, col)
		if !resolved {
			return false
		}
		if _, covered := projectedOrds[id.Ordinal]; !covered {
			return false
		}
	}
	return true
}

// newPhysicalDistinctFor builds the physical distinct for a physical inner
// member, selecting the resume-clean STREAMING executor (distinctStreamCursor)
// when the member's ordering already makes the whole-row dedup key adjacent —
// equal rows contiguous — and the fresh-per-page hash-set otherwise. Streaming
// is sound ONLY when the ordering covers ALL output columns (the full DISTINCT
// dedup key), so a non-adjacent duplicate is never dropped and an adjacent one
// never wrongly kept; anything less conservatively keeps the hash-set (correct
// within a page). This is the ordering-aware fix for the cross-page re-admission
// of TODO.md C5 — the same adjacency predicate streaming aggregation uses for
// its grouping keys. Returns nil for a non-physical member.
//
// The distinct is its own cascades expression carrying ONE child edge (RFC-184
// W2, no physicalDistinctWrapper) — but WHICH edge is CONDITIONAL on the
// streaming mode, a constraint-preserving disentangle:
//
//   - STREAMING → FREEZE the concrete inner plan in a DETACHED single-member
//     final reference (MemoizeFinalExpression). The streaming executor is sound
//     only over the exact ordering this flag was computed for; a live edge that
//     floated to a cost-tied but differently-ordered sibling would run the
//     streaming dedup over unordered input and LEAK a duplicate. The frozen edge
//     makes planFromQuantifier resolve that exact member, never a group winner.
//   - PLAIN (hash) → carry the LIVE edge the wrapper's innerQuant presented
//     (ForEachQuantifier over InitialOf(member)). A hash distinct dedups over
//     ANY inner, so freezing buys nothing and instead strands a pre-push
//     snapshot once a push rule (push_distinct_below_filter / _through_fetch)
//     re-explores the leg — the parent would then cost an unreachable edge.
//     The live exploratory edge resolves the member's plan (== the concrete
//     inner) exactly as the wrapper did, byte-identically.
//
// A follow-up will REQUEST the dedup-key ordering (inserting an InMemorySort
// when no index provides it) so the unordered `SELECT DISTINCT col` — the
// common shape — also streams; that step must not disturb the DISTINCT +
// ORDER-BY-on-a-non-projected-column dedup-by-projected-only semantics, so it
// is deliberately separated from this ordering-detection step.
func newPhysicalDistinctFor(call *ImplementationRuleCall, member expressions.RelationalExpression) expressions.RelationalExpression {
	ph, ok := member.(physicalPlanExpression)
	if !ok {
		return nil
	}
	concreteInner := ph.GetRecordQueryPlan()
	streaming := distinctStreamingEligible(member, concreteInner)
	if streaming {
		// Freeze the ordering-critical inner: a detached single-member final
		// reference over the concrete plan whose ordering this flag was measured
		// against, so it can never float to a differently-ordered sibling.
		innerQ := expressions.ForEachQuantifier(call.MemoizeFinalExpression(concreteInner))
		return plans.NewRecordQueryDistinctPlanFromQuantifier(innerQ, true)
	}
	// Plain hash distinct: carry the live exploratory edge (what the wrapper's
	// innerQuant presented) so a later push-rule canonicalization stays reachable.
	innerQ := expressions.ForEachQuantifier(expressions.InitialOf(member))
	return plans.NewRecordQueryDistinctPlanFromQuantifier(innerQ, false)
}

// distinctStreamingEligible reports whether a distinct over innerPlan — whose
// physical realization is `member` — may use the resume-clean streaming
// executor: the member's ordering must make the whole-row dedup key adjacent
// (equal rows contiguous), the same adjacency predicate streaming aggregation
// uses for its grouping keys. This is the SOLE gate for
// RecordQueryDistinctPlan.Streaming and MUST be re-evaluated at every site that
// (re)builds a distinct over a different inner — the push-through-filter/fetch
// rules included — so a rebuild never silently downgrades a streaming distinct
// to the memory-heavy hash-set, nor promotes an unordered inner to
// streaming. (A push preserves the inner ordering but changes WHICH inner the
// distinct sits over, so the decision genuinely has to be recomputed, not
// copied.)
//
// orderingSatisfiesGroupingKeys matches dedup columns to ordering keys by BARE
// field name (case-insensitive) — sound for a single-record-source inner where
// every column name is distinct. Across a JOIN two legs can share a leaf name,
// so a coincidental name+position alignment could false-positive. That is NOT a
// row-dropping hazard: the executor still dedups by the full packed row
// (distinctKey), so a wrongly-streamed non-adjacent input at worst LEAKS a
// duplicate — the same failure class as the hash-set it replaces, never a lost
// unique row. (Streaming a join-distinct also requires the join to emit rows
// ordered by the whole dedup key, itself rare.)
func distinctStreamingEligible(member expressions.RelationalExpression, innerPlan plans.RecordQueryPlan) bool {
	dedupCols := distinctKeyColumns(innerPlan)
	return len(dedupCols) > 0 && orderingSatisfiesGroupingKeys(properties.EstimateOrdering(member), dedupCols)
}

// distinctKeyColumns returns the inner plan's output columns as Values — the
// whole-row DISTINCT dedup key (distinctKey packs exactly these positional
// slots). A projection carries its output columns as projected Values directly
// (its GetResultType is always UnknownType); a BARE-column projection yields
// FieldValues in the same representation the inner ordering's keys use, so
// orderingSatisfiesGroupingKeys can prove adjacency. A COMPUTED projection
// (g/2, f(g)) yields non-FieldValue projected values that won't match — the
// distinct is then conservatively left on the hash-set. A non-projection inner
// (e.g. SELECT DISTINCT *) exposes its columns via a RecordType schema.
func distinctKeyColumns(inner plans.RecordQueryPlan) []values.Value {
	if proj, ok := inner.(*plans.RecordQueryProjectionPlan); ok {
		return proj.GetProjections()
	}
	if rt, ok := inner.GetResultType().(*values.RecordType); ok && len(rt.Fields) > 0 {
		cols := make([]values.Value, len(rt.Fields))
		for i, f := range rt.Fields {
			cols[i] = values.NewFieldValueWithResolvedOrdinal(f.Name, i, f.FieldType)
		}
		return cols
	}
	return nil
}

var _ ImplementationRule = (*ImplementDistinctFinalRule)(nil)

// findScanExpression is findRecordTypes' sibling: the same walk down through the
// transparent logical operators, returning the scan ITSELF so its flowed row
// type is available. Kept beside findRecordTypes rather than folded into it
// because the two answer different questions of the same node, and a caller
// wanting the layout must not have to re-derive it from a record-type name.
func findScanExpression(expr expressions.RelationalExpression) *expressions.FullUnorderedScanExpression {
	switch e := expr.(type) {
	case *expressions.FullUnorderedScanExpression:
		return e
	case *expressions.LogicalProjectionExpression:
		return findScanViaQuantifier(e.GetInner())
	case *expressions.LogicalFilterExpression:
		return findScanViaQuantifier(e.GetInner())
	case *expressions.LogicalSortExpression:
		return findScanViaQuantifier(e.GetInner())
	case *expressions.LogicalDistinctExpression:
		return findScanViaQuantifier(e.GetInner())
	case *expressions.LogicalUniqueExpression:
		return findScanViaQuantifier(e.GetInner())
	case *expressions.LogicalTypeFilterExpression:
		return findScanViaQuantifier(e.GetInner())
	}
	return nil
}

func findScanViaQuantifier(q expressions.Quantifier) *expressions.FullUnorderedScanExpression {
	ref := q.GetRangesOver()
	if ref == nil {
		return nil
	}
	return findScanExpression(ref.Get())
}
