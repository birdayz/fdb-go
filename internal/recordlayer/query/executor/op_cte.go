// Portions derived from FoundationDB Record Layer (MessageHelpers.java,
// QueryResult.java, IndexOrphanBehavior.java, RecordCursor.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2021 Apple Inc. and the FoundationDB project authors
// Copyright 2015-2022 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package executor

import (
	"context"
	"fmt"
	"sort"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// executeRecursiveLevelUnion implements level-order (BFS) recursive
// CTE execution. Two temp tables ping-pong between read and write
// roles: the initial plan seeds level 0 into the insert table, then
// buffers flip and the recursive plan reads from scan and writes to
// insert, repeating until a level produces zero rows.
// Mirrors Java's RecordQueryRecursiveLevelUnionPlan.executePlan.
func executeRecursiveLevelUnion(
	ctx context.Context,
	p *plans.RecordQueryRecursiveLevelUnionPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	scanAlias := p.GetTempTableScanAlias()
	insertAlias := p.GetTempTableInsertAlias()

	scanTable := NewTempTableWithState(props.State)
	insertTable := NewTempTableWithState(props.State)
	// Live-bytes model: every byte the recursion charges (ping-pong temp
	// tables via TempTableInsert, the UNION-DISTINCT seen-set) stands in for
	// the accumulated result set and is released in one place — at the final
	// cursor's teardown on success, or right here on an error return.
	var seenCharge func() int64 = func() int64 { return 0 }
	releaseWorkingSet := func() {
		scanTable.ReleaseCharges()
		insertTable.ReleaseCharges()
		props.State.ReleaseMemory(seenCharge())
	}
	handedOff := false
	defer func() {
		if !handedOff {
			releaseWorkingSet()
		}
	}()

	levelCtx := evalCtx.WithBinding(scanAlias, scanTable)
	levelCtx = levelCtx.WithBinding(insertAlias, insertTable)

	initialCursor, err := ExecutePlan(ctx, p.GetInitialState(), store, levelCtx, continuation, props.ClearSkipAndLimit())
	if err != nil {
		return nil, fmt.Errorf("executor: recursive level union initial: %w", err)
	}

	var allResults []QueryResult
	items, err := collectAllRowCapped(ctx, initialCursor, props.GetMaterializationLimit(), "recursive CTE initial state")
	initialCursor.Close()
	if err != nil {
		return nil, fmt.Errorf("executor: recursive level union initial collect: %w", err)
	}

	// UNION DISTINCT: track seen rows via a string key to detect
	// and filter duplicates (cycle detection on cyclic graphs).
	// Extract canonical column names from the seed plan so the dedup
	// key only considers CTE-relevant columns (ignoring extra join
	// columns the recursive branch may carry in its rows).
	distinct := p.IsDistinct()
	// RFC-130: the UNION-DISTINCT seen-set is a cross-level cardinality-growing
	// buffer (one key per distinct row, held across all levels) — charge each
	// NEW key via boundedSet.
	var seen *boundedSet[string]
	var keyer *cteDedupKeyer
	if distinct {
		var derr error
		seen, keyer, items, derr = initRecursiveLevelDistinct(p, props.State, items)
		if seen != nil {
			seenCharge = seen.Charged
		}
		if derr != nil {
			return nil, derr
		}
	}
	allResults = append(allResults, items...)

	const maxRecursionDepth = 1000
	for level := 0; ; level++ {
		if len(insertTable.GetList()) == 0 {
			break
		}
		if level >= maxRecursionDepth {
			return nil, &RecursiveCTEDepthExceededError{MaxDepth: maxRecursionDepth}
		}

		scanTable, insertTable = insertTable, scanTable
		insertTable.Clear()

		levelCtx = evalCtx.WithBinding(scanAlias, scanTable)
		levelCtx = levelCtx.WithBinding(insertAlias, insertTable)

		recursiveCursor, err := ExecutePlan(ctx, p.GetRecursiveState(), store, levelCtx, nil, props.ClearSkipAndLimit())
		if err != nil {
			return nil, fmt.Errorf("executor: recursive level union recursive: %w", err)
		}
		items, err := collectAllRowCapped(ctx, recursiveCursor, props.GetMaterializationLimit(), "recursive CTE recursive level")
		recursiveCursor.Close()
		if err != nil {
			return nil, fmt.Errorf("executor: recursive level union recursive collect: %w", err)
		}
		if distinct {
			var derr error
			items, derr = dedupRecursiveLevelItems(items, seen, keyer)
			if derr != nil {
				return nil, derr
			}
			// Also replace insertTable contents with only the new
			// (non-duplicate) rows so the next level's scan sees only
			// genuinely new rows. These rows were already charged when the
			// recursive plan's TempTableInsertPlan added them this level, so
			// ReplaceList swaps without re-charging (RFC-130).
			insertTable.ReplaceList(items)
		}
		allResults = append(allResults, items...)
	}

	handedOff = true
	return newCloseHookCursor(
		applySkipLimit(recordlayer.FromList(allResults), props.Skip, props.ReturnedRowLimit),
		releaseWorkingSet,
	), nil
}

func dedupRecursiveLevelItems(
	items []QueryResult,
	seen *boundedSet[string],
	keyer *cteDedupKeyer,
) ([]QueryResult, error) {
	var deduped []QueryResult
	for _, it := range items {
		k, kerr := keyer.key(it)
		if kerr != nil {
			return nil, kerr
		}
		added, err := seen.Add(k, int64(len(k)))
		if err != nil {
			return nil, err
		}
		if !added {
			continue
		}
		deduped = append(deduped, it)
	}
	return deduped, nil
}

func initRecursiveLevelDistinct(
	p *plans.RecordQueryRecursiveLevelUnionPlan,
	state *recordlayer.ExecuteState,
	items []QueryResult,
) (*boundedSet[string], *cteDedupKeyer, []QueryResult, error) {
	seen := newBoundedSet[string](state)
	// Dedup on the CTE's OUTPUT columns, the seed plan's result row; the
	// first row's layout is the fallback for a seed whose result is not a
	// record.
	canonicalCols := recursiveUnionOutputColumns(p.GetInitialState())
	if len(canonicalCols) == 0 && len(items) > 0 && items[0].Positional != nil {
		// Positional column order is already deterministic (ordinal order),
		// so no sort is needed.
		canonicalCols = items[0].Positional.TypeNames()
	}
	keyer := newCTEDedupKeyer(canonicalCols)
	deduped, err := dedupRecursiveLevelItems(items, seen, keyer)
	if err != nil {
		return seen, keyer, nil, err
	}
	return seen, keyer, deduped, nil
}

// executeRecursiveDfsJoin implements depth-first recursive CTE
// execution. The root plan seeds the traversal; for each row, the
// child plan is re-evaluated with the prior row bound via
// priorCorrelation. Supports PREORDER (emit parent then children)
// and POSTORDER (emit children then parent).
// Mirrors Java's RecordQueryRecursiveDfsJoinPlan.executePlan: the
// UNION ALL arm STREAMS through the RecursiveCursor port (per-depth
// continuation stack, primary-key check values); the DISTINCT arm (Java
// runs recursive UNION DISTINCT as UNION ALL, a Java upstream bug in
// DIVERGENCES.md) resumes by deterministic replay.
func executeRecursiveDfsJoin(
	ctx context.Context,
	p *plans.RecordQueryRecursiveDfsJoinPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	if !p.IsDistinct() {
		return executeRecursiveDfsJoinStreaming(ctx, p, store, evalCtx, continuation, props)
	}
	replayBase := func() (recordlayer.RecordCursor[QueryResult], error) {
		return executeRecursiveDfsJoinDistinctEager(ctx, p, store, evalCtx, props)
	}
	cur, err := newPositionReplayCursor(ctx, replayBase, continuation)
	if err != nil {
		return nil, err
	}
	return applySkipLimit(cur, props.Skip, props.ReturnedRowLimit), nil
}

// executeRecursiveDfsJoinStreaming is Java's executePlan: RecursiveCursor
// over the root plan and a per-parent-row child plan re-execution, with
// the prior row bound as a single-row temp table under priorCorrelation
// (the child body reads it through its TempTableScan — Go's prior-binding
// plumbing; Java binds the value directly under an internal correlation
// binding).
func executeRecursiveDfsJoinStreaming(
	ctx context.Context,
	p *plans.RecordQueryRecursiveDfsJoinPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	continuation []byte,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	nested := props.ClearSkipAndLimit()
	rootFn := func(cont []byte) (recordlayer.RecordCursor[QueryResult], error) {
		return ExecutePlan(ctx, p.GetRoot(), store, evalCtx, cont, nested)
	}
	childFn := func(value QueryResult, depth int, cont []byte) (recordlayer.RecordCursor[QueryResult], error) {
		// Java has NO depth bound (fixpoint-only); the loud cap is the
		// same cycle tripwire the eager path and the streaming level
		// union carry — never a silent truncation.
		if depth >= maxStreamingRecursionDepth {
			return nil, &RecursiveCTEDepthExceededError{MaxDepth: maxStreamingRecursionDepth}
		}
		// A transient single-row binding table (not a cardinality
		// buffer): the child body's TempTableScan(priorCorrelation)
		// reads the parent row through it.
		singleRow := NewTempTable()
		if err := singleRow.Add(value); err != nil {
			return nil, err
		}
		childCtx := evalCtx.WithBinding(p.GetPriorCorrelation(), singleRow)
		return ExecutePlan(ctx, p.GetChild(), store, childCtx, cont, nested)
	}
	// Java: "serialize the primary key for continuation integrity
	// checking" — computed rows carry none and go uncheckable (nil).
	checkFn := func(v QueryResult) []byte {
		if v.PrimaryKey != nil {
			return v.PrimaryKey.Pack()
		}
		return nil
	}
	preorder := p.GetTraversalStrategy() == plans.DfsPreorder
	cur, err := newRecursiveCursor(rootFn, childFn, checkFn, continuation, preorder, props.State)
	if err != nil {
		return nil, err
	}
	return applySkipLimit(cur, props.Skip, props.ReturnedRowLimit), nil
}

// executeRecursiveDfsJoinDistinctEager is the DISTINCT extension's eager
// traversal (the cross-level seen-set makes the recursion inherently
// materialized); resume is layered above via deterministic replay.
func executeRecursiveDfsJoinDistinctEager(
	ctx context.Context,
	p *plans.RecordQueryRecursiveDfsJoinPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	props recordlayer.ExecuteProperties,
) (recordlayer.RecordCursor[QueryResult], error) {
	rootCursor, err := ExecutePlan(ctx, p.GetRoot(), store, evalCtx, nil, props.ClearSkipAndLimit())
	if err != nil {
		return nil, fmt.Errorf("executor: recursive dfs join root: %w", err)
	}

	// The DFS rule strips the level-union TempTableInsert tops (Java's
	// matcher shape), so rows are charged HERE, once, by the byte-charging
	// collects — the buffers below are the recursion's only residency.
	rootRows, rootCharge, err := CollectAllBounded(ctx, rootCursor, props.State, props.GetMaterializationLimit(), "recursive DFS join root")
	rootCursor.Close()
	if err != nil {
		props.State.ReleaseMemory(rootCharge)
		return nil, fmt.Errorf("executor: recursive dfs join root collect: %w", err)
	}

	preorder := p.GetTraversalStrategy() == plans.DfsPreorder
	var results []QueryResult
	// RFC-130 live-bytes model: root rows + every child collect + the
	// DISTINCT seen-set are charged as they accumulate and released in one
	// place — at the final cursor's teardown, or on an error return.
	var seenCharge func() int64 = func() int64 { return 0 }
	childCharge := int64(0)
	releaseWorkingSet := func() {
		props.State.ReleaseMemory(rootCharge + childCharge)
		props.State.ReleaseMemory(seenCharge())
	}
	handedOff := false
	defer func() {
		if !handedOff {
			releaseWorkingSet()
		}
	}()
	// RFC-130: the DFS dedup seen-set is a cross-traversal cardinality-growing
	// buffer (one key per distinct visited row) — charge each NEW key via
	// boundedSet.
	var seen *boundedSet[string]
	// For UNION DISTINCT, dedup on the CTE's OUTPUT columns, the root plan's
	// result row (the first root row's layout when it is not a record).
	var keyer *cteDedupKeyer
	if p.IsDistinct() {
		seen = newBoundedSet[string](props.State)
		seenCharge = seen.Charged
		canonicalCols := recursiveUnionOutputColumns(p.GetRoot())
		if len(canonicalCols) == 0 && len(rootRows) > 0 && rootRows[0].Positional != nil {
			// Ordinal order is already deterministic; no sort needed.
			canonicalCols = rootRows[0].Positional.TypeNames()
		}
		keyer = newCTEDedupKeyer(canonicalCols)
	}

	// The SAME cycle tripwire as every other recursion arm
	// (maxStreamingRecursionDepth): the eager DISTINCT arm's former 256
	// silently CUT availability relative to the level union — with the
	// DFS legs Java-shaped, a clause-less deep DISTINCT recursion can
	// cost-flip onto this arm, and a 270-deep acyclic chain that used to
	// answer via RecursiveLevelUnion failed 54F01 on the lower cap.
	const maxRecursionDepth = maxStreamingRecursionDepth

	for _, root := range rootRows {
		if seen != nil {
			k, kerr := keyer.key(root)
			if kerr != nil {
				return nil, kerr
			}
			added, err := seen.Add(k, int64(len(k)))
			if err != nil {
				return nil, err
			}
			if !added {
				continue
			}
		}
		if err := dfsVisit(ctx, root, p, store, evalCtx, preorder, props, &results, 0, maxRecursionDepth, seen, keyer, &childCharge); err != nil {
			return nil, err
		}
	}

	handedOff = true
	// Skip/limit and the resume position are applied by the replay wrapper
	// above this cursor — the eager base serves the FULL deterministic
	// sequence.
	return newCloseHookCursor(recordlayer.FromList(results), releaseWorkingSet), nil
}

func dfsVisit(
	ctx context.Context,
	node QueryResult,
	p *plans.RecordQueryRecursiveDfsJoinPlan,
	store *recordlayer.FDBRecordStore,
	evalCtx *EvaluationContext,
	preorder bool,
	props recordlayer.ExecuteProperties,
	results *[]QueryResult,
	depth, maxDepth int,
	seen *boundedSet[string],
	keyer *cteDedupKeyer,
	childCharge *int64,
) error {
	if depth >= maxDepth {
		return &RecursiveCTEDepthExceededError{MaxDepth: maxDepth}
	}

	if preorder {
		*results = append(*results, node)
	}

	// singleRow is a transient per-visit binding holder for the prior-
	// correlation row — NOT a cardinality-growing buffer (one row, GC'd after
	// the child plan runs), and node is already charged where it was
	// collected. Use a non-charging temp table so it is not double-counted.
	singleRow := NewTempTable()
	if err := singleRow.Add(node); err != nil {
		return err
	}
	childCtx := evalCtx.WithBinding(p.GetPriorCorrelation(), singleRow)
	childCursor, err := ExecutePlan(ctx, p.GetChild(), store, childCtx, nil, props.ClearSkipAndLimit())
	if err != nil {
		return fmt.Errorf("recursive DFS child plan: %w", err)
	}

	// The insert tops are stripped (see the root collect): charge the
	// child rows here, accumulated into the traversal's working set.
	children, charge, err := CollectAllBounded(ctx, childCursor, props.State, props.GetMaterializationLimit(), "recursive DFS children")
	childCursor.Close()
	*childCharge += charge
	if err != nil {
		return fmt.Errorf("recursive DFS collect children: %w", err)
	}

	for _, child := range children {
		if seen != nil {
			k, kerr := keyer.key(child)
			if kerr != nil {
				return kerr
			}
			added, err := seen.Add(k, int64(len(k)))
			if err != nil {
				return err
			}
			if !added {
				continue
			}
		}
		if err := dfsVisit(ctx, child, p, store, evalCtx, preorder, props, results, depth+1, maxDepth, seen, keyer, childCharge); err != nil {
			return err
		}
	}

	if !preorder {
		*results = append(*results, node)
	}
	return nil
}

// recursiveUnionOutputColumns returns the OUTPUT column names of a recursive
// union leg, VERBATIM: the leg plan's result row, which is the CTE body's
// SELECT list. Used to restrict UNION DISTINCT dedup to the CTE's output
// columns (cteDedupKeyer). A copy, so the plan's own schema is never written.
func recursiveUnionOutputColumns(p plans.RecordQueryPlan) []string {
	if p == nil {
		return nil
	}
	row, ok := p.GetResultType().(*values.RecordType)
	if !ok || row == nil {
		return nil
	}
	out := make([]string, len(row.Fields))
	for i, f := range row.Fields {
		out[i] = f.Name
	}
	return out
}

// cteDedupKeyer builds the recursive-CTE UNION-DISTINCT dedup key by reading
// the canonical output columns POSITIONALLY (row.Get(ordinal)). The canonical
// columns are plan-derived (recursiveUnionOutputColumns — the seed projection's
// output layout); each leg's rows flow ONE layout (the leg plan's outermost
// projection output), so the column→ordinal bind is resolved once per row
// LAYOUT — against the row's *RecordType, plan-produced metadata — and cached,
// never re-derived per row. A canonical column absent from a layout keys as
// NULL (dimension-preserving with the seed leg).
// Go extension: Java's recursive union has
// no DISTINCT arm (RecordQueryRecursiveLevelUnionPlan is UNION ALL only).
type cteDedupKeyer struct {
	// cols are the canonical output columns VERBATIM — the spelling the plan
	// declares, not a folded copy of it.
	//
	// This folded, and the failure was a WRONG ANSWER rather than an error. The
	// bind below is `FieldIndexUnique`, an EXACT name lookup against the row's
	// own layout: for a recursive CTE whose alias list is quoted lower-case the
	// row declares `n` while the folded key asked for `N`, so every column bound
	// dedupOrdAbsent, every row keyed as all-NULL, and UNION DISTINCT collapsed
	// the entire result to one row. `WITH RECURSIVE r("n") AS (seed UNION …)`
	// returned [1] where [1,2,3] was correct; the unquoted `r(n)` control
	// returned all three, which is what makes it a case bug rather than a
	// recursion bug.
	cols []string
	ords map[*values.RecordType][]int
}

func newCTEDedupKeyer(cols []string) *cteDedupKeyer {
	canonical := make([]string, len(cols))
	copy(canonical, cols)
	return &cteDedupKeyer{cols: canonical, ords: make(map[*values.RecordType][]int)}
}

// dedupOrdAbsent and dedupOrdAmbiguous are the two NON-ordinal outcomes of
// binding a canonical column to a layout, and they are separate sentinels
// because the key builder must treat them differently.
//
// One sentinel for both was a silent row-loss bug. The bind declines on a name
// the layout declares TWICE as well as on one it does not declare at all, and a
// single "no ordinal" marker sent the duplicate case down the absent branch —
// which packs NULL for that key component. Two rows differing ONLY in the
// duplicated column then produce the same key and UNION DISTINCT drops one.
// Absent-keys-as-NULL is dimension-preserving and correct; ambiguous-keys-as-
// NULL is a wrong answer wearing the same shape.
//
// The 42702 ambiguous-reference rejection does NOT cover this. That rule is
// about a REFERENCE naming two columns; it says nothing about a ROW carrying a
// duplicated name, and a recursive-CTE leg over a join body (`SELECT x.k, y.k
// FROM t x, t y`) emits exactly that row with no reference to reject.
const (
	dedupOrdAbsent    = -1
	dedupOrdAmbiguous = -2
)

// layoutOrdinals binds the canonical columns to rt's slot ordinals, memoized
// per layout. The bind rule is RecordType.FieldIndexUnique — the exact (already
// upper-cased) name resolving to EXACTLY ONE field, the same plan-time rule
// every construction-time bake uses. A name the layout does not declare binds
// dedupOrdAbsent; a name it declares more than once binds dedupOrdAmbiguous,
// which key() refuses rather than keys as NULL.
func (k *cteDedupKeyer) layoutOrdinals(rt *values.RecordType) []int {
	if ords, ok := k.ords[rt]; ok {
		return ords
	}
	ords := make([]int, len(k.cols))
	for i, col := range k.cols {
		switch hits := rt.FieldNameHits(col); {
		case hits == 1:
			ords[i], _ = rt.FieldIndexUnique(col)
		case hits == 0:
			ords[i] = dedupOrdAbsent
		default:
			ords[i] = dedupOrdAmbiguous
		}
	}
	k.ords[rt] = ords
	return ords
}

func (k *cteDedupKeyer) key(qr QueryResult) (string, error) {
	if len(k.cols) == 0 {
		return queryResultKey(qr)
	}
	if qr.Positional == nil || qr.Positional.Type == nil {
		// A layout-less row carries no dedupable values: any constant key
		// would collapse EVERY such row into one (the retired
		// fmt.Sprintf("%v", nil) rendering did exactly that). Loud, never
		// wrong rows (RFC-180 C4).
		return "", fmt.Errorf("CTE dedup over a row with no positional layout — malformed plan")
	}
	ords := k.layoutOrdinals(qr.Positional.Type)
	slots := make([]any, len(ords))
	for i, ord := range ords {
		switch {
		case ord >= 0:
			slots[i], _ = qr.Positional.Get(ord)
		case ord == dedupOrdAmbiguous:
			// REFUSE, never dedup on a partial key. Keying NULL here would
			// collapse rows that differ in this very column; picking one of the
			// duplicate slots would dedup on an arbitrary column. Both are
			// wrong rows, so the query fails loudly instead (RFC-180 C4, the
			// same disposition as the layout-less row above).
			return "", fmt.Errorf("CTE dedup column %q is declared %d times by the leg's output row %v — "+
				"a dedup key component that names two columns cannot be resolved, and keying it as NULL would "+
				"silently collapse rows that differ in it",
				k.cols[i], qr.Positional.Type.FieldNameHits(k.cols[i]), typeFieldNames(qr.Positional.Type))
		default:
			// dedupOrdAbsent: the canonical column is not in this layout at
			// all. NULL is the dimension-preserving answer (see the type doc).
		}
	}
	return packedDedupKey(slots)
}

// queryResultKey produces a stable string key from a QueryResult's positional
// row for UNION DISTINCT deduplication in recursive CTEs. The key is built
// from VALUES ONLY (sorted by column name for determinism) so rows with
// different column names but identical values (e.g. seed {SRC:1} and
// recursive {DST:1}) are correctly identified as duplicates.
func queryResultKey(qr QueryResult) (string, error) {
	pos := qr.Positional
	if pos == nil || pos.Type == nil {
		// See cteDedupKeyer.key: a layout-less row must not collapse into
		// a shared constant key (RFC-180 C4).
		return "", fmt.Errorf("UNION DISTINCT dedup over a row with no positional layout — malformed plan")
	}
	type nv struct {
		name string
		val  any
	}
	pairs := make([]nv, 0, len(pos.Type.Fields))
	for i, f := range pos.Type.Fields {
		var v any
		if i < len(pos.Slots) {
			v = pos.Slots[i]
		}
		pairs = append(pairs, nv{name: f.Name, val: v})
	}
	sort.Slice(pairs, func(a, b int) bool { return pairs[a].name < pairs[b].name })
	slots := make([]any, len(pairs))
	for i, p := range pairs {
		slots[i] = p.val
	}
	return packedDedupKey(slots)
}
