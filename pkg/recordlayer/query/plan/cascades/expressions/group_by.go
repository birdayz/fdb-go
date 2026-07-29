package expressions

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// AggregateFunction identifies an aggregate computation.
type AggregateFunction int

const (
	AggCount AggregateFunction = iota
	AggSum
	AggMin
	AggMax
	AggAvg
)

func (f AggregateFunction) String() string {
	switch f {
	case AggCount:
		return "COUNT"
	case AggSum:
		return "SUM"
	case AggMin:
		return "MIN"
	case AggMax:
		return "MAX"
	case AggAvg:
		return "AVG"
	default:
		return "UNKNOWN"
	}
}

// AggregateSpec describes one aggregate column in a GroupBy.
type AggregateSpec struct {
	Function    AggregateFunction
	Operand     values.Value
	Alias       string
	OperandName string // canonical operand text for result-map keying (e.g. "PRICE*QTY")
	// OperandIntType is the operand's STATIC integer width at plan time, used by
	// the SUM/AVG accumulator to pick int32 vs int64 overflow semantics — Java's
	// NumericAggregationValue selects SUM_I (Math.addExact on int, int32 overflow,
	// result INT) vs SUM_L (int64 overflow, result LONG) from the operand's static
	// TypeCode. values.TypeCodeInt means the operand is a SQL INTEGER column
	// (proto TYPE_INT32) → int32 overflow; any other value (including the zero
	// TypeCodeUnknown) keeps the int64 (SUM_L) domain. This is a SEPARATE field
	// because Go's resolver widens every integer column reference to LONG
	// (the retired `TypeInt` alias WAS NullableLong), so Operand.Type() cannot carry the
	// INT32/INT64 distinction — the translator sources it from the proto-faithful
	// record type instead.
	OperandIntType values.TypeCode
}

// IsCountStar reports whether agg is a COUNT(*)-equivalent aggregate: COUNT with
// no operand (COUNT(*)), or COUNT of a CONSTANT operand (COUNT(1), COUNT(NULL),
// COUNT(TRUE)). A constant is identical for every row, so counting it counts
// every row — the same value a COUNT(*) aggregate index stores. This is the
// SINGLE SOURCE OF TRUTH for count-star classification (RFC-164 WS-3): the
// planner's aggregate-index candidate (which decides whether an aggregate
// matches a COUNT(*) index) and the executor's group cursors (which decide
// whether to emit the group's total row count vs a per-operand non-null count)
// MUST apply the SAME rule, or they drift — the "two copies" that produced the
// COUNT-COL class. It codifies the translator's documented normalization ("a
// constant operand folds into count-star", cascades_translator.go): the
// aggregate-index candidate and the SQL→logical normalization already treat any
// constant operand as count-star, so the executor uses this same rule rather
// than an outlier narrow "constant is SQL NULL only" test.
func IsCountStar(agg AggregateSpec) bool {
	if agg.Function != AggCount {
		return false
	}
	if agg.Operand == nil {
		return true
	}
	_, isConstant := agg.Operand.(*values.ConstantValue)
	return isConstant
}

// GroupByExpression groups input rows by groupingKeys and computes
// aggregates over each group. Ports Java's GroupByExpression at the
// structural level needed for the Cascades planner.
//
// Java's version uses rich Value types (RecordConstructorValue for
// grouping, AggregateValue for aggregates). Go simplifies:
// groupingKeys is a list of Values (typically FieldValues), aggregates
// is a list of function+operand pairs.
type GroupByExpression struct {
	groupingKeys []values.Value
	aggregates   []AggregateSpec
	inner        Quantifier
}

func NewGroupByExpression(
	groupingKeys []values.Value,
	aggregates []AggregateSpec,
	inner Quantifier,
) *GroupByExpression {
	return &GroupByExpression{
		groupingKeys: groupingKeys,
		aggregates:   aggregates,
		inner:        inner,
	}
}

// AggregateKeyColumnName is the canonical output-column name for one grouping
// key. THE single naming authority for a group key — the plan's OutputColumnNames,
// the executor's aggregateCursor, and the translator's ordinal baking all read
// the name from here so a baked ordinal and the emitted positional slot can never
// disagree.
func AggregateKeyColumnName(k values.Value) string {
	if fv, ok := k.(*values.FieldValue); ok {
		return strings.ToUpper(fv.Field)
	}
	return strings.ToUpper(values.ColumnNameValue(k))
}

// AggregateResultColumnName is the canonical output-column name for one aggregate
// (function + operand), IGNORING any alias: the SELECT list references this
// canonical text and the projection above applies the alias. THE single naming
// authority for an aggregate column (same rationale as AggregateKeyColumnName).
func AggregateResultColumnName(agg AggregateSpec) string {
	opName := "?"
	if agg.OperandName != "" {
		opName = strings.ReplaceAll(agg.OperandName, " ", "")
	} else if agg.Operand != nil {
		switch v := agg.Operand.(type) {
		case *values.ConstantValue:
			if v.Value == nil {
				opName = "*"
			} else {
				opName = v.Name()
			}
		case *values.FieldValue:
			opName = v.Field
		default:
			opName = values.ColumnNameValue(agg.Operand)
		}
	}
	switch agg.Function {
	case AggCount:
		return strings.ToUpper(fmt.Sprintf("COUNT(%s)", opName))
	case AggSum:
		return strings.ToUpper(fmt.Sprintf("SUM(%s)", opName))
	case AggMin:
		return strings.ToUpper(fmt.Sprintf("MIN(%s)", opName))
	case AggMax:
		return strings.ToUpper(fmt.Sprintf("MAX(%s)", opName))
	case AggAvg:
		return strings.ToUpper(fmt.Sprintf("AVG(%s)", opName))
	default:
		return strings.ToUpper(fmt.Sprintf("AGG(%s)", opName))
	}
}

// GroupByOutputColumnNames is THE single naming authority for a streaming
// aggregate's output ROW: grouping keys (in GROUP BY order) then aggregates (in
// aggregate order), each aggregate ALIAS-preferring (upper alias, else the
// canonical AggregateResultColumnName). The order — [groupKeys..., aggregates...]
// — is the ordinal order the executor's aggregateCursor emits and the translator
// bakes downstream references against. Returns an empty slice when there are no
// output columns.
func GroupByOutputColumnNames(groupingKeys []values.Value, aggregates []AggregateSpec) []string {
	names := make([]string, 0, len(groupingKeys)+len(aggregates))
	for _, k := range groupingKeys {
		names = append(names, AggregateKeyColumnName(k))
	}
	for _, a := range aggregates {
		if a.Alias != "" {
			names = append(names, strings.ToUpper(a.Alias))
		} else {
			names = append(names, AggregateResultColumnName(a))
		}
	}
	return names
}

func (e *GroupByExpression) GetGroupingKeys() []values.Value { return e.groupingKeys }
func (e *GroupByExpression) GetAggregates() []AggregateSpec  { return e.aggregates }
func (e *GroupByExpression) GetInner() Quantifier            { return e.inner }
func (e *GroupByExpression) GetQuantifiers() []Quantifier    { return []Quantifier{e.inner} }
func (e *GroupByExpression) CanCorrelate() bool              { return false }
func (e *GroupByExpression) ChildrenAsSet() bool             { return false }

func (e *GroupByExpression) GetResultValue() values.Value {
	return e.inner.GetFlowedObjectValue()
}

func (e *GroupByExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

func (e *GroupByExpression) EqualsWithoutChildren(other RelationalExpression, aliases *AliasMap) bool {
	o, ok := other.(*GroupByExpression)
	if !ok {
		return false
	}
	if len(e.groupingKeys) != len(o.groupingKeys) {
		return false
	}
	if len(e.aggregates) != len(o.aggregates) {
		return false
	}
	// Alias-aware grouping-key + aggregate-operand equality (RFC-040 040.2).
	// OperandName (alias-bearing canonical text) is intentionally not compared
	// — equality already ignored it, and it must stay out for alias-invariance.
	vm := aliases.ToValuesAliasMap()
	for i, k := range e.groupingKeys {
		if !values.SemanticEqualsUnderAliasMap(k, o.groupingKeys[i], vm) {
			return false
		}
	}
	for i, a := range e.aggregates {
		if a.Function != o.aggregates[i].Function {
			return false
		}
		if !values.SemanticEqualsUnderAliasMap(a.Operand, o.aggregates[i].Operand, vm) {
			return false
		}
	}
	return true
}

func (e *GroupByExpression) HashCodeWithoutChildren() uint64 {
	h := fnv.New64a()
	h.Write([]byte("grpby|"))
	var b [8]byte
	for _, k := range e.groupingKeys {
		binary.LittleEndian.PutUint64(b[:], values.SemanticHashCode(k))
		h.Write(b[:])
		h.Write([]byte("|"))
	}
	for _, a := range e.aggregates {
		binary.LittleEndian.PutUint64(b[:], uint64(a.Function))
		h.Write(b[:])
		binary.LittleEndian.PutUint64(b[:], values.SemanticHashCode(a.Operand))
		h.Write(b[:])
		h.Write([]byte("|"))
	}
	return h.Sum64()
}

func (e *GroupByExpression) WithQuantifiers(quantifiers []Quantifier) RelationalExpression {
	if len(quantifiers) == 0 {
		return e
	}
	cp := *e
	cp.inner = quantifiers[0]
	return &cp
}

var _ RelationalExpression = (*GroupByExpression)(nil)
