package query

import (
	"errors"
	"fmt"
	"slices"
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
// Returns the root Reference suitable for passing to Planner.PlanWithContext().
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
	cteColumnsScope map[string][]values.Field
	// recursiveCTEConsumerRows records the seed-declared and common exact rows
	// for aliases of a recursive CTE while its main query is translated. The
	// logical resolver necessarily runs before the recursive fixed point is
	// typed, so consumer Values can still carry the narrower seed row (most
	// visibly, a NOT NULL seed literal whose recursive expression is nullable).
	// The scoped bridge is consulted only when the physical input publishes the
	// exact common row; unrelated same-named windows and joined carriers decline.
	recursiveCTEConsumerRows map[values.CorrelationIdentifier][]recursiveCTEConsumerRow
	scalarSubqueries         []ScalarSubqueryPlan
	// translateErr records the FIRST translation error that carries a
	// specific SQL error code the bare nil-ref signal cannot (RFC-142:
	// AT-ordinality on a non-array source → ErrCodeWrongObjectType). Set once
	// (first writer wins) so the original cause surfaces; the caller reads it
	// when ref is nil and reports it instead of the generic "could not plan".
	translateErr error

	// inInnerCluster is the enclosure flag: true while
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
	// projection whose chain carries intervening Sort/Limit operators. It is a
	// DEFENSE-IN-DEPTH TRIPWIRE, currently UNREACHABLE at its sole consult site
	// (the B1 arm): the translateProject gate only widens the fold for UN-chained
	// shapes (its `len(chain) == 0` condition is the LIVE ORDER BY/LIMIT
	// scope-out), and a chained fold implies a projected EXISTS, which the arm
	// declines before consulting this flag. It guards a FUTURE gate widening:
	// a chained fold reaching the wrap would re-apply the sort/cleanup above it
	// with unrebased leg-qualified emissions — resolvable over a name-model
	// output row, unbound (silent NULL) over the wrap's positional output.
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
	// OUTER of an EXISTS composition (translateUnnestExistsFilter). This class
	// ordinalizes: the ordinal-seed gate is taken when the outer
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
	// unnestExistentialGatherOK admits the under-EXISTS unnest to the GATHERED
	// ordinal cluster (the multi-alias box path) instead of the name-model
	// binary seed. Computed PRE-translation, metadata-only in
	// translateUnnestExistsFilter (a decline is never poisoned by translation
	// side state), and read only by the gathered-cluster admission check in
	// translateUnnestJoin. Admission: a
	// non-INNER box left with Kind ∈ {LEFT,RIGHT} at verdict ∈ {None,Bakeable}
	// or Kind==FULL at Bakeable only (FULL+None rides the certified binary
	// seed); single esq for LEFT/RIGHT/FULL boxes (a multi-esq box wrap strands, so
	// it stays name-model), any esq count for the INNER cluster; no inner/outer scope
	// collision; the esq a simple
	// leg-correlation (buried-outer-only and whole-row-read esq shapes decline
	// to name-model — they keep today's rows, never a loud plan failure). Reset
	// after the unnest lowering like unnestUnderExistential.
	unnestExistentialGatherOK bool
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
	// unnestBoxLegConjunct is the three-state verdict for a WHERE conjunct
	// referencing a MULTI-ALIAS box leg under a lateral unnest (`(a LEFT b),
	// a.arr AS x WHERE a.col = V`): boxConjNone (no such conjunct),
	// boxConjBakeable (a NON-EXISTS conjunct whose every box-leg reference
	// resolves in the box seed's buried windows — the gathered ordinal path
	// ADMITS the shape and the WHERE-merge arm bakes the conjunct over the
	// gather's RECORDED legTypes), or boxConjUnbakeable (an EXISTS-path
	// conjunct, a subquery-carrying conjunct, or an unresolvable reference —
	// the gather declines and the shape keeps the name-model plan, correct via
	// qualified keys). Bakeability is computed PRE-translation (metadata-only)
	// so a decline is never poisoned by translation side state. Set by the
	// non-EXISTS filter-over-unnest merge and the EXISTS path
	// (translateUnnestExistsFilter — always Unbakeable this slice); read by the
	// gather's box arm (Unbakeable-only decline) and unnestExistsSeedSafe
	// (any non-None state declines the BINARY seed — it has no merge window
	// for the conjunct in either verdict). Scoped like unnestUnderExistential.
	unnestBoxLegConjunct boxConjVerdict
	// unnestGatherBoxLegTypes records, per unnest-join node, the legTypes map
	// the gathered BOX-outer seed was built with (translateGatheredUnnestCluster's
	// non-INNER arm). The WHERE-merge arm consumes it: the seed⟺merge
	// ONE-AUTHORITY LAW — the conjunct must bake over the EXACT map the seed
	// used (the box-as-one-leg buried windows, bakeCorr = the $BOX quantifier),
	// never a re-derived map (gatedJoinLegTypes would decompose the box into
	// top-level legs keyed by aliases the gathered select does not bind). A
	// carried record is a reachability FACT where re-derivation is a
	// reachability argument.
	unnestGatherBoxLegTypes map[*logical.LogicalJoin]map[string]bakeLegType
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
	// wedgeGate records the ordinal-wedge gate decision per translateJoin seed —
	// consumed by the ordinal seed, pinned by tests. Lazily initialized so
	// hand-built test translators need no constructor change.
	wedgeGate map[*logical.LogicalJoin]wedgeGateDecision
}

type recursiveCTEConsumerRow struct {
	declaration *values.RecordType
	common      *values.RecordType
}

// setTranslateErr records a translation error (first writer wins) so a
// specific SQL error code survives to the caller. RFC-142.
func (t *cascadesTranslator) setTranslateErr(err error) {
	if t.translateErr == nil {
		t.translateErr = err
	}
}

func (t *cascadesTranslator) exactSelectWithJoinType(
	result values.Value,
	quantifiers []expressions.Quantifier,
	queryPredicates []predicates.QueryPredicate,
	sourceAliases []string,
	joinType expressions.JoinType,
) expressions.RelationalExpression {
	selectExpr, err := expressions.NewSelectExpressionWithJoinType(
		result, quantifiers, queryPredicates, sourceAliases, joinType)
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"select has no exact result row: %v", err))
		return nil
	}
	return selectExpr
}

func (t *cascadesTranslator) exactSelectWithAliases(
	result values.Value,
	quantifiers []expressions.Quantifier,
	queryPredicates []predicates.QueryPredicate,
	sourceAliases []string,
) expressions.RelationalExpression {
	selectExpr, err := expressions.NewSelectExpressionWithAliases(
		result, quantifiers, queryPredicates, sourceAliases)
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"select has no exact result row: %v", err))
		return nil
	}
	return selectExpr
}

func (t *cascadesTranslator) exactFilter(
	queryPredicates []predicates.QueryPredicate,
	inner expressions.Quantifier,
) expressions.RelationalExpression {
	filter, err := expressions.NewLogicalFilterExpression(queryPredicates, inner)
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"filter has no exact flowed result row: %v", err))
		return nil
	}
	return filter
}

func (t *cascadesTranslator) exactProjectionWithOutputSchema(
	projected []values.Value,
	aliases []string,
	aliasMinted []bool,
	aliasSources []values.ProjectionAliasSource,
	outputNames []string,
	inner expressions.Quantifier,
) expressions.RelationalExpression {
	if outputNames != nil {
		// A scalar leg is represented by its whole QOV. When the SQL item names
		// a different column within that leg (UNNEST AT is the canonical case),
		// record the SQL name as a machinery alias as well as in the frozen
		// schema. That makes the semantic name visible to memo identity instead
		// of folding alpha-renamable internal binder spellings into the hash.
		derived, derivedErr := values.ProjectionResultValue(projected, aliases)
		if derivedErr == nil && len(derived.Fields) == len(outputNames) {
			for i := range projected {
				alias := ""
				if i < len(aliases) {
					alias = aliases[i]
				}
				if alias != "" || !values.QuantifierFlowsAScalarRow(projected[i]) ||
					derived.Fields[i].Name == outputNames[i] {
					continue
				}
				if len(aliases) < len(projected) {
					grown := make([]string, len(projected))
					copy(grown, aliases)
					aliases = grown
				} else {
					aliases = slices.Clone(aliases)
				}
				if len(aliasMinted) < len(projected) {
					grown := make([]bool, len(projected))
					copy(grown, aliasMinted)
					aliasMinted = grown
				} else {
					aliasMinted = slices.Clone(aliasMinted)
				}
				if len(aliasSources) < len(projected) {
					grown := make([]values.ProjectionAliasSource, len(projected))
					copy(grown, aliasSources)
					aliasSources = grown
				} else {
					aliasSources = slices.Clone(aliasSources)
				}
				aliases[i] = outputNames[i]
				aliasMinted[i] = true
				aliasSources[i] = projectionAliasSourceFromValue(projected[i])
			}
		}
	}
	projection, err := expressions.NewLogicalProjectionExpressionWithOutputSchema(
		projected, aliases, aliasMinted, outputNames, inner)
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"projection has no exact result row: %v", err))
		return nil
	}
	projection, err = projection.WithAliasSources(aliasSources)
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"projection has invalid structured alias sources: %v", err))
		return nil
	}
	return projection
}

// projectionAliasSourceFromValue captures a source from a structured Value
// tree at the instant translator machinery mints a new alias. Later physical
// Values are not consulted because their root may correctly be `_current`.
func projectionAliasSourceFromValue(v values.Value) values.ProjectionAliasSource {
	if qov, ok := values.AsQuantifiedObjectValue(v); ok {
		return values.NewProjectionAliasSource(qov.Correlation())
	}
	if fv, ok := values.AsFieldValue(v); ok {
		if qov, qovOK := values.AsQuantifiedObjectValue(fv.ChildValue()); qovOK {
			return values.NewProjectionAliasSource(qov.Correlation())
		}
	}
	return values.ProjectionAliasSource{}
}

func exactLogicalProjectionOutputNames(p *logical.LogicalProject, projected []values.Value) ([]string, error) {
	if p == nil || len(projected) != len(p.Projections) {
		return nil, fmt.Errorf("logical projection has %d labels for %d translated slots", len(p.Projections), len(projected))
	}
	fields := make([]values.RecordConstructorField, len(projected))
	for i, projectedValue := range projected {
		alias := ""
		if i < len(p.Aliases) {
			alias = p.Aliases[i]
		}
		name := values.OutputColumnName(projectedValue, alias)
		if alias == "" {
			// A COLUMN REFERENCE takes the DISPLAY name. The dotted rendering
			// (`A.W.X`) is an internal slot key that disambiguates two members of
			// one struct root inside a projection; it is not a name any scope
			// outside this projection knows. A derived table's columns ARE such a
			// scope: `(SELECT A.B, C AS Q, W.X FROM …) AS u` registers U(B, Q, X),
			// because that is what `u.x` and `WHERE b < 8` resolve against, and
			// Java agrees — its plan for this query reads
			// `MAP (_.B AS B, _.C AS Q, _.W.X AS X)`.
			//
			// Publishing the dotted key here made the two authorities disagree
			// about the SAME row: the scope minted U as RECORD(B,Q,X) while the
			// plan flowed RECORD(B,Q,A.W.X). Nothing compared them as long as
			// every U-rooted value happened to be rewritten away before execution,
			// so the disagreement sat latent and surfaced only once the producer
			// bridge stopped resolving unowned roots by name — as a runtime
			// `edge lookup U: read as RECORD(B:INT,Q:DOUBLE,X:INT), declared
			// RECORD(B:INT,Q:DOUBLE,A.W.X:INT)` on valid SQL.
			//
			// This is the same rule extractOutputProjectionNames applies to a
			// recursive CTE's output columns, for the same reason and after the
			// same symptom.
			if _, isReference := values.AsFieldValue(projectedValue); isReference {
				name = values.DisplayColumnName(projectedValue, "")
			}
			// An exact scalar leg is represented by its whole QOV. Its Value
			// display name identifies the leg (VAL/ARR1), not necessarily the
			// SQL item projected from it (UNNEST AT "AT"). Only the captured
			// parse-tree reference may override that name: punctuation in the
			// rendered expression cannot distinguish a qualified A.B from the
			// one-segment quoted identifier "A.B".
			if values.QuantifierFlowsAScalarRow(projectedValue) {
				if ref := projectionRefAt(p, i); ref.Present {
					// splitColumnRef already applied SQL identifier semantics:
					// unquoted names are folded, while quoted names retain their
					// authored case. Folding again here changes a quoted scalar
					// UNNEST output label ("val" -> VAL) even though the projection
					// reference is the output-name authority.
					name = ref.Bare
				}
			}
		}
		if name == "" {
			name = values.OrdinalFieldName(i)
		}
		fields[i] = values.RecordConstructorField{Name: name, Value: projectedValue}
	}
	resultValue := values.NewRecordConstructorValue(fields...)
	names := make([]string, len(resultValue.Fields))
	for i := range resultValue.Fields {
		names[i] = resultValue.Fields[i].Name
	}
	return names, nil
}

func (t *cascadesTranslator) exactProjectionForLogicalProject(
	projected []values.Value,
	p *logical.LogicalProject,
	inner expressions.Quantifier,
) expressions.RelationalExpression {
	outputNames, err := exactLogicalProjectionOutputNames(p, projected)
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"projection has no exact logical output schema: %v", err))
		return nil
	}
	projection := t.exactProjectionWithOutputSchema(
		projected, p.Aliases, p.AliasMinted, p.AliasSources, outputNames, inner)
	typed, ok := projection.(*expressions.LogicalProjectionExpression)
	if !ok || typed == nil {
		return projection
	}

	// A named WITH-CTE remains registered while its Main query is translated.
	// Its authored qualifier is a load-bearing LOGICAL discriminator: without
	// C.ID, a projection over the CTE can coalesce with an isomorphic derived
	// projection and later select a child carrying a different exact row. It is
	// not, however, the SQL result label — unaliased `SELECT C.ID` publishes the
	// bare leaf ID. Keep those two contracts separate: outputNames above shape
	// the emitted row, while authoredNames below participate only in memo
	// identity. Resolve against p.Input as well as cteScope so a real table alias
	// shadowing an unused same-named CTE is not misclassified.
	authoredNames := slices.Clone(outputNames)
	hasAuthoredOverride := false
	for i := range authoredNames {
		if i < len(p.Aliases) && p.Aliases[i] != "" {
			continue
		}
		ref := projectionRefAt(p, i)
		if !ref.Present || !ref.Qualified {
			continue
		}
		table := findOuterScanTable(p.Input, ref.Qualifier)
		if table == "" || !t.outerSourceIsCTE(table) {
			continue
		}
		authoredNames[i] = strings.ToUpper(ref.Qualifier) + "." + ref.Bare
		hasAuthoredOverride = authoredNames[i] != outputNames[i] || hasAuthoredOverride
	}
	if !hasAuthoredOverride {
		return typed
	}
	withIdentity, identityErr := typed.WithAuthoredOutputIdentity(authoredNames)
	if identityErr != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"projection has no exact authored output identity: %v", identityErr))
		return nil
	}
	return withIdentity
}

func (t *cascadesTranslator) exactSort(
	sortKeys []expressions.SortKey,
	inner expressions.Quantifier,
) expressions.RelationalExpression {
	sortExpr, err := expressions.NewLogicalSortExpression(sortKeys, inner)
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"sort has no exact flowed result row: %v", err))
		return nil
	}
	return sortExpr
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
	fields := make([]values.Field, 0, protoFields.Len()+1)
	for i := 0; i < protoFields.Len(); i++ {
		fd := protoFields.Get(i)
		fields = append(fields, values.Field{
			Name:      values.FieldNameForProtoField(fd),
			FieldType: FieldTypeForFD(fd),
			Ordinal:   i,
		})
	}
	// The __ROW_VERSION pseudo-field extends every planner-facing base-table
	// layout when the metadata stores row versions and the descriptor does
	// not declare a REAL field of that name (Java:
	// RecordMetaData.getPlannerType → Type.Record.addPseudoFields,
	// RecordMetaData.java:732-739). The runtime row carries the matching
	// trailing slot (executor.FromStoredRecord), so a plan-time ordinal bound
	// here reads the version bytes at run time.
	if t.md.IsStoreRecordVersions() &&
		protoFields.ByName(protoreflect.Name(values.PseudoFieldRowVersion)) == nil {
		fields = append(fields, values.Field{
			Name:      values.PseudoFieldRowVersion,
			FieldType: values.NullableVersion,
			Ordinal:   protoFields.Len(),
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

// TargetTypeForFD is the DML TARGET type of a column: the type a row
// constructor is pushed down against, Java's `targetField.getFieldType()`
// in ExpressionVisitor.parseRecordField (ExpressionVisitor.java:969). Unlike
// FieldTypeForFD (below) it descends: a struct column states its
// values.RecordType (named after the declared struct, fields in descriptor
// order), an array column its values.ArrayType with a real element type.
//
// The two are separate on purpose and the separation is temporary in the
// design, not in intent: FieldTypeForFD types the FLOWED scan row, where
// collapsing structs and arrays to UnknownType is load-bearing today (the
// anchored-leg column types, index-on-source, derived unnest all read that
// collapse). RFC-204 §4.4/§4.5 unify them by making the flowed row carry the
// nested types; until the query surface and metadata pipeline can consume
// that, widening FieldTypeForFD would change plan-time typing for every
// query rather than for DML alone.
func TargetTypeForFD(fd protoreflect.FieldDescriptor) values.Type {
	if list, wrapped, ok := values.EffectiveListField(fd); ok {
		// A NULLABLE array is stored wrapped; the ELEMENT type comes from
		// the wrapper's repeated field either way, so both shapes state the
		// same SQL type. Nullability follows the storage shape: a flat
		// repeated field is the NOT NULL array.
		//
		// The element is typed by TargetElementType, NOT by recursing into
		// TargetTypeForFD: the repeated field IS its own effective list
		// field, so recursing would descend forever.
		return values.NewArrayType(wrapped, TargetElementType(list))
	}
	return TargetElementType(fd)
}

// TargetElementType types what a field descriptor's VALUE is, with
// repetition already accounted for by the caller: applied to an array
// column's repeated field it yields the ELEMENT type, applied to a scalar or
// struct field it yields that field's type. Struct fields recurse through
// TargetTypeForFD, because a struct's fields are slots (an array field
// inside a struct is an array), not elements.
func TargetElementType(fd protoreflect.FieldDescriptor) values.Type {
	if fd.Kind() == protoreflect.MessageKind && !fd.IsMap() {
		if msg := fd.Message(); msg != nil &&
			string(msg.FullName()) != functions.UUIDProtoMessageName &&
			!values.IsWrappedArrayDescriptor(msg) {
			fields := make([]values.Field, msg.Fields().Len())
			for i := range fields {
				sub := msg.Fields().Get(i)
				fields[i] = values.Field{
					Name:      string(sub.Name()),
					FieldType: TargetTypeForFD(sub),
					Ordinal:   i,
				}
			}
			return values.NewRecordType(string(msg.Name()), true, fields)
		}
	}
	return scalarTypeForKind(fd)
}

// FieldTypeForFD maps a protoreflect.FieldDescriptor to a values.Type, mirroring
// jdbcTypeNameForFD (pkg/relational/core/embedded/select_helpers.go). Repeated/map
// and non-UUID message fields collapse to values.UnknownType — 7.6 doesn't model
// nested/array element types for the anchored leg columns. Columns are nullable
// (the flowed leg row doesn't carry per-column NOT NULL constraints).
//
// Delegates to values.FieldTypeForProtoField — see that function for why
// exactly one copy of this mapping may exist. The scan leaf typed here and
// the sargable match candidate's layout (executor.PositionalTypeForDescriptor)
// describe the same stored columns and feed the same planner decisions, so
// they must not be able to disagree.
func FieldTypeForFD(fd protoreflect.FieldDescriptor) values.Type {
	return values.FieldTypeForProtoField(fd)
}

// scalarTypeForKind maps a field descriptor's proto KIND to the SQL scalar
// type, ignoring repetition. Shared by FieldTypeForFD (which collapses
// repeated fields before asking) and TargetElementType (which asks about an
// array's element, where the repetition belongs to the array, not the type).
// It is the SAME mapping FieldTypeForFD uses, entered one step earlier —
// values owns both halves so the two can never drift apart.
func scalarTypeForKind(fd protoreflect.FieldDescriptor) values.Type {
	return values.ScalarTypeForProtoKind(fd)
}

// legColumns derives the OUTPUT columns of a logical sub-plan as the field set
// its row carries when consumed as a join leg (RFC-077 7.6 Option B): a leg
// that is itself a join exposes already-qualified (dotted) names that a parent
// propagates verbatim (the nested-join rule, inlined at the LogicalJoin arm —
// the retired name-model producer's exact dotted naming, kept as a direct
// derivation).
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
	case *logical.LogicalInlineValues:
		exact, err := ExactLogicalResultType(o, t.md)
		if err != nil {
			return nil
		}
		row, ok := exact.(*values.RecordType)
		if !ok {
			return nil
		}
		return append([]values.Field(nil), row.Fields...)
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
				// A star-admitted unnest body normalizes to the bare projection
				// of its boundary labels at translateScan; the boundary schema
				// here is those SAME labels (one predicate, all consumers).
				if starCols, star := t.derivedBodyStarOrdinalLeg(body); star {
					cols = starCols
					return
				}
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
		leftAlias := sourceAlias(left)
		rightAlias := sourceAlias(right)
		if leftAlias == "" || rightAlias == "" {
			return nil
		}
		// A join leg exposes ONLY its already-qualified (DOTTED) columns to a parent
		// — the SOURCE-ACCURATE per-table forms (O.ID, C.PRICE, …): a bare column
		// qualifies as UPPER(ALIAS).UPPER(COL); an already-dotted column (a nested
		// join leg's exposed name) propagates VERBATIM with no re-qualification —
		// the retired anchored-RC producer's exact dotted naming rule
		// (NewAnchoredJoinRecord), kept as a direct derivation. Bare names must NOT
		// propagate: a parent re-qualifies a propagated bare under
		// sourceAlias(join)=right-leg, and a name from the right leg then collides
		// with its verbatim dotted key (NewRecordConstructorValue would suffix it
		// "_2" — a spurious key the opaque merge never produces). A buried column is
		// referenced via its dotted form after PartitionSelectRule rebasing, never
		// bare. (RFC-077 7.6 — the unique-bare
		// concern is pinned by TestFDB_NestedJoinUnqualifiedProjection.)
		var fields []values.Field
		for _, leg := range []struct {
			alias string
			cols  []values.Field
		}{{leftAlias, leftCols}, {rightAlias, rightCols}} {
			prefix := strings.ToUpper(leg.alias) + "."
			for _, c := range leg.cols {
				name := strings.ToUpper(c.Name)
				if !strings.Contains(c.Name, ".") {
					name = prefix + name
				}
				// Phase D: the rename is name-only — the child leg's
				// FLOWED type rides through (typed at the scan base from
				// the proto descriptors).
				fields = append(fields, values.Field{Name: name, FieldType: c.FieldType, Ordinal: len(fields)})
			}
		}
		return fields
	case *logical.LogicalProject:
		if len(o.Projections) == 0 {
			return nil
		}
		// A machinery projection can be positional: aggregate/output-strip
		// projections deliberately leave ProjectedValues nil and state their
		// source slots through AggregateOutputOrdinals or InputOrdinals.  Those
		// nil values do not mean that the output type is unknown.  Ask the same
		// exact whole-object authority used by derivedOutputColumns before
		// falling back to an individual projected Value.  Correlated-scalar
		// seeds consume this path after materialising a grouped/global aggregate;
		// laundering its one exact slot to UNKNOWN makes the ordinal seed decline
		// even though the projection has a complete positional contract.
		var exactFields []values.Field
		if exactType, err := ExactLogicalResultType(o, t.md); err == nil {
			if recordType, ok := exactType.(*values.RecordType); ok && len(recordType.Fields) == len(o.Projections) {
				exactFields = recordType.Fields
			}
		}
		fields := make([]values.Field, len(o.Projections))
		for i := range o.Projections {
			name := o.Projections[i]
			if i < len(o.Aliases) && o.Aliases[i] != "" {
				name = o.Aliases[i]
			}
			// Phase D: an exact positional output or resolved projection carries
			// its own type; only a genuinely unresolved projection stays Unknown.
			ft := values.Type(values.UnknownType)
			if len(exactFields) == len(o.Projections) {
				ft = exactFields[i].FieldType
			} else if i < len(o.ProjectedValues) && o.ProjectedValues[i] != nil {
				if vt := o.ProjectedValues[i].Type(); vt != nil {
					ft = vt
				}
			}
			fields[i] = values.Field{Name: name, FieldType: ft, Ordinal: i}
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
		return t.aggregateOutputColumns(o)
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
			// A column-alias list RENAMES the body's output; it does not erase
			// what those columns ARE. Carry the body's own field types under the
			// new names when the widths agree — an UnknownType here makes the
			// leg inexact, which is enough for the ordinalization gate to
			// decline the whole join.
			fields := make([]values.Field, len(o.ColumnAliases))
			for i, name := range o.ColumnAliases {
				fields[i] = values.Field{Name: name, FieldType: values.UnknownType, Ordinal: i}
			}
			bodyFields := t.derivedOutputColumns(o.Body)
			if len(bodyFields) == 0 {
				if starCols, star := t.derivedBodyStarOrdinalLeg(o.Body); star {
					bodyFields = starCols
				}
			}
			if len(bodyFields) == len(fields) {
				for i := range fields {
					if bodyFields[i].FieldType != nil {
						fields[i].FieldType = bodyFields[i].FieldType
					}
				}
			}
			return fields
		}
		// A star-admitted unnest body (the derived-table twin `FROM (SELECT *
		// FROM t, t.arr AS x) AS s`) normalizes to the bare projection of its
		// boundary labels when the registered body translates (translateScan);
		// its leg schema is those labels.
		if starCols, star := t.derivedBodyStarOrdinalLeg(o.Body); star {
			return starCols
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
		// Prefer the projection's exact whole-object authority. In particular,
		// machinery projections may carry InputOrdinals with intentionally nil
		// ProjectedValues; deriving their fields from the input row keeps a
		// derived/CTE unnest collection executable instead of laundering it back
		// to UNKNOWN at this boundary.
		var exactFields []values.Field
		if exactType, err := ExactLogicalResultType(o, t.md); err == nil {
			if recordType, ok := exactType.(*values.RecordType); ok && len(recordType.Fields) == len(o.Projections) {
				exactFields = recordType.Fields
			}
		}
		fields := make([]values.Field, len(o.Projections))
		for i := range o.Projections {
			name := o.Projections[i]
			if i < len(o.Aliases) && o.Aliases[i] != "" {
				name = o.Aliases[i]
			} else if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
				// A qualified-but-unaliased passthrough (`SELECT t.arr FROM t`)
				// flows under the BARE column name — the resolver emits the
				// verbatim OUTPUT attribute (col.Id.Name(), qualifier-free), so
				// the runtime slot is keyed bare. Mirrors projectionOutputNames
				// (the class-3 derived-unnest authority); keeping the dotted
				// spelling here mis-keys the boundary layout and silently declines
				// the qualified-passthrough unnest case.
				name = name[dot+1:]
			}
			fieldType := values.Type(values.UnknownType)
			if len(exactFields) == len(o.Projections) {
				fieldType = exactFields[i].FieldType
			} else if i < len(o.ProjectedValues) && o.ProjectedValues[i] != nil && o.ProjectedValues[i].Type() != nil {
				// A partially built name-only projection may not yet have a complete
				// result type. Preserve exact per-slot authority where it exists;
				// unresolved slots remain UNKNOWN and are rejected at executable
				// ingress rather than guessed.
				fieldType = o.ProjectedValues[i].Type()
			}
			fields[i] = values.Field{Name: name, FieldType: fieldType, Ordinal: i}
		}
		return fields
	case *logical.LogicalAggregate:
		return t.aggregateOutputColumns(o)
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
	case *logical.LogicalInlineValues:
		return t.legColumns(o)
	case *logical.LogicalScan:
		return t.legColumns(o)
	case *logical.LogicalJoin:
		return t.legColumns(o)
	case *logical.LogicalCTE:
		// A derived-table / CTE reference used as a FROM source flows its
		// BODY's output columns, renamed by an explicit column-alias list
		// (`… AS d(a, b)`). An unnest over such an outer bakes its collection
		// against this layout.
		cols := t.derivedOutputColumns(o.Body)
		if len(o.ColumnAliases) == len(cols) {
			// Copy before renaming. legColumns hands back SHARED slices on two
			// arms — a pre-translated CTE's schema comes straight out of
			// cteColumnsScope, and a nested CTE body returns whatever its own
			// derivation returned — so renaming in place rewrites the schema the
			// translator will hand to the NEXT reader of that CTE. The rename is
			// this reference's alias list, not the definition's.
			//
			// ordinal_seed.go's legColumns caller already defends against the same
			// aliasing ("Copy-on-wrap: legColumns may hand back shared slices");
			// this is the arm that did not.
			renamed := make([]values.Field, len(cols))
			copy(renamed, cols)
			for i := range renamed {
				// Verbatim, and this is the SAME rule cteBoundRowType applies
				// to the same alias list. Two sites doing one job must not
				// spell it two ways: a fold here republished the CTE's columns
				// under names the row it wraps does not carry.
				renamed[i].Name = o.ColumnAliases[i]
			}
			return renamed
		}
		// No alias list, so nothing is rewritten and the callee's slice is handed
		// straight back — which is the whole reason the arm above copies. Every
		// caller of derivedOutputColumns treats the result as READ-ONLY; a writer
		// added here must copy first, because legColumns' pre-translated-CTE arm
		// returns cteColumnsScope's own slice.
		return cols
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
// reports those branches' true output names). Every SQL-built aggregate now has
// an exact final Project, so it reaches the first arm regardless of qualified or
// constant aggregate labels. The LogicalAggregate arm remains defense for
// directly constructed/legacy trees that do not carry that public contract.
// Mirrors derivedOutputColumns's recursion through row-shape-preserving
// wrappers; an unknown shape is conservatively not normalizable.
func (t *cascadesTranslator) unionBranchNormalizable(op logical.LogicalOperator) bool {
	switch o := op.(type) {
	case *logical.LogicalProject, *logical.LogicalJoin:
		return true
	case *logical.LogicalInlineValues:
		return true
	case *logical.LogicalScan:
		// A scan may be a CTE/derived-table reference (translateScan resolves it from
		// the CTE body, not a real table). A real-table scan is remappable, but a
		// CTE-reference scan is only remappable if its BODY is. SQL aggregate
		// bodies are Projects; a directly constructed bare aggregate is checked
		// by the defensive arm below. Resolve cteScope and recurse, mirroring legColumns (remove-while-
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
		// Direct-tree bare aggregate branch (SQL builders always add Project).
		if len(o.Calls) < 1 {
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
		// GROUPED direct tree: a bare grouped aggregate can plan as AggregateIndex / MultiIntersection,
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
// The parse-tree-derived Calls are the signal (RFC-180 F-2): AggregateOperands is nil for
// many shapes (e.g. SUM(col)) depending on the build path, and text scanning of the
// canonical rendering is retired.
func aggregateNamesStableForUnion(a *logical.LogicalAggregate) bool {
	if len(a.Calls) == 0 || a.HasDistinctAggregate {
		return false
	}
	for i := range a.Calls {
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
		// Structured classification (RFC-180 F-2): the parse-tree-derived
		// call info replaces text scanning of the canonical rendering. A
		// dotted operand text is conservatively rejected (qualified
		// rendering; a delimited identifier containing a dot only costs
		// the optimization, never correctness).
		call := a.Calls[i]
		if call.Star {
			continue // COUNT(*)
		}
		if call.Distinct || !call.BareColumn || call.Qualified {
			// Qualified is parse-tree truth for a bare column — a delimited
			// identifier with a literal dot stays normalizable.
			return false // qualified / expression / distinct operand → name diverges
		}
	}
	return true
}

// aggregateOutputColumns returns a LogicalAggregate's output column schema:
// the GROUP BY keys (bare column names, upper-cased) followed by each
// aggregate's output name (alias when present, else the aggregate text).
// Mirrors extractOutputColumns(LogicalAggregate). Phase D: group keys
// carry the INPUT leg's flowed type for the keyed column; aggregate
// calls carry Java's result type (values.JavaAggregateResultCode over
// the operand column's flowed code — COUNT→LONG, AVG→DOUBLE,
// SUM/MIN/MAX→operand). A key/operand the input layout cannot type
// stays Unknown (lazy until resolution). Returns nil if the aggregate
// has no output columns.
func (t *cascadesTranslator) aggregateOutputColumns(a *logical.LogicalAggregate) []values.Field {
	inputCols := t.legColumns(a.Input)
	typeOf := func(bare string) values.Type {
		up := strings.ToUpper(bare)
		var found values.Type
		matched := false
		for _, c := range inputCols {
			if strings.ToUpper(c.Name) != up || c.FieldType == nil {
				continue
			}
			// A name carried by MULTIPLE input columns with CONFLICTING
			// type codes (two legs of a join both projecting `v`, one
			// INT one STRING) is ambiguous by name alone — stay Unknown
			// rather than attach the first leg's type to what may be the
			// other leg's column. Correct-or-unknown: wrong metadata is
			// the N-F4 class; Unknown is the honest lazy answer until
			// binding-aware typing (Phase D2+) keys by leg, not name.
			// `matched` is tracked separately from the type so a FIRST
			// match that is itself UnknownType still counts as seen — a
			// later typed duplicate must not overwrite it (the name stays
			// indeterminate).
			if matched && found.Code() != c.FieldType.Code() {
				return values.UnknownType
			}
			found = c.FieldType
			matched = true
		}
		if !matched {
			return values.UnknownType
		}
		return found
	}
	var fields []values.Field
	for _, k := range a.GroupKeys {
		kt := values.Type(values.UnknownType)
		if k.Bare != "" {
			kt = typeOf(k.Bare)
		}
		// VERBATIM, like every other output-naming authority. This function is
		// what legColumns and derivedOutputColumns delegate to for an aggregate
		// leg, so a fold here makes their verbatim contract false for exactly
		// one arm — and it folded the ALIAS sixty lines below a comment saying
		// an alias must not be folded. Two claims about one alias in one file.
		fields = append(fields, values.Field{Name: k.Display, FieldType: kt, Ordinal: len(fields)})
	}
	for i, call := range a.Calls {
		name := call.CanonicalName()
		if i < len(a.Aliases) && a.Aliases[i] != "" {
			name = a.Aliases[i]
		}
		ft := values.Type(values.UnknownType)
		operandCode := values.TypeCodeUnknown
		if call.Star {
			operandCode = values.TypeCodeLong // COUNT(*): operand irrelevant
		} else if call.BareColumn && call.Operand != "" {
			if ot := typeOf(call.Operand); ot != nil {
				operandCode = ot.Code()
			}
		}
		if code, ok := values.JavaAggregateResultCode(strings.ToUpper(call.Func), operandCode); ok {
			// ALL aggregate outputs are declared NULLABLE — Java's
			// CountValue.getResultType() is Type.primitiveType(LONG)
			// (nullable default) even though COUNT is never null per
			// group: a null-extended outer leg can surface a NULL count,
			// so a not-null declaration would license wrong
			// simplifications.
			ft = values.NewPrimitiveType(code, true)
		}
		fields = append(fields, values.Field{Name: name, FieldType: ft, Ordinal: len(fields)})
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// normalizeAggOutputName folds a reference / output name to the
// WHITESPACE-insensitive key the SELECT-list-over-GROUP-BY ordinal match
// compares on: a projection references an aggregate by its canonical text
// (`SUM(UNITS * PRICE)`, spaces intact from the parse tree) while the naming
// authority renders the operand space-stripped (`SUM(UNITS*PRICE)`) — the two
// must match on the same normalized key or the ordinal bake silently misses.
//
// It is no longer CASE-insensitive, and the old doc said it was. The fold went
// with RFC-237: both sides now carry the operand's declared spelling, so a
// case-insensitive key would conflate two aggregates that differ only in a
// case-sensitive token — the collision `COUNT(CASE WHEN s='x' …)` and `…'X'…`
// produce, which is a wrong ANSWER rather than a wrong name.
func normalizeAggOutputName(s string) string {
	return strings.ReplaceAll(s, " ", "")
}

// bindPostAggregateValue resolves a structural logical draft against the real
// quantifier that owns a GroupBy output row. The logical builder records native
// ordinals but cannot publish FieldValues because no such quantifier exists at
// that layer. This is the first authority that has both halves: aggregate
// identity and the physical owner QOV.
func bindPostAggregateValue(
	v values.Value,
	agg *logical.LogicalAggregate,
	outputQOV values.Value,
) (values.Value, error) {
	if v == nil || agg == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"post-aggregate value has no complete native output contract")
	}
	if _, ok := values.AsQuantifiedObjectValue(outputQOV); !ok {
		return nil, api.NewError(api.ErrCodeInternalError,
			"post-aggregate value has no exact output quantifier")
	}
	var bindErr error
	bound := values.Replace(v, func(node values.Value) values.Value {
		if bindErr != nil {
			return node
		}
		replacement, err := bindPostAggregateNode(node, agg, outputQOV)
		if err != nil {
			bindErr = err
			return node
		}
		return replacement
	})
	if bindErr != nil {
		return nil, bindErr
	}
	return bound, nil
}

// bindPostAggregateNode binds one node only. Callers that already own a Value
// traversal (notably predicates.ReplaceValues) use this form so replacement
// FieldValues are not recursively interpreted as fresh source references.
func bindPostAggregateNode(
	node values.Value,
	agg *logical.LogicalAggregate,
	outputQOV values.Value,
) (values.Value, error) {
	if node == nil || agg == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"post-aggregate value has no complete native output contract")
	}
	outputOwner, ok := values.AsQuantifiedObjectValue(outputQOV)
	if !ok {
		return nil, api.NewError(api.ErrCodeInternalError,
			"post-aggregate value has no exact output quantifier")
	}

	ordinal := -1
	if av, isAggregate := node.(*values.AggregateValue); isAggregate {
		ordinal = aggregateValueNativeOrdinal(av, agg)
		if ordinal < 0 {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"post-aggregate expression could not bind an aggregate call to the native output row")
		}
	} else {
		first, matches := -1, 0
		for i, key := range agg.GroupKeys {
			if key.Value == nil {
				continue
			}
			if values.ValuesStructurallyEqual(node, key.Value) ||
				values.SemanticEqualsUnderAliasMap(node, key.Value, values.EmptyAliasMap()) {
				if matches == 0 {
					first = i
				}
				matches++
			}
		}
		if matches > 1 {
			return nil, api.NewErrorf(api.ErrCodeAmbiguousColumn,
				"Ambiguous columns for %s", values.ColumnNameValue(agg.GroupKeys[first].Value))
		}
		if matches == 1 {
			ordinal = first
		} else if field, isField := values.AsFieldValue(node); isField {
			if owner, hasOwner := values.AsQuantifiedObjectValue(field.ChildValue()); hasOwner &&
				owner.Correlation() == outputOwner.Correlation() &&
				values.FlowedTypesEqual(owner, outputOwner) {
				return node, nil
			}
			return nil, api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"post-aggregate expression references field %q (type %v) outside the aggregate output contract",
				values.ExplainValue(node), node.Type())
		}
	}
	if ordinal < 0 {
		return node, nil
	}
	resolved, err := values.ResolveFieldOrdinals(outputQOV, []int{ordinal})
	if err != nil {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"post-aggregate native slot %d does not resolve against its owner: %v", ordinal, err)
	}
	if node.Type() == nil || resolved.Type() == nil || !node.Type().Equals(resolved.Type()) {
		return nil, api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"post-aggregate native slot %d changes the expression type from %v to %v",
			ordinal, node.Type(), resolved.Type())
	}
	return resolved, nil
}

func aggregateValueNativeOrdinal(av *values.AggregateValue, agg *logical.LogicalAggregate) int {
	if av == nil || agg == nil {
		return -1
	}
	for i, call := range agg.Calls {
		if av.Op == values.AggCountStar {
			if call.Star {
				return len(agg.GroupKeys) + i
			}
			continue
		}
		if call.Star || !strings.EqualFold(call.Func, av.Op.Symbol()) {
			continue
		}
		if i < len(agg.AggregateOperands) && agg.AggregateOperands[i] != nil &&
			values.SemanticEqualsUnderAliasMap(av.Operand, agg.AggregateOperands[i], values.EmptyAliasMap()) {
			return len(agg.GroupKeys) + i
		}
	}
	want := normalizeAggOutputName(aggregateValueOutputName(av))
	for i, call := range agg.Calls {
		if normalizeAggOutputName(call.CanonicalName()) == want {
			return len(agg.GroupKeys) + i
		}
	}
	return -1
}

func aggregateValueOutputName(av *values.AggregateValue) string {
	if av == nil {
		return ""
	}
	if av.Op == values.AggCountStar {
		return "COUNT(*)"
	}
	operand := "*"
	if av.Operand != nil {
		operand = values.ColumnNameValue(av.Operand)
		operand = strings.ReplaceAll(operand, " ", "")
		if len(operand) > 2 && operand[0] == '(' && operand[len(operand)-1] == ')' {
			operand = operand[1 : len(operand)-1]
		}
	}
	return strings.ToUpper(av.Op.Symbol()) + "(" + operand + ")"
}

// underlyingGroupBy returns the GroupByExpression an expression's output row is
// produced by — the expression itself, or the one reached by peeling operators
// that PASS THE AGGREGATE OUTPUT ROW THROUGH UNCHANGED: a HAVING
// LogicalFilterExpression (filters rows, slots intact) and an ORDER-BY
// LogicalSortExpression (reorders rows but preserves each row's positional slots).
// So the SELECT projection above resolves its references against the aggregate
// output schema by ordinal even with HAVING/ORDER-BY between. nil when the
// expression is not (over) an aggregate. Peeling STOPS at any operator that
// reshapes the row (a projection), so an unrelated inner aggregate is never
// mistaken for this row's producer.
func underlyingGroupBy(expr expressions.RelationalExpression) *expressions.GroupByExpression {
	switch e := expr.(type) {
	case *expressions.GroupByExpression:
		return e
	case *expressions.LogicalFilterExpression:
		if qs := e.GetQuantifiers(); len(qs) == 1 {
			if inner := qs[0].GetRangesOver(); inner != nil {
				return underlyingGroupBy(inner.Get())
			}
		}
	case *expressions.LogicalSortExpression:
		if qs := e.GetQuantifiers(); len(qs) == 1 {
			if inner := qs[0].GetRangesOver(); inner != nil {
				return underlyingGroupBy(inner.Get())
			}
		}
	}
	return nil
}

func (t *cascadesTranslator) translateRef(op logical.LogicalOperator) *expressions.Reference {
	expr := t.translateOp(op)
	if expr == nil {
		return nil
	}
	return expressions.InitialOf(expr)
}

// translateSubqueryRef translates an EXISTS-subquery plan. An existential
// quantifier's child select is NEVER merged into its parent
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

// translateCorrelatedPrimaryUnnest lowers the standalone unnest used by a
// correlated EXISTS primary source. Its collection was resolved in the outer
// semantic scope and therefore carries the exact external correlation that the
// enclosing ForEach quantifier binds. Regular lateral FROM unnests leave
// CorrelatedCollection nil and are translated only through translateUnnestJoin.
func (t *cascadesTranslator) translateCorrelatedPrimaryUnnest(
	u *logical.LogicalUnnest,
) expressions.RelationalExpression {
	if u == nil || u.CorrelatedCollection == nil {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"a standalone array unnest requires a resolved outer collection"))
		return nil
	}
	if u.Alias == "" || u.AtAlias != "" {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"a correlated EXISTS array source requires one element alias without ordinality"))
		return nil
	}
	explode, err := expressions.NewExplodeExpressionWithOrdinality(u.CorrelatedCollection, false)
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"correlated array source has no exact exploded element type: %v", err))
		return nil
	}
	return explode
}

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
		case *logical.LogicalInlineValues:
			if n.Alias != "" {
				set[strings.ToUpper(n.Alias)] = struct{}{}
			}
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
// (boxGatesFresh): the box builds its whole leg-concat positionally and the
// per-leg-window rebase (channels 1+2)
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
	// A MULTI-alias box declines the BINARY ordinal seed whenever a regular
	// (non-EXISTS) WHERE conjunct references a box leg (unnestBoxLegConjunct !=
	// None) — for EITHER verdict, because the binary W4c seed has no per-leg
	// merge window for the conjunct: it would merge into the name-keyed unnest
	// SELECT, out of the executor's below-FOD hoist reach, and a positional box
	// leaves the ref unresolvable (malformed plan). Where the conjunct CAN bake
	// is the GATHERED path: a Bakeable verdict is admitted there and baked over
	// the gather's recorded legTypes (so a Bakeable shape never reaches this
	// binary gate); an Unbakeable one declines the gather AND this binary seed,
	// landing on name-model. The flag is set by translateUnnestExistsFilter
	// (EXISTS — always Unbakeable this slice) AND the non-EXISTS
	// filter-over-unnest merge (classified), so the check is BEFORE the
	// `!unnestUnderExistential` early return below. A single-source outer (==1)
	// is unaffected — its pristine prefix resolves a bare conjunct.
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
	if t.unnestBoxLegConjunct != boxConjNone && !pureSpine && len(outerBoundAliases(left)) > 1 {
		// ANY box-leg-conjunct verdict (Bakeable included) declines the BINARY
		// seed here: a Bakeable conjunct rides the GATHERED path (which admits it
		// and bakes over the recorded legTypes); the binary W4c seed has no merge
		// window for it, so the binary arm stays name-model either way.
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
// references an outer-leg alias in boxAliases — the box-leg reference that
// routes the conjunct verdict (see unnestBoxLegConjunct): Bakeable rides the
// gathered ordinal path and bakes over the recorded legTypes; Unbakeable — and
// the binary W4c seed in ALL flagged cases — stays name-model. Element/ordinal
// (unnest AS/AT) refs are NOT in boxAliases, so a WHERE on the element does not
// trip it.
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

// boxOuterBuildsPositional reports whether the unnest's OUTER box will actually
// take the ordinal seed — the EXACT condition the box-outer POSITIONAL build
// (AXIS 1) must share with the seed gate (AXIS 2). Among outer boxes ONLY a FULL
// box qualifies: clusterArity==1 holds for a merge-opaque FULL box but NEVER for
// LEFT/RIGHT (whose clusterArity is preserved-side + 1 >= 2), so a LEFT/RIGHT
// box's seed can never ordinalize (the :1496 clusterArity==1 gate blocks it).
// Building a LEFT/RIGHT box POSITIONAL while its seed stays NAME-MODEL would
// strand the name-model builder over a positional row — it reads the box by the
// ABSENT qualified LEG.COL keys → NULL / bare last-leg-wins → wrong rows (or a
// loud unresolvable-field on the ON predicate). boxGatesFresh restricts to
// fresh-gating outer boxes (false for scans and INNER clusters);
// unnestExistsSeedSafe folds in the EXISTS scope-collision guard so AXIS 1
// declines EXACTLY when the seed does. For a single-source scan outer this is
// false (boxGatesFresh false) and AXIS 1 is a no-op — a scan ref ignores the
// enclosure bit — so nothing changes off the box path.
func (t *cascadesTranslator) boxOuterBuildsPositional(left logical.LogicalOperator) bool {
	return t.clusterArity(left) == 1 && t.boxGatesFresh(left) && t.unnestExistsSeedSafe(left, false)
}

// existsInnerScopeCollidesOuter reports whether any EXISTS inner subquery's
// plan carries a source alias equal to an outer FROM leg's. VESTIGIAL for
// single-table catalog inners since the collision mint (buildCorrelatedExists
// builds those under a unique correlation — outerBoundAliases(esq.Plan) can
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
// unnestOuterLegAliases is a USER-alias universe: outerBoundAliases
// gathers user source aliases (canonical UPPER) and the merged machine
// correlation — the only machine id in reach — is deleted before use, so
// the consumers' folded lookups are total over same-case keys (fold ≡
// exact here; machine q$N ids never enter this map).
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

// protoFieldLookup resolves a field by SQL identifier: EXACT spelling first,
// then an unqualified case-insensitive scan.
//
// The exact pass has to come first, and it is not an optimization. A quoted
// identifier keeps its case, so `"aB"` must reach the field literally named
// `aB` even when a sibling `Ab` exists; a fold-first lookup answers whichever
// of the two the descriptor happens to list first. The case-insensitive scan
// behind it is the same read-side extension rlcatalog documents: a hand-written
// .proto never went through DDL normalization, so its names are lower/snake
// while an unquoted SQL reference arrives folded upper.
func protoFieldLookup(fs protoreflect.FieldDescriptors, name string) protoreflect.FieldDescriptor {
	if fd := fs.ByName(protoreflect.Name(name)); fd != nil {
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
		// An intermediate must be a singular STRUCT: flat repeated fields
		// and NullableArrayWrapper fields are both arrays here.
		if fd.IsList() || fd.Kind() != protoreflect.MessageKind || values.IsWrappedArrayDescriptor(fd.Message()) {
			return values.UnknownType, "", false, true
		}
		fields = fd.Message().Fields()
	}
	fd := protoFieldLookup(fields, fieldSegments[len(fieldSegments)-1])
	if fd == nil {
		return values.UnknownType, "", false, false
	}
	// The final segment is an array either as a flat repeated field or
	// through the NullableArrayWrapper; the element type comes from the
	// EFFECTIVE repeated field, the column name from the outer field.
	inner, _, ok := values.EffectiveListField(fd)
	if !ok {
		return values.UnknownType, "", false, true
	}
	// The column name is the SLOT name the row layout carries for this field,
	// so it must be minted by the same authority the layout uses. Folding it
	// here made it miss for any descriptor whose names are not already upper.
	return arrayFieldElementType(inner), values.FieldNameForProtoField(fd), true, true
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

// arrayFieldElementType returns the exact executable element type of an
// effective repeated proto field. The values package owns descriptor-to-row
// typing; this purpose helper only removes the enclosing repetition and makes
// the element non-null, matching FieldTypeForProtoField's ARRAY convention.
// Keeping a second nullable scalar-kind switch here made the same stored array
// disagree with itself at unnest admission.
func arrayFieldElementType(fd protoreflect.FieldDescriptor) values.Type {
	element := values.ScalarTypeForProtoKind(fd)
	if values.IsUnresolved(element) {
		return values.UnknownType
	}
	return values.WithNullability(element, false)
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
	// The entire unnest lowering (FlatMap-over-Explode, dotted-prefix
	// bipartition machinery, multi-source fallback rebuilds via
	// unnestFallbackOrReject) stays on the name model — every join
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
	var inlineOwner *logical.LogicalInlineValues
	if len(u.Segments) > 0 {
		inlineOwner = findInlineValuesOwner(j.Left, u.Segments[0])
	}
	// A lateral unnest is classified by walking the outer source's PROTO
	// descriptor for the array field (unnestArrayElementType → resolveRecordType
	// → t.md). The metadata-less translation path (TranslateToCascades /
	// TranslateToCascadesWithSubqueries(op, nil) — used by scalar-subquery / DML
	// translation and unit tests) has no descriptor to classify against. Java
	// never reaches an unnest without a SemanticAnalyzer/metadata in scope, so
	// rather than dereference nil metadata (a panic) we decline cleanly: an
	// unnest genuinely needs metadata to classify. No production caller unnests
	// without metadata (every SQL plan path passes real md). RFC-142.
	if t.md == nil && inlineOwner == nil {
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
	if outerTable == "" && inlineOwner == nil {
		// Unnest-residual class 4 (chained unnest): segment 0 names a
		// PRIOR lateral unnest's element, not a scan. Positively gated on
		// findOwnerUnnest (never merely
		// outerTable==""), and only for INNER-comma chains
		// (!unnestUnderExistential — an under-existential chain keeps its own
		// EXISTS-composition binders). A resolvable chain routes to translateChainedUnnestJoin;
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
	if inlineOwner != nil {
		var isArray, fieldPresent bool
		elementType, fieldName, isArray, fieldPresent = inlineValuesArrayElementType(inlineOwner, u.Segments[1:])
		if !isArray {
			if fieldPresent {
				t.setTranslateErr(api.NewError(api.ErrCodeInvalidColumnReference,
					"join correlation can occur only on a column of repeated (array) type"))
			} else {
				t.setTranslateErr(api.NewErrorf(api.ErrCodeUndefinedColumn,
					"column %q does not exist on source %q",
					strings.Join(u.Segments[1:], "."), u.Segments[0]))
			}
			return nil
		}
	} else if t.outerSourceIsCTE(outerTable) || outerSourceIsDerivedTable(j.Left, u.Segments[0]) {
		// Unnest-residual class 3: resolve the array field through the
		// CTE/derived body's projection to a base-table array column (the
		// flowed type is UnknownType — see classifyDerivedUnnestArray below). A
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
	// ordinal alias MUST be distinct. `FROM t, t.arr AS X AT X` would append the
	// element and the ordinal under the SAME bare+qualified names;
	// RecordConstructorValue.Evaluate stores fields in a map, so the ordinal
	// (appended last) silently OVERWRITES the element — `SELECT X` returns the
	// ordinal, not the unnested value. Reject cleanly BEFORE constructing the result,
	// consistent with the unnest-alias-vs-outer-alias rejection above. Java's
	// visitAtomTableItem binds AS and AT to two distinct quantifier columns; a
	// duplicate alias is a binding error upstream. RFC-142.
	if rejectErr := unnestAliasReject(u); rejectErr != nil {
		t.setTranslateErr(rejectErr)
		return nil
	}

	// A MULTI-SOURCE outer over a gated inner cluster gathers FLAT — the Explode
	// becomes an ordinary quantifier of one (N+1)-way select whose collection is
	// a genuine baked correlation to the OWNING source's own quantifier, matching
	// Java's shape. Runs AFTER the full validation gauntlet above (the rejections
	// are shared verbatim) and BEFORE the binary path translates j.Left as one
	// enclosed ref (the gathered path translates legs FRESH; translating both
	// ways would double-append side state, e.g. collected scalar-subquery plans).
	// A nil is a DECLINE — the binary fallback below.
	//
	// An under-EXISTS unnest admitted by unnestExistentialGatherOK (computed
	// pre-translation in translateUnnestExistsFilter) takes the gathered ordinal
	// cluster — the same path a non-EXISTS box unnest takes. A non-admitted
	// existential unnest keeps the name-model binary seed below.
	//
	// A NON-ENCLOSED unnest gathers the flat ordinal cluster. An ENCLOSED unnest
	// (prevEnclosure — an inner link of a chain, or a leg of a larger name-model
	// composition over an OUTER/nested box) keeps the name-model binary residual:
	// its ordinal seed would be a child under a still-name-model outer-box
	// parent, so gathering it here would strand the residual's box-leg rebase at
	// ordinal -1. A box outer's ON stays the null-on-empty condition INSIDE the
	// dissolve (translateGatheredUnnestCluster's LEFT/FULL guard). An under-EXISTS
	// unnest still declines the gather when its flat-leg conjunct is UNBAKEABLE
	// (unnestExistentialGatherOK false) — that shape keeps the name-model residual
	// (the scalar-subquery reach class).
	if !prevEnclosure && (!t.unnestUnderExistential || t.unnestExistentialGatherOK) {
		if sel := t.translateGatheredUnnestCluster(j, u, innerCorr, elementType, fieldName, unnestTrailing); sel != nil {
			return sel
		}
	}

	// AXIS 1, coupled to the seed gate (AXIS 2) via boxOuterBuildsPositional: a
	// FULL outer box that will take the ordinal seed BUILDS a positional row instead
	// of the name-model Datum this unnest's `inInnerCluster=true` above would
	// otherwise force. The predicate is boxOuterBuildsPositional, NOT
	// boxGatesFresh alone: a LEFT/RIGHT box gates fresh but has clusterArity>=2 so its seed
	// stays name-model — building it positional would strand the name-model
	// builder over a positional row (wrong rows). We clear the enclosure ONLY
	// for the box's own translateRef, then restore it — the rest of the unnest
	// lowering keeps the enclosed bit. `prevEnclosure ||` keeps an
	// already-enclosed unnest name-model (matching the seed's own !prevEnclosure
	// gate below): the box only builds positional when this unnest is un-enclosed.
	savedEnclosure := t.inInnerCluster
	t.inInnerCluster = prevEnclosure || !t.boxOuterBuildsPositional(j.Left)
	outerRef := t.translateRef(j.Left)
	t.inInnerCluster = savedEnclosure
	if outerRef == nil {
		return nil
	}
	outerCorr := unnestOuterCorrelation(j.Left)

	// The correlated array Value is ALWAYS the ordinal bake below
	// (unnestBakedRootCollection). There is no name-keyed alternative: the
	// outer row this Explode reads is ordinal-addressed by the seed, so a
	// collection read keyed by a column NAME has nothing to resolve against.
	//
	// This is where the qualified-name channel used to be born (RFC-197 item 6).
	// A multi-namespace outer row exposes every leg's columns bare
	// (last-leg-wins, so a bare read followed the join's EXECUTION operand
	// order — the planner may legally swap it) AND qualified `LEG.COL`, and the
	// lowering used to pack the leg into the string, `FieldValue{Field: SEG0 +
	// "." + COL}`, to escape the ambiguity. Structure in a string is not an
	// escape: nothing downstream could tell that qualifier from a leaf name
	// containing a dot, and the recovery it forced (a correlation set keyed by
	// the sliced prefix) attributed the dependency by NAME, not by identity.
	// The ordinal bake states the same fact structurally — the leg's own
	// window offset — so the ambiguity never arises and the prefix has nothing
	// to encode.
	withOrdinality := u.AtAlias != ""
	outerQ := expressions.NamedForEachQuantifier(outerCorr, outerRef)
	var innerQ expressions.Quantifier

	// Ordinalize the seed when the OUTER is a SINGLE SOURCE (clusterArity==1) AND
	// the unnest is NOT ENCLOSED in a larger name-model composition — this
	// ordinal seed is what keeps the ISOLATED single-source lateral unnest alive.
	// Three decline gates, all the same "enclosure = poison" boundary — an
	// ENCLOSED unnest's ordinal seed (baked ofOrdinal refs, non-anchored RC)
	// would be flattened by SelectMergeRule into a name-model parent whose
	// machinery cannot consume it:
	//   - clusterArity(j.Left) > 1: a multi-source OUTER (`FROM A, B, A.arr AS x`)
	//     — ordinalizing a flattened multi-table outer cluster erases the buried
	//     source names.
	//   - prevEnclosure: the unnest is a LEG of a larger multi-source join cluster
	//     (`FROM A, A.arr AS x, B`). It flattens into the (unnest × B) select; a
	//     GROUP BY / aggregation over it re-enumerates via PartitionSelectRule,
	//     whose anchored re-enumeration (NewReEnumerationAnchoredRecord) cannot
	//     resolve a non-anchored ordinal-seed leg (a loud panic). Stays name-model
	//     until the enclosing multi-source cluster itself ordinalizes. (A
	//     projection over such a leg WOULD work via the name-keyed Datum's
	//     qualified keys, but the aggregation path forces the whole class
	//     name-model — the two are indistinguishable here, at lowering.)
	//   - unnestUnderExistential, but only for a SINGLE-ALIAS outer (see
	//     unnestExistsSeedSafe): the unnest is the OUTER of an EXISTS semi-join.
	//     This used to force name-model because the existential rebase read
	//     outer-leg refs by name and panicked on baked ofOrdinal refs; leaving
	//     the EXISTS correlation's outer-leg refs LEG-RELATIVE (the mixed seed
	//     now carries executor windows) lets the executor's below-FOD hoist
	//     rebase them POSITIONALLY — so a single-source unnest under EXISTS gates
	//     ordinal like any other. A MULTI-ALIAS outer (a merge-opaque FULL OUTER box),
	//     or an EXISTS inner scanning a table aliased the same as an outer leg, stays
	//     name-model (see unnestExistsSeedSafe / existsInnerScopeCollidesOuter).
	// A decline (nil) falls back to the name-model builder.
	var resultValue values.Value
	// SINGLE-SOURCE (clusterArity==1), non-enclosed, exists-safe unnest
	// ordinalizes the seed. A MULTI-SEGMENT path (`FROM t, t.rec.arr AS x`,
	// len(Segments)>2, unnest-residual class 2) needs its COLLECTION baked as a
	// fused ofOrdinal root (unnestBakedRootCollection below), and so does the
	// single-segment case — the suffix-free instance of the same root. When the
	// bake declines (nil), the whole shape is untranslatable and the decline
	// below is LOUD.
	if t.clusterArity(j.Left) == 1 && !prevEnclosure && t.unnestExistsSeedSafe(j.Left, false) && len(u.Segments) >= 2 {
		resultValue = t.unnestOrdinalSeed(j.Left, outerCorr, innerCorr, u, elementType)
		// The COLLECTION bakes positionally under the ordinal-seed build for
		// EVERY segment arity: the single-segment `t.arr`
		// collection is the suffix-free case of the same ofOrdinal root the
		// multi-segment path fuses — the outer row is ORDINAL-addressed at the
		// build, so a name-keyed collection read has nothing to resolve
		// against (the runtime name fallback is deleted).
		if resultValue != nil {
			if baked := t.unnestBakedRootCollection(j.Left, outerCorr, u, fieldName, elementType, 1, -1); baked != nil {
				bakedExplode, explodeErr := expressions.NewExplodeExpressionWithOrdinality(baked, withOrdinality)
				if explodeErr != nil {
					t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
						"lateral unnest collection has no exact element type: %v", explodeErr))
					return nil
				}
				innerQ = expressions.NamedForEachQuantifier(innerCorr, expressions.InitialOf(bakedExplode))
			} else {
				resultValue = nil // the collection is underivable — decline LOUD
			}
		}
	}
	if resultValue == nil {
		// The ordinal seed declined (multi-source enclosed outer, exists-unsafe
		// scope, an underivable leg). There is no name-model FlatMap fallback
		// left (a full-suite producer census found no production shape reaching
		// here); the shape is untranslatable — LOUD, never silent wrong rows.
		if t.translateErr == nil {
			t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
				"lateral unnest did not ordinalize (multi-source or enclosed outer, or an exists-unsafe scope)"))
		}
		return nil
	}

	selectExpr, err := expressions.NewSelectExpressionWithJoinType(
		resultValue,
		[]expressions.Quantifier{outerQ, innerQ},
		nil,
		[]string{outerAlias, innerAlias},
		expressions.JoinInner,
	)
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"lateral unnest select has no exact result row: %v", err))
		return nil
	}
	return selectExpr
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
			fv, ok := values.AsFieldValue(node)
			if !ok {
				return node
			}
			// A correlated-primary unnest is resolved as a one-column LOCAL
			// virtual source inside the EXISTS scope. Its element reference is
			// therefore a source-relative baked leaf (E#0), not the correlated
			// FieldValue(QOV(E), E) produced by an ordinary lateral join. The
			// Explode still flows the primitive element as the whole QOV. Admit
			// exactly that standalone shape here: correlated collection,
			// no ordinality, the explicit element alias, and source ordinal 0.
			// A field below a record element has a child or a longer path and
			// remains a real field access.
			if _, hasOwner := values.AsQuantifiedObjectValue(fv.ChildValue()); !hasOwner {
				return node
			}
			qov, ok := values.AsQuantifiedObjectValue(fv.ChildValue())
			if !ok || qov.Correlation() != unnestCorr {
				return node
			}
			// A reference that DESCENDED into the element (`i.sku`) names a
			// MEMBER of the element, and no member is the element itself. The arms
			// below answer "this reference IS the whole flowed element (or its
			// ordinality companion)", so a descent must not reach them: the element
			// QOV they return carries the descent nowhere, and returning it reads
			// the whole struct where a member was asked for.
			//
			// The gate is on the ACCESSOR COUNT and cannot be on fv.Field, because
			// the name is one segment of the path and so collides by construction.
			// Under the mint that named a fused value after its ROOT it collided
			// ALWAYS — the root of a descent through an unnest binding IS the alias,
			// so every `i.<member>` matched here and was replaced by the bare
			// element (measured: `WHERE i.sku = 'x'` over `orders.items AS i`
			// reached this switch with Field="I"). Naming the fused value after its
			// LEAF narrows that to the shapes where a member is spelled like the
			// alias (`AS sku` … `sku.sku`) — narrower, and still wrong. Neither is a
			// question one segment can answer, so the arity is what decides.
			//
			// The comment above already asserted that "a field below a record
			// element has a child or a longer path and remains a real field access".
			// The longer-path half of that was never tested; this is it.
			if fv.Path().Len() > 1 {
				return node
			}
			switch strings.ToUpper(fv.DisplayName()) {
			case asAlias:
				if asAlias != "" {
					if withOrdinality {
						if resolved, err := values.ResolveFieldOrdinals(qov, []int{0}); err == nil {
							return resolved
						}
						return node
					}
					// Bare scalar element: the alias IS the whole flowed object.
					return qov
				}
			case atAlias:
				if atAlias != "" {
					if resolved, err := values.ResolveFieldOrdinals(qov, []int{1}); err == nil {
						return resolved
					}
					return node
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
		Input:                      newJoin,
		Predicate:                  andOf(residualPreds),
		ExistsSubqueries:           f.ExistsSubqueries,
		ScalarSubqueries:           f.ScalarSubqueries,
		CorrelatedScalarSubqueries: f.CorrelatedScalarSubqueries,
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
	// CANONICAL UPPER: the correlation-key namespace (Scope.AddSource
	// canonicalizes registrations the same way). A quoted-lowercase
	// unnest alias (`t.arr AS "val"`) resolves as VAL; emitting the
	// verbatim `val` here missed the executor's exact leg lookup and an
	// otherwise-valid query died unbound at runtime.
	return values.NamedCorrelationIdentifier(strings.ToUpper(corr))
}

// newLimitExprFromLogical builds the Cascades LogicalLimitExpression for a
// logical LIMIT, preserving a runtime (parameterized) row cap when present
// (RFC-156 `... <= ?` vector rank limit). The single source of truth for the
// static-vs-runtime split so every LIMIT translation site is identical.
func newLimitExprFromLogical(o *logical.LogicalLimit, q expressions.Quantifier) (*expressions.LogicalLimitExpression, error) {
	if o.LimitValue != nil {
		return expressions.NewRuntimeLogicalLimitExpression(o.LimitValue, o.Offset, q)
	}
	return expressions.NewLogicalLimitExpression(o.Limit, o.Offset, q)
}

func (t *cascadesTranslator) translateOp(op logical.LogicalOperator) expressions.RelationalExpression {
	if op == nil {
		return nil
	}
	// Opaque boundaries cut inner-join-cluster enclosure —
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
	case *logical.LogicalInlineValues:
		return t.translateInlineValues(o)
	case *logical.LogicalScan:
		return t.translateScan(o)
	case *logical.LogicalUnnest:
		return t.translateCorrelatedPrimaryUnnest(o)
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
		limitExpr, err := newLimitExprFromLogical(o, limitQ)
		if err != nil {
			t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"LIMIT input has no exact flowed row: %v", err))
			return nil
		}
		return limitExpr
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
			// Star opaque ordinal leg: an ADMITTED projection-less
			// unnest body (`WITH S AS (SELECT * FROM t, t.arr AS x)`) NORMALIZES
			// to the explicit bare projection of its boundary labels — exactly
			// what the user would write by hand — so it translates as the
			// verified BARE-PROJECTED derived-boundary class. The wrapper's
			// LogicalProjectionExpression is a SelectMergeRule boundary; without
			// it the body's SelectExpression MERGES into a gated parent select
			// and the dissolved qualified reads fall into the name-model $m
			// partition machinery, which mis-serves them over the positional
			// row (loud ordinal -1 on duplicate labels; the unique-label cases
			// only resolved through the qualifier-strip fallback). The SAME
			// predicate gates the parent's opaque-leg admission
			// (ordinalEligible/clusterArity) and the boundary schema
			// (legColumns), so every consumer sees the projected row.
			toTranslate := body
			if starCols, star := t.derivedBodyStarOrdinalLeg(body); star {
				names := make([]string, len(starCols))
				for i, c := range starCols {
					names[i] = c.Name
				}
				// NO ProjectionRefs, and that is STRUCTURAL rather than an
				// un-migrated producer. Every PARSED projection channel carries
				// the parse-tree segment triple now (CQ-52), so nothing
				// downstream has to recover a qualifier by slicing a rendered
				// name. These labels have no parse tree behind them at all —
				// they are the body's OUTPUT COLUMNS, minted here — so there is
				// no segment count to capture, and capturing one would mean
				// inventing it. An invented triple is trusted exactly like a
				// real one, which is why the honest value is the zero one.
				//
				// The labels are BARE by construction (the outer scan's columns,
				// then the link's AS/AT aliases), so nothing downstream splits
				// them today. A quoted identifier carrying a dot is legal SQL,
				// and for that case the question — should a star-projected body
				// column be leg-addressable at all? — is a BEHAVIOUR decision
				// about this normalization's output contract. It is stated, and
				// consciously left open, at the leg-window re-split arm in
				// bakeFlatRefsAgainstColumns, which is what would answer it by
				// accident if it were converted without the decision.
				proj := logical.NewProject(body, names, nil)
				// This is a positional boundary over the body's REAL translated
				// input. Do not synthesize a same-spelled QOV here: when an element
				// alias shadows a bottom column of the same exact type, the
				// synthetic whole-row correlation can be indistinguishable from the
				// scalar leg correlation (SUB), and the projection is rebound to an
				// undeclared leg object. InputOrdinals resolves against the actual
				// input quantifier after the body has translated, preserving the
				// shadow-deduped row without inventing another owner.
				inputOrdinals, mapped := t.starBodyBoundaryInputOrdinals(body, starCols)
				if !mapped {
					t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
						"star-body boundary columns do not map to the admitted ordinal row"))
					return
				}
				proj.InputOrdinals = inputOrdinals
				toTranslate = proj
			}
			result = t.translateOp(toTranslate)
		})
		return result
	}
	// Type the scan leaf with the table's canonical record type.
	// tableColumns sources fields from the proto descriptor in
	// declaration order with UPPER-cased names — the SAME order and case the
	// runtime PositionalRow carries (protoToPositional), so FieldValue.
	// resolveOrdinal's plan-time ordinal matches the runtime slot by
	// construction. Two scans of one table build structurally-equal
	// RecordTypes (tableColumns is deterministic; RecordType.Equals is
	// structural), so memo dedup on flowedType (FullUnorderedScanExpression.
	// EqualsWithoutChildren) holds without a pointer cache. An unresolvable
	// table (no metadata) degrades to UnknownType → name resolution, as before.
	cols := t.tableColumns(s.Table)
	if len(cols) == 0 {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUndefinedTable,
			"scan %q has no exact catalog row type", s.Table))
		return nil
	}
	scan, err := expressions.NewFullUnorderedScanExpression(
		[]string{s.Table}, values.NewRecordType("", false, cols))
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"scan %q has no exact result row: %v", s.Table, err))
		return nil
	}
	return scan
}

func (t *cascadesTranslator) translateFilter(f *logical.LogicalFilter) expressions.RelationalExpression {
	// Fold a WHERE-EXISTS whose post-pagination cardinality is known before any
	// routing. The front-end proves the inner either empty or non-empty (notably:
	// a non-grouped aggregate emits one row before LIMIT/OFFSET), so both EXISTS
	// and NOT EXISTS can be substituted with their exact boolean result. Running
	// here means the correlated-aggregate semi-join is never built, avoiding the
	// joined-outer correlation-placement hazard entirely.
	f = t.foldKnownExists(f)
	// Every successfully substituted alias was removed from ExistsSubqueries.
	// Any KnownTruth that remains is an unsupported consumer/boolean shape (for
	// example a synthetic projected-EXISTS carrier or an EXISTS below OR).
	// Raw-semi-joining it would reintroduce the aggregate/pagination cardinality
	// bug, so decline typed-loud.
	if hasKnownExistsTruth(f.ExistsSubqueries) {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"a cardinality-known EXISTS in this boolean position is not yet supported"))
		return nil
	}
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
	// Correlated scalar predicates own a distinct two-level lowering: first
	// materialize the per-outer scalar (strict FirstOrDefault when required),
	// then evaluate WHERE above that LEFT-scalar box and strip the hidden slot.
	// Dispatch before every generic filter rewrite so no carrier clone can drop
	// the per-row plan or move the comparison below the cardinality barrier.
	if len(f.CorrelatedScalarSubqueries) > 0 {
		return t.translateFilterWithCorrelatedScalar(f)
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
	// When the input is an ENCLOSED-unnest cluster the
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

	// The ENCLOSED-MIDDLE unnest under EXISTS (`FROM A, A.arr AS x,
	// B WHERE [x > c AND] EXISTS (…)`). The rotation dispatch below routes this
	// to translateUnnestExistsFilter, which FOLDS the non-EXISTS conjuncts into
	// the gathered unnest select itself (splitNonExistsPredicates) — so, exactly
	// like the non-EXISTS enclosedGathered probe above, pushBuriedUnnestPredicate
	// Down must STAND DOWN: its restructured tree wraps the unnest join in a
	// Filter, which hides the buried unnest from rotateEnclosedUnnest's walker and
	// strands the cluster on the name-model path. Detection is metadata-only
	// (rotateEnclosedUnnest is pure); the same guard as enclosedGathered keeps a
	// nested/enclosed EXISTS-unnest on the name-model path (correct-or-loud).
	existsEnclosedRotatable := false
	if f.Predicate != nil && len(f.ExistsSubqueries) > 0 && !t.inInnerCluster && !t.unnestUnderExistential {
		if join, isJ := f.Input.(*logical.LogicalJoin); isJ && join.Kind == logical.JoinInner {
			if _, _, _, _, _, rok := t.rotateEnclosedUnnest(join); rok {
				existsEnclosedRotatable = true
			}
		}
	}

	// A BURIED CHAINED spine behind trailing plain legs (`FROM t,
	// t.arr AS x, x.sub AS y, z WHERE …`) rotates to the box-bottom chained
	// form BEFORE the buried-predicate push — the push would filter-wrap the
	// spine inside the trailing join and hide it (the rotation peel stops at a
	// Filter), stranding the cluster on the name-model residual. With the
	// rotation the top link is the rightmost source, so the buried push stands
	// down (rightIsUnnest) and the chained WHERE arm below owns the whole
	// predicate: pure-outer conjuncts ride the lazy ⊆-outerLegs path (SARG),
	// straddles bake positionally over the chained merged row
	// (rebaseChainedOuterLegPredicate) — the exact treatment the box-bottom
	// chained certificates pin. Same enclosure stance as the single-link
	// gather probe above.
	if f.Predicate != nil && len(f.ExistsSubqueries) == 0 && !t.inInnerCluster && !t.unnestUnderExistential {
		if join, isJ := f.Input.(*logical.LogicalJoin); isJ {
			if rotated, rok := t.rotateBuriedChainedSpine(join); rok {
				f = &logical.LogicalFilter{
					Input:                      rotated,
					Predicate:                  f.Predicate,
					PredicateText:              f.PredicateText,
					ExistsSubqueries:           f.ExistsSubqueries,
					ScalarSubqueries:           f.ScalarSubqueries,
					CorrelatedScalarSubqueries: f.CorrelatedScalarSubqueries,
				}
			}
		}
	}

	pushedAllBuried := false
	if f.Predicate != nil && !enclosedGathered && !existsEnclosedRotatable {
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
		// into the existential peel's binary NLJ, which materializes its
		// inner ONCE against an unbound context — the correlated Explode yields no
		// rows and the query returns empty. The unnest MUST stay its own
		// FlatMap-over-Explode (translateUnnestJoin) as the existential's OUTER.
		// translateUnnestExistsFilter builds that nested shape (and folds any
		// WHERE-on-the-unnest-column into the inner Explode). RFC-142 (P2b).
		if join, ok := f.Input.(*logical.LogicalJoin); ok {
			if u, isUnnest := join.Right.(*logical.LogicalUnnest); isUnnest {
				return t.translateUnnestExistsFilter(f, join, u)
			}
			// The ENCLOSED-MIDDLE unnest under EXISTS
			// (`FROM A, A.arr AS x, B WHERE [x > c AND] EXISTS (…)`). Here the
			// unnest is a BURIED leg (`join.Right` is the trailing plain leg B, not
			// the unnest), so the root-form dispatch above misses it and the join
			// would fall to translateJoinWithExists, which translates the buried
			// unnest ENCLOSED → the name-model binary seed (the outer row read by
			// NAME). Rotate the cluster to the root form (Join(Join(plain legs),
			// Unnest)) the non-EXISTS path already gathers through
			// (translateEnclosedUnnestGather), then route it to the SAME
			// existential composition the unnest-LAST shape uses — where the (A,B)
			// INNER outer gathers via E-1a, the seed ordinalizes, and any non-EXISTS
			// element conjunct folds into the gathered select. The rotation runs on
			// the ORIGINAL filter (existsEnclosedRotatable stood the buried-predicate
			// push down above, so f still carries the whole WHERE for the fold). It
			// is inner-join-equivalent (comma legs) and fires ONLY for a genuine
			// buried unnest, so a plain `FROM A, B WHERE EXISTS` still falls through
			// to translateJoinWithExists unchanged.
			if existsEnclosedRotatable {
				if rebuilt, ru, _, _, _, rok := t.rotateEnclosedUnnest(join); rok {
					return t.translateUnnestExistsFilter(f, rebuilt, ru)
				}
			}
			// The join+EXISTS FLATTEN is INNER-only: it merges the WHERE's
			// non-EXISTS conjuncts into the select's predicate list, which
			// the existential implementation feeds to the NLJ as JOIN
			// predicates — for an OUTER join that turns a preserved-side
			// WHERE conjunct into ON semantics (the failing row NULL-PADS
			// instead of dropping: `dept d LEFT JOIN emp e ... WHERE d.id=3
			// AND NOT EXISTS(...)` returned every dept — a pre-existing
			// silent-wrong, reproducible on master too). OUTER
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
		// A NON-EXISTS direct unnest whose outer is a multi-alias box: classify a
		// WHERE conjunct referencing a box leg BEFORE translating the unnest —
		// the verdict is metadata-only (classifyBoxLegConjunct), so a decline is
		// never poisoned by translation side state. BAKEABLE conjuncts ride the
		// gathered ordinal path (the gather records its legTypes; the merge below
		// bakes over the record); UNBAKEABLE ones (subquery-carrying, foreign
		// correlation, unresolvable ref) keep the name-model plan, where the
		// conjunct resolves via qualified keys. The EXISTS path
		// (translateUnnestExistsFilter) always sets Unbakeable this slice.
		// Restored after so no sibling translation observes it.
		prevOuterConj := t.unnestBoxLegConjunct
		if bu, isUnnest := join.Right.(*logical.LogicalUnnest); isUnnest {
			// A WHERE conjunct on a join-leg column of a LEFT/RIGHT
			// OUTER box at the BOTTOM of a CHAINED lateral-unnest spine is the
			// un-ordinalizable straddle — LOUD-REJECT (correct-or-loud). The conjunct
			// is NULL-SUPPLIED WHERE-above-OUTER: it must evaluate on the box's
			// OUTPUT row (dropping null-padded rows), and neither representation
			// serves it — the chained merged-corr rebase bakes onto mergedCorr
			// (which COLLIDES with the first link's own inner Explode quantifier, so
			// a pushed-down ofOrdinal binds to the ELEMENT row, an ordinal-(-1)
			// strand), a box-quantifier bake lets PushFilterBelowJoinRule sink it
			// below the nested outer null-extension into the null-supplying scan
			// (LEFT→INNER, silent wrong rows), and the name-model residual strands
			// at physicalization. The check is metadata-only, so it runs BEFORE the
			// join translates: the doomed translation must not run at all (its
			// fallback would fire the name-model result-value producers for
			// a plan that is discarded). Row-answering chained shapes (element rows,
			// leg projections, element/AT WHERE, deeper links) carry no box-leg
			// conjunct and never trip this. Ordinalizing the straddle is a
			// booked reach extension (TODO.md).
			if isChainedUnnest(join.Left, bu) {
				if boxJoin := chainedSpineBottomOuterBox(join.Left); boxJoin != nil &&
					nonExistsConjunctRefsOuterLeg(f.Predicate, outerBoundAliases(boxJoin)) {
					t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
						"WHERE on a join-leg column of an OUTER JOIN under a chained lateral unnest is not supported"))
					return nil
				}
			}
			t.unnestBoxLegConjunct = boxConjNone
			if nonExistsConjunctRefsOuterLeg(f.Predicate, outerBoundAliases(join.Left)) {
				if bj, isBoxJoin := join.Left.(*logical.LogicalJoin); isBoxJoin && bj.Kind != logical.JoinInner {
					t.unnestBoxLegConjunct = t.classifyBoxLegConjunct(bj, bu, f.Predicate)
				} else {
					t.unnestBoxLegConjunct = boxConjUnbakeable
				}
			}
		}
		joinExpr := t.translateJoin(join)
		t.unnestBoxLegConjunct = prevOuterConj
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
			// merged row carries the qualified `A.c` key (the executor's mergeRows,
			// now the sole authority), so rebase any outer-leg reference (any
			// outerBoundAliases(j.Left) leg, e.g. A) to that key off the merged
			// QOV — the SAME outer-leg-to-merged rebase the EXISTS path
			// (rebaseUnnestOuterLegPredicate) and the real-JOIN+EXISTS path
			// (rebaseOuterLegRefsToMerged) perform. A single outer scan
			// (`FROM t, t.arr`) flows under segment-0's own alias, so its leg is
			// the merged corr itself and the rebase is a no-op. RFC-142.
			// The ENCLOSED gathered form (`FROM A, A.arr
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
						return t.exactSelectWithJoinType(
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
				// The GATHERED BOX-outer seed (exactly 2 quantifiers —
				// the $BOX leg + the Explode, indistinguishable by count from the
				// binary name-model select). The gather RECORDED its legTypes; the
				// conjunct bakes over that EXACT map (the seed⟺merge one-authority
				// law) so every box-leg reference lands on its buried window —
				// ofOrdinal(QOV($BOX), leafOffset+idx), WHERE-above-LEFT semantics
				// for both legs. The record's presence IS the discriminator (a
				// reachability fact, not a re-derivation argument).
				if recorded, hasRecord := t.unnestGatherBoxLegTypes[join]; hasRecord {
					// CONSUME-ONCE (the enclosedGatherCache discipline): a shared
					// subtree (a CTE body referenced twice) retranslates this SAME
					// node, and a later translation whose gather DECLINED (e.g.
					// enclosed) must not consume a record from an earlier one —
					// the positional bake would land on a name-model select.
					delete(t.unnestGatherBoxLegTypes, join)
					toMerge := bakeGatedJoinPredicates([]predicates.QueryPredicate{pred}, recorded)
					// Defensive net: the pre-translation verdict guaranteed
					// bakeability, so no unbaked buried-leg reference may survive.
					// A survivor would strand as a lazy name read over the
					// positional row (silent NULL) — loud internal error instead.
					for _, mp := range toMerge {
						if predicateRefsBuriedLeg(mp, recorded) {
							t.setTranslateErr(api.NewErrorf(api.ErrCodeInternalError,
								"box-leg conjunct reference survived the merge bake unbaked (verdict/bake drift)"))
							return nil
						}
					}
					return t.exactSelectWithJoinType(
						sel.GetResultValue(),
						sel.GetQuantifiers(),
						append(sel.GetPredicates(), toMerge...),
						sel.GetSourceAliases(),
						sel.GetJoinType(),
					)
				}
				if len(sel.GetQuantifiers()) > 2 {
					// The GATHERED flat unnest select — the cluster
					// legs are the select's OWN quantifiers, so an outer-leg
					// reference (`FieldValue(QOV(MA), c)`) is a GENUINE leg ref;
					// there is no merged row to rebase onto (the rebase would
					// point refs at a key the flat select never flows). Bake
					// cross-leg conjuncts through the cluster's own spine
					// instead, exactly as a gated join's WHERE merge does.
					//
					// (An earlier spanning fail-open here is gone — it guarded a
					// phantom bug: non-discriminating seed data misread a
					// correct all-rows result as a dropped predicate. Spanning
					// conjuncts classify, rewrite (bare QOVs are translated),
					// and filter correctly through the gathered path.)
					toMerge := []predicates.QueryPredicate{pred}
					if lj, isLJ := join.Left.(*logical.LogicalJoin); isLJ {
						toMerge = bakeGatedJoinPredicates(toMerge, t.gatedJoinLegTypes(lj))
					}
					return t.exactSelectWithJoinType(
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
					// The rebase FORM: every RC seed is ORDINAL now (the name-model
					// NewAnchoredJoinRecord fallback was deleted with its producer)
					// and takes the POSITIONAL bake (ofOrdinal).
					_, ordinalSeed := sel.GetResultValue().(*values.RecordConstructorValue)
					// (The box-leg-WHERE-over-a-chained-OUTER-box straddle never reaches
					// this rebase: the check above loud-rejects it BEFORE the join
					// translates — see the hoisted check at the top of this merge arm —
					// so the doomed translation never runs its name-model fallback.)
					baked, ok := rebaseChainedOuterLegPredicate(pred, outerLegs, mergedCorr, t.ordinalLegType(join.Left), ordinalSeed)
					if !ok {
						return nil
					}
					pred = baked
				} else {
					mergedType, _ := sel.GetResultValue().Type().(*values.RecordType)
					var rebased bool
					pred, rebased = rebaseUnnestOuterLegPredicate(pred, outerLegs, mergedCorr, mergedType, UnnestLegMintSiteNonChainedMerge)
					if !rebased {
						return nil
					}
				}
			}
			toMerge := []predicates.QueryPredicate{pred}
			// WHERE conjuncts merged into a GATED join's
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
			return t.exactSelectWithJoinType(
				sel.GetResultValue(),
				sel.GetQuantifiers(),
				merged,
				sel.GetSourceAliases(),
				sel.GetJoinType(),
			)
		}
	}

	// When this filter carries EXISTS subqueries, its select
	// gains existential quantifiers (buildExistentialSelect below) — a
	// name-model parent the outer ForEach leg merges into (post-flattening
	// arity ≥ 3 counting the existential). A join inside the leg (derived
	// table over a join) must therefore gate name-model: mark it enclosed.
	//
	// THE ENCLOSURE LIFT: a gate-eligible OUTER box (a single-source LEFT/RIGHT
	// box that gates fresh — see boxGatesFresh) is NOT enclosed, so it gates
	// ORDINAL and implementExistentialSelect's below-FOD ordinal rebase fires.
	// Everything else keeps the name-model enclosure — a buried/clustered/FULL
	// box, a non-join input. The decision routes through the ONE gate authority
	// (existsOuterGatesFresh → ordinalWedgeGateDecide).
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

	pred := f.Predicate
	if u, ok := f.Input.(*logical.LogicalUnnest); ok && u.CorrelatedCollection != nil && pred != nil {
		// The semantic scope exposes the scalar element as a one-column local
		// virtual source, so E resolves as a source-relative baked E#0 leaf.
		// Explode itself flows the scalar QOV(E); collapse that wrapper exactly
		// as the lateral-join path does or every primitive predicate evaluates
		// through a field read on a scalar and filters all rows.
		pred = rewriteUnnestPredicate(pred, u)
	}
	var preds []predicates.QueryPredicate
	if pred != nil {
		preds = []predicates.QueryPredicate{pred}
	}
	return t.exactFilter(
		preds,
		t.namedQuantifier(sourceAlias(f.Input), innerRef),
	)
}

// translateUnnestExistsFilter composes a lateral array UNNEST in the FROM list
// with a WHERE EXISTS (`SELECT v FROM t, t.arr AS v WHERE [v > 100 AND] EXISTS
// (…)`). The unnest stays its OWN FlatMap-over-Explode (it CANNOT be flattened
// into the existential peel's binary NLJ — a correlated Explode in a
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
// admitExistentialGather is the admission predicate for the
// under-EXISTS box unnest — metadata-only, computed PRE-translation (a decline
// is never poisoned by translation side state). See the
// unnestExistentialGatherOK field doc: a non-INNER
// LEFT/RIGHT box at verdict ∈ {None, Bakeable} is admitted (a None shape's
// merge is a no-op; a Bakeable box-leg conjunct bakes over the gather's
// recorded legTypes at the EXISTS merge site). FULL rides the certified binary
// seed (already producer-free) until FULL+Bakeable is supported.
func (t *cascadesTranslator) admitExistentialGather(join *logical.LogicalJoin, f *logical.LogicalFilter, verdict boxConjVerdict) bool {
	if len(f.ExistsSubqueries) == 0 {
		return false
	}
	// MULTI-ESQ admission: the gathered wrap `[ForEach(seed), ∃, ∃]` peels in
	// PartitionSelectRule to a NestedLoopJoin, which drops the seed's windowed
	// layout. The INNER-flat-cluster seed and the LEFT/RIGHT/FULL BOX seed BOTH
	// physicalize this way — for the box, translateUnnestExistsFilter's
	// multiEsqPeelBox arm bakes each existential correlation positionally at plan
	// time (the E-1a authority) so the peel keeps ∃ with the seed rather than
	// stranding a leg-relative name ref. So every box kind admits multi-esq below.
	// FULL rides the same peel; only FULL+None SINGLE-esq stays on the certified
	// binary seed (already producer-free).
	multiEsq := len(f.ExistsSubqueries) > 1
	// No inner/outer scope collision — a colliding unminted inner alias would
	// get refs meaning the INNER row baked onto the outer window (silent wrong
	// rows); the binary seed already declines on this, the gather must too.
	if t.unnestExistsScopeCollision {
		return false
	}
	// Shape arm: box or INNER flat cluster left.
	bj, isBoxJoin := join.Left.(*logical.LogicalJoin)
	if !isBoxJoin {
		return false
	}
	switch bj.Kind {
	case logical.JoinLeft, logical.JoinRight:
		// A multi-esq LEFT/RIGHT box is admitted: its `[seed, ∃, ∃]` wrap peels via
		// PartitionSelectRule to a NestedLoopJoin, and the translator BAKES each
		// existential correlation positionally at plan time (the multiEsqPeelBox arm
		// in translateUnnestExistsFilter), so the peel keeps ∃ with the seed instead
		// of stranding a leg-relative name ref.
		return verdict == boxConjNone || verdict == boxConjBakeable
	case logical.JoinInner:
		// E-1a: the INNER flat N-way cluster under EXISTS. Its existential
		// correlation leg refs are BAKED alias-aware in the translator (over the
		// gathered seed's OrdinalSeedLegWindows), NOT left leg-relative for the
		// executor hoist — the INNER cluster physicalizes to a NestedLoopJoin whose
		// result value drops the windowed layout, so the hoist recovers 0 windows
		// (the box physicalizes to a FlatMap that keeps it). Verdict-None only.
		// An aggregate over this gather ORDINALIZES, but ONLY the DIRECT shape (see the
		// under-aggregate gate above): E-1a's former blanket decline masked a nil-seedQOV
		// bug, not an NLJ limit. gatheredSeedBakeContext peels the single existential wrapper
		// to reach the seed's element slot + leg windows, so COUNT(X)/SUM(A.K)/GROUP BY bake
		// correctly. A CTE/DISTINCT-WRAPPED aggregate qualifies its group keys with the
		// wrapper alias (unbakeable over the seed windows), so the direct-gate declines it to
		// name-model (correct rows) rather than partial-baking a silent collapse.
		// E-1b: a Bakeable leg conjunct (`… A.K = 100 AND EXISTS(…)`) is
		// admitted too — it bakes IN-SELECT over the re-derivable gatedJoinLegTypes
		// (the WHERE-path channel), not the box's recorded machinery. Unbakeable
		// stays name-model (correct-or-loud).
		// INNER flat cluster: multi-esq is ADMITTED — the gathered wrap physicalizes
		// via the existential peel (pinned by the box multi-EXISTS-gather FDB
		// integration tests).
		return verdict == boxConjNone || verdict == boxConjBakeable
	default: // FULL: only a Bakeable box-leg conjunct gathers; FULL+None
		// stays on the certified binary seed (already producer-free).
		if multiEsq {
			// A multi-esq FULL box gathers like LEFT/RIGHT: its wrap peels to NLJ
			// and the existential correlations bake positionally at plan time
			// (multiEsqPeelBox). FULL+None single-esq stays on the certified binary
			// seed, but a multi-esq FULL peels the same as LEFT/RIGHT.
			return verdict == boxConjNone || verdict == boxConjBakeable
		}
		return verdict == boxConjBakeable
	}
}

// windowedOrdinalSeed reports whether a translated unnest SelectExpression carries the
// WINDOWED ORDINAL seed — the gather actually ran and produced a positional
// RecordConstructorValue with per-leg windows — versus an ANCHORED name-model binary
// seed (the gather was prevEnclosure-skipped, e.g. this cluster used as a name-model
// CTE leg, or never admitted). The E-1b in-select conjunct bake and the E-1a
// alias-aware correlation bake are BOTH valid ONLY over the windowed seed; over an
// anchored seed they leave leg refs unbound (0 rows). It also returns the seed's merged
// RecordType for the E-1a bake. This is the direct-seed proof the box arm gets for free
// from its recorded map (written only when the gather ran).
func windowedOrdinalSeed(sel *expressions.SelectExpression) (bool, *values.RecordType) {
	rc, isRC := sel.GetResultValue().(*values.RecordConstructorValue)
	if !isRC {
		return false, nil
	}
	w, mt := values.OrdinalSeedLegWindows(rc)
	return w != nil, mt
}

func (t *cascadesTranslator) translateUnnestExistsFilter(
	f *logical.LogicalFilter,
	join *logical.LogicalJoin,
	u *logical.LogicalUnnest,
) expressions.RelationalExpression {
	// Lower the unnest leg (validates the array field; records a faithful
	// diagnostic + returns nil for an invalid unnest, e.g. AT-on-a-non-array).
	// unnestUnderExistential is set so unnestExistsSeedSafe applies the single-alias
	// SCOPE gate: a single-source unnest under EXISTS ordinalizes
	// (takes the ordinal seed) and leaves the EXISTS correlation LEG-RELATIVE for the
	// executor's positional rebase. A MULTI-ALIAS outer stays name-model.
	outerAliases := outerBoundAliases(join.Left)
	prevCollision := t.unnestExistsScopeCollision
	t.unnestExistsScopeCollision = existsInnerScopeCollidesOuter(f.ExistsSubqueries, outerAliases)
	prevOuterConj := t.unnestBoxLegConjunct
	prevGatherOK := t.unnestExistentialGatherOK
	// Classify the box-leg conjunct (metadata-only, mirroring
	// the non-EXISTS WHERE-merge arm in translateFilter) and compute the gather admission. A
	// verdict-None LEFT/RIGHT box (single esq — a multi-esq box wrap strands, no
	// collision) takes the gathered ordinal cluster; the INNER cluster gathers at any
	// esq count; everything else keeps the name-model binary seed, where a box-leg
	// conjunct resolves via qualified keys.
	verdict := boxConjNone
	if nonExistsConjunctRefsOuterLeg(f.Predicate, outerAliases) {
		verdict = boxConjUnbakeable
		if bj, isBoxJoin := join.Left.(*logical.LogicalJoin); isBoxJoin {
			// E-1b: an INNER flat cluster's leg conjunct classifies via
			// the flat arm (top-level leg windows); a box's via the box arm (buried
			// leaves). Both feed admitExistentialGather's Bakeable admission.
			if bj.Kind == logical.JoinInner {
				verdict = t.classifyFlatLegConjunct(bj, u, f.Predicate)
			} else {
				verdict = t.classifyBoxLegConjunct(bj, u, f.Predicate)
			}
		}
	}
	t.unnestBoxLegConjunct = verdict
	t.unnestExistentialGatherOK = t.admitExistentialGather(join, f, verdict)
	prevUnderExist := t.unnestUnderExistential
	t.unnestUnderExistential = true
	unnestExpr := t.translateUnnestJoin(join, u)
	t.unnestUnderExistential = prevUnderExist
	t.unnestExistsScopeCollision = prevCollision
	t.unnestBoxLegConjunct = prevOuterConj
	gatheredHere := t.unnestExistentialGatherOK
	t.unnestExistentialGatherOK = prevGatherOK
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
		// An ADMITTED Bakeable box conjunct bakes over the
		// gather's RECORDED legTypes — the FUNCTIONAL twin of the non-EXISTS
		// WHERE-merge arm's record handling in translateFilter, with EXISTS-context adaptations (the gatheredHere
		// guard — EXISTS admission can hold while the gather was
		// enclosure-skipped, which the WHERE path has no gate for — and the
		// multi-`nonExists`-pred iteration appending to `merged`). The gather recorded its box-leg types;
		// each conjunct bakes onto its buried window
		// (ofOrdinal(QOV($BOX), leafOffset+idx)), consume-once (delete on read
		// so a shared-node retranslation whose gather declined can never bake
		// over a stale record), with the predicateRefsBuriedLeg loud assert
		// behind it (the verdict guaranteed bakeability; a survivor would
		// strand as a silent-NULL name read — loud internal error instead).
		bj, isInnerCluster := join.Left.(*logical.LogicalJoin)
		isInnerCluster = isInnerCluster && bj.Kind == logical.JoinInner
		// Element-rewritten non-EXISTS conjuncts — the shared input BOTH gathered-bake
		// arms fold; only the legTypes differ (box RECORDED vs INNER RE-DERIVED).
		rewritten := make([]predicates.QueryPredicate, 0, len(nonExists))
		for _, p := range nonExists {
			rewritten = append(rewritten, rewriteUnnestPredicate(p, u))
		}
		// seedWindowed: the gather ACTUALLY ran and produced the positional ordinal seed
		// (vs an ANCHORED name-model binary seed — the gather was prevEnclosure-skipped
		// while EXISTS admission still held). The box arm gets this proof for free from
		// its recorded map (written only when the gather ran); the INNER arm re-derives
		// its windows, so it MUST check the seed directly — else it bakes the conjunct
		// over an anchored seed and the leg ref strands unbound → 0 rows (review-caught:
		// the enclosed-CTE-leg regression, this cluster used as a name-model CTE leg).
		seedWindowed, _ := windowedOrdinalSeed(sel)
		if recorded, hasRecord := t.unnestGatherBoxLegTypes[join]; hasRecord && gatheredHere {
			delete(t.unnestGatherBoxLegTypes, join)
			toMerge, drift := bakeGatedJoinPredicatesChecked(rewritten, recorded)
			if drift {
				t.setTranslateErr(api.NewErrorf(api.ErrCodeInternalError,
					"box-leg conjunct reference survived the merge bake unbaked (verdict/bake drift)"))
				return nil
			}
			merged = append(merged, toMerge...)
		} else if isInnerCluster && gatheredHere && seedWindowed {
			// E-1b: an ADMITTED INNER flat cluster (verdict None or a
			// Bakeable leg conjunct) whose gather PRODUCED THE WINDOWED SEED bakes the
			// conjunct IN-SELECT over the cluster's leg windows — RE-DERIVED on the spot
			// via gatedJoinLegTypes (the WHERE path's channel), NOT the box's
			// recorded/consume-once machinery: the flat cluster's top-level legs are
			// re-derivable, so no record is needed. A conjunct on the ELEMENT rides
			// rewriteUnnestPredicate (no leg ref, so the bake leaves it, its element
			// already rewritten). The seedWindowed guard is load-bearing: without it an
			// enclosure-skipped anchored seed bakes the conjunct over unbound legs.
			legTypes := t.gatedJoinLegTypes(bj)
			toMerge, drift := bakeGatedJoinPredicatesChecked(rewritten, legTypes)
			// Safety net (CORRECT-or-name-model): the classifier guaranteed Bakeable, but
			// bakeGatedJoinPredicatesChecked reports a should-bake leg ref that failed to
			// positionally resolve (verdict/bake drift — FLAT or buried, the E-1b twin of
			// E-1a's unnestExistsRefSurvivesUnbaked). Decline to the name-model binary
			// path (correct today); the box loud-errors because it already consumed its
			// record, but E-1b always has a name-model fallback, so decline-to-correct is
			// the better disposition. (predicateRefsBuriedLeg alone was inert for a
			// flat-leg survivor — review-caught.)
			if drift {
				return nil
			}
			merged = append(merged, toMerge...)
		} else {
			// Anchored name-model seed (not admitted / no record): the
			// multi-source outer-leg rebase translateFilter applies — a
			// non-EXISTS WHERE on an outer-leg column of a ≥2-prior-source
			// unnest references QOV(A), which the inner Explode does not bind;
			// rebase it to the qualified `A.c` key off the merged outer QOV.
			// RFC-142. This is the correct and now ONLY domain of the
			// name-keyed rebase.
			mergedCorr := values.NamedCorrelationIdentifier(sourceAlias(join.Left))
			outerLegs := unnestOuterLegAliases(join.Left, mergedCorr)
			mergedType, _ := sel.GetResultValue().Type().(*values.RecordType)
			for _, p := range nonExists {
				rebased, ok := rebaseUnnestOuterLegPredicate(rewriteUnnestPredicate(p, u), outerLegs, mergedCorr, mergedType, UnnestLegMintSiteAnchoredNonExists)
				if !ok {
					return nil
				}
				merged = append(merged, rebased)
			}
		}
		unnestExpr = t.exactSelectWithJoinType(
			sel.GetResultValue(),
			sel.GetQuantifiers(),
			merged,
			sel.GetSourceAliases(),
			sel.GetJoinType(),
		)
	}
	// Record hygiene (B2-B): a verdict-None gather writes a box-leg record no
	// merge above read (nonExists was empty) — discard it so a shared-node
	// double-reference can never bake over a stale record. A Bakeable merge
	// already consumed (deleted) it above, so this is then a no-op.
	if gatheredHere && t.unnestGatherBoxLegTypes != nil {
		delete(t.unnestGatherBoxLegTypes, join)
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
	// (the executor's mergeRows, now the sole authority), exactly as a non-unnest
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
	// The EXISTS correlation's outer-leg refs, routed by whether
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
	if s, ok := unnestExpr.(*expressions.SelectExpression); ok {
		seedWindowed, ordMergedType = windowedOrdinalSeed(s)
	}
	// E-1a: a windowed INNER flat cluster (admitted by
	// admitExistentialGather) bakes BOTH the JoinPredicate channel AND the buried
	// (esq.Plan) channel alias-aware in the translator — the hoist cannot recover
	// the NLJ's dropped windows. The element-slot map is the seed's own layout.
	lj, isInnerLJ := join.Left.(*logical.LogicalJoin)
	isInnerCluster := seedWindowed && isInnerLJ && lj.Kind == logical.JoinInner
	// A MULTI-esq box (LEFT/RIGHT/FULL) is admitted to the gather but,
	// unlike a single-esq box, its `[seed, ∃, ∃]` wrap goes through the existential
	// PEEL (PartitionSelectRule) → NestedLoopJoin, which DROPS the windowed layout
	// exactly like the INNER cluster. The executor's below-FOD hoist can then no
	// longer recover the windows, so a LEG-RELATIVE JoinPredicate would strand
	// unbound (the box+seed lost in a mis-peel). The correlation must therefore be
	// BAKED positionally at plan time — the same E-1a authority the INNER cluster
	// uses — which also gives the peel a proper sibling-quantifier correlation edge
	// (∃ → seed), so the peel keeps each existential with the seed. A SINGLE-esq box
	// stays a FlatMap that keeps the layout, so it stays leg-relative (hoist rebases).
	multiEsqPeelBox := seedWindowed && isInnerLJ && lj.Kind != logical.JoinInner && len(f.ExistsSubqueries) > 1
	planTimeBake := isInnerCluster || multiEsqPeelBox
	var innerElementSlots map[string]int
	if planTimeBake {
		innerElementSlots = unnestSeedElementSlots(unnestExpr)
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
			//     exactly the shape the disabled-build probe binds positionally.
			//     The translator is the SINGLE rebase authority for buried refs
			//     (the hoist never sees them), so this cannot double-rebase.
			//   - ANCHORED seed: the qualified "LEG.COL" read off the merged
			//     binding — the same rebase the JoinPredicate channel gets.
			if lf, isLF := esq.Plan.(*logical.LogicalFilter); isLF &&
				len(lf.ExistsSubqueries) == 0 && len(lf.ScalarSubqueries) == 0 &&
				predicateIsOuterOnly(lf.Predicate, outerBoundAliases(lf.Input)) {
				var rebased predicates.QueryPredicate
				armCensus := unnestLegMintEnabled()
				if armCensus {
					RecordUnnestLegMintBranchReached(UnnestLegMintSiteBuriedNotWindowed)
				}
				if planTimeBake {
					if armCensus {
						RecordUnnestLegMintArm(UnnestLegMintSiteBuriedNotWindowed, UnnestLegMintArmPlanTimeBake)
					}
					// E-1a: a BURIED outer-only predicate over the INNER cluster (or a
					// multi-esq peel box) may reference a leg OR the ELEMENT (`… WHERE
					// X = 7` — an outer-only conjunct with no inner-table ref that
					// buildCorrelatedExists keeps here, review-caught). Bake BOTH
					// channels + the safety net.
					baked, ok := t.bakeInnerExistsPredicateOrdinal(lf.Predicate, join.Left, innerElementSlots, ordMergedType, outerLegs, mergedCorr)
					if !ok {
						return nil
					}
					rebased = baked
				} else if seedWindowed {
					if armCensus {
						RecordUnnestLegMintArm(UnnestLegMintSiteBuriedNotWindowed, UnnestLegMintArmOrdinalTwin)
					}
					baked, ok := rebaseUnnestOuterLegPredicateOrdinal(lf.Predicate, t.ordinalLegType(join.Left), ordMergedType, outerLegs, mergedCorr)
					if !ok {
						// CORRECT-or-LOUD: an outer ref the seed's outer leg type
						// cannot map is never a valid correlation — decline the
						// whole composition rather than ship a half-baked tree.
						return nil
					}
					// The buried conjunct may ALSO reference the unnest ELEMENT
					// (`EXISTS (… WHERE MA.C = X)` — outer-only relative to the
					// EXISTS inner, mixing a leg ref and the element). Below the
					// FOD only the merged outer row is bound, so the element ref
					// must bake to its seed slot exactly like the leg refs —
					// unbaked it read an unbound correlation, the comparison
					// went NULL, and EXISTS silently dropped every row (R5m/R5n).
					baked = bakeUnnestElementRefOrdinal(baked, unnestSeedElementSlots(unnestExpr), mergedCorr, ordMergedType)
					if unnestExistsRefSurvivesUnbaked(baked, outerLegs, mergedCorr) {
						// The safety net (the E-1a floor): a surviving outer/element
						// ref would mis-resolve silently — decline instead.
						return nil
					}
					rebased = baked
				} else {
					if armCensus {
						RecordUnnestLegMintArm(UnnestLegMintSiteBuriedNotWindowed, UnnestLegMintArmName)
					}
					var ok bool
					rebased, ok = rebaseUnnestOuterLegPredicate(lf.Predicate, outerLegs, mergedCorr, ordMergedType, UnnestLegMintSiteBuriedNotWindowed)
					if !ok {
						return nil
					}
				}
				esq.Plan = &logical.LogicalFilter{
					Input:                      lf.Input,
					Predicate:                  rebased,
					PredicateText:              lf.PredicateText,
					ExistsSubqueries:           lf.ExistsSubqueries,
					ScalarSubqueries:           lf.ScalarSubqueries,
					CorrelatedScalarSubqueries: lf.CorrelatedScalarSubqueries,
				}
			}
			// The JoinPredicate channel: name-model rebase for an anchored seed;
			// leg-relative for a windowed BOX ordinal seed (the executor's below-FOD
			// hoist rebases it positionally — RULE-level, unlike the buried refs).
			// For a windowed INNER flat CLUSTER (E-1a) the hoist CANNOT recover the
			// windows — the cluster physicalizes to a NestedLoopJoin whose result
			// value drops the windowed layout (the box keeps it via a FlatMap) — so
			// the translator BAKES the leg ref alias-aware over the seed's windows
			// here, exactly as the buried refs above (ordinalSlotInLegWindow resolves
			// each dup-named leg's own slot).
			jpCensus := unnestLegMintEnabled()
			if jpCensus {
				RecordUnnestLegMintBranchReached(UnnestLegMintSiteJoinPredNotWindowed)
			}
			if !seedWindowed {
				if jpCensus {
					RecordUnnestLegMintArm(UnnestLegMintSiteJoinPredNotWindowed, UnnestLegMintArmName)
				}
				var ok bool
				esq.JoinPredicate, ok = rebaseUnnestOuterLegPredicate(esq.JoinPredicate, outerLegs, mergedCorr, ordMergedType, UnnestLegMintSiteJoinPredNotWindowed)
				if !ok {
					return nil
				}
			} else if planTimeBake {
				if jpCensus {
					RecordUnnestLegMintArm(UnnestLegMintSiteJoinPredNotWindowed, UnnestLegMintArmPlanTimeBake)
				}
				// Bake the existential correlation's LEG refs (alias-aware, dup-named
				// disambiguation) AND the ELEMENT ref (`EEV.VK = X` — the merged
				// corr's own slot, only merged-corr/bare refs, NOT an inner-table
				// QOV with the same col name) onto the merged QOV, with the safety
				// net. Correct-or-loud decline → name-model binary path.
				baked, ok := t.bakeInnerExistsPredicateOrdinal(esq.JoinPredicate, join.Left, innerElementSlots, ordMergedType, outerLegs, mergedCorr)
				if !ok {
					return nil
				}
				esq.JoinPredicate = baked
			} else if jpCensus {
				// The windowed, non-plan-time-bake fall-through: this channel rebases
				// NOTHING and leaves the refs leg-relative for the executor's below-FOD
				// hoist. Recorded so the branch's arms PARTITION — without it a zero on
				// the name arm cannot be told from a branch nothing reaches.
				RecordUnnestLegMintArm(UnnestLegMintSiteJoinPredNotWindowed, UnnestLegMintArmLegRelative)
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
// key the unnest FlatMap output carries (the executor's mergeRows). This is the
// query-package twin of the cascades NLJ rule's rebaseOuterLegRefsToMerged (the
// real-JOIN+EXISTS path); both turn an outer-leg-qualified residual into a read
// off the existential outer's merged binding. References to the unnest element
// (the merged corr itself) or to the existential inner pass through untouched.
// RFC-142.
// site names WHICH of the five callers reached it, so the unnest leg-mint
// census can report them apart. The five are not one arm — three sit in an
// explicit `!seedWindowed` / `!ordinalSeed` else-branch and two apply no seed
// test at all — and a conversion driven by flipping a seed test moves only the
// first three. It is a PARAMETER rather than a call-site counter so that a new
// caller has to state which population it joins.
func rebaseUnnestOuterLegPredicate(
	p predicates.QueryPredicate,
	outerLegs map[string]struct{},
	mergedCorr values.CorrelationIdentifier,
	mergedType *values.RecordType,
	site UnnestLegMintSite,
) (predicates.QueryPredicate, bool) {
	// Counted BEFORE the inert guard, deliberately. A site that is reached with
	// nothing to rewrite and a site that is never reached print the same zero
	// once the guard has run, and they mean opposite things: the first is a live
	// arm carrying no traffic, the second is dead code.
	census := unnestLegMintEnabled()
	if census {
		RecordUnnestLegMintCall(site)
	}
	if p == nil || len(outerLegs) == 0 {
		return p, true
	}
	needsRebase := false
	for correlation := range predicates.GetCorrelatedToOfPredicate(p) {
		if _, isOuterLeg := outerLegs[strings.ToUpper(correlation.Name())]; isOuterLeg {
			needsRebase = true
			break
		}
	}
	// A record-valued UNNEST element makes the seed non-windowed, so there is
	// no merged row type to name-key. That is irrelevant when this predicate
	// reads only the element and existential inner: no outer-table leg needs a
	// rewrite. Preserve it verbatim; an actual outer-leg read still requires the
	// exact merged carrier below and fails closed when that carrier is absent.
	if !needsRebase {
		return p, true
	}
	if mergedType == nil {
		return p, false
	}
	mergedQOV, qovErr := values.NewQuantifiedObjectValue(mergedCorr, mergedType)
	if qovErr != nil {
		return p, false
	}
	ok := true
	rewrite := func(v values.Value) values.Value {
		if v == nil {
			return v
		}
		return values.Replace(v, func(node values.Value) values.Value {
			fv, isField := values.AsFieldValue(node)
			if !isField {
				return node
			}
			qov, hasOwner := values.AsQuantifiedObjectValue(fv.ChildValue())
			if !hasOwner {
				return node
			}
			leg := strings.ToUpper(qov.Correlation().Name())
			if _, isOuterLeg := outerLegs[leg]; !isOuterLeg {
				return node
			}
			// Read the qualified "LEG.COL" key off the merged unnest output. The
			// field already carries a bare column name here (resolved against the
			// outer table source), so prefix it with the leg alias.
			path := fv.Path().Ordinals()
			root, hasRoot := fv.Path().Accessor(0)
			if !hasRoot || len(path) == 0 {
				ok = false
				return node
			}
			rootName, hasName := root.DisplayName()
			if !hasName {
				ok = false
				return node
			}
			minted := leg + "." + strings.ToUpper(rootName)
			if census {
				RecordUnnestLegMintName(site, minted)
			}
			slot, found := mergedType.FieldIndexUnique(minted)
			if !found {
				ok = false
				return node
			}
			resolvedPath := append([]int{slot}, path[1:]...)
			resolved, err := values.ResolveFieldOrdinals(mergedQOV, resolvedPath)
			if err != nil || resolved.Type() == nil || !resolved.Type().Equals(fv.ResultType()) {
				ok = false
				return node
			}
			return resolved
		})
	}
	return mapPredicateValues(p, rewrite), ok
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
	chainCensus := unnestLegMintEnabled()
	if chainCensus {
		RecordUnnestLegMintBranchReached(UnnestLegMintSiteChainedNameModel)
	}
	if ordinalSeed {
		if chainCensus {
			RecordUnnestLegMintArm(UnnestLegMintSiteChainedNameModel, UnnestLegMintArmOrdinalTwin)
		}
		return rebaseUnnestOuterLegPredicateOrdinal(p, ordType, ordType, outerLegs, mergedCorr)
	}
	if chainCensus {
		RecordUnnestLegMintArm(UnnestLegMintSiteChainedNameModel, UnnestLegMintArmName)
	}
	return rebaseUnnestOuterLegPredicate(p, outerLegs, mergedCorr, ordType, UnnestLegMintSiteChainedNameModel)
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
// windows agree.
//
// SCOPE OF THE MULTI-ALIAS BRANCH, stated because the previous note here was
// FALSE and manufactured work that did not exist: it claimed the branch was
// "wired but scope-gated OFF end-to-end (unnestExistsSeedSafe keeps multi-alias
// outers name-model)". It is LIVE. unnestExistsSeedSafe's terminal disjunct is
// `len(outerBoundAliases(left)) == 1 || t.boxGatesFresh(left)`, so a multi-alias
// FULL OUTER box that gates fresh is ADMITTED and reaches this branch with
// rt.Legs populated. What the branch SERVES is a fresh-gating OUTER box
// (LEFT/RIGHT/FULL) whose legs are simple; what it DECLINES is a multi-alias
// INNER cluster and a box whose leg exposes a buried outer box. Those declines
// are DELIBERATE and permanent-until-verified, not a guard awaiting a lift: an
// INNER cluster's seed cannot express the flattened multi-source outer, and a
// buried outer box has no recorded [Start,Width) bounds, so building positional
// over either yields an unrebased ref rather than a slower plan. See
// boxGatesFresh for the exclusion list and TestMultiAliasOuterGatesOrdinal /
// TestMultiSourceInnerClusterDeclines for the admit and decline pins.
//
// multiAlias is the CALLER's structural fact (len(outerLegs) > 1). The flat
// whole-row name fallback is legitimate ONLY for a single-alias prefix (the
// pristine-prefix-at-offset-0 case, where every column belongs to the one
// alias). A MULTI-alias prefix that arrives WITHOUT leg windows must DECLINE:
// the flat fallback would silently first-match a dup-named column across
// aliases — a qualified B.ID resolving to A's slot 0, observed as
// `WHERE B.ID = 20` admitting {A.ID:20, B.ID:null} rows. Correct-or-loud at
// the resolution site: the decline keeps the whole class fail-closed even if
// a window-propagation gap upstream ever leaves Legs empty.
func ordinalSlotInLegWindow(rt *values.RecordType, leg values.CorrelationIdentifier, field string, multiAlias bool) (int, bool) {
	if rt == nil {
		return 0, false
	}
	if len(rt.Legs) > 0 {
		for _, lw := range rt.Legs {
			if values.LegIdentityCensusEnabled() {
				// RETIRED PREDICATE: `lw.Name == strings.ToUpper(leg.Name())` — this
				// function took the qualifier as a string and every caller passed the
				// UPPER fold of the correlation. Recording the verdict is what makes the
				// conversion's claim checkable: a fold-only count over the identity pair
				// sees a match that became a decline, but NOT a decline that became a
				// match (a lowercase machine-minted leg matches itself exactly while the
				// folding predicate rejected it, and that pair lands in ExactEqual).
				values.RecordLegIdentityConversion(
					values.LegSiteOrdinalSlotInLegWindow, lw.Alias, leg,
					lw.Name == strings.ToUpper(leg.Name()))
				values.RecordLegIdentityLeg(lw)
			}
			// "Does this correlation name that leg window?" — the same IDENTITY
			// question every other Group-A reader asks, so it gets the same answer
			// the same way. This used to compare the leg's TEXT against the UPPER
			// fold of the correlation, which is a text match dressed as an identity
			// check: it happened to agree because buried legs are user aliases and
			// those are upper-folded at the semantic scope's registration
			// chokepoint, but the agreement was a property of today's producers
			// rather than of the comparison.
			if !values.SameLeg(lw.Alias, leg) {
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
	return rt.FieldIndexUnique(field)
}

// rebaseUnnestOuterLegPredicateOrdinal bakes outer-table-leg references in a
// BURIED subquery predicate to FrontierPinned ofOrdinals over the typed merged
// QOV — the ordinal-seed twin of rebaseUnnestOuterLegPredicate, for the ONE
// channel the executor's positional hoist cannot reach: a predicate INSIDE an
// existential inner plan (the under-∃ placement of a subquery-internal
// outer-only conjunct). For a single-alias outer the leg is the pristine merged
// PREFIX at offset 0, so an outer column's ordinal in the outer leg type IS its
// merged-row ordinal. The multi-alias branch (a
// box with rt.Legs, ordinal resolved WITHIN the qualifier's leg window via
// ordinalSlotInLegWindow so a dup-named column bakes the right alias's slot) is
// WIRED AND LIVE — the note that used to sit here calling it "scope-gated OFF
// end-to-end (unnestExistsSeedSafe keeps multi-alias outers name-model)" was
// FALSE and is corrected in place, because two investigations planned a
// guard-lift off it. unnestExistsSeedSafe ends in
// `len(outerBoundAliases(left)) == 1 || t.boxGatesFresh(left)`, so a fresh-gating
// multi-alias OUTER box is ADMITTED (TestMultiAliasOuterGatesOrdinal pins it).
// The shapes that stay name-model are a multi-alias INNER cluster and a box with
// a buried outer-box leg, and those are deliberate declines rather than a
// pending lift — see ordinalSlotInLegWindow's scope note and boxGatesFresh.
// The baked ref is exactly the
// shape the disabled-build probe binds positionally below the FOD. The
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
	// Counted BEFORE the inert guard, for the same reason its name-keyed twin is:
	// a call that finds nothing to rebase and a call that never happens print the
	// same zero afterwards, and the two mean opposite things about whether this
	// arm is the one the corpus takes.
	if unnestLegMintEnabled() {
		RecordUnnestLegOrdinalTwinCall()
	}
	if p == nil || len(outerLegs) == 0 {
		return p, true // genuinely nothing to rebase
	}
	if outerLegType == nil || mergedType == nil {
		return p, false // no positional authority to bake against — fail closed
	}
	mergedQOV, qovErr := values.NewQuantifiedObjectValue(mergedCorr, mergedType)
	if qovErr != nil {
		return p, false
	}
	ok := true
	rewrite := func(v values.Value) values.Value {
		if v == nil {
			return v
		}
		return values.Replace(v, func(node values.Value) values.Value {
			fv, isFV := values.AsFieldValue(node)
			if !isFV {
				return node
			}
			qov, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue())
			if !isQOV {
				return node
			}
			leg := strings.ToUpper(qov.Correlation().Name())
			if _, isOuterLeg := outerLegs[leg]; !isOuterLeg {
				return node
			}
			// Resolve the slot WITHIN the qualifier's per-leg window when the outer
			// carries leg boundaries (rt.Legs — a MULTI-ALIAS outer like a FULL OUTER
			// box): a qualified ref to a dup-named column must pick THAT leg's slot,
			// not the flat first-match across the whole merged prefix (which silently
			// reads the OTHER alias's same-named column).
			// A single-alias outer has no rt.Legs and falls back to a flat whole-row
			// name lookup.
			// The MAP lookup above is keyed by upper text (the outerLegs set is built
			// from the same text channel), but the WINDOW resolution below is an
			// identity question and takes the correlation itself.
			// A DESCENT cannot be rebased by name. The slot this resolves is the
			// slot of a whole column in the leg's window, and the bake below
			// An exact ordinal field access keeps only that ordinal — the accessor
			// suffix is dropped. For a multi-accessor reference that is a read of
			// the struct ROOT where a member was named, and it is silent: the name
			// offered to the window is one segment of the path, so it either misses
			// (and the rebase declines, which is fine) or hits a DIFFERENT column
			// that happens to share the leaf's spelling, which is not.
			//
			// Declining is the fail-closed direction this function already uses for
			// an unresolvable slot, and it is the correct one: a rebase that cannot
			// carry the suffix must not pretend it did.
			path := fv.Path().Ordinals()
			root, hasRoot := fv.Path().Accessor(0)
			if !hasRoot || len(path) == 0 {
				ok = false
				return node
			}
			rootName, hasName := root.DisplayName()
			if !hasName {
				ok = false
				return node
			}
			ord, found := ordinalSlotInLegWindow(outerLegType, qov.Correlation(), strings.ToUpper(rootName), len(outerLegs) > 1)
			if !found {
				ok = false
				return node
			}
			suffix := make([]values.FieldRequest, 0, len(path)-1)
			for _, suffixOrdinal := range path[1:] {
				request, requestErr := values.FieldByOrdinal(suffixOrdinal)
				if requestErr != nil {
					ok = false
					return node
				}
				suffix = append(suffix, request)
			}
			baked, err := values.ResolveOrdinalSeedAccess(mergedQOV, ord, suffix)
			if err != nil || baked.Type() == nil || !baked.Type().Equals(fv.ResultType()) {
				ok = false
				return node
			}
			return baked
		})
	}
	np := mapPredicateValues(p, rewrite)
	return np, ok
}

// bakeUnnestElementRefOrdinal bakes an existential predicate's ELEMENT references
// (the unnest AS alias) to FrontierPinned ofOrdinals at the element's own seed
// slot over mergedQOV. It rewrites ONLY the element: a BARE ref (no QOV child)
// whose column names an element slot, OR one qualified by the MERGED/unnest corr.
// An inner-table QOV whose column happens to match an element alias (`A.arr AS CK`
// with inner `EE.CK`) is LEFT UNTOUCHED — it resolves in the existential inner
// (review-caught: a nil-windows bakeGatheredGroupValue mis-baked it to the element
// slot). Leg refs are the caller's job (rebaseUnnestOuterLegPredicateOrdinal); an
// already-Resolved node (a leg ref the prior pass baked) is skipped.
func bakeUnnestElementRefOrdinal(
	p predicates.QueryPredicate,
	elementSlots map[string]int,
	mergedCorr values.CorrelationIdentifier,
	mergedType *values.RecordType,
) predicates.QueryPredicate {
	if p == nil || len(elementSlots) == 0 || mergedType == nil {
		return p
	}
	mergedQOV, qovErr := values.NewQuantifiedObjectValue(mergedCorr, mergedType)
	if qovErr != nil {
		return p
	}
	mergedName := strings.ToUpper(mergedCorr.Name())
	rewrite := func(v values.Value) values.Value {
		if v == nil {
			return v
		}
		return values.Replace(v, func(node values.Value) values.Value {
			fv, isFV := values.AsFieldValue(node)
			// An UNPINNED baked element ref re-bakes to its seed slot like its
			// lazy twin; machinery-owned (pinned) nodes are final.
			//
			// Keyed on the ROOT's leg-relativity, NOT on SourceRelativeBaked:
			// the narrower predicate additionally demands a SINGLE accessor, so
			// it waved through a MEMBER of a struct element (`x.ek`, `x.d.dk` —
			// a two-accessor unpinned path). Such a ref then reached the merged
			// row still addressing the EXISTS scope's own layout, the comparison
			// went NULL, and EXISTS dropped every row SILENTLY — the exact
			// failure the single-accessor arm below was written to prevent, one
			// accessor deeper. The safety net shared the same narrow predicate,
			// so it reported the tree clean.
			if !isFV || fv.Path().IsFrontierPinned() {
				return node
			}
			qov, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue())
			if !isQOV || strings.ToUpper(qov.Correlation().Name()) != mergedName {
				return node // inner-table or other QOV — not the element
			}
			// The slot is named by the path's ROOT, which is the element alias;
			// fv.Field is the display LEAF and names the MEMBER on a descent.
			path := fv.Path().Ordinals()
			root, hasRoot := fv.Path().Accessor(0)
			if !hasRoot || len(path) == 0 {
				return node
			}
			rootName, hasName := root.DisplayName()
			if !hasName {
				return node
			}
			slot, ok := elementSlots[strings.ToUpper(rootName)]
			if !ok {
				return node
			}
			suffix := make([]values.FieldRequest, 0, len(path)-1)
			for _, suffixOrdinal := range path[1:] {
				request, requestErr := values.FieldByOrdinal(suffixOrdinal)
				if requestErr != nil {
					return node
				}
				suffix = append(suffix, request)
			}
			baked, err := values.ResolveOrdinalSeedAccess(mergedQOV, slot, suffix)
			if err != nil || baked.Type() == nil || !baked.Type().Equals(fv.ResultType()) {
				return node
			}
			return baked
		})
	}
	return mapPredicateValues(p, rewrite)
}

// unnestExistsRefSurvivesUnbaked reports whether any OUTER-leg or ELEMENT (merged
// corr) FieldValue survives UNBAKED (Resolved==nil) in a baked INNER-cluster
// existential predicate — the E-1a safety net (the box path's twin
// predicateRefsBuriedLeg assert). Over the INNER cluster's NLJ layout such a ref
// mis-resolves SILENTLY (0 rows), so the caller declines to name-model rather than
// ship a half-baked tree. Inner-table refs pass through (they resolve inside ∃).
func unnestExistsRefSurvivesUnbaked(
	p predicates.QueryPredicate,
	outerLegs map[string]struct{},
	mergedCorr values.CorrelationIdentifier,
) bool {
	mergedName := strings.ToUpper(mergedCorr.Name())
	survives := false
	scanVal := func(v values.Value) {
		if v == nil || survives {
			return
		}
		values.WalkValue(v, func(node values.Value) bool {
			fv, isFV := values.AsFieldValue(node)
			// An UNPINNED baked ref mis-resolves over the NLJ layout exactly
			// like a lazy one — it SURVIVES; only machinery-owned (PINNED)
			// baked nodes are safe.
			//
			// Keyed on the ROOT's leg-relativity, NOT on SourceRelativeBaked,
			// which also demanded a SINGLE accessor and so declared a
			// struct-element MEMBER ref (`x.ek`, two unpinned accessors) safe.
			// That is the shape whose bake the sibling above was missing, so
			// the one instrument that could have caught the silent 0-row drop
			// was blind to precisely it.
			if !isFV || fv.Path().IsFrontierPinned() {
				return true // pinned ofOrdinal (or non-FieldValue) — descend/skip
			}
			if qov, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue()); isQOV {
				leg := strings.ToUpper(qov.Correlation().Name())
				if leg == mergedName {
					survives = true
					return false
				}
				if _, isLeg := outerLegs[leg]; isLeg {
					survives = true
					return false
				}
			}
			return true
		})
	}
	// Read-only walk (no throwaway tree): WalkPredicate descends And/Or/Not; the
	// value-carrying leaves are ComparisonPredicate + ValuePredicate (mirroring
	// mapPredicateValues' value sites).
	predicates.WalkPredicate(p, func(qp predicates.QueryPredicate) bool {
		switch pred := qp.(type) {
		case *predicates.ComparisonPredicate:
			scanVal(pred.Operand)
			scanVal(pred.Comparison.Operand)
		case *predicates.ValuePredicate:
			scanVal(pred.Value)
		}
		return !survives
	})
	return survives
}

// bakeInnerExistsPredicateOrdinal bakes an INNER-cluster existential correlation
// predicate's OUTER references — leg columns (alias-aware, dup-named
// disambiguation via ordinalSlotInLegWindow) AND the unnest element — to
// positional ofOrdinals over the gathered seed's mergedType QOV. E-1a's single
// bake authority: the INNER cluster physicalizes to a NestedLoopJoin whose result
// value drops the windowed layout, so the executor's below-FOD hoist recovers no
// windows — the translator bakes both channels' refs directly. ok=false (caller
// declines to the name-model binary path, CORRECT-or-LOUD) when a leg ref cannot
// map, OR the safety net finds an outer/element ref that survived unbaked.
func (t *cascadesTranslator) bakeInnerExistsPredicateOrdinal(
	p predicates.QueryPredicate,
	leftJoin logical.LogicalOperator,
	elementSlots map[string]int,
	mergedType *values.RecordType,
	outerLegs map[string]struct{},
	mergedCorr values.CorrelationIdentifier,
) (predicates.QueryPredicate, bool) {
	if p == nil {
		return p, true
	}
	baked, ok := rebaseUnnestOuterLegPredicateOrdinal(p, t.ordinalLegType(leftJoin), mergedType, outerLegs, mergedCorr)
	if !ok {
		return p, false
	}
	baked = bakeUnnestElementRefOrdinal(baked, elementSlots, mergedCorr, mergedType)
	if unnestExistsRefSurvivesUnbaked(baked, outerLegs, mergedCorr) {
		return p, false
	}
	return baked, true
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
		// B1: a gated arity>=3 non-dup INNER cluster
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
		innerCorrName, joinPred := t.existsInnerCorrelation(esq)
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
		// THE UNTYPED-QOV DIVERGENCE IS MINTED HERE, not at the FlatMap
		// constructions that were previously credited with it.
		//
		// Java builds a simple select's result value as
		// overQuantifier.getFlowedObjectValue() (GraphExpansion.java:401), which is
		// typed unconditionally — QuantifiedObjectValue.of has no untyped overload
		// (QuantifiedObjectValue.java:187) and Quantifier.getFlowedObjectType is a
		// Verify.verify plus requireNonNull (Quantifier.java:801-810). Java cannot
		// express what this line builds.
		//
		// The three cascades sites that hand this value to a RecordQueryFlatMapPlan
		// flow it verbatim, exactly as Java's three constructions flow
		// selectExpression.getResultValue() (ImplementNestedLoopJoinRule.java:187,
		// 201, 214), so their untyped counts are a count of COURIERS. This mint is
		// the author, and it is counted here so the divergence is booked against a
		// measurement instead of against an inference.
		flowed, err := outerQ.RequireFlowedObjectValue()
		if err != nil {
			t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"existential outer quantifier has no exact flowed object: %v", err))
			return nil
		}
		resultValue = flowed
		values.RecordSelectResultMint(values.SelectResultMintExistsSelect, resultValue)
	}
	return t.exactSelectWithAliases(
		resultValue,
		quantifiers,
		allPreds,
		sourceAliases,
	)
}

// isScanFamilyLeg reports whether a logical leg is a single scan source
// (optionally under filters), unwrapping Filter/Fetch/FetchOnDemand to a
// Scan/Index base. It is the SOLE gate on this decision: it began as the logical
// proxy for a physical leg walk that made the same call, and that walk retired
// with the three-quantifier NLJ arm (RFC-235), leaving no second opinion to
// diverge from. The F2-LEFT projected-EXISTS fold ordinalizes ONLY when both
// legs are scan-family; a join/unnest/union/aggregate leg (a buried box) is not
// and must decline rather than fall to the name-model producer path. Conservative:
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

// existsLegBuildsPositional reports whether a projected-EXISTS fold leg is a
// bare all-INNER cluster of scan-family leaves. Such a leg is translated
// UN-ENCLOSED (AXIS 1) so its INNER box is born as a mergeable ordinal select;
// SelectMergeRule can then expose the flat `[ForEach×N, Existential]` form.
// RFC-190 routes that form through PartitionSelectRule's alias-aware
// decomposition and ordinary binary NLJ implementation; the former generalized
// the three-quantifier arm is retired. A scan leg is already positional
// regardless of enclosure (returns false — no box to un-enclose); an OUTER box
// or a wrapped join returns false (non-mergeable / out of INNER-first scope).
func existsLegBuildsPositional(op logical.LogicalOperator) bool {
	j, isJoin := op.(*logical.LogicalJoin)
	if !isJoin || j.Kind != logical.JoinInner {
		return false
	}
	return existsLegAllInnerScanFamily(j.Left) && existsLegAllInnerScanFamily(j.Right)
}

// existsLegAllInnerScanFamily is the recursion arm of existsLegBuildsPositional:
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
// projected fold (the LEFT case emits a JoinLeftOuter select — the F2-LEFT
// arm); a LEFT with a non-scan (buried-box) leg, and RIGHT/FULL, are left
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
		// projected EXISTS cleanly. LEFT DOES fold (the F2-LEFT arm): the box
		// dissolves to INNER + null-on-empty and the executor builds the positional
		// seed with the null-supplying leg NULL-filled — Java folds and answers it
		// (live-verified 4.12.11.0).
		return nil
	}
	if j.Kind == logical.JoinLeft && (!isScanFamilyLeg(j.Left) || !isScanFamilyLeg(j.Right)) {
		// F2-LEFT is SCAN-leg scope only. A buried box
		// `(a JOIN b) LEFT JOIN c` — any non-scan preserved/null-supplying leg —
		// does not ordinalize — no ordinal path admits a non-scan leg here — so
		// folding it would fall through to a
		// name-model path with a null-extended name-keyed row: correct today
		// via the row's name-keyed Datum, but that path is slated for removal.
		// Decline (→ §8 → clean 0AF00) as a reach gap (Java answers it; Go rejects)
		// rather than mint a new name-model producer. INNER keeps its buried-box
		// behavior (name-keyed, no null-extension); the asymmetry is deliberate —
		// the LEFT null-extension is exactly what the ordinal seed must carry.
		return nil
	}
	if j.Kind == logical.JoinLeft && f != nil && len(splitNonExistsPredicates(f.Predicate)) > 0 {
		// F2-LEFT is the NO-WHERE shape. A non-EXISTS WHERE predicate over
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
	// RFC-190 190.1 direct-emit: an INNER cluster of ≥3 legs is dissolved into a
	// flat NAME-model [ForEach×N, Existential] select (QueryVisitor.java:429-434
	// port, alias-bound). PartitionSelectRule decomposes it by alias.
	if j.Kind == logical.JoinInner && f != nil {
		legs := t.gatherInnerClusterLegs(j)
		if len(legs) > 2 {
			ops := make([]logical.LogicalOperator, len(legs))
			for i, l := range legs {
				ops[i] = l.op
			}
			if mintedBindingLeg(ops...) != "" {
				return nil
			}
			prevEnc := t.inInnerCluster
			quants := make([]expressions.Quantifier, 0, len(legs)+len(f.ExistsSubqueries))
			srcAliases := make([]string, 0, len(legs)+len(f.ExistsSubqueries))
			for _, leg := range legs {
				t.inInnerCluster = true
				legRef := t.translateRef(leg.op)
				t.inInnerCluster = prevEnc
				if legRef == nil {
					return nil
				}
				quants = append(quants, expressions.NamedForEachQuantifier(
					values.NamedCorrelationIdentifier(leg.binding), legRef))
				srcAliases = append(srcAliases, leg.binding)
			}
			var preds []predicates.QueryPredicate
			preds = append(preds, t.gatherInnerClusterOnPredicates(j)...)
			preds = append(preds, splitNonExistsPredicates(f.Predicate)...)
			preds = append(preds, extractExistsPredicates(f.Predicate)...)
			for _, esq := range f.ExistsSubqueries {
				subRef := t.translateSubqueryRef(esq.Plan)
				if subRef == nil {
					return nil
				}
				quants = append(quants, expressions.NamedExistentialQuantifier(esq.Alias, subRef))
				innerCorrName, joinPred := t.existsInnerCorrelation(esq)
				if joinPred != nil {
					preds = append(preds, joinPred)
				}
				srcAliases = append(srcAliases, innerCorrName)
			}
			return t.exactSelectWithAliases(resultValue, quants, preds, srcAliases)
		}
	}
	// Same enclosure as translateJoinWithExists — the
	// existential flatten is a name-model parent; its ForEach legs are enclosed.
	// AXIS 1: a bare INNER gated-box fold leg is translated
	// UN-ENCLOSED so its box is born as a mergeable ordinal select; SelectMergeRule
	// flattens it into the fold, making the fold an N-way [ForEach×N, Existential]
	// select that PartitionSelectRule decomposes into ordinary binary NLJs while
	// preserving the live existential. A non-bare-INNER-box leg (scan, OUTER box,
	// wrapped) stays enclosed.
	prevEnclosure := t.inInnerCluster
	t.inInnerCluster = !existsLegBuildsPositional(j.Left)
	leftRef := t.translateRef(j.Left)
	if leftRef == nil {
		t.inInnerCluster = prevEnclosure
		return nil
	}
	t.inInnerCluster = !existsLegBuildsPositional(j.Right)
	rightRef := t.translateRef(j.Right)
	t.inInnerCluster = prevEnclosure
	if rightRef == nil {
		return nil
	}
	// The fold's quantifiers and source aliases
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
		innerCorrName, joinPred := t.existsInnerCorrelation(esq)
		if joinPred != nil {
			allPreds = append(allPreds, joinPred)
		}
		sourceAliases = append(sourceAliases, innerCorrName)
	}

	// F2-LEFT: a LEFT-outer FROM join folds as a JoinLeftOuter select
	// — RewriteOuterJoinRule dissolves the p×q box into INNER + a null-on-empty
	// q quantifier, and the executor builds the positional seed with q NULL-filled
	// on non-matching outer rows (the projected EXISTS then reads that NULL).
	foldJoinType := expressions.JoinInner
	if j.Kind == logical.JoinLeft {
		foldJoinType = expressions.JoinLeftOuter
	}
	return t.exactSelectWithJoinType(
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
	// This method is an early-return consumer that bypasses translateFilter.
	// A correlated-scalar carrier on f therefore cannot be left for the normal
	// LEFT-scalar lowering: the EXISTS fold would consume only f's existential
	// and uncorrelated-scalar lists and leave ScalarSubqueryValue unbound.
	// Composition has no proven row-shape contract yet, so make the boundary
	// explicit and typed-loud.
	if len(f.CorrelatedScalarSubqueries) > 0 {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"projected EXISTS with a correlated scalar subquery in WHERE is not yet supported"))
		return nil
	}

	// A projected ExistsValue is a distinct consumer whose alias must stay bound
	// by the existential quantifier. Do not let the WHERE-marker fold below
	// remove that subquery and then fall back to an ordinary projection with an
	// unbound ExistsValue. Value substitution is not implemented yet, so retain
	// the explicit correct-or-loud guard before rewriting f.
	if projectionReferencesExistsSubquery(p.ProjectedValues) &&
		hasKnownExistsTruth(f.ExistsSubqueries) {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"a projected cardinality-known EXISTS is not yet supported"))
		return nil
	}

	// This fold is an early-return path that bypasses translateFilter, so perform
	// the same cardinality-known WHERE substitution here before building any
	// existential quantifier. This is load-bearing for the widened arity>=3
	// gathered-cluster path: a plain projection over that shape reaches this
	// method even when the SELECT list does not contain an ExistsValue.
	//
	// Top-level WHERE markers are removed (or absorb the predicate as FALSE).
	// A known truth that remains is a distinct, unsupported consumer/boolean
	// position (projected ExistsValue, nested OR, ...); raw-semi-joining its
	// fallback plan would ignore aggregate/pagination cardinality, so reject
	// typed-loud. If every existential was folded, return nil and let the
	// ordinary project path translate p.Input: translateFilter will perform the
	// identical fold on the original logical tree.
	f = t.foldKnownExists(f)
	if hasKnownExistsTruth(f.ExistsSubqueries) {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"a cardinality-known EXISTS in this projection-fold position is not yet supported"))
		return nil
	}
	if len(f.ExistsSubqueries) == 0 {
		return nil
	}

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

	// This projected-EXISTS fold does NOT force enclosure on f.Input.
	// An earlier `t.inInnerCluster = true` here was a name-model producer that
	// has since been retired. It was never load-bearing: f.Input
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
	for i, col := range p.Projections {
		var v values.Value
		if i < len(p.ProjectedValues) && p.ProjectedValues[i] != nil {
			v = p.ProjectedValues[i]
		} else {
			// A logical projection is required to carry either an exact Value or
			// purpose-specific non-Value metadata consumed by its owning
			// translator. The EXISTS fold has no separate slot metadata.
			return nil
		}
		// Verbatim, like every other output-name authority: `SELECT "id" AS
		// "x", EXISTS(…) AS "e"` reported X and E here while the plain
		// projection beside it reported x and e, because this arm mints the
		// FlatMap's result type on its own.
		name := col
		if i < len(p.Aliases) && p.Aliases[i] != "" {
			name = p.Aliases[i]
		} else if _, isField := values.AsFieldValue(v); !isField {
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
	src := t.classifySortSource(f.Input)

	// F2-LEFT: Java's Cascades cannot plan ANY ORDER BY over the LEFT
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
			// A NESTED key whose source column cannot be built is unfoldable, and it
			// must bail HERE rather than fall through: it is nameable, so the
			// nameability check below would wave it past, and collectExtraSortColumns
			// would then skip it for a nil value — appending no hidden column and
			// leaving the re-applied sort reading a field the folded record lacks.
			// That is a SILENT wrong order, which is strictly worse than the loud
			// failure it replaces. Declining yields a clean unsupported error.
			if _, isNested := nestedResolvedSortKey(k); isNested {
				if src.sortKeySourceValue(k) == nil {
					return nil
				}
				// SETTLED, and it must not fall through to the rendered name.
				// A nested key over a join renders THREE segments (`T1.N.SK`) and
				// sortKeyName's split takes the LAST dot, manufacturing the
				// qualifier `T1.N` — which contradicts the identity the key is
				// holding (`T1`) and is the wrong-answer population the qualifier
				// recovery census asserts at zero. The key's own correlation is
				// that identity, sortKeySourceValue used it, and there is nothing
				// left for a rendering to decide.
				continue
			}
			if src.sortKeyName(k) != "" {
				continue // a nameable column — appended as a hidden field or already in output
			}
			if k.Value == nil {
				return nil // computed via raw ORDER BY text, not nameable → unfoldable
			}
			if _, ok := outputFieldValueIndex(k.Value, fields); !ok {
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
			limitExpr, err := newLimitExprFromLogical(op, expressions.ForEachQuantifier(ref))
			if err != nil {
				t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
					"projected EXISTS LIMIT has no exact flowed row: %v", err))
				return nil
			}
			expr = limitExpr
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
		cleanupQ := expressions.ForEachQuantifier(expressions.InitialOf(expr))
		cleanupRow, err := cleanupQ.RequireFlowedObjectValue()
		if err != nil {
			t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"projected-EXISTS cleanup has no exact flowed row: %v", err))
			return nil
		}
		projVals := make([]values.Value, outputCount)
		projAliases := make([]string, outputCount)
		projMinted := make([]bool, outputCount)
		projSources := make([]values.ProjectionAliasSource, outputCount)
		for i := 0; i < outputCount; i++ {
			// FieldValue.Field MUST equal the fold's f.Name exactly: the folded
			// output record is keyed by f.Name and FieldValue.Evaluate does an
			// exact-key lookup (no qualified→bare fallback). The cleanup column's
			// datum Name then equals that key, so a Scan never reads NULL.
			// The cleanup column reads the fold's output slot i
			// (the fold RC's first outputCount fields ARE the output columns in
			// order) — baked, so it resolves positionally on the folded row.
			resolved, resolveErr := values.ResolveFieldOrdinals(cleanupRow, []int{i})
			if resolveErr != nil {
				t.setTranslateErr(resolveErr)
				return nil
			}
			projVals[i] = resolved
			// Reuse the original SELECT-list alias (""==unaliased) so the cleanup's
			// label derivation matches the non-hidden-sort path exactly.
			if i < len(p.Aliases) {
				projAliases[i] = p.Aliases[i]
			}
			// The alias is reused, so its PROVENANCE is reused with it —
			// truncated to the same outputCount. Copying the name without the
			// marker would relabel a machinery datum key as something the user
			// asked for.
			if i < len(p.AliasMinted) {
				projMinted[i] = p.AliasMinted[i]
			}
			if i < len(p.AliasSources) {
				projSources[i] = p.AliasSources[i]
			}
		}
		outputNames, outputErr := exactLogicalProjectionOutputNames(p, projVals)
		if outputErr != nil {
			t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"projected-EXISTS cleanup has no exact logical output schema: %v", outputErr))
			return nil
		}
		expr = t.exactProjectionWithOutputSchema(
			projVals, projAliases, projMinted, projSources, outputNames, cleanupQ)
		if expr == nil {
			return nil
		}
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
	// legTypes are the per-leg flowed layouts, parallel to legAliases
	// (ordinalLegType; nil entries for underivable legs), and singleType the
	// single-table layout — the layout authorities that let
	// sortKeySourceValue emit BAKED source-column references. Baked, the
	// value-based output-membership match (sortKeyInOutput /
	// pullUpToOutputField) compares equal against the resolver's baked
	// projection values (baked vs lazy is UNEQUAL by the identity refinement),
	// and an appended hidden sort column resolves positionally at eval.
	legTypes   []*values.RecordType
	singleType *values.RecordType
}

// classifySortSource inspects the fold's FROM input. A binary INNER LogicalJoin
// is a join source (its two legs flow under qualified merged-row keys); anything
// else (a single scan, a CTE/derived table) is single-table. Only INNER joins
// reach the projected-EXISTS fold (buildExistentialJoinSelect rejects outer
// joins), so we classify only that shape as a join.
func (t *cascadesTranslator) classifySortSource(input logical.LogicalOperator) sortSource {
	if j, ok := input.(*logical.LogicalJoin); ok && j.Kind == logical.JoinInner {
		// BINDING correlations, not display aliases: the fold's quantifiers
		// and merged-row keys carry the
		// binding (buildExistentialJoinSelect), and a duplicate-alias sort
		// key resolves to a binding-qualified value — display-named
		// legAliases ([A, A]) failed to attribute it and the key silently
		// degraded to the bare last-leg-wins read. Identical to the alias
		// for every non-duplicate leg. BURIED bindings under a box
		// leg join the set (structural walk) — a sort key qualified by a
		// buried source (`ORDER BY d.id` over `(dept LEFT emp) JOIN cat`)
		// must attribute to ITS leg, never degrade to the bare
		// first-match read (which silently sorted by the wrong column).
		var legAliases []string
		var legTypes []*values.RecordType
		var collect func(op logical.LogicalOperator)
		collect = func(op logical.LogicalOperator) {
			if cj, isJ := op.(*logical.LogicalJoin); isJ {
				collect(cj.Left)
				collect(cj.Right)
				return
			}
			if b := sourceBinding(op); b != "" {
				legAliases = append(legAliases, b)
				// The leg's flowed layout (nil when underivable — the
				// source value then stays lazy, loud at eval).
				legTypes = append(legTypes, t.ordinalLegType(op))
			}
		}
		collect(j.Left)
		collect(j.Right)
		return sortSource{isJoin: true, legAliases: legAliases, legTypes: legTypes}
	}
	return sortSource{isJoin: false, singleType: t.ordinalLegType(input)}
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
	// The identity is read for the CENSUS alone — resolveKeyName's answer does
	// not depend on it — so the gate is hoisted above it. Census-off this is one
	// atomic load; the call it guards allocates a ToUpper per sort key.
	var ident string
	var present bool
	if values.LegIdentityCensusEnabled() {
		ident, present = sortKeyQualifierIdentity(k)
	}
	return s.resolveKeyName(field, ident, present)
}

// sortKeyQualifierIdentity returns the STRUCTURED qualifier a sort key carries,
// and whether it carries one at all. Those are different facts and the second is
// not derivable from the first: a key that states an UNQUALIFIED reference
// carries the identity "" and a key that states nothing also carries "", and
// they mean opposite things — the first says "the parser saw one segment", the
// second says "nobody captured what the parser saw".
//
// Two channels, in precedence order, because they are two different strengths of
// evidence:
//
//   - A resolved FieldValue over a QuantifiedObjectValue. This is the STRONGEST,
//     and it is the one that makes the EXISTS fold's split a round trip:
//     sortKeyFieldRef RENDERS `LEG.COL` out of exactly this correlation, and
//     splitQualifier then slices that rendering back apart. The identity was
//     never lost — it was joined and re-parsed.
//   - The parse-tree triple the key carries (Bare/Qualifier/Qualified), under
//     SortKey's own convention that a populated Bare marks a captured triple.
func sortKeyQualifierIdentity(k logical.SortKey) (string, bool) {
	if fv, ok := values.AsFieldValue(k.Value); ok {
		if qov, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue()); isQOV {
			return strings.ToUpper(qov.Correlation().Name()), true
		}
	}
	if k.Bare != "" {
		return strings.ToUpper(k.Qualifier), true
	}
	return "", false
}

// sortKeyFieldRef returns the RAW (possibly-qualified) upper-cased field reference
// a column sort key names — `T1.ID`, `COL1` — or "" when the key is a computed
// expression. Unlike sortKeyName it does NOT strip the qualifier, so callers can
// (a) build the source-column VALUE the key references for value-based output
// membership, and (b) name an appended hidden field by the qualified provenance
// (collision-free with an output alias — RFC-141 R4 P2b).
func sortKeyFieldRef(k logical.SortKey) string {
	if fv, ok := values.AsFieldValue(k.Value); ok {
		// A composite leg reference (FieldValue{col, QOV(leg)}) — render LEG.COL.
		return strings.ToUpper(values.ColumnNameValue(fv))
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
	// Resolver Values are already exact and source-owned. Reconstructing them
	// from rendered names was needed only for the retired childless carrier; it
	// can change both the owner and a nested path. A missing Value is unresolved
	// metadata and makes the EXISTS fold inapplicable.
	if k.Value != nil {
		return k.Value
	}
	return nil
}

// legIndexOfNestedKey attributes a nested sort key to one of the join's legs.
//
// IDENTITY, never the rendered name. A nested key's own reference hangs off the
// leg's QuantifiedObjectValue, so the correlation IS the answer — and it is the
// only channel that survives a duplicate display alias, which classifySortSource
// collects BINDING correlations precisely to handle. Splitting a rendered
// `T1.N.SK` on a dot would be the RFC-197 channel this whole surface removes: the
// last-dot split yields `SK`, and the first yields a qualifier that a self-join
// spells identically for both legs.
//
// Declines (ok=false) when the key states no correlation. That is not a
// theoretical arm — an unqualified nested key over a single-table source carries
// no QOV at all — and there is no correct guess: the bare-name fallback the flat
// arm uses is last-leg-wins, which is the silent wrong-column read.
func (s sortSource) legIndexOfNestedKey(fv values.FieldValue) (int, bool) {
	qov, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue())
	if !isQOV {
		return 0, false
	}
	for li, leg := range s.legAliases {
		if leg == "" {
			continue
		}
		if values.SameLeg(values.NamedCorrelationIdentifier(leg), qov.Correlation()) {
			return li, true
		}
	}
	return 0, false
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
		if f.Value != nil && values.SemanticEqualsUnderAliasMap(src, f.Value, values.EmptyAliasMap()) {
			return f.Name, true
		}
	}
	return "", false
}

// resolveKeyName maps a (possibly qualified) sort-key field reference to the name
// it resolves against the folded output record, per the source's key shape.
func (s sortSource) resolveKeyName(field string, ident string, identPresent bool) string {
	up := strings.ToUpper(field)
	if !s.isJoin {
		return stripSortQualifier(up)
	}
	recordExistsSortSplit(up, ident, identPresent)
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
// projection: a NAME (the qualified field reference for a flat key, the resolved
// PATH for a nested one) and the source-column VALUE it reads (bare for
// single-table, qualified leg ref for a JOIN).
//
// The name is HYGIENE, not the identity. It keeps a hidden column from being
// spelled like an output alias sharing the bare column name (RFC-141 R4 P2b) and
// from two struct members being spelled alike, and it is the DISPLAY name the
// re-applied sort renders into EXPLAIN. What actually resolves a sort key onto
// its column is the VALUE match in pullUpToOutputField, which emits a baked
// ORDINAL — so a name that collided anyway (a quoted identifier can contain a
// dot) still resolves correctly. Identity is the value; the name is what a human
// reads.
type extraSortCol struct {
	name string
	val  values.Value
}

// collectExtraSortColumns returns the hidden columns to append to the folded
// projection: the ORDER BY columns whose SOURCE column is NOT already projected
// by an output field (Java's remainingOrderByExpressions). Membership is
// VALUE-based (sortKeyInOutput) — a key is "in output" only when an output field
// genuinely projects its source column, never merely sharing a bare name with an
// output alias. A sort key whose column can't be named (a computed expression) is
// skipped here — the caller (translateProjectOverExistsFilter) has already bailed
// the fold for any computed key absent from the projection, so a computed key
// reaching this point is a SELECTED expression that pulls up to its own output
// field.
//
// Order is stable and de-duplicated BY VALUE, never by the appended column's name
// (see extraSortColOfValue). Naming is a separate concern handled by
// sortKeyExtraColumnName: a flat key keeps its QUALIFIED field reference so it
// cannot collide with an output column, and a nested key is named by its resolved
// PATH so two members of one struct root are not spelled alike.
func collectExtraSortColumns(chain []logical.LogicalOperator, fields []values.RecordConstructorField, src sortSource) []extraSortCol {
	var extra []extraSortCol
	for _, op := range chain {
		s, ok := op.(*logical.LogicalSort)
		if !ok {
			continue
		}
		for _, k := range s.Keys {
			name := sortKeyExtraColumnName(k)
			if name == "" {
				continue
			}
			if _, inOutput := src.sortKeyInOutput(k, fields); inOutput {
				continue
			}
			// The VALUE is computed BEFORE identity is asserted, because the
			// value IS the identity. The dedup this replaced consulted a
			// rendered name first and so decided on a spelling it had not yet
			// earned the right to trust: two nested keys of one struct root
			// both render the root, and the second key's column was dropped as
			// a duplicate of the first while reading a different member.
			val := src.sortKeySourceValue(k)
			if val == nil {
				continue
			}
			if extraSortColOfValue(extra, val) >= 0 {
				continue
			}
			extra = append(extra, extraSortCol{name: name, val: val})
		}
	}
	return extra
}

// extraSortColOfValue returns the index of an already-collected hidden column
// that reads the SAME source value, or -1. This is Java's membership test —
// Expressions.difference over canBeDerivedFrom (Expressions.java:124-146,
// Expression.java:254-264) — and it is deliberately the same comparison
// sortKeyInOutput applies against the OUTPUT, applied here among the appended
// columns themselves.
//
// THE EQUALITY IS SYMMETRIC AND MUST STAY SYMMETRIC. Java's derivation test is
// ASYMMETRIC and is sound only in the direction Java uses it: an order-by
// expression against the OUTPUT. Applied among the extras it inverts — for
// `ORDER BY n, n.sk` the member IS derivable from its struct root, so an
// asymmetric test would drop `n.sk`'s column and recreate exactly the defect
// this function was repaired for, with a Java citation attached to it. Anyone
// "upgrading" this to canBeDerivedFrom for closer Java alignment is reintroducing
// the bug.
//
// Equality separates the two keys structurally, not by name: EqualsWithoutChildren
// compares baked FieldValues by their full ordinal PATH, so `n.co` and `n.sk`
// share the Field `N` and stay distinct. The CHILDREN recursion is load-bearing
// on the join arm and must not be optimised away — two legs whose struct sits at
// the same ordinals have EQUAL paths, and only the QOV correlation child keeps
// `t1.n.sk` from merging with `t2.n.sk`.
func extraSortColOfValue(extra []extraSortCol, val values.Value) int {
	for i, e := range extra {
		if e.val != nil && values.SemanticEqualsUnderAliasMap(val, e.val, values.EmptyAliasMap()) {
			return i
		}
	}
	return -1
}

// sortKeyExtraColumnName names the hidden column a sort key gets appended as.
//
// A NESTED key is named by its RESOLVED PATH (`N.CO`), never by sortKeyFieldRef's
// flat rendering, which for an unqualified nested key returns a SINGLE segment of
// the path — the LEAF, `CO`. (fv.Child is nil, so the ToUpper(fv.Field) arm
// fires. Childlessness is what selects that arm and it says nothing about how
// many segments the key has: the source-relative mint fuses a whole descent onto
// a childless root, so a nested key reaches this arm routinely.) The flat name is
// a faithful answer to "what does this key spell" and a wrong answer to "which
// column is this", which is the question an appended field's name is asked. Two
// members of one struct root would otherwise be spelled alike in a single
// RecordConstructorValue — and a leaf name additionally collides with any
// top-level column sharing its spelling, which is a different column of a
// possibly different type.
//
// The nested-over-JOIN shape already renders the path (fv.Child is the leg's QOV,
// so sortKeyFieldRef takes the ColumnNameValue branch), and this recomputes the
// identical string there. FLAT keys keep their qualified provenance name
// (`T1.ID`), which is what keeps a hidden column from shadowing an output alias
// sharing the bare column name.
func sortKeyExtraColumnName(k logical.SortKey) string {
	if path, nested := values.NestedResolvedPath(k.Value); nested {
		return path
	}
	return sortKeyFieldRef(k)
}

// nestedResolvedSortKey reports whether a sort key carries a multi-accessor
// resolved path — THE definition of "this key is nested", read by every site that
// needs to know.
//
// It exists as one function because the predicate was hand-copied at three sites
// and a fourth was about to be added. That drift is not hypothetical: the arms
// that made a nested key carry a distinct per-member value were added ABOVE the
// name derivation without the naming site learning about it, which is precisely
// how two keys came to read different columns while being named the same.
// The predicate itself now lives in values.NestedResolvedPath, so the sort side
// and the projection/group-key sides cannot disagree about what "nested" means;
// this stays as the SortKey-shaped wrapper its three structural callers need.
func nestedResolvedSortKey(k logical.SortKey) (values.FieldValue, bool) {
	if _, nested := values.NestedResolvedPath(k.Value); !nested {
		return nil, false
	}
	return values.AsFieldValue(k.Value)
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
// recordExistsSortSplit files one splitQualifier decision into the qualifier
// recovery census. It is a free function rather than a wrapper around
// splitQualifier itself because the two callers reach the split through
// different paths and only they hold the sort key whose structured identity is
// the counterparty; a recorder inside splitQualifier would have to report every
// call as MANUFACTURED and would erase the one thing this site is measured for.
//
// The classification is delegated so this site cannot bucket its own
// disagreement as "no counterparty".
//
// The gate is read FIRST. Unlike its siblings this helper's own body is
// allocation-free, so the gate here buys little; the cost this site actually
// imposes census-off is sortKeyQualifierIdentity's ToUpper at the CALLER, and
// that is why it hoists the gate above the helper rather than relying on this
// one.
func recordExistsSortSplit(up, ident string, identPresent bool) {
	if !values.LegIdentityCensusEnabled() {
		return
	}
	class, _ := values.ClassifyQualifierRecovery(up, ident, identPresent)
	witnessIdent := ident
	if !identPresent {
		witnessIdent = ""
	} else if ident == "" {
		// An identity that is PRESENT and unqualified is not the same as an
		// absent one, and a witness rendering both as "" would hide exactly the
		// pair a DIVERGED reading turns on.
		witnessIdent = "<unqualified>"
	}
	values.RecordQualifierRecovery(values.QualRecSiteExistsSortSplit, class, up, witnessIdent)
}

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
	sortQ := expressions.ForEachQuantifier(ref)
	outputQOV, err := sortQ.RequireFlowedObjectValue()
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"folded sort input has no exact flowed row: %v", err))
		return nil
	}
	sortKeys := make([]expressions.SortKey, len(s.Keys))
	for i, k := range s.Keys {
		nf := k.NullsFirst
		v := k.Value
		// A POSITIONAL key (`ORDER BY <n>`) IS an output ordinal by SQL
		// definition — bake slot n-1 of the folded projection's output
		// directly (see translateSort's twin; a resolved typed Value wins
		// for the same reason as there), stating the layout it indexes.
		//
		// UNREACHABLE over the 2481-query corpus: upgradeSortKeyValues resolves
		// a positional key into k.Value upstream, so k.Value is non-nil by the
		// time a positional key arrives here. That resolution is what makes this
		// arm dead; relaxing it re-arms the arm.
		if k.Value == nil && k.Pos > 0 && k.Pos <= len(fields) {
			v, err = values.ResolveFieldOrdinals(outputQOV, []int{k.Pos - 1})
			if err != nil {
				t.setTranslateErr(err)
				return nil
			}
		}
		if v == nil {
			t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"ORDER BY key %q has no resolved Value", k.Expr))
			return nil
		}
		v = pullUpSortKeyValue(k, v, fields, src, outputQOV)
		sortKeys[i] = expressions.SortKey{
			Value:      v,
			Reverse:    k.Dir == logical.SortDesc,
			NullsFirst: &nf,
		}
	}
	return t.exactSort(sortKeys, sortQ)
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
func pullUpSortKeyValue(k logical.SortKey, v values.Value, fields []values.RecordConstructorField, src sortSource, outputQOV values.Value) values.Value {
	// (1) Output-field-value match on the key's RAW value — runs for EVERY key
	// shape, mirroring the normal ORDER BY alias resolution. Handles SELECT-list
	// aliases (incl. the computed EXISTS boolean) whose Value upgradeSortKeyValues
	// set to the projected Value.
	if pulled, ok := pullUpToOutputField(v, fields, outputQOV); ok {
		return pulled
	}
	// (2) Source-column-value match — a column key resolves to the output field
	// (incl. the hidden remainingOrderBy columns) whose VALUE is its source column.
	if srcVal := src.sortKeySourceValue(k); srcVal != nil {
		if pulled, ok := pullUpToOutputField(srcVal, fields, outputQOV); ok {
			return pulled
		}
	}
	// (3) Bare OUTPUT-COLUMN name over the folded output (`ORDER BY id` where
	// `id` is an output column carried through): bake the OUTPUT ordinal at
	// plan time — first-match case-insensitive, the same rule
	// the retired runtime name read (RecordType.FieldIndex) applied, so the
	// baked slot is the very one GetByName found.
	// Already-resolved key (or an outer-row reference): resolves against the
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
func pullUpToOutputField(v values.Value, fields []values.RecordConstructorField, outputQOV values.Value) (values.Value, bool) {
	index, ok := outputFieldValueIndex(v, fields)
	if !ok {
		return nil, false
	}
	resolved, err := values.ResolveFieldOrdinals(outputQOV, []int{index})
	if err != nil {
		return nil, false
	}
	// Structural correspondence identifies the slot, but the materialized
	// output owner remains the type authority. A stale/mismatched output row
	// must not turn (say) a LONG sort expression into a STRING field merely
	// because both occupy ordinal zero.
	if v == nil || v.Type() == nil || resolved.Type() == nil || !resolved.Type().Equals(v.Type()) {
		return nil, false
	}
	return resolved, true
}

func outputFieldValueIndex(v values.Value, fields []values.RecordConstructorField) (int, bool) {
	// Pass 1: exact pointer identity — the field whose Value the sort key IS.
	// The pulled-up reference carries the OUTPUT ordinal, baked at plan time —
	// the folded row is positional.
	for i, f := range fields {
		if f.Value != nil && f.Value == v {
			// The slot's type IS the projected value's type (Phase D:
			// type at birth — the flowed type, never Unknown when known).
			return i, true
		}
	}
	// Pass 2: structural semantic equality — for keys whose Value was rebuilt
	// (not pointer-copied) but is structurally the projected expression.
	for i, f := range fields {
		if f.Value != nil && values.SemanticEqualsUnderAliasMap(v, f.Value, values.EmptyAliasMap()) {
			return i, true
		}
	}
	return -1, false
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

// findCorrelatedScalarFilterUnderUnaryChain is the scalar-carrier counterpart
// of findExistsFilterUnderUnaryChain. It deliberately peers through only the
// same row-shape-preserving Sort/Limit chain. A project carrying its own
// correlated scalar cannot currently compose with a second scalar box in that
// reachable filter: translating one first and relying on the other's arity gate
// to fail is an incidental safety property, not an architectural invariant.
func findCorrelatedScalarFilterUnderUnaryChain(input logical.LogicalOperator) *logical.LogicalFilter {
	cur := input
	for {
		if f, ok := cur.(*logical.LogicalFilter); ok {
			if len(f.CorrelatedScalarSubqueries) > 0 {
				return f
			}
			return nil
		}
		next, ok := logical.FoldTransparentUnaryInput(cur)
		if !ok {
			return nil
		}
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
	branches := make([]expressions.RelationalExpression, 0, len(u.Inputs))
	branchRefs := make([]*expressions.Reference, 0, len(u.Inputs))
	for _, branch := range u.Inputs {
		ref := t.translateRef(branch)
		if ref == nil {
			return nil
		}
		branches = append(branches, ref.Get())
		branchRefs = append(branchRefs, ref)
	}
	if u.Distinct {
		return nil
	}
	commonRow, normalize, err := exactUnionResultRow(branches)
	if err != nil {
		t.setTranslateErr(err)
		return nil
	}
	if normalize {
		for i, branchRef := range branchRefs {
			branches[i] = t.normalizeUnionLeg(branchRef, commonRow)
			if branches[i] == nil {
				return nil
			}
			branchRefs[i] = expressions.InitialOf(branches[i])
		}
	}
	quantifiers := make([]expressions.Quantifier, len(branchRefs))
	for i, branchRef := range branchRefs {
		quantifiers[i] = expressions.ForEachQuantifier(branchRef)
	}
	union, err := expressions.NewLogicalUnionExpression(quantifiers)
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"UNION inputs have no exact common result row: %v", err))
		return nil
	}
	return union
}

// exactUnionResultRow derives the one exact positional row every physical
// UNION leg must publish. SQL takes output names from the first leg and folds
// each column's type through Type.maximumType; branch-local names never
// participate in compatibility. normalize is false only when every translated
// leg already publishes the same exact row, preserving the existing no-op
// shape for the common same-schema case.
func exactUnionResultRow(
	branches []expressions.RelationalExpression,
) (*values.RecordType, bool, error) {
	if len(branches) == 0 {
		return nil, false, api.NewError(api.ErrCodeUnsupportedQuery,
			"UNION has no input branches")
	}
	records := make([]*values.RecordType, len(branches))
	for i, branch := range branches {
		if branch == nil || branch.GetResultValue() == nil {
			return nil, false, api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"UNION branch %d has no exact result Value", i)
		}
		record, ok := branch.GetResultValue().Type().(*values.RecordType)
		if !ok {
			return nil, false, api.NewErrorf(api.ErrCodeUnionIncompatibleColumns,
				"UNION branch %d result is not a record row", i)
		}
		records[i] = record
	}
	width := len(records[0].Fields)
	for i := 1; i < len(records); i++ {
		if len(records[i].Fields) != width {
			return nil, false, api.NewError(api.ErrCodeUnionIncorrectColumnCount,
				"UNION legs do not have the same number of columns")
		}
	}

	fields := make([]values.Field, width)
	for ordinal := 0; ordinal < width; ordinal++ {
		common := records[0].Fields[ordinal].FieldType
		for branch := 1; branch < len(records); branch++ {
			common = values.MaximumType(common, records[branch].Fields[ordinal].FieldType)
			if common == nil {
				return nil, false, api.NewErrorf(api.ErrCodeUnionIncompatibleColumns,
					"UNION column %d has incompatible branch types", ordinal+1)
			}
		}
		name := records[0].Fields[ordinal].Name
		if name == "" {
			name = values.OrdinalFieldName(ordinal)
		}
		fields[ordinal] = values.Field{Name: name, Ordinal: ordinal, FieldType: common}
	}
	commonRow := &values.RecordType{Fields: fields}
	for _, record := range records {
		if !record.Equals(commonRow) {
			return commonRow, true, nil
		}
	}
	return commonRow, false, nil
}

// normalizeUnionLeg re-emits one branch by exact ordinal under the UNION's
// shared row contract. No display name is resolved here: branch-local aliases
// are permitted to disagree, and the flowed row is the sole slot authority.
func (t *cascadesTranslator) normalizeUnionLeg(
	legRef *expressions.Reference,
	commonRow *values.RecordType,
) expressions.RelationalExpression {
	q := expressions.ForEachQuantifier(legRef)
	row, err := q.RequireFlowedObjectValue()
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"UNION leg has no exact flowed row: %v", err))
		return nil
	}
	projected := make([]values.Value, len(commonRow.Fields))
	outputNames := make([]string, len(commonRow.Fields))
	for i, field := range commonRow.Fields {
		resolved, resolveErr := values.ResolveFieldOrdinals(row, []int{i})
		if resolveErr != nil {
			t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"UNION output slot %d does not resolve: %v", i, resolveErr))
			return nil
		}
		projected[i], err = exactUnionSlotValue(resolved, field.FieldType)
		if err != nil {
			t.setTranslateErr(api.NewErrorf(api.ErrCodeUnionIncompatibleColumns,
				"UNION output slot %d cannot adopt the common type: %v", i, err))
			return nil
		}
		outputNames[i] = field.Name
	}
	projection, err := expressions.NewLogicalProjectionExpressionWithOutputSchema(
		projected, nil, nil, outputNames, q)
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"UNION leg normalization has no exact result row: %v", err))
		return nil
	}
	if !projection.GetResultValue().Type().Equals(commonRow) {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"UNION leg normalization produced %s, want %s",
			projection.GetResultValue().Type(), commonRow))
		return nil
	}
	return projection
}

// exactUnionSlotValue injects the implicit promotion to a UNION column's
// maximum type. PromoteValue deliberately preserves a NOT NULL child's
// nullability. When another leg makes the common column nullable, a fixed-arm
// PickValue with an unreachable typed-NULL alternative states that exact
// common CASE result without changing the selected value or using CAST
// semantics.
func exactUnionSlotValue(value values.Value, target values.Type) (values.Value, error) {
	if value == nil || target == nil {
		return nil, fmt.Errorf("source Value or target type is nil")
	}
	result := value
	if !result.Type().Equals(target) {
		if maximum := values.MaximumType(result.Type(), target); maximum == nil || !maximum.Equals(target) {
			return nil, fmt.Errorf("source type %s is not promotable to %s", result.Type(), target)
		}
		result = values.NewPromoteValue(result, target)
	}
	if result.Type().Equals(target) {
		return result, nil
	}
	if !target.IsNullable() || result.Type().IsNullable() ||
		!values.WithNullability(result.Type(), true).Equals(target) {
		return nil, fmt.Errorf("promotion produced %s instead of %s", result.Type(), target)
	}
	return values.NewPickValue(
		values.LiteralValue(int64(0)),
		[]values.Value{result, values.NewNullValue(target)},
		target,
	), nil
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
	windows      map[values.CorrelationIdentifier]values.OrdinalSeedLegWindow
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
// unnestSeedElementSlots returns each gathered-seed ELEMENT field's flat slot
// (its rc index, keyed by UPPER field name) for a windowed unnest cluster SELECT
// — the element-half of the seed's positional layout, the twin of
// gatheredSeedBakeContext's own elementSlots derivation. Used by the E-1a INNER
// cluster to bake an existential ELEMENT correlation (`… = X`) to its slot,
// alongside the leg bake. Empty for a non-windowed / non-explode seed.
// seedElementSlots derives each ELEMENT field's flat slot (its rc index, keyed by
// UPPER field name) from a gathered-cluster SELECT's result value — the ONE
// authority for the element-half of the seed's positional layout, shared by
// unnestSeedElementSlots (the E-1a existential element bake) and
// gatheredSeedBakeContext (the GROUP-BY / sort bake), so the explode-find +
// fieldValueReferencesInner derivation can't drift between them. ok=false for a
// non-windowable seed (star / AnchoredJoin result value / no Explode quantifier).
// It also returns the RC it derives from (the seed result value), so callers that
// need the seed layout (leg windows, row type) reuse the ONE cast rather than an
// unchecked re-assert.
func seedElementSlots(sel *expressions.SelectExpression) (*values.RecordConstructorValue, map[string]int, bool) {
	rc, ok := sel.GetResultValue().(*values.RecordConstructorValue)
	if !ok {
		return nil, nil, false
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
		return nil, nil, false
	}
	slots := map[string]int{}
	for i, f := range rc.Fields {
		if fieldValueReferencesInner(f.Value, innerCorr) {
			slots[strings.ToUpper(f.Name)] = i // element slot = rc index
		}
	}
	return rc, slots, true
}

func unnestSeedElementSlots(unnestExpr expressions.RelationalExpression) map[string]int {
	sel, ok := unnestExpr.(*expressions.SelectExpression)
	if !ok {
		return nil
	}
	_, slots, _ := seedElementSlots(sel)
	return slots
}

func (t *cascadesTranslator) gatheredSeedBakeContext(
	innerRef *expressions.Reference,
	fallbackAlias string,
) (gatheredSeedBake, error) {
	b := gatheredSeedBake{quant: t.namedQuantifier(fallbackAlias, innerRef)}
	// Find the windowed ordinal seed under the aggregate/sort input. E-1a's former
	// under-aggregate decline masked a nil-seedQOV bug, not an NLJ limit: seedElementSlots
	// looks for the Explode as a DIRECT quantifier, but the seed sits under one or more
	// IDENTITY wrappers that preserve its positional row — the EXISTS semi-join FlatMap (a
	// pure filter whose result is the outer quantifier's identity row), a CTE/derived
	// SELECT-* projection, a DISTINCT. Walk the outer-quantifier chain (bounded) to reach
	// it. Its per-leg windows + element slots are read against innerRef's output row, which
	// those identity wrappers pass through unchanged — so group keys / operands (element by
	// slot, leg columns via OrdinalSeedLegWindows) bake positionally. Without this the bake
	// gets a nil seedQOV and reads by name over the ordinal row → collapse.
	rc, elementSlots, ok := findWindowedSeed(innerRef.Get(), map[*expressions.Reference]bool{})
	if !ok {
		return b, nil
	}
	b.windows, _ = values.OrdinalSeedLegWindows(rc)
	b.elementSlots = elementSlots
	seedCorr := values.UniqueCorrelationIdentifier()
	b.quant = expressions.NamedForEachQuantifier(seedCorr, innerRef)
	seedQOV, err := b.quant.RequireFlowedObjectValue()
	if err != nil {
		return gatheredSeedBake{}, fmt.Errorf("gathered seed flowed row: %w", err)
	}
	if rc.Type() == nil || seedQOV.Type() == nil || !rc.Type().Equals(seedQOV.Type()) {
		return gatheredSeedBake{}, fmt.Errorf("gathered seed layout disagrees with its input quantifier")
	}
	b.seedQOV = seedQOV
	return b, nil
}

// exactGatheredCTEGroupKeyValue resolves the one group-key population whose
// semantic scope intentionally stays closed: a SELECT-* CTE over a gathered
// multi-source unnest, consumed as the sole input of an aggregate. Such a CTE
// is absent from cteScopes because publishing a statically reconstructed
// multi-leg schema globally can rebind columns in sibling joins. By the time
// translateAggregate runs, however, gatheredSeedBakeContext has reached the
// selected CTE body's exact positional seed. That seed is the executable
// group-input authority: its quantifier is also the GroupBy input quantifier,
// and its record declares the final output fields in ordinal order.
//
// Admit only an unresolved, unqualified one-segment key over a direct CTE
// scan, and only when its name occurs exactly once in the exact seed row. A
// duplicate or missing name remains unresolved (and translateAggregate rejects
// it loudly); projected/reshaped CTEs have no seedQOV and stay on their existing
// path. The returned FieldValue is rooted on the seed carrier itself, never on
// a fabricated QOV for the SQL CTE name, which has no runtime binding here.
func (t *cascadesTranslator) exactGatheredCTEGroupKeyValue(
	input logical.LogicalOperator,
	key logical.GroupKey,
	bake gatheredSeedBake,
) (values.Value, bool, error) {
	if key.Value != nil || key.Qualified || key.Bare == "" || len(key.Segs) != 1 ||
		key.Segs[0] != key.Bare || bake.seedQOV == nil {
		return nil, false, nil
	}
	scan, ok := input.(*logical.LogicalScan)
	if !ok || scan == nil || scan.Table == "" {
		return nil, false, nil
	}
	body, isCTE := t.cteScope[strings.ToUpper(scan.Table)]
	if !isCTE || body == nil {
		return nil, false, nil
	}
	seedQOV, ok := values.AsQuantifiedObjectValue(bake.seedQOV)
	if !ok {
		return nil, false, nil
	}
	// SharedFlowedType, not FlowedType: the row below is only interrogated
	// (FieldIndexUnique) and never retained or modified, so the defensive
	// rebuild is pure waste on a path the planner runs per gathered CTE key.
	seedRow, ok := values.SharedFlowedType(seedQOV).(*values.RecordType)
	if !ok || seedRow == nil {
		return nil, false, nil
	}
	ordinal, unique := seedRow.FieldIndexUnique(key.Bare)
	if !unique {
		return nil, false, nil
	}
	resolved, err := values.ResolveFieldOrdinals(seedQOV, []int{ordinal})
	if err != nil {
		return nil, false, fmt.Errorf("gathered CTE group key slot %d: %w", ordinal, err)
	}
	return resolved, true, nil
}

// exactProjectedCTEOutputGroupKeyValue resolves a group key against the exact
// row published by a projecting CTE. This is deliberately separate from
// exactGatheredCTEGroupKeyValue: a projection, sort/limit over a projection, or
// UNION reshapes the gathered seed, so a seed ordinal is no longer an ordinal
// in the aggregate input row. The aggregate's own input quantifier is the only
// executable authority for that output row.
//
// The admission is intentionally narrow. The semantic resolver must have left
// a flat one-segment key unresolved; no direct gathered-seed bake may exist;
// the logical input must be a direct scan of a registered CTE; and the exact
// translated input must prove that a positional gathered source survives below
// a reshaping wrapper. The key must occur exactly once in the quantifier's
// frozen exact RecordType. Missing and duplicate names stay unresolved and are
// rejected by translateAggregate. No QOV is fabricated for the CTE name.
func (t *cascadesTranslator) exactProjectedCTEOutputGroupKeyValue(
	input logical.LogicalOperator,
	key logical.GroupKey,
	bake gatheredSeedBake,
) (values.Value, bool, error) {
	if key.Value != nil || key.Qualified || key.Bare == "" || len(key.Segs) != 1 ||
		key.Segs[0] != key.Bare || bake.seedQOV != nil {
		return nil, false, nil
	}
	scan, ok := input.(*logical.LogicalScan)
	if !ok || scan == nil || scan.Table == "" {
		return nil, false, nil
	}
	body, isCTE := t.cteScope[strings.ToUpper(scan.Table)]
	if !isCTE || body == nil || bake.quant.GetRangesOver() == nil {
		return nil, false, nil
	}
	translatedInput := bake.quant.GetRangesOver().Get()
	if !positionalGatherUnbaked(translatedInput, map[*expressions.Reference]bool{}) {
		return nil, false, nil
	}
	outputQOV, err := bake.quant.RequireFlowedObjectValue()
	if err != nil {
		return nil, false, fmt.Errorf("projected CTE output row: %w", err)
	}
	if !values.SameLeg(outputQOV.Correlation(), values.NamedCorrelationIdentifier(sourceAlias(scan))) {
		return nil, false, nil
	}
	outputRow, ok := outputQOV.FlowedType().(*values.RecordType)
	if !ok || outputRow == nil {
		return nil, false, nil
	}
	ordinal, unique := outputRow.FieldIndexUnique(key.Bare)
	if !unique {
		return nil, false, nil
	}
	resolved, err := values.ResolveFieldOrdinals(outputQOV, []int{ordinal})
	if err != nil {
		return nil, false, fmt.Errorf("projected CTE output group key slot %d: %w", ordinal, err)
	}
	return resolved, true, nil
}

// findWindowedSeed walks the outer-quantifier chain of expr to find a WINDOWED ordinal
// seed SelectExpression — a non-anchored RecordConstructorValue carrying per-leg windows
// (OrdinalSeedLegWindows). The seed can sit under IDENTITY wrappers that preserve its
// positional row: the EXISTS semi-join FlatMap (a pure filter), a SELECT-* CTE/derived
// projection, a DISTINCT. It recurses ONLY through SelectExpression and
// LogicalDistinctExpression — never a GroupByExpression or any row-reshaping node, which
// changes the layout so the seed's slots no longer address the caller's input row (an
// ORDER-BY-over-GROUP-BY must NOT bake its key against the pre-group seed). The `seen`
// set of visited references makes the walk UNBOUNDED but terminating over the finite plan
// DAG — no depth cap, so a deep (e.g. N-chained DISTINCT) identity wrapper stack still
// reaches the seed rather than silently exhausting a bound and skipping the bake (which
// would read the group key by name over the ordinal row → silent NULL). Returns the seed
// rc + its element slots; ok=false when no windowed seed is reachable through those
// identity wrappers (a name-model / reshaped input → the caller skips the bake).
func findWindowedSeed(expr expressions.RelationalExpression, seen map[*expressions.Reference]bool) (*values.RecordConstructorValue, map[string]int, bool) {
	if expr == nil {
		return nil, nil, false
	}
	var quants []expressions.Quantifier
	switch e := expr.(type) {
	case *expressions.SelectExpression:
		if rc, slots, found := seedElementSlots(e); found {
			if w, _ := values.OrdinalSeedLegWindows(rc); w != nil {
				return rc, slots, true
			}
		}
		quants = e.GetQuantifiers()
	case *expressions.LogicalDistinctExpression:
		quants = e.GetQuantifiers()
	case *expressions.LogicalSortExpression:
		// ORDER BY reorders ROWS, not COLUMNS — column-layout-preserving, so the seed's
		// slots still address the sort's output row. Walk it, so `SELECT * … ORDER BY`
		// aggregated ordinalizes correctly rather than skipping the bake.
		quants = e.GetQuantifiers()
	case *expressions.LogicalLimitExpression:
		// LIMIT truncates ROWS, not COLUMNS — layout-preserving, same as the sort.
		quants = e.GetQuantifiers()
	default:
		// A GroupBy / UNION / physical plan / any row-reshaping node is NOT a
		// column-identity wrapper: stop, so the seed under it is never baked against the
		// reshaped output row.
		return nil, nil, false
	}
	for _, q := range quants {
		ref := q.GetRangesOver()
		if seen[ref] {
			continue
		}
		seen[ref] = true
		if rc, slots, found := findWindowedSeed(ref.Get(), seen); found {
			return rc, slots, found
		}
	}
	return nil, nil, false
}

// positionalGatherUnbaked reports whether a WINDOWED gather seed is reachable from expr
// through POSITIONAL wrappers — the identity ones findWindowedSeed walks (Select, Distinct)
// PLUS the reshaping-but-still-positional ones it stops at (a projecting/subset
// LogicalProjection, a LogicalLimit) — while STOPPING at a GroupByExpression, which
// RE-NAMES its output so a name read there resolves correctly. It is the correct-or-loud
// detector: when findWindowedSeed (identity wrappers only) fails to reach the seed but this
// broader positional walk finds it, the input row is a positional gather whose slot the
// bake cannot address — a skipped bake would then silently mis-read by name. The caller
// refuses LOUD rather than emit that silent NULL.
func positionalGatherUnbaked(expr expressions.RelationalExpression, seen map[*expressions.Reference]bool) bool {
	if expr == nil {
		return false
	}
	var quants []expressions.Quantifier
	switch e := expr.(type) {
	case *expressions.SelectExpression:
		if rc, _, found := seedElementSlots(e); found {
			if w, _ := values.OrdinalSeedLegWindows(rc); w != nil {
				return true
			}
		}
		quants = e.GetQuantifiers()
	case *expressions.LogicalDistinctExpression:
		quants = e.GetQuantifiers()
	case *expressions.LogicalProjectionExpression:
		quants = e.GetQuantifiers()
	case *expressions.LogicalLimitExpression:
		quants = e.GetQuantifiers()
	case *expressions.LogicalSortExpression:
		quants = e.GetQuantifiers()
	case *expressions.LogicalUnionExpression:
		// UNION ALL is positional but multi-branch: findWindowedSeed can't bake one branch's
		// seed over the interleaved output, so a projecting/qualified branch under it is a
		// silent-NULL hazard the floor must catch.
		quants = e.GetQuantifiers()
	default:
		// GroupBy RE-NAMES (a name read over its output resolves — no silent NULL), and a
		// physical / non-positional node is not this hazard: stop.
		return false
	}
	for _, q := range quants {
		ref := q.GetRangesOver()
		if seen[ref] {
			continue
		}
		seen[ref] = true
		if positionalGatherUnbaked(ref.Get(), seen) {
			return true
		}
	}
	return false
}

// governingProjection returns the outermost LogicalProjectionExpression that determines the
// aggregate input row's column names, reachable through passthrough wrappers (a passthrough
// Select, DISTINCT, LIMIT), or nil when none governs (a pure positional gather — whose
// unresolved name read the executor already refuses LOUD, so the aggregate floor need not).
func governingProjection(expr expressions.RelationalExpression, seen map[*expressions.Reference]bool) *expressions.LogicalProjectionExpression {
	if expr == nil {
		return nil
	}
	if p, ok := expr.(*expressions.LogicalProjectionExpression); ok {
		return p
	}
	var quants []expressions.Quantifier
	switch e := expr.(type) {
	case *expressions.SelectExpression:
		quants = e.GetQuantifiers()
	case *expressions.LogicalDistinctExpression:
		quants = e.GetQuantifiers()
	case *expressions.LogicalLimitExpression:
		quants = e.GetQuantifiers()
	case *expressions.LogicalSortExpression:
		quants = e.GetQuantifiers()
	case *expressions.LogicalUnionExpression:
		quants = e.GetQuantifiers()
	default:
		return nil
	}
	for _, q := range quants {
		ref := q.GetRangesOver()
		if seen[ref] {
			continue
		}
		seen[ref] = true
		if p := governingProjection(ref.Get(), seen); p != nil {
			return p
		}
	}
	return nil
}

// projectionOutputColumnNames returns the UPPER-cased output-column names a projection emits
// — the alias when present, else the derived name of the projected Value (values.
// OutputColumnName, the same authority the physical projection uses). These are the names a
// name-model group key resolves against.
func projectionOutputColumnNames(proj *expressions.LogicalProjectionExpression) []string {
	return proj.GetOutputNames()
}

// expressionOutputColumns derives the OUTPUT column names, in ordinal order, of
// a translated expression's row — the plan-time layout authority baked
// consumer references resolve flat references against (Java's
// FieldValue.ofFieldName against childValue.getResultType()). Coverage:
//   - LogicalProjectionExpression: the projection's output names
//     (values.OutputColumnName — the same authority the executor's posNames
//     derivation reads, so the baked ordinal and the emitted slot agree).
//   - GroupByExpression: [groupKeys..., aggregates...] via
//     GroupByOutputColumnNames (the aggregateCursor's emission order).
//   - LogicalUnion/RecursiveUnion: the FIRST leg's layout (legs are
//     positionally aligned by construction).
//   - Row-shape-preserving unaries (Filter/Sort/Limit/Distinct/Unique): peel.
//
// nil for every underivable shape — the caller leaves the reference lazy, a
// LOUD OrdinalResolutionError at eval, never a guessed slot.
func expressionOutputColumns(expr expressions.RelationalExpression) []string {
	for expr != nil {
		switch e := expr.(type) {
		case *expressions.LogicalProjectionExpression:
			return projectionOutputColumnNames(e)
		case *expressions.GroupByExpression:
			return expressions.GroupByOutputColumnNames(e.GetGroupingKeys(), e.GetAggregates())
		case *expressions.SelectExpression:
			// A SELECT whose result value is a RECORD CONSTRUCTOR flows one
			// output column per RC field, named by the field (the RC is the
			// row authority — executeProjection/computeResultLegs emit slots
			// in RC field order). A non-RC result value (a bare QOV
			// passthrough) has no derivable flat layout here.
			if rc, isRC := e.GetResultValue().(*values.RecordConstructorValue); isRC {
				names := make([]string, len(rc.Fields))
				for i, f := range rc.Fields {
					names[i] = strings.ToUpper(f.Name)
				}
				return names
			}
			return nil
		case *expressions.LogicalUnionExpression:
			qs := e.GetQuantifiers()
			if len(qs) == 0 || qs[0].GetRangesOver() == nil {
				return nil
			}
			return expressionOutputColumns(qs[0].GetRangesOver().Get())
		case *expressions.RecursiveUnionExpression:
			qs := e.GetQuantifiers()
			if len(qs) == 0 || qs[0].GetRangesOver() == nil {
				return nil
			}
			return expressionOutputColumns(qs[0].GetRangesOver().Get())
		case *expressions.TempTableInsertExpression:
			// A recursive-CTE leg is wrapped in a TempTableInsert; the insert
			// flows its input's rows unchanged, so the OUTPUT layout is the
			// input's — the leg projection ALREADY NORMALIZED to the CTE's
			// output schema (translateRecursiveCTE's normalizeLegToOutputColumns).
			// Without this passthrough an ORDER BY over a renamed recursive CTE
			// (`WITH RECURSIVE a(node, up) AS (...) SELECT node FROM a ORDER BY
			// node`) could not derive the layout and its key stayed UNBAKED —
			// loud at runtime under the ordinal model.
			if e.GetInner().GetRangesOver() == nil {
				return nil
			}
			expr = e.GetInner().GetRangesOver().Get()
		case *expressions.LogicalFilterExpression:
			if e.GetInner().GetRangesOver() == nil {
				return nil
			}
			expr = e.GetInner().GetRangesOver().Get()
		case *expressions.LogicalSortExpression:
			if e.GetInner().GetRangesOver() == nil {
				return nil
			}
			expr = e.GetInner().GetRangesOver().Get()
		case *expressions.LogicalLimitExpression:
			if e.GetInner().GetRangesOver() == nil {
				return nil
			}
			expr = e.GetInner().GetRangesOver().Get()
		case *expressions.LogicalDistinctExpression:
			if e.GetInner().GetRangesOver() == nil {
				return nil
			}
			expr = e.GetInner().GetRangesOver().Get()
		case *expressions.LogicalUniqueExpression:
			if e.GetInner().GetRangesOver() == nil {
				return nil
			}
			expr = e.GetInner().GetRangesOver().Get()
		case *expressions.FullUnorderedScanExpression:
			// The scan row conforms to the record's logical column order
			// (executor.PositionalTypeForDescriptor).
			if rt, isRT := e.GetFlowedType().(*values.RecordType); isRT && len(rt.Fields) > 0 {
				names := make([]string, len(rt.Fields))
				for i, f := range rt.Fields {
					names[i] = strings.ToUpper(f.Name)
				}
				return names
			}
			return nil
		default:
			return nil
		}
	}
	return nil
}

// projectionRefAt is the parse-tree segment triple for one projection slot, or
// the zero (uncaptured) triple. A short or absent ProjectionRefs reads as
// uncaptured for every slot it does not cover — a producer that has not been
// taught to carry segments must keep the behaviour it had.
func projectionRefAt(p *logical.LogicalProject, i int) logical.ColumnRef {
	if p == nil || i < 0 || i >= len(p.ProjectionRefs) {
		return logical.ColumnRef{}
	}
	return p.ProjectionRefs[i]
}

// nameResolvesInColumns reports whether the group-key name resolves
// case-insensitively against the input row's output columns.
//
// IT USED TO FOLD ONLY ONE SIDE — `strings.ToUpper(key)` byte-compared against
// `cols` — which was an exact match wearing a fold's clothes, and it worked
// only for as long as every output name was already upper. That population is
// smaller now (a projection publishes its columns verbatim), so a verbatim
// `Region` key would have missed a verbatim `Region` column. The failure is
// LOUD (`no exact output-slot binding`) rather than a wrong answer, which is
// why it had not been seen; it was still a live hazard this change enlarges.
//
// Case-insensitive on BOTH sides is what the doc always claimed, and it is the
// right rule for this predicate specifically: the question is STRUCTURAL — is
// this key present in the row at all — not "what is this column called". A
// naming authority must not fold; a presence gate may.
func nameResolvesInColumns(key string, cols []string) bool {
	for _, c := range cols {
		if strings.EqualFold(c, key) {
			return true
		}
	}
	return false
}

func (t *cascadesTranslator) translateSort(s *logical.LogicalSort) expressions.RelationalExpression {
	// The translator's INPUT CONTRACT: a Sort never arrives over a Project.
	// Every logical builder defers the SELECT-list projection PAST the sort
	// (`postSortStripProj`), which is what keeps an aggregate's private
	// [keys..., calls...] row addressable to ORDER BY at all, and the arm that
	// once resolved sort keys against a projection's output SPELLINGS was
	// removed as unreachable. Nothing below reconstructs it, so a
	// Sort-over-Project would silently fall through to the flat-name bake and
	// resolve the key against whatever layout expressionOutputColumns reports
	// for a projection — a leaf-name match against a DIFFERENT domain, which
	// is a wrong sort order, not a slower plan.
	//
	// So the shape is refused here, at the consumer, in addition to being
	// pinned at each builder. The builder pins say "we do not emit this"; this
	// says "and if one ever does, it does not get silently interpreted". A
	// consumer-side guard is legitimate precisely because the logical layer's
	// builders never legally produce the shape — physical operator placement is
	// the memo's business, but what the TRANSLATOR accepts as logical input is
	// the translator's.
	if _, isProj := s.Input.(*logical.LogicalProject); isProj {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"ORDER BY over a projection reached the translator: the SELECT-list "+
				"projection must be built ABOVE the sort. Resolving sort keys against "+
				"a projection's output names would order by a leaf-name match in the "+
				"wrong domain; an ordinal-addressed pull-up is required before this "+
				"shape may exist"))
		return nil
	}
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
	bake, bakeErr := t.gatheredSeedBakeContext(innerRef, sourceAlias(s.Input))
	if bakeErr != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"ORDER BY input has no exact gathered-seed row: %v", bakeErr))
		return nil
	}
	sortOwner, ownerErr := bake.quant.RequireFlowedObjectValue()
	if ownerErr != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"ORDER BY input has no exact flowed object: %v", ownerErr))
		return nil
	}
	logicalAggregate := logicalAggregateUnder(s.Input)
	// The input expression's OUTPUT layout, when derivable —
	// a FLAT lazy sort key naming an output column bakes to its slot at plan
	// time (Java resolves the key against the child's result type); the
	// select's leg boundaries serve dotted keys.
	inputCols := expressionOutputColumns(innerRef.Get())
	// An ORDER BY over an AGGREGATE output binds its keys to the aggregate's
	// output ordinals, spelled CANONICALLY (AggregateKeyColumnName — the same
	// authority the aggregate's provided-ordering hint advertises), so a
	// group-key ORDER BY renders identically to the provided ordering and the
	// enforcer sort is elided (Java: the sort requirement is satisfied by the
	// streaming aggregate's own group-key order).
	sortGB := underlyingGroupBy(innerRef.Get())
	var sortGBNames []string
	if sortGB != nil {
		sortGBNames = expressions.GroupByOutputColumnNames(sortGB.GetGroupingKeys(), sortGB.GetAggregates())
	}
	// A sort NEVER sits over the grouped select's reshaping projection: both
	// builders defer that projection PAST the sort (`postSortStripProj`), which
	// is what keeps the aggregate's private [keys..., calls...] row addressable
	// to ORDER BY at all. The pull-up-onto-projection-output arm that used to
	// live here was therefore unreachable from the day it was written, and its
	// two name-match loops resolved a sort key against the projection's output
	// SPELLINGS — RFC-197's leaf-name-as-identity shape, in code no query could
	// run. Removed rather than migrated: there is no ordinal to convert to when
	// the domain it would index cannot exist.
	//
	// The layering that makes this so is pinned at every builder by the
	// TestSortNeverSitsOverAProjection family (core/embedded) — all seven
	// NewSort sites across the four contexts — and refused at this function's
	// entry above, so a builder that starts emitting Sort-over-Project fails
	// loudly at both ends rather than being silently reinterpreted here.
	sortKeys := make([]expressions.SortKey, len(s.Keys))
	for i, k := range s.Keys {
		nf := k.NullsFirst
		v := k.Value
		// A POSITIONAL key (`ORDER BY <n>`) IS an output ordinal by SQL
		// definition: when the sort input is the SELECT-list projection, bake
		// slot n-1 of its output directly — no text-rendering
		// round-trip, which diverges for computed items whose canonical source
		// text differs from the derived output spelling (`col1 + 10` vs
		// `(COL1 + 10)`). A key whose ordinal was already resolved into
		// the select list's typed item Value (upgradeSortKeyValues) keeps
		// that Value — the input projection here can be a DERIVED source's
		// layout, whose slots are not this select's ordinals.
		exactAggregateOrdinal := k.HasAggregateOutputOrdinal
		exactAggregateValue := exactAggregateOrdinal || k.AggregateOutputValueExact
		if exactAggregateOrdinal {
			if sortGB == nil || logicalAggregate == nil || k.AggregateOutputOrdinal < 0 || k.AggregateOutputOrdinal >= len(sortGBNames) {
				t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
					"ORDER BY aggregate output ordinal is outside the native aggregate row"))
				return nil
			}
			var resolveErr error
			v, resolveErr = values.ResolveFieldOrdinals(sortOwner, []int{k.AggregateOutputOrdinal})
			if resolveErr != nil {
				t.setTranslateErr(resolveErr)
				return nil
			}
		} else if k.AggregateOutputValueExact {
			if sortGB == nil || logicalAggregate == nil || v == nil {
				t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
					"exact ORDER BY aggregate Value has no native aggregate row"))
				return nil
			}
			bound, bindErr := bindPostAggregateValue(v, logicalAggregate, sortOwner)
			if bindErr != nil {
				t.setTranslateErr(bindErr)
				return nil
			}
			v = bound
		}
		if v == nil && k.Pos > 0 && k.Pos <= len(inputCols) {
			var resolveErr error
			v, resolveErr = values.ResolveFieldOrdinals(sortOwner, []int{k.Pos - 1})
			if resolveErr != nil {
				t.setTranslateErr(resolveErr)
				return nil
			}
		} else if k.Pos > 0 {
			// A positional key whose slot the input's output layout cannot
			// serve (no derivable columns) must not silently fall back to the
			// TEXT rendering — that text is the RIGHT union leg's spelling
			// and misresolves. Loud, never a misread. RFC-180.
			if _, isUnion := innerRef.Get().(*expressions.LogicalUnionExpression); isUnion {
				t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
					"positional ORDER BY %d is not derivable from the UNION output", k.Pos))
				return nil
			}
		}
		// minted is the lazy carrier THIS iteration created from the key's
		// canonical text — the one value whose parse-tree segments the SortKey
		// still carries. A key that arrived with a resolved Value, or that a
		// rebase pass below rewrites, is not it, and pointer identity says so.
		if v == nil {
			t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"ORDER BY key %q has no resolved Value or ordinal metadata", k.Expr))
			return nil
		}
		if sortGB != nil && !exactAggregateValue {
			t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
				"ORDER BY over GroupBy is missing exact aggregate-output metadata"))
			return nil
		}
		if logicalDerivedProjectionInput(s.Input) {
			var normalizeErr error
			v, normalizeErr = translateDerivedSortKeyToPhysicalInput(v, sortOwner)
			if normalizeErr != nil {
				t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
					"ORDER BY derived input key %d cannot adopt its physical output names: %v", i, normalizeErr))
				return nil
			}
		}
		if bake.seedQOV != nil {
			v, bakeErr = bakeGatheredGroupValue(v, bake.windows, bake.elementSlots, bake.seedQOV)
			if bakeErr != nil {
				t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
					"ORDER BY key could not bind to the gathered seed: %v", bakeErr))
				return nil
			}
		}
		// An ORDER BY key still holding the carrier minted above resolves from
		// the PARSER's segments — the same conversion the projection channel
		// got, on the channel that carried the segments first.
		sortKeys[i] = expressions.SortKey{
			Value:      v,
			Reverse:    k.Dir == logical.SortDesc,
			NullsFirst: &nf,
		}
	}
	return t.exactSort(sortKeys, bake.quant)
}

func logicalAggregateUnder(op logical.LogicalOperator) *logical.LogicalAggregate {
	cur := op
	for {
		switch e := cur.(type) {
		case *logical.LogicalAggregate:
			return e
		case *logical.LogicalFilter:
			cur = e.Input
		case *logical.LogicalSort:
			cur = e.Input
		case *logical.LogicalLimit:
			cur = e.Input
		default:
			return nil
		}
	}
}

// logicalDerivedProjectionInput identifies the row-preserving wrapper chain
// whose outward column names belong to a derived-table / authored CTE
// boundary. PreserveMainSource CTEs are scope envelopes; their Main source,
// not the wrapper, owns the outward schema and must not take this bridge.
func logicalDerivedProjectionInput(op logical.LogicalOperator) bool {
	switch typed := op.(type) {
	case *logical.LogicalCTE:
		return !typed.PreserveMainSource
	case *logical.LogicalFilter:
		return logicalDerivedProjectionInput(typed.Input)
	case *logical.LogicalSort:
		return logicalDerivedProjectionInput(typed.Input)
	case *logical.LogicalLimit:
		return logicalDerivedProjectionInput(typed.Input)
	case *logical.LogicalDistinct:
		return logicalDerivedProjectionInput(typed.Input)
	default:
		return false
	}
}

// translateDerivedSortKeyToPhysicalInput moves a simple derived-column ORDER
// BY key from the authored derived-row declaration onto the translated
// producer's exact output row. SQL preserves a delimited output spelling such
// as "a.b" in the semantic key, while the physical projection contract folds
// that slot to A.B. The two rows still describe the same positional object: the
// field ordinal, exact leaf type, and nullability are unchanged.
//
// TranslateProjectionInputNameNormalization is the authority for that narrow
// bridge. It admits only same-correlation concrete record rows that differ in
// top-level field names and rebuilds the complete ordinal path; no rendered
// name or runtime lookup participates. A foreign owner, nested access, or
// computed program is retained for its own binding authority. Same-owner type,
// width, record-nullability, or nested-shape drift is an error, never an
// ordinal guess.
func translateDerivedSortKeyToPhysicalInput(
	value values.Value,
	target values.QuantifiedObjectValue,
) (values.Value, error) {
	field, ok := values.AsFieldValue(value)
	if !ok {
		return value, nil
	}
	path := field.Path().Ordinals()
	if len(path) != 1 {
		return value, nil
	}
	declaration, ok := values.AsQuantifiedObjectValue(field.ChildValue())
	if !ok || declaration.Correlation() != target.Correlation() {
		return value, nil
	}
	if values.FlowedTypesEqual(declaration, target) {
		return value, nil
	}
	return values.TranslateProjectionInputNameNormalization(value, declaration, target)
}

// validateExactAggregateProjectContract checks the logical, producer-owned
// SELECT-output contract before translateProject can take any early-return
// path. Physical GroupBy output names and width are checked only after the
// input has translated; this part deliberately needs no physical expression.
func validateExactAggregateProjectContract(p *logical.LogicalProject) (*logical.LogicalAggregate, error) {
	if p.AggregateOutputOrdinals == nil {
		return nil, nil
	}
	agg := logicalAggregateUnder(p.Input)
	if agg == nil || len(p.AggregateOutputOrdinals) != len(p.Projections) {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"post-aggregate projection has an incomplete output-slot layout")
	}
	if len(agg.OutputSlots) != len(p.Projections) {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"post-aggregate projection does not cover the aggregate SELECT output contract")
	}
	nativeWidth := len(agg.GroupKeys) + len(agg.Calls)
	for i, ordinal := range p.AggregateOutputOrdinals {
		computed := i < len(p.IsComputed) && p.IsComputed[i]
		if agg.OutputSlots[i].SelectOrdinal != i+1 ||
			agg.OutputSlots[i].NativeOrdinal != ordinal ||
			(computed != (ordinal == -1)) {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"post-aggregate projection disagrees with the aggregate SELECT output contract")
		}
		if ordinal < -1 || ordinal >= nativeWidth {
			return nil, api.NewError(api.ErrCodeUnsupportedQuery,
				"post-aggregate projection contains an invalid native output ordinal")
		}
	}
	return agg, nil
}

func (t *cascadesTranslator) translateProject(p *logical.LogicalProject) expressions.RelationalExpression {
	// Validate the exact aggregate boundary before any fold, correlated-scalar
	// dispatch, or input translation can return. Malformed logical contracts
	// are never eligible for another translation path.
	exactAggregateLayout := p.AggregateOutputOrdinals != nil
	exactAggregate, contractErr := validateExactAggregateProjectContract(p)
	if contractErr != nil {
		t.setTranslateErr(contractErr)
		return nil
	}
	// The correlated-scalar lowering is a separate projection authority and
	// returns before the physical GroupBy row is available here. Until it can
	// consume and prove this exact aggregate contract itself, reject the
	// composition rather than bypass native-slot canonicalization.
	if exactAggregateLayout && len(p.CorrelatedScalarSubqueries) > 0 {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"a post-aggregate projection with a correlated scalar subquery is not yet supported"))
		return nil
	}

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

	// Two independently-carried correlated scalars would require nesting two
	// LEFT-scalar boxes while preserving both private slots and cardinality
	// barriers. That composition is not implemented. Reject before translating
	// either carrier; do not depend on clusterArity becoming poison only after
	// the first box happens to be translated.
	if len(p.CorrelatedScalarSubqueries) > 0 &&
		findCorrelatedScalarFilterUnderUnaryChain(p.Input) != nil {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"correlated scalar subqueries in both SELECT and WHERE are not yet supported"))
		return nil
	}

	// ON-clause EXISTS is translated through a separate existential-join path,
	// not translateFilter's known-truth substitution. Until that consumer can
	// replace its ON marker with the constant, reject rather than raw-semi-join
	// the fallback plan and lose aggregate/pagination cardinality.
	if join, ok := p.Input.(*logical.LogicalJoin); ok &&
		hasKnownExistsTruth(join.OnExistsSubqueries) {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"a cardinality-known EXISTS in a JOIN ON clause is not yet supported"))
		return nil
	}

	// An INNER join carrying an ON-clause EXISTS is
	// SEMANTICALLY IDENTICAL to WHERE-EXISTS — Java folds every inner-join ON
	// predicate into the WHERE of one SelectExpression (QueryVisitor.visitSimpleTable
	// conjoins inner-join expressions into the WHERE), so `JOIN e ON e.c_id = c.id
	// AND EXISTS(sub)` plans as the same correlated semi-join over the flattened
	// a⋈c⋈e cluster. Go's ON-EXISTS lift already builds the ExistentialValuePredicate
	// marker into join.OnPredicate and the subquery into join.OnExistsSubqueries;
	// synthesize the equivalent WHERE-EXISTS filter here so the projection routes
	// through the ORDINAL gather (translateExistsOverGatheredCluster) instead of the
	// name-model ON-exists semi-join (translateJoin's OnExistsSubqueries arm). The
	// non-EXISTS ON conjuncts stay on the join (SARG'd by the cluster machinery, as
	// they are for an equivalent WHERE-EXISTS query); only the existential marker is
	// lifted into the filter. Fail-open: a non-foldable shape (arity<=2, dup-alias,
	// ungated, or the gather declining) falls through to today's name-model path.
	if join, ok := p.Input.(*logical.LogicalJoin); ok && join.Kind == logical.JoinInner &&
		len(join.OnExistsSubqueries) > 0 && len(p.CorrelatedScalarSubqueries) == 0 {
		if onPred, isPred := join.OnPredicate.(predicates.QueryPredicate); isPred {
			if markers := extractExistsPredicates(onPred); len(markers) > 0 {
				joinCopy := *join
				joinCopy.OnPredicate = andOf(splitNonExistsPredicates(onPred))
				joinCopy.OnExistsSubqueries = nil
				synthFilter := &logical.LogicalFilter{
					Input:            &joinCopy,
					Predicate:        andOf(markers),
					ExistsSubqueries: join.OnExistsSubqueries,
				}
				if t.existsFoldableGatheredCluster(synthFilter) {
					if sel := t.translateProjectOverExistsFilter(p, synthFilter, nil); sel != nil {
						return sel
					}
				}
			}
		}
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
	// B1 widening: the fold ALSO fires for a plain WHERE-EXISTS
	// (no projected EXISTS) over a gated arity>=3 non-dup INNER cluster — the B1
	// wrap needs the projection folded in (its leg references only resolve over
	// the wrapped box when rebased at translation; an ordinary Map above the wrap
	// cannot see the legs). An intervening ORDER BY / LIMIT (a non-empty chain) is
	// supported too: translateProjectOverExistsFilter re-applies the chain ABOVE the
	// wrap with each sort key pulled up onto the wrap's positional output (the B1
	// gather no longer declines on t.existsFoldHasChain). When the B1 arm declines,
	// buildExistentialSelect bails the widened fold (nil) and the ordinary path
	// keeps today's plan shape.
	if filter, chain := findExistsFilterUnderUnaryChain(p.Input); filter != nil &&
		(projectionReferencesExistsSubquery(p.ProjectedValues) ||
			t.existsFoldableGatheredCluster(filter)) {
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
	projectionQ := t.namedQuantifier(sourceAlias(p.Input), innerRef)
	projectionInput, flowedErr := projectionQ.RequireFlowedObjectValue()
	if flowedErr != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"projection input has no exact flowed object type: %v", flowedErr))
		return nil
	}
	if p.InputOrdinals != nil {
		if exactAggregateLayout || len(p.InputOrdinals) != len(p.Projections) {
			t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
				"projection has a malformed positional input contract"))
			return nil
		}
	}
	projected := make([]values.Value, len(p.Projections))
	for i := range p.Projections {
		if i < len(p.ProjectedValues) && p.ProjectedValues[i] != nil {
			projected[i] = p.ProjectedValues[i]
			continue
		}
		if p.InputOrdinals != nil {
			resolved, err := values.ResolveFieldOrdinals(projectionInput, []int{p.InputOrdinals[i]})
			if err != nil {
				t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
					"projection input slot %d does not resolve: %v", p.InputOrdinals[i], err))
				return nil
			}
			projected[i] = resolved
			continue
		}
		if exactAggregateLayout && i < len(p.AggregateOutputOrdinals) && p.AggregateOutputOrdinals[i] >= 0 {
			continue // native-slot metadata binds below, against projectionInput
		}
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"projection slot %d has no resolved Value", i))
		return nil
	}
	// A recursive CTE's main query was resolved against the seed declaration
	// before the recursive fixed point could widen it. Retarget only that
	// explicitly scoped source onto the physical common row. The bridge
	// re-resolves complete ordinal paths and admits MaximumType widening only;
	// ordinary derived/name normalization below remains exact-name-only.
	for i := range projected {
		if projected[i] == nil {
			continue
		}
		normalized, normalizeErr := t.normalizeRecursiveCTEConsumerValue(projected[i])
		if normalizeErr != nil {
			t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"recursive CTE projection slot %d cannot adopt its common output row: %v", i, normalizeErr))
			return nil
		}
		projected[i] = normalized
	}
	// A derived boundary may publish SQL output names in its logical contract
	// that differ from the physical projection's producer-local names. Duplicate
	// aliases ([X, X, Y] -> [X, X_2, Y]) and a delimited lower-case alias
	// ([a.b] -> [A.B]) are both instances of that same positional boundary. The
	// consumer Value was resolved before physical normalization, so move only the
	// exact derived-input root onto projectionInput. The values-layer bridge
	// proves that correlation, width, ordinals, record/leaf nullability, and every
	// exact leaf type agree; only top-level field names may differ.
	if logicalDerivedProjectionInput(p.Input) {
		logicalInputType, logicalTypeErr := ExactLogicalResultType(p.Input, t.md)
		if logicalTypeErr == nil && !values.FlowedTypeEquals(projectionInput, logicalInputType) {
			declaration, declarationErr := values.NewQuantifiedObjectValue(
				projectionInput.Correlation(), logicalInputType)
			if declarationErr != nil {
				t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
					"derived projection input has no exact logical declaration: %v", declarationErr))
				return nil
			}
			for i := range projected {
				if projected[i] == nil {
					continue
				}
				normalized, normalizeErr := values.TranslateProjectionInputNameNormalization(
					projected[i], declaration, projectionInput)
				if normalizeErr != nil {
					// A derived/CTE wrapper can also change record identity or
					// structure. That is not output-name normalization; preserve the
					// pre-existing program for its ordinary translator path instead of
					// turning an inapplicable optional bridge into a new rejection.
					var coded interface {
						Code() values.ResolutionErrorCode
					}
					if errors.As(normalizeErr, &coded) && coded.Code() == values.LayoutTypeMismatch {
						continue
					}
					t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
						"derived projection input slot %d cannot adopt its physical output names: %v", i, normalizeErr))
					return nil
				}
				projected[i] = normalized
			}
		}
		// A quoted lower-case output spelling can already have been folded in
		// ExactLogicalResultType while its structured Value still carries the
		// authored exact declaration. Normalize each simple input field from that
		// declaration as well. This is the same checked names-only bridge used by
		// translateSort; computed and foreign-rooted programs remain untouched.
		for i := range projected {
			if projected[i] == nil {
				continue
			}
			normalized, normalizeErr := translateDerivedSortKeyToPhysicalInput(
				projected[i], projectionInput)
			if normalizeErr != nil {
				t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
					"derived projection input slot %d cannot adopt its physical output names: %v", i, normalizeErr))
				return nil
			}
			projected[i] = normalized
		}
	}
	if exactAggregateLayout {
		nativeWidth := len(exactAggregate.GroupKeys) + len(exactAggregate.Calls)
		gb := underlyingGroupBy(innerRef.Get())
		if gb == nil {
			t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
				"post-aggregate projection has no GroupBy output row"))
			return nil
		}
		if names := expressions.GroupByOutputColumnNames(gb.GetGroupingKeys(), gb.GetAggregates()); len(names) != nativeWidth {
			t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
				"post-aggregate projection disagrees with the GroupBy output width"))
			return nil
		}
		for i, ordinal := range p.AggregateOutputOrdinals {
			switch {
			case ordinal >= 0 && ordinal < nativeWidth:
				resolved, err := values.ResolveFieldOrdinals(projectionInput, []int{ordinal})
				if err != nil {
					t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
						"post-aggregate projection slot %d does not resolve: %v", ordinal, err))
					return nil
				}
				projected[i] = resolved
			case ordinal == -1:
				if projected[i] == nil {
					t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
						"computed post-aggregate output did not resolve to a Value"))
					return nil
				}
				bound, err := bindPostAggregateValue(projected[i], exactAggregate, projectionInput)
				if err != nil {
					t.setTranslateErr(err)
					return nil
				}
				projected[i] = bound
			default:
				t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
					"post-aggregate projection contains an invalid native output ordinal"))
				return nil
			}
		}
	}
	// A SELECT list over a GROUP BY resolves each aggregate/group-key
	// reference to the aggregate OUTPUT COLUMN by ordinal (Java's
	// FieldValue.ofOrdinalNumber over the GroupByExpression result) — so the
	// alias-named aggregate slot resolves even though the projection references the
	// canonical text. Without this the reference stays a free canonical name that
	// misses the aggregate's ordinal PositionalRow (whose slot is alias-named).
	if underlyingGroupBy(innerRef.Get()) != nil && !exactAggregateLayout {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"projection over GroupBy is missing its native output-slot contract"))
		return nil
	}
	return t.exactProjectionForLogicalProject(projected, p, projectionQ)
}

// translateSingleSourceCorrelatedScalarJoin is the one lowering authority for a
// correlated scalar over a single-source outer, shared by SELECT-list and
// WHERE-comparison consumers. It materializes [outer columns..., scalar] with a
// LEFT join, preserving NULL-on-empty, and carries StrictSingle on the inner
// quantifier so a second row is SQLSTATE 21000. Consumers decide how to use and
// subsequently hide the scalar slot; neither is allowed to rebuild this join.
func (t *cascadesTranslator) translateSingleSourceCorrelatedScalarJoin(
	outerPlan logical.LogicalOperator,
	csq logical.CorrelatedScalarSubquery,
) (*expressions.SelectExpression, *values.RecordType, *values.RecordType) {
	outerRef := t.translateRef(outerPlan)
	if outerRef == nil {
		return nil, nil, nil
	}
	outerAlias := sourceBinding(outerPlan)
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
		return nil, nil, nil
	}

	// Wrap with LogicalLimitExpression if the inner plan had a LIMIT.
	if innerLimit != nil {
		innerAlias := sourceAlias(innerPlan)
		limitQ := t.namedQuantifier(innerAlias, innerRef)
		limitExpr, err := newLimitExprFromLogical(innerLimit, limitQ)
		if err != nil {
			t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"correlated scalar LIMIT has no exact flowed row: %v", err))
			return nil, nil, nil
		}
		innerRef = expressions.InitialOf(limitExpr)
	}

	// Source-anchored correlated-scalar-subquery join seed (RFC-077 7.6).
	//
	// The inner is a scalar SUBQUERY exposing exactly ONE value. The projection
	// reads it as the QUALIFIED name <innerAlias>.<scalarCol> — and the inner
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
	// VERBATIM: this NAMES the ordinal seed's inner-leg column, so it is the
	// spelling the correlated subquery's result column reports. Folding it made
	// `(SELECT SUM(x."Amount") …)` label itself SUM(X.AMOUNT) for a column
	// declared `Amount` — the same defect as the aggregate mint, one boundary
	// out. The other ToUpper on this field (logical_predicate.go's `want`,
	// clustered_outer_scalar's innerKey) sit in front of EqualFold comparisons,
	// where they decide nothing; this one decides a name.
	scalarCol := csq.ScalarCol
	outerCols := t.legColumns(outerPlan)
	if outerCols == nil || outerAlias == "" || scalarCol == "" || csq.InnerAlias == "" {
		return nil, nil, nil
	}
	// Ordinalize the 2-leg seed when the OUTER is a SINGLE SOURCE
	// (clusterArity==1); the clustered-outer dispatch above already ordinalized
	// the gated multi-table outers. The name-model anchored fallback
	// has been retired, so this ordinal seed is the sole surviving
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
	if t.clusterArity(outerPlan) == 1 {
		ordinalInnerCorr := values.UniqueCorrelationIdentifier()
		resultValue = t.scalarSubqueryOrdinalSeed(outerAlias, outerPlan, innerPlan, ordinalInnerCorr, csq.InnerAlias, scalarCol)
		if resultValue != nil {
			innerLegCorr = ordinalInnerCorr
		}
	}
	if resultValue == nil {
		// The name-model NewScalarSubqueryAnchoredRecord
		// fallback is retired — a full-corpus census reached it from no SQL
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
		if cj := peelToClusterJoin(outerPlan); cj != nil {
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
		return nil, nil, nil
	}

	// When the front end cannot prove that the scalar yields AT MOST ONE inner
	// row per outer row, it marks the inner quantifier strict-single so
	// ImplementNestedLoopJoinRule wraps it in a strict FirstOrDefault (a second
	// row → 21000). StrictSingle is false only when exact pagination caps a
	// multi-row-capable source to 0/1 rows, or when the source is intrinsically
	// <=1 (for example a non-grouped real aggregate, even with LIMIT >1).
	// The quantifier carries innerLegCorr — the ordinal seed's fresh unique id
	// (the sole surviving path; the name-model fallback has been retired).
	var innerQ expressions.Quantifier
	if csq.StrictSingle {
		innerQ = expressions.NamedForEachStrictSingleQuantifier(innerLegCorr, innerRef)
	} else {
		innerQ = expressions.NamedForEachQuantifier(innerLegCorr, innerRef)
	}

	joinSelect, err := expressions.NewSelectExpressionWithJoinType(
		resultValue,
		[]expressions.Quantifier{outerQ, innerQ},
		nil,
		[]string{outerAlias, innerLegCorr.Name()},
		expressions.JoinLeftOuter,
	)
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"correlated scalar join has no exact result row: %v", err))
		return nil, nil, nil
	}
	rc, ok := resultValue.(*values.RecordConstructorValue)
	if !ok {
		t.setTranslateErr(api.NewError(api.ErrCodeInternalError,
			"correlated scalar ordinal seed did not produce a record constructor"))
		return nil, nil, nil
	}
	mergedType, ok := rc.Type().(*values.RecordType)
	if !ok {
		t.setTranslateErr(api.NewError(api.ErrCodeInternalError,
			"correlated scalar ordinal seed did not produce a record type"))
		return nil, nil, nil
	}
	outerType := t.ordinalLegType(outerPlan)
	if outerType == nil || len(mergedType.Fields) != len(outerType.Fields)+1 {
		t.setTranslateErr(api.NewError(api.ErrCodeInternalError,
			"correlated scalar ordinal seed output schema does not equal outer schema plus one scalar"))
		return nil, nil, nil
	}
	return joinSelect, mergedType, outerType
}

func (t *cascadesTranslator) translateProjectWithCorrelatedScalar(p *logical.LogicalProject) expressions.RelationalExpression {
	csq := p.CorrelatedScalarSubqueries[0]

	// A MULTI-TABLE outer cluster dispatches to the
	// clustered-outer ordinal path first. decline=true is the CORRECT-or-LOUD
	// policy: a known non-rightmost correlation that did not ordinalize would
	// silently NULL (JOIN..ON / LEFT outers) or mis-plan (comma clusters) under
	// an anchored fallback — refuse to translate instead.
	if sel, decline := t.translateClusteredOuterScalar(p, csq); sel != nil || decline {
		return sel
	}

	joinSelect, mergedType, outerType := t.translateSingleSourceCorrelatedScalarJoin(p.Input, csq)
	if joinSelect == nil {
		return nil
	}
	joinRef := expressions.InitialOf(joinSelect)
	projQ := t.namedQuantifier("", joinRef)
	rowQOV, err := projQ.RequireFlowedObjectValue()
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"correlated scalar projection has no exact materialized row: %v", err))
		return nil
	}

	projected := make([]values.Value, len(p.Projections))
	for i := range p.Projections {
		if i >= len(p.ProjectedValues) || p.ProjectedValues[i] == nil {
			t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"correlated scalar projection slot %d has no resolved Value", i))
			return nil
		}
		replaced, replaceErr := replaceScalarSubqueryRef(
			p.ProjectedValues[i], csq, rowQOV, len(mergedType.Fields)-1)
		if replaceErr != nil {
			t.setTranslateErr(replaceErr)
			return nil
		}
		rebased, rebaseErr := rebaseCorrelatedScalarOuterValue(
			replaced, sourceBinding(p.Input), outerType, rowQOV)
		if rebaseErr != nil {
			t.setTranslateErr(rebaseErr)
			return nil
		}
		projected[i] = rebased
	}

	return t.exactProjectionForLogicalProject(projected, p, projQ)
}

// It binds to the seed row's LAST slot, which is where both correlated-scalar
// seeds put the scalar leg (scalarSubqueryOrdinalSeed and
// clusteredOuterOrdinalSeed each append it after every outer column).
//
// It used to render the inner correlation and the scalar column into the text
// `INNER.SCALARCOL` and leave a lazy carrier for the flat baker to match by
// name — a joined identifier minted from an identity that was already in hand,
// purely so a downstream reader could take it apart. The name is kept as the
// DISPLAY spelling; the ordinal is what reads the row.
func replaceScalarSubqueryRef(
	v values.Value,
	csq logical.CorrelatedScalarSubquery,
	rowQOV values.Value,
	slot int,
) (values.Value, error) {
	if _, ok := values.AsQuantifiedObjectValue(rowQOV); !ok {
		return nil, api.NewError(api.ErrCodeInternalError,
			"correlated scalar slot has no exact materialized-row owner")
	}
	var replaceErr error
	replaced := values.Replace(v, func(node values.Value) values.Value {
		if replaceErr != nil {
			return node
		}
		ssq, isSSQ := node.(*values.ScalarSubqueryValue)
		if !isSSQ || ssq.Alias != csq.Alias {
			return node
		}
		resolved, err := values.ResolveFieldOrdinals(rowQOV, []int{slot})
		if err != nil {
			replaceErr = api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"correlated scalar slot %d does not resolve against its materialized row: %v", slot, err)
			return node
		}
		if ssq.Type() == nil || resolved.Type() == nil || !ssq.Type().Equals(resolved.Type()) {
			replaceErr = api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"correlated scalar slot %d changes the scalar result type", slot)
			return node
		}
		return resolved
	})
	if replaceErr != nil {
		return nil, replaceErr
	}
	return replaced, nil
}

// fieldOntoCorrelatedScalarRow rebases one outer-source field onto the fresh
// materialized [outer..., scalar] row. The root ordinal changes to the merged
// row's slot while a structured-column suffix is preserved verbatim.
func fieldOntoCorrelatedScalarRow(
	fv values.FieldValue,
	rowQOV values.Value,
	ordinal int,
) (values.Value, error) {
	if fv == nil || fv.Path() == nil || fv.Path().Len() == 0 {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"correlated scalar outer field has no exact ordinal path")
	}
	ordinals := fv.Path().Ordinals()
	ordinals[0] = ordinal
	baked, err := values.ResolveFieldOrdinals(rowQOV, ordinals)
	if err != nil {
		return nil, err
	}
	if fv.ResultType() == nil || baked.Type() == nil || !fv.ResultType().Equals(baked.Type()) {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"correlated scalar outer-field rebase changes the field type")
	}
	return baked, nil
}

func rebaseCorrelatedScalarOuterValue(
	v values.Value,
	outerAlias string,
	outerType *values.RecordType,
	rowQOV values.Value,
) (values.Value, error) {
	if v == nil || outerAlias == "" || outerType == nil {
		return nil, api.NewError(api.ErrCodeUnsupportedQuery,
			"correlated scalar projection has no exact outer-row contract")
	}
	row, ok := values.AsQuantifiedObjectValue(rowQOV)
	if !ok {
		return nil, api.NewError(api.ErrCodeInternalError,
			"correlated scalar projection has no exact materialized-row owner")
	}
	var rebaseErr error
	rebased := values.Replace(v, func(node values.Value) values.Value {
		if rebaseErr != nil {
			return node
		}
		fv, isField := values.AsFieldValue(node)
		if !isField {
			return node
		}
		owner, hasOwner := values.AsQuantifiedObjectValue(fv.ChildValue())
		if !hasOwner || !strings.EqualFold(owner.Correlation().Name(), outerAlias) {
			return node
		}
		ordinals := fv.Path().Ordinals()
		if len(ordinals) == 0 || ordinals[0] < 0 || ordinals[0] >= len(outerType.Fields) {
			rebaseErr = api.NewError(api.ErrCodeUnsupportedQuery,
				"correlated scalar projection outer field is outside the outer-row layout")
			return node
		}
		resolved, err := fieldOntoCorrelatedScalarRow(fv, rowQOV, ordinals[0])
		if err != nil {
			rebaseErr = api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"correlated scalar projection could not rebase an outer field: %v", err)
			return node
		}
		return resolved
	})
	if rebaseErr != nil {
		return nil, rebaseErr
	}
	for corr := range values.GetCorrelatedToOfValue(rebased) {
		if corr != row.Correlation() {
			return nil, api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"correlated scalar projection retained unmaterialized correlation %s", corr.Name())
		}
	}
	return rebased, nil
}

type correlatedScalarPredicateFacts struct {
	hasTargetScalar bool
	hasAnyScalar    bool
	hasExistsValue  bool
}

// inspectCorrelatedScalarPredicateValues examines every Value embedded anywhere
// in a predicate. Using the shared predicate-value spine is important here:
// comparisons can carry values on either side (and in range/vector payloads),
// so inspecting only a ComparisonPredicate's visible operand would make an
// orphan carrier or a foreign scalar look safe.
func inspectCorrelatedScalarPredicateValues(
	p predicates.QueryPredicate,
	target values.CorrelationIdentifier,
) correlatedScalarPredicateFacts {
	var facts correlatedScalarPredicateFacts
	predicates.ReplaceValues(p, func(node values.Value) values.Value {
		switch n := node.(type) {
		case *values.ScalarSubqueryValue:
			facts.hasAnyScalar = true
			if n.Alias == target {
				facts.hasTargetScalar = true
			}
		case *values.ExistsValue:
			facts.hasExistsValue = true
		}
		return node
	})
	return facts
}

// partitionCorrelatedScalarWherePredicate splits only top-level AND
// conjuncts. A scalar-free leaf that references no binding other than the
// single outer source can run inside the outer leg before the per-row scalar
// evaluation. This is semantically observable: an excluded outer row must not
// trigger a cardinality error in a scalar subquery that SQL never evaluates for
// that row. OR and NOT remain atomic above the scalar box; distributing either
// would change three-valued/short-circuit semantics and duplicate evaluation.
func partitionCorrelatedScalarWherePredicate(
	p predicates.QueryPredicate,
	outerAlias string,
) (predicates.QueryPredicate, predicates.QueryPredicate) {
	var pre, post []predicates.QueryPredicate
	var partition func(predicates.QueryPredicate)
	partition = func(candidate predicates.QueryPredicate) {
		if candidate == nil {
			return
		}
		if and, ok := candidate.(*predicates.AndPredicate); ok {
			for _, child := range and.SubPredicates {
				partition(child)
			}
			return
		}

		movable := true
		switch candidate.(type) {
		case *predicates.OrPredicate, *predicates.NotPredicate:
			movable = false
		}
		facts := inspectCorrelatedScalarPredicateValues(candidate, values.CorrelationIdentifier{})
		if facts.hasAnyScalar || facts.hasExistsValue ||
			predicates.ContainsExistentialPredicate(candidate) {
			movable = false
		}
		if movable {
			for corr := range predicates.GetCorrelatedToOfPredicate(candidate) {
				if !strings.EqualFold(corr.Name(), outerAlias) {
					movable = false
					break
				}
			}
		}
		if movable {
			pre = append(pre, candidate)
		} else {
			post = append(post, candidate)
		}
	}
	partition(p)
	return andOf(pre), andOf(post)
}

// rebaseCorrelatedScalarFilterPredicate makes every outer-row and scalar read
// relative to one fresh typed QOV that binds the LEFT-scalar join's materialized
// [outer..., scalar] result. This correlation is intentionally distinct from
// the join's outer/inner quantifiers: it keeps the WHERE comparison above the
// LEFT null-extension and the (possibly strict) FirstOrDefault barrier.
func rebaseCorrelatedScalarFilterPredicate(
	p predicates.QueryPredicate,
	csq logical.CorrelatedScalarSubquery,
	outerAlias string,
	outerType, mergedType *values.RecordType,
	rowCorr values.CorrelationIdentifier,
) (predicates.QueryPredicate, bool) {
	if p == nil || outerType == nil || mergedType == nil ||
		len(mergedType.Fields) != len(outerType.Fields)+1 {
		return nil, false
	}
	rowQOV, err := values.NewQuantifiedObjectValue(rowCorr, mergedType)
	if err != nil {
		return nil, false
	}
	ok := true
	rewritten := predicates.ReplaceValues(p, func(node values.Value) values.Value {
		if n, isScalar := node.(*values.ScalarSubqueryValue); isScalar {
			if n.Alias != csq.Alias {
				return node
			}
			baked, resolveErr := values.ResolveFieldOrdinals(rowQOV, []int{len(outerType.Fields)})
			if resolveErr != nil || n.Type() == nil || baked.Type() == nil || !n.Type().Equals(baked.Type()) {
				ok = false
				return node
			}
			return baked
		}
		if n, isField := values.AsFieldValue(node); isField {
			child, hasChild := values.AsQuantifiedObjectValue(n.ChildValue())
			if !hasChild || !strings.EqualFold(child.Correlation().Name(), outerAlias) {
				return node
			}
			ordinals := n.Path().Ordinals()
			if len(ordinals) == 0 {
				ok = false
				return node
			}
			ordinal := ordinals[0]
			if ordinal < 0 || ordinal >= len(outerType.Fields) {
				ok = false
				return node
			}
			baked, bakeErr := fieldOntoCorrelatedScalarRow(n, rowQOV, ordinal)
			if bakeErr != nil {
				ok = false
				return node
			}
			return baked
		}
		if n, isQOV := values.AsQuantifiedObjectValue(node); isQOV {
			// A bare outer-row QOV (row-valued expression) has no scalar WHERE
			// lowering. Field reads are handled by the FieldValue arm above.
			if strings.EqualFold(n.Correlation().Name(), outerAlias) {
				ok = false
			}
		}
		return node
	})
	if !ok {
		return nil, false
	}

	// Structural safety net: neither the unmaterialized scalar alias nor the old
	// outer binding may survive the rewrite. A survivor would evaluate unbound or
	// let a rule move the comparison below the scalar barrier.
	predicates.ReplaceValues(rewritten, func(v values.Value) values.Value {
		values.WalkValue(v, func(node values.Value) bool {
			if n, isScalar := node.(*values.ScalarSubqueryValue); isScalar {
				if n.Alias == csq.Alias {
					ok = false
					return false
				}
			}
			if n, isQOV := values.AsQuantifiedObjectValue(node); isQOV {
				if strings.EqualFold(n.Correlation().Name(), outerAlias) {
					ok = false
					return false
				}
			}
			return true
		})
		return v
	})
	if !ok {
		return nil, false
	}
	for corr := range predicates.GetCorrelatedToOfPredicate(rewritten) {
		if corr != rowCorr {
			return nil, false
		}
	}
	return rewritten, true
}

// translateFilterWithCorrelatedScalar lowers a WHERE comparison against one
// correlated scalar as:
//
//	Project(outer-only,
//	  Filter(rebased comparison,
//	    LEFT-scalar-join(outer, FirstOrDefault(inner))))
//
// The hidden scalar is available to the filter but cannot leak into the
// consumer's schema. The shared join helper is also used by projection scalars,
// so strict cardinality, NULL-on-empty, and explicit pagination cannot drift
// between the two SQL positions.
func (t *cascadesTranslator) translateFilterWithCorrelatedScalar(f *logical.LogicalFilter) expressions.RelationalExpression {
	if len(f.CorrelatedScalarSubqueries) != 1 || f.Predicate == nil {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"exactly one correlated scalar subquery in a WHERE predicate is currently supported"))
		return nil
	}
	if len(f.ExistsSubqueries) > 0 || len(f.ScalarSubqueries) > 0 {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"mixing correlated scalar and other subqueries in one WHERE predicate is not yet supported"))
		return nil
	}
	csq := f.CorrelatedScalarSubqueries[0]
	facts := inspectCorrelatedScalarPredicateValues(f.Predicate, csq.Alias)
	if !facts.hasTargetScalar {
		if facts.hasAnyScalar || facts.hasExistsValue ||
			predicates.ContainsExistentialPredicate(f.Predicate) {
			t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
				"correlated scalar WHERE carrier does not match the predicate's subquery value"))
			return nil
		}
		// Predicate simplification can erase a dead scalar expression while its
		// attached plan remains on the logical carrier (FALSE AND scalar=...,
		// TRUE OR scalar=...). Do not evaluate that dead plan and spuriously
		// raise 21000; translate the surviving ordinary filter with the carrier
		// explicitly retired.
		copyFilter := *f
		copyFilter.CorrelatedScalarSubqueries = nil
		return t.translateFilter(&copyFilter)
	}
	if t.clusterArity(f.Input) != 1 {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"correlated scalar subquery in WHERE over a multi-source outer is not yet supported"))
		return nil
	}
	prePredicate, postPredicate := partitionCorrelatedScalarWherePredicate(f.Predicate, sourceBinding(f.Input))
	if postPredicate == nil {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"correlated scalar WHERE carrier has no residual scalar consumer"))
		return nil
	}
	outerPlan := f.Input
	if prePredicate != nil {
		outerPlan = &logical.LogicalFilter{
			Input:     f.Input,
			Predicate: prePredicate,
		}
	}
	joinSelect, mergedType, outerType := t.translateSingleSourceCorrelatedScalarJoin(outerPlan, csq)
	if joinSelect == nil {
		return nil
	}
	joinRef := expressions.InitialOf(joinSelect)

	filterCorr := values.UniqueCorrelationIdentifier()
	pred, ok := rebaseCorrelatedScalarFilterPredicate(
		postPredicate,
		csq,
		sourceBinding(outerPlan),
		outerType,
		mergedType,
		filterCorr,
	)
	if !ok {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"correlated scalar WHERE predicate could not be rebound to its materialized scalar row"))
		return nil
	}
	filterExpr, err := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{pred},
		expressions.NamedForEachQuantifier(filterCorr, joinRef),
	)
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"correlated scalar WHERE filter has no exact flowed row: %v", err))
		return nil
	}
	filterRef := expressions.InitialOf(filterExpr)

	// Strip the private scalar slot. The outer type/ordinal prefix comes from
	// scalarSubqueryOrdinalSeed itself, so this projection is a proof-preserving
	// prefix identity rather than a name-based schema reconstruction.
	outputCorr := values.UniqueCorrelationIdentifier()
	outputQ := expressions.NamedForEachQuantifier(outputCorr, filterRef)
	outputQOV, err := outputQ.RequireFlowedObjectValue()
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"correlated scalar WHERE cleanup has no exact flowed row: %v", err))
		return nil
	}
	projected := make([]values.Value, len(outerType.Fields))
	aliases := make([]string, len(outerType.Fields))
	// Every name here is read off the INTERNAL merged row type, so every slot is
	// machinery-named — no `AS` reached this projection. Where that internal
	// name is a leg-qualified key, the result-set label must still report the
	// bare column, which is what the provenance buys.
	aliasMinted := make([]bool, len(outerType.Fields))
	for i := range outerType.Fields {
		fv, err := values.ResolveFieldOrdinals(outputQOV, []int{i})
		if err != nil {
			t.setTranslateErr(api.NewError(api.ErrCodeInternalError,
				"correlated scalar WHERE outer-schema projection could not bind an outer slot"))
			return nil
		}
		projected[i] = fv
		aliases[i] = outerType.Fields[i].Name
		aliasMinted[i] = true
	}
	projection, err := expressions.NewLogicalProjectionExpressionWithAliasProvenance(
		projected,
		aliases,
		aliasMinted,
		outputQ,
	)
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"correlated scalar WHERE cleanup has no exact output row: %v", err))
		return nil
	}
	return projection
}

func (t *cascadesTranslator) translateDistinct(d *logical.LogicalDistinct) expressions.RelationalExpression {
	innerRef := t.translateRef(d.Input)
	if innerRef == nil {
		return nil
	}
	distinct, err := expressions.NewLogicalDistinctExpression(
		t.namedQuantifier(sourceAlias(d.Input), innerRef),
	)
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"DISTINCT input has no exact flowed result row: %v", err))
		return nil
	}
	return distinct
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

// Java fdb-relational 4.12.11.0 supports GROUP BY end-to-end — grouped and global
// COUNT/SUM/AVG/MIN/MAX via the streaming aggregator (an aggregate index is only an
// optional fast path, never required). AstNormalizer rejects only OFFSET/LIMIT, never
// GROUP BY; QueryVisitor routes to LogicalOperator.generateGroupBy → GroupByExpression →
// ImplementStreamingAggregationRule → RecordQueryStreamingAggregationPlan. So this is
// Java-parity translation, not a Go-only extension. GROUP BY also composes over a
// derived/CTE source (GroupByQueryTests.java exercises GROUP BY over a multi-source-FROM
// derived table); an aggregate over a CTE/DISTINCT-wrapped lateral-unnest gather ordinalizes
// too — gatheredSeedBakeContext walks the identity wrappers to the seed so the group keys /
// operands bake positionally, correct in both name-model and demolition (flip) states.
func (t *cascadesTranslator) translateAggregate(a *logical.LogicalAggregate) expressions.RelationalExpression {
	if a.OutputSlots != nil {
		nativeWidth := len(a.GroupKeys) + len(a.Calls)
		seenSelectOrdinals := make(map[int]struct{}, len(a.OutputSlots))
		for i, slot := range a.OutputSlots {
			_, duplicate := seenSelectOrdinals[slot.SelectOrdinal]
			if slot.SelectOrdinal != i+1 || duplicate ||
				slot.NativeOrdinal < -1 || slot.NativeOrdinal >= nativeWidth {
				t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
					"aggregate SELECT output layout is malformed"))
				return nil
			}
			seenSelectOrdinals[slot.SelectOrdinal] = struct{}{}
		}
	}
	if a.HasHaving && a.HavingPredicate == nil {
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
	// A GATHERED unnest input un-collapses to
	// the raw per-leg seed, so positionally BAKE the group keys / operands to their flat
	// slots via the shared gatheredSeedBakeContext (see its doc) — the qualifier-honoring
	// read that replaces the retired name-keyed wrap. The outer WHERE already baked itself
	// (bakeGatedJoinPredicates fires on the SelectExpression).
	bake, bakeErr := t.gatheredSeedBakeContext(innerRef, sourceAlias(a.Input))
	if bakeErr != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"aggregate input has no exact gathered-seed row: %v", bakeErr))
		return nil
	}
	groupByQuant := bake.quant
	// Proto-FAITHFUL typed columns of the aggregate input. FALLBACK source for
	// a SUM/AVG operand's static integer width (int32 vs int64 overflow) when
	// the operand itself carries no static type — an unresolved minted carrier.
	// A RESOLVED operand states its own width (Java NumericAggregationValue
	// picks SUM_I vs SUM_L from the operand's static TypeCode) and never
	// consults this list. nil for a non-scan / derived input — untyped operands
	// over those keep the int64 (SUM_L) domain.
	aggInputFields := t.legColumns(a.Input)
	groupKeys := make([]values.Value, len(a.GroupKeys))
	projectedCTEKeys := make([]bool, len(a.GroupKeys))
	for i, key := range a.GroupKeys {
		resolvedKey := key.Value
		if resolvedKey == nil {
			candidate, resolved, resolveErr := t.exactGatheredCTEGroupKeyValue(a.Input, key, bake)
			if resolveErr != nil {
				t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
					"aggregate group key %q could not bind to the exact gathered CTE row: %v", key.Display, resolveErr))
				return nil
			}
			if resolved {
				resolvedKey = candidate
			}
		}
		if resolvedKey == nil {
			candidate, resolved, resolveErr := t.exactProjectedCTEOutputGroupKeyValue(a.Input, key, bake)
			if resolveErr != nil {
				t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
					"aggregate group key %q could not bind to the exact projected CTE row: %v", key.Display, resolveErr))
				return nil
			}
			if resolved {
				resolvedKey = candidate
				projectedCTEKeys[i] = true
			}
		}
		if resolvedKey == nil {
			t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"aggregate group key %q has no resolved exact Value", key.Display))
			return nil
		}
		groupKeys[i] = resolvedKey
		if bake.seedQOV != nil {
			var err error
			groupKeys[i], err = bakeGatheredGroupValue(
				groupKeys[i], bake.windows, bake.elementSlots, bake.seedQOV)
			if err != nil {
				t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
					"aggregate group key %q could not bind to the gathered seed: %v", key.Display, err))
				return nil
			}
		}
	}
	// Correct-or-loud floor for the PROJECTING-CTE-aggregate class: the bake was SKIPPED
	// (findWindowedSeed couldn't reach an identity-wrapped seed) but the input is a
	// POSITIONAL ordinal gather under a RESHAPING projection. The name-model fallback reads
	// each group key by NAME over the projection's output columns — which is CORRECT for a
	// BARE projection (`SELECT "AID"` names the output "AID", matching the D-stripped key
	// "AID") but silently NULL for a QUALIFIED/mis-naming one (`SELECT A."AID"` names the
	// output "A.AID", which the key "AID" cannot match). So refuse LOUD only when a group
	// key does NOT resolve against the governing projection's output-column names; a
	// name-resolvable (bare) projection is kept (pre-existing correct behavior,
	// review-caught). Both sub-cases still need projected-output-layout
	// ordinalization (Java answers GROUP BY over a projecting derived source,
	// GroupByQueryTests:699) — booked as a TODO.
	if bake.seedQOV == nil && positionalGatherUnbaked(innerRef.Get(), map[*expressions.Reference]bool{}) {
		if proj := governingProjection(innerRef.Get(), map[*expressions.Reference]bool{}); proj != nil {
			names := projectionOutputColumnNames(proj)
			for i, key := range a.GroupKeys {
				if projectedCTEKeys[i] {
					continue
				}
				if key.Value == nil || !nameResolvesInColumns(key.Display, names) {
					t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
						"aggregate GROUP BY over a projected ordinal gather has no exact output-slot binding"))
					return nil
				}
			}
		}
	}
	aggSpecs := make([]expressions.AggregateSpec, 0, len(a.Calls))
	for i := range a.Calls {
		// STRUCTURED aggregate info only (RFC-180 F-1): the builders capture
		// function/operand/star/distinct from the parse tree in
		// LogicalAggregate.Calls — the sole representation since F-3. SQL
		// text is never re-parsed here — the deleted splitter mangled nested
		// arithmetic ("(AMOUNT+10)*2" split on the inner '+') into
		// unresolvable operands that accumulated to NULL and silently
		// dropped HAVING groups.
		call := a.Calls[i]
		fn, fnOK := aggregateFunctionByName(call.Func)
		if !fnOK {
			return nil
		}
		if call.Distinct {
			// DISTINCT aggregates decline here exactly as before (the
			// distinct path is served elsewhere); a bare decline keeps the
			// established error surface.
			return nil
		}
		spec := expressions.AggregateSpec{Function: fn, OperandName: call.Operand}
		// The resolved operand (set by upgradeAggregateOperands /
		// buildCorrelatedScalar via resolver.WalkExpression) is the sole
		// source of truth. COUNT(*) is represented by a nil operand, which is
		// the GroupBy expression's explicit count-star contract. Every other
		// missing operand declines typed; no name or Unknown carrier is minted.
		switch {
		case i < len(a.AggregateOperands) && a.AggregateOperands[i] != nil:
			spec.Operand = a.AggregateOperands[i]
		case call.Star:
			spec.Operand = nil
		default:
			t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"aggregate operand %q did not resolve to an exact Value", call.Operand))
			return nil
		}
		if bake.seedQOV != nil && spec.Operand != nil {
			var err error
			spec.Operand, err = bakeGatheredGroupValue(
				spec.Operand, bake.windows, bake.elementSlots, bake.seedQOV)
			if err != nil {
				t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
					"aggregate operand %q could not bind to the gathered seed: %v", call.Operand, err))
				return nil
			}
		}
		if i < len(a.Aliases) && a.Aliases[i] != "" {
			// VERBATIM. The alias reached the logical layer through
			// functions.NormalizeIdentifier at the parse boundary, so folding
			// it here is the same second normalization this whole family of
			// defects is.
			//
			// Say what the removal of that fold rests on, because it is NOT a
			// corpus observation. Instrumented over the 2593-query corpus this
			// branch is taken THREE times, and all three carry a canonical
			// aggregate name that is already upper (`COUNT(*)`,
			// `MAX(E2.SALARY)`, `SUM(ORDERS.QTY)`) — so the fold was a no-op
			// everywhere it ran, and no query shape could be built that drove
			// it with a delimited spelling. What IS pinned is the authority
			// downstream: GroupByOutputColumnNames publishes an alias verbatim
			// (expressions.TestGroupByOutputColumnNames_AliasIsVerbatim), so a
			// fold reintroduced anywhere on the way to it is a fold that
			// authority will faithfully report.
			spec.Alias = a.Aliases[i]
		}
		// PLAN-TIME numeric-operand gate (Java NumericAggregationValue.encapsulate).
		// Java looks the aggregate up in an operator map keyed by (function, operand
		// TypeCode) whose entries are ONLY numeric (INT/LONG/FLOAT/DOUBLE); a
		// non-numeric operand yields a null lookup and Verify.verifyNotNull throws
		// "unable to encapsulate aggregate operation due to type mismatch(es)" at
		// PLAN time — data-INDEPENDENT, so an empty or all-NULL table errors too
		// (not the data-dependent per-row runtime gate). Mirror that for
		// SUM/AVG/MIN/MAX; the operand's static type is reliable for the
		// numeric/non-numeric split (only the INT32→LONG width is widened, and both
		// are numeric). COUNT accepts any type (CountValue, not
		// NumericAggregationValue) and is not gated. Unknown static type falls
		// through to the runtime backstop.
		if aggregateRejectsNonNumericOperand(spec.Function) && spec.Operand != nil {
			if ot := spec.Operand.Type(); ot != nil {
				if code := ot.Code(); code != values.TypeCodeUnknown && !code.IsNumeric() {
					t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedOperation,
						"unable to encapsulate aggregate operation due to type mismatch(es)"))
					return nil
				}
			}
		}
		// Static integer WIDTH of the operand (SUM_I vs SUM_L): the operand's
		// own static type first (Java's encapsulate rule), the proto-faithful
		// input columns as the untyped-carrier fallback. INTEGER (TYPE_INT32)
		// → int32 overflow in the executor.
		spec.OperandIntType = aggregateOperandIntType(spec.Operand, aggInputFields)
		aggSpecs = append(aggSpecs, spec)
	}
	groupBy, err := expressions.NewGroupByExpression(
		groupKeys,
		aggSpecs,
		groupByQuant,
	)
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"aggregate has no exact output row: %v", err))
		return nil
	}
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

	// HAVING is the first layer that owns the GroupBy output quantifier. Bind
	// every aggregate/group-key draft directly to that exact row; the logical
	// layer intentionally carries no fabricated current/Unknown FieldValue.
	havingQ := expressions.ForEachQuantifier(groupByRef)
	havingQOV, err := havingQ.RequireFlowedObjectValue()
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"HAVING has no exact aggregate output quantifier: %v", err))
		return nil
	}
	havingPred, havingBindErr := predicates.TransformEmbeddedValuesChecked(a.HavingPredicate, func(v values.Value) (values.Value, error) {
		return bindPostAggregateValue(v, a, havingQOV)
	})
	if havingBindErr != nil {
		t.setTranslateErr(havingBindErr)
		return nil
	}
	filter, err := expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{havingPred},
		havingQ,
	)
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"HAVING filter has no exact flowed result: %v", err))
		return nil
	}
	return filter
}

// aggregateRejectsNonNumericOperand reports whether fn is one of the numeric
// aggregates whose Java operator map (NumericAggregationValue) has numeric-only
// entries, so a non-numeric operand must be rejected at plan time. COUNT is the
// only aggregate that accepts a non-numeric operand.
func aggregateRejectsNonNumericOperand(fn expressions.AggregateFunction) bool {
	switch fn {
	case expressions.AggSum, expressions.AggAvg, expressions.AggMin, expressions.AggMax:
		return true
	}
	return false
}

// aggregateOperandIntType returns the operand's STATIC integer width for the
// int32-vs-int64 SUM/AVG overflow decision.
//
// The AUTHORITY is the operand's own static result type — Java's exact rule:
// NumericAggregationValue.encapsulate keys the operator map on
// `Pair.of(logicalOperator, type0.getTypeCode())` where type0 is
// `arguments.get(0).getResultType()` (NumericAggregationValue.java:196-209),
// so SUM over an INT-typed operand is SUM_I (Math.addExact on int, 22003 at
// the int32 boundary) no matter what expression tree the operand sits in — a
// join-leg reference carries its column's own INT in Java exactly like a
// single-table one. Go's resolver states the same width on the reference since
// the width-faithful typing of RFC-181 P0.5 (sqlTypeToCascadesType: INTEGER →
// TypeCodeInt), and it survives a join because the correlated resolution arm
// stamps columnCascadesType on the reference itself. Trusting the static type
// here is the same trust the plan-time numeric-operand gate above already
// extends to it.
//
// The ORDINAL fallback below serves the one population without a static type:
// a minted bare-column carrier (Typ Unknown — the catalog pass did not run, or
// resolution declined). It is keyed off the proto-faithful input record type,
// identified by ORDINAL in inputFields' layout, never by display name
// (RFC-197). Two lists meet there and they are derived SEPARATELY — the
// operand was baked against the translated expression's output columns, while
// inputFields comes from the logical input's leg columns — so the ordinal is
// only usable if the two describe the same layout. That is not assumed:
// OrdinalIn is given inputFields' own ordinal domain and answers only when the
// operand's baked domain IS that layout; a reference from some other layout,
// or with a non-zero correlation (inputFields describes the aggregate input's
// OWN row), declines and keeps the int64 SUM_L domain — the direction that
// cannot manufacture a wrong overflow
// (TestFDB_AggregateOperandWidthDeclinesForeignLayout pins a BIGINT join sum
// answering, not 22003).
func aggregateOperandIntType(operand values.Value, _ []values.Field) values.TypeCode {
	if operand != nil {
		if ot := operand.Type(); ot != nil {
			if code := ot.Code(); code != values.TypeCodeUnknown {
				return code
			}
		}
	}
	// RFC-232 removes the Unknown carrier whose physical ordinal was used as a
	// second type authority. Exact resolved operands state their width directly;
	// an unstated operand is rejected by GroupBy construction before execution.
	return values.TypeCodeUnknown
}

// aggregateFunctionByName maps the parse-tree-captured aggregate function
// name (logical.AggregateCall.Func) to its typed AggregateFunction. Unknown
// names decline (same surface as the retired text parser).
func aggregateFunctionByName(name string) (expressions.AggregateFunction, bool) {
	switch name {
	case "COUNT":
		return expressions.AggCount, true
	case "SUM":
		return expressions.AggSum, true
	case "MIN":
		return expressions.AggMin, true
	case "MAX":
		return expressions.AggMax, true
	case "AVG":
		return expressions.AggAvg, true
	default:
		return 0, false
	}
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

	// The ENCLOSED unnest class (`FROM A, A.arr AS x, B`
	// — an unnest join buried as a leg of this inner cluster) gathers into
	// the same flat (N+1)-quantifier select via rotation. Fail-open: nil
	// falls through to the paths below, which translate the buried unnest
	// ENCLOSED (the name-model residual) with the faithful diagnostics.
	if sel := t.translateEnclosedUnnestGather(j); sel != nil {
		return sel
	}

	// The BURIED CHAINED spine (`FROM t, t.arr AS x, x.sub AS y,
	// z` — a chained spine behind trailing plain legs) rotates to the
	// box-bottom chained form and re-dispatches, so the whole cluster takes
	// the chained ordinal seed instead of name-modeling the trailing join
	// around a buried spine leg. Same enclosure stance as the single-link
	// gather above; fail-open (an inadmissible rotated spine keeps the
	// original tree and the paths below).
	if !t.inInnerCluster && !t.unnestUnderExistential {
		if rotated, ok := t.rotateBuriedChainedSpine(j); ok {
			return t.translateJoin(rotated)
		}
	}

	left := j.Left
	right := j.Right
	kind := j.Kind
	if kind == logical.JoinRight {
		left, right = right, left
		kind = logical.JoinLeft
	}

	// Decide (and record) the ordinal-wedge gate for this
	// seed BEFORE leg translation mutates the enclosure flag. Consumed
	// below: a gated join seeds the ORDINAL result value + baked
	// predicates instead of the name-model anchored RC.
	gateDecision := t.ordinalWedgeGate(j)

	// A GATED INNER root with NESTED inner joins translates FLAT
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
	// Legs of a GATED parent translate FRESH (their own inner
	// joins gate independently; SelectMergeRule composes ordinal RVs -- see
	// translateGatheredInnerCluster). Enclosure poisoning survives only for
	// NAME-MODEL parents (a non-gated inner cluster, a LEFT box preserved
	// leg -- the RFC-153 dissolve/flatten machinery stays name-model).
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
	// A GATED binary join's quantifiers and
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
		// duplicate legs — the differential carve-out class).
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
			// CheckBuriedExistentialPredicate requires and the existential peel
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
	var resultValue values.Value
	if gateDecision.Gated {
		// The ordinal wedge seed: baked ofOrdinalNumber
		// concatenation of the two legs + eager (leg, ordinal) predicate
		// baking.
		// rvLeft/rvRight are DECLARATION order — only the
		// null-supplying ROLE keys on the original kind; a RIGHT join's null
		// side is its LEFT operand.
		var legTypes map[string]bakeLegType
		resultValue, legTypes = t.buildOrdinalJoinResultValue([]clusterLeg{
			clusterLegOf(rvLeft, j.Kind == logical.JoinRight || j.Kind == logical.JoinFull),
			clusterLegOf(rvRight, j.Kind == logical.JoinLeft || j.Kind == logical.JoinFull),
		})
		preds = bakeGatedJoinPredicates(preds, legTypes)
	}
	if resultValue == nil {
		// UNGATED, or a gated leg's columns are not derivable (the catalog-free
		// nil-md path). The name-model anchored seed has been deleted (a
		// full-suite producer census fired zero, so no production shape
		// reaches here); an ungated join is untranslatable — LOUD, with the
		// gate's own Reason naming the cause, never silent wrong rows.
		if t.translateErr == nil {
			reason := gateDecision.Reason
			if reason == "" {
				reason = "a leg's columns are not derivable"
			}
			t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"join did not ordinalize (%s)", reason))
		}
		return nil
	}

	quantifiers := []expressions.Quantifier{leftQ, rightQ}
	sourceAliases := []string{leftAlias, rightAlias}

	// EXISTS in the ON clause (RFC-154 §5): attach each lifted EXISTS subquery as
	// an existential quantifier + its correlation predicate, producing a
	// 2-ForEach-+-Existential SelectExpression that the NLJ rule's
	// the existential peel path lowers to a semi-join. Only populated for
	// INNER joins (upgradeJoinOnPredicates rejects OUTER EXISTS-in-ON), so the
	// joinType passed below is JoinInner and the existential semantics match
	// EXISTS-in-WHERE-over-a-join (translateJoinWithExists).
	// Defensive polarity guard for the ON-lift path: a flagged esq under a
	// negated ON marker would outer-route an outer-only conjunct into
	// anti-join semantics (same law as the WHERE sites).
	if onPred, ok := j.OnPredicate.(predicates.QueryPredicate); ok && t.declineNegatedOuterOnlyEsq(onPred, j.OnExistsSubqueries) {
		return nil
	}
	if hasKnownExistsTruth(j.OnExistsSubqueries) {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"a cardinality-known EXISTS in a JOIN ON clause is not yet supported"))
		return nil
	}
	for _, esq := range j.OnExistsSubqueries {
		subRef := t.translateSubqueryRef(esq.Plan)
		if subRef == nil {
			return nil
		}
		existQ := expressions.NamedExistentialQuantifier(esq.Alias, subRef)
		quantifiers = append(quantifiers, existQ)
		innerCorrName, joinPred := t.existsInnerCorrelation(esq)
		if joinPred != nil {
			preds = append(preds, joinPred)
		}
		sourceAliases = append(sourceAliases, innerCorrName)
	}

	return t.exactSelectWithJoinType(
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
// the existential peel path handles this 2+1 pattern.
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

	// The flatten consults the wedge
	// gate (ONE authority — ordinalWedgeGate) and,
	// when the join gates, seeds the baked ordinal RC below instead of the
	// anchored one. Decided BEFORE leg translation mutates the enclosure flag
	// (the translateJoin convention). Two flatten-specific narrowings on top
	// of the shared decision:
	//   - Arity EXACTLY 2: this arm builds exactly two ForEach legs, and
	//     buildOrdinalJoinResultValue types them as single-source legs — a
	//     nested-cluster leg would seed a 2-leg concat whose windows
	//     disagree with the arity SelectMergeRule's flattening produces.
	//     The DECLINE is the safety mechanism itself. The N-way flatten
	//     rides the gathered-cluster machinery instead.
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
	// consumers (the WHERE-conjunct baking arm, the ENCLOSURE LIFT above)
	// read, and a Gated record over an ANCHORED seed would misroute
	// them. The record always matches the seed actually built below.
	if t.wedgeGate == nil {
		t.wedgeGate = make(map[*logical.LogicalJoin]wedgeGateDecision)
	}
	t.wedgeGate[j] = gateDecision

	// Flatten join + EXISTS into a single SelectExpression
	// with ForEach(left), ForEach(right), and Existential quantifiers.
	// A NON-gated flat select is a
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
	// A GATED flatten's quantifiers and source
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

	// Add EXISTS subqueries as existential quantifiers. A minted-binding
	// (duplicate-alias) gated flatten with a LEG-INDEPENDENT EXISTS does not
	// decline here — the executor's identity-FlatMap pass-through
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
		innerCorrName, joinPred := t.existsInnerCorrelation(esq)
		if joinPred != nil {
			allPreds = append(allPreds, joinPred)
		}
		sourceAliases = append(sourceAliases, innerCorrName)
	}

	// The RV uses DECLARATION order (Java assembles the
	// result value in source order regardless of join type).
	//
	// A GATED flatten seeds the baked
	// ordinal RC over its two ForEach legs — existential quantifiers
	// contribute NO columns (Java's model: existentials carry no output) —
	// and bakes the COMBINED predicate list (join ON + WHERE conjuncts + the
	// EXISTS correlation predicates). The baked correlation predicates
	// flow into the existential FlatMap's inner plan as FrontierPinned
	// references over the merged outer, where the outer binder binds the
	// outer positionally and the ordinal existential rebase handles the
	// merged references.
	var resultValue values.Value
	if gatedFlatten {
		var legTypes map[string]bakeLegType
		// The null-supplying side is the JOIN KIND's, exactly as the plain
		// gated binary arm derives it. Seeding both legs as preserved dropped
		// the outer-join null-extension from this shape alone: the seed's leg
		// QOV then flowed a NON-nullable record, and the physical join —
		// which computes its null-supplying aliases from the same join kind —
		// refused its own output layout ("null-supplying source Q must be
		// nullable"). A projected EXISTS over a LEFT JOIN could not plan at
		// all, which is a query Java answers.
		resultValue, legTypes = t.buildOrdinalJoinResultValue([]clusterLeg{
			clusterLegOf(j.Left, j.Kind == logical.JoinRight || j.Kind == logical.JoinFull),
			clusterLegOf(j.Right, j.Kind == logical.JoinLeft || j.Kind == logical.JoinFull),
		})
		allPreds = bakeGatedJoinPredicates(allPreds, legTypes)
	}
	if resultValue == nil {
		// UNGATED, or a gated leg's columns are not derivable (the catalog-free
		// nil-md path). The name-model anchored seed has been deleted; mirrors
		// translateJoin — a nil result value must not flow into the
		// SelectExpression (it would nil-deref downstream, e.g.
		// GetCorrelatedToOfValue), so the shape is untranslatable — LOUD, with
		// the gate's own Reason naming the cause.
		if t.translateErr == nil {
			reason := gateDecision.Reason
			if reason == "" {
				reason = "a leg's columns are not derivable"
			}
			t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"existential join flatten did not ordinalize (%s)", reason))
		}
		return nil
	}
	return t.exactSelectWithJoinType(
		resultValue,
		quantifiers,
		allPreds,
		sourceAliases,
		expressions.JoinInner,
	)
}

// foldKnownExists substitutes exact cardinality-derived EXISTS results in a
// filter's top-level AND conjuncts. KnownTruth describes EXISTS itself; a
// NOT-EXISTS marker receives the boolean inverse. TRUE conjuncts disappear and
// FALSE absorbs the entire AND. The corresponding subquery plan is removed, so
// neither correlation routing nor execution can perturb the constant result.
//
// The existential lowering supports these markers as top-level AND conjuncts.
// For robustness, an alias is folded only when every occurrence in the whole
// predicate is one of those direct conjuncts. An occurrence nested under OR or
// another unsupported boolean shape leaves the filter unchanged for that alias
// rather than orphaning a marker by dropping its quantifier.
func (t *cascadesTranslator) foldKnownExists(f *logical.LogicalFilter) *logical.LogicalFilter {
	if f == nil || f.Predicate == nil || len(f.ExistsSubqueries) == 0 {
		return f
	}
	known := make(map[values.CorrelationIdentifier]predicates.TriBool)
	for _, esq := range f.ExistsSubqueries {
		if esq.KnownTruth != nil {
			known[esq.Alias] = esq.KnownTruth
		}
	}
	if len(known) == 0 {
		return f
	}

	// Count every marker occurrence, stopping at a recognized NOT-EXISTS so its
	// existential child is not counted a second time.
	allOccurrences := make(map[values.CorrelationIdentifier]int)
	predicates.WalkPredicate(f.Predicate, func(p predicates.QueryPredicate) bool {
		if alias, ok := predicates.IsExistentialPredicate(p); ok {
			allOccurrences[alias]++
			return false
		}
		if alias, ok := predicates.IsNotExistentialPredicate(p); ok {
			allOccurrences[alias]++
			return false
		}
		return true
	})

	var conjuncts []predicates.QueryPredicate
	var flattenAnd func(predicates.QueryPredicate)
	flattenAnd = func(p predicates.QueryPredicate) {
		if and, ok := p.(*predicates.AndPredicate); ok {
			for _, sub := range and.SubPredicates {
				flattenAnd(sub)
			}
			return
		}
		conjuncts = append(conjuncts, p)
	}
	flattenAnd(f.Predicate)

	type marker struct {
		alias   values.CorrelationIdentifier
		negated bool
		ok      bool
	}
	markers := make([]marker, len(conjuncts))
	directOccurrences := make(map[values.CorrelationIdentifier]int)
	for i, conjunct := range conjuncts {
		if alias, ok := predicates.IsExistentialPredicate(conjunct); ok {
			if _, isKnown := known[alias]; isKnown {
				markers[i] = marker{alias: alias, ok: true}
				directOccurrences[alias]++
			}
			continue
		}
		if alias, ok := predicates.IsNotExistentialPredicate(conjunct); ok {
			if _, isKnown := known[alias]; isKnown {
				markers[i] = marker{alias: alias, negated: true, ok: true}
				directOccurrences[alias]++
			}
		}
	}

	eligible := make(map[values.CorrelationIdentifier]struct{})
	for alias, direct := range directOccurrences {
		if direct > 0 && direct == allOccurrences[alias] {
			eligible[alias] = struct{}{}
		}
	}
	if len(eligible) == 0 {
		return f
	}

	keptPredicates := make([]predicates.QueryPredicate, 0, len(conjuncts))
	foldedAliases := make(map[values.CorrelationIdentifier]struct{})
	for i, conjunct := range conjuncts {
		m := markers[i]
		if !m.ok {
			keptPredicates = append(keptPredicates, conjunct)
			continue
		}
		if _, canFold := eligible[m.alias]; !canFold {
			keptPredicates = append(keptPredicates, conjunct)
			continue
		}
		foldedAliases[m.alias] = struct{}{}
		truth := *known[m.alias]
		if m.negated {
			truth = !truth
		}
		if !truth {
			f2 := *f
			// FALSE absorbs every EXISTS conjunct, so none of their quantifiers
			// need to be built. EXISTS has no side effects.
			f2.ExistsSubqueries = nil
			f2.Predicate = predicates.NewConstantPredicate(predicates.TriFalse)
			return &f2
		}
		// TRUE disappears from an AND.
	}

	keptSubqueries := make([]logical.ExistsSubquery, 0, len(f.ExistsSubqueries))
	for _, esq := range f.ExistsSubqueries {
		if _, folded := foldedAliases[esq.Alias]; !folded {
			keptSubqueries = append(keptSubqueries, esq)
		}
	}
	var rewritten predicates.QueryPredicate
	switch len(keptPredicates) {
	case 0:
		rewritten = predicates.NewConstantPredicate(predicates.TriTrue)
	case 1:
		rewritten = keptPredicates[0]
	default:
		rewritten = predicates.NewAnd(keptPredicates...)
	}
	f2 := *f
	f2.ExistsSubqueries = keptSubqueries
	f2.Predicate = rewritten
	return &f2
}

func hasKnownExistsTruth(subqueries []logical.ExistsSubquery) bool {
	for _, esq := range subqueries {
		if esq.KnownTruth != nil {
			return true
		}
	}
	return false
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
		if qov, isQOV := values.AsQuantifiedObjectValue(ev.Value); isQOV {
			if _, isFlagged := flagged[qov.Correlation()]; isFlagged {
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
// table's name. Since the collision mint, buildCorrelatedExists already builds
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
func (t *cascadesTranslator) existsInnerCorrelation(esq logical.ExistsSubquery) (string, predicates.QueryPredicate) {
	// The rename is ONLY safe when the inner has ONE well-defined source alias
	// (existsInnerSafeToRename) — a plain single-table scan, optionally under
	// further filters, INCLUDING a nested EXISTS: the rebase below touches only
	// esq.JoinPredicate, a value tree entirely separate from esq.Plan, so it can
	// neither reach nor need to reach a correlation buried inside esq.Plan (that
	// reference resolves in the nested plan's own re-translation, over the same
	// unchanged scan alias). The one inner shape that DOES carry a reference the
	// rename cannot reach is a JOIN inner: it emits a MERGED row resolved by
	// qualified leg keys (T2.ID, T3.T2_ID, …), never a single-alias binding
	// (executePredicatesFilter: producesMergedRows ⇒ bindAlias=false); pointing
	// the predicate at a `<uniqueAlias>.*` namespace nothing writes yields NULL.
	// That keeps the leg/source-alias routing — the merged-row inner routes by
	// distinct qualified keys and cannot clobber the outer binding.
	if !existsInnerSafeToRename(esq.Plan) {
		return sourceAlias(esq.Plan), esq.JoinPredicate
	}
	uniqueAlias := esq.Alias
	srcAlias := values.NamedCorrelationIdentifier(sourceAlias(esq.Plan))
	joinPred := esq.JoinPredicate
	if joinPred != nil && srcAlias != uniqueAlias {
		aliasMap, err := values.NewAliasMap([]values.AliasPair{{Source: srcAlias, Target: uniqueAlias}})
		if err != nil {
			t.setTranslateErr(api.NewErrorf(api.ErrCodeInternalError,
				"invalid EXISTS correlation rebase from %s to %s: %v", srcAlias.Name(), uniqueAlias.Name(), err))
			return "", nil
		}
		// CHECKED, and this is the site where the error-less spelling is most
		// dangerous. It "fails closed with nil" — but nil is not closed HERE: it
		// is the NO-JOIN-PREDICATE sentinel this function's own caller tests
		// (`if joinPred != nil { preds = append(...) }`). A failed rebase would
		// therefore drop the correlation entirely and turn a correlated EXISTS
		// into one that matches EVERY outer row, silently and with the right
		// row count for the uncorrelated reading. The arm just above already
		// routes its error this way; this one has to match it.
		//
		// RFC-232 is what makes this reachable rather than theoretical: the
		// failure originates in values.RebaseValueChecked, and exact types are
		// precisely what gave value reconstruction something to reject.
		rebased, rerr := predicates.RebasePredicateChecked(joinPred, aliasMap)
		if rerr != nil {
			t.setTranslateErr(api.NewErrorf(api.ErrCodeInternalError,
				"EXISTS correlation rebase from %s to %s: %v", srcAlias.Name(), uniqueAlias.Name(), rerr))
			return "", nil
		}
		joinPred = rebased
	}
	return uniqueAlias.Name(), joinPred
}

// existsInnerSafeToRename reports whether an existential subquery's plan has
// ONE well-defined source alias (sourceAlias(op)) that fully captures the
// plan's correlatable identity as seen from OUTSIDE — the shape for which
// rebasing esq.JoinPredicate's references to that alias, onto the unique
// existential alias, is safe. Returns false for a JOIN (a merged row keyed by
// SEVERAL leg aliases — one rebase target cannot capture them all) and a
// CTE/derived-table (its own correlation namespace). A LogicalFilter carrying
// its OWN nested ExistsSubqueries is safe to walk through: the rename only
// rewrites esq.JoinPredicate (a value tree entirely separate from esq.Plan),
// so it can never reach — and never needs to reach — a correlation buried
// INSIDE esq.Plan. That inner reference resolves in the nested plan's OWN
// re-translation (translateFilter's own sourceAlias(f.Input) call), which
// walks the SAME unchanged scan and so recomputes the SAME alias regardless
// of what the enclosing existential quantifier ends up named. Declining to
// rename here left esq.JoinPredicate referencing the plan's OLD internal
// alias while the enclosing quantifier is registered under esq.Alias — two
// different CorrelationIdentifiers — so the join predicate resolved against
// nothing the outer SelectExpression's row-eval context ever bound (a
// nested-EXISTS-with-its-own-correlation middle, e.g. `EXISTS (SELECT 1 FROM
// t WHERE t.c < outer.x AND EXISTS (SELECT 1 FROM u WHERE u.c < t.c))`, hit
// this: the middle correlation `t.c < outer.x` rode the plan's un-renamed
// scan alias, and nothing in the enclosing select bound it). Walks the
// single-child chain the same way sourceAlias does.
func existsInnerSafeToRename(op logical.LogicalOperator) bool {
	for cur := op; cur != nil; {
		switch o := cur.(type) {
		case *logical.LogicalInlineValues:
			return true
		case *logical.LogicalScan:
			return true
		case *logical.LogicalJoin:
			return false
		case *logical.LogicalCTE:
			return false
		case *logical.LogicalFilter:
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
		case *logical.LogicalInlineValues:
			return strings.ToUpper(o.Alias)
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
			if o.PreserveMainSource {
				return sourceAlias(o.Main)
			}
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
// parser minted a duplicate-alias binding id (LogicalScan/LogicalCTE/LogicalUnnest.Binding,
// carried from the single mint
// authority assignFromLegBindingIDs). Every correlation / bake-map / window
// key reads THIS; display surfaces keep sourceAlias. Minted ids are
// FOLD-STABLE upper form (`Q$DUPN`) so the existing UPPER-fold lookups treat
// them exactly like aliases; alias-bound legs return sourceAlias's UPPER
// form, so non-duplicate queries are byte-identical to the original alias-only keying.
// mintedBindingLeg returns the first FROM-leg source in the given subtrees
// carrying a parser-minted duplicate-alias binding (Scan/CTE/Unnest .Binding
// — set ONLY when a later duplicate leg was renamed; "" everywhere else), or
// "" when none. The name-model join machinery keys its anchored RC and merged
// rows by DISPLAY alias, which cannot represent a minted binding (two
// same-named legs collide last-wins; the resolver's binding-qualified
// references read NULL off the display-keyed row). Callers use this to
// decline LOUDLY at every name-model construction a minted-binding query can
// narrow into — never silent wrong rows. It does
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
		case *logical.LogicalInlineValues:
			if o.Binding != "" {
				return o.Binding
			}
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
		case *logical.LogicalInlineValues:
			if o.Binding != "" {
				return o.Binding
			}
			return sourceAlias(cur)
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
			if o.PreserveMainSource {
				return sourceBinding(o.Main)
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
		origCols := extractOutputColumns(body)
		starBodied := false
		if len(origCols) == 0 {
			// A star-bodied CTE (`WITH c1(x, y, z) AS (SELECT * FROM t)`)
			// has NO final projection — the plan bottoms at the scan — so
			// extractOutputColumns sees nothing and the aliases were
			// silently dropped: `SELECT * FROM c1` came back labeled with
			// the TABLE's column names where Java labels X, Y, Z
			// (SemanticAnalyzer expands the star before the alias check;
			// cte.yamsql pins the labels). Expand the star from metadata
			// here, exactly like the executor will.
			origCols, starBodied = t.starBodyColumns(body)
		}
		switch {
		case len(origCols) == len(c.ColumnAliases):
			// The re-aliasing projection reads POSITIONALLY (baked
			// ordinals), not by name: CTE column lists are positional,
			// and duplicate body output labels (`SELECT id AS x, v AS x`)
			// would make both name-based reads bind the first slot,
			// silently duplicating its values.
			proj := logical.NewProject(body, origCols, c.ColumnAliases)
			proj.ProjectedValues = make([]values.Value, len(origCols))
			// No projection input quantifier exists in the logical layer. Carry
			// only the positional metadata; translateProject resolves it against
			// the real exact input quantifier before publishing any Value.
			proj.InputOrdinals = make([]int, len(origCols))
			for i := range origCols {
				proj.InputOrdinals[i] = i
			}
			body = proj
		case len(origCols) > 0 && (starBodied || cteBodyWidthIsExact(body)):
			// The POINT-OF-TRUTH arity check (Java SemanticAnalyzer.
			// validateCteColumnAliases): the body is BUILT here, so its
			// output width is the real one — every shape (nested WITH,
			// lateral unnest, qualified stars, shadowed sources) validates
			// uniformly, with no parallel static width predictor to drift.
			// Silently skipping the aliases instead executed the CTE with
			// the mismatched list ignored. Rejection fires only for
			// EXACT-width roots (Project): an Aggregate root's
			// extractOutputColumns is its deduplicated internal layout,
			// which legitimately differs from the visible SELECT list
			// (`SELECT id, id … GROUP BY id` is 2 visible over 1 internal).
			t.setTranslateErr(api.NewErrorf(api.ErrCodeInvalidColumnReference,
				"cte query has %d column(s), however %d aliases defined",
				len(origCols), len(c.ColumnAliases)))
			return nil
		}
		// Unknown or inexact widths stay lenient — never reject a valid
		// query on an unmodeled shape.
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

// starBodyColumns expands the output columns of a projection-less CTE body —
// one that bottoms at a table scan through width-transparent operators — from
// the table's metadata, in declaration order. Returns (nil, false) when the
// body's bottom is not a resolvable scan (set operations, unnests, …), which
// keeps those shapes on the lenient no-rename path.
func (t *cascadesTranslator) starBodyColumns(op logical.LogicalOperator) ([]string, bool) {
	for {
		switch o := op.(type) {
		case *logical.LogicalDistinct:
			op = o.Input
		case *logical.LogicalSort:
			op = o.Input
		case *logical.LogicalLimit:
			op = o.Input
		case *logical.LogicalFilter:
			op = o.Input
		case *logical.LogicalScan:
			fields := t.tableColumns(o.Table)
			if len(fields) == 0 {
				return nil, false
			}
			cols := make([]string, len(fields))
			for i, f := range fields {
				cols[i] = f.Name
			}
			return cols, true
		default:
			return nil, false
		}
	}
}

func extractOutputColumns(op logical.LogicalOperator) []string {
	switch o := op.(type) {
	case *logical.LogicalProject:
		// Per-slot alias preference: the OUTPUT name of an aliased
		// projection slot is the alias (`SELECT id AS x` outputs X, not
		// ID). Returning the underlying Projections handed translateCTE
		// stale source names — its re-aliasing projection then read ID
		// from a row shaped [X,Y] and failed ordinal resolution at
		// runtime.
		if len(o.Aliases) == len(o.Projections) {
			cols := make([]string, len(o.Projections))
			for i, proj := range o.Projections {
				if o.Aliases[i] != "" {
					cols[i] = o.Aliases[i]
				} else {
					cols[i] = proj
				}
			}
			return cols
		}
		return o.Projections
	case *logical.LogicalAggregate:
		var cols []string
		for _, k := range o.GroupKeys {
			cols = append(cols, k.Display)
		}
		for i, call := range o.Calls {
			if i < len(o.Aliases) && o.Aliases[i] != "" {
				cols = append(cols, o.Aliases[i])
			} else {
				cols = append(cols, call.CanonicalName())
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
	case *logical.LogicalCTE:
		// A CTE carrier's output is its MAIN query's output — a nested-WITH
		// body (`c2(a) AS (WITH c3 … SELECT x FROM c3)`) wraps its SELECT in
		// LogicalCTE(c3, Main=SELECT). Without this arm the c2(a) column-alias
		// projection in translateCTE never applied (extractOutputColumns
		// returned nil ≠ len(aliases)) and c2's output kept the body's inner
		// spelling — a later `SELECT a FROM c2` died at runtime with an
		// ordinal-resolution error on the aliased name.
		return extractOutputColumns(o.Main)
	case *logical.LogicalUnion:
		// SQL exposes a set operation's FIRST branch's output names, so that is
		// the list a column-alias rename replaces. Without this arm a
		// union-bodied `WITH u(k,n) AS (… UNION ALL …)` found no columns to
		// rename, silently kept the branch's own labels, and the renamed
		// reference then disagreed with the row the CTE declares — a hard exact
		// type conflict at evaluation, not a cosmetic label.
		if len(o.Inputs) == 0 {
			return nil
		}
		return extractOutputColumns(o.Inputs[0])
	}
	return nil
}

// cteBodyWidthIsExact reports whether extractOutputColumns(op) is the
// body's VISIBLE output width (a projection root — possibly under
// row-preserving Sort/Limit/Filter/Distinct — lists the SELECT items
// one-to-one). Aggregate roots are NOT exact: their column list is the
// deduplicated internal grouping layout, which a SELECT list may read
// multiple times.
func cteBodyWidthIsExact(op logical.LogicalOperator) bool {
	switch o := op.(type) {
	case *logical.LogicalProject:
		return true
	case *logical.LogicalDistinct:
		return cteBodyWidthIsExact(o.Input)
	case *logical.LogicalSort:
		return cteBodyWidthIsExact(o.Input)
	case *logical.LogicalLimit:
		return cteBodyWidthIsExact(o.Input)
	case *logical.LogicalFilter:
		return cteBodyWidthIsExact(o.Input)
	case *logical.LogicalCTE:
		return cteBodyWidthIsExact(o.Main)
	}
	return false
}

// ValidateCTEAliasArities walks the BUILT logical tree and applies the
// point-of-truth alias-arity check to EVERY LogicalCTE carrying column
// aliases — including CTEs that are never referenced (whose bodies
// translateCTE registers lazily and never descends into) and CTEs nested
// inside another CTE's body. Same exact-width rule as translateCTE's inline
// backstop; recursive CTEs are excluded (their seed/recursive arms have
// their own validation path).
func ValidateCTEAliasArities(op logical.LogicalOperator) error {
	if op == nil {
		return nil
	}
	if c, ok := op.(*logical.LogicalCTE); ok {
		if len(c.ColumnAliases) > 0 && !c.Recursive {
			if origCols := extractOutputColumns(c.Body); len(origCols) > 0 &&
				len(origCols) != len(c.ColumnAliases) && cteBodyWidthIsExact(c.Body) {
				return api.NewErrorf(api.ErrCodeInvalidColumnReference,
					"cte query has %d column(s), however %d aliases defined",
					len(origCols), len(c.ColumnAliases))
			}
		}
	}
	for _, child := range op.Children() {
		if err := ValidateCTEAliasArities(child); err != nil {
			return err
		}
	}
	// Attached subquery plans (EXISTS/scalar on filters, projections,
	// HAVING, ON) are not Children() — a CTE declared inside one escaped
	// the walk.
	for _, sub := range logical.AttachedPlans(op) {
		if err := ValidateCTEAliasArities(sub); err != nil {
			return err
		}
	}
	return nil
}

// recursiveCTECommonResultRow derives the exact positional row that both
// recursive-union inserts and the intervening temp-table scan must publish.
// Output names are seed-authoritative; branch-local names do not participate in
// compatibility. Each slot is folded through SQL's implicit-promotion lattice,
// including nullability widening, before the recursive branch is translated.
func (t *cascadesTranslator) recursiveCTECommonResultRow(
	seed *values.RecordType,
	recursiveBranches []logical.LogicalOperator,
	outCols []string,
) (*values.RecordType, error) {
	if seed == nil || len(seed.Fields) != len(outCols) {
		return nil, fmt.Errorf("seed row width does not match the recursive CTE output")
	}
	fields := make([]values.Field, len(outCols))
	for i := range fields {
		fields[i] = values.Field{
			Name:      outCols[i],
			Ordinal:   i,
			FieldType: seed.Fields[i].FieldType,
		}
	}
	resultNullable := seed.Nullable
	for branchIndex, branch := range recursiveBranches {
		var branchFields []values.Field
		if branchType, err := ExactLogicalResultType(branch, t.md); err == nil {
			if record, ok := branchType.(*values.RecordType); ok {
				branchFields = record.Fields
				resultNullable = resultNullable || record.Nullable
			}
		}
		// A projection-less self-reference cannot be derived by the metadata-only
		// logical typer. The provisional cteColumnsScope registration gives the
		// translator's schema walker the exact seed slots without fabricating an
		// executable scan.
		if len(branchFields) == 0 {
			branchFields = t.derivedOutputColumns(branch)
		}
		if len(branchFields) != len(fields) {
			return nil, fmt.Errorf("recursive branch %d has width %d, want %d",
				branchIndex, len(branchFields), len(fields))
		}
		for ordinal := range fields {
			common := values.MaximumType(fields[ordinal].FieldType, branchFields[ordinal].FieldType)
			if common == nil {
				return nil, fmt.Errorf("recursive branch %d column %d types %s and %s are incompatible",
					branchIndex, ordinal+1, fields[ordinal].FieldType, branchFields[ordinal].FieldType)
			}
			fields[ordinal].FieldType = common
		}
	}
	row := &values.RecordType{Nullable: resultNullable, Fields: fields}
	if _, err := values.SnapshotExactType(row); err != nil {
		return nil, fmt.Errorf("recursive CTE common row is not exact: %w", err)
	}
	return row, nil
}

// recursiveCTEMainBindings returns the exact correlations under which scans of
// cteName are visible in the main query. A nested same-named CTE shadows the
// outer definition, so its complete subtree is excluded. Binding identifiers
// win over display aliases exactly as they do at quantifier construction.
func recursiveCTEMainBindings(
	op logical.LogicalOperator,
	cteName string,
	bindings map[values.CorrelationIdentifier]struct{},
) {
	if op == nil {
		return
	}
	switch current := op.(type) {
	case *logical.LogicalScan:
		if strings.EqualFold(current.Table, cteName) {
			binding := sourceBinding(current)
			if binding != "" {
				bindings[values.NamedCorrelationIdentifier(binding)] = struct{}{}
			}
		}
		return
	case *logical.LogicalCTE:
		if strings.EqualFold(current.Name, cteName) {
			return
		}
	}
	for _, child := range op.Children() {
		recursiveCTEMainBindings(child, cteName, bindings)
	}
	for _, attached := range logical.AttachedPlans(op) {
		recursiveCTEMainBindings(attached, cteName, bindings)
	}
}

// pushRecursiveCTEConsumerRows scopes the seed-declared/common-row pair to
// each main-query alias of one recursive CTE. The returned bindings are popped
// after main translation; stacks preserve an enclosing recursive definition.
func (t *cascadesTranslator) pushRecursiveCTEConsumerRows(
	main logical.LogicalOperator,
	cteName string,
	declaration *values.RecordType,
	common *values.RecordType,
) []values.CorrelationIdentifier {
	bindings := make(map[values.CorrelationIdentifier]struct{})
	recursiveCTEMainBindings(main, cteName, bindings)
	if len(bindings) == 0 {
		return nil
	}
	if t.recursiveCTEConsumerRows == nil {
		t.recursiveCTEConsumerRows = make(map[values.CorrelationIdentifier][]recursiveCTEConsumerRow)
	}
	result := make([]values.CorrelationIdentifier, 0, len(bindings))
	for binding := range bindings {
		t.recursiveCTEConsumerRows[binding] = append(
			t.recursiveCTEConsumerRows[binding],
			recursiveCTEConsumerRow{declaration: declaration, common: common},
		)
		result = append(result, binding)
	}
	return result
}

func (t *cascadesTranslator) popRecursiveCTEConsumerRows(bindings []values.CorrelationIdentifier) {
	for _, binding := range bindings {
		stack := t.recursiveCTEConsumerRows[binding]
		if len(stack) <= 1 {
			delete(t.recursiveCTEConsumerRows, binding)
			continue
		}
		t.recursiveCTEConsumerRows[binding] = stack[:len(stack)-1]
	}
}

// translateRecursiveCTEConsumerValue retargets one seed-declared logical Value
// onto the recursive fixed point's exact common row. Admission is intentionally
// narrower than a generic phase-root translation: the correlation and complete
// declaration row must match, the physical target must equal the recorded
// common row, every field is re-resolved by its full ordinal path, and the new
// leaf/whole-value types may only be the MaximumType widening of the old ones.
// Foreign windows, a reordered row, or an incompatible type remain outside the
// bridge rather than being inferred from a display name.
func translateRecursiveCTEConsumerValue(
	value values.Value,
	declaration values.QuantifiedObjectValue,
	target values.QuantifiedObjectValue,
) (values.Value, error) {
	if value == nil || declaration == nil || target == nil {
		return nil, fmt.Errorf("recursive CTE consumer bridge has a nil Value or root")
	}
	commonRoot := values.MaximumType(declaration.FlowedType(), target.FlowedType())
	if commonRoot == nil || !values.FlowedTypeEquals(target, commonRoot) {
		return nil, fmt.Errorf("recursive CTE declared row %s does not widen exactly to %s",
			declaration.FlowedType(), target.FlowedType())
	}

	var rewriteErr error
	rewritten := values.Replace(value, func(node values.Value) values.Value {
		if rewriteErr != nil {
			return node
		}
		if field, isField := values.AsFieldValue(node); isField {
			root, isRoot := values.AsQuantifiedObjectValue(field.ChildValue())
			if !isRoot || root.Correlation() != declaration.Correlation() ||
				!values.FlowedTypesEqual(root, declaration) {
				return node
			}
			resolved, err := values.ResolveFieldOrdinals(target, field.Path().Ordinals())
			if err != nil {
				rewriteErr = fmt.Errorf("recursive CTE field path %v does not resolve on the common row: %w",
					field.Path().Ordinals(), err)
				return node
			}
			commonLeaf := values.MaximumType(field.ResultType(), resolved.Type())
			if commonLeaf == nil || !commonLeaf.Equals(resolved.Type()) {
				rewriteErr = fmt.Errorf("recursive CTE field path %v changes incompatibly from %s to %s",
					field.Path().Ordinals(), field.ResultType(), resolved.Type())
				return node
			}
			return resolved
		}
		if root, isRoot := values.AsQuantifiedObjectValue(node); isRoot &&
			root.Correlation() == declaration.Correlation() &&
			values.FlowedTypesEqual(root, declaration) {
			return target
		}
		return node
	})
	if rewriteErr != nil {
		return nil, rewriteErr
	}
	if rewritten == value {
		return value, nil
	}
	if rewritten == nil || rewritten.Type() == nil {
		return nil, fmt.Errorf("recursive CTE consumer bridge produced no exact Value")
	}
	commonResult := values.MaximumType(value.Type(), rewritten.Type())
	if commonResult == nil || !commonResult.Equals(rewritten.Type()) {
		return nil, fmt.Errorf("recursive CTE consumer Value changes incompatibly from %s to %s",
			value.Type(), rewritten.Type())
	}
	return rewritten, nil
}

func (t *cascadesTranslator) normalizeRecursiveCTEConsumerValue(
	value values.Value,
) (values.Value, error) {
	if value == nil {
		return value, nil
	}
	result := value
	for binding, stack := range t.recursiveCTEConsumerRows {
		if len(stack) == 0 {
			continue
		}
		scope := stack[len(stack)-1]
		if scope.declaration == nil || scope.common == nil {
			continue
		}
		declaration, err := values.NewQuantifiedObjectValue(binding, scope.declaration)
		if err != nil {
			return nil, fmt.Errorf("recursive CTE consumer declaration is not exact: %w", err)
		}
		target, err := values.NewQuantifiedObjectValue(binding, scope.common)
		if err != nil {
			return nil, fmt.Errorf("recursive CTE consumer common row is not exact: %w", err)
		}
		result, err = translateRecursiveCTEConsumerValue(result, declaration, target)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
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

	// The recursive-CTE body ordinalizes where possible. An earlier blanket
	// `t.inInnerCluster = true` forced every body join name-model; lifting it
	// (surgically — this site only; inInnerCluster is a genuine enclosure flag
	// at the non-recursive unnest/existential/scalar-subquery sites, where a
	// broad lift breaks multi-source unnest) lets the gate's own arms classify
	// each body sub-join independently: a plain inner/outer body GATES and
	// takes the ordinal branch (buildOrdinalJoinResultValue → a positional row
	// aligned by position to the CTE output — Java's seed-typed frontier,
	// RecursiveUnionExpression.mergeValues); an unnest/existential body stays
	// name-model (per the gate's own classification) and keeps the dotted-split
	// normalization arm (which survives to convergence). normalizeLegToOutputColumns
	// detects the ordinalized body (any baked-ordinal projected value) and
	// re-emits POSITIONALLY; the temp table stays a positional QueryResult
	// buffer keyed by outCols in NAME-METADATA only, with a bare-outCols Datum
	// riding alongside for UNION-DISTINCT dedup and Main name resolution.

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
	// keeps OUTPUT names (the source-name reverse-map has been retired), so a
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
	// model / a loud OrdinalResolutionError under the ordinal model (a gap in
	// alias-list handling that predates the ordinal model). Derive the seed
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
		outCols = c.ColumnAliases // normalized once, at the parse capture
	}

	// Derive the exact positional row shared by every iteration BEFORE creating
	// the self-reference scan. SQL UNION compatibility is positional: names come
	// from the seed/output alias list, while each slot uses Type.maximumType over
	// the seed and recursive branches. In particular, `0 AS level` is NOT NULL
	// but `level + 1` is nullable; publishing the seed's narrower row on the temp
	// scan would make later iterations disagree with the recursive insert.
	seedResult := seedExpr.GetResultValue()
	if seedResult == nil || seedResult.Type() == nil {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"recursive CTE seed has no exact result type"))
		return nil
	}
	seedHandle, seedTypeErr := values.ExactTypeForValue(seedResult)
	if seedTypeErr != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"recursive CTE seed has no exact result type: %v", seedTypeErr))
		return nil
	}
	seedType, isSeedRecord := seedHandle.Type().(*values.RecordType)
	seedWidth := 0
	if isSeedRecord {
		seedWidth = len(seedType.Fields)
	}
	if !isSeedRecord || seedWidth != len(outCols) {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"recursive CTE seed width %d disagrees with %d output columns",
			seedWidth, len(outCols)))
		return nil
	}
	// A provisional column-only registration lets projection-less recursive
	// terms derive self-reference slots without publishing an executable scan.
	// It is immediately replaced by the common row below, before translation.
	seedFields := make([]values.Field, len(outCols))
	for i := range outCols {
		seedFields[i] = values.Field{
			Name:      outCols[i],
			FieldType: seedType.Fields[i].FieldType,
			Ordinal:   i,
		}
	}
	declaredRow := &values.RecordType{
		Nullable: seedType.Nullable,
		Fields:   append([]values.Field(nil), seedFields...),
	}
	t.cteColumnsScope[cteName] = seedFields
	commonRow, commonErr := t.recursiveCTECommonResultRow(seedType, recursiveBranches, outCols)
	if commonErr != nil {
		delete(t.cteColumnsScope, cteName)
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"recursive CTE branches have no exact common result row: %v", commonErr))
		return nil
	}
	tempFields := append([]values.Field(nil), commonRow.Fields...)
	t.cteColumnsScope[cteName] = tempFields

	// Normalize the seed onto that exact row. This is a no-op for the common
	// same-schema case, preserving its plan shape; a rename, promotion, or
	// nullability widening becomes an explicit ordinal projection.
	if !seedType.Equals(commonRow) {
		seedExpr = t.normalizeRecursiveLegToOutputRow(seedExpr, commonRow)
		if seedExpr == nil {
			delete(t.cteColumnsScope, cteName)
			return nil
		}
	}

	// Owning inserts (Java TempTableInsertExpression.ofCorrelated defaults
	// isOwningTempTable=true for CTE legs): the owning insert cursor snapshots
	// its table in its continuation, which lets the recursive union resume
	// mid-level.
	seedInsert, err := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(expressions.InitialOf(seedExpr)), insertAlias, true,
	)
	if err != nil {
		delete(t.cteColumnsScope, cteName)
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"recursive CTE seed insert has no exact flowed row: %v", err))
		return nil
	}

	// The self-reference temp table carries the COMMON CTE row, including any
	// nullability/type widening. Thus a recursive join leg anchors on the same
	// exact contract that both inserts publish (RFC-077 7.6).
	tempScan, err := expressions.NewTempTableScanExpression(scanAlias, commonRow)
	if err != nil {
		delete(t.cteColumnsScope, cteName)
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"recursive CTE temp scan has no exact row type: %v", err))
		return nil
	}
	t.cteExprScope[cteName] = tempScan
	t.cteColumnsScope[cteName] = tempFields
	var recursiveConsumerBindings []values.CorrelationIdentifier
	for _, branch := range recursiveBranches {
		recursiveConsumerBindings = append(recursiveConsumerBindings,
			t.pushRecursiveCTEConsumerRows(branch, cteName, declaredRow, commonRow)...)
	}
	var recursiveExpr expressions.RelationalExpression
	if len(recursiveBranches) == 1 {
		recursiveExpr = t.translateOp(recursiveBranches[0])
	} else {
		recursiveExpr = t.translateUnion(&logical.LogicalUnion{Inputs: recursiveBranches, Distinct: false})
	}
	t.popRecursiveCTEConsumerRows(recursiveConsumerBindings)
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
	// has been retired). recursiveRemapValues never persists a qualified key:
	// each read is FieldValue{Field: <bare>, Child: QOV(<qualifier>)} — it reads
	// the qualified datum key ("B.ID") while projectionColumnName returns the BARE
	// field, so the qualified key (which would collide with the next recursion
	// level's same-qualified join side and stall the recursion one level early) is
	// never copied in. executeProjection also emits the value under the bare body
	// column; when that differs from the OUTPUT name it is an INERT extra key
	// (unqualified, re-qualified under the scan alias at the next level).
	recCols := extractOuterProjectionColumns(recursiveBranches[0])
	if len(outCols) > 0 && len(recCols) > 0 && len(outCols) == len(recCols) {
		recursiveExpr = t.normalizeRecursiveLegToOutputRow(recursiveExpr, commonRow)
		if recursiveExpr == nil {
			return nil
		}
	} else if recursiveExpr.GetResultValue() == nil ||
		!recursiveExpr.GetResultValue().Type().Equals(commonRow) {
		// A projection-less recursive term can already expose the exact common
		// row. If it does not, it still needs the same typed positional bridge;
		// do not let the strict RecursiveUnion constructor be the first place the
		// mismatch becomes visible.
		recursiveExpr = t.normalizeRecursiveLegToOutputRow(recursiveExpr, commonRow)
		if recursiveExpr == nil {
			return nil
		}
	}

	// Wrap recursive leg in TempTableInsert.
	recursiveRef := expressions.InitialOf(recursiveExpr)
	recursiveInsert, err := expressions.NewTempTableInsertExpression(
		expressions.ForEachQuantifier(recursiveRef), insertAlias, true,
	)
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"recursive CTE recursive insert has no exact flowed row: %v", err))
		return nil
	}

	// Build RecursiveUnionExpression.
	seedInsertRef := expressions.InitialOf(seedInsert)
	recursiveInsertRef := expressions.InitialOf(recursiveInsert)
	strategy := expressions.TraversalAny
	switch c.TraversalOrder {
	case logical.TraversalLevelOrder:
		// An EXPLICIT level_order pins the level union (Java LEVEL gates
		// the DFS rule off); only the clause-less ANY leaves the choice
		// to the cost model.
		strategy = expressions.TraversalLevel
	case logical.TraversalPreOrder:
		strategy = expressions.TraversalPreorder
	case logical.TraversalPostOrder:
		strategy = expressions.TraversalPostorder
	}
	var recUnion *expressions.RecursiveUnionExpression
	if union.Distinct {
		recUnion, err = expressions.NewRecursiveUnionExpressionDistinct(
			expressions.ForEachQuantifier(seedInsertRef),
			expressions.ForEachQuantifier(recursiveInsertRef),
			scanAlias, insertAlias,
			strategy,
		)
	} else {
		recUnion, err = expressions.NewRecursiveUnionExpression(
			expressions.ForEachQuantifier(seedInsertRef),
			expressions.ForEachQuantifier(recursiveInsertRef),
			scanAlias, insertAlias,
			strategy,
		)
	}
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"recursive CTE branches have no exact common result row: %v", err))
		return nil
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
	t.cteColumnsScope[cteName] = tempFields
	consumerBindings := t.pushRecursiveCTEConsumerRows(c.Main, cteName, declaredRow, commonRow)
	result := t.translateOp(c.Main)
	t.popRecursiveCTEConsumerRows(consumerBindings)
	delete(t.cteExprScope, cteName)
	delete(t.cteColumnsScope, cteName)
	return result
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
			continue
		}
		// A COLUMN REFERENCE takes the DISPLAY name — the same one the result
		// set reports. Its projection TEXT is not that name: a nested read
		// writes `n.sk`, and keying the temp table by N.SK left every
		// reference to the CTE — which resolves the SQL column SK — reading a
		// column the row does not have (`edge lookup R: read as
		// RECORD(SK:LONG?), declared RECORD(N.SK:LONG?)`). The qualifier is an
		// internal slot key; it does not cross into a name a user writes.
		//
		// Only a reference. A COMPUTED slot's text is its own name here (a
		// caller may have put the output name in it), and rendering the value
		// instead would name `0 AS level` the column "0".
		if i < len(p.ProjectedValues) && p.ProjectedValues[i] != nil {
			if _, isReference := values.AsFieldValue(p.ProjectedValues[i]); isReference {
				out[i] = values.DisplayColumnName(p.ProjectedValues[i], "")
				continue
			}
		}
		out[i] = p.Projections[i]
	}
	return out
}

// normalizeLegToOutputColumns wraps a recursive-CTE leg in an exact ordinal
// projection and re-emits its slots under the CTE's output names. The leg's
// flowed record is the sole layout authority: every source read resolves by
// position, so dotted aliases and computed display strings are never parsed
// back into correlations. The wrapper also gives both recursive-union arms the
// same names without changing their positional result contract.
func (t *cascadesTranslator) normalizeLegToOutputColumns(
	leg expressions.RelationalExpression,
	outCols []string,
) expressions.RelationalExpression {
	q := expressions.ForEachQuantifier(expressions.InitialOf(leg))
	row, err := q.RequireFlowedObjectValue()
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"recursive CTE leg has no exact flowed row: %v", err))
		return nil
	}
	projected := make([]values.Value, len(outCols))
	for i := range outCols {
		projected[i], err = values.ResolveFieldOrdinals(row, []int{i})
		if err != nil {
			t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"recursive CTE output slot %d does not resolve: %v", i, err))
			return nil
		}
	}
	projection, err := expressions.NewLogicalProjectionExpressionWithAliases(
		projected, append([]string(nil), outCols...), q)
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"recursive CTE normalization has no exact result row: %v", err))
		return nil
	}
	return projection
}

// normalizeRecursiveLegToOutputRow re-emits one recursive-CTE leg under the
// exact common positional row derived for the recursion. Resolving by ordinal
// preserves source identity, while exactUnionSlotValue applies only the SQL
// implicit promotion/nullability widening already proven by MaximumType.
func (t *cascadesTranslator) normalizeRecursiveLegToOutputRow(
	leg expressions.RelationalExpression,
	outputRow *values.RecordType,
) expressions.RelationalExpression {
	if leg == nil || outputRow == nil {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"recursive CTE normalization has no exact source or output row"))
		return nil
	}
	q := expressions.ForEachQuantifier(expressions.InitialOf(leg))
	row, err := q.RequireFlowedObjectValue()
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"recursive CTE leg has no exact flowed row: %v", err))
		return nil
	}
	projected := make([]values.Value, len(outputRow.Fields))
	outputNames := make([]string, len(outputRow.Fields))
	for i, field := range outputRow.Fields {
		resolved, resolveErr := values.ResolveFieldOrdinals(row, []int{i})
		if resolveErr != nil {
			t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"recursive CTE output slot %d does not resolve: %v", i, resolveErr))
			return nil
		}
		projected[i], err = exactUnionSlotValue(resolved, field.FieldType)
		if err != nil {
			t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"recursive CTE output slot %d cannot adopt the common type: %v", i, err))
			return nil
		}
		outputNames[i] = field.Name
	}
	projection, err := expressions.NewLogicalProjectionExpressionWithOutputSchema(
		projected, nil, nil, outputNames, q)
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"recursive CTE normalization has no exact result row: %v", err))
		return nil
	}
	if !projection.GetResultValue().Type().Equals(outputRow) {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"recursive CTE normalization produced %v, want %v",
			projection.GetResultValue().Type(), outputRow))
		return nil
	}
	return projection
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
		explode, err := expressions.NewExplodeExpression(ins.ValuesArray)
		if err != nil {
			t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"INSERT VALUES has no exact element row type: %v", err))
			return nil
		}
		innerRef = expressions.InitialOf(explode)
	}
	if innerRef == nil {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"INSERT has no exact source row"))
		return nil
	}
	var q expressions.Quantifier
	q = expressions.ForEachQuantifier(innerRef)
	targetFields := t.tableColumns(ins.Table)
	if len(targetFields) == 0 {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUndefinedTable,
			"INSERT target %q has no exact catalog row type", ins.Table))
		return nil
	}
	insert, err := expressions.NewInsertExpression(q, ins.Table, &values.RecordType{Fields: targetFields})
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"INSERT has no exact result row: %v", err))
		return nil
	}
	return insert
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
		newVal := a.Value
		if newVal == nil {
			t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
				"UPDATE assignment %q has no resolved exact Value", a.Column))
			return nil
		}
		transforms[i] = expressions.UpdateTransform{
			FieldPath: strings.ToUpper(a.Column),
			NewValue:  newVal,
		}
	}
	if innerRef == nil {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"UPDATE has no exact input row"))
		return nil
	}
	q := expressions.ForEachQuantifier(innerRef)
	targetFields := t.tableColumns(upd.Target)
	if len(targetFields) == 0 {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUndefinedTable,
			"UPDATE target %q has no exact catalog row type", upd.Target))
		return nil
	}
	update, err := expressions.NewUpdateExpression(q, upd.Target, &values.RecordType{Fields: targetFields}, transforms)
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"UPDATE has no exact result row: %v", err))
		return nil
	}
	return update
}

func (t *cascadesTranslator) translateDelete(del *logical.LogicalDelete) expressions.RelationalExpression {
	var innerRef *expressions.Reference
	if del.Input != nil {
		innerRef = t.translateRef(del.Input)
		if innerRef == nil {
			return nil
		}
	}
	if innerRef == nil {
		t.setTranslateErr(api.NewError(api.ErrCodeUnsupportedQuery,
			"DELETE has no exact input row"))
		return nil
	}
	deleteExpr, err := expressions.NewDeleteExpression(expressions.ForEachQuantifier(innerRef), del.Target)
	if err != nil {
		t.setTranslateErr(api.NewErrorf(api.ErrCodeUnsupportedQuery,
			"DELETE has no exact result row: %v", err))
		return nil
	}
	return deleteExpr
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
