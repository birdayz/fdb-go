package plans

import (
	"fmt"
	"slices"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryProjectionPlan applies a projection (column selection /
// expression evaluation) over an inner plan's row stream. Mirrors
// Java's conceptual projection in RecordQueryFetchFromPartialRecordPlan
// / the MapPipelinedCursor mechanics. The seed models it as a distinct
// plan node for clarity.
type RecordQueryProjectionPlan struct {
	PlanExprBase
	projections []values.Value
	aliases     []string
	// outputNames is the exact logical output schema, frozen before the
	// projection program is alpha-rebased onto a physical child edge.
	outputNames []string
	// aliasMinted is parallel to aliases: true = the MACHINERY wrote that
	// output name (the duplicated-bare-leaf dedup pinning a leg-qualified datum
	// key so two same-named columns stay apart in the executor's row map),
	// false = the user's `AS`. The two are spelled alike, so the ResultSet
	// metadata site — the only consumer — cannot tell them apart from the
	// string; it reads this instead.
	//
	// EXCLUDED from structuralKey, exactly as values.FieldPath.FrontierPinned is
	// excluded from value identity: it records who named a slot, not what the
	// slot computes. Two projections differing only here are the same memo
	// member.
	aliasMinted []bool
	// aliasSources is the frozen structured source for each machinery-minted
	// alias. It is metadata provenance and remains unchanged when projections
	// are physically reanchored onto an exact `_current` output carrier.
	// Excluded from structuralKey together with aliasMinted.
	aliasSources []values.ProjectionAliasSource
	innerQ       expressions.Quantifier
	resultValue  values.Value
	// outputNameOverrides is the frozen SQL schema delta beyond the natural
	// Value/alias names. Generated correlation-derived names stay out of memo
	// identity; externally frozen positional keys remain in it.
	outputNameOverrides []string
	// distinctProofIndexName names the secondary UNIQUE index whose uniqueness
	// licensed eliding a DISTINCT above this projection. See
	// distinct_proof_stamp.go — unlike aliasMinted beside it, this IS folded
	// into structuralKey, because it records a proved fact the plan's
	// correctness rests on rather than who named a slot.
	distinctProofIndexName string
}

// GetDistinctProofIndexName implements DistinctProofStamped.
func (p *RecordQueryProjectionPlan) GetDistinctProofIndexName() string {
	return p.distinctProofIndexName
}

// WithDistinctProofIndexName implements DistinctProofStampable.
func (p *RecordQueryProjectionPlan) WithDistinctProofIndexName(indexName string) RecordQueryPlan {
	cp := *p
	cp.distinctProofIndexName = indexName
	return &cp
}

func NewRecordQueryProjectionPlan(projections []values.Value, inner RecordQueryPlan) (*RecordQueryProjectionPlan, error) {
	return newRecordQueryProjectionPlan(projections, nil, nil, nil, QuantifierOverPlan(inner))
}

func NewRecordQueryProjectionPlanWithAliases(projections []values.Value, aliases []string, inner RecordQueryPlan) (*RecordQueryProjectionPlan, error) {
	return newRecordQueryProjectionPlan(projections, aliases, nil, nil, QuantifierOverPlan(inner))
}

// NewRecordQueryProjectionPlanWithOutputSchema rebuilds a projection over a
// concrete child while preserving the output schema and alias provenance that
// were established before its Value program was rebased.
func NewRecordQueryProjectionPlanWithOutputSchema(
	projections []values.Value,
	aliases []string,
	aliasMinted []bool,
	outputNames []string,
	inner RecordQueryPlan,
) (*RecordQueryProjectionPlan, error) {
	return newRecordQueryProjectionPlan(
		projections, aliases, aliasMinted, outputNames, QuantifierOverPlan(inner))
}

// NewRecordQueryProjectionPlanFromQuantifier builds the projection directly over
// the LIVE inner memo edge the implement rule already memoized, rather than
// snapshotting a bare plan. The plan is then its own cascades expression
// carrying the child edge once — no wrapper storing a second copy (RFC-184 W2).
func NewRecordQueryProjectionPlanFromQuantifier(projections []values.Value, aliases []string, innerQ expressions.Quantifier) (*RecordQueryProjectionPlan, error) {
	return newRecordQueryProjectionPlan(projections, aliases, nil, nil, innerQ)
}

func (p *RecordQueryProjectionPlan) GetProjections() []values.Value {
	return slices.Clone(p.projections)
}
func (p *RecordQueryProjectionPlan) GetAliases() []string { return slices.Clone(p.aliases) }
func (p *RecordQueryProjectionPlan) GetOutputNames() []string {
	return slices.Clone(p.outputNames)
}

// GetAliasMinted returns the per-slot alias provenance, parallel to GetAliases:
// true = machinery-minted datum key, false (and every slot past the slice) =
// the user's `AS`.
func (p *RecordQueryProjectionPlan) GetAliasMinted() []bool { return slices.Clone(p.aliasMinted) }

// GetAliasSources returns a defensive copy of the frozen structured source
// identities for machinery-minted aliases. A short/nil vector is uncaptured,
// never permission to reconstruct a source from alias text.
func (p *RecordQueryProjectionPlan) GetAliasSources() []values.ProjectionAliasSource {
	return slices.Clone(p.aliasSources)
}

// NewRecordQueryProjectionPlanFromQuantifierWithProvenance is
// NewRecordQueryProjectionPlanFromQuantifier plus the per-slot record of who
// named each output. Every lowering of a logical projection goes through here
// so the provenance survives the logical→physical boundary; the plain
// constructor stays for machinery that has no aliases to explain.
func NewRecordQueryProjectionPlanFromQuantifierWithProvenance(projections []values.Value, aliases []string, aliasMinted []bool, innerQ expressions.Quantifier) (*RecordQueryProjectionPlan, error) {
	return newRecordQueryProjectionPlan(projections, aliases, aliasMinted, nil, innerQ)
}

// NewRecordQueryProjectionPlanFromQuantifierWithOutputSchema preserves the
// exact logical result schema while rebased Values address the selected
// physical edge. outputNames must be the logical projection's frozen,
// deduplicated names in slot order.
func NewRecordQueryProjectionPlanFromQuantifierWithOutputSchema(
	projections []values.Value,
	aliases []string,
	aliasMinted []bool,
	outputNames []string,
	innerQ expressions.Quantifier,
) (*RecordQueryProjectionPlan, error) {
	return newRecordQueryProjectionPlan(projections, aliases, aliasMinted, outputNames, innerQ)
}

func newRecordQueryProjectionPlan(
	projections []values.Value,
	aliases []string,
	aliasMinted []bool,
	outputNames []string,
	innerQ expressions.Quantifier,
) (*RecordQueryProjectionPlan, error) {
	if innerQ.GetRangesOver() == nil {
		return nil, fmt.Errorf("RecordQueryProjectionPlan requires an inner plan")
	}
	reanchored := make([]values.Value, len(projections))
	for i, projection := range projections {
		var err error
		reanchored[i], err = reanchorCurrentValueForInput(projection, innerQ)
		if err != nil {
			return nil, fmt.Errorf("RecordQueryProjectionPlan projection %d input carrier: %w", i, err)
		}
	}
	return newRecordQueryProjectionPlanFromBoundValues(
		reanchored, aliases, aliasMinted, outputNames, innerQ)
}

// newRecordQueryProjectionPlanFromBoundValues finishes a projection whose
// programs are already expressed at innerQ's exact output boundary. The only
// non-constructor caller is WithQuantifiers' selected-to-selected relink: it
// translates both the old declared edge and the pointer-exact old output
// carrier onto the new selected carrier before entering here. Re-running the
// child's materializer in that case would treat an already-produced current
// row as an input program and can rematch a duplicated leaf name to its other
// owner.
func newRecordQueryProjectionPlanFromBoundValues(
	projections []values.Value,
	aliases []string,
	aliasMinted []bool,
	outputNames []string,
	innerQ expressions.Quantifier,
) (*RecordQueryProjectionPlan, error) {
	resultValue, err := projectionResultValueForOutputSchema(projections, aliases, outputNames)
	if err != nil {
		return nil, fmt.Errorf("RecordQueryProjectionPlan result Value: %w", err)
	}
	outputNameOverrides, err := values.ProjectionOutputSchemaIdentityOverrides(projections, aliases, outputNames)
	if err != nil {
		return nil, fmt.Errorf("RecordQueryProjectionPlan output identity: %w", err)
	}
	base, err := newPlanExprBaseForValue("RecordQueryProjectionPlan", resultValue)
	if err != nil {
		return nil, err
	}
	return &RecordQueryProjectionPlan{
		PlanExprBase:        base,
		projections:         slices.Clone(projections),
		aliases:             slices.Clone(aliases),
		outputNames:         projectionRecordFieldNames(resultValue),
		aliasMinted:         slices.Clone(aliasMinted),
		innerQ:              innerQ,
		resultValue:         resultValue,
		outputNameOverrides: outputNameOverrides,
	}, nil
}

func projectionResultValueForOutputSchema(
	projections []values.Value,
	aliases []string,
	outputNames []string,
) (*values.RecordConstructorValue, error) {
	return values.ProjectionResultValueForOutputSchema(projections, aliases, outputNames)
}

func projectionRecordFieldNames(result *values.RecordConstructorValue) []string {
	if result == nil {
		return nil
	}
	names := make([]string, len(result.Fields))
	for i := range result.Fields {
		names[i] = result.Fields[i].Name
	}
	return names
}

// WithAliasProvenance returns a copy carrying the given per-slot alias
// provenance. It is the rebase/rebuild path's carry-across: a rewrite that
// hands back "the same projection, moved" must preserve who named each slot.
func (p *RecordQueryProjectionPlan) WithAliasProvenance(aliasMinted []bool) *RecordQueryProjectionPlan {
	cp := *p
	cp.aliasMinted = slices.Clone(aliasMinted)
	cp.aliasSources = slices.Clone(p.aliasSources)
	for i := range cp.aliasSources {
		if i >= len(cp.aliasMinted) || !cp.aliasMinted[i] {
			cp.aliasSources[i] = values.ProjectionAliasSource{}
		}
	}
	return &cp
}

// WithAliasSources returns a copy carrying checked structured alias-source
// provenance. The source is intentionally metadata-only and does not enter
// structural identity or hashing.
func (p *RecordQueryProjectionPlan) WithAliasSources(
	sources []values.ProjectionAliasSource,
) (*RecordQueryProjectionPlan, error) {
	if p == nil {
		return nil, fmt.Errorf("RecordQueryProjectionPlan alias sources: nil plan")
	}
	if err := values.ValidateProjectionAliasSources(sources, p.aliasMinted, len(p.projections)); err != nil {
		return nil, fmt.Errorf("RecordQueryProjectionPlan alias sources: %w", err)
	}
	cp := *p
	cp.aliasSources = slices.Clone(sources)
	return &cp, nil
}

func (p *RecordQueryProjectionPlan) GetInner() RecordQueryPlan { return planFromQuantifier(p.innerQ) }

// GetInnerQuantifier returns the live child edge — the memo quantifier the
// projection ranges over (RFC-184 W2).
func (p *RecordQueryProjectionPlan) GetInnerQuantifier() expressions.Quantifier { return p.innerQ }

// GetQuantifiers reports the real child quantifier, overriding
// PlanExprBase's none.
func (p *RecordQueryProjectionPlan) GetQuantifiers() []expressions.Quantifier {
	if p.innerQ.GetRangesOver() == nil {
		return nil
	}
	return []expressions.Quantifier{p.innerQ}
}

// IsIdentity returns true if this projection passes all columns through
// unchanged: it has no schema-changing output alias and its sole
// QuantifiedObjectValue references this projection's inner quantifier. An
// identity projection can be removed without changing either rows or schema.
func (p *RecordQueryProjectionPlan) IsIdentity() bool {
	if len(p.projections) != 1 {
		return false
	}
	// nil, an empty slice, and one explicit empty placeholder all mean "derive
	// the output name". More entries are malformed for a one-slot projection;
	// decline removal rather than guessing which schema was intended.
	if len(p.aliases) > 1 || len(p.aliases) == 1 && p.aliases[0] != "" {
		return false
	}
	qov, ok := values.AsQuantifiedObjectValue(p.projections[0])
	return ok && qov.FlowedType() != nil && qov.FlowedType().Code() == values.TypeCodeRecord &&
		qov.Correlation() == p.innerQ.GetAlias()
}

// GetResultValue states the row this projection PRODUCES — the same derivation
// its logical twin uses (values.ProjectionResultValue), so the two cannot drift.
//
// It must be stated here rather than inherited: PlanExprBase.GetResultValue
// mints a QOV over a FRESH unique correlation, which no consumer can resolve
// against this plan's columns.
//
// This is also what the executor emits. executeProjection builds a
// PositionalRow with exactly one slot per projection, named by
// values.OutputColumnName — the authority ProjectionResultValue also uses — so
// the row stated here and the row emitted there are the same row.
func (p *RecordQueryProjectionPlan) GetResultValue() values.Value {
	return p.resultValue
}

// GetResultType derives from the result value, which is Java's arrangement:
// RelationalExpression.getResultType() is `Type.Relation(getResultValue()
// .getResultType())` and NO Java expression or plan overrides it
// (RelationalExpression.java:194-197). It was a hardcoded UnknownType, which
// made every consumer re-derive the row by name — the supply side RFC-226
// removes.
func (p *RecordQueryProjectionPlan) GetResultType() values.Type {
	return p.GetResultValue().Type()
}

func (p *RecordQueryProjectionPlan) GetChildren() []RecordQueryPlan {
	inner := p.GetInner()
	if inner == nil {
		return nil
	}
	return []RecordQueryPlan{inner}
}

// structuralKey lists the fields that distinguish this projection in the memo:
// the projection list and output aliases, compared by semantic identity
// (RFC-176 P2 — see
// semanticValueEquals): Java's model (RecordQueryMapPlan.equalsWithoutChildren
// → semanticEqualsForResults), where every semantic discriminator a projected
// Value carries — in particular a plan-time-resolved ordinal accessor
// (values.NewFieldValueWithResolvedOrdinal, the recursive-CTE duplicate-alias
// wrap; Java: distinct ofOrdinalNumber ordinals are distinct FieldPaths) —
// joins identity structurally. Two reads of duplicate-named slots differ ONLY
// by ordinal, so unifying them would let extraction pick a plan reading the
// WRONG slot. Children are excluded; the same key drives both
// EqualsPlanWithoutChildren and HashCodeWithoutChildren.
//
// NOTE(explain format, RFC-176 P3): identity was previously keyed on the
// ExplainValue renderings, which therefore had to be injective over every
// semantic discriminator — the origin of the '#'-escape (raw '#' doubled,
// ordinal reads rendered "X#0"; PR #446 rounds 2-3). After P2 no identity
// code path reads a rendering; rendering is for humans, identity is
// structural. The escape is RETAINED as an explain-format guarantee —
// debugging output that collapses two different reads is still a bug — and
// its tests (TestFieldValue_ExplainOrdinalEscape, the plans-level
// TestProjectionPlan_Identity_OrdinalVsLiteralHashField) now pin exactly
// that, plus the matching injective discriminator in writeSemanticHash's
// FieldValue arm.
func (p *RecordQueryProjectionPlan) structuralKey() *structuralKey {
	return newStructuralKey().
		Values(p.projections).
		Strs(projectionOutputIdentityKeys(p.projections, p.aliases)).
		Strs(p.outputNameOverrides).
		Str(p.distinctProofIndexName)
}

// TieBreakHashCodeWithoutChildren returns the projection's schema-neutral
// historical structural hash for deterministic candidate ranking. Memo
// equality/hashing remains schema-aware through structuralKey.
func (p *RecordQueryProjectionPlan) TieBreakHashCodeWithoutChildren() uint64 {
	return newStructuralKey().Values(p.projections).Hash("projplan|")
}

// projectionOutputIdentityKeys returns one output-schema discriminator per
// slot. The values package owns the exact normalization so logical and physical
// projection identity cannot drift.
func projectionOutputIdentityKeys(projections []values.Value, aliases []string) []string {
	keys := make([]string, len(projections))
	for i, projection := range projections {
		alias := ""
		if i < len(aliases) {
			alias = aliases[i]
		}
		keys[i] = values.ProjectionOutputIdentityKey(projection, alias)
	}
	return keys
}

func (p *RecordQueryProjectionPlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryProjectionPlan)
	return ok && p.structuralKey().Equal(o.structuralKey())
}

func (p *RecordQueryProjectionPlan) HashCodeWithoutChildren() uint64 {
	if hash, ok := p.cachedStructuralHash(p); ok {
		return hash
	}
	hash := p.structuralKey().Hash("projplan|")
	p.storeStructuralHash(p, hash)
	return hash
}

func (p *RecordQueryProjectionPlan) Explain() string {
	var b strings.Builder
	b.WriteString("Project([")
	explained := values.ExplainPlanValues(p.projections)
	for i, rendered := range explained {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(rendered)
	}
	b.WriteString("], ")
	if inner := p.GetInner(); inner != nil {
		b.WriteString(inner.Explain())
	} else {
		b.WriteString("<nil>")
	}
	b.WriteByte(')')
	b.WriteString(explainDistinctProofSuffix(p.distinctProofIndexName))
	return b.String()
}

var (
	_ DistinctProofStampable           = (*RecordQueryProjectionPlan)(nil)
	_ RecordQueryPlan                  = (*RecordQueryProjectionPlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryProjectionPlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryProjectionPlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns a copy ranging over the given child quantifier —
// Java's copy-on-write withChild(Reference).
func (p *RecordQueryProjectionPlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("RecordQueryProjectionPlan", len(qs), 1); err != nil {
		return nil, err
	}
	rebased := slices.Clone(p.projections)
	oldInput, err := p.innerQ.RequireFlowedObjectValue()
	if err != nil {
		return nil, fmt.Errorf("RecordQueryProjectionPlan.WithQuantifiers old input: %w", err)
	}
	newInput, err := qs[0].RequireFlowedObjectValue()
	if err != nil {
		return nil, fmt.Errorf("RecordQueryProjectionPlan.WithQuantifiers new input: %w", err)
	}
	if !oldInput.FlowedType().Equals(newInput.FlowedType()) {
		return nil, fmt.Errorf(
			"RecordQueryProjectionPlan.WithQuantifiers input type changed from %s to %s",
			oldInput.FlowedType(), newInput.FlowedType())
	}
	oldLayout, oldSelected, err := selectedInputOrdinalLayout(p.innerQ)
	if err != nil {
		return nil, fmt.Errorf("RecordQueryProjectionPlan.WithQuantifiers old layout: %w", err)
	}
	newLayout, newSelected, err := selectedInputOrdinalLayout(qs[0])
	if err != nil {
		return nil, fmt.Errorf("RecordQueryProjectionPlan.WithQuantifiers new layout: %w", err)
	}
	exactSelectedRelink := oldSelected && newSelected
	for i, projection := range rebased {
		target := newInput
		if exactSelectedRelink {
			// A selected physical projection program can be expressed either at
			// its declared quantifier edge or directly on the selected child's
			// exact output carrier. Move both representations to the new selected
			// carrier. TranslatePhaseRoot is pointer-exact, so an independently
			// minted same-shaped current row remains foreign.
			target = newLayout.Carrier()
		}
		rebased[i], err = values.TranslateDeclaredEdgeRoot(projection, oldInput, target)
		if err != nil {
			return nil, fmt.Errorf("RecordQueryProjectionPlan.WithQuantifiers slot %d: %w", i, err)
		}
		if exactSelectedRelink {
			rebased[i], err = values.TranslatePhaseRoot(
				rebased[i], oldLayout.Carrier(), newLayout.Carrier())
			if err != nil {
				return nil, fmt.Errorf(
					"RecordQueryProjectionPlan.WithQuantifiers slot %d output phase: %w", i, err)
			}
			// A projection may have been constructed on a live memo edge before
			// the child alternative was selected. Its retained source program is
			// then neither the declared whole-row edge nor the prior selected
			// carrier (for example EL._0 over a FlatMap that materializes EL).
			// Give the newly selected child one checked chance to prove that
			// lineage. Values already moved to newLayout.Carrier are pointer-exact
			// output programs and the materializer leaves them unchanged.
			if !valueReferencesExactQOV(rebased[i], newLayout.Carrier()) {
				if materializer, ok := childValueMaterializer(selectedPlanFromQuantifier(qs[0])); ok {
					rebased[i], err = materializer.reanchorInputValueToOutput(rebased[i])
				}
				if err != nil {
					return nil, fmt.Errorf(
						"RecordQueryProjectionPlan.WithQuantifiers slot %d selected producer: %w", i, err)
				}
			}
		}
		if rebased[i] == nil {
			return nil, fmt.Errorf("RecordQueryProjectionPlan.WithQuantifiers slot %d: checked edge translation returned nil", i)
		}
	}

	var rebuilt *RecordQueryProjectionPlan
	if exactSelectedRelink {
		// The two checked translations above already placed every child-owned
		// root at the replacement plan's OUTPUT boundary. Calling the ordinary
		// constructor would feed that output row back through a FlatMap's INPUT
		// lineage and double-normalize duplicated names.
		rebuilt, err = newRecordQueryProjectionPlanFromBoundValues(
			rebased, p.aliases, p.aliasMinted, p.outputNames, qs[0])
	} else {
		rebuilt, err = newRecordQueryProjectionPlan(
			rebased, p.aliases, p.aliasMinted, p.outputNames, qs[0])
	}
	if err != nil {
		return nil, err
	}
	rebuilt.outputNameOverrides = slices.Clone(p.outputNameOverrides)
	rebuilt.aliasSources = slices.Clone(p.aliasSources)
	rebuilt.distinctProofIndexName = p.distinctProofIndexName
	return rebuilt, nil
}

// WithChildren rebuilds over a fresh inner quantifier — the optional interface
// plan extraction uses to preserve the strict-singleton invariant. The fresh
// child edge has a fresh correlation, so WithQuantifiers also checked-rebases
// every retained projection onto that exact edge before rebuilding the result
// Value and admitted physical properties.
func (p *RecordQueryProjectionPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("RecordQueryProjectionPlan.WithChildren: expected 1 child, got %d", len(qs))
	}
	return p.WithQuantifiers(qs)
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryProjectionPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
