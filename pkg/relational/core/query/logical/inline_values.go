// Portions derived from FoundationDB Record Layer (ExplodeExpression.java,
// TableFunctionExpression.java),
// Copyright 2015-2020 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package logical

import (
	"fmt"
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// LogicalInlineValues is a multi-row VALUES table source in a FROM clause.
// Its collection is the exact literal array Java lowers directly to an
// ExplodeExpression: every array element is one named record row.
//
// This is deliberately distinct from both LogicalSingleton (the
// single-row, zero-column SELECT-without-FROM source) and LogicalUnnest (a lateral
// correlated array access). Conflating either shape with an inline table would
// give it the wrong cardinality or make it participate in lateral-unnest
// gather/collision rules.
type LogicalInlineValues struct {
	Alias      string
	Binding    string
	collection values.Value
	// stream is set instead of collection for a built-in table function
	// (range): Java lowers it to a TableFunctionExpression, not an Explode.
	stream     values.Value
	resultType values.ExactTypeHandle
}

// NewTableFunctionSource constructs a table-function source over a streaming
// Value whose Type is the per-row record type.
func NewTableFunctionSource(alias string, stream values.Value) (*LogicalInlineValues, error) {
	if alias == "" || stream == nil {
		return nil, fmt.Errorf("table function source requires an alias and a stream value")
	}
	resultType, err := values.SnapshotExactType(stream.Type())
	if err != nil {
		return nil, fmt.Errorf("table function result row: %w", err)
	}
	return &LogicalInlineValues{Alias: alias, stream: stream, resultType: resultType}, nil
}

// NewInlineValues constructs an exact literal-table source. alias is the
// source's query-block correlation (an authored inline-table alias, or a
// parser-minted private alias when SQL omitted one).
func NewInlineValues(alias string, collection values.Value) (*LogicalInlineValues, error) {
	if alias == "" {
		return nil, fmt.Errorf("inline VALUES source requires a non-empty correlation alias")
	}
	if collection == nil {
		return nil, fmt.Errorf("inline VALUES source collection is nil")
	}
	array, ok := collection.Type().(*values.ArrayType)
	if !ok || array.ElementType == nil {
		return nil, fmt.Errorf("inline VALUES source requires an exact array collection, got %v", collection.Type())
	}
	row, ok := array.ElementType.(*values.RecordType)
	if !ok {
		return nil, fmt.Errorf("inline VALUES source array element is not a record row: %v", array.ElementType)
	}
	resultType, err := values.SnapshotExactType(row)
	if err != nil {
		return nil, fmt.Errorf("inline VALUES result row: %w", err)
	}
	return &LogicalInlineValues{
		Alias:      alias,
		collection: collection,
		resultType: resultType,
	}, nil
}

func (*LogicalInlineValues) Children() []LogicalOperator { return []LogicalOperator{} }

func (v *LogicalInlineValues) Explain(indent string) string {
	if v.stream != nil {
		return fmt.Sprintf("%sTableFunction(%s AS %s)", indent, values.ExplainValue(v.stream), v.Alias)
	}
	return fmt.Sprintf("%sInlineValues(%s AS %s)", indent, values.ExplainValue(v.collection), v.Alias)
}

func (v *LogicalInlineValues) CollectionValue() values.Value { return v.collection }

func (v *LogicalInlineValues) StreamValue() values.Value { return v.stream }

// SetStream binds a table function's arguments once its lateral scope is
// known; the row type is fixed by the function, not by its arguments.
func (v *LogicalInlineValues) SetStream(stream values.Value) { v.stream = stream }

func (v *LogicalInlineValues) ResultType() values.Type { return v.resultType.Type() }

// FindOwnerInlineValues resolves one visible inline VALUES source in the
// current logical FROM scope. CTE bodies are separate scopes, so a CTE exposes
// only its Main here. Duplicate aliases are ambiguous and deliberately return
// nil instead of selecting whichever leaf the traversal happens to visit
// first.
func FindOwnerInlineValues(op LogicalOperator, alias string) *LogicalInlineValues {
	if op == nil || alias == "" {
		return nil
	}
	var found *LogicalInlineValues
	ambiguous := false
	var walk func(LogicalOperator)
	walk = func(current LogicalOperator) {
		if current == nil || ambiguous {
			return
		}
		switch typed := current.(type) {
		case *LogicalInlineValues:
			if !strings.EqualFold(typed.Alias, alias) {
				return
			}
			if found != nil && found != typed {
				ambiguous = true
				return
			}
			found = typed
		case *LogicalCTE:
			walk(typed.Main)
		default:
			for _, child := range current.Children() {
				walk(child)
			}
		}
	}
	walk(op)
	if ambiguous {
		return nil
	}
	return found
}
