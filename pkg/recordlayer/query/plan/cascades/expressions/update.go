package expressions

import (
	"encoding/binary"
	"hash/fnv"
	"sort"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// UpdateTransform is one column-update of an UPDATE statement: a
// FieldPath identifying the target column (dot-separated for nested
// records — e.g. "header.priority") and a replacement Value to evaluate
// against the row being updated. Mirrors Java's `Update.TransformSpec`
// shape, kept to the path + replacement Value the executor consumes.
type UpdateTransform struct {
	FieldPath string
	NewValue  values.Value
}

// UpdateExpression represents UPDATE <recordType> SET col=expr WHERE ...
// Carries target record type + inner Quantifier producing the rows to
// update + the SET-list transforms.
//
// Ports the structural surface of Java's
// `com.apple.foundationdb.record.query.plan.cascades.expressions.UpdateExpression`.
// Java's full implementation includes a Type.Record `targetType` and
// a `transformations` map keyed by FieldPath. We keep the simpler
// list-of-transforms shape with a plain-string FieldPath (Java
// models it as a list of accessors, but the planner serialises to
// dot-separated form anyway).
type UpdateExpression struct {
	inner            Quantifier
	targetRecordType string
	transforms       []UpdateTransform // canonicalised: sorted by FieldPath
}

// NewUpdateExpression builds an UPDATE. The transforms slice is
// copied AND sorted by FieldPath (canonicalisation — two UPDATEs
// with the same SET-list in different SQL textual order should be
// EqualsWithoutChildren-equal).
func NewUpdateExpression(inner Quantifier, targetRecordType string, transforms []UpdateTransform) *UpdateExpression {
	copied := make([]UpdateTransform, len(transforms))
	copy(copied, transforms)
	sort.SliceStable(copied, func(i, j int) bool { return copied[i].FieldPath < copied[j].FieldPath })
	return &UpdateExpression{
		inner:            inner,
		targetRecordType: targetRecordType,
		transforms:       copied,
	}
}

// GetInner returns the inner Quantifier.
func (e *UpdateExpression) GetInner() Quantifier { return e.inner }

// GetTargetRecordType returns the target record-type name.
func (e *UpdateExpression) GetTargetRecordType() string { return e.targetRecordType }

// GetTransforms returns the canonical (sorted-by-FieldPath) transform
// list. Read-only.
func (e *UpdateExpression) GetTransforms() []UpdateTransform { return e.transforms }

// GetResultValue passes the inner's flowed object through with the TYPE
// DELIBERATELY STRIPPED, for the reason InsertExpression.GetResultValue states at
// length, and here the divergence is larger.
//
// Java's UpdateExpression.java:84 is
// `new QueriedValue(computeResultType(inner.getFlowedObjectType(), targetType))`,
// and computeResultType (:209-213) builds a TWO-FIELD record — `OLD` carrying the
// inner's row and `NEW` carrying the target's. An UPDATE flows the before/after
// pair, which is what makes `UPDATE … RETURNING "OLD"."X", "NEW"."X"` expressible.
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
	return values.NewQuantifiedObjectValue(e.inner.GetAlias())
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
	if len(e.transforms) != len(o.transforms) {
		return false
	}
	// Alias-aware SET-value equality (RFC-040 040.2). FieldPath is a string
	// path (alias-free). Inert under the memo's empty-alias path until PR-A.
	vm := aliases.ToValuesAliasMap()
	for i := range e.transforms {
		if e.transforms[i].FieldPath != o.transforms[i].FieldPath {
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
		h.Write([]byte(tx.FieldPath))
		h.Write([]byte{0x1})
		binary.LittleEndian.PutUint64(buf[:], values.SemanticHashCode(tx.NewValue))
		h.Write(buf[:])
		h.Write([]byte{0x2})
	}
	return h.Sum64()
}

func (e *UpdateExpression) WithQuantifiers(quantifiers []Quantifier) RelationalExpression {
	cp := *e
	cp.inner = quantifiers[0]
	return &cp
}

var _ RelationalExpression = (*UpdateExpression)(nil)
