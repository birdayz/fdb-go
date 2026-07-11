package query

import (
	"fmt"
	"strconv"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/functions"
	"fdb.dev/pkg/relational/core/query/logical"
)

// ScalarSubqueryPlan pairs a correlation alias with a logical operator
// tree for a scalar subquery. Collected during translation and passed
// to the executor for pre-evaluation.
type ScalarSubqueryPlan struct {
	Alias values.CorrelationIdentifier
	Plan  logical.LogicalOperator
}

// TranslateToCascades converts a logical.LogicalOperator tree into a
// cascades RelationalExpression tree rooted in a Reference. This is
// the bridge between the SQL parser's logical plan and the Cascades
// optimizer.
//
// Returns the root Reference suitable for passing to Planner.Plan().
// Returns nil if the operator tree contains shapes that can't be
// translated (unsupported operators fall through to nil).
func TranslateToCascades(op logical.LogicalOperator) *expressions.Reference {
	ref, _ := TranslateToCascadesWithSubqueries(op, nil)
	return ref
}

// TranslateToCascadesWithSubqueries is like TranslateToCascades but
// also returns any scalar subquery plans collected during translation.
// These must be planned independently and pre-evaluated by the
// executor before running the main plan.
//
// md carries the record metadata used to source join-leg columns when building
// the source-anchored join result value (RFC-077 7.6). Pass nil to keep the legacy
// opaque-seed behavior — the no-md callers today are TranslateToCascades (used for
// scalar-subquery translation, which has no md in scope) and DML translation.
// (Tests pass real md where they exercise anchoring.) The scan leaf is NEVER typed
// from md (it stays Type.AnyRecord/UnknownType, matching Java — see RFC-077 v3
// amendment); md is consulted only to enumerate a leg's columns for the anchored
// RecordConstructor.
func TranslateToCascadesWithSubqueries(op logical.LogicalOperator, md *recordlayer.RecordMetaData) (*expressions.Reference, []ScalarSubqueryPlan) {
	ref, subs, _ := TranslateToCascadesWithError(op, md)
	return ref, subs
}

// TranslateToCascadesWithError is TranslateToCascadesWithSubqueries plus an
// explicit translation error. A non-nil error carries a specific SQL error
// code (e.g. ErrCodeWrongObjectType for AT-ordinality on a non-array source,
// RFC-142) that a bare nil ref (untranslatable → UNSUPPORTED_QUERY) cannot.
// The caller surfaces it verbatim instead of the generic "could not plan".
func TranslateToCascadesWithError(op logical.LogicalOperator, md *recordlayer.RecordMetaData) (*expressions.Reference, []ScalarSubqueryPlan, error) {
	t := &cascadesTranslator{
		md:              md,
		cteScope:        make(map[string]logical.LogicalOperator),
		cteShadowStack:  make(map[string][]logical.LogicalOperator),
		cteExprScope:    make(map[string]expressions.RelationalExpression),
		cteColumnsScope: make(map[string][]values.Field),
	}
	ref := t.translateRef(op)
	return ref, t.scalarSubqueries, t.translateErr
}

type cascadesTranslator struct {
	md       *recordlayer.RecordMetaData
	cteScope map[string]logical.LogicalOperator
	// cteShadowStack tracks, per upper-cased name, the OUTER bindings a
	// same-named registration shadowed (translateCTE pushes; nil = the name
	// was unbound outside). CTE bodies translate LAZILY at scan resolution,
	// so lexical scoping must be reconstructed there: resolving a name to a
	// registered body pops one level for the body's own translation — the
	// body's references to its OWN name then resolve against the DEFINING
	// scope (the shadowed outer binding: a derived-table alias-carrier
	// wrapping `SELECT * FROM c` inside `WITH c AS (…)` reads the WITH
	// body — or the real table when nothing was shadowed). Without this,
	// the wrapper's registration silently rebound the outer name to itself
	// and `WITH c … FROM (SELECT * FROM c) c` returned zero rows.
	cteShadowStack map[string][]logical.LogicalOperator
	cteExprScope   map[string]expressions.RelationalExpression
	// cteColumnsScope holds the OUTPUT column schema of each pre-translated CTE
	// (recursive CTE / temp-table self-reference) registered in cteExprScope,
	// keyed by upper-cased CTE name (RFC-077 7.6). cteExprScope stores an opaque
	// RelationalExpression whose column names legColumns cannot recover; this
	// parallel map records them so a CTE reference used as a JOIN LEG anchors
	// (FieldValue(QOV(cteAlias), col) per column). nil/absent entry → not
	// column-derivable → the leg cannot anchor (a join over it is untranslatable;
	// the opaque-merge fallback was retired in RFC-077 7.6).
	cteColumnsScope  map[string][]values.Field
	scalarSubqueries []ScalarSubqueryPlan
	// translateErr records the FIRST translation error that carries a
	// specific SQL error code the bare nil-ref signal cannot (RFC-142:
	// AT-ordinality on a non-array source → ErrCodeWrongObjectType). Set once
	// (first writer wins) so the original cause surfaces; the caller reads it
	// when ref is nil and reports it instead of the generic "could not plan".
	translateErr error

	// inInnerCluster is the RFC-173 Slice 2 enclosure flag: true while
	// translating a subtree whose selects merge (post-flattening) into an
	// enclosing name-model select — inner-join legs and the existential/unnest
	// flatten legs. A join translated under the flag is a leg of a bigger
	// cluster and stays name-model (ordinalWedgeGate). The flag is PRESERVED
	// through transparent translations (filter/project/CTE-body inlining —
	// SelectMergeRule merges through their selects) and RESET at opaque
	// boundaries (translateOp entry: aggregate/distinct/sort/limit/union/DML;
	// translateJoin: outer-join legs). Safety direction: a missed reset leaves
	// the flag true → the nested join under-gates to the name model — never
	// the reverse.
	inInnerCluster bool
	// existsFoldHasChain is set while translateProjectOverExistsFilter folds a
	// projection whose chain carries intervening Sort/Limit operators. The B1
	// existential wrap (translateExistsOverGatheredCluster) DECLINES in that
	// case: the fold's chain re-application (sort keys via sortKeySourceValue +
	// the hidden-field cleanup projection) re-emits UNREBASED leg-qualified
	// values above the wrap — resolvable over a name-model output row (qualified
	// merged keys), unbound (silent NULL) over the wrap's positional output.
	// Ordering over the wrap needs those emissions rebased to output names — a
	// booked extension; until then chained shapes keep the name-model plan.
	existsFoldHasChain bool
	// underAggregate is set while lowering the INPUT of a GROUP BY / aggregate
	// (translateAggregate). A gathered multi-source unnest's grouped element / leg
	// reference used to group every row under NULL (`SELECT EL, COUNT(*) FROM a, a.arr
	// AS EL, b GROUP BY EL`) because the flat positional seed had no name key for it.
	// The gather now UN-COLLAPSES: translateGatheredUnnestCluster returns the RAW
	// per-leg seed and translateAggregate POSITIONALLY BAKES the group keys / operands
	// over it — leg columns via OrdinalSeedLegWindows, the element by its rc slot
	// (fieldValueReferencesInner) — so the grouped read resolves by ordinal, no
	// name-keyed wrap. This flag is only what marks the input as an aggregate INPUT
	// (so the bake site knows to look); the wrap it once gated is deleted.
	underAggregate bool
	// unnestUnderExistential is set while lowering a lateral unnest that is the
	// OUTER of an EXISTS composition (translateUnnestExistsFilter). RFC-173 commit
	// 5a ORDINALIZES this class: the W4c ordinal-seed gate is taken when the outer
	// is a SINGLE ALIAS (unnestExistsSeedSafe), and the EXISTS correlation rebases
	// on ONE layout authority (translateUnnestExistsFilter) — the translator
	// pre-bakes when the seed has no executor windows (mixed/AT-only), else the
	// executor's below-FOD window branch owns it (fully-baked AS+AT). A MULTI-ALIAS
	// outer (a merge-opaque FULL OUTER box) still stays name-model: the ordinal
	// rebase cannot disambiguate two aliases' same-named columns. The flag is what
	// unnestExistsSeedSafe keys the single-alias restriction on. Preserved across
	// the unnest lowering only; reset at every other seed by the translator's
	// normal flow.
	unnestUnderExistential bool
	// unnestExistsScopeCollision forces the under-EXISTS unnest to the ANCHORED
	// (name-model) seed when an EXISTS inner subquery's plan carries a source
	// alias equal to an outer FROM leg's. Since the collision mint
	// (buildCorrelatedExists), a SINGLE-TABLE catalog inner is born under a
	// unique correlation, so it can never trip this — the flag now fires only
	// for the UNMINTED inner shapes (multi-source and CTE inners, which keep
	// their SQL names). Those route by source-alias name keys the rename never
	// touches, so a colliding name would be served from the wrong scope at the
	// merged row; declining to name-model is the conservative floor until
	// mint-per-leg lands (booked). Scoped like unnestUnderExistential.
	unnestExistsScopeCollision bool
	// unnestOuterConjunctOnBoxLeg forces the lateral unnest to the ANCHORED
	// (name-model) seed when the enclosing filter has a regular (NON-EXISTS) WHERE
	// conjunct referencing a MULTI-ALIAS box leg (`(a FULL OUTER b), a.arr AS x
	// WHERE a.col = V [AND EXISTS(…)]`). The regular conjunct is merged into the
	// unnest SELECT, where the box's output is name-keyed — a positional bake there
	// does NOT reach the executor's positional row (unlike an EXISTS-inner ref,
	// which the below-FOD hoist rebases), so an ordinal box seed leaves the
	// conjunct's box-leg ref unresolvable (malformed plan). It is set in BOTH the
	// EXISTS path (translateUnnestExistsFilter) AND the non-EXISTS filter-over-unnest
	// merge, and checked in unnestExistsSeedSafe BEFORE the under-existential gate,
	// so it declines in either. Full ordinalization of such a conjunct is a
	// follow-on; until then the shape stays name-model (correct via qualified
	// keys). Only the MULTI-alias box declines — a single-source outer's pristine
	// prefix at offset 0 resolves a bare conjunct fine. Scoped like
	// unnestUnderExistential.
	unnestOuterConjunctOnBoxLeg bool
	// enclosedGatherCache memoizes the flat select translateFilter's
	// enclosed-gather PROBE built, keyed by the ORIGINAL join root, so the
	// dispatch (translateEnclosedUnnestGather via translateJoin) consumes it
	// instead of translating the whole cluster a second time. Consume-once
	// (deleted on read): the probe runs immediately before the dispatch on
	// the same tree, and a stale entry surviving a decline elsewhere would be
	// a wrong-tree translation. This also closes the latent side-state
	// hazard both W5 impl reviews flagged: a discarded probe translation
	// re-runs translateRef per leg, which is pure ONLY while ordinalEligible
	// bars subquery-bearing legs (a scalar-subquery leg would double-append
	// t.scalarSubqueries — the translateUnnestJoin call-ordering hazard).
	enclosedGatherCache map[*logical.LogicalJoin]expressions.RelationalExpression
	// wedgeGate records the Slice 2 gate decision per translateJoin seed —
	// consumed by the W3 ordinal seed, pinned by tests. Lazily initialized so
	// hand-built test translators need no constructor change.
	wedgeGate map[*logical.LogicalJoin]wedgeGateDecision
}

// setTranslateErr records a translation error (first writer wins) so a
// specific SQL error code survives to the caller. RFC-142.
func (t *cascadesTranslator) setTranslateErr(err error) {
	if t.translateErr == nil {
		t.translateErr = err
	}
}

// tableColumns returns a real table's columns (name + proto-derived type) from
// metadata, or nil when md is absent or the table can't be resolved. Field names
// are upper-cased to match the rest of the cascades layer's column naming. Used to
// source join-leg columns for the source-anchored join result value (RFC-077 7.6);
// it does NOT type the scan leaf.
func (t *cascadesTranslator) tableColumns(table string) []values.Field {
	if t.md == nil {
		return nil
	}
	rt := t.resolveRecordType(table)
	if rt == nil || rt.Descriptor == nil {
		return nil
	}
	protoFields := rt.Descriptor.Fields()
	fields := make([]values.Field, 0, protoFields.Len())
	for i := 0; i < protoFields.Len(); i++ {
		fd := protoFields.Get(i)
		fields = append(fields, values.Field{
			Name:      strings.ToUpper(string(fd.Name())),
			FieldType: fieldTypeForFD(fd),
			Ordinal:   i,
		})
	}
	return fields
}

// resolveRecordType resolves a table name to its record type CASE-INSENSITIVELY.
// The SQL path upper-cases table names in the logical plan (Scan(ORDER)), but the
// metadata keys record types under their proto names (mixed case, e.g. "Order"),
// so a direct GetRecordType("ORDER") misses. The relational layer is
// case-insensitive (Java's SemanticAnalyzer resolves identifiers case-folded), so
// fall back to a case-insensitive scan when the exact lookup misses. Without this
// every real-table join seed fell back to the opaque merge — the columns were
// unreachable (RFC-077 7.6).
//
// The fallback picks the lexicographically-smallest matching proto name so the
// result is DETERMINISTIC even in the (metadata-invalid) case of two record types
// that differ only by case — map iteration order is not stable. In well-formed
// metadata proto names are unique, so at most one name matches and the order is moot.
func (t *cascadesTranslator) resolveRecordType(table string) *recordlayer.RecordType {
	if rt := t.md.GetRecordType(table); rt != nil {
		return rt
	}
	var bestName string
	var best *recordlayer.RecordType
	for name, rt := range t.md.RecordTypes() {
		if strings.EqualFold(name, table) && (best == nil || name < bestName) {
			bestName, best = name, rt
		}
	}
	return best
}

// fieldTypeForFD maps a protoreflect.FieldDescriptor to a values.Type, mirroring
// jdbcTypeNameForFD (pkg/relational/core/embedded/select_helpers.go). Repeated/map
// and non-UUID message fields collapse to values.UnknownType — 7.6 doesn't model
// nested/array element types for the anchored leg columns. Columns are nullable
// (the flowed leg row doesn't carry per-column NOT NULL constraints).
func fieldTypeForFD(fd protoreflect.FieldDescriptor) values.Type {
	if fd.IsList() || fd.IsMap() {
		return values.UnknownType
	}
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return values.NewPrimitiveType(values.TypeCodeBoolean, true)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return values.NewPrimitiveType(values.TypeCodeInt, true)
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return values.NewPrimitiveType(values.TypeCodeLong, true)
	case protoreflect.FloatKind:
		return values.NewPrimitiveType(values.TypeCodeFloat, true)
	case protoreflect.DoubleKind:
		return values.NewPrimitiveType(values.TypeCodeDouble, true)
	case protoreflect.StringKind:
		return values.NewPrimitiveType(values.TypeCodeString, true)
	case protoreflect.BytesKind:
		return values.NewPrimitiveType(values.TypeCodeBytes, true)
	case protoreflect.MessageKind:
		if msg := fd.Message(); msg != nil && string(msg.FullName()) == functions.UUIDProtoMessageName {
			return values.NewPrimitiveType(values.TypeCodeUuid, true)
		}
		return values.UnknownType
	}
	return values.UnknownType
}

// legColumns derives the OUTPUT columns of a logical sub-plan as the field set a
// source-anchored join result value would carry for that leg (RFC-077 7.6 Option
// B). The names it returns are EXACTLY the field names NewAnchoredJoinRecord
// emits, so a parent join's anchored RC composes its legs consistently — a leg
// that is itself a join exposes already-qualified (dotted) names that the parent
// propagates verbatim (the nested-join case NewAnchoredJoinRecord handles).
//
// Per-shape derivation (mirrors Option B's legOutputColumns):
//   - LogicalScan      → the table's bare columns from metadata (tableColumns).
//   - LogicalFilter    → its inner's columns (a filter is row-shape-preserving).
//   - LogicalLimit     → its inner's columns (limit is row-shape-preserving).
//   - LogicalJoin      → the field set of the join's anchored RC over its two
//     legs (qualified ALIAS.COL + bare-unique, dotted-propagated).
//   - LogicalProject   → the projected column names (the SELECT list).
//   - anything else (aggregate / distinct / union / cte / subquery) → nil.
//
// Returns nil whenever any required source is unavailable (no md, an unresolvable
// table, an unsupported shape) — the seed site (buildJoinResultValue) then treats
// the join as untranslatable (the retired opaque seed fallback is gone). The Field
// types are best-effort (UnknownType for derived shapes); only the NAMES are
// load-bearing for name-based resolution.
func (t *cascadesTranslator) legColumns(op logical.LogicalOperator) []values.Field {
	switch o := op.(type) {
	case *logical.LogicalScan:
		// A CTE/derived-table scan resolves to its BODY, not a real table —
		// translateScan honors cteScope/cteExprScope (a CTE name SHADOWS a real
		// table). legColumns mirrors that (RFC-077 7.6):
		//   - cteExprScope holds a PRE-TRANSLATED body (recursive-CTE reference /
		//     temp-table self-reference); its output columns are not readable from
		//     the RelationalExpression, so cteColumnsScope records them alongside —
		//     return that schema so the recursive-CTE leg anchors (nil entry → not
		//     derivable → the leg cannot anchor, a join over it is untranslatable);
		//   - cteScope holds the logical body: derive its output columns so the CTE
		//     leg anchors. The CTE is REMOVED from scope while deriving the body
		//     (exactly like translateScan) so a scan inside the body that references
		//     the same name resolves to the REAL table, not back to the CTE —
		//     otherwise legColumns recurses forever (the CTE-shadow stack overflow).
		key := strings.ToUpper(o.Table)
		if _, ok := t.cteExprScope[key]; ok {
			return t.cteColumnsScope[key]
		}
		if body, ok := t.cteScope[key]; ok {
			var cols []values.Field
			t.inCTEDefiningScope(key, body, func() {
				cols = t.derivedOutputColumns(body)
			})
			return cols
		}
		return t.tableColumns(o.Table)
	case *logical.LogicalFilter:
		return t.legColumns(o.Input)
	case *logical.LogicalLimit:
		return t.legColumns(o.Input)
	case *logical.LogicalJoin:
		left, right := o.Left, o.Right
		if o.Kind == logical.JoinRight {
			left, right = right, left
		}
		leftCols := t.legColumns(left)
		rightCols := t.legColumns(right)
		if leftCols == nil || rightCols == nil {
			return nil
		}
		leftAlias := values.NamedCorrelationIdentifier(sourceAlias(left))
		rightAlias := values.NamedCorrelationIdentifier(sourceAlias(right))
		rc := values.NewAnchoredJoinRecord([]values.AnchoredJoinLeg{
			{Alias: leftAlias, Columns: leftCols},
			{Alias: rightAlias, Columns: rightCols},
		})
		// A join leg exposes ONLY its already-qualified (DOTTED) columns to a parent
		// — the SOURCE-ACCURATE per-table forms (O.ID, C.PRICE, …). The anchored RC
		// ALSO carries bare names (its OWN resolution convenience at this level), but
		// those must NOT propagate: a parent re-qualifies a propagated bare under
		// sourceAlias(join)=right-leg, and a name from the right leg then collides
		// with its verbatim dotted key (NewRecordConstructorValue would suffix it
		// "_2" — a spurious key the opaque merge never produces). A buried column is
		// referenced via its dotted form after PartitionSelectRule rebasing, never
		// bare. (RFC-077 7.6 — the unique-bare
		// concern is pinned by TestFDB_NestedJoinUnqualifiedProjection.)
		var fields []values.Field
		for _, f := range rc.Fields {
			if strings.Contains(f.Name, ".") {
				fields = append(fields, values.Field{Name: f.Name, FieldType: values.UnknownType, Ordinal: len(fields)})
			}
		}
		return fields
	case *logical.LogicalProject:
		if len(o.Projections) == 0 {
			return nil
		}
		fields := make([]values.Field, len(o.Projections))
		for i := range o.Projections {
			name := o.Projections[i]
			if i < len(o.Aliases) && o.Aliases[i] != "" {
				name = o.Aliases[i]
			}
			fields[i] = values.Field{Name: strings.ToUpper(name), FieldType: values.UnknownType, Ordinal: i}
		}
		return fields
	case *logical.LogicalUnnest:
		// A lateral unnest leg exposes its AS-bound element column (and, with
		// ordinality, the AT-bound ordinal). The element/ordinal types are
		// best-effort (UnknownType) — only the NAMES are load-bearing for
		// name-based resolution by a parent join's anchored RC. RFC-142.
		var cols []values.Field
		if o.Alias != "" {
			cols = append(cols, values.Field{Name: strings.ToUpper(o.Alias), FieldType: values.UnknownType, Ordinal: len(cols)})
		}
		if o.AtAlias != "" {
			cols = append(cols, values.Field{Name: strings.ToUpper(o.AtAlias), FieldType: values.NotNullInt, Ordinal: len(cols)})
		}
		return cols
	case *logical.LogicalSort:
		// Row-shape-preserving: the sort's output columns are its inner's.
		return t.legColumns(o.Input)
	case *logical.LogicalDistinct:
		// Row-shape-preserving: DISTINCT does not change the column set.
		return t.legColumns(o.Input)
	case *logical.LogicalUnion:
		return t.unionOutputColumns(o)
	case *logical.LogicalAggregate:
		// Output columns = the GROUP BY keys followed by the aggregate output
		// column names (alias when present, else the aggregate text), mirroring
		// extractOutputColumns / buildAggColumns.
		return aggregateOutputColumns(o)
	case *logical.LogicalCTE:
		// A CTE-wrapped derived table used as a JOIN LEG (e.g. FROM a,
		// (SELECT …) b): translateCTE registers the body under the CTE name and
		// translates Main (a pass-through Scan of the name), so the leg's output
		// columns ARE the body's output columns — renamed by ColumnAliases when
		// present (WITH b(x,y) AS …), exactly as translateCTE wraps the body in a
		// renaming Project. A recursive CTE leg is not column-derivable here → nil
		// (the leg cannot anchor; the opaque-merge fallback was retired).
		if o.Recursive {
			return nil
		}
		if len(o.ColumnAliases) > 0 {
			fields := make([]values.Field, len(o.ColumnAliases))
			for i, name := range o.ColumnAliases {
				fields[i] = values.Field{Name: strings.ToUpper(name), FieldType: values.UnknownType, Ordinal: i}
			}
			return fields
		}
		return t.derivedOutputColumns(o.Body)
	default:
		// Subquery / Explode / DML and other non-row-producing shapes are not
		// column-derivable here → nil. A join seed with a non-derivable leg is
		// untranslatable (the opaque-merge fallback was retired in RFC-077 7.6);
		// every production query reaches a derivable leg shape (proven no-fallback).
		return nil
	}
}

// derivedOutputColumns derives a logical sub-plan's OUTPUT columns as a
// values.Field list (RFC-077 7.6) for shapes that define a column SCHEMA but
// are not themselves a join leg's quantifier source — used for CTE/derived-table
// bodies. It mirrors legColumns for the row-shape-preserving / project / aggregate
// shapes but, for a Project, returns the projected column NAMES (the body's
// output schema) so the CTE leg's columns match what the body flows. Returns nil
// for an underivable shape.
func (t *cascadesTranslator) derivedOutputColumns(op logical.LogicalOperator) []values.Field {
	switch o := op.(type) {
	case *logical.LogicalProject:
		if len(o.Projections) == 0 {
			return nil
		}
		fields := make([]values.Field, len(o.Projections))
		for i := range o.Projections {
			name := o.Projections[i]
			if i < len(o.Aliases) && o.Aliases[i] != "" {
				name = o.Aliases[i]
			}
			fields[i] = values.Field{Name: strings.ToUpper(name), FieldType: values.UnknownType, Ordinal: i}
		}
		return fields
	case *logical.LogicalAggregate:
		return aggregateOutputColumns(o)
	case *logical.LogicalDistinct:
		return t.derivedOutputColumns(o.Input)
	case *logical.LogicalSort:
		return t.derivedOutputColumns(o.Input)
	case *logical.LogicalLimit:
		return t.derivedOutputColumns(o.Input)
	case *logical.LogicalFilter:
		return t.derivedOutputColumns(o.Input)
	case *logical.LogicalUnion:
		return t.unionOutputColumns(o)
	case *logical.LogicalScan:
		return t.legColumns(o)
	case *logical.LogicalJoin:
		return t.legColumns(o)
	}
	return nil
}

// unionOutputColumns returns a UNION's output column schema for anchoring it as a
// join leg. SQL exposes the FIRST branch's names; the executor unions later
// branches by POSITION (remapUnionColumnsByPosition, keyed on planColumnNamesWithMD).
// That position-remap is reliable for PROJECTION/scan-schema'd branches — verified
// e2e: `(SELECT id AS x … UNION ALL SELECT v AS y …)` joins correctly — so anchoring
// a leg with mismatched branch aliases to the first branch's names is sound there.
//
// It is NOT reliable for an AGGREGATE-schema'd branch: planColumnNamesWithMD unwraps
// the aggregate to its input scan's column names, so a differently-aliased aggregate
// branch is not remapped to the first branch's name and its rows read as NULL —
// silently dropping join matches (a pre-existing executor gap, verified wrong on
// master too; tracked as TODO 7.6-union-remap). So when branch names DIFFER, anchor
// only if every branch's schema-defining node is normalizable (projection/scan); an
// aggregate-schema'd mismatched-alias union leg returns nil → untranslatable, a clean
// "unsupported" error rather than silently-wrong rows. When branch names
// AGREE the remap is a no-op, so any shape is safe. Returns nil for no branches / an
// underivable first branch.
func (t *cascadesTranslator) unionOutputColumns(u *logical.LogicalUnion) []values.Field {
	if len(u.Inputs) == 0 {
		return nil
	}
	first := t.derivedOutputColumns(u.Inputs[0])
	if first == nil {
		return nil
	}
	allAgree := true
	allNormalizable := true
	for _, br := range u.Inputs {
		bc := t.derivedOutputColumns(br)
		if len(bc) != len(first) {
			return nil
		}
		for i := range bc {
			if bc[i].Name != first[i].Name {
				allAgree = false
			}
		}
		if !t.unionBranchNormalizable(br) {
			allNormalizable = false
		}
	}
	if allAgree || allNormalizable {
		return first
	}
	return nil
}

// unionBranchNormalizable reports whether the executor's union position-remap can
// remap this branch's columns to the first branch's names — i.e. whether the
// branch's SCHEMA-defining node is a projection or scan (planColumnNamesWithMD
// reports those branches' true output names). An AGGREGATE-schema'd branch is NOT
// normalizable (the executor unwraps it to its input scan's names — TODO
// 7.6-union-remap). Mirrors derivedOutputColumns's recursion through the
// row-shape-preserving wrappers; an unknown shape is conservatively not normalizable.
func (t *cascadesTranslator) unionBranchNormalizable(op logical.LogicalOperator) bool {
	switch o := op.(type) {
	case *logical.LogicalProject, *logical.LogicalJoin:
		return true
	case *logical.LogicalScan:
		// A scan may be a CTE/derived-table reference (translateScan resolves it from
		// the CTE body, not a real table). A real-table scan is remappable, but a
		// CTE-reference scan is only remappable if its BODY is — a CTE whose body is a
		// bare aggregate is NOT (the executor unwraps it to its input scan's
		// names). Resolve cteScope and recurse, mirroring legColumns (remove-while-
		// recursing so a same-named scan inside the body resolves to the real table,
		// not back to the CTE). A pre-translated (recursive) CTE ref is unverifiable →
		// conservatively not normalizable.
		key := strings.ToUpper(o.Table)
		if _, ok := t.cteExprScope[key]; ok {
			return false
		}
		if body, ok := t.cteScope[key]; ok {
			var n bool
			t.inCTEDefiningScope(key, body, func() {
				n = t.unionBranchNormalizable(body)
			})
			return n
		}
		return true
	case *logical.LogicalAggregate:
		// Bare aggregate branch (no Project).
		if len(o.Aggregates) < 1 {
			return false // 0-aggregate (group-only) shape — distinct concern, gated.
		}
		// UNGROUPED: unchanged from RFC-080. An ungrouped aggregate has no aggregate-index
		// candidate (groupingCount==0) so it always plans as StreamingAgg; RFC-080 allowed these
		// union join legs and they work — do NOT re-gate them here (regressing previously-working
		// ungrouped legs). Any residual ungrouped logical-vs-physical name divergence is a
		// pre-existing RFC-080 matter for the naming-unification follow-up, not RFC-081's scope.
		if len(o.GroupKeys) == 0 {
			return true
		}
		// GROUPED (RFC-081): a bare grouped aggregate can plan as AggregateIndex / MultiIntersection,
		// whose canonical row key can DIVERGE from the logical leg-schema name (aggregateOutputColumns,
		// the raw aggregate text) — so the executor's position-remap reads a missing key → NULL. The
		// names agree only for COUNT(*) and FUNC(<bare column>); a qualified operand (SUM(t.c) →
		// physical SUM(C)), a constant (COUNT(1)/COUNT(NULL) → grouped count-star index COUNT(*)), an
		// expression, or DISTINCT diverge → gate (clean error, never wrong rows). Unifying logical and
		// physical aggregate naming so the divergent forms work is a follow-up.
		return aggregateNamesStableForUnion(o)
	case *logical.LogicalDistinct:
		return t.unionBranchNormalizable(o.Input)
	case *logical.LogicalSort:
		return t.unionBranchNormalizable(o.Input)
	case *logical.LogicalLimit:
		return t.unionBranchNormalizable(o.Input)
	case *logical.LogicalFilter:
		return t.unionBranchNormalizable(o.Input)
	case *logical.LogicalUnion:
		if len(o.Inputs) == 0 {
			return false
		}
		for _, br := range o.Inputs {
			if !t.unionBranchNormalizable(br) {
				return false
			}
		}
		return true
	case *logical.LogicalCTE:
		return t.unionBranchNormalizable(o.Body)
	}
	return false
}

// aggregateNamesStableForUnion reports whether every aggregate in a bare aggregate union
// branch has a STABLE output name — i.e. the logical leg-schema name (aggregateOutputColumns,
// the raw aggregate text) equals the physical row key the executor writes (StreamingAgg
// aggResultName / AggregateIndex canonical). Stable iff each aggregate is COUNT(*) or
// FUNC(<bare column identifier>); a qualified operand (SUM(t.c)), a constant (COUNT(1)), an
// expression (SUM(a*b)), or DISTINCT canonicalizes differently between the two, so the union
// position-remap would read a missing key → NULL (RFC-081). False for a 0-aggregate branch.
//
// The aggregate TEXT is the reliable signal: AggregateOperands is nil for many shapes (e.g.
// SUM(col)) depending on the build path, and a.Aggregates is canonical planner output (not raw
// SQL), so inspecting it is sound here.
func aggregateNamesStableForUnion(a *logical.LogicalAggregate) bool {
	if len(a.Aggregates) == 0 || a.HasDistinctAggregate {
		return false
	}
	for i, text := range a.Aggregates {
		// A constant operand — COUNT(1), COUNT(NULL), COUNT(TRUE) — folds into count-star,
		// so a grouped aggregate index reports COUNT(*) ≠ the logical text. The resolved
		// operand reliably distinguishes a literal (ConstantValue) from a column, which the
		// text cannot (COUNT(NULL)'s arg "NULL" looks like an identifier). Literals resolve
		// even where a column operand is left nil, so this catch is sound.
		//
		// This deliberately does NOT reuse expressions.IsCountStar (RFC-164 WS-3): that
		// classifier answers "is this COUNT count-star?" for a SINGLE COUNT aggregate; here
		// the question is union-branch NAME stability for ANY aggregate function, and the
		// gate is a conservative any-constant-operand reject (SUM(1), MIN(NULL) too) — a
		// different question at a different scope, not a fourth copy of the count-star rule.
		if i < len(a.AggregateOperands) {
			if _, isConst := a.AggregateOperands[i].(*values.ConstantValue); isConst {
				return false
			}
		}
		arg, ok := aggregateArgText(text)
		if !ok {
			return false
		}
		if arg == "*" {
			continue // COUNT(*)
		}
		if !isBareColumnIdentifier(arg) {
			return false // qualified / expression / numeric-literal operand → name diverges
		}
	}
	return true
}

// aggregateArgText returns the argument of a canonical aggregate text "FUNC(arg)" — the
// content between the first '(' and the last ')'. ok=false when not in that shape.
func aggregateArgText(text string) (string, bool) {
	openIdx := strings.IndexByte(text, '(')
	closeIdx := strings.LastIndexByte(text, ')')
	if openIdx < 0 || closeIdx <= openIdx {
		return "", false
	}
	return text[openIdx+1 : closeIdx], true
}

// isBareColumnIdentifier reports whether s is a single unqualified SQL identifier
// ([A-Za-z_][A-Za-z0-9_]*): no qualifier dot, whitespace (DISTINCT), operator (expression),
// '*', or leading digit (numeric literal). Exactly the operands whose FUNC(s) name is identical
// in the logical schema and the physical row key.
func isBareColumnIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c == '_':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// aggregateOutputColumns returns a LogicalAggregate's output column schema:
// the GROUP BY keys (bare column names, upper-cased) followed by each
// aggregate's output name (alias when present, else the aggregate text).
// Mirrors extractOutputColumns(LogicalAggregate). Types are UnknownType
// (only names are load-bearing for name-based resolution). Returns nil if
// the aggregate has no output columns.
func aggregateOutputColumns(a *logical.LogicalAggregate) []values.Field {
	var fields []values.Field
	for _, k := range a.GroupKeys {
		fields = append(fields, values.Field{Name: strings.ToUpper(k), FieldType: values.UnknownType, Ordinal: len(fields)})
	}
	for i, agg := range a.Aggregates {
		name := agg
		if i < len(a.Aliases) && a.Aliases[i] != "" {
			name = a.Aliases[i]
		}
		fields = append(fields, values.Field{Name: strings.ToUpper(name), FieldType: values.UnknownType, Ordinal: len(fields)})
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// buildJoinResultValue builds the result value for a binary join seed (RFC-077
// 7.6 Option B): the source-anchored RecordConstructorValue —
// FieldValue(QOV(leg), col) per column, named by NewAnchoredJoinRecord — so field
// pull-up resolves through composeFieldOverConstructor by name, anchored to the
// source quantifier. left/right are the POST-swap operands (RIGHT-join
// normalization happens at the call site), so the leg order matches the [outer,
// inner] ordering the column derivation + reversal signal read.
//
// Returns nil when a leg's output columns are not derivable (legColumns nil) or a
// leg alias is empty — the retired opaque merge seed fallback was removed in
// RFC-077 7.6 (proven unreachable for every md-bearing production query; only the
// catalog-free nil-md TranslateToCascades path, used by unit tests, can't derive a
// leg's columns). The caller treats nil as an untranslatable join.
func (t *cascadesTranslator) buildJoinResultValue(left, right logical.LogicalOperator, leftAlias, rightAlias string) values.Value {
	leftCols := t.legColumns(left)
	rightCols := t.legColumns(right)
	// Both legs must have a non-empty alias (the anchored RC keys columns by
	// QOV(alias); a zero alias would panic NewQuantifiedObjectValue) AND derivable
	// columns.
	if leftAlias == "" || rightAlias == "" || leftCols == nil || rightCols == nil {
		return nil
	}
	if producerCensusObserver != nil {
		producerCensusObserver(ProducerCensusRecord{Producer: "P4", Enclosed: t.inInnerCluster, Shape: fmt.Sprintf("%T|%T", left, right)})
	}
	return values.NewAnchoredJoinRecord([]values.AnchoredJoinLeg{
		{Alias: values.NamedCorrelationIdentifier(leftAlias), Columns: leftCols},
		{Alias: values.NamedCorrelationIdentifier(rightAlias), Columns: rightCols},
	})
}

func (t *cascadesTranslator) translateRef(op logical.LogicalOperator) *expressions.Reference {
	expr := t.translateOp(op)
	if expr == nil {
		return nil
	}
	return expressions.InitialOf(expr)
}

// translateSubqueryRef translates an EXISTS-subquery plan. RFC-173 Slice 2:
// an existential quantifier's child select is NEVER merged into its parent
// (SelectMergeRule only targets ForEach quantifiers), so the subquery roots a
// FRESH cluster regardless of where the enclosing select sits — a 2-way join
// inside an EXISTS gates on its own arity, not the outer enclosure.
func (t *cascadesTranslator) translateSubqueryRef(op logical.LogicalOperator) *expressions.Reference {
	prevEnclosure := t.inInnerCluster
	t.inInnerCluster = false
	ref := t.translateRef(op)
	t.inInnerCluster = prevEnclosure
	return ref
}

// --- Lateral array UNNEST (RFC-142) --------------------------------------

// findOuterScanTable resolves a lateral unnest's outer source alias to its
// scanned table name among the VISIBLE FROM-scope sources of the outer leg.
// It is the shared logical.FindOuterScanTable walk (the embedded cascades
// generator's AT-on-table pass resolves the same way through the same helper),
// so the translator and the early generator pass never diverge.
func findOuterScanTable(op logical.LogicalOperator, alias string) string {
	return logical.FindOuterScanTable(op, alias)
}

// outerSourceIsCTE reports whether `table` — the RESOLVED scan-table name the
// unnest's segment-0 source binds to in `j.Left` (findOuterScanTable: the CTE
// name for a CTE reference `FROM X`, the real table for `FROM T1 AS X`) — names a
// CTE or derived-table source currently in scope, i.e. its OUTPUT is a
// CTE-projected schema, not a base-table descriptor. Derived tables lower to a
// `LogicalCTE` registered under their alias (translateCTE), so both common-table
// expressions and `(SELECT …) AS d` derived tables appear in the CTE scope maps.
// A `LogicalUnnest` whose outer BOUND source is such a CTE must be validated
// against the CTE output type, not base-table metadata (P2a). It is keyed on
// the resolved scan TABLE — never the segment-0 alias — so a real table aliased
// with a CTE's name (`FROM T1 AS X` while a CTE `X` exists) does NOT match: the
// visible scan `T1` shadows the unused CTE (over-rejection). RFC-142.
func (t *cascadesTranslator) outerSourceIsCTE(table string) bool {
	key := strings.ToUpper(table)
	if _, ok := t.cteScope[key]; ok {
		return true
	}
	if _, ok := t.cteExprScope[key]; ok {
		return true
	}
	if _, ok := t.cteColumnsScope[key]; ok {
		return true
	}
	return false
}

// outerSourceIsDerivedTable reports whether `alias` (the unnest's segment-0
// outer source name) is bound, in the outer sub-plan `op`, to a DERIVED-TABLE /
// CTE leg. This is the STRUCTURAL twin of outerSourceIsCTE: it reads the logical
// tree directly rather than the cteScope maps, so it fires INDEPENDENT of
// cteScope population order.
//
// CRITICAL (silent-wrong): a derived table `(SELECT … ) AS D` lowers to a
// `LogicalCTE{Name:D, Main:Scan(D)}` inside `j.Left`, but that CTE's body is only
// registered into cteScope when `j.Left` is *translated* (translateCTE) — which
// happens AFTER the metadata-validation guard in translateUnnestJoin. So
// outerSourceIsCTE returns false at the guard, and findOuterScanTable's walk into
// `Main` resolves `D` to its alias-scan → the REAL table `D` of the same name (if
// one exists). The unnest then validates `ARR` against the real table's ARRAY
// metadata while the FlatMap reads the SCALAR `ARR` of the derived row → one
// wrong scalar row per outer row. Detecting the derived/CTE leg STRUCTURALLY —
// by the in-scope quantifier alias, exactly as Java's
// generateCorrelatedFieldAccess resolves the in-scope source, not the catalog
// table — rejects the derived-output unnest cleanly in ALL cases, even when a
// real same-named table exists.
//
// Delegates to the shared logical.OuterSourceIsDerivedTable walk so the
// translator's CTE/derived guard and the embedded generator's early AT-on-table
// pass detect a derived source identically.
func outerSourceIsDerivedTable(op logical.LogicalOperator, alias string) bool {
	return logical.OuterSourceIsDerivedTable(op, alias)
}

// outerBoundAliases collects the source aliases bound by the outer leg of a
// lateral unnest (the scan/source aliases visible in `op`), so the unnest's
// element/ordinal binding alias can be checked for a collision against them
// (P1). Like findOuterScanTable, it does NOT descend into CTE/derived
// BODIES — only the visible Main leg — so it sees exactly the aliases the
// unnest's merged outer row flows under. RFC-142.
func outerBoundAliases(op logical.LogicalOperator) map[string]struct{} {
	set := make(map[string]struct{})
	var walk func(logical.LogicalOperator)
	walk = func(o logical.LogicalOperator) {
		switch n := o.(type) {
		case *logical.LogicalScan:
			a := n.Alias
			if a == "" {
				a = n.Table
			}
			if a != "" {
				set[strings.ToUpper(a)] = struct{}{}
			}
		case *logical.LogicalUnnest:
			// A prior unnest leg binds its element/ordinal alias.
			if n.Alias != "" {
				set[strings.ToUpper(n.Alias)] = struct{}{}
			}
			if n.AtAlias != "" {
				set[strings.ToUpper(n.AtAlias)] = struct{}{}
			}
		case *logical.LogicalCTE:
			walk(n.Main)
		default:
			for _, c := range o.Children() {
				walk(c)
			}
		}
	}
	walk(op)
	return set
}

// unnestExistsSeedSafe reports whether a lateral unnest may take the ordinal
// seed given its EXISTS context. A SINGLE-NAMESPACE outer always qualifies. A
// MULTI-ALIAS outer qualifies ONLY when it is an OUTER-join box that gates fresh
// (boxGatesFresh) — the RFC-173 S4 Step-B lift: the box births its whole
// leg-concat positionally and the per-leg-window rebase (channels 1+2)
// disambiguates its dup-named legs by their [Start,Width) windows, so a
// qualified correlation to a buried leg (`FOB.K` in `a FULL OUTER b`) resolves
// to THAT leg, not the same-named column of the other leg. This is coupled to
// AXIS 1 (the box-outer enclosure at the unnest's translateRef) through the
// SAME boxGatesFresh predicate — see boxGatesFresh; either half alone is broken.
//
// A multi-alias outer that is NOT a fresh-gating outer box (an INNER cluster, a
// name-model-enclosed box) stays NAME-MODEL: an INNER cluster's seed still
// declines the flattened multi-source outer (R1), and an ordinal seed over it
// would bake the merged prefix blind to the alias count.
//
// This is a genuine SCOPE gate, not a prediction of the executor's predicate
// routing: the shadow / outer-only-conjunct / inner-alias-collision shapes that
// earlier declines guarded are now handled POSITIONALLY by the executor — the
// mixed seed carries per-leg windows (including a synthesized element window,
// values.OrdinalSeedLegWindows), the below-FOD hoist rebases every
// inner-residual outer ref to a baked ofOrdinal, and the rule routes by the
// renamed correlation identity, not by name. A plain (non-EXISTS) unnest is
// unaffected.
func (t *cascadesTranslator) unnestExistsSeedSafe(left logical.LogicalOperator, pureSpine bool) bool {
	// A MULTI-alias box declines to name-model when a regular (non-EXISTS) WHERE
	// conjunct references a box leg (unnestOuterConjunctOnBoxLeg): the ordinal seed
	// cannot yet bake such a conjunct positionally — it is merged into the
	// name-keyed unnest SELECT, out of the executor's below-FOD hoist reach, so a
	// positional box leaves the conjunct's box-leg ref unresolvable (malformed
	// plan). This holds in BOTH paths — the flag is set by translateUnnestExistsFilter
	// (EXISTS) AND the non-EXISTS filter-over-unnest merge — so the check is BEFORE
	// the `!unnestUnderExistential` early return below. A single-source outer
	// (==1) is unaffected — its pristine prefix resolves a bare conjunct.
	//
	// This arm is scoped to genuine BOX bases: a PURE chained-unnest spine
	// (pureSpine, passed true ONLY by the chained ordinal gate when the
	// admitted spine BOTTOMS at a source binding exactly one alias) is
	// multi-alias by construction — its aliases are chain links, not box legs —
	// and the chained rebase authority (rebaseChainedOuterLegPredicate) bakes
	// or lazies every reachable outer ref on the chained select, with the
	// (pred, !ok) fail-closed net behind it. Declining here would needlessly
	// kick every FILTERED 3+-link chain to name-model. A spine that bottoms in
	// a FULL box (also clusterArity==1, so equally ADMITTED) passes
	// pureSpine=false — its bottom aliases ARE box legs, and suppressing this
	// arm for them would ordinalize the chained link over the first link's
	// name-model seed (silently wrong rows). Every OTHER decline arm below
	// stays live for spines.
	if t.unnestOuterConjunctOnBoxLeg && !pureSpine && len(outerBoundAliases(left)) > 1 {
		return false
	}
	if !t.unnestUnderExistential {
		return true
	}
	if t.unnestExistsScopeCollision {
		return false
	}
	return len(outerBoundAliases(left)) == 1 || t.boxGatesFresh(left)
}

// nonExistsConjunctRefsOuterLeg reports whether any NON-EXISTS conjunct of pred
// references an outer-leg alias in boxAliases — the box-leg reference the
// ordinal unnest seed cannot yet bake in a regular WHERE conjunct (see
// unnestOuterConjunctOnBoxLeg). Element/ordinal (unnest AS/AT) refs are
// NOT in boxAliases, so a WHERE on the element does not trip it.
func nonExistsConjunctRefsOuterLeg(pred predicates.QueryPredicate, boxAliases map[string]struct{}) bool {
	if len(boxAliases) == 0 {
		return false
	}
	for _, p := range splitNonExistsPredicates(pred) {
		for c := range predicates.GetCorrelatedToOfPredicate(p) {
			if _, ok := boxAliases[strings.ToUpper(c.Name())]; ok {
				return true
			}
		}
	}
	return false
}

// boxOuterBirthsPositional reports whether the unnest's OUTER box will actually
// take the ordinal seed — the EXACT condition the box-outer POSITIONAL birth
// (AXIS 1) must share with the seed gate (AXIS 2). Among outer boxes ONLY a FULL
// box qualifies: clusterArity==1 holds for a merge-opaque FULL box but NEVER for
// LEFT/RIGHT (whose clusterArity is preserved-side + 1 >= 2), so a LEFT/RIGHT
// box's seed can never ordinalize (the :1496 clusterArity==1 gate blocks it).
// Birthing a LEFT/RIGHT box POSITIONAL while its seed stays NAME-MODEL would
// strand the name-model builder over a positional row — it reads the box by the
// ABSENT qualified LEG.COL keys → NULL / bare last-leg-wins → wrong rows (or a
// loud unresolvable-field on the ON predicate). boxGatesFresh restricts to
// fresh-gating outer boxes (false for scans and INNER clusters);
// unnestExistsSeedSafe folds in the EXISTS scope-collision guard so AXIS 1
// declines EXACTLY when the seed does. For a single-source scan outer this is
// false (boxGatesFresh false) and AXIS 1 is a no-op — a scan ref ignores the
// enclosure bit — so nothing changes off the box path.
func (t *cascadesTranslator) boxOuterBirthsPositional(left logical.LogicalOperator) bool {
	return t.clusterArity(left) == 1 && t.boxGatesFresh(left) && t.unnestExistsSeedSafe(left, false)
}

// existsInnerScopeCollidesOuter reports whether any EXISTS inner subquery's
// plan carries a source alias equal to an outer FROM leg's. VESTIGIAL for
// single-table catalog inners since the collision mint (buildCorrelatedExists
// births those under a unique correlation — outerBoundAliases(esq.Plan) can
// never intersect the outer legs). It still fires for the UNMINTED shapes —
// multi-source and CTE inners keeping their SQL names — whose merged rows
// route by source-alias name keys, so a colliding name would be served from
// the wrong scope; those stay name-model as the conservative floor until
// mint-per-leg lands (booked; see unnestExistsScopeCollision).
func existsInnerScopeCollidesOuter(esqs []logical.ExistsSubquery, outerLegs map[string]struct{}) bool {
	if len(outerLegs) == 0 {
		return false
	}
	for _, esq := range esqs {
		// CLEAN-PATH SKIP: an esq with NO join predicate whose plan the
		// rename can re-identify contributes no collision. There is no
		// cross-scope predicate to mis-serve, the plan's internal refs are
		// self-contained (built by the full planner without outer scopes),
		// and existsInnerCorrelation rebinds the FOD under esq.Alias — a
		// generated name no SQL leg shares — so the runtime interface is
		// collision-free even though the plan's SOURCE alias may equal an
		// outer leg's. A JoinPredicate-nil inner the rename DECLINES (a
		// join/CTE plan) still counts: its merged rows route by source-alias
		// name keys under the ∃ and stay conservatively name-model.
		if esq.JoinPredicate == nil && existsInnerSafeToRename(esq.Plan) {
			continue
		}
		for a := range outerBoundAliases(esq.Plan) {
			if _, ok := outerLegs[a]; ok {
				return true
			}
		}
	}
	return false
}

// unnestOuterLegAliases returns the outer leg aliases of a lateral unnest's outer
// sub-plan EXCEPT the one the merged row flows under (mergedCorr =
// sourceAlias(j.Left), the RIGHTMOST leg). It is the set rebaseUnnestOuterLegPredicate
// must rewrite for a multi-source unnest WHERE: a reference to a NON-flow leg
// (`A.c` in `FROM A, B, A.arr AS X` where the row flows under B) reads its column
// off the merged QOV via the qualified `A.c` key, while the flow-leg's own column
// (`B.d`) is already read bare off the merged QOV and must NOT be re-qualified —
// a single-source unnest (`FROM t, t.arr`) flows under segment-0's own alias, so
// the set is empty and the rebase is a no-op. RFC-142.
func unnestOuterLegAliases(op logical.LogicalOperator, mergedCorr values.CorrelationIdentifier) map[string]struct{} {
	all := outerBoundAliases(op)
	delete(all, strings.ToUpper(mergedCorr.Name()))
	return all
}

// unnestArrayElementType returns the element type for a lateral unnest's
// array field, whether the field resolves to an array, AND whether the field
// EXISTS on the outer source at all. It walks the outer source's proto
// descriptor along the unnest's field segments (`u.Segments[1:]`; segment 0 is
// the outer source alias) and asserts the final field is repeated
// (`IsList()`). For a scalar-element array the element type is the scalar;
// for a struct array (message element) or an unrecognized kind it is
// UnknownType (the runtime flows the raw element).
//
// The `fieldPresent` return distinguishes Java's two failure modes
// (`generateCorrelatedFieldAccess` / `resolveCorrelatedIdentifier`):
//
//   - field MISSING on the source (`fieldPresent == false`): the dotted name
//     is not a column of the source → the caller treats it as a genuine table
//     (table-not-found path), mirroring Java falling through from
//     `resolveCorrelatedIdentifier` to an undefined-table error.
//   - field PRESENT but NON-array (`fieldPresent == true, isArray == false`):
//     a real scalar column referenced as an unnest source → Java's
//     `INVALID_COLUMN_REFERENCE`/`WRONG_OBJECT_TYPE` ("repeated type" assert).
//
// RFC-142.
func (t *cascadesTranslator) unnestArrayElementType(outerTable string, fieldSegments []string) (elementType values.Type, fieldName string, isArray, fieldPresent bool) {
	rt := t.resolveRecordType(outerTable)
	if rt == nil || rt.Descriptor == nil {
		return values.UnknownType, "", false, false
	}
	return arrayFieldFromDescriptor(rt.Descriptor.Fields(), fieldSegments)
}

// protoFieldLookup resolves a field by SQL identifier (case-insensitive —
// proto names are lower/snake, SQL upper).
func protoFieldLookup(fs protoreflect.FieldDescriptors, name string) protoreflect.FieldDescriptor {
	if fd := fs.ByName(protoreflect.Name(strings.ToLower(name))); fd != nil {
		return fd
	}
	for i := 0; i < fs.Len(); i++ {
		if f := fs.Get(i); strings.EqualFold(string(f.Name()), name) {
			return f
		}
	}
	return nil
}

// arrayFieldFromDescriptor classifies a lateral unnest's array field by
// per-segment descent over a proto record's fields (Java's
// SemanticAnalyzer.lookupNestedField STRUCT rule): every INTERMEDIATE segment
// must be a singular MESSAGE field; the FINAL segment must be repeated. Shared
// by the base-table path (unnestArrayElementType) and the chained path (which
// descends the OWNER unnest's element message). Returns:
//   - (elemType, name, true, true) for a repeated final — a valid array;
//   - (Unknown, "", false, true) for a present-but-scalar final, or a
//     non-record/repeated intermediate — Java's "repeated type" assert;
//   - (Unknown, "", false, false) for an absent field.
func arrayFieldFromDescriptor(fields protoreflect.FieldDescriptors, fieldSegments []string) (elementType values.Type, fieldName string, isArray, fieldPresent bool) {
	if len(fieldSegments) == 0 {
		return values.UnknownType, "", false, false
	}
	for _, seg := range fieldSegments[:len(fieldSegments)-1] {
		fd := protoFieldLookup(fields, seg)
		if fd == nil {
			return values.UnknownType, "", false, false
		}
		if fd.IsList() || fd.Kind() != protoreflect.MessageKind {
			return values.UnknownType, "", false, true
		}
		fields = fd.Message().Fields()
	}
	fd := protoFieldLookup(fields, fieldSegments[len(fieldSegments)-1])
	if fd == nil {
		return values.UnknownType, "", false, false
	}
	if !fd.IsList() {
		return values.UnknownType, "", false, true
	}
	return arrayFieldElementType(fd), strings.ToUpper(string(fd.Name())), true, true
}

// containsLateralUnnest reports whether a logical sub-plan contains a
// LogicalUnnest in the SAME (current) FROM scope — i.e. this is a CHAINED /
// multi-unnest FROM list (`FROM t, t.arr1 AS v1, t.arr2 AS v2`). RFC-142.
//
// CRITICAL (P2a): the walk MUST NOT descend into a CTE / derived-table
// BODY. A derived table `(SELECT v FROM T1, T1.arr AS v) AS d` is its OWN FROM
// scope; its inner unnest belongs to that scope, not the outer one. The outer
// FROM scope only sees the derived table's OUTPUT alias `d` (its Main leg). If
// the walk descended into `LogicalCTE.Body` it would count the derived table's
// own unnest and wrongly reject the outer query as "multiple lateral array
// unnests in one FROM clause" — a valid query falsely rejected. So at a
// LogicalCTE we inspect ONLY its Main (the visible alias projection), never its
// Body — mirroring findOuterScanTable / outerBoundAliases, which resolve a
// derived/CTE source against its Main only.
func containsLateralUnnest(op logical.LogicalOperator) bool {
	if op == nil {
		return false
	}
	if _, ok := op.(*logical.LogicalUnnest); ok {
		return true
	}
	if cte, ok := op.(*logical.LogicalCTE); ok {
		// A derived/CTE source is its own FROM scope; only its Main (visible
		// alias projection) is in the current scope, never its Body.
		return containsLateralUnnest(cte.Main)
	}
	for _, c := range op.Children() {
		if containsLateralUnnest(c) {
			return true
		}
	}
	return false
}

// arrayFieldElementType returns the element type of a repeated proto field.
// fieldTypeForFD collapses list fields to UnknownType, so the element kind is
// read directly from the field descriptor's scalar Kind (a struct/message
// element stays UnknownType — the runtime flows the raw struct map). RFC-142.
func arrayFieldElementType(fd protoreflect.FieldDescriptor) values.Type {
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return values.NewPrimitiveType(values.TypeCodeBoolean, true)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return values.NewPrimitiveType(values.TypeCodeInt, true)
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return values.NewPrimitiveType(values.TypeCodeLong, true)
	case protoreflect.FloatKind:
		return values.NewPrimitiveType(values.TypeCodeFloat, true)
	case protoreflect.DoubleKind:
		return values.NewPrimitiveType(values.TypeCodeDouble, true)
	case protoreflect.StringKind:
		return values.NewPrimitiveType(values.TypeCodeString, true)
	case protoreflect.BytesKind:
		return values.NewPrimitiveType(values.TypeCodeBytes, true)
	case protoreflect.MessageKind:
		if msg := fd.Message(); msg != nil && string(msg.FullName()) == functions.UUIDProtoMessageName {
			return values.NewPrimitiveType(values.TypeCodeUuid, true)
		}
		return values.UnknownType
	default:
		return values.UnknownType
	}
}

// translateUnnestJoin lowers a lateral array unnest source (`FROM t, t.arr AS
// x [AT ord]`) — a LogicalJoin whose Right is a LogicalUnnest — into a
// correlated FlatMap-over-Explode SelectExpression, mirroring Java's
// `LogicalOperator.generateCorrelatedFieldAccess`:
//
//   - outer leg = the source the array field belongs to (j.Left);
//   - inner = Explode of the correlated array Value (FieldValue{arr} over
//     QOV(outerAlias)), wrapped in a forEach quantifier under the AS alias;
//     WITH ORDINALITY when an AT alias is present;
//   - result value projects the outer columns + the element bound to the AS
//     alias (and, with ordinality, the 1-based ordinal bound to the AT alias).
//
// The ImplementNestedLoopJoinRule's correlated-FlatMap path implements the
// SelectExpression as RecordQueryFlatMapPlan(outer, explode, …, resultValue,
// false) — the review-confirmed non-existential, no-FirstOrDefault path.
//
// Returns nil (untranslatable) for a non-scan outer or an unresolvable field;
// when the source carries an AT alias but is NOT a correlated array, it
// records ErrCodeWrongObjectType (Java's WRONG_OBJECT_TYPE) and returns nil so
// the planner surfaces the faithful diagnostic. RFC-142.
func (t *cascadesTranslator) translateUnnestJoin(j *logical.LogicalJoin, u *logical.LogicalUnnest) expressions.RelationalExpression {
	// RFC-173 Slice 2: the entire unnest lowering (FlatMap-over-Explode,
	// dotted-prefix bipartition machinery, multi-source fallback rebuilds via
	// unnestFallbackOrReject) is name-model until Slice 3 (review W4-deferral ruling) — every join
	// translated beneath it, including the fallback's rebuilt LogicalJoins,
	// is marked enclosed so it cannot gate ordinal.
	//
	// prevEnclosure captures the ENCLOSED bit on entry (t.inInnerCluster BEFORE
	// this unnest sets it): true iff THIS unnest is itself a leg of a larger
	// name-model cluster. The W4c ordinal-seed gate below reads it as "enclosed"
	// (an enclosed unnest declines to name-model — its ordinal seed would be
	// flattened into a name-model parent that panics on a baked leg).
	prevEnclosure := t.inInnerCluster
	t.inInnerCluster = true
	defer func() { t.inInnerCluster = prevEnclosure }()
	// A lateral unnest is classified by walking the outer source's PROTO
	// descriptor for the array field (unnestArrayElementType → resolveRecordType
	// → t.md). The metadata-less translation path (TranslateToCascades /
	// TranslateToCascadesWithSubqueries(op, nil) — used by scalar-subquery / DML
	// translation and unit tests) has no descriptor to classify against. Java
	// never reaches an unnest without a SemanticAnalyzer/metadata in scope, so
	// rather than dereference nil metadata (a panic) we decline cleanly: an
	// unnest genuinely needs metadata to classify. No production caller unnests
	// without metadata (every SQL plan path passes real md). RFC-142.
	if t.md == nil {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"lateral array unnest requires record metadata to classify the array field"))
		return nil
	}
	// The multiple-unnest guard (CHAINED `FROM t, t.arr1 AS v1, t.arr2 AS v2`)
	// is applied LATER — only AFTER the right side is confirmed a VALID array
	// unnest (past the !isArray validation below). Running it here, before the
	// array-source validation, would mask an invalid right-side candidate after a
	// prior unnest: `FROM T1, T1.arr AS V, U AT O` (AT on a non-array table) or
	// `FROM T1, T1.arr AS V, T1.id AS X` (a scalar field) would wrongly report
	// "multiple unnests" (UNSUPPORTED_QUERY) instead of the faithful
	// WRONG_OBJECT_TYPE the array validation produces. So: an AT-on-non-array or
	// scalar candidate after an unnest → the array-validation error fires first; a
	// genuine SECOND array unnest → the multiple-unnest guard. RFC-142.

	// The FlatMap binds the outer row under sourceAlias(j.Left), which is the
	// rightmost FROM leg. For a single outer source this IS segment 0
	// (`FROM t, t.arr`); when the unnest follows MORE THAN ONE prior source
	// (`FROM A, B, A.arr AS X`) the outer is the merged `A × B` row flowed under
	// B's alias, and segment 0 (A) is not the flow leg — the array field is read
	// QUALIFIED to A below.
	outerAlias := sourceAlias(j.Left)
	// Resolve segment 0 to the SCAN it actually binds to in `j.Left` FIRST — the
	// CTE/derived rejection below is tied to that BOUND source, not to the segment-0
	// alias name (over-rejection). `findOuterScanTable` returns the
	// scan's TABLE name for the alias: a real table `T1` for `T1 AS X`, the CTE name
	// `X` for a CTE reference `FROM X` (the scan's Table holds the CTE name), or `d`
	// for a derived table `(…) AS d` (its Main alias-scan). When segment 0 does not
	// resolve to a visible scan it is not a correlated source at all (schema-
	// qualified table, or a name hidden behind a derived-table boundary) — the table
	// path handles it; an AT alias is then invalid.
	outerTable := findOuterScanTable(j.Left, u.Segments[0])
	if outerTable == "" {
		// RFC-173 unnest-residual class 4 (chained unnest): segment 0 names a
		// PRIOR lateral unnest's element, not a scan. Positively gated on
		// findOwnerUnnest (design ruling 4 condition 2 — never merely
		// outerTable==""), and only for INNER-comma chains
		// (!unnestUnderExistential — an under-existential chain keeps item-2's
		// binders). A resolvable chain routes to translateChainedUnnestJoin;
		// anything else (schema-qualified, derived-hidden) falls through to the
		// existing fallback.
		//
		// NOT gated on prevEnclosure: a chained unnest is always name-model
		// residual, and a 3+-link chain translates its OUTER (which contains the
		// PRIOR chained link) with the enclosure bit set — so an inner chained
		// link would observe prevEnclosure=true. Gating on !prevEnclosure would
		// collapse the chain there (the inner link declines and the outer's
		// translateRef returns nil). The chain shape is decided by
		// isChainedUnnest alone, exactly as Java nests each link the same way.
		if !t.unnestUnderExistential && isChainedUnnest(j.Left, u) {
			if sel := t.translateChainedUnnestJoin(j, u, prevEnclosure); sel != nil {
				return sel
			}
			// A chained unnest that classified to a loud error set the
			// translate error; return nil so the caller surfaces it.
			if t.translateErr != nil {
				return nil
			}
		}
		// segment 0 names a prior unnest's AT ORDINAL alias (`t.arr AS x AT o,
		// o.sub AS y`): `o` binds a scalar integer, so `o.sub` is a field access
		// on a scalar — Java rejects it at resolution (UNDEFINED_COLUMN). Surface
		// that honest error instead of the generic fallback reject (which would
		// rebuild `o.sub` as a phantom scan and fail with 0AF00). Only the
		// field-access shape (a sub-path past the ordinal name) reaches here — a
		// bare `o AS y` is a different, earlier-handled shape.
		if len(u.Segments) >= 2 && logical.IsUnnestOrdinalAlias(j.Left, u.Segments[0]) {
			t.setTranslateErr(api.NewErrorf(api.ErrCodeUndefinedColumn,
				"column %q does not exist on source %q",
				strings.Join(u.Segments[1:], "."), u.Segments[0]))
			return nil
		}
		return t.unnestFallbackOrReject(j, u)
	}
	// Java's `generateCorrelatedFieldAccess` validates the array field against the
	// in-scope source's OUTPUT type (its quantifier's flowed columns), NOT a base-
	// table descriptor. When the BOUND source is a CTE / derived-table, that output
	// is the CTE's PROJECTED columns — a renamed/computed schema that may differ from
	// any base table (`WITH T1 AS (SELECT ID AS ARR FROM T1) … FROM T1, T1.ARR`: the
	// CTE output `ARR` is the SCALAR renamed `ID`, even though a real base table `T1`
	// has an ARRAY column `ARR`). Validating `ARR` against the base-table descriptor
	// here would explode the WRONG column (silent-wrong, P2a). The leg-column
	// TYPES the translator derives for a CTE/derived output are best-effort
	// `UnknownType` (legColumns), so the element type is not recoverable at this
	// point; rather than validate against the wrong base-table metadata, reject a
	// CTE/derived-source unnest cleanly. Single-array unnest over a REAL table (the
	// R5 core) is unaffected. RFC-142.
	//
	// The rejection is tied to the ACTUAL source bound in `j.Left` for segment 0,
	// NOT to a CTE that merely SHARES segment 0's name in the global WITH scope
	// (over-rejection): a real table aliased with a CTE's name
	// (`WITH X AS (…) SELECT V FROM T1 AS X, X.ARR AS V`) SHADOWS the unused CTE — the
	// VISIBLE scan `T1 AS X` is the source, so the unnest is valid and MUST plan.
	// Both arms therefore key on the resolved bound source:
	//   - outerSourceIsCTE(outerTable): the scan's resolved table name IS a CTE in
	//     scope. For a CTE used as the source, findOuterScanTable returns the CTE
	//     name (the scan's Table), so this fires; for `T1 AS X` it returns the real
	//     table `T1`, so it does NOT fire even when a CTE `X` exists globally.
	//   - outerSourceIsDerivedTable(j.Left, segment 0): a LogicalCTE leg in j.Left
	//     whose Name == segment 0 — the STRUCTURAL twin, load-bearing for the
	//     DERIVED-PRIMARY shape `FROM (SELECT ID AS ARR FROM T1) AS D, D.ARR AS V`.
	//     A derived table's LogicalCTE body is registered into cteScope only when
	//     j.Left is *translated* (translateCTE), which is AFTER this guard — so
	//     outerSourceIsCTE(outerTable) is still false there; the structural arm reads
	//     the logical tree directly so it fires regardless of cteScope timing and
	//     regardless of whether the alias also names a real table. The in-scope
	//     derived source is preferred over the catalog table, exactly as Java
	//     resolves the in-scope quantifier alias. RFC-142.
	var elementType values.Type
	var fieldName string
	if t.outerSourceIsCTE(outerTable) || outerSourceIsDerivedTable(j.Left, u.Segments[0]) {
		// RFC-173 unnest-residual class 3: resolve the array field through the
		// CTE/derived body's projection to a base-table array column (the
		// flowed type is UnknownType — see rfc173_w5_derived_unnest.go). A
		// bare passthrough classifies; every other body shape declines loudly.
		et, outName, disp := t.classifyDerivedUnnestArray(j.Left, u)
		switch disp {
		case derivedUnnestArray:
			elementType, fieldName = et, outName
			// Fall through to the shared build path below (skip the base-table
			// unnestArrayElementType classification — already done via the body).
		case derivedUnnestWrongType:
			t.setTranslateErr(api.NewError(api.ErrCodeInvalidColumnReference,
				"join correlation can occur only on a column of repeated (array) type"))
			return nil
		case derivedUnnestUndefined:
			t.setTranslateErr(api.NewErrorf(api.ErrCodeUndefinedColumn,
				"column %q does not exist on source %q",
				strings.Join(u.Segments[1:], "."), u.Segments[0]))
			return nil
		default: // derivedUnnestUnsupported
			t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
				"unnest over a computed/non-passthrough CTE/derived-table output is not yet supported"))
			return nil
		}
	} else {
		var isArray, fieldPresent bool
		elementType, fieldName, isArray, fieldPresent = t.unnestArrayElementType(outerTable, u.Segments[1:])
		if !isArray {
			// Segment 0 matched a scan whose table is `outerTable`. Three sub-cases,
			// matching Java's `generateAccess`/`resolveCorrelatedIdentifier`:
			//
			//   - PRESENT-but-scalar (fieldPresent): a real non-array correlated
			//     source → Java's `generateCorrelatedFieldAccess` "repeated type"
			//     assert → WRONG_OBJECT_TYPE (P2c).
			//   - source is a REAL table but the field is MISSING: an unresolvable
			//     correlated field on a known source — Java's `resolveCorrelatedIdentifier`
			//     fails the field lookup → a clean UNDEFINED_COLUMN, NOT a silent table
			//     fallback that produces a generic translation failure (P2c).
			//   - source is NOT a real table (a derived-table alias `d` whose record
			//     type doesn't resolve): the field can't be checked here → table path.
			if fieldPresent {
				t.setTranslateErr(api.NewError(api.ErrCodeInvalidColumnReference,
					"join correlation can occur only on a column of repeated (array) type"))
				return nil
			}
			// AT on a BARE source (`FROM T1, T1 AT ord`): segment 0 names a visible
			// scan, but there are NO field segments to resolve — the source is the
			// TABLE/alias itself, not an array field on it. AT is valid only on a
			// correlated array, so this converges with the other AT-on-a-table
			// rejection paths (unnestFallbackOrReject, demoteSchemaQualifiedUnnest) on
			// Java's WRONG_OBJECT_TYPE — NOT an UNDEFINED_COLUMN for an empty field
			// name. (Without the AT this single-segment shape isn't even classified as
			// an unnest; the AT forces it here so it can be rejected faithfully.)
			// RFC-142.
			if u.AtAlias != "" && len(u.Segments) < 2 {
				t.setTranslateErr(api.NewError(api.ErrCodeWrongObjectType,
					"AT ordinality is only valid on a correlated array source (FROM t, t.arr AS x AT ord)"))
				return nil
			}
			if t.resolveRecordType(outerTable) != nil {
				// Known source, missing field: unresolvable correlated field.
				t.setTranslateErr(api.NewErrorf(api.ErrCodeUndefinedColumn,
					"column %q does not exist on source %q",
					strings.Join(u.Segments[1:], "."), u.Segments[0]))
				return nil
			}
			return t.unnestFallbackOrReject(j, u)
		}
	}

	// CHAINED unnest (`FROM t, t.arr1 AS v1, t.arr2 AS v2`) lowers to a nested
	// FlatMap whose inner Explode correlates to the OUTERMOST scan while its
	// outer is the first unnest's FlatMap — Java's `q3._0._1` deep-tuple shape.
	// Go's name-keyed merged-row model does not yet thread the first unnest's
	// element/ordinal columns through the second FlatMap's merged outer row, so
	// rather than emit silently-wrong rows we reject the multi-unnest shape
	// cleanly. Single-array unnest (the R5 core) is fully supported.
	//
	// This guard runs ONLY now that the right side is confirmed a VALID array
	// unnest (past the !isArray validation above): a genuine SECOND array unnest
	// → UNSUPPORTED_QUERY here; an AT-on-non-array or scalar candidate after a
	// prior unnest already returned the faithful WRONG_OBJECT_TYPE above, so it
	// never reaches this guard. RFC-142.
	if containsLateralUnnest(j.Left) {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"multiple lateral array unnests in one FROM clause are not yet supported"))
		return nil
	}

	// The inner quantifier's correlation MUST be the VISIBLE unnest alias — the
	// AS alias, or, in the AT-only form (`FROM t, t.arr AT a`), the AT alias.
	// This is exactly the correlation `unnestScopeSourceAdder` registers the
	// unnest's virtual scope source under, and the one `rewriteUnnestPredicate`
	// rebases a WHERE-on-ordinal predicate to (`QOV(<alias>)._1`). Binding the
	// inner under a PRIVATE synthesized name (as an earlier cut did for AT-only)
	// left the predicate correlated to `QOV(AT)` while the Explode flowed under
	// `QOV(__unnest_…)`, so the NLJ rule never pushed the predicate into the inner
	// Explode filter — `WHERE a = 1` planned as an unbound outer filter and
	// dropped the wrong rows. Mirrors Java's single `resultingQuantifier` driving
	// both the AS and AT bindings (`generateCorrelatedFieldAccess`). RFC-142.
	innerCorr := unnestSourceCorrelation(u)
	innerAlias := innerCorr.Name()

	// P1 (silent-wrong): the unnest's element/ordinal binding alias MUST
	// NOT collide with the outer FlatMap correlation or any already-bound outer
	// source alias. If it did (`FROM T1 AS X, X.arr AS X`, or the aliasless
	// `FROM T1 AS ARR, ARR.arr` where the defaulted field-name alias ARR equals
	// the outer alias), innerCorr would equal outerCorr and the flatMapCursor
	// would bind BOTH the outer row and the inner element under one name — the
	// inner element overwrites the outer row, silently corrupting projections
	// and predicates. Reject cleanly instead. Java never reaches this because a
	// duplicate quantifier alias is a binding error upstream. RFC-142.
	collide := func(name string) bool {
		if name == "" {
			return false
		}
		if strings.EqualFold(name, outerAlias) {
			return true
		}
		_, ok := outerBoundAliases(j.Left)[strings.ToUpper(name)]
		return ok
	}
	if collide(u.Alias) || collide(u.AtAlias) {
		t.setTranslateErr(api.NewError(api.ErrCodeDuplicateAlias,
			"lateral unnest alias collides with an outer FROM-source alias; use a distinct AS/AT alias"))
		return nil
	}
	// P2b (silent-wrong, overwrite): the AS element alias and the AT
	// ordinal alias MUST be distinct. `FROM t, t.arr AS X AT X` appends the element
	// and the ordinal under the SAME bare+qualified names in buildUnnestResultValue;
	// RecordConstructorValue.Evaluate stores fields in a map, so the ordinal
	// (appended last) silently OVERWRITES the element — `SELECT X` returns the
	// ordinal, not the unnested value. Reject cleanly BEFORE constructing the result,
	// consistent with the unnest-alias-vs-outer-alias rejection above. Java's
	// visitAtomTableItem binds AS and AT to two distinct quantifier columns; a
	// duplicate alias is a binding error upstream. RFC-142.
	if u.Alias != "" && u.AtAlias != "" && strings.EqualFold(u.Alias, u.AtAlias) {
		t.setTranslateErr(api.NewError(api.ErrCodeDuplicateAlias,
			"lateral unnest AS and AT aliases must be distinct; use different names for the element and the ordinal"))
		return nil
	}

	// RFC-173 W5: a MULTI-SOURCE outer over a gated inner cluster gathers FLAT
	// — the Explode becomes an ordinary quantifier of one (N+1)-way select whose
	// collection is a genuine baked correlation to the OWNING source's own
	// quantifier (Java's shape; the flat-at-translation design ruling). Runs
	// AFTER the full validation gauntlet above (the rejections are shared
	// verbatim) and BEFORE the binary path translates j.Left as one enclosed
	// ref (the gathered path translates legs FRESH; translating both ways would
	// double-append side state, e.g. collected scalar-subquery plans). Enclosed
	// and under-existential unnests keep today's paths (the enclosed class is
	// the next W5 commit; the existential class is re-chartered to the
	// W4-left+EXISTS slice). A nil is a DECLINE — the binary fallback below.
	if !prevEnclosure && !t.unnestUnderExistential {
		if sel := t.translateGatheredUnnestCluster(j, u, innerCorr, elementType, fieldName, unnestTrailing); sel != nil {
			return sel
		}
	}

	// RFC-173 S4 Step-B, AXIS 1 (coupled to the seed gate / AXIS 2 via
	// boxOuterBirthsPositional): a FULL outer box that will take the ordinal seed
	// BIRTHS a positional row instead of the name-model Datum this unnest's
	// :1074 `inInnerCluster=true` would otherwise force (the :167 enclosure
	// poison). The predicate is boxOuterBirthsPositional, NOT boxGatesFresh
	// alone: a LEFT/RIGHT box gates fresh but has clusterArity>=2 so its seed
	// stays name-model — birthing it positional would strand the name-model
	// builder over a positional row (wrong rows). We clear the enclosure ONLY
	// for the box's own translateRef, then restore it — the rest of the unnest
	// lowering keeps the enclosed bit. `prevEnclosure ||` keeps an
	// already-enclosed unnest name-model (matching the seed's own !prevEnclosure
	// gate below): the box only births positional when this unnest is un-enclosed.
	savedEnclosure := t.inInnerCluster
	t.inInnerCluster = prevEnclosure || !t.boxOuterBirthsPositional(j.Left)
	outerRef := t.translateRef(j.Left)
	t.inInnerCluster = savedEnclosure
	if outerRef == nil {
		return nil
	}
	outerCorr := values.NamedCorrelationIdentifier(outerAlias)

	// The correlated array Value: FieldValue{arrField} over QOV(outer).
	//
	// When the outer is a MERGED row (a multi-source FROM, e.g. `FROM A, B,
	// A.arr AS X`), the merged row flows under the rightmost leg's alias
	// (`sourceAlias(j.Left)`) and exposes every source's columns BOTH bare
	// (last-leg-wins) AND qualified `LEG.COL`. The bare key's winner follows
	// the join's EXECUTION operand order — an order the planner may legally
	// swap (cost model, tie-breaks) — so a bare read here is order-dependent:
	// it explodes whichever leg's array happened to merge LAST, not the array
	// the classifier type-checked. That includes the RIGHTMOST source
	// (`FROM A, B, B.arr AS X`): the old seg0==flow-alias bare-read arm was
	// correct only while the step-1 join kept declaration order, and returned
	// A's elements the moment the cost model preferred the swapped operands
	// (caught by the stable-tie-break landing). Read the QUALIFIED
	// `SEG0.FIELD` key whenever the outer row carries MORE THAN ONE source
	// namespace — the anchored merged record carries the qualified key for
	// every leg under either operand order.
	//
	// The authority is outerBoundAliases (the outer row's VISIBLE namespace
	// count), NOT clusterArity: a FULL OUTER box is merge-OPAQUE (arity 1 —
	// correct for SelectMergeRule purposes) yet its output row is MERGED
	// (`FROM a FULL JOIN b, a.arr AS x` — bare keys last-leg-wins across
	// both legs), so the arity proxy left exactly that shape on the bare
	// read. Only a genuine SINGLE-NAMESPACE outer (`FROM t, t.arr` — a
	// scan/derived row, bare keys only) reads the bare field. RFC-142.
	// For a MULTI-SEGMENT path (`t.a.b`, unnest-residual class 2) the root
	// read is the FIRST field segment; the remaining segments descend the
	// struct value at eval (the unpinned multi-accessor path — root by name
	// key, suffix through FieldValue's proto-message arm, Java's
	// FieldValue.ofFields shape).
	rootField := fieldName
	if len(u.Segments) > 2 {
		rootField = strings.ToUpper(u.Segments[1])
	}
	arrayFieldKey := rootField
	seg0 := strings.ToUpper(u.Segments[0])
	if len(outerBoundAliases(j.Left)) != 1 {
		arrayFieldKey = seg0 + "." + rootField
	}
	var arrayValue values.Value
	if len(u.Segments) > 2 {
		// The root reads by Datum key; the suffix descends the struct value
		// by field NAME (FieldValue's proto-message arm). Both ordinals are
		// the LOUD sentinel -1 — never consulted on the name-model / proto
		// paths, and out-of-range (a clean error) rather than a silent slot
		// read if either ever reached the positional descent arm.
		accs := []values.ResolvedAccessor{{Field: arrayFieldKey, Ordinal: -1}}
		for _, seg := range u.Segments[2:] {
			accs = append(accs, values.ResolvedAccessor{Field: strings.ToUpper(seg), Ordinal: -1})
		}
		arrayValue = &values.FieldValue{
			Field: fieldName,
			Typ:   values.NewArrayType(true, elementType),
			Child: values.NewQuantifiedObjectValue(outerCorr),
			// UNPINNED: the residual's rows are name-model — the root key
			// resolves against the Datum; only the suffix descends
			// positionally-agnostic through the struct value.
			Resolved: &values.FieldPath{Accessors: accs},
		}
	} else {
		arrayValue = values.NewFieldValue(
			values.NewQuantifiedObjectValue(outerCorr),
			arrayFieldKey,
			values.NewArrayType(true, elementType),
		)
	}
	withOrdinality := u.AtAlias != ""
	explode := expressions.NewExplodeExpressionWithOrdinality(arrayValue, withOrdinality)
	explodeRef := expressions.InitialOf(explode)

	innerQ := expressions.NamedForEachQuantifier(innerCorr, explodeRef)
	outerQ := expressions.NamedForEachQuantifier(outerCorr, outerRef)

	// RFC-173 W4c: ordinalize the seed when the OUTER is a SINGLE SOURCE
	// (clusterArity==1) AND the unnest is NOT ENCLOSED in a larger name-model
	// composition. The name model is deleted in S4, so this ordinal seed is what
	// keeps the ISOLATED single-source lateral unnest alive. Three decline gates,
	// all the W2/W5 "enclosure = poison" boundary — an ENCLOSED unnest's ordinal
	// seed (baked ofOrdinal refs, non-anchored RC) is flattened by SelectMergeRule
	// into a name-model parent whose machinery cannot consume it:
	//   - clusterArity(j.Left) > 1: a multi-source OUTER (`FROM A, B, A.arr AS x`)
	//     — ordinalizing a flattened multi-table outer cluster erases the buried
	//     source names (W5).
	//   - prevEnclosure: the unnest is a LEG of a larger multi-source join cluster
	//     (`FROM A, A.arr AS x, B`). It flattens into the (unnest × B) select; a
	//     GROUP BY / aggregation over it re-enumerates via PartitionSelectRule,
	//     whose anchored re-enumeration (NewReEnumerationAnchoredRecord) cannot
	//     resolve a non-anchored ordinal-seed leg (a loud panic). Stays name-model
	//     until W5 ordinalizes the enclosing multi-source cluster. (A projection
	//     over such a leg WOULD work via the coexistence Datum's qualified keys,
	//     but the aggregation path forces the whole class name-model — the two are
	//     indistinguishable here, at lowering.)
	//   - unnestUnderExistential (RFC-173 commit 5a — LIFTED, but only for a
	//     SINGLE-ALIAS outer, see unnestExistsSeedSafe): the unnest is the OUTER of
	//     an EXISTS semi-join. This used to force name-model because the existential
	//     rebase read outer-leg refs by name and panicked on baked ofOrdinal refs;
	//     commit 5a leaves the EXISTS correlation's outer-leg refs LEG-RELATIVE (the
	//     mixed seed now carries executor windows) and the executor's below-FOD hoist
	//     rebases them POSITIONALLY — so a single-source unnest under EXISTS gates
	//     ordinal like any other. A MULTI-ALIAS outer (a merge-opaque FULL OUTER box),
	//     or an EXISTS inner scanning a table aliased the same as an outer leg, stays
	//     name-model (see unnestExistsSeedSafe / existsInnerScopeCollidesOuter).
	// A decline (nil) falls back to the name-model builder.
	var resultValue values.Value
	// SINGLE-SOURCE (clusterArity==1), non-enclosed, exists-safe unnest
	// ordinalizes the seed. A MULTI-SEGMENT path (`FROM t, t.rec.arr AS x`,
	// len(Segments)>2, unnest-residual class 2) also needs its COLLECTION baked
	// as a fused ofOrdinal root (unnestBakedRootCollection below) — the shared
	// name-keyed arrayValue does NOT descend under the ordinal-seed birth. When
	// the bake declines (nil), the whole ordinal path declines and the name-model
	// builder (which owns the name-keyed collection) takes over.
	if t.clusterArity(j.Left) == 1 && !prevEnclosure && t.unnestExistsSeedSafe(j.Left, false) && len(u.Segments) >= 2 {
		resultValue = t.unnestOrdinalSeed(j.Left, outerCorr, innerCorr, u, elementType)
		if resultValue != nil && len(u.Segments) > 2 {
			if baked := t.unnestBakedRootCollection(j.Left, outerCorr, u, fieldName, elementType, 1, -1); baked != nil {
				bakedExplode := expressions.NewExplodeExpressionWithOrdinality(baked, withOrdinality)
				innerQ = expressions.NamedForEachQuantifier(innerCorr, expressions.InitialOf(bakedExplode))
			} else {
				resultValue = nil // decline the ordinal path → name-model residual
			}
		}
	}
	if resultValue == nil {
		resultValue = t.buildUnnestResultValue(j.Left, outerCorr, outerAlias, innerCorr, u, elementType, prevEnclosure)
	}
	if resultValue == nil {
		return nil
	}

	return expressions.NewSelectExpressionWithJoinType(
		resultValue,
		[]expressions.Quantifier{outerQ, innerQ},
		nil,
		[]string{outerAlias, innerAlias},
		expressions.JoinInner,
	)
}

// unnestFallbackOrReject handles a candidate comma source whose segment 0 did
// NOT resolve to a real in-scope TABLE source (it is a schema-qualified table,
// a name hidden behind a derived-table boundary, or a derived-table alias whose
// record type can't be inspected for an array field). The dotted name is then
// treated as a genuine table cross-join: re-translate the join with the right
// child as a plain scan of the joined name (the table-not-found path surfaces
// later if it is unknown). An AT alias here is still invalid — AT requires a
// correlated array (Java's WRONG_OBJECT_TYPE).
//
// The PRESENT-but-scalar and missing-field-on-a-known-source cases are handled
// inline by translateUnnestJoin (WRONG_OBJECT_TYPE / UNDEFINED_COLUMN) before
// this is ever reached. RFC-142.
func (t *cascadesTranslator) unnestFallbackOrReject(j *logical.LogicalJoin, u *logical.LogicalUnnest) expressions.RelationalExpression {
	if u.AtAlias != "" {
		t.setTranslateErr(api.NewError(api.ErrCodeWrongObjectType,
			"AT ordinality is only valid on a correlated array source (FROM t, t.arr AS x AT ord)"))
		return nil
	}
	tableName := strings.Join(u.Segments, ".")
	alias := u.Alias
	if alias == "" {
		alias = tableName
	}
	rebuilt := &logical.LogicalJoin{
		Left:        j.Left,
		Right:       logical.NewScan(tableName, alias),
		Kind:        j.Kind,
		OnText:      j.OnText,
		OnPredicate: j.OnPredicate,
	}
	return t.translateJoin(rebuilt)
}

// buildUnnestResultValue builds the FlatMap RETURN value for a lateral unnest:
// the outer leg's columns (anchored to QOV(outer)) plus the unnested element
// (and, with ordinality, the 1-based ordinal). The element is the WHOLE inner
// quantifier value (QOV(inner)) for the bare variant, or FieldValue.ofOrdinal
// (element=0, ordinal=1) for the WITH ORDINALITY variant. Mirrors Java's
// attribute list in generateCorrelatedFieldAccess. RFC-142.
// The enclosed arg is the CALLER'S ENTRY enclosure (prevEnclosure), for the
// producer census — NOT t.inInnerCluster, which both callers unconditionally set
// to true on entry (so reading the field here would report every P5 firing as
// enclosed by construction, blinding the census to an un-enclosed P5 residual).
func (t *cascadesTranslator) buildUnnestResultValue(
	outer logical.LogicalOperator,
	outerCorr values.CorrelationIdentifier,
	outerAlias string,
	innerCorr values.CorrelationIdentifier,
	u *logical.LogicalUnnest,
	elementType values.Type,
	enclosed bool,
) values.Value {
	outerCols := t.legColumns(outer)
	if outerCols == nil {
		return nil
	}
	if producerCensusObserver != nil {
		producerCensusObserver(ProducerCensusRecord{Producer: "P5", Enclosed: enclosed, Shape: fmt.Sprintf("unnest|%T", outer)})
	}
	// Outer leg: bare + qualified ALIAS.COL fields, exactly as a normal join leg.
	base := values.NewAnchoredJoinRecord([]values.AnchoredJoinLeg{
		{Alias: outerCorr, Columns: outerCols},
	})
	// MULTI-SOURCE outer (`FROM A, B, A.arr AS X`): legColumns(LogicalJoin)
	// returns ONLY the already-qualified DOTTED columns (A.C, A.ARR, B.D, …);
	// NewAnchoredJoinRecord propagates a dotted leg column VERBATIM with NO bare
	// form (the nested-join rule). So the anchored outer record above carries only
	// dotted keys — `SELECT C` (a bare outer column) and `ORDER BY C` would read a
	// MISSING bare `C` key → NULL column / no-op sort. But the runtime merged outer
	// row bound under QOV(outerCorr) ALSO carries bare keys (mergeRows writes both
	// bare last-leg-wins AND ALIAS.COL — executor.go), exactly as a real join's
	// result row does. Emit the matching bare fields here so the FlatMap RETURN
	// value carries the outer merged row's BARE keys as well as the qualified ones,
	// faithful to mergeRows/NewAnchoredJoinRecord. The bare key is read off
	// QOV(outerCorr) by its bare name; last-occurrence (= right-leg) wins on a
	// cross-leg collision, matching NewAnchoredJoinRecord's leg-order last-leg-wins.
	// A single outer SCAN (`FROM t, t.arr`) already exposes bare columns via
	// legColumns(LogicalScan), so base already carries them and this adds nothing
	// new (the bareSeen guard dedups). The unnest AS/AT shadowing still applies
	// below (the element/ordinal bare key wins over a same-named outer bare key).
	// RFC-142.
	bareSeen := map[string]struct{}{}
	for _, f := range base.Fields {
		if !strings.Contains(f.Name, ".") {
			bareSeen[strings.ToUpper(f.Name)] = struct{}{}
		}
	}
	outerBareLast := map[string]values.Value{}
	var outerBareOrder []string
	for _, c := range outerCols {
		dot := strings.LastIndexByte(c.Name, '.')
		if dot < 0 {
			continue // a bare leg column — base already emitted it
		}
		bare := strings.ToUpper(c.Name[dot+1:])
		if _, ok := bareSeen[bare]; ok {
			continue // base already carries this bare key (single-scan leg)
		}
		// FieldValue(QOV(outerCorr), bare) reads the merged row's bare key.
		v := values.NewFieldValue(values.NewQuantifiedObjectValue(outerCorr), bare, c.FieldType)
		if _, dup := outerBareLast[bare]; !dup {
			outerBareOrder = append(outerBareOrder, bare)
		}
		outerBareLast[bare] = v // last-occurrence (right leg) wins
	}
	// The unnest's AS/AT aliases SHADOW any same-named outer column: `... AS x`
	// binds x to the element, even when the outer source already has a column
	// named x (the name-collision case). Drop the outer's BARE field for a
	// colliding name so the unnest's bare field is authoritative; the outer's
	// explicitly-qualified `OUTER.x` form is preserved for an outer-qualified
	// reference. RFC-142.
	shadowed := map[string]struct{}{}
	if u.Alias != "" {
		shadowed[strings.ToUpper(u.Alias)] = struct{}{}
	}
	if u.AtAlias != "" {
		shadowed[strings.ToUpper(u.AtAlias)] = struct{}{}
	}
	var fields []values.RecordConstructorField
	for _, f := range base.Fields {
		if _, clash := shadowed[strings.ToUpper(f.Name)]; clash && !strings.Contains(f.Name, ".") {
			continue
		}
		fields = append(fields, f)
	}
	// Append the derived bare keys for a multi-source (dotted) outer leg, in
	// stable order, skipping any name the unnest AS/AT shadows.
	for _, bare := range outerBareOrder {
		if _, clash := shadowed[bare]; clash {
			continue
		}
		fields = append(fields, values.RecordConstructorField{Name: bare, Value: outerBareLast[bare]})
	}

	innerQOV := values.NewQuantifiedObjectValue(innerCorr)
	withOrdinality := u.AtAlias != ""

	// The AS-bound element. With ordinality, the inner flows a 2-field record;
	// the element is ordinal field 0 (its type carried by the FieldValue). Without,
	// the inner flows the BARE element — the alias IS the whole flowed object
	// (Java's generateCorrelatedFieldAccess primitive branch binds to the QOV, NOT
	// a FieldValue). A plain QOV defaults to UnknownType, which result-set column
	// metadata would report as BIGINT; bind the element's flowed type to the array's
	// elementType (STRING for a STRING array, etc.) so the element column advertises
	// its real type, matching the ordinality path. RFC-142.
	var elementValue values.Value
	if withOrdinality {
		elementValue = values.NewOrdinalFieldValue(innerQOV, 0, elementType)
	} else {
		elementValue = values.NewQuantifiedObjectValueOfType(innerCorr, elementType)
	}
	// The unnest leg's source alias — how the SELECT scope qualifies a
	// reference to the AS/AT column (the unnest virtual source's correlation
	// name). Key both the bare and the `<leg>.<col>` qualified forms so a
	// qualified reference (`<leg>.AT`) also resolves against the FlatMap output.
	// RFC-142.
	legAlias := strings.ToUpper(u.Alias)
	if legAlias == "" {
		legAlias = strings.ToUpper(u.AtAlias)
	}
	addField := func(bareKey string, v values.Value) {
		fields = append(fields, values.RecordConstructorField{Name: bareKey, Value: v})
		if q := legAlias + "." + bareKey; q != bareKey {
			fields = append(fields, values.RecordConstructorField{Name: q, Value: v})
		}
	}
	if u.Alias != "" {
		addField(strings.ToUpper(u.Alias), elementValue)
	}
	// The AT-bound 1-based ordinal (INT NOT NULL), ordinal field 1.
	if withOrdinality {
		addField(strings.ToUpper(u.AtAlias), values.NewOrdinalFieldValue(innerQOV, 1, values.NotNullInt))
	}

	rc := values.NewRecordConstructorValue(fields...)
	rc.AnchoredJoin = true
	return rc
}

// rewriteUnnestPredicate rewrites a WHERE predicate's references to a lateral
// unnest's AS/AT columns so they match what the inner Explode actually flows,
// then the NLJ rule pushes the rewritten predicate into the inner Explode filter
// (Java's `EXPLODE … | FILTER …`). The unnest's WHERE references are
// `FieldValue{Field:<alias>, Child:QOV(unnestCorr)}` (the virtual scope source
// resolves the AS/AT columns to a field over the unnest correlation). What the
// inner Explode flows depends on the ordinality:
//
//   - WITH ORDINALITY: the inner flows a 2-field record (`_0`=element,
//     `_1`=ordinal). Rewrite the AS reference to ordinal field 0 and the AT
//     reference to ordinal field 1 (`FieldValue{_0|_1, QOV}`) — Java's
//     `FieldValue.ofOrdinalNumber(qov, 0|1)`.
//
//   - NON-ORDINAL: the inner flows the BARE SCALAR element (no struct). The AS
//     reference must collapse to the WHOLE `QuantifiedObjectValue(unnestCorr)`
//     (the scalar itself), NOT a FieldValue over it — a FieldValue would read a
//     named subfield of a scalar and evaluate NULL, filtering everything out.
//     This mirrors Java's `generateCorrelatedFieldAccess` primitive branch, which
//     binds the alias directly to `resultingQuantifier.getFlowedObjectValue()`
//     (the QOV) rather than a FieldValue accessor. RFC-142.
func rewriteUnnestPredicate(p predicates.QueryPredicate, u *logical.LogicalUnnest) predicates.QueryPredicate {
	unnestCorr := unnestSourceCorrelation(u)
	asAlias := strings.ToUpper(u.Alias)
	atAlias := strings.ToUpper(u.AtAlias)
	withOrdinality := u.AtAlias != ""
	rewriteValue := func(v values.Value) values.Value {
		if v == nil {
			return v
		}
		return values.Replace(v, func(node values.Value) values.Value {
			fv, ok := node.(*values.FieldValue)
			if !ok || fv.Child == nil {
				return node
			}
			qov, ok := fv.Child.(*values.QuantifiedObjectValue)
			if !ok || qov.Correlation != unnestCorr {
				return node
			}
			switch strings.ToUpper(fv.Field) {
			case asAlias:
				if asAlias != "" {
					if withOrdinality {
						return values.NewOrdinalFieldValue(qov, 0, fv.Typ)
					}
					// Bare scalar element: the alias IS the whole flowed object.
					return qov
				}
			case atAlias:
				if atAlias != "" {
					return values.NewOrdinalFieldValue(qov, 1, fv.Typ)
				}
			}
			return node
		})
	}
	return mapPredicateValues(p, rewriteValue)
}

// buriedUnnestLegs collects every lateral-unnest leg in `op` whose element/ordinal
// columns survive into the outer (NON-rightmost) join's merged row — i.e. an unnest
// BURIED in the left subtree of a 3+-source FROM list (`FROM T1, T1.arr AS V, U`,
// where the outer LogicalJoin's Right is U and the unnest is in its Left). Mirrors
// the `containsLateralUnnest` recursion (and `outerBoundAliases` /
// `findOuterScanTable`): it does NOT descend into a CTE / derived-table Body — a
// derived source is its own FROM scope, and its inner unnest belongs to that scope,
// not the current one. RFC-142.
func buriedUnnestLegs(op logical.LogicalOperator) []*logical.LogicalUnnest {
	var out []*logical.LogicalUnnest
	var walk func(logical.LogicalOperator)
	walk = func(o logical.LogicalOperator) {
		if o == nil {
			return
		}
		if u, ok := o.(*logical.LogicalUnnest); ok {
			out = append(out, u)
			return
		}
		if cte, ok := o.(*logical.LogicalCTE); ok {
			// A derived/CTE source is its own FROM scope; only its visible Main.
			walk(cte.Main)
			return
		}
		for _, c := range o.Children() {
			walk(c)
		}
	}
	walk(op)
	return out
}

// predicateRefsCorrelation reports whether predicate p references the
// correlation identifier corr anywhere in its value tree (GetCorrelatedTo).
func predicateRefsCorrelation(p predicates.QueryPredicate, corr values.CorrelationIdentifier) bool {
	if p == nil {
		return false
	}
	_, ok := predicates.GetCorrelatedToOfPredicate(p)[corr]
	return ok
}

// pushBuriedUnnestPredicateDown rewrites the logical tree so a WHERE conjunct that
// filters a BURIED lateral-unnest element/ordinal — an unnest that is NOT the
// rightmost FROM item (`FROM T1, T1.arr AS V, U WHERE V > 0`, where the outer
// LogicalJoin's Right is U and the unnest is in its Left) — is pushed DOWN to a
// LogicalFilter wrapping the inner join in which the unnest IS the rightmost source.
// That makes the buried case structurally identical to the direct
// `FROM T1, T1.arr AS V WHERE V > 0` shape, so the SAME proven direct-unnest WHERE
// path (rewriteUnnestPredicate → folded into the inner Explode's PredicatesFilter,
// Java's `EXPLODE … | FILTER …`) handles it — for EVERY comparison operator. Left at
// the OUTER NestedLoopJoin the element reference would read the FlatMap binding under
// an ambiguous correlation and evaluate NULL → every matching row dropped (P1,
// silent-wrong).
//
// Only a conjunct that references the buried unnest's correlation AND no source
// OUTSIDE join.Left (i.e. not the rightmost-leg join.Right) is pushed — a conjunct
// also referencing the rightmost leg (`V = U.x`) is a genuine cross-leg join
// predicate and STAYS at the outer level. The returned operator is the restructured
// tree (`Join(Filter(Left, pushedConjuncts), Right)` under the residual
// LogicalFilter); when nothing is pushable f is returned unchanged so non-buried and
// pure-cross-leg shapes are untouched. RFC-142.
//
// EXISTS composition (silent-wrong): this push runs BEFORE the
// EXISTS early-return in translateFilter so a buried-unnest element/ordinal filter
// combined with EXISTS (`FROM T1, T1.arr AS V, U WHERE V > 1 AND EXISTS (…)`) is
// pushed into the inner Explode FIRST. Otherwise the EXISTS dispatch routes the
// whole filter through the generic join+EXISTS path (translateJoinWithExists), which
// appends `V > 1` to the outer NLJ's predicates where QOV(V) is UNBOUND → every
// matching row silently dropped. Only the buried-unnest NON-EXISTS conjuncts move
// down; the EXISTS subqueries + their existential markers (extractExistsPredicates)
// stay in the residual outer filter so the existential semi-join is preserved.
func pushBuriedUnnestPredicateDown(f *logical.LogicalFilter) *logical.LogicalFilter {
	if f == nil || f.Predicate == nil {
		return f
	}
	join, ok := f.Input.(*logical.LogicalJoin)
	if !ok || join.Kind != logical.JoinInner {
		return f
	}
	// Only the BURIED shape: the rightmost source (join.Right) is NOT itself the
	// unnest (that direct shape is handled by the existing path), but join.Left
	// contains one or more buried unnest legs.
	if _, rightIsUnnest := join.Right.(*logical.LogicalUnnest); rightIsUnnest {
		return f
	}
	buried := buriedUnnestLegs(join.Left)
	if len(buried) == 0 {
		return f
	}
	// The aliases bound by the RIGHTMOST leg (join.Right) — a conjunct that
	// references any of these is a cross-leg predicate and must NOT be pushed
	// below the join.
	rightAliases := outerBoundAliases(join.Right)

	conjuncts := splitNonExistsPredicates(f.Predicate)
	var pushed, residual []predicates.QueryPredicate
	for _, c := range conjuncts {
		corrSet := predicates.GetCorrelatedToOfPredicate(c)
		// References the rightmost leg? → cross-leg, stays at the outer level.
		refsRight := false
		for ra := range rightAliases {
			if _, ok := corrSet[values.NamedCorrelationIdentifier(ra)]; ok {
				refsRight = true
				break
			}
		}
		// References any buried unnest's element/ordinal correlation?
		refsUnnest := false
		for _, u := range buried {
			if predicateRefsCorrelation(c, unnestSourceCorrelation(u)) {
				refsUnnest = true
				break
			}
		}
		if refsUnnest && !refsRight {
			pushed = append(pushed, c)
		} else {
			residual = append(residual, c)
		}
	}
	if len(pushed) == 0 {
		return f
	}

	// Wrap join.Left in a LogicalFilter carrying the pushed conjuncts (where the
	// unnest is the rightmost source → the direct-unnest WHERE path fires), then
	// re-join with join.Right. The residual conjuncts stay in the outer filter.
	innerFilter := &logical.LogicalFilter{Input: join.Left, Predicate: andOf(pushed)}
	newJoin := &logical.LogicalJoin{
		Left:        innerFilter,
		Right:       join.Right,
		Kind:        join.Kind,
		OnText:      join.OnText,
		OnPredicate: join.OnPredicate,
	}
	// When the original filter carried EXISTS, the existential markers
	// (ExistentialValuePredicate / NOT(...)) and their subqueries MUST survive in
	// the residual outer filter — splitNonExistsPredicates dropped the markers, so
	// re-attach them. The EXISTS dispatch in translateFilter then runs on the
	// restructured tree (the buried-unnest element conjuncts already pushed into the
	// inner Explode) and threads only the existential + remaining outer conjuncts to
	// the semi-join. RFC-142.
	residualPreds := append([]predicates.QueryPredicate{}, residual...)
	residualPreds = append(residualPreds, extractExistsPredicates(f.Predicate)...)
	return &logical.LogicalFilter{
		Input:            newJoin,
		Predicate:        andOf(residualPreds),
		ExistsSubqueries: f.ExistsSubqueries,
		ScalarSubqueries: f.ScalarSubqueries,
	}
}

// mapPredicateValues applies fn to every Value operand of a predicate tree,
// reconstructing the predicate. Mirrors the shapes the cascades NLJ rule's
// rebaseOuterLegRefsToMerged handles (Comparison/Value/And/Or/Not); other
// shapes pass through unchanged. RFC-142.
func mapPredicateValues(p predicates.QueryPredicate, fn func(values.Value) values.Value) predicates.QueryPredicate {
	if p == nil {
		return p
	}
	switch pred := p.(type) {
	case *predicates.ComparisonPredicate:
		newOperand := fn(pred.Operand)
		newCompOperand := pred.Comparison.Operand
		if newCompOperand != nil {
			newCompOperand = fn(newCompOperand)
		}
		if newOperand == pred.Operand && newCompOperand == pred.Comparison.Operand {
			return p
		}
		// Copy the whole Comparison and replace ONLY the rebased RHS operand,
		// preserving Escape (the LIKE escape rune) AND every other Comparison
		// subclass field (ParameterName, the Text* tokenizer/analyzer/distance
		// fields, the DistanceRank vector fields). A partial {Type, Operand,
		// Escape} reconstruction would silently drop the rest. RFC-142.
		cmp := pred.Comparison
		cmp.Operand = newCompOperand
		return &predicates.ComparisonPredicate{
			Operand:    newOperand,
			Comparison: cmp,
		}
	case *predicates.ValuePredicate:
		newVal := fn(pred.Value)
		if newVal == pred.Value {
			return p
		}
		return predicates.NewValuePredicate(newVal)
	case *predicates.AndPredicate:
		subs := make([]predicates.QueryPredicate, len(pred.SubPredicates))
		changed := false
		for i, s := range pred.SubPredicates {
			subs[i] = mapPredicateValues(s, fn)
			if subs[i] != s {
				changed = true
			}
		}
		if !changed {
			return p
		}
		return predicates.NewAnd(subs...)
	case *predicates.OrPredicate:
		subs := make([]predicates.QueryPredicate, len(pred.SubPredicates))
		changed := false
		for i, s := range pred.SubPredicates {
			subs[i] = mapPredicateValues(s, fn)
			if subs[i] != s {
				changed = true
			}
		}
		if !changed {
			return p
		}
		return predicates.NewOr(subs...)
	case *predicates.NotPredicate:
		newChild := mapPredicateValues(pred.Child, fn)
		if newChild == pred.Child {
			return p
		}
		return predicates.NewNot(newChild)
	default:
		return p
	}
}

// unnestSourceCorrelation is the correlation the unnest's WHERE references are
// qualified by — the AS alias, else the AT alias (mirroring
// unnestScopeSourceAdder's correlation-name choice). RFC-142.
func unnestSourceCorrelation(u *logical.LogicalUnnest) values.CorrelationIdentifier {
	corr := u.Alias
	if corr == "" {
		corr = u.AtAlias
	}
	return values.NamedCorrelationIdentifier(corr)
}

// newLimitExprFromLogical builds the Cascades LogicalLimitExpression for a
// logical LIMIT, preserving a runtime (parameterized) row cap when present
// (RFC-156 `... <= ?` vector rank limit). The single source of truth for the
// static-vs-runtime split so every LIMIT translation site is identical.
func newLimitExprFromLogical(o *logical.LogicalLimit, q expressions.Quantifier) *expressions.LogicalLimitExpression {
	if o.LimitValue != nil {
		return expressions.NewRuntimeLogicalLimitExpression(o.LimitValue, o.Offset, q)
	}
	return expressions.NewLogicalLimitExpression(o.Limit, o.Offset, q)
}

func (t *cascadesTranslator) translateOp(op logical.LogicalOperator) expressions.RelationalExpression {
	if op == nil {
		return nil
	}
	// RFC-173 Slice 2: opaque boundaries cut inner-join-cluster enclosure —
	// they lower to non-SelectExpression boxes SelectMergeRule cannot merge
	// through, so a join beneath them roots its OWN cluster (fresh gate walk).
	// Transparent ops (scan/filter/project/CTE) and joins (which manage their
	// legs' enclosure themselves) preserve the flag. Missing a type here fails
	// SAFE: the nested join stays "enclosed" → name model.
	switch op.(type) {
	case *logical.LogicalAggregate, *logical.LogicalDistinct, *logical.LogicalSort,
		*logical.LogicalLimit, *logical.LogicalUnion,
		*logical.LogicalInsert, *logical.LogicalUpdate, *logical.LogicalDelete:
		if t.inInnerCluster {
			defer func() { t.inInnerCluster = true }()
			t.inInnerCluster = false
		}
	}
	switch o := op.(type) {
	case *logical.LogicalScan:
		return t.translateScan(o)
	case *logical.LogicalFilter:
		return t.translateFilter(o)
	case *logical.LogicalLimit:
		// (helper below threads o.LimitValue for parameterized RFC-156 rank caps)
		// Every LIMIT — top-level and nested alike — is translated to a
		// LogicalLimitExpression (→ RecordQueryLimitPlan) so it is applied
		// at its correct pipeline position by the operator. There is no
		// post-execution hoist anymore (see RFC-128): a nested LIMIT under
		// a Filter/Sort/Join inside a derived table must NOT be lifted to
		// the top-level pagination, which produced wrong rows. This mirrors
		// the correlated-scalar path (translateProjectWithCorrelatedScalar),
		// which already peels the inner LIMIT and re-emits it here.
		innerRef := t.translateRef(o.Input)
		if innerRef == nil {
			return nil
		}
		limitQ := t.namedQuantifier(sourceAlias(o.Input), innerRef)
		return newLimitExprFromLogical(o, limitQ)
	case *logical.LogicalUnion:
		return t.translateUnion(o)
	case *logical.LogicalSort:
		return t.translateSort(o)
	case *logical.LogicalProject:
		return t.translateProject(o)
	case *logical.LogicalJoin:
		return t.translateJoin(o)
	case *logical.LogicalAggregate:
		return t.translateAggregate(o)
	case *logical.LogicalDistinct:
		return t.translateDistinct(o)
	case *logical.LogicalCTE:
		return t.translateCTE(o)
	case *logical.LogicalInsert:
		return t.translateInsert(o)
	case *logical.LogicalUpdate:
		return t.translateUpdate(o)
	case *logical.LogicalDelete:
		return t.translateDelete(o)
	default:
		return nil
	}
}

func (t *cascadesTranslator) translateScan(s *logical.LogicalScan) expressions.RelationalExpression {
	key := strings.ToUpper(s.Table)
	// Pre-translated expression scope (recursive CTE references).
	if expr, ok := t.cteExprScope[key]; ok {
		return expr
	}
	if body, ok := t.cteScope[key]; ok {
		// Translate the body in its DEFINING scope (shadow-stack pop): its
		// own references to this name resolve to the shadowed outer binding
		// when one exists (the derived alias-carrier over `SELECT * FROM c`
		// inside `WITH c AS (…)`), to the real table otherwise — never back
		// to the CTE itself (infinite recursion).
		var result expressions.RelationalExpression
		t.inCTEDefiningScope(key, body, func() {
			result = t.translateOp(body)
		})
		return result
	}
	// Type the scan leaf with the table's canonical record type (RFC-173
	// Slice 1). tableColumns sources fields from the proto descriptor in
	// declaration order with UPPER-cased names — the SAME order and case the
	// runtime PositionalRow carries (protoToPositional), so FieldValue.
	// resolveOrdinal's plan-time ordinal matches the runtime slot by
	// construction. Two scans of one table build structurally-equal
	// RecordTypes (tableColumns is deterministic; RecordType.Equals is
	// structural), so memo dedup on flowedType (FullUnorderedScanExpression.
	// EqualsWithoutChildren) holds without a pointer cache. An unresolvable
	// table (no metadata) degrades to UnknownType → name resolution, as before.
	flowed := values.Type(values.UnknownType)
	if cols := t.tableColumns(s.Table); len(cols) > 0 {
		flowed = values.NewRecordType("", false, cols)
	}
	return expressions.NewFullUnorderedScanExpression(
		[]string{s.Table}, flowed,
	)
}

func (t *cascadesTranslator) translateFilter(f *logical.LogicalFilter) expressions.RelationalExpression {
	// RFC-173: fold a POSITIVE WHERE-EXISTS over an unconditional-one-row
	// aggregate (esq.AlwaysTrue) to TRUE — drop its existential quantifier AND
	// rewrite its EXISTS marker in the predicate to TRUE (foldAlwaysTrueExists),
	// before any routing. EXISTS(unconditional-one-row) is always satisfied, so
	// `P AND EXISTS(...)` collapses to `P`. Running here — ahead of the
	// join-flatten — means the correlated-aggregate semi-join is never built, so
	// the joined-outer / windowed-DML hazards of the semi-join approach cannot
	// arise. NOT EXISTS (negated) and projected consumers do NOT fold.
	f = t.foldAlwaysTrueExists(f)
	// Narrowed correct-or-loud decline: a SCALAR SUBQUERY in a filter over a chained
	// unnest (`FROM t, t.a AS x, x.b AS y WHERE t.id = (SELECT …)`) rides the wedgeGate
	// POSITIONAL bake (rebaseUnnestOuterLegPredicateOrdinal), NOT the per-conjunct
	// name-keyed rebase, and bakes the outer ref to ordinal -1 → a malformed plan. The
	// name-model residual answered it SILENT-WRONG ([] instead of Java's rows). Reject it
	// LOUDLY (0A000) rather than ship either the internal error or the silent-wrong rows.
	// Java answers this (reach gap, booked as its own slice: make the positional-bake path
	// carry the scalar-subquery predicate). TYPED detection (f.ScalarSubqueries), never a
	// text match. Every NON-subquery filter over a chained unnest ordinalizes correctly and
	// falls through here — the coarse "any filter suppresses" decline is retired.
	if len(f.ScalarSubqueries) > 0 && filterInputHasChainedUnnest(f.Input) {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedOperation,
			"scalar subquery in a filter over a chained lateral unnest is not yet supported"))
		return nil
	}
	if f.Predicate == nil && f.PredicateText != "" {
		return nil
	}
	if f.Predicate != nil && predicateContainsUnsafeFunction(f.Predicate) {
		return nil
	}

	// BURIED lateral-unnest element/ordinal WHERE (P1): push the
	// unnest-element conjuncts of a non-rightmost unnest (`FROM T1, T1.arr AS V, U
	// WHERE V > 0`) DOWN to a filter wrapping the inner join where the unnest IS the
	// rightmost source, so the proven direct-unnest WHERE path folds them into the
	// inner Explode (Java's `EXPLODE … | FILTER …`) instead of leaving them on the
	// outer NestedLoopJoin (where the element reference evaluates NULL and drops
	// every row). A no-op for non-buried / pure-cross-leg shapes. The restructured
	// filter's residual conjuncts (cross-leg / outer-table predicates) flow through
	// the normal path below. RFC-142.
	//
	// This runs BEFORE the EXISTS dispatch: when a buried-unnest
	// element/ordinal filter is combined with EXISTS (`… WHERE V > 1 AND EXISTS
	// (…)`), the EXISTS early-return below would otherwise route the WHOLE filter
	// through translateJoinWithExists, appending `V > 1` to the outer NLJ where
	// QOV(V) is unbound → every matching row silently dropped. pushBuriedUnnest
	// PredicateDown pushes only the buried-unnest NON-EXISTS conjuncts into the inner
	// Explode and preserves the EXISTS subqueries + existential markers in the
	// residual outer filter, so the EXISTS dispatch then handles only the remaining
	// existential + outer conjuncts. RFC-142.
	// RFC-173 W5 commit 3: when the input is an ENCLOSED-unnest cluster the
	// gathered path will take (`FROM A, A.arr AS x, B WHERE …`), the push must
	// STAND DOWN — its restructured tree (a Filter wrapping the unnest join)
	// un-gathers the cluster, and a spanning conjunct (`x > B.c`, never
	// pushable) would then land raw on the residual NLJ where the element is
	// unbound → silent 0 rows. With the push skipped, the WHOLE conjunct set
	// reaches the gathered merge arm below, which rewrites element refs and
	// bakes leg refs uniformly (the root form's exact treatment — pushBuried
	// already skips the root form for the same reason). The probe TRANSLATES
	// the rotated cluster; the built select is MEMOIZED on the translator
	// (enclosedGatherCache, consume-once) so the dispatch below returns it
	// instead of translating the cluster twice. Fail-open: a declined gather
	// keeps today's push semantics.
	//
	// The `!t.inInnerCluster` read is caller-contextual (merge-leg vs fresh-root, not
	// subtree-derivable); its decouple to a downward enclosure parameter is booked in
	// TODO.md. translateProjectOverExistsFilter documents why retiring that producer
	// (`=false`) hands an enclosed derived body this gathered path correct-direction.
	enclosedGathered := false
	if f.Predicate != nil && len(f.ExistsSubqueries) == 0 && !t.inInnerCluster && !t.unnestUnderExistential {
		if join, isJ := f.Input.(*logical.LogicalJoin); isJ {
			if rebuilt, ru, et, fn, rpos, rok := t.rotateEnclosedUnnest(join); rok {
				if sel := t.translateGatheredUnnestCluster(
					rebuilt, ru, unnestSourceCorrelation(ru), et, fn, rpos,
				); sel != nil {
					enclosedGathered = true
					if t.enclosedGatherCache == nil {
						t.enclosedGatherCache = make(map[*logical.LogicalJoin]expressions.RelationalExpression)
					}
					t.enclosedGatherCache[join] = sel
				}
			}
		}
	}

	pushedAllBuried := false
	if f.Predicate != nil && !enclosedGathered {
		pushed := pushBuriedUnnestPredicateDown(f)
		if pushed != f && pushed.Predicate == nil && len(pushed.ExistsSubqueries) == 0 {
			// Every conjunct was pushed below the join (the buried-unnest
			// all-element WHERE, no EXISTS) — the residual filter is empty.
			pushedAllBuried = true
		}
		f = pushed
	}

	// Collect scalar subquery plans — they'll be planned independently
	// and pre-evaluated by the executor.
	for _, ssq := range f.ScalarSubqueries {
		t.scalarSubqueries = append(t.scalarSubqueries, ScalarSubqueryPlan{
			Alias: ssq.Alias,
			Plan:  ssq.Plan,
		})
	}

	// All conjuncts pushed below the join → lower the restructured join directly
	// rather than wrapping it in a no-op [0 preds] PredicatesFilter. (Scalar
	// subqueries were already collected above; ExistsSubqueries is empty here.)
	if pushedAllBuried {
		return t.translateOp(f.Input)
	}

	// EXISTS subqueries: when the filter carries existential subquery
	// plans, build a SelectExpression with ForEach + Existential
	// quantifiers. The ExistentialValuePredicate in the predicate tree references
	// the existential alias; the planner's ImplementSimpleSelectRule
	// handles the existential quantifier via FirstOrDefaultPlan.
	// RFC-141: the existential quantifier attaches whenever the filter
	// carries existential subqueries — including a PROJECTED EXISTS with no
	// WHERE (f.Predicate == nil). For a projected-only EXISTS the existential
	// boolean is computed by the projection's ExistsValue, so no existential
	// WHERE filter is generated; the quantifier still must attach so the
	// FlatMap (FirstOrDefault inner) is built.
	if len(f.ExistsSubqueries) > 0 {
		// When the filter's input is a join, flatten into a single
		// SelectExpression with ForEach(left), ForEach(right), and
		// Existential(exists_scan). This avoids nesting one
		// SelectExpression (the join) inside another (the EXISTS filter),
		// which causes the Cascades planner to diverge. The NLJ rule
		// handles the 2+1 quantifier shape directly.
		//
		// EXCEPTION — a lateral array UNNEST right child (`FROM t, t.arr AS v
		// WHERE EXISTS (…)`): the flatten path would feed the CORRELATED Explode
		// into implementJoinWithExistential's binary NLJ, which materializes its
		// inner ONCE against an unbound context — the correlated Explode yields no
		// rows and the query returns empty. The unnest MUST stay its own
		// FlatMap-over-Explode (translateUnnestJoin) as the existential's OUTER.
		// translateUnnestExistsFilter builds that nested shape (and folds any
		// WHERE-on-the-unnest-column into the inner Explode). RFC-142 (P2b).
		if join, ok := f.Input.(*logical.LogicalJoin); ok {
			if u, isUnnest := join.Right.(*logical.LogicalUnnest); isUnnest {
				return t.translateUnnestExistsFilter(f, join, u)
			}
			// The join+EXISTS FLATTEN is INNER-only: it merges the WHERE's
			// non-EXISTS conjuncts into the select's predicate list, which
			// the existential implementation feeds to the NLJ as JOIN
			// predicates — for an OUTER join that turns a preserved-side
			// WHERE conjunct into ON semantics (the failing row NULL-PADS
			// instead of dropping: `dept d LEFT JOIN emp e ... WHERE d.id=3
			// AND NOT EXISTS(...)` returned every dept — W4-left red-first
			// pin g; master-identical, a pre-existing silent-wrong). OUTER
			// kinds fall through to the generic arm below: the join
			// translates as its own (enclosed) select with proper
			// WHERE-above-LEFT placement and the existential select wraps
			// it — post-rewriting the box dissolves and merges back into
			// the 2+1 shape with the predicates at their correct levels.
			if join.Kind == logical.JoinInner {
				return t.translateJoinWithExists(join, f)
			}
		}
	}

	// When the filter wraps an INNER join (FROM a, b WHERE ...), merge
	// the WHERE predicates into the SelectExpression so the NLJ rule
	// sees them as join predicates. For LEFT OUTER joins, the WHERE
	// must stay as a filter ABOVE the join — merging would turn WHERE
	// conditions into ON conditions, preventing NULL-padded rows from
	// being properly filtered.
	if join, ok := f.Input.(*logical.LogicalJoin); ok && f.Predicate != nil && len(f.ExistsSubqueries) == 0 && join.Kind != logical.JoinLeft && join.Kind != logical.JoinRight && join.Kind != logical.JoinFull {
		// A NON-EXISTS direct unnest whose outer is a multi-alias box: a regular
		// WHERE conjunct on a box leg cannot be baked positionally (it merges into
		// the name-keyed unnest SELECT below via the name-model rebase), so flag it
		// BEFORE translating the unnest — unnestExistsSeedSafe then declines the box
		// to name-model, where the conjunct resolves via qualified keys. Same
		// narrowing as the EXISTS path (translateUnnestExistsFilter); the flag is
		// checked in both. Restored after so no sibling translation observes it.
		prevOuterConj := t.unnestOuterConjunctOnBoxLeg
		if _, isUnnest := join.Right.(*logical.LogicalUnnest); isUnnest {
			t.unnestOuterConjunctOnBoxLeg = nonExistsConjunctRefsOuterLeg(f.Predicate, outerBoundAliases(join.Left))
		}
		joinExpr := t.translateJoin(join)
		t.unnestOuterConjunctOnBoxLeg = prevOuterConj
		if joinExpr == nil {
			return nil
		}
		if sel, ok := joinExpr.(*expressions.SelectExpression); ok {
			pred := f.Predicate
			// A WHERE predicate over a lateral array unnest references the
			// unnest's AS/AT columns by their user aliases (qualified by the
			// unnest correlation). The inner Explode does NOT flow a row keyed by
			// those aliases, so rewrite the references to what it actually flows:
			//   - WITH ORDINALITY: a 2-field record keyed `_0`/`_1` → VAL becomes
			//     ordinal-0, AT becomes ordinal-1 over the unnest QOV.
			//   - NON-ORDINAL: a BARE SCALAR element → the AS reference collapses
			//     to the whole QOV(unnestCorr) (the scalar itself); a FieldValue
			//     over the scalar would read a named subfield of a scalar and
			//     evaluate NULL, filtering every element out (the P1a bug).
			// The NLJ rule then pushes the rewritten predicate into the inner
			// Explode filter (Java's `EXPLODE … | FILTER …`). RFC-142.
			//
			// A WHERE that also references an OUTER-LEG column needs a second
			// rebase when the unnest follows ≥2 prior sources (`FROM A, B, A.arr
			// AS X WHERE X = A.c`). rewriteUnnestPredicate touches only the X/AT
			// references; `A.c` stays `FieldValue{c, QOV(A)}`. But the unnest
			// FlatMap binds the merged outer row under sourceAlias(j.Left) (the
			// RIGHTMOST leg B), so QOV(A) is UNBOUND inside the inner Explode's
			// PredicatesFilter → `X = NULL` drops every matching element. The
			// merged row carries the qualified `A.c` key (mergeRows/
			// NewAnchoredJoinRecord), so rebase any outer-leg reference (any
			// outerBoundAliases(j.Left) leg, e.g. A) to that key off the merged
			// QOV — the SAME outer-leg-to-merged rebase the EXISTS path
			// (rebaseUnnestOuterLegPredicate) and the real-JOIN+EXISTS path
			// (rebaseOuterLegRefsToMerged) perform. A single outer scan
			// (`FROM t, t.arr`) flows under segment-0's own alias, so its leg is
			// the merged corr itself and the rebase is a no-op. RFC-142.
			// RFC-173 W5 commit 3: the ENCLOSED gathered form (`FROM A, A.arr
			// AS x, B WHERE …` — translateJoin above returned the flat
			// gathered select via rotation). Same WHERE treatment as the
			// root-form gathered arm below: rewrite element/ordinal refs to
			// what the Explode flows, bake leg refs through the rotated plain
			// cluster's own leg types. The signature check (>2 quantifiers,
			// SOME quantifier binds the unnest correlation — the Explode sits
			// at its FROM position, mid-list for the enclosed form) keeps
			// declined residual translations on the name-model path below.
			if _, rootUnnest := join.Right.(*logical.LogicalUnnest); !rootUnnest && enclosedGathered {
				if rebuilt, ru, _, _, _, rok := t.rotateEnclosedUnnest(join); rok {
					quants := sel.GetQuantifiers()
					bindsUnnest := false
					for _, q := range quants {
						if q.GetAlias() == unnestSourceCorrelation(ru) {
							bindsUnnest = true
							break
						}
					}
					if len(quants) > 2 && bindsUnnest {
						toMerge := []predicates.QueryPredicate{rewriteUnnestPredicate(pred, ru)}
						if lj, isLJ := rebuilt.Left.(*logical.LogicalJoin); isLJ {
							toMerge = bakeGatedJoinPredicates(toMerge, t.gatedJoinLegTypes(lj))
						}
						return expressions.NewSelectExpressionWithJoinType(
							sel.GetResultValue(),
							sel.GetQuantifiers(),
							append(sel.GetPredicates(), toMerge...),
							sel.GetSourceAliases(),
							sel.GetJoinType(),
						)
					}
				}
			}
			if u, ok := join.Right.(*logical.LogicalUnnest); ok {
				pred = rewriteUnnestPredicate(pred, u)
				if len(sel.GetQuantifiers()) > 2 {
					// RFC-173 W5: the GATHERED flat unnest select — the cluster
					// legs are the select's OWN quantifiers, so an outer-leg
					// reference (`FieldValue(QOV(MA), c)`) is a GENUINE leg ref;
					// there is no merged row to rebase onto (the rebase would
					// point refs at a key the flat select never flows). Bake
					// cross-leg conjuncts through the cluster's own spine
					// instead, exactly as a gated join's WHERE merge does.
					//
					// (The commit-1 spanning fail-open is GONE — it guarded a
					// RETRACTED phantom: non-discriminating seed data misread a
					// correct all-rows result as a dropped predicate. Spanning
					// conjuncts classify, rewrite (bare QOVs are translated),
					// and filter correctly through the gathered path.)
					toMerge := []predicates.QueryPredicate{pred}
					if lj, isLJ := join.Left.(*logical.LogicalJoin); isLJ {
						toMerge = bakeGatedJoinPredicates(toMerge, t.gatedJoinLegTypes(lj))
					}
					return expressions.NewSelectExpressionWithJoinType(
						sel.GetResultValue(),
						sel.GetQuantifiers(),
						append(sel.GetPredicates(), toMerge...),
						sel.GetSourceAliases(),
						sel.GetJoinType(),
					)
				}
				mergedCorr := values.NamedCorrelationIdentifier(sourceAlias(join.Left))
				outerLegs := unnestOuterLegAliases(join.Left, mergedCorr)
				if isChainedUnnest(join.Left, u) {
					// CHAINED unnest (`FROM t, t.a AS x, x.b AS y`): rebase outer-col refs PER
					// conjunct, gated on PUSHABLE-TO-SCAN (correlated-to ⊆ outer base legs). A
					// conjunct referencing an in-chain element (x/y) is NOT scan-pushable — it bakes
					// POSITIONALLY over ordinalLegType(join.Left) (the outer QOV's own type) so an
					// ofOrdinal resolves on the ordinal row at every level CNF + pushdown lands it
					// (a name key would strand ordinal -1, so an ordinalized OR malformed-plans). An
					// outer-col-ONLY conjunct stays lazy (SARG on Scan(t)). A seed with no positional
					// authority → decline to name-model (correct-or-loud). This path is
					// linearity-agnostic: ordinalLegType accumulates every link's columns in spine
					// order regardless of ownership topology, so fork spines rebase identically.
					// The rebase FORM depends on the seed `sel`: an ORDINAL seed (an admitted spine —
					// linear or fork, any depth) takes the POSITIONAL bake (ofOrdinal); a NAME-MODEL
					// seed (a declined shape: box-base, FULL-box-bottom filtered, enclosed →
					// NewAnchoredJoinRecord fallback) takes the NAME-KEY rebase against its qualified
					// merged row — baking positionally onto the name-keyed row would strand
					// (ordinal -1).
					ordinalSeed := false
					if rc, isRC := sel.GetResultValue().(*values.RecordConstructorValue); isRC && !rc.AnchoredJoin {
						ordinalSeed = true
					}
					baked, ok := rebaseChainedOuterLegPredicate(pred, outerLegs, mergedCorr, t.ordinalLegType(join.Left), ordinalSeed)
					if !ok {
						return nil
					}
					pred = baked
				} else {
					pred = rebaseUnnestOuterLegPredicate(pred, outerLegs, mergedCorr)
				}
			}
			toMerge := []predicates.QueryPredicate{pred}
			// RFC-173 Slice 2 W3b: WHERE conjuncts merged into a GATED join's
			// select are join predicates too — bake their direct leg
			// references exactly like the ON predicates at the seed (contract
			// ruling #2: eager (leg, ordinal), one baking rule for every
			// predicate the ordinal select carries).
			if d, gok := t.wedgeGate[join]; gok && d.Gated {
				// legTypes MUST pair aliases with types per GATHERED leg
				// (gatedJoinLegTypes) — pairing sourceAlias(join.Left) with
				// ordinalLegType(join.Left) breaks on a nested-binary cluster:
				// the alias recurses to the buried rightmost table while the
				// type is the whole subtree's concat, baking concat-relative
				// ordinals onto single-table quantifiers.
				toMerge = bakeGatedJoinPredicates(toMerge, t.gatedJoinLegTypes(join))
			}
			merged := append(sel.GetPredicates(), toMerge...)
			return expressions.NewSelectExpressionWithJoinType(
				sel.GetResultValue(),
				sel.GetQuantifiers(),
				merged,
				sel.GetSourceAliases(),
				sel.GetJoinType(),
			)
		}
	}

	// RFC-173 Slice 2: when this filter carries EXISTS subqueries, its select
	// gains existential quantifiers (buildExistentialSelect below) — a
	// name-model parent the outer ForEach leg merges into (post-flattening
	// arity ≥ 3 counting the existential). A join inside the leg (derived
	// table over a join) must therefore gate name-model: mark it enclosed.
	//
	// RFC-173 item-2 commit 4 — the ENCLOSURE LIFT: a gate-eligible OUTER box
	// (a single-source LEFT/RIGHT box that gates as W4-left) is NOT enclosed,
	// so it gates ORDINAL and implementExistentialSelect's below-FOD ordinal
	// rebase fires (the W4-left machinery was dead until now purely because
	// this arm poisoned the gate). Everything else keeps the name-model
	// enclosure — a buried/clustered/FULL box, a non-join input. The decision
	// routes through the ONE gate authority (existsOuterGatesFresh →
	// ordinalWedgeGateDecide, ruling condition 4).
	prevEnclosure := t.inInnerCluster
	if len(f.ExistsSubqueries) > 0 {
		// Lift the enclosure ONLY when not ALREADY enclosed: if this
		// WHERE-EXISTS filter is itself a leg of a larger name-model merge
		// (prevEnclosure true — a transparent join / derived body that will
		// flatten-merge the box), the parent name-models the box regardless, so
		// letting the child gate ordinal here seeds a positional row the merge
		// then mis-binds. existsOuterGatesFresh probes with a FRESH position, so
		// it cannot see the enclosing context on its own — prevEnclosure carries
		// it. When already enclosed, stay enclosed.
		t.inInnerCluster = prevEnclosure || !t.existsOuterGatesFresh(f.Input)
	}
	innerRef := t.translateRef(f.Input)
	t.inInnerCluster = prevEnclosure
	if innerRef == nil {
		return nil
	}

	if len(f.ExistsSubqueries) > 0 {
		// resultOverride nil ⇒ WHERE-EXISTS: the SelectExpression returns the
		// bare outer row (a projection above handles the SELECT list). RFC-141
		// projected-EXISTS folds the projection's RecordConstructor in here as
		// the result value (see translateProjectOverExistsFilter).
		return t.buildExistentialSelect(f, innerRef, nil)
	}

	var preds []predicates.QueryPredicate
	if f.Predicate != nil {
		preds = []predicates.QueryPredicate{f.Predicate}
	}
	return expressions.NewLogicalFilterExpression(
		preds,
		t.namedQuantifier(sourceAlias(f.Input), innerRef),
	)
}

// translateUnnestExistsFilter composes a lateral array UNNEST in the FROM list
// with a WHERE EXISTS (`SELECT v FROM t, t.arr AS v WHERE [v > 100 AND] EXISTS
// (…)`). The unnest stays its OWN FlatMap-over-Explode (it CANNOT be flattened
// into implementJoinWithExistential's binary NLJ — a correlated Explode in a
// plain NLJ materializes its inner once against an unbound context and yields no
// rows). The composition is therefore NESTED:
//
//		FlatMap(outer = <unnest FlatMap, WHERE-on-element folded into the Explode>,
//		        inner = FirstOrDefault(EXISTS subplan) | residual existential filter)
//
//	  - The unnest leg lowers via translateUnnestJoin (the SAME path the non-EXISTS
//	    unnest uses — no duplicated lowering). A WHERE that references the unnest's
//	    AS/AT column is rewritten (rewriteUnnestPredicate) and MERGED into the
//	    unnest SelectExpression — IDENTICAL to translateFilter's non-EXISTS
//	    unnest+WHERE merge — so the NLJ rule pushes it into the inner Explode filter
//	    (Java's `EXPLODE … | FILTER …`). Without the fold the element predicate
//	    would land on the OUTER scan, where the unnest column does not exist
//	    (silently dropping every row).
//	  - The existential semi-join wraps that unnest reference via the shared
//	    buildExistentialSelect. The non-EXISTS predicate is already folded into the
//	    unnest ref, so the existential filter passed down carries ONLY the EXISTS
//	    subqueries + their correlation predicates (Predicate cleared) — never
//	    re-applying the element filter at the wrong (outer) level.
//
// RFC-142 (P2b).
func (t *cascadesTranslator) translateUnnestExistsFilter(
	f *logical.LogicalFilter,
	join *logical.LogicalJoin,
	u *logical.LogicalUnnest,
) expressions.RelationalExpression {
	// Lower the unnest leg (validates the array field; records a faithful
	// diagnostic + returns nil for an invalid unnest, e.g. AT-on-a-non-array).
	// unnestUnderExistential is set so unnestExistsSeedSafe applies the single-alias
	// SCOPE gate: RFC-173 commit 5a ORDINALIZES a single-source unnest under EXISTS
	// (the W4c ordinal seed) and leaves the EXISTS correlation LEG-RELATIVE for the
	// executor's positional rebase. A MULTI-ALIAS outer stays name-model.
	outerAliases := outerBoundAliases(join.Left)
	prevCollision := t.unnestExistsScopeCollision
	t.unnestExistsScopeCollision = existsInnerScopeCollidesOuter(f.ExistsSubqueries, outerAliases)
	prevOuterConj := t.unnestOuterConjunctOnBoxLeg
	t.unnestOuterConjunctOnBoxLeg = nonExistsConjunctRefsOuterLeg(f.Predicate, outerAliases)
	prevUnderExist := t.unnestUnderExistential
	t.unnestUnderExistential = true
	unnestExpr := t.translateUnnestJoin(join, u)
	t.unnestUnderExistential = prevUnderExist
	t.unnestExistsScopeCollision = prevCollision
	t.unnestOuterConjunctOnBoxLeg = prevOuterConj
	if unnestExpr == nil {
		return nil
	}

	// Fold the NON-EXISTS WHERE predicates into the unnest SelectExpression,
	// rewriting unnest-column references to what the inner Explode flows — the
	// IDENTICAL merge translateFilter performs for a non-EXISTS unnest+WHERE. Only
	// the non-EXISTS parts (splitNonExistsPredicates) are merged here; the EXISTS
	// predicate stays out of the unnest select (it references the existential alias,
	// which the unnest select does not bind) and is threaded by the existential
	// select below.
	nonExists := splitNonExistsPredicates(f.Predicate)
	if len(nonExists) > 0 {
		sel, ok := unnestExpr.(*expressions.SelectExpression)
		if !ok {
			return nil
		}
		merged := append([]predicates.QueryPredicate{}, sel.GetPredicates()...)
		// Same multi-source outer-leg rebase translateFilter applies: a
		// non-EXISTS WHERE on an outer-leg column of a ≥2-prior-source unnest
		// (`FROM A, B, A.arr AS X WHERE X = A.c [AND EXISTS …]`) references
		// QOV(A), which the inner Explode does not bind — rebase it to the
		// qualified `A.c` key off the merged outer QOV (sourceAlias(join.Left)).
		// RFC-142.
		mergedCorr := values.NamedCorrelationIdentifier(sourceAlias(join.Left))
		outerLegs := unnestOuterLegAliases(join.Left, mergedCorr)
		for _, p := range nonExists {
			rebased := rebaseUnnestOuterLegPredicate(rewriteUnnestPredicate(p, u), outerLegs, mergedCorr)
			merged = append(merged, rebased)
		}
		unnestExpr = expressions.NewSelectExpressionWithJoinType(
			sel.GetResultValue(),
			sel.GetQuantifiers(),
			merged,
			sel.GetSourceAliases(),
			sel.GetJoinType(),
		)
	}

	innerRef := expressions.InitialOf(unnestExpr)

	// P2a (silent-wrong): an EXISTS subquery whose residual
	// correlation references the ORIGINAL OUTER TABLE (`EXISTS (SELECT 1 FROM U
	// WHERE U.V > T1.ID)` — the residual `T1.ID`) — NOT the unnest element/ordinal.
	// buildCorrelatedExists resolved `T1.ID` against the outer scope's REAL table
	// source T1, so its JoinPredicate carries FieldValue{ID, Child:QOV(T1)}. But the
	// existential's outer here is the UNNEST FlatMap, whose merged output row is
	// bound under sourceAlias(join) (= the unnest's AS/AT alias VAL), NOT under T1.
	// So at execution the residual's QOV(T1) is unbound → `U.V > NULL` is false for
	// every row → ALL rows silently dropped. The unnest FlatMap output anchors the
	// outer leg's columns under BOTH bare (ID) and qualified (T1.ID) keys
	// (buildUnnestResultValue → NewAnchoredJoinRecord), exactly as a non-unnest
	// `WHERE EXISTS` correlates to its FROM source. Rebase every EXISTS subquery's
	// JoinPredicate so a reference to an outer-table leg alias (outerBoundAliases of
	// join.Left, e.g. T1) reads the qualified T1.ID key off the unnest FlatMap's
	// merged binding (QOV(unnestAlias)) — the same outer-leg-to-merged rebase the
	// real-JOIN+EXISTS path performs (rebaseOuterLegRefsToMerged). A residual
	// referencing the unnest ELEMENT (VAL) is bound by the FlatMap already (the
	// P2c path) and is left untouched: it is NOT an outer-table-leg alias.
	// RFC-142.
	mergedCorr := values.NamedCorrelationIdentifier(sourceAlias(join))
	outerLegs := outerBoundAliases(join.Left)
	// RFC-173 commit 5a — the EXISTS correlation's outer-leg refs, routed by whether
	// the seed carries executor WINDOWS, checked DIRECTLY (not proxied through
	// !rc.AnchoredJoin):
	//   - WINDOWED ordinal seed (mixed no-AT → 1 outer window, fully-baked AS+AT →
	//     2 windows; accept-equivalent to the executor's ordinalJoinSpans): leave the
	//     refs LEG-RELATIVE. The executor's below-FOD hoist rebases each inner-residual
	//     outer ref POSITIONALLY — one layout authority, no translator prediction.
	//   - otherwise (an ANCHORED name-model seed — a MULTI-ALIAS outer or an
	//     inner-scope collision; a non-RC/non-Select unnest; or the unreachable
	//     windowless-ordinal seed): rebase to the qualified "LEG.COL" key. Every
	//     REACHABLE non-anchored seed IS windowed, so this equals !rc.AnchoredJoin
	//     today — but the direct window check keeps the routing correct-or-name-model
	//     (never a positional ref over a windowless row) if a windowless ordinal seed
	//     ever becomes reachable, rather than silently mis-routing it leg-relative.
	seedWindowed := false
	var ordMergedType *values.RecordType
	if sel, ok := unnestExpr.(*expressions.SelectExpression); ok {
		if rc, isRC := sel.GetResultValue().(*values.RecordConstructorValue); isRC && !rc.AnchoredJoin {
			if w, mt := values.OrdinalSeedLegWindows(rc); w != nil {
				seedWindowed = true
				ordMergedType = mt
			}
		}
	}
	existsSubqueries := f.ExistsSubqueries
	if len(outerLegs) > 0 && mergedCorr.Name() != "" {
		existsSubqueries = make([]logical.ExistsSubquery, len(f.ExistsSubqueries))
		for i, esq := range f.ExistsSubqueries {
			// A MULTI-TABLE EXISTS inner whose correlation references the unnest
			// ELEMENT (or ordinal) has no working evaluation path: the conjunct
			// must evaluate below the FirstOrDefault against the inner join's
			// rows with the element bound from the OUTER row, and the
			// multi-table threading for that binding does not exist yet — the
			// predicate silently evaluated NULL and dropped every inner row
			// (EXISTS ≡ false for all outers). Decline LOUDLY (CORRECT-or-LOUD);
			// the element-scoped multi-table threading is tracked follow-on work.
			if len(outerBoundAliases(esq.Plan)) > 1 && esq.JoinPredicate != nil {
				corrs := predicates.GetCorrelatedToOfPredicate(esq.JoinPredicate)
				for c := range corrs {
					name := strings.ToUpper(c.Name())
					if (u.Alias != "" && name == strings.ToUpper(u.Alias)) ||
						(u.AtAlias != "" && name == strings.ToUpper(u.AtAlias)) {
						t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
							"EXISTS with a multi-table FROM referencing the unnest element is not supported"))
						return nil
					}
				}
			}
			// A BURIED subquery-internal OUTER-ONLY filter (buildCorrelatedExists
			// keeps an outer-only conjunct INSIDE the subquery so it evaluates
			// under the ∃ in both polarities — the NOT-EXISTS pre-filter fix).
			// Below the FOD an outer-LEG ref has no direct binding here (the
			// unnest FlatMap binds its merged output, never the leg alias), and
			// the executor's positional hoist covers RULE-level predicates only —
			// a buried leg ref would fall to the alias-unchecked frontier
			// fallback and silently read the INNER row. Make it bindable IN
			// PLACE, keeping the under-∃ placement:
			//   - WINDOWED ordinal seed: BAKE the leg ref to an ofOrdinal over
			//     the typed merged QOV. For the single-alias outer (the only
			//     reachable shape today) the leg IS the pristine prefix at offset
			//     0, so the leg-type ordinal IS the merged ordinal; for a
			//     multi-alias outer (a box, when the guard lifts) the slot is
			//     resolved WITHIN the qualifier's rt.Legs window
			//     (rebaseUnnestOuterLegPredicateOrdinal → ordinalSlotInLegWindow),
			//     so a dup-named column bakes the right alias's slot. A baked
			//     FrontierPinned outer ref inside an existential inner plan is
			//     exactly the shape the disabled-birth probe binds positionally.
			//     The translator is the SINGLE rebase authority for buried refs
			//     (the hoist never sees them), so this cannot double-rebase.
			//   - ANCHORED seed: the qualified "LEG.COL" read off the merged
			//     binding — the same rebase the JoinPredicate channel gets.
			if lf, isLF := esq.Plan.(*logical.LogicalFilter); isLF &&
				len(lf.ExistsSubqueries) == 0 && len(lf.ScalarSubqueries) == 0 &&
				predicateIsOuterOnly(lf.Predicate, outerBoundAliases(lf.Input)) {
				var rebased predicates.QueryPredicate
				if seedWindowed {
					baked, ok := rebaseUnnestOuterLegPredicateOrdinal(lf.Predicate, t.ordinalLegType(join.Left), ordMergedType, outerLegs, mergedCorr)
					if !ok {
						// CORRECT-or-LOUD: an outer ref the seed's outer leg type
						// cannot map is never a valid correlation — decline the
						// whole composition rather than ship a half-baked tree.
						return nil
					}
					rebased = baked
				} else {
					rebased = rebaseUnnestOuterLegPredicate(lf.Predicate, outerLegs, mergedCorr)
				}
				esq.Plan = &logical.LogicalFilter{Input: lf.Input, Predicate: rebased, PredicateText: lf.PredicateText}
			}
			// The JoinPredicate channel: name-model rebase for an anchored seed;
			// leg-relative for a windowed ordinal seed (the executor's below-FOD
			// hoist rebases it positionally — RULE-level, unlike the buried refs).
			if !seedWindowed {
				esq.JoinPredicate = rebaseUnnestOuterLegPredicate(esq.JoinPredicate, outerLegs, mergedCorr)
			}
			existsSubqueries[i] = esq
		}
	}

	// The unnest ref now owns the WHERE-on-element; the existential select must
	// thread ONLY the EXISTS predicate(s) — the ExistentialValuePredicate /
	// NOT(ExistentialValuePredicate) marker that drives the residual semi-join
	// filter (QOV IS NOT NULL / IS NULL) in the NLJ rule. The NON-EXISTS parts are
	// already folded into the unnest ref above; re-applying them here (above the
	// FlatMap) would push the element filter onto the outer scan where the unnest
	// column does not exist. Carry ONLY the existential markers in the synthesized
	// filter's Predicate. The existential's outer correlation is sourceAlias(join)
	// = the unnest's AS/AT alias, which is what the unnest SelectExpression flows up.
	existsPreds := extractExistsPredicates(f.Predicate)
	existsOnly := &logical.LogicalFilter{
		Input:            join,
		Predicate:        andOf(existsPreds),
		ExistsSubqueries: existsSubqueries,
		ScalarSubqueries: f.ScalarSubqueries,
	}
	return t.buildExistentialSelect(existsOnly, innerRef, nil)
}

// rebaseUnnestOuterLegPredicate rewrites references to an outer-table leg alias
// (outerLegs — the FROM-source aliases bound under the unnest's outer FlatMap,
// e.g. T1) so they resolve against the unnest FlatMap's MERGED output row bound
// under mergedCorr (the unnest's AS/AT alias). A leg reference
// `FieldValue{Field:"ID", Child:QOV("T1")}` becomes
// `FieldValue{Field:"T1.ID", Child:QOV(mergedCorr)}` — the qualified "LEG.COL"
// key the unnest FlatMap output carries (NewAnchoredJoinRecord). This is the
// query-package twin of the cascades NLJ rule's rebaseOuterLegRefsToMerged (the
// real-JOIN+EXISTS path); both turn an outer-leg-qualified residual into a read
// off the existential outer's merged binding. References to the unnest element
// (the merged corr itself) or to the existential inner pass through untouched.
// RFC-142.
func rebaseUnnestOuterLegPredicate(
	p predicates.QueryPredicate,
	outerLegs map[string]struct{},
	mergedCorr values.CorrelationIdentifier,
) predicates.QueryPredicate {
	if p == nil || len(outerLegs) == 0 {
		return p
	}
	mergedQOV := values.NewQuantifiedObjectValue(mergedCorr)
	rewrite := func(v values.Value) values.Value {
		if v == nil {
			return v
		}
		return values.Replace(v, func(node values.Value) values.Value {
			fv, ok := node.(*values.FieldValue)
			if !ok || fv.Child == nil {
				return node
			}
			qov, ok := fv.Child.(*values.QuantifiedObjectValue)
			if !ok {
				return node
			}
			leg := strings.ToUpper(qov.Correlation.Name())
			if _, isOuterLeg := outerLegs[leg]; !isOuterLeg {
				return node
			}
			// Read the qualified "LEG.COL" key off the merged unnest output. The
			// field already carries a bare column name here (resolved against the
			// outer table source), so prefix it with the leg alias.
			return values.NewFieldValue(mergedQOV, leg+"."+strings.ToUpper(fv.Field), fv.Typ)
		})
	}
	return mapPredicateValues(p, rewrite)
}

// rebaseChainedOuterLegPredicate rebases outer-leg references PER CONJUNCT for a CHAINED
// unnest (`FROM t, t.a AS x, x.b AS y`). The discriminator is PUSHABLE-TO-SCAN, expressed
// structurally as "correlated-to ⊆ outerLegs": a conjunct whose every referenced correlation
// is an outer base leg (outerLegs, e.g. {t}) is pushed by PushFilterBelowJoinRule down toward
// Scan(t) where t is directly correlated — leave its refs lazy (QOV(t)) so they resolve as a
// SARG. A conjunct referencing ANY correlation bound INSIDE the chained structure (the
// first-link element x, the second-link element y) is NOT scan-pushable: it stays at the
// FlatMap/Explode level whose row is the ordinal merged row, so bake its outer-leg refs
// POSITIONALLY over ordType — ordinalLegType(j.Left), the SAME type the seed's outer leg run
// uses. Positional, NOT a name key: after this predicate is merged into the ordinal select,
// NormalizePredicatesRule distributes an OR to CNF and PredicatePushDownRule pushes each clause
// to whichever level its quantifiers allow. A name accessor resolves on the merged row ONLY at
// the inner Explode but STRANDS (ordinal -1) on the first-link ordinal FlatMap where a pushed
// PURE-OUTER CNF clause (`t.id=10 OR t.id=3` from `(t.id=10 AND y>2) OR (t.id=3 AND y<5)`)
// lands; an ofOrdinal over ordType (= the outer QOV's own type, so the baked QOV MATCHES the
// seed's — no drift tripwire) resolves on the ordinal row at BOTH levels. Splits the top-level
// AND (bakeConjuncts discipline) so a straddling `t.id = y` bakes positionally while a sibling
// `t.id = 1` (outer-only) stays lazy. Returns ok=false (→ caller declines to name-model,
// correct-or-loud) when a non-pushable conjunct cannot be positionally baked (no windowed type).
//
// SEED-FORM ENFORCED by the ordinalSeed arg (NOT the chain depth): the POSITIONAL bake fires
// ONLY over an ORDINAL seed; a NAME-MODEL seed takes the NAME-KEY rebase (its merged row carries
// qualified `leg.col` keys). The caller sets ordinalSeed from `sel`'s own RC (`!AnchoredJoin`) —
// so ANY chained unnest that declined to name-model (a 3+-link chain via clusterArity poison, a
// 2-chain buried behind a trailing table, or a `!ok` positional decline) correctly keeps the
// name-key rebase, exactly as before this slice. The positional bake is validated only for the
// ordinalizing 2-CHAIN; a 3+-link chain's ordinalization + its mixed-inner-ref placement (the
// strand living in pushBuriedUnnestPredicateDown/rewriteUnnestPredicate) is the deeper-nesting
// slice.
func rebaseChainedOuterLegPredicate(
	p predicates.QueryPredicate,
	outerLegs map[string]struct{},
	mergedCorr values.CorrelationIdentifier,
	ordType *values.RecordType,
	ordinalSeed bool,
) (predicates.QueryPredicate, bool) {
	if p == nil || len(outerLegs) == 0 {
		return p, true
	}
	if and, isAnd := p.(*predicates.AndPredicate); isAnd {
		newSubs := make([]predicates.QueryPredicate, len(and.SubPredicates))
		changed := false
		for i, s := range and.SubPredicates {
			sub, ok := rebaseChainedOuterLegPredicate(s, outerLegs, mergedCorr, ordType, ordinalSeed)
			if !ok {
				return p, false
			}
			newSubs[i] = sub
			if newSubs[i] != s {
				changed = true
			}
		}
		if !changed {
			return p, true
		}
		return predicates.NewAnd(newSubs...), true
	}
	if chainedPredScanPushable(p, outerLegs) {
		return p, true // outer-base-leg refs only → pushed to Scan(t); leave lazy (SARG)
	}
	// A non-pushable conjunct (references an in-chain element): rebase its outer-leg refs onto
	// the merged row. Over an ORDINAL seed, bake POSITIONALLY over ordType so an ofOrdinal
	// resolves on the ordinal row at every level CNF + pushdown lands the clause (a name key
	// would strand ordinal -1). Over a NAME-MODEL seed (a declined 3+-link fallback), the merged
	// row carries qualified `leg.col` keys — use the NAME-KEY rebase (a positional bake against
	// the name-keyed row would strand DEEP ordinal -1).
	if ordinalSeed {
		return rebaseUnnestOuterLegPredicateOrdinal(p, ordType, ordType, outerLegs, mergedCorr)
	}
	return rebaseUnnestOuterLegPredicate(p, outerLegs, mergedCorr), true
}

// chainedPredScanPushable reports whether every correlation the conjunct references is an
// outer base leg (outerLegs) — i.e. PushFilterBelowJoinRule can push it all the way to
// Scan(t). A conjunct that also references a correlation bound inside the chained structure
// (the first-link element x, the second-link element y) is NOT scan-pushable. A constant
// predicate (empty correlated-to) is vacuously pushable — it has no outer-leg refs to rebase,
// so gate/keep are both no-ops.
func chainedPredScanPushable(p predicates.QueryPredicate, outerLegs map[string]struct{}) bool {
	for corr := range predicates.GetCorrelatedToOfPredicate(p) {
		if _, ok := outerLegs[strings.ToUpper(corr.Name())]; !ok {
			return false
		}
	}
	return true
}

// ordinalSlotInLegWindow resolves a QUALIFIED outer-leg column (`leg`.`field`) to
// its ABSOLUTE slot in the merged prefix. When the merged type carries per-leg
// boundaries (rt.Legs — populated by buriedLegBounds for a MULTI-ALIAS box outer),
// it searches ONLY within the named leg's [Start, Start+Width) window, so a
// dup-named column (two aliases each with "ID") resolves to the CORRECT alias's
// slot. Anything not found within the qualifier's window — a column absent from it,
// OR a qualifier absent from the leg list entirely — declines LOUDLY (0,false),
// NEVER a flat first-match (which would silently read another alias's same-named
// column). Consumes the same rt.Legs metadata OrdinalSeedLegWindows emits for
// the serve side — one layout authority, so translator-rebase and executor
// windows agree. NOTE: the multi-alias branch is wired but scope-gated OFF
// end-to-end (unnestExistsSeedSafe keeps multi-alias outers name-model); it
// goes live only when that guard lifts (channel 2).
//
// multiAlias is the CALLER's structural fact (len(outerLegs) > 1). The flat
// FieldIndex fallback is legitimate ONLY for a single-alias prefix (the
// pristine-prefix-at-offset-0 case, where every column belongs to the one
// alias). A MULTI-alias prefix that arrives WITHOUT leg windows must DECLINE:
// the flat fallback would silently first-match a dup-named column across
// aliases — a qualified B.ID resolving to A's slot 0, observed as
// `WHERE B.ID = 20` admitting {A.ID:20, B.ID:null} rows. Correct-or-loud at
// the resolution site: the decline keeps the whole class fail-closed even if
// a window-propagation gap upstream ever leaves Legs empty.
func ordinalSlotInLegWindow(rt *values.RecordType, leg, field string, multiAlias bool) (int, bool) {
	if rt == nil {
		return 0, false
	}
	if len(rt.Legs) > 0 {
		for _, lw := range rt.Legs {
			// Legs.Name is contractually UPPER (buriedLegBounds); field names are
			// UPPER on both sides (ordinalLegType stores + caller passes ToUpper),
			// so an exact == mirrors the flat FieldIndex fallback exactly.
			if lw.Name != leg {
				continue
			}
			if lw.Start < 0 {
				return 0, false // malformed window — never index at a negative slot
			}
			for i := lw.Start; i < lw.Start+lw.Width && i < len(rt.Fields); i++ {
				if rt.Fields[i].Name == field {
					return i, true
				}
			}
			return 0, false // column NOT in this leg's window — loud decline
		}
		return 0, false // qualifier NOT among the leg windows — loud decline (never flat)
	}
	if multiAlias {
		return 0, false // multi-alias prefix WITHOUT windows — loud decline (never flat)
	}
	return rt.FieldIndex(field)
}

// rebaseUnnestOuterLegPredicateOrdinal bakes outer-table-leg references in a
// BURIED subquery predicate to FrontierPinned ofOrdinals over the typed merged
// QOV — the ordinal-seed twin of rebaseUnnestOuterLegPredicate, for the ONE
// channel the executor's positional hoist cannot reach: a predicate INSIDE an
// existential inner plan (the under-∃ placement of a subquery-internal
// outer-only conjunct). For a single-alias outer the leg is the pristine merged
// PREFIX at offset 0, so an outer column's ordinal in the outer leg type IS its
// merged-row ordinal — the ONLY shape reachable today. The multi-alias branch (a
// box with rt.Legs, ordinal resolved WITHIN the qualifier's leg window via
// ordinalSlotInLegWindow so a dup-named column bakes the right alias's slot) is
// WIRED but scope-gated OFF end-to-end (unnestExistsSeedSafe keeps multi-alias
// outers name-model); it goes live only when that guard lifts (channel 2, coupled
// with the RULE-level below-FOD executor hoist). The baked ref is then exactly the
// shape the disabled-birth probe binds positionally below the FOD. The
// translator is the SINGLE rebase authority for buried refs (RULE-level
// predicates stay the executor hoist's), so no double-rebase exists. Returns
// ok=false (caller declines, CORRECT-or-LOUD) for an outer ref the leg type
// cannot map or a missing type authority — never a half-baked tree.
func rebaseUnnestOuterLegPredicateOrdinal(
	p predicates.QueryPredicate,
	outerLegType, mergedType *values.RecordType,
	outerLegs map[string]struct{},
	mergedCorr values.CorrelationIdentifier,
) (predicates.QueryPredicate, bool) {
	if p == nil || len(outerLegs) == 0 {
		return p, true // genuinely nothing to rebase
	}
	if outerLegType == nil || mergedType == nil {
		return p, false // no positional authority to bake against — fail closed
	}
	mergedQOV := values.NewQuantifiedObjectValueOfType(mergedCorr, mergedType)
	ok := true
	rewrite := func(v values.Value) values.Value {
		if v == nil {
			return v
		}
		return values.Replace(v, func(node values.Value) values.Value {
			fv, isFV := node.(*values.FieldValue)
			if !isFV || fv.Child == nil {
				return node
			}
			qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
			if !isQOV {
				return node
			}
			leg := strings.ToUpper(qov.Correlation.Name())
			if _, isOuterLeg := outerLegs[leg]; !isOuterLeg {
				return node
			}
			// Resolve the slot WITHIN the qualifier's per-leg window when the outer
			// carries leg boundaries (rt.Legs — a MULTI-ALIAS outer like a FULL OUTER
			// box): a qualified ref to a dup-named column must pick THAT leg's slot,
			// not the flat first-match across the whole merged prefix (which silently
			// reads the OTHER alias's same-named column — the RFC-173 S4 c5a hazard).
			// A single-alias outer has no rt.Legs and falls back to a flat FieldIndex.
			ord, found := ordinalSlotInLegWindow(outerLegType, leg, strings.ToUpper(fv.Field), len(outerLegs) > 1)
			if !found {
				ok = false
				return node
			}
			baked, err := values.NewFieldValueOfOrdinal(mergedQOV, ord)
			if err != nil {
				ok = false
				return node
			}
			return baked
		})
	}
	np := mapPredicateValues(p, rewrite)
	return np, ok
}

// predicateIsOuterOnly reports whether a predicate references at least one
// correlation and NONE of them is in innerAliases — the discriminator for a
// subquery-internal OUTER-ONLY filter (buildCorrelatedExists places one; an
// UNCORRELATED subquery's own filter references its inner sources and must
// never match). Nil/reference-free predicates are not outer-only.
func predicateIsOuterOnly(p predicates.QueryPredicate, innerAliases map[string]struct{}) bool {
	if p == nil {
		return false
	}
	corrs := predicates.GetCorrelatedToOfPredicate(p)
	if len(corrs) == 0 {
		return false
	}
	for c := range corrs {
		if _, isInner := innerAliases[strings.ToUpper(c.Name())]; isInner {
			return false
		}
	}
	return true
}

// andOf combines predicates into a single QueryPredicate: nil for an empty list,
// the lone predicate for one, an AndPredicate for several. Used to rebuild a
// filter's predicate from a filtered subset (e.g. only the EXISTS markers).
func andOf(preds []predicates.QueryPredicate) predicates.QueryPredicate {
	switch len(preds) {
	case 0:
		return nil
	case 1:
		return preds[0]
	default:
		return predicates.NewAnd(preds...)
	}
}

// buildExistentialSelect builds the SelectExpression for a LogicalFilter that
// carries existential subqueries (RFC-141). It attaches a ForEach(outer) plus
// one Existential quantifier per subquery, threading the WHERE predicates (the
// ExistentialValuePredicate routes to the residual semi-join filter in the NLJ
// rule) and each subquery's correlation predicate. The resultValue is:
//
//   - resultOverride when non-nil — a PROJECTED EXISTS folds its projection's
//     RecordConstructor in here so the existential boolean is evaluated by the
//     FlatMap's result value with the inner binding live (matching Java's
//     "FLATMAP q0 -> { ... DEFAULT NULL AS q1 RETURN (q0.ID, exists(q1)) }");
//   - the bare outer QuantifiedObjectValue otherwise (WHERE-EXISTS — a separate
//     projection above handles the SELECT list).
func (t *cascadesTranslator) buildExistentialSelect(
	f *logical.LogicalFilter,
	innerRef *expressions.Reference,
	resultOverride values.Value,
) expressions.RelationalExpression {
	// Projected EXISTS + JOIN in FROM (no WHERE): the existential filter's input
	// is a LogicalJoin. Flatten the join's two ForEach quantifiers and the
	// existential quantifier(s) into ONE SelectExpression with the projection as
	// the result value — the same 2+1 flatten translateJoinWithExists does for
	// WHERE-EXISTS, but emitting the folded projection. Nesting the join
	// SelectExpression inside the existential one (the non-join path's single
	// outer quantifier over translateJoin(join)) would put the projected
	// ExistsValue above the join's own select; the flatten keeps the projection —
	// and its ExistsValue — in the same SelectExpression that owns the existential
	// quantifier, so the §8 guard passes and the boolean is computed with the
	// inner binding live (Java's single SelectExpression: all FROM quantifiers +
	// the existential + the projection).
	if join, ok := f.Input.(*logical.LogicalJoin); ok && resultOverride != nil {
		// RFC-173 Outcome-B B1 (U-1): a gated arity>=3 non-dup INNER cluster
		// ordinalizes via the existential wrap over its own gathered cluster —
		// the join stays a separately-enumerated (SARG-preserving) expression
		// and the projection + EXISTS correlations rebase onto its positional
		// output. Fail-open: nil falls through below.
		if sel := t.translateExistsOverGatheredCluster(join, f, resultOverride); sel != nil {
			return sel
		}
		if !projectionReferencesExistsSubquery([]values.Value{resultOverride}) {
			// The WIDENED fold (a plain WHERE-EXISTS with no projected EXISTS)
			// exists ONLY for the B1 arm; when it declines, bail the fold so the
			// ordinary (un-folded) path keeps today's name-model plan shape —
			// folding it through the name-model flatten would CHANGE plan shapes
			// for shapes B1 doesn't serve.
			return nil
		}
		return t.buildExistentialJoinSelect(join, f, resultOverride)
	}

	outerAlias := sourceAlias(f.Input)
	outerQ := t.namedQuantifier(outerAlias, innerRef)
	quantifiers := []expressions.Quantifier{outerQ}

	if t.declineNegatedOuterOnlyEsqValue(resultOverride, f.ExistsSubqueries) {
		return nil
	}
	if t.declineNegatedOuterOnlyEsq(f.Predicate, f.ExistsSubqueries) {
		return nil
	}
	allPreds := splitNonExistsPredicates(f.Predicate)
	allPreds = append(allPreds, extractExistsPredicates(f.Predicate)...)
	var innerCorrNames []string
	for _, esq := range f.ExistsSubqueries {
		subRef := t.translateSubqueryRef(esq.Plan)
		if subRef == nil {
			return nil
		}
		existQ := expressions.NamedExistentialQuantifier(esq.Alias, subRef)
		quantifiers = append(quantifiers, existQ)
		// Register the existential inner under its UNIQUE alias (esq.Alias) and
		// rebase the join predicate onto it, so the inner correlation can never
		// collide with the outer source alias (the alias-shadow regression).
		innerCorrName, joinPred := existsInnerCorrelation(esq)
		innerCorrNames = append(innerCorrNames, innerCorrName)
		if joinPred != nil {
			allPreds = append(allPreds, joinPred)
		}
	}

	var sourceAliases []string
	if outerAlias != "" {
		sourceAliases = []string{outerAlias}
		sourceAliases = append(sourceAliases, innerCorrNames...)
	}

	resultValue := resultOverride
	if resultValue == nil {
		resultValue = values.NewQuantifiedObjectValue(outerQ.GetAlias())
	}
	return expressions.NewSelectExpressionWithAliases(
		resultValue,
		quantifiers,
		allPreds,
		sourceAliases,
	)
}

// isScanFamilyLeg reports whether a logical leg is a single scan source
// (optionally under filters) — the logical proxy for the executor's
// legIsOrdinalSafe, which unwraps Filter/Fetch/FetchOnDemand to a Scan/Index
// base. The RFC-173 S4 F2-LEFT projected-EXISTS fold ordinalizes ONLY when both
// legs are scan-family; a join/unnest/union/aggregate leg (a buried box) is not
// and must decline rather than fall to the name-model :698 path. Conservative:
// anything not recognised as scan-under-filters returns false (decline).
func isScanFamilyLeg(op logical.LogicalOperator) bool {
	for {
		switch n := op.(type) {
		case *logical.LogicalScan:
			return true
		case *logical.LogicalFilter:
			op = n.Input
		default:
			return false
		}
	}
}

// existsLegBirthsPositional reports whether a projected-EXISTS fold leg is a
// bare all-INNER cluster of scan-family leaves. Such a leg is translated
// UN-ENCLOSED (AXIS 1) so its INNER box is born as a mergeable ordinal select;
// SelectMergeRule then FLATTENS it into the fold, making the fold an N-way
// `[ForEach×N, Existential]` select the generalized implementJoinWithExistential
// plans (RFC-173 S4 :2908/:3033, N-way flat existential). A scan leg is already
// positional regardless of enclosure (returns false — no box to un-enclose); an
// OUTER box or a wrapped join returns false (non-mergeable / out of INNER-first
// scope).
func existsLegBirthsPositional(op logical.LogicalOperator) bool {
	j, isJoin := op.(*logical.LogicalJoin)
	if !isJoin || j.Kind != logical.JoinInner {
		return false
	}
	return existsLegAllInnerScanFamily(j.Left) && existsLegAllInnerScanFamily(j.Right)
}

// existsLegAllInnerScanFamily is the recursion arm of existsLegBirthsPositional:
// a scan, or a bare INNER join recursively of such.
func existsLegAllInnerScanFamily(op logical.LogicalOperator) bool {
	switch o := op.(type) {
	case *logical.LogicalScan:
		return true
	case *logical.LogicalJoin:
		return o.Kind == logical.JoinInner &&
			existsLegAllInnerScanFamily(o.Left) && existsLegAllInnerScanFamily(o.Right)
	default:
		return false
	}
}

// buildExistentialJoinSelect folds a projection (resultValue) over a
// JOIN-in-FROM that carries projected-EXISTS subqueries into a single
// SelectExpression: ForEach(left), ForEach(right), and one Existential
// quantifier per subquery, with the join ON predicate and each subquery's
// correlation predicate threaded. Mirrors translateJoinWithExists but emits the
// folded projection as the result value instead of the join's anchored record,
// so the projected ExistsValue is evaluated by the FlatMap with the inner
// binding live (RFC-141 §8). INNER and LEFT-with-scan-legs reach here for the
// projected fold (the LEFT case emits a JoinLeftOuter select — RFC-173 S4
// F2-LEFT); a LEFT with a non-scan (buried-box) leg, and RIGHT/FULL, are left
// unfolded and the §8 guard rejects them cleanly.

func (t *cascadesTranslator) buildExistentialJoinSelect(
	j *logical.LogicalJoin,
	f *logical.LogicalFilter,
	resultValue values.Value,
) expressions.RelationalExpression {
	if t.declineNegatedOuterOnlyEsq(f.Predicate, f.ExistsSubqueries) {
		return nil
	}
	if t.declineNegatedOuterOnlyEsqValue(resultValue, f.ExistsSubqueries) {
		return nil
	}
	if j.Kind == logical.JoinFull || j.Kind == logical.JoinRight {
		// FULL: the existential semi-join cannot carry the FULL drain (never
		// rewritten, never merged). RIGHT: the fold's JoinType has no
		// JoinRightOuter — RIGHT needs the operand swap translateJoin does, a
		// booked follow-on. Both return nil → the §8 guard rejects the unfolded
		// projected EXISTS cleanly. LEFT DOES fold (RFC-173 S4 F2-LEFT): the box
		// dissolves to INNER + null-on-empty and the executor births the positional
		// seed with the null-supplying leg NULL-filled — Java folds and answers it
		// (live-verified 4.12.11.0).
		return nil
	}
	if j.Kind == logical.JoinLeft && (!isScanFamilyLeg(j.Left) || !isScanFamilyLeg(j.Right)) {
		// RFC-173 S4 F2-LEFT is SCAN-leg scope only. A buried box
		// `(a JOIN b) LEFT JOIN c` — any non-scan preserved/null-supplying leg —
		// does not ordinalize (the executor's legIsOrdinalSafe rejects the join
		// leg → gatedSeedStep1 false), so folding it would fall through to the
		// name-model :698 path with a null-extended name-keyed row: correct today
		// via the coexistence Datum but a producer the demolition removes. Decline
		// (→ §8 → clean 0AF00) as a reach gap (Java answers it; Go rejects) rather
		// than mint a name-model producer. INNER keeps its buried-box behavior
		// (task-scope :698, no null-extension); the asymmetry is deliberate — the
		// LEFT null-extension is exactly what the ordinal seed must carry.
		return nil
	}
	if j.Kind == logical.JoinLeft && f != nil && len(splitNonExistsPredicates(f.Predicate)) > 0 {
		// RFC-173 S4 F2-LEFT is the NO-WHERE shape. A non-EXISTS WHERE predicate over
		// a LEFT fold would land in the JoinLeftOuter select's predicate list where the
		// NLJ treats it as an ON condition — null-extending non-matching rows instead
		// of FILTERING them: `... LEFT JOIN q ON q.qid = p.id WHERE p.v = 10` keeps
		// p.v = 20 null-extended (wrong rows; Java correctly returns only p.v = 10).
		// Java places the WHERE ABOVE the outer join; the fold cannot express that in a
		// single select yet, so decline (→ §8 → clean 0AF00) rather than yield wrong
		// rows. Booked follow-on: above-join WHERE placement for the LEFT fold. INNER
		// is unaffected — ON ≡ WHERE for an inner join, so the predicate filters.
		return nil
	}
	// RFC-173 Slice 2: same enclosure as translateJoinWithExists — the
	// existential flatten is a name-model parent; its ForEach legs are enclosed.
	// RFC-173 S4 :2908/:3033, AXIS 1: a bare INNER gated-box fold leg is translated
	// UN-ENCLOSED so its box is born as a mergeable ordinal select; SelectMergeRule
	// flattens it into the fold, making the fold an N-way [ForEach×N, Existential]
	// select the generalized implementJoinWithExistential plans. Coupled to the
	// executor N-leg dispatch/seed (two-site certificate). A non-bare-INNER-box leg
	// (scan, OUTER box, wrapped) stays enclosed.
	prevEnclosure := t.inInnerCluster
	t.inInnerCluster = !existsLegBirthsPositional(j.Left)
	leftRef := t.translateRef(j.Left)
	if leftRef == nil {
		t.inInnerCluster = prevEnclosure
		return nil
	}
	t.inInnerCluster = !existsLegBirthsPositional(j.Right)
	rightRef := t.translateRef(j.Right)
	t.inInnerCluster = prevEnclosure
	if rightRef == nil {
		return nil
	}
	// RFC-173 QP-REF-BIND item 1: the fold's quantifiers and source aliases
	// carry the BINDING correlation (== the alias for every non-duplicate
	// leg; the parser-minted id for a later duplicate). The resolver emits
	// binding-qualified references for a duplicate leg (QOV(Q$DUPn).COL), and
	// every downstream consumer — the implementation arm's rebase, the NLJ
	// merged row's qualified LEG.COL keys, the hidden ORDER BY columns — keys
	// legs by these names: display-named [A, A] quantifiers left the second
	// leg's binding unbound and its columns served NULL. The name-model
	// merged row distinguishes duplicate legs exactly when the leg keys are
	// the distinct bindings.
	leftAlias := sourceBinding(j.Left)
	rightAlias := sourceBinding(j.Right)

	leftQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(leftAlias), leftRef,
	)
	rightQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(rightAlias), rightRef,
	)
	quantifiers := []expressions.Quantifier{leftQ, rightQ}

	var allPreds []predicates.QueryPredicate
	if j.OnPredicate != nil {
		if qp, ok := j.OnPredicate.(predicates.QueryPredicate); ok {
			allPreds = append(allPreds, qp)
		}
	}
	// A projected EXISTS with no WHERE carries no filter predicate, but defend
	// against a residual non-EXISTS predicate riding on the synthesized filter.
	allPreds = append(allPreds, splitNonExistsPredicates(f.Predicate)...)
	allPreds = append(allPreds, extractExistsPredicates(f.Predicate)...)

	sourceAliases := []string{leftAlias, rightAlias}
	for _, esq := range f.ExistsSubqueries {
		subRef := t.translateSubqueryRef(esq.Plan)
		if subRef == nil {
			return nil
		}
		existQ := expressions.NamedExistentialQuantifier(esq.Alias, subRef)
		quantifiers = append(quantifiers, existQ)
		innerCorrName, joinPred := existsInnerCorrelation(esq)
		if joinPred != nil {
			allPreds = append(allPreds, joinPred)
		}
		sourceAliases = append(sourceAliases, innerCorrName)
	}

	// RFC-173 S4 F2-LEFT: a LEFT-outer FROM join folds as a JoinLeftOuter select
	// — RewriteOuterJoinRule dissolves the p×q box into INNER + a null-on-empty
	// q quantifier, and the executor births the positional seed with q NULL-filled
	// on non-matching outer rows (the projected EXISTS then reads that NULL).
	foldJoinType := expressions.JoinInner
	if j.Kind == logical.JoinLeft {
		foldJoinType = expressions.JoinLeftOuter
	}
	return expressions.NewSelectExpressionWithJoinType(
		resultValue,
		quantifiers,
		allPreds,
		sourceAliases,
		foldJoinType,
	)
}

// translateProjectOverExistsFilter folds a projection that references a
// projected EXISTS into the existential SelectExpression's result value
// (RFC-141 Phase 2). It builds a RecordConstructorValue from the projection's
// values + output aliases and passes it as the SelectExpression result value,
// so the FlatMap's result computes the projected columns — including the
// existential boolean (ExistsValue.eval reads the inner binding the FlatMap
// establishes). Returns nil to fall back to the ordinary projection path when
// any projected Value is unresolved (the walker couldn't build it).
//
// chain holds the intervening unary operators (ORDER BY / LIMIT) that sat
// between the project and the existential filter, ordered top-to-bottom (the
// element closest to the project first). They are re-applied ON TOP of the
// folded SelectExpression so ORDER BY / LIMIT semantics are preserved — the
// sort/limit operates over the projected output rows (including the computed
// existential boolean), matching Java's
// `generateSort(generateSimpleSelect(output...), orderBys)`.
func (t *cascadesTranslator) translateProjectOverExistsFilter(
	p *logical.LogicalProject,
	f *logical.LogicalFilter,
	chain []logical.LogicalOperator,
) expressions.RelationalExpression {
	// Collect the FILTER's (uncorrelated) scalar subqueries. The fold's early
	// return in translateProject bypasses translateFilter — which is where
	// f.ScalarSubqueries would otherwise be registered — so a scalar subquery in
	// the WHERE of a projected-EXISTS query (`SELECT id, EXISTS(...) FROM t1 WHERE
	// price > (SELECT MAX(x) FROM t2)`) would never be pre-evaluated, leaving its
	// value unbound (NULL) and the comparison silently dropped (RFC-141 R4,
	// the projected variant). Register them here, exactly as
	// translateFilter does, so the executor pre-evaluates and binds them. A
	// CORRELATED scalar subquery in the WHERE is not collected here (Java's
	// grammar cannot place one there either); only the uncorrelated list is
	// pre-evaluated.
	for _, ssq := range f.ScalarSubqueries {
		t.scalarSubqueries = append(t.scalarSubqueries, ScalarSubqueryPlan{
			Alias: ssq.Alias,
			Plan:  ssq.Plan,
		})
	}

	// RFC-173 S4: this projected-EXISTS fold does NOT force enclosure on f.Input.
	// The former `t.inInnerCluster = true` here was a name-model PRODUCER — one of
	// the enclosure setters the endgame retires. It was never load-bearing: f.Input
	// is scan / opaque-box (translateOp clears it) / LogicalCTE, and the only f.Input
	// that can carry a name-model construct is a CTE body over a join/unnest — which
	// projected-EXISTS-over-CTE declines (0AF00) unconditionally, so the enclosure's
	// effect on the body was always discarded. Worse, where it WOULD become
	// observable it flipped the WRONG way: for an enclosed multi-source unnest body
	// (`FROM A, A.arr AS x, B WHERE <spanning>`), `=true` SUPPRESSES translateFilter's
	// enclosed-gather rotation (the `!t.inInnerCluster` read there) → the spanning
	// conjunct lands on an unbound element → silent-0; `=false` FIRES the rotation →
	// the body gets the gathered path that the direct (non-wrapped) form already
	// answers correctly. So the flag suppressed correct treatment rather than
	// protecting it — clearing it is correct today (discarded) and correct-direction
	// when the reach gap closes. The `!t.inInnerCluster` read in translateFilter is
	// CALLER-CONTEXTUAL (a filter's "merge leg vs fresh root" is not subtree-derivable),
	// so there is no clean local tree predicate to replace it — the genuine decouple
	// is threading enclosure context as a downward parameter (Volcano required-property
	// style), booked separately in TODO.md; it is not this lift's blocker.
	//
	// A JOIN input is NOT translated here: buildExistentialJoinSelect re-translates the
	// legs itself and ignores innerRef, and translating the enclosed join anyway both
	// wastes work and trips the name-model minted-binding decline for a duplicate-alias
	// FROM the fold serves fine (its binding-keyed select owns the whole construction).
	var innerRef *expressions.Reference
	if _, isJoin := f.Input.(*logical.LogicalJoin); !isJoin {
		prevEnclosure := t.inInnerCluster
		t.inInnerCluster = false
		innerRef = t.translateRef(f.Input)
		t.inInnerCluster = prevEnclosure
		if innerRef == nil {
			return nil
		}
	}

	fields := make([]values.RecordConstructorField, len(p.Projections))
	outputNames := make(map[string]struct{}, len(p.Projections))
	for i, col := range p.Projections {
		var v values.Value
		if i < len(p.ProjectedValues) && p.ProjectedValues[i] != nil {
			v = p.ProjectedValues[i]
		} else if i < len(p.IsComputed) && p.IsComputed[i] {
			// A computed projection the walker couldn't resolve — bail so the
			// ordinary projection path (text fallback) handles it.
			return nil
		} else {
			v = &values.FieldValue{Field: strings.ToUpper(col), Typ: values.UnknownType}
		}
		name := strings.ToUpper(col)
		if i < len(p.Aliases) && p.Aliases[i] != "" {
			name = strings.ToUpper(p.Aliases[i])
		} else if _, isField := v.(*values.FieldValue); !isField {
			// An UNALIASED COMPUTED (non-field) expression — `id + 1`, `COUNT(*)`,
			// CASE, etc. The normal projection path names it with the GENERATED
			// positional `_i` (deriveProjectionColumnDef's `_idx` rule;
			// executeProjection also stores the value under the `_i` key). Using the
			// expression TEXT (`ID + 1`) here would change Rows.Columns() from `_0`
			// to `ID + 1` purely because an EXISTS was added — and break a downstream
			// positional reference to the generated column. Use the SAME positional
			// name so the folded column's record key + Name + Label are identical to
			// the non-EXISTS control on every axis (RecordConstructorValue.Evaluate
			// keys the row by f.Name; foldedColumnDef derives Name/Label from it).
			name = "_" + strconv.Itoa(i)
		}
		fields[i] = values.RecordConstructorField{Name: name, Value: v}
		outputNames[name] = struct{}{}
	}
	outputCount := len(fields)

	// Classify the FROM source as single-table or a (binary INNER) JOIN. This
	// drives how qualified ORDER BY keys are handled: for a single-table source
	// the merged outer row carries columns under BARE keys, so a qualified key is
	// stripped to its bare column; for a JOIN source the merged outer row carries
	// columns under QUALIFIED `LEG.COL` keys (the bare key is last-leg-wins and
	// would pick the WRONG leg — mergeRows writes both), so the qualified key must
	// be PRESERVED and resolve against the qualified merged-row key. This is the
	// sort-key analog of rebaseOuterLegValue / the P1a alias-binding fix.
	src := classifySortSource(f.Input)

	// RFC-173 S4 F2-LEFT: Java's Cascades cannot plan ANY ORDER BY over the LEFT
	// projected-EXISTS fold (verified 4.12.11.0 — "could not plan query", even a
	// bare key), and classifySortSource classifies only INNER as a join source, so
	// a LEFT source's qualified sort key degrades to the bare last-leg-wins read and
	// mis-orders on a column-name collision (`ORDER BY q.k` when both legs have `k`).
	// Decline the fold when a LEFT source carries any ORDER BY (→ §8 → clean 0AF00),
	// matching Java exactly — never a silent wrong order. Booked follow-on if the
	// LEFT+ORDER-BY reach is ever wanted (a Go extension beyond Java's planner, with
	// LEFT classified as a join source). INNER is unaffected.
	if lj, ok := f.Input.(*logical.LogicalJoin); ok && lj.Kind == logical.JoinLeft {
		for _, op := range chain {
			if _, isSort := op.(*logical.LogicalSort); isSort {
				return nil
			}
		}
	}

	// A COMPUTED (non-column) ORDER BY key that is NOT one of the projected SELECT outputs
	// cannot be carried through the fold: collectExtraSortColumns can only append NAMED
	// columns, so the sort re-applied above the folded FlatMap would read a record that lacks
	// the expression's input columns and silently mis-order (e.g. `... ORDER BY col1 + 1`
	// where `col1 + 1` is not selected). Bail the fold (→ the projected-EXISTS guard
	// rejects the query cleanly with ErrCodeUnsupportedQuery) rather than return wrong rows. A
	// SELECTED computed expression pulls up to its own output field and remains foldable.
	for _, op := range chain {
		s, ok := op.(*logical.LogicalSort)
		if !ok {
			continue
		}
		for _, k := range s.Keys {
			if src.sortKeyName(k) != "" {
				continue // a nameable column — appended as a hidden field or already in output
			}
			if k.Value == nil {
				return nil // computed via raw ORDER BY text, not nameable → unfoldable
			}
			if _, ok := pullUpToOutputField(k.Value, fields); !ok {
				return nil // computed key absent from the projection → unfoldable; guard rejects
			}
		}
	}

	// ORDER BY a column that is NOT in the SELECT output (e.g.
	// `SELECT id, EXISTS(...) FROM t1 ORDER BY col1`) needs Java's
	// remainingOrderByExpressions branch (LogicalOperator.generateSelect):
	// concat the extra sort columns onto the folded projection, sort, then
	// re-project to drop them. Without this the sort key (a FieldValue over a
	// column the result record doesn't carry) silently fails to resolve and the
	// sort becomes a no-op (wrong order). We therefore append every sort-key
	// column missing from the output as an extra trailing field — those
	// reference the outer scan row, which the existential SelectExpression's
	// outer quantifier flows in full, so they resolve.
	extraSortCols := collectExtraSortColumns(chain, fields, src)
	for _, ec := range extraSortCols {
		// The hidden field is named by its QUALIFIED field reference (`T1.ID`,
		// `T2.SK`) — collision-free with an output alias that shares the bare column
		// name — and carries the source-column VALUE: a bare field over the outer
		// scan row for single-table (`FieldValue{ID}`), a QUALIFIED leg reference for
		// a JOIN (`FieldValue{Field:COL, Child:QOV(LEG)}`, which the NLJ rule's
		// rebaseOuterLegValue rewrites onto the merged row's `LEG.COL` key). The sort
		// above resolves the key to this field; the final cleanup pull-up drops it.
		fields = append(fields, values.RecordConstructorField{Name: ec.name, Value: ec.val})
		outputNames[ec.name] = struct{}{}
	}

	resultValue := values.NewRecordConstructorValue(fields...)
	prevChain := t.existsFoldHasChain
	t.existsFoldHasChain = len(chain) > 0
	folded := t.buildExistentialSelect(f, innerRef, resultValue)
	t.existsFoldHasChain = prevChain
	if folded == nil {
		return nil
	}

	// Re-apply the intervening sort/limit on top of the folded projection.
	// chain is top-to-bottom; we rebuild bottom-up, wrapping the folded result
	// with the operator nearest the filter first, so the original nesting is
	// preserved (Project(Sort(Limit(Filter))) → Sort(Limit(FoldedSelect))).
	expr := folded
	for i := len(chain) - 1; i >= 0; i-- {
		ref := expressions.InitialOf(expr)
		switch op := chain[i].(type) {
		case *logical.LogicalSort:
			expr = t.applySortOverRef(op, ref, fields, src)
		case *logical.LogicalLimit:
			expr = newLimitExprFromLogical(op, expressions.ForEachQuantifier(ref))
		default:
			// findExistsFilterUnderUnaryChain only collects Sort/Limit; any
			// other operator here is a bug — bail to the ordinary path.
			return nil
		}
		if expr == nil {
			return nil
		}
	}

	// Java's final pull-up: when extra ORDER BY columns were appended, re-project
	// only the original output so the sort columns don't leak into the result.
	//
	// RFC-141 ROOT FIX (P2): the cleanup MUST reuse the ORIGINAL per-column
	// alias provenance — leaving an unaliased column UNALIASED — so adding a hidden
	// sort column does not change any visible column's public label. The earlier
	// code force-aliased EVERY field to its datum Name (projAliases[i] = name),
	// which turned `SELECT t1.id` into an explicit alias `T1.ID` (label leaked the
	// qualifier) and re-aliased the EXISTS column to its raw expression. The fold's
	// first `outputCount` fields are the original SELECT outputs (extras are
	// appended after), so p.Aliases[i] (""==unaliased) is the original provenance.
	// We also preserve each projected value's TYPE (FieldValue.Typ = the folded
	// field's value type), so the EXISTS column stays BOOLEAN through the cleanup.
	if len(extraSortCols) > 0 {
		projVals := make([]values.Value, outputCount)
		projAliases := make([]string, outputCount)
		for i := 0; i < outputCount; i++ {
			// FieldValue.Field MUST equal the fold's f.Name exactly: the folded
			// output record is keyed by f.Name and FieldValue.Evaluate does an
			// exact-key lookup (no qualified→bare fallback). The cleanup column's
			// datum Name then equals that key, so a Scan never reads NULL.
			name := fields[i].Name
			typ := values.UnknownType
			if fields[i].Value != nil {
				if vt := fields[i].Value.Type(); vt != nil {
					typ = vt
				}
			}
			projVals[i] = &values.FieldValue{Field: name, Typ: typ}
			// Reuse the original SELECT-list alias (""==unaliased) so the cleanup's
			// label derivation matches the non-hidden-sort path exactly.
			if i < len(p.Aliases) {
				projAliases[i] = strings.ToUpper(p.Aliases[i])
			}
		}
		expr = expressions.NewLogicalProjectionExpressionWithAliases(
			projVals, projAliases, expressions.ForEachQuantifier(expressions.InitialOf(expr)),
		)
	}
	return expr
}

// sortSource classifies a projected-EXISTS fold's FROM source for ORDER BY
// key handling. The FlatMap's merged outer row carries columns under different
// key shapes depending on the source:
//
//   - single-table (isJoin=false): the outer scan row flows columns under their
//     BARE names (`ID`, `COL1`). A qualified ORDER BY key (`t1.col1`) is stripped
//     to its bare column so it resolves against that row.
//   - binary INNER JOIN (isJoin=true): mergeRows writes BOTH bare last-leg-wins
//     keys AND authoritative QUALIFIED `LEG.COL` keys. The bare key picks the
//     WRONG leg, so a qualified ORDER BY key (`t2.sk`) must be PRESERVED as the
//     QUALIFIED key (`T2.SK`) and resolve against the qualified merged-row key.
//     legAliases are the join's leg FROM-aliases (left, right) — a qualified
//     sort key whose qualifier names a leg is rebased onto that leg; one whose
//     qualifier names neither leg is treated as bare (defensive: it cannot be
//     attributed to a known leg).
//
// This is the sort-key analog of the NLJ rule's rebaseOuterLegValue / the P1a
// alias-binding fix: a join's merged row is resolved by qualified key, never the
// last-leg-wins bare key.
type sortSource struct {
	isJoin     bool
	legAliases []string
}

// classifySortSource inspects the fold's FROM input. A binary INNER LogicalJoin
// is a join source (its two legs flow under qualified merged-row keys); anything
// else (a single scan, a CTE/derived table) is single-table. Only INNER joins
// reach the projected-EXISTS fold (buildExistentialJoinSelect rejects outer
// joins), so we classify only that shape as a join.
func classifySortSource(input logical.LogicalOperator) sortSource {
	if j, ok := input.(*logical.LogicalJoin); ok && j.Kind == logical.JoinInner {
		// BINDING correlations, not display aliases (RFC-173 QP-REF-BIND
		// item 1): the fold's quantifiers and merged-row keys carry the
		// binding (buildExistentialJoinSelect), and a duplicate-alias sort
		// key resolves to a binding-qualified value — display-named
		// legAliases ([A, A]) failed to attribute it and the key silently
		// degraded to the bare last-leg-wins read. Identical to the alias
		// for every non-duplicate leg. Item 3: BURIED bindings under a box
		// leg join the set (structural walk) — a sort key qualified by a
		// buried source (`ORDER BY d.id` over `(dept LEFT emp) JOIN cat`)
		// must attribute to ITS leg, never degrade to the bare
		// first-match read (which silently sorted by the wrong column).
		var legAliases []string
		var collect func(op logical.LogicalOperator)
		collect = func(op logical.LogicalOperator) {
			if cj, isJ := op.(*logical.LogicalJoin); isJ {
				collect(cj.Left)
				collect(cj.Right)
				return
			}
			if b := sourceBinding(op); b != "" {
				legAliases = append(legAliases, b)
			}
		}
		collect(j.Left)
		collect(j.Right)
		return sortSource{isJoin: true, legAliases: legAliases}
	}
	return sortSource{isJoin: false}
}

// sortKeyName returns the upper-cased name a sort key resolves against the folded
// output record. Single-table: the BARE column (`T1.COL1`→`COL1`). JOIN: the
// QUALIFIED key when the qualifier names a known leg (`T2.SK`→`T2.SK`), else the
// bare column. Returns "" when the key is not a simple column reference (a
// computed expression). Used only by the computed-key nameability guard;
// output membership is VALUE-based (sortKeyInOutput), not by this name.
func (s sortSource) sortKeyName(k logical.SortKey) string {
	field := sortKeyFieldRef(k)
	if field == "" {
		return ""
	}
	return s.resolveKeyName(field)
}

// sortKeyFieldRef returns the RAW (possibly-qualified) upper-cased field reference
// a column sort key names — `T1.ID`, `COL1` — or "" when the key is a computed
// expression. Unlike sortKeyName it does NOT strip the qualifier, so callers can
// (a) build the source-column VALUE the key references for value-based output
// membership, and (b) name an appended hidden field by the qualified provenance
// (collision-free with an output alias — RFC-141 R4 P2b).
func sortKeyFieldRef(k logical.SortKey) string {
	if fv, ok := k.Value.(*values.FieldValue); ok {
		if fv.Child == nil {
			return strings.ToUpper(fv.Field)
		}
		// A composite leg reference (FieldValue{col, QOV(leg)}) — render LEG.COL.
		return strings.ToUpper(values.ExplainValue(fv))
	}
	if k.Value != nil {
		// Non-field Value (computed expression) — not a nameable column.
		return ""
	}
	field := strings.TrimSpace(k.Expr)
	if field == "" {
		return ""
	}
	// A bare or qualified identifier only — reject anything with operators,
	// parentheses, or whitespace (a computed expression), which the folded
	// record cannot expose by a single name.
	for _, r := range field {
		if !(r == '_' || r == '.' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')) {
			return ""
		}
	}
	return strings.ToUpper(field)
}

// sortKeySourceValue returns the SOURCE-COLUMN value a column sort key references
// — the value that, when evaluated against the existential SelectExpression's
// flowed outer/merged row, yields the column the user asked to sort by:
//
//   - single-table: a BARE FieldValue over the outer scan row (`t1.id`→FieldValue{ID}).
//   - JOIN leg: a QUALIFIED leg reference (`t2.sk`→FieldValue{Field:SK, Child:QOV(T2)})
//     so the NLJ rule's rebaseOuterLegValue rewrites it onto the authoritative
//     merged-row `LEG.COL` key (never the last-leg-wins bare key).
//
// This is the value used for VALUE-based output membership: a sort key is "in the
// output" iff an output field's VALUE equals this source-column value — NOT iff
// its bare name matches an output field NAME. The bare-name test conflated a
// qualified source reference (`t1.id`) with an unrelated output alias that shares
// the bare name (`col1 AS id` → output column named `ID`), so the sort ordered by
// the wrong column (RFC-141 R4 P2b). Returns nil for a computed key.
func (s sortSource) sortKeySourceValue(k logical.SortKey) values.Value {
	field := sortKeyFieldRef(k)
	if field == "" {
		return nil
	}
	if s.isJoin {
		if qual, col, ok := splitQualifier(field); ok {
			for _, leg := range s.legAliases {
				if leg != "" && strings.ToUpper(leg) == qual {
					return values.NewFieldValue(
						values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(qual)),
						col, values.UnknownType,
					)
				}
			}
		}
		return &values.FieldValue{Field: stripSortQualifier(field), Typ: values.UnknownType}
	}
	// Single-table: the outer scan row carries bare keys, so the source column is
	// the bare leaf (`t1.id`→ID), regardless of the qualifier.
	return &values.FieldValue{Field: stripSortQualifier(field), Typ: values.UnknownType}
}

// sortKeyInOutput reports whether some output field genuinely PROJECTS the source
// column a sort key references — a VALUE match against the source-column value,
// not a bare-name match against an output field's NAME. Returns the matching
// output field name (so the caller can pull the key up to it), or "" when the
// source column is not projected (a hidden remainingOrderBy column is appended).
func (s sortSource) sortKeyInOutput(k logical.SortKey, fields []values.RecordConstructorField) (string, bool) {
	src := s.sortKeySourceValue(k)
	if src == nil {
		return "", false
	}
	for _, f := range fields {
		if f.Value != nil && values.SemanticEqualsUnderAliasMap(src, f.Value, values.AliasMap{}) {
			return f.Name, true
		}
	}
	return "", false
}

// resolveKeyName maps a (possibly qualified) sort-key field reference to the name
// it resolves against the folded output record, per the source's key shape.
func (s sortSource) resolveKeyName(field string) string {
	up := strings.ToUpper(field)
	if !s.isJoin {
		return stripSortQualifier(up)
	}
	// JOIN source: keep the qualified `LEG.COL` key when the qualifier names a
	// known leg (it resolves the authoritative merged-row qualified key). A bare
	// key, or one whose qualifier is not a leg, falls back to the bare column.
	if qual, _, ok := splitQualifier(up); ok {
		for _, leg := range s.legAliases {
			if leg != "" && strings.ToUpper(leg) == qual {
				return up
			}
		}
	}
	return stripSortQualifier(up)
}

// extraSortCol is a hidden remainingOrderBy column appended to the folded
// projection: a collision-free NAME (the qualified field reference) and the
// source-column VALUE it reads (bare for single-table, qualified leg ref for a
// JOIN). The qualified name guarantees the hidden column never shadows an output
// alias that happens to share the bare column name (RFC-141 R4 P2b).
type extraSortCol struct {
	name string
	val  values.Value
}

// collectExtraSortColumns returns the hidden columns to append to the folded
// projection: the ORDER BY columns whose SOURCE column is NOT already projected
// by an output field (Java's remainingOrderByExpressions). Membership is
// VALUE-based (sortKeyInOutput) — a key is "in output" only when an output field
// genuinely projects its source column, never merely sharing a bare name with an
// output alias. Each appended column is named by its QUALIFIED field reference so
// it cannot collide with an output column. A sort key whose column can't be named
// (a computed expression) is skipped here — the caller
// (translateProjectOverExistsFilter) has already bailed the fold for any computed
// key absent from the projection, so a computed key reaching this point is a
// SELECTED expression that pulls up to its own output field. Order is stable and
// de-duplicated by name.
func collectExtraSortColumns(chain []logical.LogicalOperator, fields []values.RecordConstructorField, src sortSource) []extraSortCol {
	var extra []extraSortCol
	seen := map[string]struct{}{}
	for _, op := range chain {
		s, ok := op.(*logical.LogicalSort)
		if !ok {
			continue
		}
		for _, k := range s.Keys {
			name := sortKeyFieldRef(k)
			if name == "" {
				continue
			}
			if _, inOutput := src.sortKeyInOutput(k, fields); inOutput {
				continue
			}
			if _, dup := seen[name]; dup {
				continue
			}
			val := src.sortKeySourceValue(k)
			if val == nil {
				continue
			}
			seen[name] = struct{}{}
			extra = append(extra, extraSortCol{name: name, val: val})
		}
	}
	return extra
}

// stripSortQualifier returns the upper-cased BARE column name of a (possibly
// qualified) sort-key field reference: `T1.COL1` → `COL1`, `COL1` → `COL1`. A SQL
// `alias.column` reference has the column as its FINAL dotted segment, so we take
// everything after the last `.`. An empty or trailing-dot input yields the
// upper-cased whole (defensive).
func stripSortQualifier(field string) string {
	up := strings.ToUpper(field)
	if i := strings.LastIndex(up, "."); i >= 0 && i+1 < len(up) {
		return up[i+1:]
	}
	return up
}

// splitQualifier splits an upper-cased `QUAL.COL` reference into (QUAL, COL, true).
// A bare name, an empty string, or a trailing/leading dot yields ("", "", false).
// Only a SINGLE qualifier is split (the LAST dot) — a deeper `A.B.C` is uncommon
// in the EXISTS fold and is treated as qualifier `A.B`, column `C`.
func splitQualifier(field string) (string, string, bool) {
	up := strings.ToUpper(field)
	i := strings.LastIndex(up, ".")
	if i <= 0 || i+1 >= len(up) {
		return "", "", false
	}
	return up[:i], up[i+1:], true
}

// applySortOverRef builds a LogicalSortExpression with the given inner
// reference, deriving its sort keys from the LogicalSort's keys. The keys
// reference the projected output record's columns (the folded SelectExpression
// flows a record whose fields are the projected columns by name), so a
// FieldValue over the column name resolves against that output — mirroring
// Java's OrderByExpression.pullUp onto the projection's result value.
//
// fields are the folded projection's output fields. A sort key that references a
// SELECT-list alias whose value is a COMPUTED expression — most importantly the
// projected ExistsValue for `ORDER BY has_t2 DESC` — arrives with k.Value set to
// the raw expression (upgradeSortKeyValues copies proj.ProjectedValues[idx]). If
// that raw value were re-applied here, it would be evaluated ABOVE the FlatMap,
// where the existential binding is dead — the EXISTS sort key would be false for
// every row and the order would be wrong. pullUpSortKeyValue rewrites such a key
// to a FieldValue over the already-computed output column (Java's pull-up onto
// the lower select's getResultValue()), so the sort orders by the materialized
// boolean column.
func (t *cascadesTranslator) applySortOverRef(s *logical.LogicalSort, ref *expressions.Reference, fields []values.RecordConstructorField, src sortSource) expressions.RelationalExpression {
	sortKeys := make([]expressions.SortKey, len(s.Keys))
	for i, k := range s.Keys {
		nf := k.NullsFirst
		v := k.Value
		if v == nil {
			v = &values.FieldValue{Field: k.Expr, Typ: values.UnknownType}
		}
		v = pullUpSortKeyValue(k, v, fields, src)
		sortKeys[i] = expressions.SortKey{
			Value:      v,
			Reverse:    k.Dir == logical.SortDesc,
			NullsFirst: &nf,
		}
	}
	return expressions.NewLogicalSortExpression(sortKeys, expressions.ForEachQuantifier(ref))
}

// pullUpSortKeyValue rewrites a sort-key Value onto the folded projection's
// output record (Java's OrderByExpression.pullUp against the select's result
// value). The fold re-applies the ORDER BY ON TOP of the folded projection, so a
// sort key must resolve to the OUTPUT field that produced it — exactly the
// correspondence the normal ORDER BY path (upgradeSortKeyValues) establishes
// when it resolves a SELECT-list alias to its projected Value.
//
// The resolution is, in priority order:
//
//  1. OUTPUT-FIELD-VALUE MATCH (pullUpToOutputField): the key's Value
//     semantically equals one of the projected output fields' Values → it is the
//     pull-up of a SELECT-list alias (or the computed EXISTS boolean), so it is
//     replaced by a FieldValue over THAT output column's name. This is the same
//     match the normal ORDER BY alias path performs: `upgradeSortKeyValues` set
//     the key's Value to the exact projected Value (pointer identity), so an
//     alias key — even one rewritten to a flat FieldValue over the underlying
//     column (`col1 AS id, id AS x ORDER BY x` rewrites `x`→FieldValue{ID} =
//     ProjectedValues[X]) — pulls up to the output field whose value it IS (`X`),
//     NOT to the output column that merely shares the underlying name (`ID`,
//     which here holds col1). Running this match FIRST for every key shape is the
//     fix for the P2a divergence: previously the FieldValue case returned
//     before trying it, so an alias whose value was a simple column read the
//     wrong output field.
//
//  2. SOURCE-COLUMN-VALUE MATCH (column keys only): a key that is a (possibly
//     qualified) column reference — `t1.id`, `col1`, `t2.sk` — resolves to the
//     output field whose VALUE is that SOURCE column. The source-column value is
//     built source-aware (src.sortKeySourceValue): a BARE FieldValue over the
//     outer scan row for single-table, a QUALIFIED leg reference for a JOIN leg
//     (resolving the AUTHORITATIVE merged-row `LEG.COL` key, never the last-leg-
//     wins bare key). The match runs against the EXTENDED output fields, which
//     include the hidden remainingOrderBy columns appended for keys not otherwise
//     projected — so a non-selected key (`ORDER BY t1.id` over `SELECT col1 AS
//     id`) pulls up to its hidden field (named by the QUALIFIED ref, collision-
//     free), and a SELECTED source column (`SELECT t1.id … ORDER BY t1.id`) pulls
//     up to that output field. Matching by VALUE — not by stripping the qualifier
//     to a bare name and searching output NAMES — is the P2b fix: the
//     bare-name search resolved a qualified source key to an unrelated output
//     alias that merely shared the bare name (sorting by the wrong column).
//
// A key matching neither is left unchanged — it resolves against the flowed
// record as-is.
func pullUpSortKeyValue(k logical.SortKey, v values.Value, fields []values.RecordConstructorField, src sortSource) values.Value {
	// (1) Output-field-value match on the key's RAW value — runs for EVERY key
	// shape, mirroring the normal ORDER BY alias resolution. Handles SELECT-list
	// aliases (incl. the computed EXISTS boolean) whose Value upgradeSortKeyValues
	// set to the projected Value.
	if pulled, ok := pullUpToOutputField(v, fields); ok {
		return pulled
	}
	// (2) Source-column-value match — a column key resolves to the output field
	// (incl. the hidden remainingOrderBy columns) whose VALUE is its source column.
	if srcVal := src.sortKeySourceValue(k); srcVal != nil {
		if pulled, ok := pullUpToOutputField(srcVal, fields); ok {
			return pulled
		}
	}
	// Bare/already-resolved key (or an outer-row reference): resolves against the
	// flowed record unchanged.
	return v
}

// pullUpToOutputField rewrites a sort-key Value to a FieldValue over the folded
// projection's OUTPUT column whose Value the key semantically equals — Java's
// OrderByExpression.pullUp onto the lower select's getResultValue(). This is the
// shared key↔output-field correspondence: a SELECT-list alias key (whose Value
// upgradeSortKeyValues set to the exact projected Value) pulls up to the output
// field it IS, not to a same-named column. Returns (rewritten, true) on a match,
// (nil, false) otherwise.
//
// A flat-name FieldValue key that is already an output column BY NAME (a bare
// column carried straight through, e.g. `ORDER BY id` where `id` is also the
// output name) is intentionally NOT matched here unless its VALUE matches an
// output field — it falls to the name-based resolution so it keeps resolving by
// name. The value match only fires when the key's Value is structurally the
// projected expression (the alias / computed case), which is precisely when
// pulling up to the output field is required for correctness.
//
// POINTER IDENTITY is preferred over structural semantic equality: when two
// output fields share a semantically-equal value (`id AS a, id AS b ORDER BY
// b`), `upgradeSortKeyValues` copied the EXACT projected Value pointer into the
// sort key, so the pointer-identical field is the one the key actually names
// (`b`). A single semantic-equality pass alone would return the first equal
// field (`a`) — harmless for the sort result (the values are equal so the order
// is identical), but it would pull up to the wrong output column name. The two
// passes keep the pulled-up name faithful to the named alias.
func pullUpToOutputField(v values.Value, fields []values.RecordConstructorField) (values.Value, bool) {
	// Pass 1: exact pointer identity — the field whose Value the sort key IS.
	for _, f := range fields {
		if f.Value != nil && f.Value == v {
			return &values.FieldValue{Field: f.Name, Typ: values.UnknownType}, true
		}
	}
	// Pass 2: structural semantic equality — for keys whose Value was rebuilt
	// (not pointer-copied) but is structurally the projected expression.
	for _, f := range fields {
		if f.Value != nil && values.SemanticEqualsUnderAliasMap(v, f.Value, values.AliasMap{}) {
			return &values.FieldValue{Field: f.Name, Typ: values.UnknownType}, true
		}
	}
	return nil, false
}

// findExistsFilterUnderUnaryChain descends from a project's input through any
// intervening single-child unary operators (ORDER BY / LIMIT) to find a
// LogicalFilter that carries existential subqueries. It returns that filter and
// the chain of intervening operators ordered top-to-bottom (closest to the
// project first), or (nil, nil) when the input is not such a shape. Only Sort
// and Limit are treated as "transparent" intervening operators — a Project,
// Join, Aggregate, etc. between the outer project and the filter changes the
// row shape and is NOT folded through.
func findExistsFilterUnderUnaryChain(input logical.LogicalOperator) (*logical.LogicalFilter, []logical.LogicalOperator) {
	var chain []logical.LogicalOperator
	cur := input
	for {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			if len(f.ExistsSubqueries) > 0 {
				return f, chain
			}
			return nil, nil
		}
		// Descend only through fold-transparent unary operators (Sort/Limit);
		// logical.FoldTransparentUnaryInput is the shared transparency set the
		// generator's existsFilterReachableForFold also consults.
		next, ok := logical.FoldTransparentUnaryInput(cur)
		if !ok {
			return nil, nil
		}
		chain = append(chain, cur)
		cur = next
	}
}

// projectionReferencesExistsSubquery reports whether any projected Value is (or
// contains) an ExistsValue — the structural signal that the projection must be
// folded into the existential SelectExpression's result value (RFC-141 Phase 2)
// so the boolean is computed with the inner existential binding live.
func projectionReferencesExistsSubquery(projected []values.Value) bool {
	for _, v := range projected {
		if v == nil {
			continue
		}
		found := false
		values.WalkValue(v, func(node values.Value) bool {
			if _, ok := node.(*values.ExistsValue); ok {
				found = true
				return false
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

func valueContainsUnsafeScalarFunction(v values.Value) bool {
	unsafe := false
	values.WalkValue(v, func(node values.Value) bool {
		if sf, ok := node.(*values.ScalarFunctionValue); ok {
			if !values.IsCascadesSafeScalarFunction(sf.FuncName) {
				unsafe = true
				return false
			}
		}
		return true
	})
	return unsafe
}

func predicateContainsUnsafeFunction(p predicates.QueryPredicate) bool {
	unsafe := false
	predicates.WalkPredicate(p, func(qp predicates.QueryPredicate) bool {
		switch pred := qp.(type) {
		case *predicates.ComparisonPredicate:
			if valueContainsUnsafeScalarFunction(pred.Operand) {
				unsafe = true
				return false
			}
			if pred.Comparison.Operand != nil && valueContainsUnsafeScalarFunction(pred.Comparison.Operand) {
				unsafe = true
				return false
			}
		case *predicates.ValuePredicate:
			if valueContainsUnsafeScalarFunction(pred.Value) {
				unsafe = true
				return false
			}
		}
		return true
	})
	return unsafe
}

func (t *cascadesTranslator) translateUnion(u *logical.LogicalUnion) expressions.RelationalExpression {
	quantifiers := make([]expressions.Quantifier, 0, len(u.Inputs))
	for _, branch := range u.Inputs {
		ref := t.translateRef(branch)
		if ref == nil {
			return nil
		}
		quantifiers = append(quantifiers, expressions.ForEachQuantifier(ref))
	}
	if u.Distinct {
		return nil
	}
	return expressions.NewLogicalUnionExpression(quantifiers)
}

// gatheredSeedBake carries the positional-bake context for a gathered ordinal-seed
// input: the shared-authority per-leg windows + element slots that map a leg-column /
// element reference to its flat seed slot, a fresh seed QOV to read that slot via
// ofOrdinal, and the quantifier bound to it. It is the ONE authority every OUTER
// operator that resolves a leg column over the flat OUTPUT row shares — GROUP BY keys /
// aggregate operands (translateAggregate) and ORDER BY keys (translateSort) bake
// IDENTICALLY through it, never a per-operator copy that could drift. seedQOV is nil
// when the input is NOT a genuine ordinal seed (a name-model FALLBACK RC is ANCHORED
// and excluded); the caller then keeps the plain named quantifier and skips baking.
type gatheredSeedBake struct {
	windows      map[string]values.OrdinalSeedLegWindow
	elementSlots map[string]int
	seedQOV      values.Value
	quant        expressions.Quantifier
}

// gatheredSeedBakeContext detects a gathered ordinal seed at innerRef and builds its
// bake context. When innerRef is a genuine ordinal seed — a flat SelectExpression whose
// result RC is a NON-ANCHORED positional seed carrying an Explode element — it returns
// the per-leg windows, the element slots (the seed's OWN element fields located by
// fieldValueReferencesInner, keyed by rc index), a fresh seed QOV over the seed's row
// type, and a quantifier REBOUND to that QOV's correlation so a baked ofOrdinal read
// binds. Otherwise seedQOV is nil and quant is the plain named quantifier over
// fallbackAlias (the name-model path). namedQuantifier is a pure constructor, so
// computing the default eagerly and rebinding on a hit is free.
func (t *cascadesTranslator) gatheredSeedBakeContext(innerRef *expressions.Reference, fallbackAlias string) gatheredSeedBake {
	b := gatheredSeedBake{quant: t.namedQuantifier(fallbackAlias, innerRef)}
	sel, ok := innerRef.Get().(*expressions.SelectExpression)
	if !ok {
		return b
	}
	rc, ok := sel.GetResultValue().(*values.RecordConstructorValue)
	if !ok || rc.AnchoredJoin {
		return b
	}
	var innerCorr values.CorrelationIdentifier
	foundExplode := false
	for _, q := range sel.GetQuantifiers() {
		if _, isExplode := q.GetRangesOver().Get().(*expressions.ExplodeExpression); isExplode {
			innerCorr = q.GetAlias()
			foundExplode = true
			break
		}
	}
	if !foundExplode {
		return b
	}
	b.windows, _ = values.OrdinalSeedLegWindows(rc)
	b.elementSlots = map[string]int{}
	for i, f := range rc.Fields {
		if fieldValueReferencesInner(f.Value, innerCorr) {
			b.elementSlots[strings.ToUpper(f.Name)] = i // element slot = rc index
		}
	}
	seedCorr := values.UniqueCorrelationIdentifier()
	b.seedQOV = values.NewQuantifiedObjectValueOfType(seedCorr, rc.Type())
	b.quant = expressions.NamedForEachQuantifier(seedCorr, innerRef)
	return b
}

func (t *cascadesTranslator) translateSort(s *logical.LogicalSort) expressions.RelationalExpression {
	innerRef := t.translateRef(s.Input)
	if innerRef == nil {
		return nil
	}
	// Bake each sort key that names a leg column or the element to its flat seed slot via
	// the shared gatheredSeedBakeContext (whose doc owns the rationale) — the same
	// authority GROUP BY keys bake through. Without it a gathered-seed leg-column key
	// stays an unresolved name reading a dead constant, so InMemorySort sorts on nothing
	// (the silent DESC==ASC bug). A key already carrying a resolved ordinal is left as-is
	// by the bake; a non-seed input has seedQOV nil, so keys and quantifier are untouched.
	bake := t.gatheredSeedBakeContext(innerRef, sourceAlias(s.Input))
	sortKeys := make([]expressions.SortKey, len(s.Keys))
	for i, k := range s.Keys {
		nf := k.NullsFirst
		v := k.Value
		if v == nil {
			v = &values.FieldValue{Field: k.Expr, Typ: values.UnknownType}
		}
		if bake.seedQOV != nil {
			v = bakeGatheredGroupValue(v, bake.windows, bake.elementSlots, bake.seedQOV)
		}
		sortKeys[i] = expressions.SortKey{
			Value:      v,
			Reverse:    k.Dir == logical.SortDesc,
			NullsFirst: &nf,
		}
	}
	return expressions.NewLogicalSortExpression(sortKeys, bake.quant)
}

func (t *cascadesTranslator) translateProject(p *logical.LogicalProject) expressions.RelationalExpression {
	// Collect scalar subquery plans from projections. This MUST run for every
	// projection — including the RFC-141 projected-EXISTS fold below — because
	// a SELECT can mix a projected EXISTS with an (uncorrelated) scalar
	// subquery, e.g. `SELECT id, EXISTS(...), (SELECT MAX(id) FROM t2) FROM t1`.
	// The scalar subquery's plan is pre-evaluated by the executor and bound by
	// alias; skipping this collection (as the early-return fold path used to)
	// left the scalar column unbound → it came back NULL.
	for _, ssq := range p.ScalarSubqueries {
		t.scalarSubqueries = append(t.scalarSubqueries, ScalarSubqueryPlan{
			Alias: ssq.Alias,
			Plan:  ssq.Plan,
		})
	}

	// RFC-141 Phase 2: a projection over a filter that carries existential
	// subqueries, where the projection itself references a projected EXISTS,
	// folds INTO the existential SelectExpression's result value — so the
	// existential boolean is computed by the FlatMap with the inner binding
	// live (a separate Map above the FlatMap could not see that binding). Java
	// builds exactly this single FlatMap whose RETURN is the projection.
	//
	// The filter may not be the project's DIRECT input: an ORDER BY / LIMIT
	// sits between them (the builder emits Project(Sort(Filter)), with LIMIT
	// hoisted above the Project). findExistsFilterUnderUnaryChain sees THROUGH
	// those intervening unary operators to the existential filter. The fold
	// then re-applies the sort/limit ON TOP of the folded SelectExpression —
	// matching Java's `generateSort(generateSimpleSelect(output...), orderBys)`
	// (LogicalOperator.generateSelect): the projection is built first with the
	// existential binding live, then the sort wraps it, its keys rebased onto
	// the projected output record.
	// RFC-173 Outcome-B B1 widening: the fold ALSO fires for a plain WHERE-EXISTS
	// (no projected EXISTS) over a gated arity>=3 non-dup INNER cluster — the B1
	// wrap needs the projection folded in (its leg references only resolve over
	// the wrapped box when rebased at translation; an ordinary Map above the wrap
	// cannot see the legs). When the B1 arm declines, buildExistentialSelect bails
	// the widened fold (nil) and the ordinary path keeps today's plan shape.
	if filter, chain := findExistsFilterUnderUnaryChain(p.Input); filter != nil &&
		(projectionReferencesExistsSubquery(p.ProjectedValues) ||
			(len(chain) == 0 && t.existsFoldableGatheredCluster(filter))) {
		// A projected EXISTS combined with a CORRELATED scalar subquery in the same
		// SELECT list cannot be folded (the fold's existential SelectExpression and
		// the correlated-scalar LEFT-OUTER join select are incompatible structures —
		// see findUnfoldableProjectedExists). The logical guard rejects this shape
		// before translation; this is defense-in-depth so the fold's early return can
		// NEVER bypass the correlated-scalar dispatch below and silently drop the
		// scalar column. Bailing here returns nil → the caller emits a clean
		// ErrCodeUnsupportedQuery rather than wrong rows.
		if len(p.CorrelatedScalarSubqueries) > 0 {
			return nil
		}
		if sel := t.translateProjectOverExistsFilter(p, filter, chain); sel != nil {
			return sel
		}
	}

	if len(p.CorrelatedScalarSubqueries) > 1 {
		return nil
	}
	if len(p.CorrelatedScalarSubqueries) == 1 {
		return t.translateProjectWithCorrelatedScalar(p)
	}

	innerRef := t.translateRef(p.Input)
	if innerRef == nil {
		return nil
	}
	projected := make([]values.Value, len(p.Projections))
	for i, col := range p.Projections {
		if i < len(p.ProjectedValues) && p.ProjectedValues[i] != nil {
			projected[i] = p.ProjectedValues[i]
			continue
		}
		// Computed expression without a resolved Value — the walker
		// couldn't handle this shape. Bail so the query falls back.
		if i < len(p.IsComputed) && p.IsComputed[i] {
			return nil
		}
		projected[i] = &values.FieldValue{Field: strings.ToUpper(col), Typ: values.UnknownType}
	}
	return expressions.NewLogicalProjectionExpressionWithAliases(
		projected,
		p.Aliases,
		t.namedQuantifier(sourceAlias(p.Input), innerRef),
	)
}

func (t *cascadesTranslator) translateProjectWithCorrelatedScalar(p *logical.LogicalProject) expressions.RelationalExpression {
	csq := p.CorrelatedScalarSubqueries[0]

	// RFC-173 W4b shape 1: a MULTI-TABLE outer cluster dispatches to the
	// clustered-outer ordinal path first. decline=true is the CORRECT-or-LOUD
	// policy: a known non-rightmost correlation that did not ordinalize would
	// silently NULL (JOIN..ON / LEFT outers) or mis-plan (comma clusters) under
	// an anchored fallback — refuse to translate instead.
	if sel, decline := t.translateClusteredOuterScalar(p, csq); sel != nil || decline {
		return sel
	}

	// RFC-173 S4: the OUTER inherits the enclosing context (the former
	// `t.inInnerCluster = true` name-model producer is retired). Only a single-source
	// (clusterArity==1) or ungated outer reaches here — a buried-join/multi-source
	// outer is arity≠1 and declines regardless of the flag — so inheriting
	// prevEnclosure is the honest value (forcing false would be a latent wrong
	// assertion when the whole project is itself a name-model leg).
	outerRef := t.translateRef(p.Input)
	if outerRef == nil {
		return nil
	}
	outerAlias := sourceAlias(p.Input)
	outerQ := t.namedQuantifier(outerAlias, outerRef)

	// Peel LogicalLimit off the inner plan and re-attach it explicitly here, so
	// the limit caps each per-outer-row evaluation of the correlated scalar
	// subquery. (translateOp now translates a LogicalLimit to a
	// LogicalLimitExpression at the inner's own position — RFC-128; for the
	// correlated case we instead bind it to the quantifier the join drives, so
	// we peel it before translating the inner.)
	innerPlan := csq.InnerPlan
	var innerLimit *logical.LogicalLimit
	if lim, ok := innerPlan.(*logical.LogicalLimit); ok {
		innerLimit = lim
		innerPlan = lim.Input
	}

	// The INNER roots a FRESH cluster: a correlated-scalar subquery is NEVER merged
	// into its parent select (like the EXISTS inner — SelectMergeRule only targets
	// ForEach quantifiers), so it gates on its OWN arity, not the outer's enclosure.
	// translateSubqueryRef is the same primitive the existential inner uses; routing
	// the scalar inner through it makes the two never-merged-subquery classes
	// consistent and removes the defer's outer/inner enclosure conflation.
	innerRef := t.translateSubqueryRef(innerPlan)
	if innerRef == nil {
		return nil
	}

	// Wrap with LogicalLimitExpression if the inner plan had a LIMIT.
	if innerLimit != nil {
		innerAlias := sourceAlias(innerPlan)
		limitQ := t.namedQuantifier(innerAlias, innerRef)
		limitExpr := newLimitExprFromLogical(innerLimit, limitQ)
		innerRef = expressions.InitialOf(limitExpr)
	}

	// Source-anchored correlated-scalar-subquery join seed (RFC-077 7.6).
	//
	// The inner is a scalar SUBQUERY exposing exactly ONE value. The projection
	// reads it as the QUALIFIED name <innerAlias>.<scalarCol> — replaceScalarSubqueryRef
	// builds that field name (upper(innerAlias)+"."+upper(scalarCol)) — and the inner
	// quantifier's row carries the scalar under the key scalarCol (the runtime
	// mergeRows PREFIXES every inner key with innerAlias, dots and all, so
	// <innerAlias>.<scalarCol> resolves iff the inner key == scalarCol; it does).
	// The ordinal seed (scalarSubqueryOrdinalSeed) names the inner leg's SINGLE
	// field EXACTLY <innerAlias>.<scalarCol> and values it ofOrdinal(QOV(innerCorr),
	// 0) — so composeFieldOverConstructor folds the scalar reference onto the inner
	// leg with no NULL, whether or not scalarCol is itself dotted (a non-aggregate
	// subquery keeps its table qualifier, "C.NAME"; the RC field NAME carries the
	// qualified form the projection reads while the leg is keyed by a fresh unique
	// correlation). The outer leg carries its derivable columns so the (bare or
	// qualified) outer projections resolve too.
	//
	// Untranslatable when the outer columns are not derivable (only the catalog-free
	// nil-md path — production always passes md): the opaque-seed fallback was RETIRED
	// in RFC-077 7.6, so there is no result value to flow.
	scalarCol := strings.ToUpper(csq.ScalarCol)
	outerCols := t.legColumns(p.Input)
	if outerCols == nil || outerAlias == "" || scalarCol == "" || csq.InnerAlias == "" {
		return nil
	}
	// RFC-173 W4b: ordinalize the 2-leg seed when the OUTER is a SINGLE SOURCE
	// (clusterArity==1); the clustered-outer dispatch above already ordinalized
	// the gated multi-table outers (shape 1). The name-model anchored fallback
	// was RETIRED in S4 (R3), so this ordinal seed is the sole surviving
	// correlated-scalar seed; a decline (nil) loud-declines below.
	//
	// The former innerScalarIsRowColumn guard (shape 3) is GONE: a COMPUTED scalar
	// is now MATERIALIZED as the inner's projected output (buildCorrelatedScalar,
	// positional `_0`), so the scalar is ALWAYS present in the inner row (plain
	// column, aggregate output, or projected computation) — the guard's "is the
	// scalar in the inner row" question is unconditionally yes. The single inner
	// leg reads ofOrdinal(inner, 0) regardless of whether the scalar is a stored
	// column or a computed expression.
	//
	// The former innerContainsJoin gate (shape 2) is GONE too: the ordinal
	// seed's inner leg is keyed by a FRESH unique correlation id, not the SQL
	// alias, so a JOIN-inner's own typed QOV(InnerAlias, N-field) can no longer
	// collide with the seed's 1-field inner leg at widenLegTypesFromPlan
	// (see scalarSubqueryOrdinalSeed).
	var resultValue values.Value
	var innerLegCorr values.CorrelationIdentifier
	if t.clusterArity(p.Input) == 1 {
		ordinalInnerCorr := values.UniqueCorrelationIdentifier()
		resultValue = t.scalarSubqueryOrdinalSeed(outerAlias, p.Input, ordinalInnerCorr, csq.InnerAlias, scalarCol)
		if resultValue != nil {
			innerLegCorr = ordinalInnerCorr
		}
	}
	if resultValue == nil {
		// RFC-173 S4 (R3): the name-model NewScalarSubqueryAnchoredRecord
		// fallback is RETIRED — a full-corpus census reached it from no SQL
		// query. A correlated scalar whose outer did not ordinalize declines
		// LOUDLY (correct-or-loud, never a silent name-model result). Probe the
		// gate ONCE for the diagnosis:
		//   - a GATED outer reaching here is a dispatch gap — its rows are
		//     POSITIONAL, so an anchored (alias-keyed) read would silent-NULL;
		//   - an UNGATED outer never had a correct anchored home either (the
		//     retired fallback resolved only single-source outers; every merged
		//     multi-source row it would have silent-NULLed). The gate's own
		//     Reason names the real ungated cause (dup-poisoned `FROM x,x`,
		//     lateral unnest, existential-ON, …) — none reachable from SQL today,
		//     but a loud decline with the accurate reason beats a hand-guessed one.
		if cj := peelToClusterJoin(p.Input); cj != nil {
			prevEnclosure := t.inInnerCluster
			t.inInnerCluster = false
			d := t.ordinalWedgeGateDecide(cj)
			t.inInnerCluster = prevEnclosure
			if d.Gated {
				t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
					"correlated scalar subquery: the gated outer cluster's ordinal dispatch declined (positional rows cannot take an anchored fallback)"))
			} else {
				t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
					"correlated scalar subquery: the outer did not ordinalize (%s) — unclassifiable for the ordinal seed", d.Reason))
			}
		} else {
			t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"correlated scalar subquery: the single-source outer is not derivable for the ordinal seed"))
		}
		return nil
	}

	// With no user LIMIT, the scalar must yield AT MOST ONE inner row per outer
	// row: mark the inner quantifier strict-single so ImplementNestedLoopJoinRule
	// wraps it in a strict FirstOrDefault (a second row → 21000). A user LIMIT
	// leaves StrictSingle false — the LIMIT is the user's deliberate truncation.
	// The quantifier carries innerLegCorr — the ordinal seed's fresh unique id
	// (the sole surviving path; the name-model fallback was retired in S4/R3).
	var innerQ expressions.Quantifier
	if csq.StrictSingle {
		innerQ = expressions.NamedForEachStrictSingleQuantifier(innerLegCorr, innerRef)
	} else {
		innerQ = expressions.NamedForEachQuantifier(innerLegCorr, innerRef)
	}

	joinSelect := expressions.NewSelectExpressionWithJoinType(
		resultValue,
		[]expressions.Quantifier{outerQ, innerQ},
		nil,
		[]string{outerAlias, innerLegCorr.Name()},
		expressions.JoinLeftOuter,
	)
	joinRef := expressions.InitialOf(joinSelect)

	projected := make([]values.Value, len(p.Projections))
	innerCorr := values.NamedCorrelationIdentifier(csq.InnerAlias)
	for i, col := range p.Projections {
		if i < len(p.ProjectedValues) && p.ProjectedValues[i] != nil {
			projected[i] = replaceScalarSubqueryRef(p.ProjectedValues[i], csq, innerCorr)
			continue
		}
		if i < len(p.IsComputed) && p.IsComputed[i] {
			return nil
		}
		projected[i] = &values.FieldValue{Field: strings.ToUpper(col), Typ: values.UnknownType}
	}

	projQ := t.namedQuantifier("", joinRef)
	return expressions.NewLogicalProjectionExpressionWithAliases(
		projected,
		p.Aliases,
		projQ,
	)
}

func replaceScalarSubqueryRef(v values.Value, csq logical.CorrelatedScalarSubquery, innerCorr values.CorrelationIdentifier) values.Value {
	return values.Replace(v, func(node values.Value) values.Value {
		if ssq, ok := node.(*values.ScalarSubqueryValue); ok && ssq.Alias == csq.Alias {
			qualifiedName := strings.ToUpper(innerCorr.Name()) + "." + strings.ToUpper(csq.ScalarCol)
			return &values.FieldValue{Field: qualifiedName, Typ: values.UnknownType}
		}
		return node
	})
}

func (t *cascadesTranslator) translateDistinct(d *logical.LogicalDistinct) expressions.RelationalExpression {
	innerRef := t.translateRef(d.Input)
	if innerRef == nil {
		return nil
	}
	return expressions.NewLogicalDistinctExpression(
		t.namedQuantifier(sourceAlias(d.Input), innerRef),
	)
}

// aggregateOperandReferencesColumn reports whether any aggregate operand reads a
// COLUMN — i.e. is not COUNT(*) (nil operand) and not a bare constant (COUNT(1)).
// Such an operand over a gathered multi-source unnest must be POSITIONALLY BAKED over
// the un-collapsed seed even without a GROUP BY (the global SUM(EL) case) — so this
// marks the input underAggregate to trigger the bake; a pure COUNT(*) references
// nothing and keeps the raw seed unbaked. The resolved operands are populated on the
// logical node before cascades translation (upgradeAggregateOperands).
func aggregateOperandReferencesColumn(a *logical.LogicalAggregate) bool {
	for _, op := range a.AggregateOperands {
		if op == nil {
			continue
		}
		if _, isConst := op.(*values.ConstantValue); isConst {
			continue
		}
		return true
	}
	return false
}

// Go extension: Java's fdb-relational 4.11.1.0 does not support GROUP BY;
// its AstNormalizer rejects it with UNSUPPORTED_QUERY before reaching the planner.
func (t *cascadesTranslator) translateAggregate(a *logical.LogicalAggregate) expressions.RelationalExpression {
	if a.Having != "" && a.HavingPredicate == nil {
		return nil
	}
	for _, ssq := range a.HavingScalarSubqueries {
		t.scalarSubqueries = append(t.scalarSubqueries, ScalarSubqueryPlan{
			Alias: ssq.Alias,
			Plan:  ssq.Plan,
		})
	}
	// A gathered multi-source unnest under this aggregate flows its RAW per-leg
	// positional seed, and any reference to an element/leg column — a grouped element
	// key, OR a global aggregate OPERAND like SUM(EL)/SUM(WAUX.WV) — is POSITIONALLY
	// BAKED over it below (leg columns via OrdinalSeedLegWindows, the element by its rc
	// slot). Mark the input translation as underAggregate so the bake site fires.
	// Triggered when there is a GROUP BY (the grouped key) OR any aggregate operand
	// references a column (the global SUM(EL) case). A global COUNT(*) references
	// nothing, so it keeps the raw seed unbaked.
	prevUnderAgg := t.underAggregate
	if len(a.GroupKeys) > 0 || aggregateOperandReferencesColumn(a) {
		t.underAggregate = true
	}
	innerRef := t.translateRef(a.Input)
	t.underAggregate = prevUnderAgg
	if innerRef == nil {
		return nil
	}
	// RFC-173 S4 qualifier-honoring resolution: a GATHERED unnest input un-collapses to
	// the raw per-leg seed, so positionally BAKE the group keys / operands to their flat
	// slots via the shared gatheredSeedBakeContext (see its doc) — the qualifier-honoring
	// read that replaces the retired name-keyed wrap. The outer WHERE already baked itself
	// (bakeGatedJoinPredicates fires on the SelectExpression).
	bake := t.gatheredSeedBakeContext(innerRef, sourceAlias(a.Input))
	groupByQuant := bake.quant
	groupKeys := make([]values.Value, len(a.GroupKeys))
	for i, key := range a.GroupKeys {
		if i < len(a.GroupKeyValues) && a.GroupKeyValues[i] != nil {
			groupKeys[i] = a.GroupKeyValues[i]
		} else {
			groupKeys[i] = &values.FieldValue{Field: key, Typ: values.UnknownType}
		}
		if bake.seedQOV != nil {
			groupKeys[i] = bakeGatheredGroupValue(groupKeys[i], bake.windows, bake.elementSlots, bake.seedQOV)
		}
	}
	aggSpecs := make([]expressions.AggregateSpec, 0, len(a.Aggregates))
	for i, aggText := range a.Aggregates {
		spec, ok := parseAggregateText(aggText)
		if !ok {
			return nil
		}
		// The resolved operand (set by upgradeAggregateOperands /
		// buildCorrelatedScalar via resolver.WalkExpression) is the single
		// source of truth. parseAggregateText only reconstructs the operand by
		// re-scanning the slot-name text, and parseOperandValue is a naive
		// left-to-right splitter that mangles nested/parenthesised arithmetic
		// (e.g. "(AMOUNT+10)*2" splits on the inner '+' into garbage atoms),
		// yielding an unresolvable operand that accumulates to NULL and silently
		// drops HAVING groups. Whenever a resolved operand is present, it wins —
		// never the lossy reparse. (A prior `!isArith` guard preferred the
		// reparse for arithmetic operands; that was the operand-routing hole.)
		if i < len(a.AggregateOperands) && a.AggregateOperands[i] != nil {
			spec.Operand = a.AggregateOperands[i]
		}
		if bake.seedQOV != nil && spec.Operand != nil {
			spec.Operand = bakeGatheredGroupValue(spec.Operand, bake.windows, bake.elementSlots, bake.seedQOV)
		}
		if i < len(a.Aliases) && a.Aliases[i] != "" {
			spec.Alias = strings.ToUpper(a.Aliases[i])
		}
		aggSpecs = append(aggSpecs, spec)
	}
	groupBy := expressions.NewGroupByExpression(
		groupKeys,
		aggSpecs,
		groupByQuant,
	)
	if a.HavingPredicate == nil {
		return groupBy
	}
	groupByRef := expressions.InitialOf(groupBy)

	// HAVING with EXISTS subqueries is not supported — the correlation
	// references pre-GROUP-BY scope (table columns) but the HAVING
	// evaluates in post-GROUP-BY scope (group keys + aggregates).
	// Java doesn't support this either (no test coverage). Return nil
	// so the planner produces "could not plan query" instead of
	// silently returning wrong results. NOTE: this blanket rejection is
	// also what keeps HAVING out of the esq polarity-guard surface — if
	// it is ever narrowed, HAVING becomes a fifth consumer and needs
	// declineNegatedOuterOnlyEsq wiring like the WHERE/ON sites.
	if len(a.HavingExistsSubqueries) > 0 {
		return nil
	}

	return expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{a.HavingPredicate},
		expressions.ForEachQuantifier(groupByRef),
	)
}

func parseAggregateText(text string) (expressions.AggregateSpec, bool) {
	upper := strings.ToUpper(strings.TrimSpace(text))
	lparen := strings.Index(upper, "(")
	if lparen < 0 {
		return expressions.AggregateSpec{}, false
	}
	rparen := strings.LastIndex(upper, ")")
	if rparen < lparen {
		return expressions.AggregateSpec{}, false
	}
	funcName := strings.TrimSpace(upper[:lparen])
	operandText := strings.TrimSpace(upper[lparen+1 : rparen])

	var fn expressions.AggregateFunction
	switch funcName {
	case "COUNT":
		fn = expressions.AggCount
	case "SUM":
		fn = expressions.AggSum
	case "MIN":
		fn = expressions.AggMin
	case "MAX":
		fn = expressions.AggMax
	case "AVG":
		fn = expressions.AggAvg
	default:
		return expressions.AggregateSpec{}, false
	}

	if strings.HasPrefix(operandText, "DISTINCT ") {
		return expressions.AggregateSpec{}, false
	}

	var operand values.Value
	if operandText == "*" {
		operand = &values.ConstantValue{Value: nil, Typ: values.UnknownType}
	} else {
		operand = parseOperandValue(operandText)
	}

	return expressions.AggregateSpec{Function: fn, Operand: operand, OperandName: operandText}, true
}

func parseOperandValue(text string) values.Value {
	for _, op := range []struct {
		sym string
		op  values.ArithmeticOp
	}{
		{"+", values.OpAdd},
		{"-", values.OpSub},
		{"*", values.OpMul},
		{"/", values.OpDiv},
	} {
		idx := strings.Index(text, op.sym)
		if idx > 0 && idx < len(text)-1 {
			left := strings.TrimSpace(text[:idx])
			right := strings.TrimSpace(text[idx+1:])
			if left != "" && right != "" {
				return &values.ArithmeticValue{
					Op:    op.op,
					Left:  parseAtomValue(left),
					Right: parseAtomValue(right),
				}
			}
		}
	}
	return parseAtomValue(text)
}

func parseAtomValue(text string) values.Value {
	if n, err := strconv.ParseInt(text, 10, 64); err == nil {
		return &values.ConstantValue{Value: n, Typ: values.NullableLong}
	}
	if f, err := strconv.ParseFloat(text, 64); err == nil {
		return &values.ConstantValue{Value: f, Typ: values.NullableDouble}
	}
	return &values.FieldValue{Field: text, Typ: values.UnknownType}
}

func (t *cascadesTranslator) translateJoin(j *logical.LogicalJoin) expressions.RelationalExpression {
	// For RIGHT JOIN, swap branches and treat as LEFT JOIN. The NLJ
	// executor iterates the "outer" (left) and for each unmatched row
	// emits NULLs for the inner (right) columns. Swapping makes the
	// originally-right table the outer, which is exactly RIGHT JOIN
	// semantics. This matches the standard approach — Java's Cascades
	// doesn't distinguish RIGHT from LEFT either; the planner
	// normalises RIGHT → LEFT with swapped children.
	// Lateral array UNNEST (`FROM t, t.arr AS x [AT ord]`): the right child is
	// a LogicalUnnest. Lower it to a correlated FlatMap-over-Explode rather
	// than a generic join (RFC-142).
	if u, ok := j.Right.(*logical.LogicalUnnest); ok {
		return t.translateUnnestJoin(j, u)
	}

	// RFC-173 W5 commit 3: the ENCLOSED unnest class (`FROM A, A.arr AS x, B`
	// — an unnest join buried as a leg of this inner cluster) gathers into
	// the same flat (N+1)-quantifier select via rotation. Fail-open: nil
	// falls through to the paths below, which translate the buried unnest
	// ENCLOSED (the name-model residual) with the faithful diagnostics.
	if sel := t.translateEnclosedUnnestGather(j); sel != nil {
		return sel
	}

	left := j.Left
	right := j.Right
	kind := j.Kind
	if kind == logical.JoinRight {
		left, right = right, left
		kind = logical.JoinLeft
	}

	// RFC-173 Slice 2: decide (and record) the ordinal-wedge gate for this
	// seed BEFORE leg translation mutates the enclosure flag. W3b consumes
	// it below: a gated join seeds the ORDINAL result value + baked
	// predicates instead of the name-model anchored RC.
	gateDecision := t.ordinalWedgeGate(j)

	// S3 fulcrum: a GATED INNER root with NESTED inner joins translates FLAT
	// — one select over ALL the cluster's direct-nesting legs (Java flattens
	// inner joins at translation, QueryVisitor.java:429-434; nested binaries
	// are never seeded). Derived/filter boundaries stay legs and compose via
	// SelectMergeRule during rewriting. The 2-leg case (no nesting) and the
	// gated FULL box keep the binary flow below unchanged.
	if gateDecision.Gated && kind == logical.JoinInner {
		if legs := t.gatherInnerClusterLegs(j); len(legs) > 2 {
			return t.translateGatheredInnerCluster(j, legs, nil)
		}
	}

	// Leg enclosure: an INNER (incl. cross) join's legs are part of THIS
	// cluster — a nested join there merges into a ≥3-way select and must stay
	// name-model. A LEFT-outer box's PRESERVED (left) leg is ALSO enclosed:
	// RewriteOuterJoinRule dissolves the box into an INNER + null-on-empty
	// select during REWRITING, and SelectMergeRule then flattens the
	// preserved child into it (the RFC-153 joined-preserved machinery — the
	// W3b flip's drift assert caught a gated preserved-leg join being merged
	// exactly there). The NULL-SUPPLYING (right) leg becomes the
	// null-on-empty subselect — never a merge target — and FULL-outer boxes
	// are never rewritten: those legs root fresh clusters. Restored before
	// the OnExists subplan loop: existential subplans are never merged into
	// this select, so they root fresh clusters too.
	prevEnclosure := t.inInnerCluster
	// S3 fulcrum: legs of a GATED parent translate FRESH (their own inner
	// joins gate independently; SelectMergeRule composes ordinal RVs -- see
	// translateGatheredInnerCluster). Enclosure poisoning survives only for
	// NAME-MODEL parents (a non-gated inner cluster, a LEFT box preserved
	// leg -- the RFC-153 dissolve/flatten machinery stays name-model to W4).
	t.inInnerCluster = !gateDecision.Gated && (kind == logical.JoinInner || kind == logical.JoinLeft)
	leftRef := t.translateRef(left)
	if leftRef == nil {
		t.inInnerCluster = prevEnclosure
		return nil
	}
	if kind == logical.JoinLeft {
		// Null-supplying leg: fresh cluster (see above).
		t.inInnerCluster = false
	}
	rightRef := t.translateRef(right)
	t.inInnerCluster = prevEnclosure
	if rightRef == nil {
		return nil
	}
	leftAlias := sourceAlias(left)
	rightAlias := sourceAlias(right)
	// RFC-173 QP-REF-BIND item 1: a GATED binary join's quantifiers and
	// row namespaces carry the BINDING correlation (== the alias for every
	// non-duplicate leg; the parser-minted id for a later duplicate) —
	// matching the ordinal seed RC's QOVs, the bake maps and the executor's
	// span windows, and matching what the resolver emits for a reference
	// bound to a duplicate leg. The NAME-MODEL arm keeps the DISPLAY alias:
	// its anchored RC and merged-row keys are alias-qualified, and swapping
	// namespaces there would nil every qualified read (the still-poisoned
	// dup classes stay correct-or-loud via per-attribute 42702 + the gate).
	if gateDecision.Gated {
		leftAlias = legBinding(left)
		rightAlias = legBinding(right)
	} else if leg := mintedBindingLeg(left, right); leg != "" {
		// A minted-binding leg reached the NAME-MODEL arm (the gate narrowed
		// off — nesting, arity, poison): its display-keyed anchored RC and
		// merged rows cannot carry the binding, so the resolver's
		// binding-qualified reads would serve silent NULLs. Decline LOUDLY
		// (correct-or-loud; the ordinal seed is the only representation for
		// duplicate legs — the dual-window carve-out class).
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"duplicate FROM alias: leg %s requires the ordinal seed; the name-model join cannot serve it (%s)",
			leg, gateDecision.Reason))
		return nil
	}

	// Use named quantifiers so aliases match the predicate QOV
	// correlations created by the SQL resolver.
	leftQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(leftAlias), leftRef,
	)
	rightQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(rightAlias), rightRef,
	)

	var preds []predicates.QueryPredicate
	if j.OnPredicate != nil {
		if qp, ok := j.OnPredicate.(predicates.QueryPredicate); ok {
			// When the ON clause carries EXISTS subqueries (RFC-154 §5), flatten a
			// top-level AND so the ExistentialValuePredicate becomes its OWN
			// top-level conjunct — the directly-handled semi-join shape
			// CheckBuriedExistentialPredicate requires and implementJoinWithExistential
			// routes (a single And(equi, EXISTS) predicate reads as a BURIED
			// existential and is rejected). Mirrors translateJoinWithExists's flatten
			// of the WHERE predicate. Non-EXISTS joins keep the single predicate
			// (the conjunctive SelectExpression predicate list is semantically the
			// same; this avoids touching the heavily-tested plain-join shape).
			if and, ok := qp.(*predicates.AndPredicate); ok && len(j.OnExistsSubqueries) > 0 {
				preds = append(preds, and.SubPredicates...)
			} else {
				preds = []predicates.QueryPredicate{qp}
			}
		}
	}

	var joinType expressions.JoinType
	switch kind {
	case logical.JoinLeft:
		joinType = expressions.JoinLeftOuter
	case logical.JoinFull:
		// FULL OUTER is symmetric — no operand swap (the JoinRight swap
		// above does not fire for JoinFull). The materialized NLJ keeps
		// the original left/right column layout.
		joinType = expressions.JoinFullOuter
	default:
		joinType = expressions.JoinInner
	}

	// The result value (which drives `SELECT *` column order) follows the
	// ORIGINAL SQL FROM declaration order — j.Left then j.Right — NOT the
	// execution swap a RIGHT JOIN applies (swapping to drive the originally-right
	// table as the NLJ outer). Java's RIGHT JOIN normalizes to LEFT with swapped
	// children too, but its `SELECT *` still lists the columns in declaration
	// order; building the anchored RC from (left,right) post-swap would emit the
	// right table's columns first (dept.*, emp.* for `emp RIGHT JOIN dept`), a
	// column-order divergence. The quantifiers stay in execution (swapped) order;
	// the RC keys columns by alias, so leg ORDER only affects `SELECT *` layout.
	rvLeft, rvRight := j.Left, j.Right
	rvLeftAlias, rvRightAlias := sourceAlias(rvLeft), sourceAlias(rvRight)
	var resultValue values.Value
	if gateDecision.Gated {
		// RFC-173 Slice 2 W3b — the ordinal wedge seed: baked ofOrdinalNumber
		// concatenation of the two legs + eager (leg, ordinal) predicate
		// baking (contract ruling #2). Same untranslatable-on-nil rule as the
		// anchored seed below.
		// W4-left: rvLeft/rvRight are DECLARATION order (I2 — only the
		// null-supplying ROLE keys on the original kind; a RIGHT join's null
		// side is its LEFT operand).
		var legTypes map[string]bakeLegType
		resultValue, legTypes = t.buildOrdinalJoinResultValue([]clusterLeg{
			clusterLegOf(rvLeft, j.Kind == logical.JoinRight),
			clusterLegOf(rvRight, j.Kind == logical.JoinLeft),
		})
		preds = bakeGatedJoinPredicates(preds, legTypes)
	} else {
		resultValue = t.buildJoinResultValue(rvLeft, rvRight, rvLeftAlias, rvRightAlias)
	}
	if resultValue == nil {
		// A leg's columns are not derivable (only the catalog-free nil-md path;
		// every md-bearing production query anchors — RFC-077 7.6). Untranslatable.
		return nil
	}

	quantifiers := []expressions.Quantifier{leftQ, rightQ}
	sourceAliases := []string{leftAlias, rightAlias}

	// EXISTS in the ON clause (RFC-154 §5): attach each lifted EXISTS subquery as
	// an existential quantifier + its correlation predicate, producing a
	// 2-ForEach-+-Existential SelectExpression that the NLJ rule's
	// implementJoinWithExistential path lowers to a semi-join. Only populated for
	// INNER joins (upgradeJoinOnPredicates rejects OUTER EXISTS-in-ON), so the
	// joinType passed below is JoinInner and the existential semantics match
	// EXISTS-in-WHERE-over-a-join (translateJoinWithExists).
	// Defensive polarity guard for the ON-lift path: a flagged esq under a
	// negated ON marker would outer-route an outer-only conjunct into
	// anti-join semantics (same law as the WHERE sites).
	if onPred, ok := j.OnPredicate.(predicates.QueryPredicate); ok && t.declineNegatedOuterOnlyEsq(onPred, j.OnExistsSubqueries) {
		return nil
	}
	for _, esq := range j.OnExistsSubqueries {
		subRef := t.translateSubqueryRef(esq.Plan)
		if subRef == nil {
			return nil
		}
		existQ := expressions.NamedExistentialQuantifier(esq.Alias, subRef)
		quantifiers = append(quantifiers, existQ)
		innerCorrName, joinPred := existsInnerCorrelation(esq)
		if joinPred != nil {
			preds = append(preds, joinPred)
		}
		sourceAliases = append(sourceAliases, innerCorrName)
	}

	return expressions.NewSelectExpressionWithJoinType(
		resultValue,
		quantifiers,
		preds,
		sourceAliases,
		joinType,
	)
}

// translateJoinWithExists builds a flat SelectExpression from a LogicalJoin
// + LogicalFilter that carries EXISTS subqueries. Instead of nesting one
// SelectExpression (the join) inside another (the EXISTS filter), this
// method produces a single SelectExpression with ForEach(left),
// ForEach(right), and Existential quantifiers. The combined predicate
// covers both the join ON and the filter WHERE. The NLJ rule's
// implementJoinWithExistential path handles this 2+1 pattern.
func (t *cascadesTranslator) translateJoinWithExists(
	j *logical.LogicalJoin,
	f *logical.LogicalFilter,
) expressions.RelationalExpression {
	if t.declineNegatedOuterOnlyEsq(f.Predicate, f.ExistsSubqueries) {
		return nil
	}
	// The flatten is INNER-only BY CONTRACT: the dispatch in translateFilter
	// routes every OUTER kind to the generic arm (merging a preserved-side
	// WHERE conjunct into the flat select would turn it into ON semantics —
	// null-padding rows that must drop). A non-INNER join here is a caller
	// bug; decline rather than silently mistranslate it to INNER.
	if j.Kind != logical.JoinInner {
		return nil
	}
	left := j.Left
	right := j.Right

	// Collect scalar subquery plans from the filter.
	for _, ssq := range f.ScalarSubqueries {
		t.scalarSubqueries = append(t.scalarSubqueries, ScalarSubqueryPlan{
			Alias: ssq.Alias,
			Plan:  ssq.Plan,
		})
	}

	// RFC-173 QP-REF-BIND item 2, commit 3: the flatten consults the wedge
	// gate (ONE authority — ordinalWedgeGate; design-ruling condition 4) and,
	// when the join gates, seeds the baked ordinal RC below instead of the
	// anchored one. Decided BEFORE leg translation mutates the enclosure flag
	// (the translateJoin convention). Two flatten-specific narrowings on top
	// of the shared decision:
	//   - Arity EXACTLY 2: this arm builds exactly two ForEach legs, and
	//     buildOrdinalJoinResultValue types them as single-source legs — a
	//     nested-cluster leg would seed a 2-leg concat whose windows
	//     disagree with the arity SelectMergeRule's flattening produces.
	//     The DECLINE is the safety mechanism itself (the historical S2
	//     drift assert died with the exactly-2 wedge at the S3 fulcrum).
	//     The N-way flatten rides the gathered-cluster machinery when a
	//     later slice routes it here.
	//   - No existential-alias collisions: an EXISTS alias colliding with a
	//     leg alias (or another EXISTS alias) makes the flat select's
	//     correlations indistinguishable — fail toward the name model, the
	//     gate's unclassifiable direction.
	gateDecision := t.ordinalWedgeGateDecide(j)
	gatedFlatten := gateDecision.Gated
	if gatedFlatten && gateDecision.Arity != 2 {
		gatedFlatten = false
		gateDecision = wedgeGateDecision{
			Arity:  gateDecision.Arity,
			Reason: "existential flatten builds exactly two ForEach legs (nested-cluster leg would drift the seed against post-flattening arity)",
		}
	}
	if gatedFlatten {
		// Key the collision namespace on the BINDING correlations — the names
		// the gated quantifiers actually carry (a later duplicate leg is
		// Q$DUPn, its display alias shared): an existential alias must not
		// collide with either.
		seen := map[string]struct{}{
			strings.ToUpper(sourceBinding(left)):  {},
			strings.ToUpper(sourceBinding(right)): {},
		}
		for _, esq := range f.ExistsSubqueries {
			key := strings.ToUpper(esq.Alias.Name())
			if _, dup := seen[key]; dup {
				gatedFlatten = false
				gateDecision = wedgeGateDecision{
					Arity:  gateDecision.Arity,
					Reason: "existential alias collides with a leg alias (indistinguishable correlations)",
				}
				break
			}
			seen[key] = struct{}{}
		}
	}
	// Record the NARROWED decision — the map is the one truth downstream
	// consumers (the WHERE-conjunct baking arm today, commit 4's enclosure
	// lift) read, and a Gated record over an ANCHORED seed would misroute
	// them. The record always matches the seed actually built below.
	if t.wedgeGate == nil {
		t.wedgeGate = make(map[*logical.LogicalJoin]wedgeGateDecision)
	}
	t.wedgeGate[j] = gateDecision

	// Flatten join + EXISTS into a single SelectExpression
	// with ForEach(left), ForEach(right), and Existential quantifiers.
	// RFC-173 Slice 2 (pre-commit-3 residual): a NON-gated flat select is a
	// name-model merge-absorbing parent, so its ForEach legs are ENCLOSED — a
	// nested join there must not gate ordinal. Legs of a GATED flatten
	// translate FRESH (the translateJoin gated-parent convention: their own
	// inner joins gate independently).
	prevEnclosure := t.inInnerCluster
	t.inInnerCluster = !gatedFlatten
	leftRef := t.translateRef(left)
	if leftRef == nil {
		t.inInnerCluster = prevEnclosure
		return nil
	}
	rightRef := t.translateRef(right)
	t.inInnerCluster = prevEnclosure
	if rightRef == nil {
		return nil
	}

	leftAlias := sourceAlias(left)
	rightAlias := sourceAlias(right)
	// RFC-173 QP-REF-BIND item 1: a GATED flatten's quantifiers and source
	// aliases carry the BINDING correlation (== the alias for every
	// non-duplicate leg; the parser-minted id for a later duplicate) —
	// matching the ordinal seed RC's QOVs and the executor's span windows,
	// exactly as translateJoin's gated binary arm. Without this the dup
	// flatten seeded [A, Q$DUP1] while naming BOTH quantifiers [A, A]; the
	// step-1 NLJ then adapted the second leg's row against the first leg's
	// type and died loudly in the leg adapter. The NAME-MODEL arm keeps the
	// DISPLAY aliases (its anchored RC and merged-row keys are
	// alias-qualified).
	if gatedFlatten {
		leftAlias = legBinding(left)
		rightAlias = legBinding(right)
	} else if leg := mintedBindingLeg(left, right); leg != "" {
		// A minted-binding leg while the flatten narrowed off the gate
		// (arity ≠ 2, existential-alias collision): the name-model flat
		// select keys legs by display alias and would serve silent NULLs
		// for the binding's columns. Decline LOUDLY (correct-or-loud) —
		// "fail toward the name model" is not a safe direction for a
		// duplicate-alias query.
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"duplicate FROM alias: leg %s requires the gated existential flatten; narrowed to the name model (%s)",
			leg, gateDecision.Reason))
		return nil
	}

	leftQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(leftAlias), leftRef,
	)
	rightQ := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier(rightAlias), rightRef,
	)
	quantifiers := []expressions.Quantifier{leftQ, rightQ}

	// Combine join ON predicates + filter WHERE predicates.
	var allPreds []predicates.QueryPredicate
	if j.OnPredicate != nil {
		if qp, ok := j.OnPredicate.(predicates.QueryPredicate); ok {
			allPreds = append(allPreds, qp)
		}
	}
	if f.Predicate != nil {
		if and, ok := f.Predicate.(*predicates.AndPredicate); ok {
			allPreds = append(allPreds, and.SubPredicates...)
		} else {
			allPreds = append(allPreds, f.Predicate)
		}
	}

	// Add EXISTS subqueries as existential quantifiers. RFC-173 S4 commit 4: a
	// minted-binding (duplicate-alias) gated flatten with a LEG-INDEPENDENT EXISTS
	// no longer declines here — the executor's identity-FlatMap pass-through now
	// propagates the gated outer's own positional row (keyed on the outer's
	// ordinal seed via downstreamLegWindows, not the exists inner's probe), so
	// the minted-dup upper (QOV(Q$DUPn)) resolves positionally instead of serving
	// NULLs off a name Datum that never had the binding-keyed column.
	sourceAliases := []string{leftAlias, rightAlias}
	for _, esq := range f.ExistsSubqueries {
		subRef := t.translateSubqueryRef(esq.Plan)
		if subRef == nil {
			return nil
		}
		existQ := expressions.NamedExistentialQuantifier(esq.Alias, subRef)
		quantifiers = append(quantifiers, existQ)
		innerCorrName, joinPred := existsInnerCorrelation(esq)
		if joinPred != nil {
			allPreds = append(allPreds, joinPred)
		}
		sourceAliases = append(sourceAliases, innerCorrName)
	}

	// The RV uses DECLARATION order (design ruling I2: Java assembles the
	// result value in source order regardless of join type).
	//
	// RFC-173 QP-REF-BIND item 2, commit 3: a GATED flatten seeds the baked
	// ordinal RC over its two ForEach legs — existential quantifiers
	// contribute NO columns (Java's model: existentials carry no output) —
	// and bakes the COMBINED predicate list (join ON + WHERE conjuncts + the
	// EXISTS correlation predicates). The baked correlation predicates are
	// what flow into the existential FlatMap's inner plan as FrontierPinned
	// references over the merged outer, where the item-2 commit-2
	// disabled-birth binder binds the outer positionally and the ordinal
	// existential rebase (W4-left machinery) handles the merged references.
	// This retires the W4-left F2 scope note: the ordinal seed here was
	// twice REVERTED because those executor binders did not exist — the 2+1
	// select's correlated-FlatMap path bound name maps and the seed's baked
	// refs died loudly (E1). Commit 2 landed the binders; E2 validated this
	// exact seed end-to-end.
	var resultValue values.Value
	if gatedFlatten {
		var legTypes map[string]bakeLegType
		resultValue, legTypes = t.buildOrdinalJoinResultValue([]clusterLeg{
			clusterLegOf(j.Left, false),
			clusterLegOf(j.Right, false),
		})
		allPreds = bakeGatedJoinPredicates(allPreds, legTypes)
	} else {
		resultValue = t.buildJoinResultValue(j.Left, j.Right, sourceAlias(j.Left), sourceAlias(j.Right))
	}
	if resultValue == nil {
		// A leg's columns are not derivable (only the catalog-free nil-md path;
		// every md-bearing production query anchors — RFC-077 7.6). Untranslatable.
		// Mirrors translateJoin: the opaque-seed fallback was retired, so a nil
		// result value must not flow into the SelectExpression (it would nil-deref
		// downstream, e.g. GetCorrelatedToOfValue).
		return nil
	}
	return expressions.NewSelectExpressionWithJoinType(
		resultValue,
		quantifiers,
		allPreds,
		sourceAliases,
		expressions.JoinInner,
	)
}

// foldAlwaysTrueExists removes POSITIVE WHERE-EXISTS subqueries flagged
// AlwaysTrue (an unconditional-one-row aggregate inner) from a filter, folding
// `EXISTS(inner)` to TRUE: with no existential quantifier built and the EXISTS
// marker already stripped from the predicate by splitNonExistsPredicates, the
// conjunct simply disappears (`P AND TRUE == P`). ONLY positive consumers fold:
// a negated one (`NOT EXISTS`) would be FALSE, not TRUE, so it is kept
// (pre-existing behavior). f.ExistsSubqueries are AND-conjunct EXISTS by the
// same invariant the existential-quantifier lowering relies on. Returns f
// unchanged when nothing folds (no allocation on the common path).
func (t *cascadesTranslator) foldAlwaysTrueExists(f *logical.LogicalFilter) *logical.LogicalFilter {
	if f == nil || len(f.ExistsSubqueries) == 0 {
		return f
	}
	anyAlwaysTrue := false
	for _, esq := range f.ExistsSubqueries {
		if esq.AlwaysTrue {
			anyAlwaysTrue = true
			break
		}
	}
	if !anyAlwaysTrue {
		return f
	}
	negated := map[values.CorrelationIdentifier]struct{}{}
	if f.Predicate != nil {
		predicates.WalkPredicate(f.Predicate, func(p predicates.QueryPredicate) bool {
			if a, ok := predicates.IsNotExistentialPredicate(p); ok {
				negated[a] = struct{}{}
			}
			return true
		})
	}
	// If ANY AlwaysTrue esq is consumed under NOT EXISTS, DECLINE the whole
	// fold. `NOT EXISTS(one-row)` is FALSE, which this positive-only fold does
	// not implement; folding just the sibling positive alias and leaving the
	// (unfixed) negated one would silently change a formerly-rejected /
	// pre-existing shape (e.g. `EXISTS(agg) AND NOT EXISTS(agg)` must be empty,
	// not the broken negated residual). Preserve the base behavior for the whole
	// filter — negated always-true folding is a booked follow-on.
	for _, esq := range f.ExistsSubqueries {
		if _, isNeg := negated[esq.Alias]; esq.AlwaysTrue && isNeg {
			return f
		}
	}
	kept := make([]logical.ExistsSubquery, 0, len(f.ExistsSubqueries))
	foldedAliases := map[values.CorrelationIdentifier]struct{}{}
	for _, esq := range f.ExistsSubqueries {
		if _, isNeg := negated[esq.Alias]; esq.AlwaysTrue && !isNeg {
			foldedAliases[esq.Alias] = struct{}{}
			continue
		}
		kept = append(kept, esq)
	}
	if len(foldedAliases) == 0 {
		return f
	}
	f2 := *f
	f2.ExistsSubqueries = kept
	// Replace each folded esq's positive EXISTS marker with TRUE in the
	// predicate (and collapse `P AND TRUE` -> P). The generic filter path would
	// otherwise choke on a marker whose quantifier was dropped.
	f2.Predicate = stripFoldedExistsMarkers(f.Predicate, foldedAliases)
	return &f2
}

// stripFoldedExistsMarkers rewrites a predicate, replacing each POSITIVE
// ExistentialValuePredicate whose alias was folded (EXISTS -> unconditionally
// TRUE) with a TRUE constant, and dropping `AND TRUE` conjuncts. Only descends
// through AND (folded esqs are AND-conjunct EXISTS).
func stripFoldedExistsMarkers(pred predicates.QueryPredicate, folded map[values.CorrelationIdentifier]struct{}) predicates.QueryPredicate {
	if pred == nil {
		return nil
	}
	if a, ok := predicates.IsExistentialPredicate(pred); ok {
		if _, isFolded := folded[a]; isFolded {
			return predicates.NewConstantPredicate(predicates.TriTrue)
		}
		return pred
	}
	and, ok := pred.(*predicates.AndPredicate)
	if !ok {
		return pred
	}
	newSubs := make([]predicates.QueryPredicate, 0, len(and.SubPredicates))
	for _, sub := range and.SubPredicates {
		ns := stripFoldedExistsMarkers(sub, folded)
		if c, isConst := ns.(*predicates.ConstantPredicate); isConst && c.Value == predicates.TriTrue {
			continue // P AND TRUE == P
		}
		newSubs = append(newSubs, ns)
	}
	switch len(newSubs) {
	case 0:
		return predicates.NewConstantPredicate(predicates.TriTrue)
	case 1:
		return newSubs[0]
	default:
		return &predicates.AndPredicate{SubPredicates: newSubs}
	}
}

// declineNegatedOuterOnlyEsq records a LOUD decline (and reports true) when
// an esq flagged OuterOnlyJoinConjuncts is consumed under a NEGATED
// existential marker in pred. Such an esq's join predicate carries a
// conjunct with no inner-source reference (the Case-1 nested-EXISTS middle
// routes them there — the inside placement does not plan for that
// composition); the semi-join outer-routes it, which is VALID for positive
// polarity (P ∧ ∃(Q) ≡ ∃(P∧Q)) but under NOT EXISTS computes P ∧ ¬∃(Q)
// where ¬∃(P∧Q) is due — silently dropping every ¬P outer row. Positive
// consumers are untouched (their routing is a genuine equivalence).
func (t *cascadesTranslator) declineNegatedOuterOnlyEsq(pred predicates.QueryPredicate, esqs []logical.ExistsSubquery) bool {
	if pred == nil || len(esqs) == 0 {
		return false
	}
	negated := map[values.CorrelationIdentifier]struct{}{}
	predicates.WalkPredicate(pred, func(p predicates.QueryPredicate) bool {
		if a, ok := predicates.IsNotExistentialPredicate(p); ok {
			negated[a] = struct{}{}
		}
		return true
	})
	if len(negated) == 0 {
		return false
	}
	for _, esq := range esqs {
		if _, isNeg := negated[esq.Alias]; isNeg && esq.OuterOnlyJoinConjuncts {
			t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedOperation,
				"NOT EXISTS over a nested-EXISTS subquery with an outer-only conjunct is not supported"))
			return true
		}
	}
	return false
}

// declineNegatedOuterOnlyEsqValue is declineNegatedOuterOnlyEsq's PROJECTED
// twin, and STRICTER: a projected EXISTS consumes the boolean in the RESULT
// VALUE (the synthesized filter's Predicate is nil), where outer-routing the
// flagged conjunct FILTERS THE ROW STREAM — but a projected boolean must
// never filter rows (Java emits the boolean per outer row; the row-drop was
// observed live: 0 rows where Java answers (id,false) pairs). The
// P ∧ ∃(Q) ≡ ∃(P∧Q) equivalence only licenses outer-routing for WHERE
// consumption under positive polarity, so a flagged esq whose ExistsValue is
// referenced by the result value declines in BOTH polarities.
func (t *cascadesTranslator) declineNegatedOuterOnlyEsqValue(v values.Value, esqs []logical.ExistsSubquery) bool {
	if v == nil || len(esqs) == 0 {
		return false
	}
	flagged := map[values.CorrelationIdentifier]struct{}{}
	for _, esq := range esqs {
		if esq.OuterOnlyJoinConjuncts {
			flagged[esq.Alias] = struct{}{}
		}
	}
	if len(flagged) == 0 {
		return false
	}
	hit := false
	values.WalkValue(v, func(node values.Value) bool {
		ev, isExists := node.(*values.ExistsValue)
		if !isExists {
			return true
		}
		if qov, isQOV := ev.Value.(*values.QuantifiedObjectValue); isQOV {
			if _, isFlagged := flagged[qov.Correlation]; isFlagged {
				hit = true
			}
		}
		return !hit
	})
	if hit {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedOperation,
			"a projected EXISTS over a nested-EXISTS subquery with an outer-only conjunct is not supported"))
	}
	return hit
}

// splitNonExistsPredicates extracts the non-EXISTS parts of a predicate
// tree. EXISTS predicates (and NOT EXISTS) are dropped — they're
// represented by the Existential quantifier in the SelectExpression.
// Compound AND predicates are flattened: AND(ExistentialValuePredicate, c.id < 10)
// yields just [c.id < 10].
func splitNonExistsPredicates(pred predicates.QueryPredicate) []predicates.QueryPredicate {
	if pred == nil {
		return nil
	}
	if _, ok := predicates.IsExistentialPredicate(pred); ok {
		return nil
	}
	if _, ok := predicates.IsNotExistentialPredicate(pred); ok {
		return nil
	}
	if and, ok := pred.(*predicates.AndPredicate); ok {
		var result []predicates.QueryPredicate
		for _, sub := range and.SubPredicates {
			result = append(result, splitNonExistsPredicates(sub)...)
		}
		return result
	}
	return []predicates.QueryPredicate{pred}
}

// extractExistsPredicates returns the EXISTS-related predicates that
// splitNonExistsPredicates drops: bare ExistentialValuePredicate or
// NOT(ExistentialValuePredicate). The rule's implementExistentialSelect
// needs these to detect EXISTS vs NOT EXISTS.
func extractExistsPredicates(pred predicates.QueryPredicate) []predicates.QueryPredicate {
	if pred == nil {
		return nil
	}
	if _, ok := predicates.IsExistentialPredicate(pred); ok {
		return []predicates.QueryPredicate{pred}
	}
	if _, ok := predicates.IsNotExistentialPredicate(pred); ok {
		return []predicates.QueryPredicate{pred}
	}
	if and, ok := pred.(*predicates.AndPredicate); ok {
		var result []predicates.QueryPredicate
		for _, sub := range and.SubPredicates {
			result = append(result, extractExistsPredicates(sub)...)
		}
		return result
	}
	return nil
}

func (t *cascadesTranslator) namedQuantifier(alias string, ref *expressions.Reference) expressions.Quantifier {
	if alias != "" {
		return expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier(alias), ref,
		)
	}
	return expressions.ForEachQuantifier(ref)
}

// existsInnerCorrelation registers an existential subquery's inner correlation
// under the existential quantifier's UNIQUE alias (esq.Alias, minted by
// values.UniqueCorrelationIdentifier()) rather than the subquery's SOURCE table
// name (sourceAlias(esq.Plan)). It returns:
//
//   - the source alias string to register in the SelectExpression's
//     GetSourceAliases() (the unique alias name), so the NLJ rule derives the
//     existential INNER correlation from it; and
//   - the join predicate with its inner-leg references rebased from the source
//     alias to the unique alias, so the predicate's QOV correlation MATCHES the
//     FlatMap inner binding (the join-pred filter binds under the same alias).
//
// Java gives every existential quantifier its own unique correlation identity;
// the inner correlation predicate references THAT identity, never the source
// table's name. Since the collision mint, buildCorrelatedExists already births
// a single-table catalog inner under its own unique correlation (the scan
// alias and the join predicate's inner refs carry the minted name), so for the
// minted class this rename is pure identity PLUMBING — it rebases the build-
// time mint onto esq.Alias so the NLJ binding and the predicate agree on ONE
// name. The rename remains load-bearing for the residual single-table shapes
// that reach here under their SQL source name (e.g. the clean-build
// non-correlated path): there the name can equal an outer source alias
// (`... FROM t WHERE id > 1 AND EXISTS (SELECT 1 FROM t ...)`), and without
// the rename the FlatMap would bind both the outer row and the FirstOrDefault
// inner under the SAME correlation (the inner clobbers the outer → NULL
// pass-through row), and an outer-only predicate (`id > 1`, correlated to the
// shared name) would be misclassified as an INNER join predicate and pushed
// below the FOD. Routing the existential inner through the unique alias makes
// outer and inner correlations distinct by construction, so neither the
// binding nor the predicate classification can collide. The source table's
// columns still flow up under their bare names inside the subquery plan; only
// the JOIN-LEVEL correlation identity changes, so field lookups (bm["COL"])
// are unaffected.
func existsInnerCorrelation(esq logical.ExistsSubquery) (string, predicates.QueryPredicate) {
	// The rename is ONLY safe when the inner is a plain single-table scan whose
	// ENTIRE correlation to the parent is captured in esq.JoinPredicate. Two
	// inner shapes carry references to their OWN source alias that the rename
	// cannot reach, so renaming the binding orphans them and the EXISTS goes
	// silently false:
	//
	//   - a JOIN inner emits a MERGED row resolved by qualified leg keys
	//     (T2.ID, T3.T2_ID, …), never a single-alias binding
	//     (executePredicatesFilter: producesMergedRows ⇒ bindAlias=false);
	//     pointing the predicate at a `<uniqueAlias>.*` namespace nothing writes
	//     yields NULL; and
	//   - a NESTED-EXISTS inner (a LogicalFilter carrying its own
	//     ExistsSubqueries) has a nested existential correlation that references
	//     the MIDDLE scan's source alias from INSIDE esq.Plan — not in
	//     esq.JoinPredicate — so the rename leaves it bound to the old alias.
	//
	// Both keep the leg/source-alias routing. The alias-shadow collision the
	// rename fixes only arises for a clean single-table inner (one bare
	// namespace bound under one alias); the merged-row / nested-EXISTS inners
	// route by distinct qualified keys and cannot clobber the outer binding.
	if !existsInnerSafeToRename(esq.Plan) {
		return sourceAlias(esq.Plan), esq.JoinPredicate
	}
	uniqueAlias := esq.Alias
	srcAlias := values.NamedCorrelationIdentifier(sourceAlias(esq.Plan))
	joinPred := esq.JoinPredicate
	if joinPred != nil && srcAlias != uniqueAlias {
		joinPred = predicates.RebasePredicate(joinPred, values.AliasMap{srcAlias: uniqueAlias})
	}
	return uniqueAlias.Name(), joinPred
}

// existsInnerSafeToRename reports whether an existential subquery's plan is a
// clean single-table scan whose only correlation to the parent lives in
// esq.JoinPredicate — the only shape for which renaming the inner correlation to
// the unique existential alias is safe. Returns false for a JOIN (merged-row
// keyed by leg aliases), a CTE/derived-table (its own correlation namespace), or
// a LogicalFilter carrying ExistsSubqueries (a nested EXISTS whose correlation
// references the inner scan's alias from inside the plan). Walks the single-child
// chain the same way sourceAlias does; a plain WHERE filter (no nested EXISTS) is
// transparent.
func existsInnerSafeToRename(op logical.LogicalOperator) bool {
	for cur := op; cur != nil; {
		switch o := cur.(type) {
		case *logical.LogicalScan:
			return true
		case *logical.LogicalJoin:
			return false
		case *logical.LogicalCTE:
			return false
		case *logical.LogicalFilter:
			// A nested EXISTS inside the inner WHERE references the inner scan's
			// own alias from within esq.Plan — the rename can't reach it.
			if len(o.ExistsSubqueries) > 0 {
				return false
			}
			ch := o.Children()
			if len(ch) == 1 {
				cur = ch[0]
				continue
			}
			return false
		default:
			ch := cur.Children()
			if len(ch) == 1 {
				cur = ch[0]
				continue
			}
			return false
		}
	}
	return false
}

func sourceAlias(op logical.LogicalOperator) string {
	for cur := op; cur != nil; {
		switch o := cur.(type) {
		case *logical.LogicalScan:
			if o.Alias != "" {
				return strings.ToUpper(o.Alias)
			}
			return strings.ToUpper(o.Table)
		case *logical.LogicalUnnest:
			if o.Alias != "" {
				return strings.ToUpper(o.Alias)
			}
			if o.AtAlias != "" {
				return strings.ToUpper(o.AtAlias)
			}
			return strings.ToUpper(strings.Join(o.Segments, "."))
		case *logical.LogicalJoin:
			return sourceAlias(o.Right)
		case *logical.LogicalCTE:
			// CTE-wrapped derived tables: the CTE name IS the
			// derived-table alias. Return it directly so the NLJ
			// executor qualifies merged-row keys under the alias
			// the user specified (e.g. "sq1"), not the underlying
			// table name buried inside the CTE body.
			return strings.ToUpper(o.Name)
		default:
			ch := cur.Children()
			if len(ch) == 1 {
				cur = ch[0]
				continue
			}
			return ""
		}
	}
	return ""
}

// sourceBinding is the leg's BINDING correlation name: sourceAlias unless the
// parser minted a duplicate-alias binding id (RFC-173 QP-REF-BIND item 1 —
// LogicalScan/LogicalCTE/LogicalUnnest.Binding, carried from the single mint
// authority assignFromLegBindingIDs). Every correlation / bake-map / window
// key reads THIS; display surfaces keep sourceAlias. Minted ids are
// FOLD-STABLE upper form (`Q$DUPN`) so the existing UPPER-fold lookups treat
// them exactly like aliases; alias-bound legs return sourceAlias's UPPER
// form, so non-duplicate queries are byte-identical to the pre-item-1 keying.
// mintedBindingLeg returns the first FROM-leg source in the given subtrees
// carrying a parser-minted duplicate-alias binding (Scan/CTE/Unnest .Binding
// — set ONLY when a later duplicate leg was renamed; "" everywhere else), or
// "" when none. The name-model join machinery keys its anchored RC and merged
// rows by DISPLAY alias, which cannot represent a minted binding (two
// same-named legs collide last-wins; the resolver's binding-qualified
// references read NULL off the display-keyed row). Callers use this to
// decline LOUDLY at every name-model construction a minted-binding query can
// narrow into — never silent wrong rows (RFC-173 QP-REF-BIND item 1). It does
// NOT descend into existential/scalar subquery plans: those translate their
// own FROM and guard themselves. It DOES descend through CTE/derived bodies
// (Children()): deliberate — a dup buried in a body that reaches a name-model
// parent is the same silent-NULL hazard, and the failure direction of the
// over-approximation is a loud decline, never wrong rows (unlike the
// FindOuterScanTable convention, which must NOT cross body scopes because it
// NAMES a table for the caller's own scope).
func mintedBindingLeg(ops ...logical.LogicalOperator) string {
	for _, op := range ops {
		if op == nil {
			continue
		}
		switch o := op.(type) {
		case *logical.LogicalScan:
			if o.Binding != "" {
				return o.Binding
			}
		case *logical.LogicalUnnest:
			if o.Binding != "" {
				return o.Binding
			}
		case *logical.LogicalCTE:
			if o.Binding != "" {
				return o.Binding
			}
		}
		if b := mintedBindingLeg(op.Children()...); b != "" {
			return b
		}
	}
	return ""
}

func sourceBinding(op logical.LogicalOperator) string {
	for cur := op; cur != nil; {
		switch o := cur.(type) {
		case *logical.LogicalScan:
			if o.Binding != "" {
				return o.Binding
			}
			return sourceAlias(cur)
		case *logical.LogicalUnnest:
			return sourceAlias(cur)
		case *logical.LogicalJoin:
			return sourceBinding(o.Right)
		case *logical.LogicalCTE:
			if o.Binding != "" {
				return o.Binding
			}
			return sourceAlias(cur)
		default:
			ch := cur.Children()
			if len(ch) == 1 {
				cur = ch[0]
				continue
			}
			return ""
		}
	}
	return ""
}

func (t *cascadesTranslator) translateCTE(c *logical.LogicalCTE) expressions.RelationalExpression {
	if c.Recursive {
		return t.translateRecursiveCTE(c)
	}
	body := c.Body
	if len(c.ColumnAliases) > 0 {
		if origCols := extractOutputColumns(body); len(origCols) == len(c.ColumnAliases) {
			body = logical.NewProject(body, origCols, c.ColumnAliases)
		}
	}
	name := strings.ToUpper(c.Name)
	// Save the OUTER binding this registration shadows (nil = unbound): the
	// derived-table alias-carrier reuses cteScope, so a wrapper named like
	// an enclosing WITH-CTE must not clobber it — the wrapper's body reads
	// the outer binding (via the shadow-stack pop at scan resolution), and
	// siblings after this CTE keep resolving the outer name.
	prevBody, hadPrev := t.cteScope[name]
	// Lazy init: unit tests build translators as bare struct literals,
	// bypassing the constructor.
	if t.cteShadowStack == nil {
		t.cteShadowStack = make(map[string][]logical.LogicalOperator)
	}
	if hadPrev {
		t.cteShadowStack[name] = append(t.cteShadowStack[name], prevBody)
	} else {
		t.cteShadowStack[name] = append(t.cteShadowStack[name], nil)
	}
	t.cteScope[name] = body
	result := t.translateOp(c.Main)
	st := t.cteShadowStack[name]
	t.cteShadowStack[name] = st[:len(st)-1]
	if hadPrev {
		t.cteScope[name] = prevBody
	} else {
		delete(t.cteScope, name)
	}
	return result
}

// inCTEDefiningScope runs fn with `key` resolving as it does in the DEFINING
// scope of the cteScope body being expanded: the shadow stack pops one level
// (the body's own references to `key` then hit the shadowed outer binding
// when one exists, the real table otherwise) and is restored afterwards,
// together with the body's registration. Every cteScope body expansion must
// go through this — a bare delete-while-recursing loses the outer binding
// (`WITH c AS (…) … FROM (SELECT * FROM c) c` read the real table instead of
// the WITH body), and a bare re-register loops forever on self-reference.
func (t *cascadesTranslator) inCTEDefiningScope(key string, body logical.LogicalOperator, fn func()) {
	st := t.cteShadowStack[key]
	var outer logical.LogicalOperator
	popped := false
	if n := len(st); n > 0 {
		outer = st[n-1]
		t.cteShadowStack[key] = st[:n-1]
		popped = true
	}
	if outer != nil {
		t.cteScope[key] = outer
	} else {
		delete(t.cteScope, key)
	}
	fn()
	t.cteScope[key] = body
	if popped {
		// Restoring the pre-pop slice is safe despite sharing its backing
		// array with any nested push during fn: a nested registration that
		// appended into the freed slot wrote the SAME enclosing binding this
		// restore reinstates (scope chains share their prefix), so the write
		// is idempotent by construction.
		t.cteShadowStack[key] = st
	}
}

func extractOutputColumns(op logical.LogicalOperator) []string {
	switch o := op.(type) {
	case *logical.LogicalProject:
		return o.Projections
	case *logical.LogicalAggregate:
		var cols []string
		cols = append(cols, o.GroupKeys...)
		for i, agg := range o.Aggregates {
			if i < len(o.Aliases) && o.Aliases[i] != "" {
				cols = append(cols, o.Aliases[i])
			} else {
				cols = append(cols, agg)
			}
		}
		return cols
	case *logical.LogicalDistinct:
		return extractOutputColumns(o.Input)
	case *logical.LogicalSort:
		return extractOutputColumns(o.Input)
	case *logical.LogicalLimit:
		return extractOutputColumns(o.Input)
	case *logical.LogicalFilter:
		return extractOutputColumns(o.Input)
	}
	return nil
}

// translateRecursiveCTE translates a WITH RECURSIVE CTE into a
// RecursiveUnionExpression. Mirrors Java's
// QueryVisitor.handleRecursiveNamedQuery:
//  1. Partition the UNION ALL body into seed (non-recursive) and
//     recursive (self-referencing) branches.
//  2. Translate the seed branch normally.
//  3. Translate the recursive branch with the CTE self-reference
//     resolving to a TempTableScanExpression.
//  4. Wrap both legs in TempTableInsertExpression.
//  5. Create RecursiveUnionExpression with scan/insert aliases.
//  6. Translate the Main query with the CTE name resolving to the
//     RecursiveUnionExpression.
func (t *cascadesTranslator) translateRecursiveCTE(c *logical.LogicalCTE) expressions.RelationalExpression {
	cteName := strings.ToUpper(c.Name)

	// RFC-173 S4 (R1): the recursive-CTE body is ORDINALIZED. The old blanket
	// `t.inInnerCluster = true` forced every body join name-model; lifting it
	// (surgically — this site only; inInnerCluster is a genuine enclosure flag
	// at the non-recursive unnest/existential/w4b sites, where a broad lift
	// breaks multi-source unnest) lets the gate's own arms classify each body
	// sub-join: a plain inner/outer body GATES and takes the ordinal branch
	// (buildOrdinalJoinResultValue → a positional row aligned by position to the
	// CTE output — Java's seed-typed frontier, RecursiveUnionExpression.mergeValues);
	// an unnest/existential body stays name-model via the gate's :106/:112/:143
	// arms and keeps the dotted-split normalization arm (which survives to
	// convergence). normalizeLegToOutputColumns detects the ordinalized body
	// (any baked-ordinal projected value) and re-emits POSITIONALLY; the temp
	// table stays a positional QueryResult buffer keyed by outCols in
	// NAME-METADATA only, the coexisting bare-outCols Datum riding alongside for
	// UNION-DISTINCT dedup and Main name resolution until Slice 4 retires Datum.

	// The body must be a UNION ALL or UNION DISTINCT.
	union, ok := c.Body.(*logical.LogicalUnion)
	if !ok || len(union.Inputs) < 2 {
		return nil
	}

	// Partition branches into seed (no self-reference) and recursive
	// (references the CTE name).
	var seedBranches, recursiveBranches []logical.LogicalOperator
	for _, branch := range union.Inputs {
		if logicalOpReferencesCTE(branch, cteName) {
			recursiveBranches = append(recursiveBranches, branch)
		} else {
			seedBranches = append(seedBranches, branch)
		}
	}
	if len(seedBranches) == 0 || len(recursiveBranches) == 0 {
		return nil
	}

	scanAlias := values.NamedCorrelationIdentifier(cteName + "forScan")
	insertAlias := values.NamedCorrelationIdentifier(cteName + "forInsert")

	// Translate the seed leg. Multiple seed branches become a union.
	var seedExpr expressions.RelationalExpression
	if len(seedBranches) == 1 {
		seedExpr = t.translateOp(seedBranches[0])
	} else {
		seedExpr = t.translateUnion(&logical.LogicalUnion{Inputs: seedBranches, Distinct: false})
	}
	if seedExpr == nil {
		return nil
	}

	// outCols: the CTE's OUTPUT column names — the schema every reference to
	// the CTE resolves against (the recursive branch's self-reference predicates
	// AND the Main query). Standard SQL: the seed's projection defines these
	// names, overridden by an explicit column-alias list `WITH RECURSIVE d(a, b)`.
	//
	// The temp table is keyed under these OUTPUT names. Identifier resolution
	// keeps OUTPUT names (the source-name reverse-map is retired, RFC-173), so a
	// recursive predicate `b.id = a.up` reads field UP — the CTE's OUTPUT column —
	// not the seed's source PARENT. The temp table (which the self-reference scans)
	// must therefore be keyed by UP for the join predicate to match. Both legs are
	// normalized to emit these names; nothing renames the temp table afterwards.
	seedSrc := extractOuterProjectionColumns(seedBranches[0])
	seedOut := make([]string, len(seedSrc))
	for i, n := range extractOutputProjectionNames(seedBranches[0]) {
		seedOut[i] = strings.ToUpper(n)
	}
	// A projection-less seed (`SELECT * FROM t`) exposes no projection columns,
	// which silently DROPPED an explicit CTE column-alias list
	// (`WITH RECURSIVE cte(a, b) AS (SELECT * FROM t UNION ALL …)`): the alias
	// gate below never length-matched, the temp table stayed keyed by the base
	// columns, and a recursive reference to `a` was a silent NULL under the name
	// model / a loud OrdinalResolutionError under the ordinal model (review P2 +
	// reviewer's pre-existing corner, RFC-173 Slice 1 gauntlet). Derive the seed
	// schema from the operator's output — table columns for a scan
	// (derivedOutputColumns) — so the alias list applies and the seed normalizes
	// onto it.
	if len(seedSrc) == 0 && len(c.ColumnAliases) > 0 {
		if fields := t.derivedOutputColumns(seedBranches[0]); len(fields) > 0 {
			seedSrc = make([]string, len(fields))
			seedOut = make([]string, len(fields))
			for i, f := range fields {
				seedSrc[i] = f.Name
				seedOut[i] = f.Name
			}
		}
	}
	outCols := seedOut
	if len(c.ColumnAliases) > 0 && len(c.ColumnAliases) == len(outCols) {
		outCols = c.ColumnAliases // already upper-cased
	}

	// Normalize the SEED to the OUTPUT schema when a column-alias list renames it
	// beyond the seed's own output names. Skipped when they already coincide (no
	// alias list, or the seed uses AS matching the list), keeping the common
	// recursive-CTE plan unchanged.
	if len(seedSrc) > 0 && len(outCols) == len(seedSrc) && !equalFoldSlices(outCols, seedOut) {
		// len==1 guard: a MULTI-branch seed translates to a union leg (name-keyed,
		// never positional); recursiveBodyIsPositional inspects only the first
		// logical branch, so gate it on a single branch to avoid a false-positive
		// signal (benign — a union row degrades to a name read — but wrong node).
		seedExpr = normalizeLegToOutputColumns(seedExpr, seedSrc, outCols, len(seedBranches) == 1 && t.recursiveBodyIsPositional(seedBranches[0]))
	}

	// Wrap seed in TempTableInsert.
	seedRef := expressions.InitialOf(seedExpr)
	seedInsert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(seedRef), insertAlias, false,
	)

	// Translate the recursive leg with the CTE self-reference resolving
	// to a TempTableScanExpression(scanAlias). The self-reference temp table
	// carries the CTE's OUTPUT column names, so a join leg referencing the CTE
	// inside the recursive branch (e.g. FROM descendants AS a, t AS b) anchors on
	// those columns (RFC-077 7.6).
	t.cteExprScope[cteName] = expressions.NewTempTableScanExpression(scanAlias)
	t.cteColumnsScope[cteName] = fieldsFromColumnNames(outCols)
	var recursiveExpr expressions.RelationalExpression
	if len(recursiveBranches) == 1 {
		recursiveExpr = t.translateOp(recursiveBranches[0])
	} else {
		recursiveExpr = t.translateUnion(&logical.LogicalUnion{Inputs: recursiveBranches, Distinct: false})
	}
	delete(t.cteExprScope, cteName)
	delete(t.cteColumnsScope, cteName)
	if recursiveExpr == nil {
		return nil
	}

	// Normalize the recursive leg's output columns to the CTE's OUTPUT schema
	// (outCols). In standard SQL, the CTE's output column names are defined by
	// the seed (and any column-alias list). The recursive branch often uses
	// qualified column references (e.g. SELECT b.id, b.parent) which produce
	// datum keys like "B.ID"; without this normalization the outer query (and
	// DFS recursion) can't find the expected columns, yielding NULL for every row.
	//
	// The temp table is keyed under outCols so it agrees with the recursive
	// predicates, which read the CTE's OUTPUT columns (the source-name reverse-map
	// is retired, RFC-173). recursiveRemapValues never persists a qualified key:
	// each read is FieldValue{Field: <bare>, Child: QOV(<qualifier>)} — it reads
	// the qualified datum key ("B.ID") while projectionColumnName returns the BARE
	// field, so the qualified key (which would collide with the next recursion
	// level's same-qualified join side and stall the recursion one level early) is
	// never copied in. executeProjection also emits the value under the bare body
	// column; when that differs from the OUTPUT name it is an INERT extra key
	// (unqualified, re-qualified under the scan alias at the next level).
	recCols := extractOuterProjectionColumns(recursiveBranches[0])
	if len(outCols) > 0 && len(recCols) > 0 && len(outCols) == len(recCols) {
		recursiveExpr = normalizeLegToOutputColumns(recursiveExpr, recCols, outCols, len(recursiveBranches) == 1 && t.recursiveBodyIsPositional(recursiveBranches[0]))
	}

	// Wrap recursive leg in TempTableInsert.
	recursiveRef := expressions.InitialOf(recursiveExpr)
	recursiveInsert := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(recursiveRef), insertAlias, false,
	)

	// Build RecursiveUnionExpression.
	seedInsertRef := expressions.InitialOf(seedInsert)
	recursiveInsertRef := expressions.InitialOf(recursiveInsert)
	strategy := expressions.TraversalAny
	switch c.TraversalOrder {
	case logical.TraversalPreOrder:
		strategy = expressions.TraversalPreorder
	case logical.TraversalPostOrder:
		strategy = expressions.TraversalPostorder
	}
	var recUnion *expressions.RecursiveUnionExpression
	if union.Distinct {
		recUnion = expressions.NewRecursiveUnionExpressionDistinct(
			expressions.ForEachQuantifier(seedInsertRef),
			expressions.ForEachQuantifier(recursiveInsertRef),
			scanAlias, insertAlias,
			strategy,
		)
	} else {
		recUnion = expressions.NewRecursiveUnionExpression(
			expressions.ForEachQuantifier(seedInsertRef),
			expressions.ForEachQuantifier(recursiveInsertRef),
			scanAlias, insertAlias,
			strategy,
		)
	}

	// No outward rename projection is needed: the temp table (and therefore the
	// recursive union's output) is already keyed under the CTE's OUTPUT column
	// names (outCols) — the column-alias list, when present, was baked into
	// outCols and applied to BOTH legs before the temp-table inserts.
	cteResult := recUnion

	// Register the result so the Main query's scan of the CTE name resolves to
	// it. The OUTWARD column schema is outCols — so a CTE reference used as a JOIN
	// LEG in the Main query anchors instead of falling back to the opaque merge
	// (RFC-077 7.6).
	t.cteExprScope[cteName] = cteResult
	t.cteColumnsScope[cteName] = fieldsFromColumnNames(outCols)
	result := t.translateOp(c.Main)
	delete(t.cteExprScope, cteName)
	delete(t.cteColumnsScope, cteName)
	return result
}

// fieldsFromColumnNames builds an anchored-RC leg's column schema from a list of
// output column NAMES (upper-cased), typed UnknownType (only names are
// load-bearing for name-based field resolution). Returns nil for an empty list,
// marking the leg's columns as not derivable (RFC-077 7.6).
func fieldsFromColumnNames(names []string) []values.Field {
	if len(names) == 0 {
		return nil
	}
	fields := make([]values.Field, len(names))
	for i, n := range names {
		fields[i] = values.Field{Name: strings.ToUpper(n), FieldType: values.UnknownType, Ordinal: i}
	}
	return fields
}

// extractOuterProjectionColumns returns the SOURCE column names from the
// outermost LogicalProject in a logical operator tree. Returns nil if
// no LogicalProject is found. Used by translateRecursiveCTE to detect
// schema mismatches between seed and recursive branches. These are the
// READ side (what the branch's projection pulls from its input); the
// OUTPUT names a reference resolves against are extractOutputProjectionNames.
func extractOuterProjectionColumns(op logical.LogicalOperator) []string {
	if p, ok := op.(*logical.LogicalProject); ok {
		return p.Projections
	}
	return nil
}

// extractOutputProjectionNames returns the OUTPUT column names of the outermost
// LogicalProject — the alias when present, else the source column name. These
// are the names a reference to the branch resolves against (Java resolves to the
// output attribute verbatim). Returns nil if no LogicalProject is found.
func extractOutputProjectionNames(op logical.LogicalOperator) []string {
	p, ok := op.(*logical.LogicalProject)
	if !ok {
		return nil
	}
	out := make([]string, len(p.Projections))
	for i := range p.Projections {
		if i < len(p.Aliases) && p.Aliases[i] != "" {
			out[i] = p.Aliases[i]
		} else {
			out[i] = p.Projections[i]
		}
	}
	return out
}

// normalizeLegToOutputColumns wraps a recursive-CTE leg with the normalization
// projection that re-emits its output columns under the CTE's OUTPUT names
// (outCols). The wrap READS the leg's output by its PHYSICAL column names — the
// Datum/positional keys the executor actually writes (legPhysicalOutputNames) —
// not the LOGICAL names the logical plan carries. For a bare or qualified column
// the two coincide ("ID", "T.ID"); for a COMPUTED column they differ (logical
// "N + 1" vs physical "(N + 1)", values.ProjectionColumnName), and reading by
// the logical name was a silent NULL under the tolerant name model (recursion
// stalled one level early: `recursive_cte_depth_counter` returned 2 instead of
// Java's 10 — a pre-existing silent-wrong) and a loud OrdinalResolutionError
// under the ordinal model. Found by the RFC-173 §5 dual-window differential on
// its first run.
//
// The WRAP form (not an alias override on the leg's own projection) is
// deliberate: for a qualified-join body the wrap is what STRIPS the qualified
// datum keys ("T.ID") before the temp-table insert — recursiveRemapValues reads
// the qualified key but projects the BARE output name — preserving the "never
// persist a qualified key" invariant AND the temp-table row size the RFC-130
// memory budget is calibrated to (an alias override leaks the qualified keys
// into the temp rows: TestFDB_RFC130_RecursiveCTE_NoDoubleCharge regressed).
func normalizeLegToOutputColumns(leg expressions.RelationalExpression, legCols, outCols []string, positional bool) expressions.RelationalExpression {
	physNames, verbatimField, fromProjection := legPhysicalOutputNames(leg, legCols)
	return expressions.NewLogicalProjectionExpressionWithAliases(
		recursiveRemapValues(physNames, verbatimField, fromProjection, positional),
		append([]string(nil), outCols...),
		expressions.ForEachQuantifier(expressions.InitialOf(leg)),
	)
}

// recursiveBodyIsPositional reports whether a recursive-CTE branch's output row
// is POSITIONAL (RFC-173 S4 R1): its FROM is a GATED (ordinal) join, so the
// branch emits a positional flat concat rather than name-keyed dotted datums,
// and the leg normalization must read by ordinal (recursiveRemapValues'
// positional arm). Peels the branch's top projection + transparent filters to
// the FROM join and reads the gate decision recorded during translation (baking
// is post-translation, so the projected values aren't yet baked here — the gate
// decision is the authoritative translation-time signal). A scan/aggregate/union
// body (no direct gated join) stays name-model and keeps the dotted-split arm.
func (t *cascadesTranslator) recursiveBodyIsPositional(op logical.LogicalOperator) bool {
	for {
		switch o := op.(type) {
		case *logical.LogicalProject:
			op = o.Input
		case *logical.LogicalFilter:
			op = o.Input
		case *logical.LogicalJoin:
			d, found := t.wedgeGate[o]
			return found && d.Gated
		default:
			return false
		}
	}
}

// legPhysicalOutputNames returns a recursive-CTE leg's PHYSICAL output column
// names — the keys its top projection actually emits — plus whether they came
// from the leg's own top PROJECTION (read i ↔ emitted slot i by construction,
// enabling ordinal reads in the wrap). Falls back to the LOGICAL names when
// the leg's top expression is not a projection (bare-column shapes, where the
// two coincide; a computed column under a non-projection top would loud-error
// under ordinal resolution, which the §5 dual-window differential watches for).
//
// The names come from the shared values.OutputColumnName authority — the SAME
// rule executeProjection uses for the emitted positional row's slot names
// (upper-cased ALIAS when the projected column carries one, else the
// values.ProjectionColumnName rendering; RFC-173 §4 Slice 1: posNames is
// alias-preferring) — so an aliased leg on the positional frontier
// (`SELECT v + 1 AS v`, or a seed `SELECT id AS x` renamed by an explicit CTE
// column list) is re-read by the alias the executor actually writes. Reading
// the source/computed rendering there is a GetByName miss and the ordinal
// model is loud on a miss by design: OrdinalResolutionError on a valid
// recursive CTE (review P2, Slice-1 follow-up). The name-keyed Datum stores the
// alias key alongside the source key, so the alias also resolves on the
// off-frontier (join-body) name path — one read name valid on both models.
// The second return is the per-column classification of the NAME'S
// PROVENANCE: true when the emitted name is a plain *values.FieldValue's
// Field string VERBATIM (unaliased — an identifier, possibly a
// genuinely-dotted lazy "B.ID", by construction), so a dot in it IS a
// qualifier. False for everything else: an EXPRESSION rendering
// ("(B.ID + 1)", "1.5") whose dots are never qualifiers, AND an
// ALIAS-derived name — an alias is ONE identifier, never qualifier syntax,
// and a QUOTED alias may legally contain a dot (`AS "A.B"` — splitting it
// manufactured QOV("A"), the same garbage-correlation class; review
// finding, provenance not value type). recursiveRemapValues picks the read
// arm from this classification, never from the string's shape (an earlier
// string-grammar discriminator misread a float literal's rendering as
// IDENT.IDENT). nil classification = the logical-name fallback path, where
// every name is a column identifier by construction.
func legPhysicalOutputNames(leg expressions.RelationalExpression, logicalCols []string) ([]string, []bool, bool) {
	lp, ok := leg.(*expressions.LogicalProjectionExpression)
	if !ok || len(lp.GetProjectedValues()) != len(logicalCols) {
		return logicalCols, nil, false
	}
	aliases := lp.GetAliases()
	out := make([]string, len(logicalCols))
	verbatimField := make([]bool, len(logicalCols))
	for i, v := range lp.GetProjectedValues() {
		alias := ""
		if i < len(aliases) {
			alias = aliases[i]
		}
		out[i] = values.OutputColumnName(v, alias)
		_, isField := v.(*values.FieldValue)
		verbatimField[i] = alias == "" && isField
	}
	return out, verbatimField, true
}

// recursiveRemapValues builds the read-side Values for a recursive-CTE leg's
// normalization projection. Each source column becomes a FieldValue: a dotted
// reference (a join body's "B.ID") reads the QUALIFIED datum key via a
// QuantifiedObjectValue child while projectionColumnName returns the BARE field,
// so a qualified key is never persisted into the temp table (a qualified key
// would collide with the next recursion level's same-qualified join side and
// stall the recursion one level early). A bare column reads the bare key.
//
// When the names came from the leg's own top projection (ordinalReads), a bare
// read ALSO carries a plan-time-resolved ordinal accessor (read i ↔ the leg's
// emitted positional slot i by construction — Java's FieldValue.ofOrdinalNumber
// model, which RFC-173 §4 Slice 1 makes authoritative on the frontier). The
// ordinal read is what makes DUPLICATE output aliases sound: `SELECT a+1 AS x,
// b+1 AS x` emits two slots both named X, and every name-based resolution
// collapses them (positional GetByName is first-match; the name-keyed Datum is
// last-wins) — a silent second-column-copies-first wrong result (review P2 on
// PR #446). By ordinal each read hits its own slot; the field NAME is kept for
// the off-frontier Datum path, where a merged join row is name-keyed (and a
// projection over a join is never on the positional frontier, so the dotted
// QOV reads below never need ordinals). Non-projection legs (ordinalReads
// false: scan-top star seeds, multi-branch unions) keep pure name reads —
// their columns are table columns, which cannot be duplicate-named.
// The dotted split fires from the NAME-PROVENANCE classification, never from
// the string's shape: verbatimField[i] marks a name that is an UNALIASED
// plain *values.FieldValue's Field string verbatim — an identifier by
// construction, so a dot in it IS a qualifier. Everything else never splits:
// a computed rendering ("(B.ID + 1)", a float literal's "1.5") whose dots
// are not qualifiers, and an ALIAS-derived name (one identifier by
// definition; a QUOTED alias may legally contain a dot). A string-grammar
// discriminator here misread those and manufactured garbage correlations
// like QOV("(B") / QOV("1") / QOV("A") — the S4 kill-list first-dot-split
// hazard (review findings, three classes). verbatimField nil = the
// logical-name fallback path, identifiers by construction.
//
// RESIDUAL (pre-existing, dies with the name machinery in S4): within a
// verbatim Field string itself, "is this dot a qualifier?" stays ambiguous —
// the lazy name-model FieldValue spells both the qualified "B.ID" and a
// QUOTED identifier containing a dot ("A-B.C", reachable only through a
// quoted CTE column alias re-projected in a recursive leg; proto field names
// cannot carry dots) in the same string. Master's unconditional first-dot
// split broke the quoted class identically; disambiguating needs the lazy
// dotted-Field constructor to mark qualifier provenance — the name-model
// machinery S4 deletes.
func recursiveRemapValues(cols []string, verbatimField []bool, ordinalReads, positional bool) []values.Value {
	out := make([]values.Value, len(cols))
	for i, c := range cols {
		cu := strings.ToUpper(c)
		if positional {
			// RFC-173 S4 (R1): an ORDINALIZED recursive body emits a positional
			// row; read EVERY column by ordinal (slot i) — the dotted-split name
			// arm below mis-reads a positional row (QOV(<qualifier>) has no
			// namespace on it → OrdinalResolutionError). This node is an UNPINNED
			// baked node (values.go:411) — dual-window by design: the ORDINAL
			// window reads slot i (authoritative), the §5 name-model window
			// (OracleBakedNameFallback) falls back to Datum[nameReadRootKey], i.e.
			// Datum[Resolved.Root().Field]. So the two names DECOUPLE:
			//   - Field = BARE column → ProjectionColumnName emits the temp-row
			//     key BARE (no dotted key: a "C.ID" key would DOUBLE the row and
			//     bust the RFC-130 budget);
			//   - Resolved.Root() = the FULL physName cu ("C.ID") → the name-window
			//     fallback reads the body's QUALIFIED output Datum key, matching
			//     the pre-lift dotted-split's read (the body's projection keys its
			//     Datum by the qualified ProjectionColumnName).
			bare := cu
			if dot := strings.IndexByte(cu, '.'); dot >= 0 {
				bare = cu[dot+1:]
			}
			out[i] = &values.FieldValue{
				Field:    bare,
				Typ:      values.UnknownType,
				Resolved: values.NewFieldPathOfSingle(cu, i, false),
			}
			continue
		}
		identName := verbatimField == nil || verbatimField[i]
		if dot := strings.IndexByte(cu, '.'); dot >= 0 && identName {
			out[i] = &values.FieldValue{
				Field: cu[dot+1:],
				Typ:   values.UnknownType,
				Child: values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(cu[:dot])),
			}
		} else if ordinalReads {
			out[i] = values.NewFieldValueWithResolvedOrdinal(cu, i, values.UnknownType)
		} else {
			out[i] = &values.FieldValue{Field: cu, Typ: values.UnknownType}
		}
	}
	return out
}

// equalFoldSlices reports whether two string slices are element-wise equal
// under ASCII case folding.
func equalFoldSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !strings.EqualFold(a[i], b[i]) {
			return false
		}
	}
	return true
}

// logicalOpReferencesCTE walks a LogicalOperator tree and reports
// whether any LogicalScan references the given CTE name (case-
// insensitive). Used to partition UNION ALL branches into seed vs
// recursive legs.
func logicalOpReferencesCTE(op logical.LogicalOperator, cteName string) bool {
	if op == nil {
		return false
	}
	if scan, ok := op.(*logical.LogicalScan); ok {
		if strings.EqualFold(scan.Table, cteName) {
			return true
		}
	}
	for _, child := range op.Children() {
		if logicalOpReferencesCTE(child, cteName) {
			return true
		}
	}
	return false
}

func (t *cascadesTranslator) translateInsert(ins *logical.LogicalInsert) expressions.RelationalExpression {
	var innerRef *expressions.Reference
	switch {
	case ins.Source != nil:
		// INSERT … SELECT: the source plan produces the rows.
		innerRef = t.translateRef(ins.Source)
		if innerRef == nil {
			return nil
		}
	case ins.ValuesArray != nil:
		// INSERT … VALUES: explode the literal array of records into a
		// stream, matching Java (ExplodeExpression over the array Value).
		explode := expressions.NewExplodeExpression(ins.ValuesArray)
		innerRef = expressions.InitialOf(explode)
	}
	var q expressions.Quantifier
	if innerRef != nil {
		q = expressions.ForEachQuantifier(innerRef)
	}
	return expressions.NewInsertExpression(q, ins.Table, values.UnknownType)
}

func (t *cascadesTranslator) translateUpdate(upd *logical.LogicalUpdate) expressions.RelationalExpression {
	var innerRef *expressions.Reference
	if upd.Input != nil {
		innerRef = t.translateRef(upd.Input)
		if innerRef == nil {
			return nil
		}
	}
	transforms := make([]expressions.UpdateTransform, len(upd.Sets))
	for i, a := range upd.Sets {
		// Prefer the catalog-resolved RHS Value (evaluated per row by the
		// executor); fall back to the canonical text only when the builder
		// ran without catalog resolution (then the executor cannot evaluate
		// it — but this keeps the structure for explain/legacy paths).
		newVal := a.Value
		if newVal == nil {
			newVal = &values.ConstantValue{Value: a.Expr, Typ: values.UnknownType}
		}
		transforms[i] = expressions.UpdateTransform{
			FieldPath: strings.ToUpper(a.Column),
			NewValue:  newVal,
		}
	}
	var q expressions.Quantifier
	if innerRef != nil {
		q = expressions.ForEachQuantifier(innerRef)
	}
	return expressions.NewUpdateExpression(q, upd.Target, transforms)
}

func (t *cascadesTranslator) translateDelete(del *logical.LogicalDelete) expressions.RelationalExpression {
	var innerRef *expressions.Reference
	if del.Input != nil {
		innerRef = t.translateRef(del.Input)
		if innerRef == nil {
			return nil
		}
	}
	var q expressions.Quantifier
	if innerRef != nil {
		q = expressions.ForEachQuantifier(innerRef)
	}
	return expressions.NewDeleteExpression(q, del.Target)
}

// FindUnsupportedFunction walks the logical plan tree and returns the
// name of the first ScalarFunctionValue that isn't in the supported set.
// Returns "" if all functions are supported.
func FindUnsupportedFunction(op logical.LogicalOperator) string {
	if op == nil {
		return ""
	}
	if proj, ok := op.(*logical.LogicalProject); ok {
		for _, v := range proj.ProjectedValues {
			if fn := findUnsafeFuncInValue(v); fn != "" {
				return fn
			}
		}
	}
	if f, ok := op.(*logical.LogicalFilter); ok && f.Predicate != nil {
		if fn := findUnsafeFuncInPredicate(f.Predicate); fn != "" {
			return fn
		}
	}
	if u, ok := op.(*logical.LogicalUpdate); ok {
		// UPDATE SET RHS expressions must reject unsupported functions
		// just like projections, matching the naive path.
		for _, a := range u.Sets {
			if a.Value != nil {
				if fn := findUnsafeFuncInValue(a.Value); fn != "" {
					return fn
				}
			}
		}
	}
	for _, child := range op.Children() {
		if fn := FindUnsupportedFunction(child); fn != "" {
			return fn
		}
	}
	return ""
}

func findUnsafeFuncInValue(v values.Value) string {
	if v == nil {
		return ""
	}
	var found string
	values.WalkValue(v, func(node values.Value) bool {
		if sf, ok := node.(*values.ScalarFunctionValue); ok {
			if !values.IsCascadesSafeScalarFunction(sf.FuncName) {
				found = sf.FuncName
				return false
			}
		}
		return true
	})
	return found
}

func findUnsafeFuncInPredicate(p predicates.QueryPredicate) string {
	var found string
	predicates.WalkPredicate(p, func(qp predicates.QueryPredicate) bool {
		switch pred := qp.(type) {
		case *predicates.ComparisonPredicate:
			if fn := findUnsafeFuncInValue(pred.Operand); fn != "" {
				found = fn
				return false
			}
			if pred.Comparison.Operand != nil {
				if fn := findUnsafeFuncInValue(pred.Comparison.Operand); fn != "" {
					found = fn
					return false
				}
			}
		case *predicates.ValuePredicate:
			if fn := findUnsafeFuncInValue(pred.Value); fn != "" {
				found = fn
				return false
			}
		}
		return true
	})
	return found
}
