package expressions

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"slices"
	"sort"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// UpdateTransform is one field-update of an UPDATE statement: the target
// field, resolved, and a replacement Value to evaluate against the row being
// updated. Java's transformation map is keyed by a FieldValue.FieldPath of
// ResolvedAccessors, and RecordQueryUpdatePlan keys its trie by their ordinals
// (MessageHelpers.transformMessage addresses each field by it). FieldOrdinals
// is that path: a column's position in the target, then, for a field of a
// struct column, each field's position in the struct before it. FieldNames
// are the fields' names along it, which the plan and the executor check
// against the ordinals and errors name.
type UpdateTransform struct {
	FieldOrdinals []int
	FieldNames    []string
	NewValue      values.Value
}

// CloneUpdateTransforms copies transforms and each one's path, so a caller's
// later change to its slices reaches no expression or plan built from them.
func CloneUpdateTransforms(transforms []UpdateTransform) []UpdateTransform {
	out := make([]UpdateTransform, len(transforms))
	for i, t := range transforms {
		out[i] = UpdateTransform{FieldOrdinals: slices.Clone(t.FieldOrdinals), FieldNames: slices.Clone(t.FieldNames), NewValue: t.NewValue}
	}
	return out
}

// FieldPath is the transform's field path for display, its names joined by '.'.
func (t UpdateTransform) FieldPath() string { return strings.Join(t.FieldNames, ".") }

// updateOrdinalsLess is Java's FieldValue.FieldPath.comparator: the ordinal
// paths compared lexicographically.
func updateOrdinalsLess(a, b []int) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// UpdateOrdinalsPrefix reports whether path a is a prefix of path b (or
// equal to it), FieldValue.FieldPath.isPrefixOf over the ordinals.
func UpdateOrdinalsPrefix(a, b []int) bool {
	if len(a) > len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// UpdateTransformAmbiguousError is Java's SemanticException
// UPDATE_TRANSFORM_AMBIGUOUS: one transform's field path is a prefix of
// another's, so which value the shared field takes is ambiguous
// (RecordQueryUpdatePlan.checkAndPrepareOrderedFieldPaths).
type UpdateTransformAmbiguousError struct {
	Prefix, Path string
}

func (e *UpdateTransformAmbiguousError) Error() string {
	return fmt.Sprintf("the transformations used in an UPDATE statement are ambiguous: %s is a prefix of %s", e.Prefix, e.Path)
}

// UpdateExpression represents UPDATE <recordType> SET col=expr WHERE ...
// Carries target record type + inner Quantifier producing the rows to
// update + the SET-list transforms.
//
// Ports the structural surface of Java's
// `com.apple.foundationdb.record.query.plan.cascades.expressions.UpdateExpression`.
// Java's full implementation includes a Type.Record `targetType` and
// a `transformations` map keyed by FieldPath. Go keeps a list of
// transforms, each a resolved field path (Java's FieldPath of
// ResolvedAccessors), ordered by ordinals as Java orders the map's paths.
type UpdateExpression struct {
	inner            Quantifier
	targetRecordType string
	// targetAlias is the correlation the SET values read the target row through,
	// carried apart from targetRecordType (the stored record-type name, the
	// structural identity), RFC-238 §7c. The SQL translator scopes the SET list
	// under the table's SQL name, which differs from the stored name for an
	// escaped table (`MY$TABLE` stored as `MY__1TABLE`). The zero value reads the
	// target through NamedCorrelationIdentifier(targetRecordType).
	targetAlias values.CorrelationIdentifier
	targetType  values.ExactTypeHandle
	transforms  []UpdateTransform // canonicalised: sorted by FieldOrdinals
	resultValue values.Value
}

// NewUpdateExpression builds an UPDATE. The transforms slice is
// copied AND sorted by the field paths' ordinals (canonicalisation — two UPDATEs
// with the same SET-list in different SQL textual order should be
// EqualsWithoutChildren-equal).
func NewUpdateExpression(inner Quantifier, targetRecordType string, targetType values.Type, transforms []UpdateTransform) (*UpdateExpression, error) {
	oldValue, err := requireFlowedResult("UpdateExpression OLD", inner)
	if err != nil {
		return nil, err
	}
	if _, ok := targetType.(*values.RecordType); !ok {
		return nil, fmt.Errorf("UpdateExpression target type: expected a record, got %v", targetType)
	}
	exactTarget, err := snapshotExpressionResultType("UpdateExpression target", targetType)
	if err != nil {
		return nil, err
	}
	resultType := &values.RecordType{Fields: []values.Field{
		{Name: "old", Ordinal: 0, FieldType: oldValue.FlowedType()},
		{Name: "new", Ordinal: 1, FieldType: values.WithNullability(exactTarget.Type(), true)},
	}}
	exactResult, err := snapshotExpressionResultType("UpdateExpression", resultType)
	if err != nil {
		return nil, err
	}
	copied := CloneUpdateTransforms(transforms)
	// Java orders the transformation map's paths by their ordinals
	// (FieldValue.FieldPath.comparator).
	sort.SliceStable(copied, func(i, j int) bool { return updateOrdinalsLess(copied[i].FieldOrdinals, copied[j].FieldOrdinals) })
	return &UpdateExpression{
		inner:            inner,
		targetRecordType: targetRecordType,
		targetType:       exactTarget,
		transforms:       copied,
		resultValue:      values.NewQueriedValue(nil, exactResult.Type()),
	}, nil
}

// GetInner returns the inner Quantifier.
func (e *UpdateExpression) GetInner() Quantifier { return e.inner }

// GetTargetRecordType returns the target record-type name.
func (e *UpdateExpression) GetTargetRecordType() string { return e.targetRecordType }

// WithTargetAlias returns a copy whose SET values read the target row through
// alias (see the targetAlias field).
func (e *UpdateExpression) WithTargetAlias(alias values.CorrelationIdentifier) *UpdateExpression {
	cp := *e
	cp.targetAlias = alias
	return &cp
}

// GetTargetAlias is the correlation the SET values read the target row through.
func (e *UpdateExpression) GetTargetAlias() values.CorrelationIdentifier {
	if e.targetAlias.IsZero() {
		return values.NamedCorrelationIdentifier(e.targetRecordType)
	}
	return e.targetAlias
}

// GetTargetType returns a defensive copy of the exact target record type.
func (e *UpdateExpression) GetTargetType() values.Type { return e.targetType.Type() }

// GetTransforms returns the canonical transform list, sorted by the field
// paths' ordinals. Read-only.
func (e *UpdateExpression) GetTransforms() []UpdateTransform { return e.transforms }

// GetResultValue passes the inner's flowed object through with the TYPE
// DELIBERATELY STRIPPED, for the reason InsertExpression.GetResultValue states at
// length, and here the divergence is larger.
//
// Java's UpdateExpression.java:84 is
// `new QueriedValue(computeResultType(inner.getFlowedObjectType(), targetType))`,
// and computeResultType (:209-213) builds a TWO-FIELD record — `OLD` carrying the
// inner's row and `NEW` carrying the target's. An UPDATE flows the before/after
// pair, which is what makes `UPDATE … RETURNING "old".x, "new".x` expressible.
// Go returns the inner's row: not a differently-shaped version of the same claim,
// a different row with a different column count.
//
// Untyped that was inert. Typed it asserts an N-column source row where the
// operator produces a 2-column old/new pair, so a reader that believes it reads
// every slot at the wrong depth. Stating no type is the honest interim.
//
// The real fix is the OLD/NEW record, and it needs the target type this expression
// does not yet carry (Go's UpdateExpression takes only the target record's NAME).
// That is work, not a wall — the name resolves to a type through the schema the
// planner already consults — but it changes what an UPDATE's result value IS and
// every consumer of it moves with it. Booked in TODO.md beside the INSERT half.
func (e *UpdateExpression) GetResultValue() values.Value {
	return e.resultValue
}

// GetQuantifiers returns the single inner Quantifier.
func (e *UpdateExpression) GetQuantifiers() []Quantifier {
	return []Quantifier{e.inner}
}

// CanCorrelate is false.
func (e *UpdateExpression) CanCorrelate() bool { return false }

// ChildrenAsSet is false.
func (e *UpdateExpression) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the union of correlation
// sets across the SET-list NewValue trees.
func (e *UpdateExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	for _, tx := range e.transforms {
		for k := range values.GetCorrelatedToOfValue(tx.NewValue) {
			out[k] = struct{}{}
		}
	}
	return out
}

// EqualsWithoutChildren compares targetRecordType + canonical
// transform list (FieldPath equality + replacement Value Explain
// equality).
func (e *UpdateExpression) EqualsWithoutChildren(other RelationalExpression, aliases *AliasMap) bool {
	o, ok := other.(*UpdateExpression)
	if !ok {
		return false
	}
	if e.targetRecordType != o.targetRecordType {
		return false
	}
	if !values.ExactTypesEqual(e.targetType, o.targetType) {
		return false
	}
	if len(e.transforms) != len(o.transforms) {
		return false
	}
	// Alias-aware SET-value equality (RFC-040 040.2). A field path compares
	// by its ordinals, as Java's ResolvedAccessor equality does; it is
	// alias-free. Inert under the memo's empty-alias path until PR-A.
	vm := aliases.ToValuesAliasMap()
	for i := range e.transforms {
		if !slices.Equal(e.transforms[i].FieldOrdinals, o.transforms[i].FieldOrdinals) {
			return false
		}
		if !values.SemanticEqualsUnderAliasMap(e.transforms[i].NewValue, o.transforms[i].NewValue, vm) {
			return false
		}
	}
	return true
}

// HashCodeWithoutChildren mixes a class-discriminating constant with
// the target record-type name and canonical transform list.
func (e *UpdateExpression) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("update|"))
	h.Write([]byte(e.targetRecordType))
	h.Write([]byte{0})
	var buf [8]byte
	for _, tx := range e.transforms {
		for _, o := range tx.FieldOrdinals {
			binary.LittleEndian.PutUint64(buf[:], uint64(o)) //nolint:gosec
			h.Write(buf[:])
		}
		h.Write([]byte{0x1})
		binary.LittleEndian.PutUint64(buf[:], values.SemanticHashCode(tx.NewValue))
		h.Write(buf[:])
		h.Write([]byte{0x2})
	}
	return h.Sum64()
}

func (e *UpdateExpression) WithQuantifiers(quantifiers []Quantifier) (RelationalExpression, error) {
	if err := requireQuantifierArity("UpdateExpression", len(quantifiers), 1); err != nil {
		return nil, err
	}
	rebuilt, err := NewUpdateExpression(quantifiers[0], e.targetRecordType, e.targetType.Type(), e.transforms)
	if err != nil {
		return nil, err
	}
	return rebuilt.WithTargetAlias(e.targetAlias), nil
}

var _ RelationalExpression = (*UpdateExpression)(nil)
