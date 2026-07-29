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
	innerQ      expressions.Quantifier
}

func NewRecordQueryProjectionPlan(projections []values.Value, inner RecordQueryPlan) *RecordQueryProjectionPlan {
	return &RecordQueryProjectionPlan{
		projections: slices.Clone(projections),
		innerQ:      QuantifierOverPlan(inner),
	}
}

func NewRecordQueryProjectionPlanWithAliases(projections []values.Value, aliases []string, inner RecordQueryPlan) *RecordQueryProjectionPlan {
	return &RecordQueryProjectionPlan{
		projections: slices.Clone(projections),
		aliases:     slices.Clone(aliases),
		innerQ:      QuantifierOverPlan(inner),
	}
}

// NewRecordQueryProjectionPlanFromQuantifier builds the projection directly over
// the LIVE inner memo edge the implement rule already memoized, rather than
// snapshotting a bare plan. The plan is then its own cascades expression
// carrying the child edge once — no wrapper storing a second copy (RFC-184 W2).
func NewRecordQueryProjectionPlanFromQuantifier(projections []values.Value, aliases []string, innerQ expressions.Quantifier) *RecordQueryProjectionPlan {
	return &RecordQueryProjectionPlan{
		projections: slices.Clone(projections),
		aliases:     slices.Clone(aliases),
		innerQ:      innerQ,
	}
}

func (p *RecordQueryProjectionPlan) GetProjections() []values.Value {
	return slices.Clone(p.projections)
}
func (p *RecordQueryProjectionPlan) GetAliases() []string { return slices.Clone(p.aliases) }

// GetAliasMinted returns the per-slot alias provenance, parallel to GetAliases:
// true = machinery-minted datum key, false (and every slot past the slice) =
// the user's `AS`.
func (p *RecordQueryProjectionPlan) GetAliasMinted() []bool { return slices.Clone(p.aliasMinted) }

// NewRecordQueryProjectionPlanFromQuantifierWithProvenance is
// NewRecordQueryProjectionPlanFromQuantifier plus the per-slot record of who
// named each output. Every lowering of a logical projection goes through here
// so the provenance survives the logical→physical boundary; the plain
// constructor stays for machinery that has no aliases to explain.
func NewRecordQueryProjectionPlanFromQuantifierWithProvenance(projections []values.Value, aliases []string, aliasMinted []bool, innerQ expressions.Quantifier) *RecordQueryProjectionPlan {
	p := NewRecordQueryProjectionPlanFromQuantifier(projections, aliases, innerQ)
	p.aliasMinted = slices.Clone(aliasMinted)
	return p
}

// WithAliasProvenance returns a copy carrying the given per-slot alias
// provenance. It is the rebase/rebuild path's carry-across: a rewrite that
// hands back "the same projection, moved" must preserve who named each slot.
func (p *RecordQueryProjectionPlan) WithAliasProvenance(aliasMinted []bool) *RecordQueryProjectionPlan {
	cp := *p
	cp.aliasMinted = slices.Clone(aliasMinted)
	return &cp
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
	qov, ok := p.projections[0].(*values.QuantifiedObjectValue)
	return ok && qov.Correlation == p.innerQ.GetAlias()
}

func (p *RecordQueryProjectionPlan) GetResultType() values.Type { return values.UnknownType }

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
		Strs(projectionOutputIdentityKeys(p.projections, p.aliases))
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
	return p.structuralKey().Hash("projplan|")
}

func (p *RecordQueryProjectionPlan) Explain() string {
	var b strings.Builder
	b.WriteString("Project([")
	for i, v := range p.projections {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(values.ExplainValue(v))
	}
	b.WriteString("], ")
	if inner := p.GetInner(); inner != nil {
		b.WriteString(inner.Explain())
	} else {
		b.WriteString("<nil>")
	}
	b.WriteByte(')')
	return b.String()
}

var (
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
func (p *RecordQueryProjectionPlan) WithQuantifiers(qs []expressions.Quantifier) expressions.RelationalExpression {
	if len(qs) != 1 {
		return p
	}
	cp := *p
	cp.innerQ = qs[0]
	return &cp
}

// WithChildren rebuilds over a fresh inner quantifier — the optional interface
// plan extraction uses to preserve the strict-singleton invariant. Delegates to
// WithQuantifiers.
func (p *RecordQueryProjectionPlan) WithChildren(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if len(qs) != 1 {
		return nil, fmt.Errorf("RecordQueryProjectionPlan.WithChildren: expected 1 child, got %d", len(qs))
	}
	return p.WithQuantifiers(qs), nil
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryProjectionPlan) GetRecordQueryPlan() RecordQueryPlan { return p }
