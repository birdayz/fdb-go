// Portions derived from FoundationDB Record Layer (FunctionKeyExpression.java,
// KeyExpression.java, FieldKeyExpression.java, NestingKeyExpression.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fmt"

	"google.golang.org/protobuf/proto"
)

// GroupingKeyExpression wraps a key expression and divides its columns into
// "grouping" (leading) and "grouped" (trailing) parts. The grouping columns
// form the GROUP BY key, and the grouped columns are the aggregated values.
// Matches Java's com.apple.foundationdb.record.metadata.expressions.GroupingKeyExpression.
type GroupingKeyExpression struct {
	wholeKey     KeyExpression
	groupedCount int // number of trailing columns that are "grouped" (aggregated)
}

// GroupBy creates a GroupingKeyExpression that groups by the given expressions
// and treats the receiver as the aggregated value.
// Example: Field("score").GroupBy(Field("game_id")) →
//
//	wholeKey = Concat(game_id, score), groupedCount = 1
//
// Matches Java's KeyExpression.groupBy().
func GroupBy(grouped KeyExpression, groupBy ...KeyExpression) *GroupingKeyExpression {
	groupedColCount := grouped.ColumnSize()
	allExprs := make([]KeyExpression, 0, len(groupBy)+1)
	// Java's groupBy goes through Key.Expressions.concat, whose
	// ThenKeyExpression constructor flattens a nested Then into its children
	// (ThenKeyExpression.java:264-270), as Concat does; the flattened list is
	// needed here to tell a single child from several.
	//
	// A Then that Java's constructor refused keeps its place in program order
	// (Concat): the whole key stays a Then carrying it, so Build reports it.
	var fault uint64
	for _, e := range append(append([]KeyExpression{}, groupBy...), grouped) {
		if then, ok := e.(*CompositeKeyExpression); ok {
			allExprs = append(allExprs, then.SubKeyExpressions()...)
			fault = earlierFault(fault, then.arityFaultSeq)
			continue
		}
		allExprs = append(allExprs, e)
	}

	var wholeKey KeyExpression
	if len(allExprs) == 1 && fault == 0 {
		wholeKey = allExprs[0]
	} else {
		then := Concat(allExprs...).(*CompositeKeyExpression)
		then.arityFaultSeq = earlierFault(then.arityFaultSeq, fault)
		wholeKey = then
	}
	return &GroupingKeyExpression{wholeKey: wholeKey, groupedCount: groupedColCount}
}

// Ungrouped creates a GroupingKeyExpression where all columns are "grouped"
// (aggregated) and there are no grouping columns. Used for ungrouped counts.
// Example: Ungrouped(EmptyKey()) → count all records with no grouping.
// Matches Java's KeyExpression.ungrouped().
func Ungrouped(expr KeyExpression) *GroupingKeyExpression {
	return &GroupingKeyExpression{
		wholeKey:     expr,
		groupedCount: expr.ColumnSize(),
	}
}

// GroupAll creates a GroupingKeyExpression where all columns are "grouping"
// and there are no aggregated columns. Used for COUNT indexes where the
// entire expression forms the GROUP BY key.
// Example: GroupAll(Field("price")) → count grouped by price.
// Matches Java's new GroupingKeyExpression(expr, 0).
func GroupAll(expr KeyExpression) *GroupingKeyExpression {
	return &GroupingKeyExpression{
		wholeKey:     expr,
		groupedCount: 0,
	}
}

// Evaluate delegates to the whole key expression.
// The grouping/grouped split is metadata, not evaluation logic.
func (g *GroupingKeyExpression) Evaluate(record *FDBStoredRecord[proto.Message], msg proto.Message) ([][]any, error) {
	return g.wholeKey.Evaluate(record, msg)
}

// EvaluateFlat is the whole key's one tuple. A whole key yielding no tuple or
// several (a fan-out) is an error, so callers evaluate its entries instead.
func (g *GroupingKeyExpression) EvaluateFlat(record *FDBStoredRecord[proto.Message], msg proto.Message) ([]any, error) {
	if fe, ok := g.wholeKey.(flatEvaluator); ok {
		return fe.EvaluateFlat(record, msg)
	}
	tuples, err := g.wholeKey.Evaluate(record, msg)
	if err != nil {
		return nil, err
	}
	if len(tuples) != 1 {
		return nil, fmt.Errorf("EvaluateFlat: grouping key produced %d tuples, expected 1", len(tuples))
	}
	return tuples[0], nil
}

// FieldNames returns field names from the whole key.
func (g *GroupingKeyExpression) FieldNames() []string {
	return g.wholeKey.FieldNames()
}

// ColumnSize delegates to the whole key expression's column size.
func (g *GroupingKeyExpression) ColumnSize() int {
	return g.wholeKey.ColumnSize()
}

// GetGroupedCount returns the number of trailing "grouped" (aggregated) columns.
func (g *GroupingKeyExpression) GetGroupedCount() int {
	return g.groupedCount
}

// GetGroupingCount returns the number of leading "grouping" (GROUP BY) columns.
func (g *GroupingKeyExpression) GetGroupingCount() int {
	return g.wholeKey.ColumnSize() - g.groupedCount
}

// KeyWithValueExpression wraps an inner key expression and splits its evaluated
// columns into a "key" portion and a "value" portion. The key portion (columns
// 0..splitPoint-1) is stored in the FDB key, and the value portion (columns
// splitPoint..end) is stored in the FDB value. This enables "covering indexes"
// where additional query-relevant columns can be read without fetching the record.
// Matches Java's com.apple.foundationdb.record.metadata.expressions.KeyWithValueExpression.
type KeyWithValueExpression struct {
	innerKey   KeyExpression
	splitPoint int
}

// KeyWithValue creates a KeyWithValueExpression. splitPoint is the number of
// leading columns that go into the FDB key; remaining columns go into the value.
// Matches Java's Key.Expressions.keyWithValue(inner, splitPoint).
func KeyWithValue(inner KeyExpression, splitPoint int) *KeyWithValueExpression {
	return &KeyWithValueExpression{innerKey: inner, splitPoint: splitPoint}
}

// Evaluate delegates to the inner key expression. The key/value split is only
// applied when writing/reading index entries, not during evaluation.
func (k *KeyWithValueExpression) Evaluate(record *FDBStoredRecord[proto.Message], msg proto.Message) ([][]any, error) {
	return k.innerKey.Evaluate(record, msg)
}

// FieldNames delegates to the inner key expression.
func (k *KeyWithValueExpression) FieldNames() []string {
	return k.innerKey.FieldNames()
}

// ColumnSize returns the split point — only key columns count, not value columns.
// Matches Java's KeyWithValueExpression.getColumnSize().
func (k *KeyWithValueExpression) ColumnSize() int {
	return k.splitPoint
}

// InnerKey returns the wrapped key expression.
func (k *KeyWithValueExpression) InnerKey() KeyExpression {
	return k.innerKey
}

// SplitPoint returns the number of leading columns that form the key portion.
func (k *KeyWithValueExpression) SplitPoint() int {
	return k.splitPoint
}

// SplitEvaluatedKey splits a full evaluated result into key and value portions.
// key = columns[0:splitPoint], value = columns[splitPoint:].
// Matches Java's KeyWithValueExpression.getKey() / getValue().
func (k *KeyWithValueExpression) SplitEvaluatedKey(fullKey []any) (keyPart []any, valuePart []any) {
	if k.splitPoint >= len(fullKey) {
		return fullKey, nil
	}
	return fullKey[:k.splitPoint], fullKey[k.splitPoint:]
}
