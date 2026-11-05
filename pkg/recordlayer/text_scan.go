// Portions derived from FoundationDB Record Layer (TextScan.java,
// RecordQueryTextIndexPlan.java, RecordQueryPlanWithIndex.java,
// TextScanPlanner.java, RecordQueryPlanner.java),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"container/heap"
	"context"
	"fmt"
	"math"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// TextScan combines TEXT token scans for a comparison, matching Java TextScan.
// GroupingKey is an equality prefix; Suffix bounds columns after the text field (nil means absent).
type TextScan struct {
	Index       *Index
	GroupingKey tuple.Tuple
	Comparison  *TextComparison
	Suffix      *TupleRange
}

// NewTextScan is TextScan(index, groupingComparisons, textComparison,
// suffixComparisons) (TextScan.java:99-107) over evaluated comparisons.
func NewTextScan(index *Index, groupingKey tuple.Tuple, comparison *TextComparison, suffix *TupleRange) *TextScan {
	return &TextScan{Index: index, GroupingKey: groupingKey, Comparison: comparison, Suffix: suffix}
}

// String is TextScan.toString (TextScan.java:525-527).
func (s *TextScan) String() string {
	suffix := "null"
	if s.Suffix != nil {
		suffix = fmt.Sprint(*s.Suffix)
	}
	grouping := "null"
	if s.GroupingKey != nil {
		grouping = fmt.Sprint(s.GroupingKey)
	}
	return fmt.Sprintf("TextScan(%s %s, %s, %s)", s.Index.Name, grouping, s.Comparison, suffix)
}

// CreatesDuplicates detects repeated records from matching prefix tokens
// or fanned-out values (TextScan.java:467-472).
func (s *TextScan) CreatesDuplicates() bool {
	return !s.Comparison.Type().IsEquality() || createsDuplicatesRec(s.Index.RootExpression, true)
}

// tokenList uses supplied tokens or QUERY-mode tokenization at the index’s tokenizer version
// (TextScan.java:111-128).
func (s *TextScan) tokenList(removeStopWords bool) ([]string, error) {
	var tokens []string
	switch comparand := s.Comparison.Comparand().(type) {
	case []string:
		tokens = comparand
	case string:
		tokenizer, err := getTextTokenizer(s.Index)
		if err != nil {
			return nil, err
		}
		version, err := getTextTokenizerVersion(s.Index)
		if err != nil {
			return nil, err
		}
		if tokens, err = tokenizer.TokenizeToList(comparand, version, TokenizerModeQuery); err != nil {
			return nil, err
		}
	default:
		return nil, &RecordCoreError{Message: fmt.Sprintf("Comparand for text query of incompatible type: %T", comparand), IndexName: s.Index.Name}
	}
	if removeStopWords {
		kept := tokens[:0:0]
		for _, t := range tokens {
			if t != "" {
				kept = append(kept, t)
			}
		}
		tokens = kept
	}
	return tokens, nil
}

// suffixComparisonKey identifies a record by all columns after the group and token
// (TextScan.java:142-148).
func suffixComparisonKey(firstEntries int) ComparisonKeyFunc[*IndexEntry] {
	return func(e *IndexEntry) (tuple.Tuple, error) {
		if firstEntries > len(e.Key) {
			return nil, &RecordCoreError{Message: fmt.Sprintf("text index entry key %v is shorter than its %d prefix columns", e.Key, firstEntries)}
		}
		return e.Key[firstEntries:], nil
	}
}

// Scan returns matching index entries; multi-token scans resume from the combining
// merge cursor’s Java-compatible continuation (TextScan.java:160-169).
//
// ALL, PHRASE and ALL_WITHIN intersections seek within loaded bunches or reopen only when needed.
// See DIVERGENCES.md, "TEXT intersections seek"; scan limiters and timers report read costs.
func (s *TextScan) Scan(store *FDBRecordStore, continuation []byte, props ScanProperties) (RecordCursor[*IndexEntry], error) {
	if props.ExecuteProperties.ScanState == nil {
		// Every token scan charges one budget, as Java's children share ExecuteState.
		props.ExecuteProperties.ScanState = NewScanLimiterState()
	}
	tokens, err := s.tokenList(true)
	if err != nil {
		return nil, err
	}
	return s.scan(store, s.GroupingKey, s.Suffix, tokens, continuation, props)
}

// scan is TextScan.scan(store, context, prefix, suffix, index, tokenList,
// continuation, scanProperties) (TextScan.java:173-262).
func (s *TextScan) scan(store *FDBRecordStore, prefix tuple.Tuple, suffix *TupleRange, tokens []string, continuation []byte, props ScanProperties) (RecordCursor[*IndexEntry], error) {
	if len(tokens) == 0 {
		return Empty[*IndexEntry](), nil
	}
	prefixEntries := 1 + len(prefix)
	keyFunc := suffixComparisonKey(prefixEntries)
	exec := props.ExecuteProperties
	skipLimit := func(c RecordCursor[*IndexEntry]) RecordCursor[*IndexEntry] {
		return LimitRowsCursor(SkipCursor(c, exec.Skip), exec.ReturnedRowLimit)
	}
	typ := s.Comparison.Type()

	switch {
	case typ == TextContainsPrefix || (len(tokens) == 1 && (typ == TextContainsAllPrefixes || typ == TextContainsAnyPrefix)):
		if len(tokens) != 1 {
			return nil, &RecordCoreError{Message: fmt.Sprintf("text prefix comparison included %d comparands instead of one", len(tokens)), IndexName: s.Index.Name}
		}
		fn, err := s.scanTokenPrefix(store, tokens[0], prefix, suffix, props)
		if err != nil {
			return nil, err
		}
		return fn(continuation), nil
	case len(tokens) == 1:
		// Intersection and union need two children, so every other single-token
		// comparison is this one range scan.
		return s.scanToken(store, tokens[0], prefix, suffix, props).cursor(continuation), nil
	case typ == TextContainsAll:
		child := props.WithExecuteProperties(exec.ClearSkipAndLimit())
		children, err := s.intersectionChildren(store, tokens, prefix, suffix, child, prefixEntries, continuation)
		if err != nil {
			return nil, err
		}
		return skipLimit(intersectionSeeking(children.cursors, children.seekers, keyFunc, props.IsReverse(), children.resume)), nil
	case typ == TextContainsAllPrefixes:
		child := props.WithExecuteProperties(exec.ClearSkipAndLimit())
		fns := make([]CursorFunction[*IndexEntry], len(tokens))
		for i, token := range tokens {
			fn, err := s.scanTokenPrefix(store, token, prefix, suffix, child)
			if err != nil {
				return nil, err
			}
			fns[i] = fn
		}
		cursor, err := ProbableIntersection(keyFunc, fns, s.Comparison.ExpectedRecords(), s.Comparison.FalsePositivePercentage(), continuation)
		if err != nil {
			return nil, err
		}
		return skipLimit(cursor), nil
	case typ == TextContainsAny:
		// The children keep a row limit raised by the skip, which the union
		// applies again (TextScan.java:220-229).
		child := props.WithExecuteProperties(exec.ClearSkipAndAdjustLimit())
		fns := make([]CursorFunction[*IndexEntry], len(tokens))
		for i, token := range tokens {
			fns[i] = s.scanToken(store, token, prefix, suffix, child).cursor
		}
		cursor, err := Union(keyFunc, props.IsReverse(), fns, continuation)
		if err != nil {
			return nil, err
		}
		return skipLimit(cursor), nil
	case typ == TextContainsAnyPrefix:
		child := props.WithExecuteProperties(exec.ClearSkipAndAdjustLimit())
		fns := make([]CursorFunction[*IndexEntry], len(tokens))
		for i, token := range tokens {
			fn, err := s.scanTokenPrefix(store, token, prefix, suffix, child)
			if err != nil {
				return nil, err
			}
			fns[i] = fn
		}
		cursor, err := UnorderedUnion(fns, continuation)
		if err != nil {
			return nil, err
		}
		return skipLimit(cursor), nil
	}

	// TEXT_CONTAINS_ALL_WITHIN and TEXT_CONTAINS_PHRASE intersect every token
	// scan and filter the matches on their position lists.
	var predicate func([]*IndexEntry) (bool, error)
	switch typ {
	case TextContainsAllWithin:
		maxDistance := s.Comparison.MaxDistance()
		predicate = func(entries []*IndexEntry) (bool, error) { return entriesContainAllWithin(entries, maxDistance) }
	case TextContainsPhrase:
		withStopWords, err := s.tokenList(false)
		if err != nil {
			return nil, err
		}
		predicate = func(entries []*IndexEntry) (bool, error) { return entriesContainPhrase(entries, withStopWords) }
	default:
		return nil, &RecordCoreError{Message: fmt.Sprintf("unsupported comparison type for text query: %s", typ), IndexName: s.Index.Name}
	}
	child := props.WithExecuteProperties(exec.ClearSkipAndLimit())
	children, err := s.intersectionChildren(store, tokens, prefix, suffix, child, prefixEntries, continuation)
	if err != nil {
		return nil, err
	}
	multi := intersectionMultiSeeking(children.cursors, children.seekers, keyFunc, props.IsReverse(), children.resume)
	return skipLimit(firstOfMatches(FilterErrCursor(multi, predicate))), nil
}

// firstOfMatches is the .map(indexEntries -> indexEntries.get(0)) of
// TextScan.java:258.
func firstOfMatches(c RecordCursor[[]*IndexEntry]) RecordCursor[*IndexEntry] {
	return MapCursor(c, func(entries []*IndexEntry) *IndexEntry { return entries[0] })
}

// textIntersectionChildren are the token scans of an intersection.
type textIntersectionChildren struct {
	cursors []RecordCursor[*IndexEntry]
	seekers []childSeeker
	resume  []IntersectionChildResume
}

// intersectionChildren restores one token scan per Java intersection child continuation (END is empty).
// A child can seek when its comparison key is its entire key within the token map.
func (s *TextScan) intersectionChildren(store *FDBRecordStore, tokens []string, prefix tuple.Tuple, suffix *TupleRange, props ScanProperties, prefixEntries int, continuation []byte) (*textIntersectionChildren, error) {
	if len(tokens) < 2 {
		return nil, &RecordCoreArgumentError{Message: fmt.Sprintf("not enough child cursors provided to IntersectionCursor: %d", len(tokens))}
	}
	resume, err := DecodeIntersectionContinuation(continuation, len(tokens))
	if err != nil {
		return nil, &RecordCoreError{Message: "invalid continuation", Cause: err}
	}
	canSeek := prefixEntries == textFieldPosition(s.Index.RootExpression)+1
	cursors := make([]RecordCursor[*IndexEntry], len(tokens))
	seekers := make([]childSeeker, len(tokens))
	for i, token := range tokens {
		r := resume[i]
		if r.Started && len(r.Continuation) == 0 {
			cursors[i] = Empty[*IndexEntry]()
			continue
		}
		var childCont []byte
		if r.Started {
			childCont = r.Continuation
		}
		scan := s.scanToken(store, token, prefix, suffix, props)
		cursor, text := scan.open(childCont)
		cursors[i] = cursor
		if canSeek && text != nil {
			seekers[i] = text.seekTo
		}
	}
	return &textIntersectionChildren{cursors: cursors, seekers: seekers, resume: resume}, nil
}

// tokenScan reads one token under the grouping prefix and optional suffix bounds
// (TextScan.java:442-457).
type tokenScan struct {
	store     *FDBRecordStore
	index     *Index
	scanRange TupleRange
	props     ScanProperties
}

func (s *TextScan) scanToken(store *FDBRecordStore, token string, prefix tuple.Tuple, suffix *TupleRange, props ScanProperties) tokenScan {
	var scanRange TupleRange
	if suffix != nil {
		scanRange = suffix.Prepend(tuple.Tuple{token})
	} else {
		scanRange = TupleRangeAllOf(tuple.Tuple{token})
	}
	if prefix != nil {
		scanRange = scanRange.Prepend(prefix)
	}
	return tokenScan{store: store, index: s.Index, scanRange: scanRange, props: props}
}

func (t tokenScan) cursor(continuation []byte) RecordCursor[*IndexEntry] {
	c, _ := t.open(continuation)
	return c
}

func (t tokenScan) open(continuation []byte) (RecordCursor[*IndexEntry], *textCursor) {
	return t.store.scanTextIndex(t.index, t.scanRange, continuation, t.props)
}

// scanTokenPrefix scans matching tokens; suffix bounds are refused as a second inequality
// (TextScan.java:426-440).
func (s *TextScan) scanTokenPrefix(store *FDBRecordStore, token string, prefix tuple.Tuple, suffix *TupleRange, props ScanProperties) (CursorFunction[*IndexEntry], error) {
	if suffix != nil {
		return nil, &RecordCoreError{Message: "text prefix comparison included inequality scan comparison", IndexName: s.Index.Name}
	}
	scanRange := TupleRangePrefixString(token)
	if prefix != nil {
		scanRange = scanRange.Prepend(prefix)
	}
	t := tokenScan{store: store, index: s.Index, scanRange: scanRange, props: props}
	return t.cursor, nil
}

// positionList is one entry of TextScan.getPositionsLists (TextScan.java:
// 264-272): the entry value's first element.
func positionList(e *IndexEntry) ([]int64, error) {
	if len(e.Value) == 0 {
		return nil, &RecordCoreError{Message: fmt.Sprintf("text index entry %v has no position list", e.Key)}
	}
	list, ok := e.Value[0].(tuple.Tuple)
	if !ok {
		return nil, &ClassCastError{Message: fmt.Sprintf("text index entry position list is %T, not a list", e.Value[0])}
	}
	out := make([]int64, len(list))
	for i, v := range list {
		n, ok := v.(int64)
		if !ok {
			return nil, &ClassCastError{Message: fmt.Sprintf("text index entry position is %T, not an integer", v)}
		}
		out[i] = n
	}
	return out, nil
}

func positionLists(entries []*IndexEntry) ([][]int64, error) {
	lists := make([][]int64, len(entries))
	for i, e := range entries {
		l, err := positionList(e)
		if err != nil {
			return nil, err
		}
		lists[i] = l
	}
	return lists, nil
}

// positionHeap is the min-queue of TextScan.entriesContainAllWithin.
type positionHead struct {
	value int64
	rest  []int64
}
type positionHeap []positionHead

func (h positionHeap) Len() int           { return len(h) }
func (h positionHeap) Less(i, j int) bool { return h[i].value < h[j].value }
func (h positionHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *positionHeap) Push(x any)        { *h = append(*h, x.(positionHead)) }
func (h *positionHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

// entriesContainAllWithin requires every token in one maxDistance window;
// tokens without position lists count as close (TextScan.java:274-322).
func entriesContainAllWithin(entries []*IndexEntry, maxDistance int) (bool, error) {
	if len(entries) == 0 {
		return false, nil
	}
	lists, err := positionLists(entries)
	if err != nil {
		return false, err
	}
	nonEmpty := lists[:0:0]
	for _, l := range lists {
		if len(l) > 0 {
			nonEmpty = append(nonEmpty, l)
		}
	}
	if len(nonEmpty) == 0 {
		return true, nil
	}
	h := make(positionHeap, 0, len(nonEmpty))
	max := int64(math.MinInt32)
	for _, l := range nonEmpty {
		if l[0] > max {
			max = l[0]
		}
		h = append(h, positionHead{value: l[0], rest: l[1:]})
	}
	heap.Init(&h)
	for {
		minElem := heap.Pop(&h).(positionHead)
		if max-minElem.value <= int64(maxDistance) {
			return true, nil
		}
		if len(minElem.rest) == 0 {
			return false, nil
		}
		next := minElem.rest[0]
		if next > max {
			max = next
		}
		heap.Push(&h, positionHead{value: next, rest: minElem.rest[1:]})
	}
}

// positionListsAndDeltas widens expected token gaps for stop words and missing positions
// (TextScan.java:324-359).
func positionListsAndDeltas(entries []*IndexEntry, tokensWithStopWords []string) ([][]int64, []int64, error) {
	lists, err := positionLists(entries)
	if err != nil {
		return nil, nil, err
	}
	hasStopOrEmpty := false
	for _, t := range tokensWithStopWords {
		if t == "" {
			hasStopOrEmpty = true
		}
	}
	for _, l := range lists {
		if len(l) == 0 {
			hasStopOrEmpty = true
		}
	}
	var deltas []int64
	if !hasStopOrEmpty {
		for range lists {
			deltas = append(deltas, 1)
		}
		return lists, deltas, nil
	}
	var kept [][]int64
	next := 0
	current := int64(1)
	for _, t := range tokensWithStopWords {
		if t == "" {
			current++
			continue
		}
		if next >= len(lists) {
			return nil, nil, &RecordCoreError{Message: "phrase has more tokens than intersected entries"}
		}
		l := lists[next]
		next++
		if len(l) == 0 {
			current++
		} else {
			kept = append(kept, l)
			deltas = append(deltas, current)
			current = 1
		}
	}
	return kept, deltas, nil
}

// entriesContainPhrase is TextScan.entriesContainPhrase (TextScan.java:
// 361-424): one position per token at the expected offsets.
func entriesContainPhrase(entries []*IndexEntry, tokensWithStopWords []string) (bool, error) {
	if len(entries) == 0 {
		return false, nil
	}
	lists, deltas, err := positionListsAndDeltas(entries, tokensWithStopWords)
	if err != nil {
		return false, err
	}
	if len(lists) == 0 {
		return true, nil
	}
	current := make([]int64, len(lists))
	next := make([]int, len(lists)) // index of the next unread position
	for i, l := range lists {
		current[i] = l[0]
		next[i] = 1
	}
	for {
		expected := current[0]
		allMatched := true
		for i := 1; i < len(current); i++ {
			expected += deltas[i]
			value := current[i]
			for value < expected && next[i] < len(lists[i]) {
				value = lists[i][next[i]]
				next[i]++
			}
			if value < expected {
				return false, nil
			}
			current[i] = value
			if value > expected {
				allMatched = false
				break
			}
		}
		if allMatched {
			return true, nil
		}
		if next[0] >= len(lists[0]) {
			return false, nil
		}
		current[0] = lists[0][next[0]]
		next[0]++
	}
}

// TextIndexPlan is Java's RecordQueryTextIndexPlan: a text scan of one index,
// whose records are fetched by primary key.
type TextIndexPlan struct {
	IndexName string
	TextScan  *TextScan
	Reverse   bool
}

// NewTextIndexPlan is RecordQueryTextIndexPlan(indexName, textScan, reverse)
// (RecordQueryTextIndexPlan.java:81-86).
func NewTextIndexPlan(indexName string, textScan *TextScan, reverse bool) *TextIndexPlan {
	return &TextIndexPlan{IndexName: indexName, TextScan: textScan, Reverse: reverse}
}

// ExecuteEntries applies plan direction and execute properties to the text scan
// (RecordQueryTextIndexPlan.java:90-94).
func (p *TextIndexPlan) ExecuteEntries(store *FDBRecordStore, continuation []byte, props ExecuteProperties) (RecordCursor[*IndexEntry], error) {
	return p.TextScan.Scan(store, continuation, NewScanProperties(props).WithReverse(p.Reverse))
}

// Execute fetches entries by primary key and rejects orphans, matching Java’s
// RecordQueryPlanWithIndex.executePlan with FetchIndexRecords.PRIMARY_KEY.
func (p *TextIndexPlan) Execute(store *FDBRecordStore, continuation []byte, props ExecuteProperties) (RecordCursor[*FDBIndexedRecord], error) {
	if props.ScanState == nil {
		props.ScanState = NewScanLimiterState()
	}
	entries, err := p.ExecuteEntries(store, continuation, props)
	if err != nil {
		return nil, err
	}
	return store.FetchIndexRecords(entries, IndexOrphanError, props.ScanState), nil
}

// String is the plan's explain form.
func (p *TextIndexPlan) String() string {
	dir := ""
	if p.Reverse {
		dir = " REVERSE"
	}
	return fmt.Sprintf("TextIndexScan(%s, %s%s)", p.IndexName, p.TextScan.Comparison, dir)
}

// FetchIndexRecords loads each entry’s record by primary key and charges scanState.
func (store *FDBRecordStore) FetchIndexRecords(entries RecordCursor[*IndexEntry], orphan IndexOrphanBehavior, scanState *ScanLimiterState) RecordCursor[*FDBIndexedRecord] {
	if scanState == nil {
		scanState = NewScanLimiterState()
	}
	loader := &indexRecordLoader{store: store, scanState: scanState, orphan: orphan}
	cursor := MapPipelined(entries, loader.load, store.GetPipelineSize(PipelineOperationIndexToRecord))
	if orphan == IndexOrphanSkip {
		cursor = FilterCursor(cursor, func(record *FDBIndexedRecord) bool { return record != nil })
	}
	return cursor
}

// scanTextIndex also returns the TEXT cursor for leapfrog seeks; it is nil
// when the scan cannot start and the returned cursor will fail.
func (store *FDBRecordStore) scanTextIndex(index *Index, scanRange TupleRange, continuation []byte, props ScanProperties) (RecordCursor[*IndexEntry], *textCursor) {
	tm, err := store.textIndexMaintainer(index)
	if err != nil {
		return &errorCursor[*IndexEntry]{err: err}, nil
	}
	cursor, text := tm.scanText(scanRange, continuation, props)
	if props.ExecuteProperties.Skip > 0 {
		text = nil // the skip cursor holds entries the text cursor has passed
	}
	return instrumentCursor(store.context.Timer(), EventScanIndex, cursor), text
}

// textIndexMaintainer is the readable-index check and maintainer lookup of
// FDBRecordStore.scanIndex for a BY_TEXT_TOKEN scan.
func (store *FDBRecordStore) textIndexMaintainer(index *Index) (*textIndexMaintainer, error) {
	state, err := store.readIndexState(index.Name)
	if err != nil {
		return nil, err
	}
	if !state.IsScannable() {
		return nil, &IndexNotReadableError{IndexName: index.Name, CurrentState: state}
	}
	maintainer, err := store.getIndexMaintainer(index)
	if err != nil {
		return nil, err
	}
	tm, ok := maintainer.(*textIndexMaintainer)
	if !ok {
		return nil, &RecordCoreError{Message: fmt.Sprintf("index %q (type %s) does not support BY_TEXT_TOKEN scan", index.Name, index.Type), IndexName: index.Name}
	}
	return tm, nil
}

// TextQuery supplies record types, equality values for grouping columns and a text comparison
// to the old planner’s text-index path (RecordQueryPlanner.java:1702-1734).
type TextQuery struct {
	RecordTypes []string
	GroupingKey tuple.Tuple
	Comparison  *TextComparison
	// Reverse is the query's sort direction (no sort is allowed).
	Reverse bool
	// KeepDuplicates is RecordQuery.setRemoveDuplicates(false).
	KeepDuplicates bool
}

// TextQueryPlanError reports that an index cannot satisfy a text query,
// where Java's planText returns no plan for the index.
type TextQueryPlanError struct {
	IndexName string
	Reason    string
}

func (e *TextQueryPlanError) Error() string {
	return fmt.Sprintf("text index %q cannot satisfy the query: %s", e.IndexName, e.Reason)
}

// TextQueryPlan wraps a text scan with type filtering, strict ALL_PREFIXES rechecks
// and primary-key deduplication where required.
type TextQueryPlan struct {
	TextPlan *TextIndexPlan
	// TypeFilter, when non-empty, is RecordQueryTypeFilterPlan's record types.
	TypeFilter []string
	// StrictFilter is the RecordQueryFilterPlan of a strict all-prefixes query.
	StrictFilter bool
	// RemovesDuplicates is RecordQueryUnorderedPrimaryKeyDistinctPlan.
	RemovesDuplicates bool
}

// PlanTextQuery matches Java planText: grouping equalities AND the text comparison,
// wrapped with duplicate removal when needed (RecordQueryPlanner.java:1702-1734, 1106-1117).
func PlanTextQuery(md *RecordMetaData, index *Index, q TextQuery) (*TextQueryPlan, error) {
	if md == nil || index == nil {
		return nil, &TextQueryPlanError{Reason: "metadata and index are required"}
	}
	if canonicalIndexType(index.Type) != IndexTypeText {
		return nil, &TextQueryPlanError{IndexName: index.Name, Reason: fmt.Sprintf("index type %s is not a text index", index.Type)}
	}
	if q.Comparison == nil {
		return nil, &TextQueryPlanError{IndexName: index.Name, Reason: "no text comparison"}
	}
	// TextScanPlanner.getScanForQuery (TextScanPlanner.java:214-243): every
	// grouping column must be matched by an equality.
	var grouping tuple.Tuple
	if g, ok := index.RootExpression.(*GroupingKeyExpression); ok {
		if len(q.GroupingKey) != g.GetGroupingCount() {
			return nil, &TextQueryPlanError{IndexName: index.Name, Reason: fmt.Sprintf("%d grouping values for %d grouping columns", len(q.GroupingKey), g.GetGroupingCount())}
		}
		grouping = q.GroupingKey
	} else if len(q.GroupingKey) > 0 {
		return nil, &TextQueryPlanError{IndexName: index.Name, Reason: "index has no grouping columns"}
	}
	if !textMatchesTokenizer(q.Comparison, index) {
		return nil, &TextQueryPlanError{IndexName: index.Name, Reason: fmt.Sprintf("comparison requires tokenizer %q", q.Comparison.TokenizerName())}
	}
	if !textContainsPositionsIfNecessary(q.Comparison, index) {
		return nil, &TextQueryPlanError{IndexName: index.Name, Reason: fmt.Sprintf("%s needs position lists the index omits", q.Comparison.Type())}
	}
	scan := NewTextScan(index, grouping, q.Comparison, nil)
	plan := &TextQueryPlan{TextPlan: NewTextIndexPlan(index.Name, scan, q.Reverse)}

	// addTypeFilterIfNeeded: the index's types the query does not ask for. A
	// query without record types asks for all of them.
	queried := map[string]bool{}
	for _, name := range q.RecordTypes {
		queried[name] = true
	}
	covered := map[string]bool{}
	for _, rt := range md.RecordTypesForIndex(index) {
		covered[rt.Name] = true
	}
	requested := append([]string(nil), q.RecordTypes...)
	if len(requested) == 0 {
		for name := range md.RecordTypes() {
			requested = append(requested, name)
		}
	}
	for _, name := range requested {
		if !covered[name] {
			return nil, &TextQueryPlanError{IndexName: index.Name, Reason: fmt.Sprintf("index does not cover requested record type %q", name)}
		}
	}
	for _, rt := range md.RecordTypesForIndex(index) {
		if len(q.RecordTypes) > 0 && !queried[rt.Name] {
			plan.TypeFilter = append([]string{}, q.RecordTypes...)
			break
		}
	}
	if q.Comparison.Type() == TextContainsAllPrefixes && q.Comparison.IsStrict() {
		plan.StrictFilter = true
	}
	if scan.CreatesDuplicates() && !q.KeepDuplicates {
		common, err := textQueryCommonPrimaryKey(md, q.RecordTypes)
		if err != nil {
			return nil, err
		}
		if common == nil {
			return nil, &RecordCoreError{Message: "A common primary key is required to remove duplicates", IndexName: index.Name}
		}
		plan.RemovesDuplicates = true
	}
	return plan, nil
}

// textQueryCommonPrimaryKey returns the shared primary key of queried types (all types if unnamed),
// or nil (RecordQueryPlanner.java:1064-1092).
func textQueryCommonPrimaryKey(md *RecordMetaData, recordTypes []string) (KeyExpression, error) {
	if len(recordTypes) == 0 {
		return md.CommonPrimaryKey(), nil
	}
	var common KeyExpression
	for i, name := range recordTypes {
		rt := md.GetRecordType(name)
		if rt == nil {
			return nil, &MetaDataError{Message: fmt.Sprintf("Unknown record type %s", name)}
		}
		if i == 0 {
			common = rt.PrimaryKey
		} else if !keyExpressionEquals(common, rt.PrimaryKey) {
			return nil, nil
		}
	}
	return common, nil
}

// textMatchesTokenizer is TextScanPlanner.matchesTokenizer
// (TextScanPlanner.java:73-83).
func textMatchesTokenizer(c *TextComparison, index *Index) bool {
	if c.TokenizerName() == "" {
		return true
	}
	name := index.Options[IndexOptionTextTokenizerName]
	if name == "" {
		name = DefaultTextTokenizerName
	}
	return c.TokenizerName() == name
}

// textContainsPositionsIfNecessary is
// TextScanPlanner.containsPositionsIfNecessary (TextScanPlanner.java:98-100).
func textContainsPositionsIfNecessary(c *TextComparison, index *Index) bool {
	switch c.Type() {
	case TextContainsAll, TextContainsAny, TextContainsPrefix:
		return true
	}
	return !getTextOmitPositions(index)
}

// Execute applies distinct/filter/type-filter wrappers without child skip/limit,
// then applies skip/limit to their output, as Java’s corresponding plans do.
func (p *TextQueryPlan) Execute(store *FDBRecordStore, continuation []byte, props ExecuteProperties) (RecordCursor[*FDBIndexedRecord], error) {
	wrapped := len(p.TypeFilter) > 0 || p.StrictFilter || p.RemovesDuplicates
	inner := props
	if wrapped {
		inner = props.ClearSkipAndLimit()
	}
	cursor, err := p.TextPlan.Execute(store, continuation, inner)
	if err != nil {
		return nil, err
	}
	if len(p.TypeFilter) > 0 {
		types := map[string]bool{}
		for _, name := range p.TypeFilter {
			types[name] = true
		}
		cursor = FilterCursor(cursor, func(r *FDBIndexedRecord) bool {
			return r.Record.RecordType != nil && types[r.Record.RecordType.Name]
		})
	}
	if p.StrictFilter {
		comparison := p.TextPlan.TextScan.Comparison
		index := p.TextPlan.TextScan.Index
		cursor = FilterErrCursor(cursor, func(r *FDBIndexedRecord) (bool, error) {
			return recordSatisfiesText(index, comparison, r)
		})
	}
	if p.RemovesDuplicates {
		seen := map[string]bool{}
		cursor = FilterCursor(cursor, func(r *FDBIndexedRecord) bool {
			key := string(r.Record.PrimaryKey.Pack())
			if seen[key] {
				return false
			}
			seen[key] = true
			return true
		})
	}
	if wrapped {
		cursor = SkipThenLimit(cursor, props.Skip, props.ReturnedRowLimit)
	}
	return cursor, nil
}

// recordSatisfiesText rechecks the indexed field; for a fanned-out field,
// any value satisfying the comparison is enough.
func recordSatisfiesText(index *Index, c *TextComparison, r *FDBIndexedRecord) (bool, error) {
	values, err := index.RootExpression.Evaluate(r.Record, r.Record.Record)
	if err != nil {
		return false, err
	}
	pos := textFieldPosition(index.RootExpression)
	for _, v := range values {
		if pos >= len(v) {
			continue
		}
		ok, err := c.Eval(v[pos])
		if err != nil {
			return false, err
		}
		if ok != nil && *ok {
			return true, nil
		}
	}
	return false, nil
}

// errFilterCursor is FilterCursor with a predicate that can fail.
type errFilterCursor[T any] struct {
	inner      RecordCursor[T]
	predicate  func(T) (bool, error)
	lastNoNext *RecordCursorResult[T]
}

// FilterErrCursor is Java's RecordCursor.filter with a predicate whose
// failure ends the cursor with that error.
func FilterErrCursor[T any](inner RecordCursor[T], predicate func(T) (bool, error)) RecordCursor[T] {
	return &errFilterCursor[T]{inner: inner, predicate: predicate}
}

func (c *errFilterCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[T], error) {
	if c.lastNoNext != nil {
		return *c.lastNoNext, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return RecordCursorResult[T]{}, err
		}
		result, err := c.inner.OnNext(ctx)
		if err != nil {
			return result, err
		}
		if !result.HasNext() {
			c.lastNoNext = &result
			return result, nil
		}
		ok, err := c.predicate(result.GetValue())
		if err != nil {
			return RecordCursorResult[T]{}, err
		}
		if ok {
			return result, nil
		}
	}
}

func (c *errFilterCursor[T]) Close() error   { return c.inner.Close() }
func (c *errFilterCursor[T]) IsClosed() bool { return c.inner.IsClosed() }
