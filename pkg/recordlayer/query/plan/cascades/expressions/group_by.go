package expressions

import (
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"slices"
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
	// TypeCode. values.TypeCodeInt means the operand is statically a SQL INTEGER
	// (proto TYPE_INT32) → int32 overflow; any other value (including the zero
	// TypeCodeUnknown) keeps the int64 (SUM_L) domain. This is a SEPARATE field
	// rather than a read of Operand.Type() at execution time because a minted
	// bare-column operand carries no static type — the translator answers from
	// the operand's own resolved type when it states one (Java's encapsulate
	// rule) and from the proto-faithful input record type otherwise.
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
	resultValue  *values.RecordConstructorValue
}

func NewGroupByExpression(
	groupingKeys []values.Value,
	aggregates []AggregateSpec,
	inner Quantifier,
) (*GroupByExpression, error) {
	groupingCopy := slices.Clone(groupingKeys)
	aggregateCopy := slices.Clone(aggregates)
	names := GroupByOutputColumnNames(groupingCopy, aggregateCopy)
	fields := make([]values.RecordConstructorField, 0, len(names))
	for i, groupingKey := range groupingCopy {
		if groupingKey == nil {
			return nil, fmt.Errorf("GroupByExpression grouping key %d: value is nil", i)
		}
		if _, err := snapshotExpressionResultType("GroupByExpression grouping key", groupingKey.Type()); err != nil {
			return nil, err
		}
		fields = append(fields, values.RecordConstructorField{Name: names[i], Value: groupingKey})
	}
	for i, aggregate := range aggregateCopy {
		result, err := groupByAggregateResultValue(aggregate)
		if err != nil {
			return nil, fmt.Errorf("GroupByExpression aggregate %d: %w", i, err)
		}
		fields = append(fields, values.RecordConstructorField{
			Name:  names[len(groupingCopy)+i],
			Value: result,
		})
	}
	// This is the aggregate's private native ordinal row, not a user-facing
	// SELECT projection. Duplicate grouping-key names remain duplicate here:
	// the physical streaming aggregate and executor address these slots by
	// ordinal and publish the same raw schema. A later output projection owns
	// SQL name de-duplication.
	resultValue := values.NewRawRecordConstructorValue(fields...)
	if _, err := snapshotExpressionResultType("GroupByExpression", resultValue.Type()); err != nil {
		return nil, err
	}
	return &GroupByExpression{
		groupingKeys: groupingCopy,
		aggregates:   aggregateCopy,
		inner:        inner,
		resultValue:  resultValue,
	}, nil
}

func groupByAggregateResultValue(aggregate AggregateSpec) (values.Value, error) {
	var (
		op         values.AggregateOp
		resultType values.Type
	)
	switch aggregate.Function {
	case AggCount:
		if aggregate.Operand == nil {
			op = values.AggCountStar
		} else {
			op = values.AggCount
			if _, err := snapshotExpressionResultType("COUNT operand", aggregate.Operand.Type()); err != nil {
				return nil, err
			}
		}
		resultType = values.NullableLong
	case AggAvg, AggSum, AggMin, AggMax:
		if aggregate.Operand == nil {
			return nil, fmt.Errorf("%s requires an operand", aggregate.Function)
		}
		operandType, err := snapshotExpressionResultType(aggregate.Function.String()+" operand", aggregate.Operand.Type())
		if err != nil {
			return nil, err
		}
		if !numericAggregateType(operandType.Type()) {
			return nil, fmt.Errorf("%s requires a numeric operand, got %v", aggregate.Function, operandType.Type())
		}
		switch aggregate.Function {
		case AggAvg:
			op = values.AggAvg
			resultType = values.NullableDouble
		case AggSum:
			op = values.AggSum
			resultType = values.WithNullability(operandType.Type(), true)
		case AggMin:
			op = values.AggMin
			resultType = values.WithNullability(operandType.Type(), true)
		case AggMax:
			op = values.AggMax
			resultType = values.WithNullability(operandType.Type(), true)
		}
	default:
		return nil, fmt.Errorf("unsupported aggregate function %d", aggregate.Function)
	}
	exactResult, err := snapshotExpressionResultType(aggregate.Function.String(), resultType)
	if err != nil {
		return nil, err
	}
	aggregateValue := &values.AggregateValue{Op: op, Operand: aggregate.Operand}
	return values.NewDerivedValueWithType([]values.Value{aggregateValue}, exactResult.Type()), nil
}

func numericAggregateType(typ values.Type) bool {
	if typ == nil {
		return false
	}
	switch typ.Code() {
	case values.TypeCodeInt, values.TypeCodeLong, values.TypeCodeFloat, values.TypeCodeDouble:
		return true
	default:
		return false
	}
}

// AggregateKeyColumnName is the canonical output-column name for one grouping
// key. THE single naming authority for a group key — the plan's OutputColumnNames,
// the executor's aggregateCursor, and the translator's ordinal baking all read
// the name from here so a baked ordinal and the emitted positional slot can never
// disagree.
//
// A NESTED key takes its resolved PATH, never a single segment of it. `Field`
// carries one segment — the struct root when this was written, so `n.sk` and
// `n.co` were both `N`; the leaf now, so `t1.n.sk` and a flat `sk` are both
// `SK` — and this name is a MAP KEY in three downstream last-wins maps, so
// either spelling silently collapses two grouping columns into one and returns
// too few groups. Nested-path GROUP BY does not plan today, which is the
// only reason that is latent rather than live; the conversion lands FIRST so
// implementing the feature cannot arm it.
//
// The path is QUALIFIED when the key reference carries a Child — `T1.N.SK` over
// a ≥2-source FROM, `N.SK` over one — because NestedResolvedPath renders through
// the child. That is deliberate and is argued at the predicate: over two sources
// each declaring an `n`, a bare `N.SK` would re-create one level up exactly the
// collapse this function exists to prevent. `Child == nil` is the common case,
// not the rule.
// The name is taken VERBATIM, and the nested arm above already was. A grouping
// key names a column that exists elsewhere — in the source row it reads and in
// the projection that references the group — and a fold here made this the one
// authority that spelled it differently. That was invisible while every
// descriptor name was upper-folded on the way in, and it is a
// reference-misses-its-own-column bug the moment one is not.
func AggregateKeyColumnName(k values.Value) string {
	if path, nested := values.NestedResolvedPath(k); nested {
		return path
	}
	if fv, ok := values.AsFieldValue(k); ok {
		return fv.DisplayName()
	}
	return values.ColumnNameValue(k)
}

// AggregateResultColumnName is the canonical output-column name for one aggregate
// (function + operand), IGNORING any alias: the SELECT list references this
// canonical text and the projection above applies the alias. THE single naming
// authority for an aggregate column (same rationale as AggregateKeyColumnName).
//
// The operand's text comes from OperandName — the parse text captured once, at
// the sole production mint (cascades_translator.go), and carried on the spec as
// DATA. That is the shape Java uses for every name it keeps: Column.of stores an
// Optional<String> on the Field at construction (Column.java:81-82,
// Type.java:2908-2910) and every downstream read is a getter
// (Type.java:2750-2763), never a re-derivation from the Value. Where Java keeps
// no name it keeps NONE — an unaliased aggregate is Column.unnamedOf
// (GroupByExpression.java:754) and surfaces as the positional `_0`
// (Type.java:2645-2651), so the rendered `COUNT(X)` spelling below is a Go-only
// display convention with no upstream contract behind it.
//
// A Value-derived fallback is therefore deliberately NOT a `.Field` read. Reading
// the leaf name off a FieldValue is a SECOND copy of the Value→name rendering
// rule, and it disagrees with the authority (ColumnNameValue) on exactly the
// shape that matters: a qualified operand `t.v` renders bare `V`, so `SUM(t.v)`
// and `SUM(u.v)` both spell `SUM(V)` and collapse in the last-wins aggregate
// output-ordinal map (groupByOutputOrdinals). ColumnNameValue is the one
// rendering every output-naming site must use, and it keeps them apart.
func AggregateResultColumnName(agg AggregateSpec) string {
	opName := "?"
	if agg.OperandName != "" {
		opName = strings.ReplaceAll(agg.OperandName, " ", "")
	} else if agg.Operand != nil {
		if c, isConst := agg.Operand.(*values.ConstantValue); isConst {
			if c.Value == nil {
				opName = "*"
			} else {
				opName = c.Name()
			}
		} else {
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

func (e *GroupByExpression) GetGroupingKeys() []values.Value { return slices.Clone(e.groupingKeys) }
func (e *GroupByExpression) GetAggregates() []AggregateSpec  { return slices.Clone(e.aggregates) }
func (e *GroupByExpression) GetInner() Quantifier            { return e.inner }
func (e *GroupByExpression) GetQuantifiers() []Quantifier    { return []Quantifier{e.inner} }
func (e *GroupByExpression) CanCorrelate() bool              { return false }
func (e *GroupByExpression) ChildrenAsSet() bool             { return false }

// GetResultValue passes the inner's flowed object through with the TYPE
// DELIBERATELY STRIPPED. Both halves need saying, and the passthrough half is
// already a known, measured divergence.
//
// Java's GroupByExpression.getResultValue() (:129) is
// `resultValueFunction.apply(groupingValue, aggregateValue)` (:152) — a record
// constructor of the GROUPING columns and the AGGREGATE columns, i.e. the row the
// operator OUTPUTS. Go returns the inner's row, which is the row it CONSUMES.
// The consequence is measured and documented at the site that pays for it
// (rule_push_requested_ordering_through_groupby.go): pushing an output-slot
// reference through Go's result value is the identity, so a request for output
// slot 0 pushes to input slot 0, value equality fails on every group-by in the
// corpus, and an index that served the grouping order is replaced by a
// materialized sort.
//
// While the flowed accessor carried no type that wrongness was confined to the
// Value's shape. Typed, the site additionally ASSERTS that a GROUP BY flows its
// input row — legs and all — and a reader that believes a stated row takes it at
// its word. Stating no type is the honest interim; stating the input's is a wrong
// answer with a stated type on it.
//
// The real fix is the output row, built from GetGroupingKeys and GetAggregates,
// and it is CQ-59's exact territory: it changes the space every downstream
// reference to a group-by's result is baked against, so it is that item's unit of
// work rather than a rider here. Until it lands this site must not be "cleaned up"
// back onto the typed accessor.
func (e *GroupByExpression) GetResultValue() values.Value {
	return e.resultValue
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

func (e *GroupByExpression) WithQuantifiers(quantifiers []Quantifier) (RelationalExpression, error) {
	if err := requireQuantifierArity("GroupByExpression", len(quantifiers), 1); err != nil {
		return nil, err
	}
	return NewGroupByExpression(e.groupingKeys, e.aggregates, quantifiers[0])
}

var _ RelationalExpression = (*GroupByExpression)(nil)
