// Portions derived from FoundationDB Record Layer (
// IndexOnlyAggregateValue.java, NumericAggregationValue.java,
// GroupByExpression.java, Type.java, and others),
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

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
	// AggMinEver and AggMaxEver are Java's IndexOnlyAggregateValue.MinEver /
	// MaxEver: non-evaluable, answerable only from a MIN_EVER / MAX_EVER index.
	AggMinEver
	AggMaxEver
	// AggBitmapConstructAgg is Java's NumericAggregationValue.BitmapConstructAgg.
	AggBitmapConstructAgg
	// AggArrayAgg is Java's ArrayAggValue.
	AggArrayAgg
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
	case AggMinEver:
		return "MIN_EVER"
	case AggMaxEver:
		return "MAX_EVER"
	case AggBitmapConstructAgg:
		return "BITMAP_CONSTRUCT_AGG"
	case AggArrayAgg:
		return "ARRAY_AGG"
	default:
		return "UNKNOWN"
	}
}

// HasStreamingAccumulator reports whether the streaming aggregation can
// compute f over a group's rows. MIN_EVER and MAX_EVER are index-only in Java
// too (IndexOnlyAggregateValue is non-evaluable).
func (f AggregateFunction) HasStreamingAccumulator() bool {
	switch f {
	case AggCount, AggSum, AggMin, AggMax, AggAvg, AggArrayAgg, AggBitmapConstructAgg:
		return true
	default:
		return false
	}
}

// AggregateSpec describes one aggregate column in a GroupBy.
type AggregateSpec struct {
	Function AggregateFunction
	Operand  values.Value
	Alias    string
	// OperandName is the canonical operand text for result-map keying (e.g.
	// "PRICE*QTY").
	//
	// PRODUCER CONTRACT — it must arrive ALREADY CANONICAL, because
	// AggregateResultColumnName carries it verbatim and repairs nothing.
	// Canonical means the operand's TOKENS, concatenated with no whitespace
	// BETWEEN them, each token upper-cased EXCEPT:
	//
	//   - a delimited identifier, which contributes its declared spelling
	//     unquoted — INCLUDING any space inside it, so `"two words"` stays
	//     `two words` and the name legitimately contains a space;
	//   - a STRING LITERAL, which is data rather than a name, and folding it
	//     collides two aggregates that count different things.
	//
	// The earlier wording here said "whitespace already removed", which the two
	// exception clauses directly below it contradict: inter-token whitespace is
	// dropped, whitespace INSIDE a token is content and is kept.
	//
	// Nothing validates this, and the reason is worth stating rather than
	// leaving as an omission: the canonical form is produced from an ANTLR token
	// stream at the parse boundary (embedded.aggOperandCanonicalText and the two
	// name mints in logical_predicate.go), and a checker here would have to
	// re-derive that from a flat string it cannot re-tokenize — the same reason
	// parseColRef cannot see a dot inside a delimited identifier. The guard is
	// therefore the OUTPUT side — group_by_naming_verbatim_test.go pins that this
	// field is published unedited, so a producer that folds shows up as a folded
	// column name rather than as nothing.
	OperandName string
	// OperandIntType carries the operand's static numeric TypeCode at plan time.
	// The legacy field name is retained for source compatibility. SUM/AVG use
	// TypeCodeInt for Java's int32 overflow semantics and TypeCodeFloat for
	// float32 accumulation; LONG and DOUBLE retain their respective widths.
	// The translator copies Operand.Type().Code(), matching Java's operator
	// selection in NumericAggregationValue.encapsulate, so execution needs no
	// per-row type derivation. TypeCodeUnknown retains the legacy runtime-carrier
	// dispatch with int64 overflow checks; resolved SQL operands state their code.
	OperandIntType values.TypeCode
	// IgnoreNulls and Limit are ARRAY_AGG's options (values.ArrayAggNoLimit
	// when uncapped).
	IgnoreNulls bool
	Limit       int
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
	return newGroupByExpression(groupingKeys, aggregates, inner,
		GroupByOutputColumnNames(groupingKeys, aggregates))
}

func newGroupByExpression(
	groupingKeys []values.Value,
	aggregates []AggregateSpec,
	inner Quantifier,
	names []string,
) (*GroupByExpression, error) {
	groupingCopy := slices.Clone(groupingKeys)
	aggregateCopy := slices.Clone(aggregates)
	if len(names) != len(groupingCopy)+len(aggregateCopy) {
		return nil, fmt.Errorf("GroupByExpression: %d output names for %d keys and %d aggregates",
			len(names), len(groupingCopy), len(aggregateCopy))
	}
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

// WithTranslatedValues rebuilds the aggregate over translated keys, aggregates
// and input, keeping its output columns' names: a correlation rewrite changes
// what the row is computed from, not the row, and the names are data, as Java
// keeps a Field's name on its Type.
func (e *GroupByExpression) WithTranslatedValues(
	groupingKeys []values.Value, aggregates []AggregateSpec, inner Quantifier,
) (*GroupByExpression, error) {
	return newGroupByExpression(groupingKeys, aggregates, inner, e.OutputColumnNames())
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
	case AggMinEver, AggMaxEver:
		// Java's MinEverFn / MaxEverFn take any single operand and keep its
		// type (IndexOnlyAggregateValue.encapsulate); what the index may store
		// is the index generator's to decide.
		if aggregate.Operand == nil {
			return nil, fmt.Errorf("%s requires an operand", aggregate.Function)
		}
		operandType, err := snapshotExpressionResultType(aggregate.Function.String()+" operand", aggregate.Operand.Type())
		if err != nil {
			return nil, err
		}
		indexOnlyOp := values.IndexOnlyMinEverLong
		if aggregate.Function == AggMaxEver {
			indexOnlyOp = values.IndexOnlyMaxEverLong
		}
		exactResult, err := snapshotExpressionResultType(aggregate.Function.String(), values.WithNullability(operandType.Type(), true))
		if err != nil {
			return nil, err
		}
		return values.NewDerivedValueWithType(
			[]values.Value{values.NewIndexOnlyAggregateValue(indexOnlyOp, aggregate.Operand)}, exactResult.Type()), nil
	case AggBitmapConstructAgg:
		// Java's operator map has BITMAP_CONSTRUCT_AGG over INT and LONG only
		// (NumericAggregationValue.PhysicalOperator), yielding BYTES.
		if aggregate.Operand == nil {
			return nil, fmt.Errorf("%s requires an operand", aggregate.Function)
		}
		operandType, err := snapshotExpressionResultType(aggregate.Function.String()+" operand", aggregate.Operand.Type())
		if err != nil {
			return nil, err
		}
		if code := operandType.Type().Code(); code != values.TypeCodeInt && code != values.TypeCodeLong {
			return nil, fmt.Errorf("%s requires an INT or LONG operand, got %v", aggregate.Function, operandType.Type())
		}
		op = values.AggBitmapConstructAgg
		resultType = values.NullableBytes
	case AggArrayAgg:
		if aggregate.Operand == nil {
			return nil, fmt.Errorf("%s requires an operand", aggregate.Function)
		}
		av := values.NewArrayAggValue(aggregate.Operand, aggregate.IgnoreNulls, aggregate.Limit)
		exactResult, err := snapshotExpressionResultType(aggregate.Function.String(), av.Type())
		if err != nil {
			return nil, err
		}
		return values.NewDerivedValueWithType([]values.Value{av}, exactResult.Type()), nil
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
// key. THE single naming authority for a group key — the plan's stated output row
// (OutputRecordType), the executor's aggregateCursor, and the translator's ordinal baking all read
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
// OperandName arrives ALREADY canonical — upper-cased except for delimited
// identifiers, whitespace already dropped (embedded.aggOperandCanonicalText, the
// sole mint). It is therefore used VERBATIM here. The two repairs that used to
// sit at this site, a space-strip and an upper-fold over the whole composed
// name, were the reason `SUM("qty")` on a column genuinely named `qty` reported
// itself as `SUM(QTY)` while the same row's GROUP BY key reported `qty`. A
// repair applied to a name is a second normalization, and a second
// normalization is what this whole family of defects is.
func AggregateResultColumnName(agg AggregateSpec) string {
	opName := "?"
	if agg.OperandName != "" {
		opName = agg.OperandName
	} else if agg.Operand != nil {
		if c, isConst := agg.Operand.(*values.ConstantValue); isConst {
			if c.Value == nil {
				opName = "*"
			} else {
				// A constant's rendered name is not a user identifier — it is
				// this package's own word for "a literal sat here". So it is
				// upper-cased, and that is not the fold this file removed: the
				// rule at the mint is "upper-case what is not a delimited
				// identifier", and a minted placeholder is squarely on the
				// upper side of it. The line below is the one that must not
				// fold, because ColumnNameValue renders real field names.
				opName = strings.ToUpper(c.Name())
			}
		} else {
			opName = values.ColumnNameValue(agg.Operand)
		}
	}
	// The function symbol is written upper-case as a LITERAL, so no fold is
	// needed to make it upper — and none may be applied, because a fold here
	// reaches the operand too.
	switch agg.Function {
	case AggCount:
		return fmt.Sprintf("COUNT(%s)", opName)
	case AggSum:
		return fmt.Sprintf("SUM(%s)", opName)
	case AggMin:
		return fmt.Sprintf("MIN(%s)", opName)
	case AggMax:
		return fmt.Sprintf("MAX(%s)", opName)
	case AggAvg:
		return fmt.Sprintf("AVG(%s)", opName)
	case AggArrayAgg, AggMinEver, AggMaxEver, AggBitmapConstructAgg:
		return fmt.Sprintf("%s(%s)", agg.Function, opName)
	default:
		return fmt.Sprintf("AGG(%s)", opName)
	}
}

// GroupByOutputColumnNames is THE single naming authority for a streaming
// aggregate's output ROW: grouping keys (in GROUP BY order) then aggregates (in
// aggregate order), each aggregate ALIAS-preferring (the alias verbatim, else
// the canonical AggregateResultColumnName). The order — [groupKeys..., aggregates...]
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
			names = append(names, a.Alias)
		} else {
			names = append(names, AggregateResultColumnName(a))
		}
	}
	return names
}

// OutputColumnNames is the aggregate row's column names, stated once at
// construction and kept across rewrites; consumers read them rather than
// derive them again from keys that a rewrite may have re-rooted.
func (e *GroupByExpression) OutputColumnNames() []string {
	names := make([]string, len(e.resultValue.Fields))
	for i, field := range e.resultValue.Fields {
		names[i] = field.Name
	}
	return names
}

func (e *GroupByExpression) GetGroupingKeys() []values.Value { return slices.Clone(e.groupingKeys) }
func (e *GroupByExpression) GetAggregates() []AggregateSpec  { return slices.Clone(e.aggregates) }
func (e *GroupByExpression) GetInner() Quantifier            { return e.inner }
func (e *GroupByExpression) GetQuantifiers() []Quantifier    { return []Quantifier{e.inner} }
func (e *GroupByExpression) CanCorrelate() bool              { return false }
func (e *GroupByExpression) ChildrenAsSet() bool             { return false }

func (e *GroupByExpression) GetResultValue() values.Value {
	return e.resultValue
}

func (e *GroupByExpression) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return values.GetCorrelatedToOfValue(e.resultValue)
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
		if a.Function != o.aggregates[i].Function || a.IgnoreNulls != o.aggregates[i].IgnoreNulls || a.Limit != o.aggregates[i].Limit {
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
		binary.LittleEndian.PutUint64(b[:], uint64(a.Function)|uint64(uint32(a.Limit))<<16)
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
	return e.WithTranslatedValues(e.groupingKeys, e.aggregates, quantifiers[0])
}

var _ RelationalExpression = (*GroupByExpression)(nil)
