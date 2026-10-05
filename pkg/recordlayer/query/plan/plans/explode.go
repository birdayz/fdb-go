package plans

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RecordQueryExplodePlan "explodes" a collection-typed Value into a
// stream of element values. Leaf plan (no children). Mirrors Java's
// RecordQueryExplodePlan.
type RecordQueryExplodePlan struct {
	PlanExprBase
	collectionValue values.Value
	// resultType is the immutable constructor-time snapshot shared by the
	// result carrier and runtime row materializer. collectionValue remains an
	// ordinary Value graph and can be rebuilt/mutated by planning; rereading its
	// Type after admission would let execution emit a row outside the already
	// published exact output layout.
	resultType values.ExactTypeHandle
	// withOrdinality, when true, makes executePlan emit a 2-field record
	// (element, 1-based ordinal) per element instead of the bare element.
	// Mirrors Java's `RecordQueryExplodePlan.withOrdinality`.
	withOrdinality bool
	// zeroBasedOrdinality numbers the elements from 0 instead of 1. Valid only
	// with ordinality. Mirrors Java's `RecordQueryExplodePlan.zeroBasedOrdinality`
	// (proto field 3); no SQL form builds it.
	zeroBasedOrdinality bool
	// ordinalityNames name the two ordinality slots, as the logical Explode
	// it implements named them.
	ordinalityNames [2]string
	// resultValue is the stable per-instance QuantifiedObjectValue standing for
	// the rows this explode emits — minted once at construction, returned by
	// GetResultValue, EXCLUDED from Equals/Hash (its correlation id is unique per
	// instance). A bare leaf that stands as its own Cascades expression must
	// present a consistent row identity across repeated interrogations, the role
	// physicalExplodeWrapper's fresh-per-call GetResultValue could not (RFC-184
	// W2). nil for struct-literal test plans that bypass the constructor;
	// GetResultValue returns nil rather than inventing a row identity there.
	resultValue values.Value
}

// NewRecordQueryExplodePlan builds a bare (non-ordinal) Explode plan.
func NewRecordQueryExplodePlan(collectionValue values.Value) (*RecordQueryExplodePlan, error) {
	return newRecordQueryExplodePlan(collectionValue, false, false, [2]string{})
}

// NewRecordQueryExplodePlanWithOrdinalityBase is Java's three-argument
// `new RecordQueryExplodePlan(collectionValue, withOrdinality,
// zeroBasedOrdinality)`. Zero-based ordinals without ordinality are refused.
func NewRecordQueryExplodePlanWithOrdinalityBase(collectionValue values.Value, withOrdinality, zeroBasedOrdinality bool) (*RecordQueryExplodePlan, error) {
	if !withOrdinality {
		return newRecordQueryExplodePlan(collectionValue, false, zeroBasedOrdinality, [2]string{})
	}
	return newRecordQueryExplodePlan(collectionValue, true, zeroBasedOrdinality, [2]string{values.OrdinalFieldName(0), values.OrdinalFieldName(1)})
}

// NewRecordQueryExplodePlanWithOrdinality builds an Explode plan that
// also emits a 1-based ordinal alongside each element.
func NewRecordQueryExplodePlanWithOrdinality(collectionValue values.Value, withOrdinality bool) (*RecordQueryExplodePlan, error) {
	return NewRecordQueryExplodePlanWithOrdinalityBase(collectionValue, withOrdinality, false)
}

// NewRecordQueryExplodePlanWithOrdinalityNames builds a WITH ORDINALITY
// Explode plan whose slots carry the given names (see
// expressions.NewExplodeExpressionWithOrdinalityNames).
func NewRecordQueryExplodePlanWithOrdinalityNames(collectionValue values.Value, elementName, ordinalName string) (*RecordQueryExplodePlan, error) {
	if elementName == "" || ordinalName == "" || elementName == ordinalName {
		return nil, fmt.Errorf("RecordQueryExplodePlan: ordinality names must be two distinct names, got %q and %q", elementName, ordinalName)
	}
	return newRecordQueryExplodePlan(collectionValue, true, false, [2]string{elementName, ordinalName})
}

func newRecordQueryExplodePlan(collectionValue values.Value, withOrdinality, zeroBasedOrdinality bool, names [2]string) (*RecordQueryExplodePlan, error) {
	if collectionValue == nil {
		return nil, fmt.Errorf("RecordQueryExplodePlan: collection Value is nil")
	}
	if zeroBasedOrdinality && !withOrdinality {
		return nil, fmt.Errorf("RecordQueryExplodePlan: cannot base ordinals that are not produced")
	}
	arrayType, ok := collectionValue.Type().(*values.ArrayType)
	if !ok || arrayType == nil || arrayType.ElementType == nil {
		return nil, fmt.Errorf("RecordQueryExplodePlan: collection Value must have an exact ARRAY type")
	}
	resultType := arrayType.ElementType
	if withOrdinality {
		resultType = values.ExplodeOrdinalityResultTypeNamed(resultType, names[0], names[1])
	}
	base, err := newPlanExprBaseForType("RecordQueryExplodePlan", resultType)
	if err != nil {
		return nil, err
	}
	exactResult, err := values.SnapshotExactType(resultType)
	if err != nil {
		return nil, fmt.Errorf("RecordQueryExplodePlan result type: %w", err)
	}
	return &RecordQueryExplodePlan{
		PlanExprBase:        base,
		collectionValue:     collectionValue,
		withOrdinality:      withOrdinality,
		zeroBasedOrdinality: zeroBasedOrdinality,
		ordinalityNames:     names,
		resultValue:         base.resultValue,
		resultType:          exactResult,
	}, nil
}

func (p *RecordQueryExplodePlan) GetCollectionValue() values.Value { return p.collectionValue }

// GetOrdinalityNames returns the element and ordinal slot names of a WITH
// ORDINALITY plan (empty for the bare variant).
func (p *RecordQueryExplodePlan) GetOrdinalityNames() (string, string) {
	return p.ordinalityNames[0], p.ordinalityNames[1]
}

// WithCollection rebuilds this plan over another collection, keeping its
// ordinality and slot names.
func (p *RecordQueryExplodePlan) WithCollection(collectionValue values.Value) (*RecordQueryExplodePlan, error) {
	return newRecordQueryExplodePlan(collectionValue, p.withOrdinality, p.zeroBasedOrdinality, p.ordinalityNames)
}

// NewRecordQueryExplodePlanFor implements a logical Explode, keeping its
// ordinality and slot names.
func NewRecordQueryExplodePlanFor(explode *expressions.ExplodeExpression) (*RecordQueryExplodePlan, error) {
	element, ordinal := explode.GetOrdinalityNames()
	return newRecordQueryExplodePlan(explode.GetCollectionValue(), explode.GetWithOrdinality(),
		explode.GetZeroBasedOrdinality(), [2]string{element, ordinal})
}

// IsWithOrdinality reports whether the plan emits ordinals.
func (p *RecordQueryExplodePlan) IsWithOrdinality() bool { return p.withOrdinality }

// IsZeroBasedOrdinality reports whether the ordinals start at 0, not 1.
func (p *RecordQueryExplodePlan) IsZeroBasedOrdinality() bool { return p.zeroBasedOrdinality }

// FirstOrdinal is the ordinal of the first element: 0 when zero-based, else 1.
func (p *RecordQueryExplodePlan) FirstOrdinal() int {
	if p.zeroBasedOrdinality {
		return 0
	}
	return 1
}

// GetElementType returns the array element type, or UnknownType when the
// collection is not array-typed.
func (p *RecordQueryExplodePlan) GetElementType() values.Type {
	if p.resultType != nil {
		resultType := p.resultType.Type()
		if p.withOrdinality {
			if row, ok := resultType.(*values.RecordType); ok && len(row.Fields) > 0 {
				return row.Fields[0].FieldType
			}
			return values.UnknownType
		}
		return resultType
	}
	if p.collectionValue == nil {
		return values.UnknownType
	}
	if at, ok := p.collectionValue.Type().(*values.ArrayType); ok && at.ElementType != nil {
		return at.ElementType
	}
	return values.UnknownType
}

func (p *RecordQueryExplodePlan) GetResultType() values.Type {
	if p.resultType != nil {
		return p.resultType.Type()
	}
	elem := p.GetElementType()
	if p.withOrdinality {
		if p.ordinalityNames[0] != "" {
			return values.ExplodeOrdinalityResultTypeNamed(elem, p.ordinalityNames[0], p.ordinalityNames[1])
		}
		return values.ExplodeOrdinalityResultType(elem)
	}
	return elem
}

func (p *RecordQueryExplodePlan) GetChildren() []RecordQueryPlan { return nil }

// structuralKey folds the Explode identity: the collection Value by POINTER
// identity (ValuePtr — the hand-rolled equals used ==, NOT semantic equality)
// and the withOrdinality flag. Drives both Equals and Hash. The zero-based flag
// is a component only when set, so every existing key is unchanged (Java hashes
// the flag only when true, RecordQueryExplodePlan.java:318-319).
func (p *RecordQueryExplodePlan) structuralKey() *structuralKey {
	key := newStructuralKey().ValuePtr(p.collectionValue).Bool(p.withOrdinality).
		Str(p.ordinalityNames[0]).Str(p.ordinalityNames[1])
	if p.zeroBasedOrdinality {
		key = key.Str("zero-based")
	}
	return key
}

func (p *RecordQueryExplodePlan) EqualsPlanWithoutChildren(other RecordQueryPlan) bool {
	o, ok := other.(*RecordQueryExplodePlan)
	return ok && p.keyFor(p).Equal(o.keyFor(o))
}

func (p *RecordQueryExplodePlan) HashCodeWithoutChildren() uint64 {
	if hash, ok := p.cachedStructuralHash(p); ok {
		return hash
	}
	hash := p.keyFor(p).Hash("explodeplan|")
	p.storeStructuralHash(p, hash)
	return hash
}

func (p *RecordQueryExplodePlan) Explain() string {
	name := "<nil>"
	if p.collectionValue != nil {
		name = p.collectionValue.Name()
	}
	if p.withOrdinality {
		return fmt.Sprintf("Explode(%s WITH ORDINALITY)", name)
	}
	return fmt.Sprintf("Explode(%s)", name)
}

var (
	_ RecordQueryPlan                  = (*RecordQueryExplodePlan)(nil)
	_ expressions.RelationalExpression = (*RecordQueryExplodePlan)(nil)
)

// EqualsWithoutChildren is the RelationalExpression-shaped comparison; see
// planEqualsAsExpression.
func (p *RecordQueryExplodePlan) EqualsWithoutChildren(other expressions.RelationalExpression, _ *expressions.AliasMap) bool {
	return planEqualsAsExpression(p, other)
}

// WithQuantifiers returns this plan unchanged — it has no quantifiers to
// replace while children are raw pointers (RFC-183 P5 step 1).
func (p *RecordQueryExplodePlan) WithQuantifiers(qs []expressions.Quantifier) (expressions.RelationalExpression, error) {
	if err := validateQuantifierArity("RecordQueryExplodePlan", len(qs), 0); err != nil {
		return nil, err
	}
	return p, nil
}

// GetResultValue returns the explode's STABLE per-instance result value — the
// single correlation identity a bare explode carries as its own memo expression
// (RFC-184 W2). Struct-literal test plans that bypass the constructor return
// nil; only the checked constructor establishes an exact result identity.
func (p *RecordQueryExplodePlan) GetResultValue() values.Value {
	return p.resultValue
}

// GetCorrelatedToWithoutChildren reports the correlations of this plan's
// collection value, mirroring physicalExplodeWrapper.
func (p *RecordQueryExplodePlan) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	if v := p.GetCollectionValue(); v != nil {
		return values.GetCorrelatedToOfValue(v)
	}
	return map[values.CorrelationIdentifier]struct{}{}
}

// GetRecordQueryPlan returns the plan itself.
func (p *RecordQueryExplodePlan) GetRecordQueryPlan() RecordQueryPlan { return p }
