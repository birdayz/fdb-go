package expressions

import (
	"encoding/binary"
	"hash/fnv"
	"slices"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// LogicalProjectionExpression represents the projection list of a SELECT.
// The set of Values it captures determines what columns flow upward;
// rows themselves are passed through from the inner Quantifier.
//
// Ports Java's
// `com.apple.foundationdb.record.query.plan.cascades.expressions.LogicalProjectionExpression`.
//
// Note: Java's GetResultValue returns the inner's flowed object value,
// NOT a RecordConstructor over the projection list. The projection list
// is captured separately and consumed by upstream rules (the projection
// is implemented physically by a RecordQueryMapPlan that pulls the
// projected values out of the inner). We preserve that contract here.
type LogicalProjectionExpression struct {
	projectedValues []values.Value
	aliases         []string
	// aliasMinted is parallel to aliases: true = the MACHINERY wrote that
	// output name, false (and every slot past the slice) = the user did, via
	// `AS`. Carried, not recovered: a machinery key and a user alias can be
	// spelled identically ("A.K"), so only provenance separates them, and the
	// result-set label site needs exactly that separation.
	//
	// EXCLUDED from identity/hash exactly as values.FieldPath.FrontierPinned is
	// — a marker of who NAMED the slot, not of what the slot IS. Two projections
	// differing only in it emit the same rows under the same output names and
	// must intern as one memo member.
	aliasMinted []bool
	inner       Quantifier
}

// NewLogicalProjectionExpression constructs a projection over `inner`
// emitting the given Value list. The list is copied defensively.
func NewLogicalProjectionExpression(projectedValues []values.Value, inner Quantifier) *LogicalProjectionExpression {
	copied := make([]values.Value, len(projectedValues))
	copy(copied, projectedValues)
	return &LogicalProjectionExpression{
		projectedValues: copied,
		inner:           inner,
	}
}

// NewLogicalProjectionExpressionWithAliases includes output column aliases.
func NewLogicalProjectionExpressionWithAliases(projectedValues []values.Value, aliases []string, inner Quantifier) *LogicalProjectionExpression {
	copied := make([]values.Value, len(projectedValues))
	copy(copied, projectedValues)
	return &LogicalProjectionExpression{
		projectedValues: copied,
		aliases:         slices.Clone(aliases),
		inner:           inner,
	}
}

// GetProjectedValues returns a defensive copy of the projection list.
func (e *LogicalProjectionExpression) GetProjectedValues() []values.Value {
	return slices.Clone(e.projectedValues)
}

// NewLogicalProjectionExpressionWithAliasProvenance is
// NewLogicalProjectionExpressionWithAliases plus the per-slot record of WHO
// named each output — the machinery or the user's `AS`. Every rewrite that
// hands an existing projection's aliases to a new expression must come through
// here, or the provenance is dropped and a machinery key starts reading as a
// user label.
func NewLogicalProjectionExpressionWithAliasProvenance(projectedValues []values.Value, aliases []string, aliasMinted []bool, inner Quantifier) *LogicalProjectionExpression {
	e := NewLogicalProjectionExpressionWithAliases(projectedValues, aliases, inner)
	e.aliasMinted = slices.Clone(aliasMinted)
	return e
}

// GetAliases returns a defensive copy of the output column aliases.
func (e *LogicalProjectionExpression) GetAliases() []string {
	return slices.Clone(e.aliases)
}

// GetAliasMinted returns a defensive copy of the per-slot alias provenance,
// parallel to GetAliases. A nil or short result reads as "the user named it"
// for every slot it does not cover.
func (e *LogicalProjectionExpression) GetAliasMinted() []bool {
	return slices.Clone(e.aliasMinted)
}

// GetInner returns the inner Quantifier.
func (e *LogicalProjectionExpression) GetInner() Quantifier { return e.inner }

// GetResultValue passes the inner's flowed object value through —
// matches Java's contract.
func (e *LogicalProjectionExpression) GetResultValue() values.Value {
	return e.inner.GetFlowedObjectValue()
}

// GetQuantifiers returns the single inner Quantifier.
func (e *LogicalProjectionExpression) GetQuantifiers() []Quantifier {
	return []Quantifier{e.inner}
}

// CanCorrelate is always false for a projection — single child.
func (e *LogicalProjectionExpression) CanCorrelate() bool { return false }

// ChildrenAsSet is false — single child.
func (e *LogicalProjectionExpression) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren is the union of correlation sets
// across the projection list.
func (e *LogicalProjectionExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	for _, v := range e.projectedValues {
		for k := range values.GetCorrelatedToOfValue(v) {
			out[k] = struct{}{}
		}
	}
	return out
}

// EqualsWithoutChildren compares two projections by projection-list and
// output-schema equality. values.ProjectionOutputIdentityKey folds the
// executor-visible name where it carries identity beyond the Value itself:
// explicit aliases and unaliased FieldValue display names at any depth.
func (e *LogicalProjectionExpression) EqualsWithoutChildren(other RelationalExpression, aliases *AliasMap) bool {
	o, ok := other.(*LogicalProjectionExpression)
	if !ok {
		return false
	}
	if len(e.projectedValues) != len(o.projectedValues) {
		return false
	}
	// Alias-aware projected-Value equality (RFC-040 040.2). Inert under the
	// memo's empty-alias path until PR-A.
	vm := aliases.ToValuesAliasMap()
	for i := range e.projectedValues {
		if values.ProjectionOutputIdentityKey(e.projectedValues[i], projectionAliasAt(e.aliases, i)) !=
			values.ProjectionOutputIdentityKey(o.projectedValues[i], projectionAliasAt(o.aliases, i)) {
			return false
		}
		if !values.SemanticEqualsUnderAliasMap(e.projectedValues[i], o.projectedValues[i], vm) {
			return false
		}
	}
	return true
}

// HashCodeWithoutChildren hashes the projection list and each slot's additional
// schema-name discriminator. This keeps semantically-equal baked FieldValues
// with different display names in distinct schema buckets without folding
// internal correlation aliases into the otherwise alias-invariant hash.
func (e *LogicalProjectionExpression) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	var buf [8]byte
	for i, v := range e.projectedValues {
		binary.LittleEndian.PutUint64(buf[:], values.SemanticHashCode(v))
		h.Write(buf[:])
		outputIdentity := values.ProjectionOutputIdentityKey(v, projectionAliasAt(e.aliases, i))
		binary.LittleEndian.PutUint64(buf[:], uint64(len(outputIdentity)))
		h.Write(buf[:])
		h.Write([]byte(outputIdentity))
	}
	return h.Sum64()
}

// TieBreakHashCodeWithoutChildren returns the schema-neutral projection hash
// used only for deterministic candidate ranking. Output names belong in memo
// identity, but letting them perturb a cost tie-break changes the chosen
// operator placement for otherwise identical work. This is the exact
// pre-schema-identity projection hash: semantic projected Values plus their
// ordered slot boundaries.
func (e *LogicalProjectionExpression) TieBreakHashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	var buf [8]byte
	for _, v := range e.projectedValues {
		binary.LittleEndian.PutUint64(buf[:], values.SemanticHashCode(v))
		h.Write(buf[:])
		h.Write([]byte{0})
	}
	return h.Sum64()
}

func projectionAliasAt(aliases []string, ordinal int) string {
	if ordinal < len(aliases) {
		return aliases[ordinal]
	}
	return ""
}

// WithQuantifiers rewires the child edge and changes nothing else, so it copies
// the whole struct rather than re-listing fields: a field-by-field literal
// silently drops anything added later, and this runs on EVERY memo child
// rewrite — the one place a dropped alias provenance would be invisible.
func (e *LogicalProjectionExpression) WithQuantifiers(quantifiers []Quantifier) RelationalExpression {
	cp := *e
	cp.inner = quantifiers[0]
	return &cp
}

var _ RelationalExpression = (*LogicalProjectionExpression)(nil)
