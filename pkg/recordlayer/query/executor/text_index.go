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

// executeTextIndexScan is RecordQueryTextIndexPlan.executeEntries
// (RecordQueryTextIndexPlan.java:96-104) followed by the record fetch: every
// posting stream, intersection, union and position filter is
// recordlayer.TextScan's.
//
// Java binds the grouping columns to one tuple. SQL equality can name several
// physical groups (a DOUBLE grouping column's -0.0 and +0.0 are distinct tuple
// elements), so the grouping equalities bind to a scan range set with one
// leaf per physical group, and each leaf runs a TextScan with that group's
// key. A leaf resumes from TextScan's own continuation.
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
	if !store.IsIndexReadable(idx.Name) {
		return nil, &recordlayer.IndexNotReadableError{IndexName: idx.Name, CurrentState: store.GetIndexState(idx.Name)}
	}
	if idx.HasFilteringPredicate() {
		return nil, &FilteredIndexPlanError{IndexName: idx.Name}
	}
	grouping := 0
	if g, ok := idx.RootExpression.(*recordlayer.GroupingKeyExpression); ok {
		grouping = g.GetGroupingCount()
	}
	for _, position := range idx.PrimaryKeyComponentPositions() {
		if position == grouping {
			return nil, &predicates.TextComparisonError{Message: "text scan cannot reconstruct a primary key trimmed into the tokenized text column"}
		}
	}
	if len(scan.Grouping) != grouping {
		return nil, &predicates.TextComparisonError{Message: "text scan requires equality bindings for every grouping column"}
	}
	if len(scan.KeyTypes) < grouping+1+len(scan.Suffix) {
		return nil, &predicates.TextComparisonError{Message: "text scan key types do not cover its grouping and suffix columns"}
	}
	tokenizer, err := recordlayer.GetTextTokenizer(idx.Options[recordlayer.IndexOptionTextTokenizerName])
	if err != nil {
		return nil, err
	}
	if requested := scan.Predicate.TextTokenizerName; requested != "" && requested != tokenizer.Name() {
		return nil, &predicates.TextComparisonError{Message: "text query tokenizer does not match index tokenizer"}
	}
	version := recordlayer.GlobalMinVersion
	if s, ok := idx.Options[recordlayer.IndexOptionTextTokenizerVersion]; ok {
		v, e := recordlayer.JavaParseInt(s)
		if e != nil {
			return nil, e
		}
		version = int(v)
	}
	typ := scan.Predicate.Type
	if len(scan.Suffix) > 0 && (typ == predicates.ComparisonTextContainsPrefix || typ == predicates.ComparisonTextContainsAnyPrefix || typ == predicates.ComparisonTextContainsAllPrefixes) {
		// TextScan.scanTokenPrefix (TextScan.java:429-431).
		return nil, &predicates.TextComparisonError{Message: "text prefix comparison included inequality scan comparison"}
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

	// The range set runs over the grouping and suffix columns: the index key
	// without its token column.
	comps := make([]*predicates.ComparisonRange, 0, grouping+len(scan.Suffix))
	for _, v := range scan.Grouping {
		comps = append(comps, predicates.EmptyComparisonRange().Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: v}).Range)
	}
	comps = append(comps, scan.Suffix...)
	keyTypes := make([]values.Type, 0, len(comps))
	keyTypes = append(keyTypes, scan.KeyTypes[:grouping]...)
	keyTypes = append(keyTypes, scan.KeyTypes[grouping+1:grouping+1+len(scan.Suffix)]...)
	// Each physical group is its own leaf: a range covering both zero groups
	// would interleave their tokens.
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
		if len(rng.Low) < grouping || len(rng.High) < grouping || !bytes.Equal(rng.Low[:grouping].Pack(), rng.High[:grouping].Pack()) {
			return nil, fmt.Errorf("executor: text scan range %v does not bind one group of %q", rng, idx.Name)
		}
		var groupKey tuple.Tuple
		if grouping > 0 {
			groupKey = append(tuple.Tuple(nil), rng.Low[:grouping]...)
		}
		cursor, e := recordlayer.NewTextScan(idx, groupKey, comparison, nil).Scan(store, cont, sp)
		if e != nil || len(scan.Suffix) == 0 {
			return cursor, e
		}
		// TextScan seeks a suffix range's start in the bunched map, whose keys
		// are each bunch's first posting, and so skips the postings of a bunch
		// that starts before the range (Java's planner never builds a suffix
		// range, TextScanPlanner.java:126). The leaf scans the whole group and
		// keeps the postings inside the range.
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

// textScanRuntimeSalt binds a continuation to the scanned tokens as well as
// the plan: a parameterized comparand changes the token streams that the
// continuation positions. Every term, stop-word slots included, is
// length-framed so that different term lists never share an identity.
func textScanRuntimeSalt(planHash uint64, tokenizer string, version int, terms []string) string {
	encoded := binary.BigEndian.AppendUint64(nil, planHash)
	encoded = binary.BigEndian.AppendUint64(encoded, uint64(version))
	for _, term := range append([]string{tokenizer}, terms...) {
		encoded = binary.BigEndian.AppendUint64(encoded, uint64(len(term)))
		encoded = append(encoded, term...)
	}
	return fmt.Sprintf("text:%x", sha256.Sum256(encoded))
}
