// Portions derived from FoundationDB Record Layer (RecordQueryTextIndexPlan.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TextScanGroupBindingError reports a range that does not identify one physical TEXT group.
type TextScanGroupBindingError struct {
	IndexName       string
	Range           recordlayer.TupleRange
	GroupingColumns int
}

func (e *TextScanGroupBindingError) Error() string {
	return fmt.Sprintf("executor: text scan range %v does not bind one %d-column group of %q", e.Range, e.GroupingColumns, e.IndexName)
}

func textScanGroup(indexName string, rng recordlayer.TupleRange, grouping int) (tuple.Tuple, error) {
	if len(rng.Low) < grouping || len(rng.High) < grouping || !bytes.Equal(rng.Low[:grouping].Pack(), rng.High[:grouping].Pack()) {
		return nil, &TextScanGroupBindingError{IndexName: indexName, Range: rng, GroupingColumns: grouping}
	}
	return append(tuple.Tuple(nil), rng.Low[:grouping]...), nil
}

// executeTextIndexScan runs recordlayer.TextScan once per physical group the
// grouping bindings name (SQL's -0.0 = 0.0 names two), then fetches records.
func executeTextIndexScan(ctx context.Context, p *plans.RecordQueryTextIndexPlan, store *recordlayer.FDBRecordStore, eval *EvaluationContext, continuation []byte, props recordlayer.ExecuteProperties) (recordlayer.RecordCursor[QueryResult], error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scan := p.GetTextScan()
	if scan.Predicate == nil || !scan.Predicate.Type.IsText() {
		return nil, &predicates.TextComparisonError{Message: "text index plan requires a typed text comparison"}
	}
	idx := store.GetMetaData().GetIndex(p.GetIndexName())
	if idx == nil || idx.Type != recordlayer.IndexTypeText {
		return nil, &predicates.TextComparisonError{Message: "text index plan requires a TEXT index"}
	}
	if err := requireReadableQueryIndex(store, idx); err != nil {
		return nil, err
	}
	layout, err := recordlayer.DescribeTextIndex(idx)
	if err != nil {
		return nil, err
	}
	if layout.TokenizedPrimaryKey {
		return nil, &predicates.TextComparisonError{Message: "text scan cannot reconstruct a primary key trimmed into the tokenized text column"}
	}
	grouping, tokenizer, version := layout.TextPosition, layout.Tokenizer, layout.TokenizerVersion
	if len(scan.Grouping) != grouping {
		return nil, &predicates.TextComparisonError{Message: "text scan requires equality bindings for every grouping column"}
	}
	if len(scan.Suffix) > 0 && scan.Predicate.Type.IsTextPrefix() {
		// TextScan.scanTokenPrefix (TextScan.java:429-431).
		return nil, &recordlayer.RecordCoreError{Message: "text prefix comparison included inequality scan comparison", IndexName: idx.Name}
	}
	if len(scan.KeyTypes) < grouping+1+len(scan.Suffix) {
		return nil, &predicates.TextComparisonError{Message: "text scan key types do not cover its grouping and suffix columns"}
	}
	if requested := scan.Predicate.TextTokenizerName; requested != "" && requested != tokenizer.Name() {
		return nil, &predicates.TextComparisonError{Message: "text query tokenizer does not match index tokenizer"}
	}
	var comparand any
	if scan.Predicate.Operand != nil {
		if comparand, err = scan.Predicate.Operand.Evaluate(eval); err != nil {
			return nil, err
		}
	}
	comparison, err := scan.Predicate.RecordLayerTextComparison(comparand)
	if err != nil {
		return nil, err
	}
	if comparison == nil {
		// A NULL comparand matches nothing: TextComparison.eval is null.
		if len(continuation) != 0 {
			return nil, &predicates.TextComparisonError{Message: "cannot resume an empty text query from a nonempty continuation"}
		}
		return recordlayer.Empty[QueryResult](), nil
	}
	terms, err := textScanTerms(comparison, tokenizer, version)
	if err != nil {
		return nil, err
	}

	// The range set runs over the index key without its token column.
	comps := make([]*predicates.ComparisonRange, 0, grouping+len(scan.Suffix))
	for _, v := range scan.Grouping {
		comps = append(comps, predicates.EmptyComparisonRange().Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: v}).Range)
	}
	comps = append(comps, scan.Suffix...)
	keyTypes := make([]values.Type, 0, len(comps))
	keyTypes = append(keyTypes, scan.KeyTypes[:grouping]...)
	keyTypes = append(keyTypes, scan.KeyTypes[grouping+1:grouping+1+len(scan.Suffix)]...)
	// No terminal-zero widening: one range over both zero groups mixes tokens.
	spec, err := bindScanComparisonsToRangeSetWithTerminalWidening(comps, keyTypes, scanBindContext(eval), p.IsReverse(),
		textScanRuntimeSalt(p.HashCodeWithoutChildren(), tokenizer.Name(), version, terms), false)
	if err != nil {
		return nil, err
	}
	if props.ScanState == nil {
		props.ScanState = recordlayer.NewScanLimiterState()
	}
	childProps := recordlayer.NewScanProperties(props.ClearSkipAndLimit()).WithReverse(p.IsReverse())
	entries, err := newScanRangeSetCursor(spec, continuation, childProps, func(rng recordlayer.TupleRange, cont []byte, sp recordlayer.ScanProperties) (recordlayer.RecordCursor[*recordlayer.IndexEntry], error) {
		groupKey, e := textScanGroup(idx.Name, rng, grouping)
		if e != nil {
			return nil, e
		}
		cursor, e := recordlayer.NewTextScan(idx, groupKey, comparison, nil).Scan(store, cont, sp)
		if e != nil || len(scan.Suffix) == 0 {
			return cursor, e
		}
		// Caller-built suffix plans retain whole-group scanning and logical filtering.
		// Lower-level TextScan also supports logical bounds; SQL planning emits none.
		bounds := rng.ToFDBRange(subspace.FromBytes(nil))
		begin, end := bounds.Begin.FDBKey(), bounds.End.FDBKey()
		return recordlayer.FilterCursor(cursor, func(entry *recordlayer.IndexEntry) bool {
			key := make(tuple.Tuple, 0, len(entry.Key)-1)
			key = append(append(key, entry.Key[:grouping]...), entry.Key[grouping+1:]...)
			packed := key.Pack()
			return bytes.Compare(packed, begin) >= 0 && bytes.Compare(packed, end) < 0
		}), nil
	})
	if err != nil {
		return nil, err
	}
	return applySkipLimit(newIndexFetchCursor(entries, store, props.ScanState), props.Skip, props.ReturnedRowLimit), nil
}

// textScanTerms is the token list TextScan scans (TextScan.getTokenList,
// TextScan.java:111-128), stop-word slots included.
func textScanTerms(comparison *recordlayer.TextComparison, tokenizer recordlayer.TextTokenizer, version int) ([]string, error) {
	switch comparand := comparison.Comparand().(type) {
	case []string:
		return comparand, nil
	case string:
		return tokenizer.TokenizeToList(comparand, version, recordlayer.TokenizerModeQuery)
	default:
		return nil, &predicates.TextComparisonError{Message: fmt.Sprintf("comparand for text query has incompatible type: %T", comparand)}
	}
}

// textScanRuntimeSalt binds a continuation to the length-framed terms too: a
// parameter changes the token streams it positions.
func textScanRuntimeSalt(planHash uint64, tokenizer string, version int, terms []string) string {
	encoded := binary.BigEndian.AppendUint64(nil, planHash)
	encoded = binary.BigEndian.AppendUint64(encoded, uint64(version))
	for _, term := range append([]string{tokenizer}, terms...) {
		encoded = binary.BigEndian.AppendUint64(encoded, uint64(len(term)))
		encoded = append(encoded, term...)
	}
	return fmt.Sprintf("text:%x", sha256.Sum256(encoded))
}
