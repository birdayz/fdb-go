package embedded

import (
	"context"
	"database/sql/driver"

	"github.com/birdayz/fdb-record-layer-go/pkg/relational/api"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/functions"
)

// Aggregate executor (hash-aggregate semantics with in-memory
// grouping). Runs after the scan path has produced a `[]map[col]val`
// row slice — the shared representation the JOIN, CTE, and filtered-
// scan paths emit. aggregateMapRows applies GROUP BY + aggregate
// function evaluation + HAVING + an optional trailing
// stripAggregateNonVisible pass that drops columns injected purely for
// ORDER BY / HAVING.
//
// Doesn't run the scan itself; execSelectQueryFull / execSelectJoin /
// execSelectFromCTE each feed this function with their already-
// filtered rows. As Phase 1c of RFC 021 introduces plan/physical
// operators, aggregateMapRows becomes the implementation of the
// HashAggregate operator's Execute method with no semantic change.

// requireMinMaxNumeric is the runtime gate for MIN / MAX over non-NULL
// values. fdb-relational 4.11.1.0 rejects MIN / MAX over non-numeric
// columns with `VerifyException: unable to encapsulate aggregate
// operation due to type mismatch(es)` (CLAUDE.md gotcha:
// "MIN(s) / MAX(s) over non-numeric columns is unsupported"). Same
// architectural reason in both engines: the function registry only
// installs numeric MIN / MAX overloads. Lexicographic min / max over
// strings or bytes needs a `SELECT col FROM t ORDER BY col LIMIT 1`
// rewrite.
//
// The error message is INTENTIONALLY VERBATIM the Java
// `VerifyException` message — so the cross-engine harness can pin
// `ExpectErrorContains: "unable to encapsulate aggregate operation"`
// and assert IDENTICAL substrings on both sides, proving Java's
// effective non-support matches Go's rejection at the message level.
// Per project conformance principle: doesn't work in Java → doesn't
// work in Go, with identical error wording.
func requireMinMaxNumeric(v driver.Value) error {
	if _, ok := functions.ToFloat64(v); ok {
		return nil
	}
	return api.NewErrorf(api.ErrCodeUnsupportedOperation,
		"unable to encapsulate aggregate operation due to type mismatch(es)")
}

func (c *EmbeddedConnection) aggregateMapRows(ctx context.Context, sq *selectQuery, filtered []map[string]driver.Value) (cols []string, colTypes []string, data [][]driver.Value, err error) {
	// DISTINCT aggregates are intentionally rejected — Java alignment.
	// fdb-relational 4.11.1.0's parser visitor NPEs on every aggregate
	// with DISTINCT (`AggregateWindowedFunctionContext.ALL().getText()`
	// is called unconditionally; ALL is null when DISTINCT is present,
	// per CLAUDE.md gotcha "COUNT(DISTINCT col) NPEs in fdb-relational").
	// Go matches by failing at execution time with
	// `ErrCodeUnsupportedOperation`. Same architectural reason in both
	// engines: visitor doesn't handle the DISTINCT case. Workaround:
	// pre-aggregate via a derived table — `SELECT COUNT(*) FROM
	// (SELECT DISTINCT col FROM t)` — though SELECT DISTINCT itself
	// has its own conformance status. Per project conformance
	// principle: doesn't work in Java → doesn't work in Go.
	for _, ac := range sq.aggCols {
		if ac.aggDistinct {
			return nil, nil, nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
				"DISTINCT aggregate %s is not supported", ac.aggFunc)
		}
	}
	if sq.countStar {
		count := int64(len(filtered))
		// HAVING refers to the aggregate function, not the SELECT-list
		// alias (SQL §7.12: HAVING is evaluated against the group, not
		// the output columns). Always use canonical "COUNT(*)" for the
		// HAVING rowMap key. The output column name does honor the
		// alias so the outer scope (derived tables, etc.) sees it.
		if sq.havingExpr != nil {
			keep, hErr := evalHaving(ctx, c, map[string]driver.Value{"COUNT(*)": count}, sq.havingExpr)
			if hErr != nil {
				return nil, nil, nil, hErr
			}
			if !keep {
				return []string{countStarOutName(sq)}, []string{"BIGINT"}, nil, nil
			}
		}
		return []string{countStarOutName(sq)}, []string{"BIGINT"}, [][]driver.Value{{count}}, nil
	}

	// Map-path SQL §7.10 GR1 pre-check: every `groupCol` in sq.aggCols
	// that's a bare column reference (not an expression) must either
	// be in sq.groupBy or be a defined-but-ungrouped projection (→
	// 42803). We can tell "defined" vs "undefined" here by probing
	// the first filtered row's keys — the map-path invariant is that
	// scanTableToMaps / cteRowsToMaps populate bare + qualified forms
	// for every defined column. Empty filtered result is ambiguous
	// (we can't tell defined vs undefined) so we skip the check and
	// preserve the silent-NULL-fill behavior there; correctness under
	// Java's plan-time ordering isn't achievable without threading
	// the source schema through this function. Ambiguous pre-probe
	// (sentinel in row[gcol]) also surfaces 42702 via the emission
	// loop below — consistent.
	if len(sq.groupBy) > 0 && len(filtered) > 0 {
		groupByNames := make(map[string]bool, len(sq.groupBy)*2)
		for i, gn := range sq.groupBy {
			if i < len(sq.groupByExprs) && sq.groupByExprs[i] != nil {
				continue
			}
			groupByNames[gn] = true
			// Also store the bare form so `GROUP BY x.col1` matches
			// a `col1` reference in SELECT-list expressions, and vice
			// versa. scanTableToMaps / cteRowsToMaps populate both.
			if ref := parseColRef(gn); ref.isQualified() {
				groupByNames[ref.bare()] = true
			}
		}
		for _, ac := range sq.aggCols {
			if ac.groupCol == "" || ac.outExpr != nil {
				continue
			}
			if groupByNames[ac.groupCol] {
				continue
			}
			// Qualified SELECT-list ref against an unqualified GROUP
			// BY (or vice versa): check the bare last-segment too, so
			// `SELECT x.col1 FROM ... GROUP BY col1` passes.
			if ref := parseColRef(ac.groupCol); ref.isQualified() {
				if groupByNames[ref.bare()] {
					continue
				}
			}
			if v, defined := filtered[0][ac.groupCol]; defined {
				if m, isAmb := v.(ambiguousColumnMarker); isAmb {
					return nil, nil, nil, api.NewErrorf(api.ErrCodeAmbiguousColumn,
						"column reference %q is ambiguous", m.Col)
				}
				return nil, nil, nil, api.NewErrorf(api.ErrCodeGroupingError,
					"column %q must appear in the GROUP BY clause or be used in an aggregate function",
					ac.groupCol)
			}
		}
		// Aggregate function argument: MAX(x.col) etc. must reference a
		// column defined by the source. Java errors 42703 when it isn't.
		// Pre-fix Go silently treated nil as a NULL and the MIN/MAX slot
		// was left at zero-value. Probe the first filtered row's keys.
		for _, ac := range sq.aggCols {
			if ac.aggArg == "" || ac.aggExpr != nil {
				continue
			}
			if _, found := mapLookupStr(filtered[0], ac.aggArg); found {
				continue
			}
			return nil, nil, nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
				"column %q not found", ac.aggArg)
		}
		// outExpr entries (post-aggregation projection expressions like
		// `x.col1 + x.col2`): each referenced column must either be in
		// GROUP BY or wrapped in an aggregate. Java errors 42803 for
		// ungrouped; pre-swingshift-41 Go let the reference through then
		// failed at emit time with 42703 ("not found in row"). Walk the
		// expression tree collecting bare column refs; any ref not in
		// GROUP BY that IS defined in the source → 42803. Refs inside
		// aggregate calls are fine (the aggregate computes them).
		//
		// Asymmetry: when a ref is NOT in GROUP BY AND NOT defined in
		// the source, we intentionally do NOT raise 42803 here — it
		// falls through and fails downstream at evalExprOnMap time with
		// 42703 ("column not found"). Matches Java's distinction:
		// 42803 = "column exists but ungrouped"; 42703 = "column
		// doesn't exist at all".
		//
		// groupByNames preserves identifier case from the AST (no
		// case folding). A GROUP BY that uses `x.Col1` and an outExpr
		// that uses `x.col1` will miss the match and raise a false
		// 42803 — consistent with the rest of this evaluator's case-
		// sensitive semantics, not a new regression.
		for _, ac := range sq.aggCols {
			if ac.outExpr == nil {
				continue
			}
			for _, ref := range harvestColumnRefs(ac.outExpr) {
				cr := parseColRef(ref)
				if groupByNames[ref] || groupByNames[cr.bare()] {
					continue
				}
				// Check both qualified and bare forms for ambiguity and
				// definition, using the structured colRef to avoid manual
				// dot-splitting.
				vQual, definedQual := filtered[0][ref]
				vBare, definedBare := filtered[0][cr.bare()]
				if m, isAmb := vQual.(ambiguousColumnMarker); definedQual && isAmb {
					return nil, nil, nil, api.NewErrorf(api.ErrCodeAmbiguousColumn,
						"column reference %q is ambiguous", m.Col)
				}
				if m, isAmb := vBare.(ambiguousColumnMarker); definedBare && isAmb {
					return nil, nil, nil, api.NewErrorf(api.ErrCodeAmbiguousColumn,
						"column reference %q is ambiguous", m.Col)
				}
				if definedQual || definedBare {
					return nil, nil, nil, api.NewErrorf(api.ErrCodeGroupingError,
						"column %q must appear in the GROUP BY clause or be used in an aggregate function",
						ref)
				}
			}
		}
	}

	type mapGroupState struct {
		groupVals []driver.Value
		counts    []int64
		// SUM accumulators: maintain BOTH an int64 and a float64
		// running total per slot so we can emit int64 when every
		// observed value is integral (matches Java's
		// SUM(BIGINT)→BIGINT typing, important for the
		// `SUM(BIGINT)/COUNT(*)` integer-division semantic) and fall
		// back to float64 once a non-int value is seen. sumNonInt
		// starts as false (zero-value) — i.e. "still int-only" — and
		// only flips to true. Overflow on `sumsI[i] += iv` wraps
		// silently, same as Java's `long` accumulator on SUM(BIGINT).
		sums      []float64
		sumsI     []int64
		sumNonInt []bool
		mins      []driver.Value
		maxes     []driver.Value
		avgs      []float64
		avgsN     []int64
	}
	groupOrder := []string{}
	groups := map[string]*mapGroupState{}
	hasGroups := len(sq.groupBy) > 0
	for _, row := range filtered {
		gVals := make([]driver.Value, len(sq.groupBy))
		for gi, gcol := range sq.groupBy {
			if gi < len(sq.groupByExprs) && sq.groupByExprs[gi] != nil {
				v, evalErr := evalExprOnMap(ctx, c, row, sq.groupByExprs[gi])
				if evalErr != nil {
					return nil, nil, nil, evalErr
				}
				gVals[gi] = v
				continue
			}
			v, lookupErr := mapLookupStrChecked(row, gcol)
			if lookupErr != nil {
				return nil, nil, nil, lookupErr
			}
			gVals[gi] = v
		}
		key := groupByKey(gVals)
		if !hasGroups {
			key = ""
		}
		gs, exists := groups[key]
		if !exists {
			gs = &mapGroupState{
				groupVals: gVals,
				counts:    make([]int64, len(sq.aggCols)),
				sums:      make([]float64, len(sq.aggCols)),
				sumsI:     make([]int64, len(sq.aggCols)),
				sumNonInt: make([]bool, len(sq.aggCols)),
				mins:      make([]driver.Value, len(sq.aggCols)),
				maxes:     make([]driver.Value, len(sq.aggCols)),
				avgs:      make([]float64, len(sq.aggCols)),
				avgsN:     make([]int64, len(sq.aggCols)),
			}
			groups[key] = gs
			groupOrder = append(groupOrder, key)
		}
		for i, ac := range sq.aggCols {
			if ac.groupCol != "" {
				continue
			}
			if ac.outExpr != nil {
				// Post-aggregation expression — evaluated at emit time
				// against the rowMap, not during scan accumulation.
				continue
			}
			var colVal driver.Value
			switch {
			case ac.aggExpr != nil:
				v, evalErr := evalExprOnMap(ctx, c, row, ac.aggExpr)
				if evalErr != nil {
					return nil, nil, nil, evalErr
				}
				colVal = v
			case ac.aggArg != "":
				// Prefer the qualified key. OUTER JOINs explicitly set
				// `alias.col` to NULL for unmatched rows, and falling back
				// to the bare `col` on a present-but-NULL qualified key
				// would pick up the other side's column (e.g. row["a.id"]
				// = NULL on an unmatched-right row, row["id"] = b.id) —
				// exactly the nightshift-36 bug but for aggregates. Only
				// fall through to the bare column when the qualified key
				// is absent from the row.
				resolved, lookupErr := mapLookupStrChecked(row, ac.aggArg)
				if lookupErr != nil {
					return nil, nil, nil, lookupErr
				}
				colVal = resolved
			}
			hasArg := ac.aggArg != "" || ac.aggExpr != nil
			// COUNT(*) (no arg) counts every row, including all-NULL.
			// COUNT(<col|expr>)/SUM/MIN/MAX/AVG skip NULLs per SQL standard.
			if ac.aggFunc == "COUNT" && !hasArg {
				gs.counts[i]++
				continue
			}
			if colVal == nil {
				continue
			}
			gs.counts[i]++
			switch ac.aggFunc {
			case "SUM", "AVG":
				fv, ok := functions.ToFloat64(colVal)
				if !ok {
					return nil, nil, nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
						"unable to encapsulate aggregate operation due to type mismatch(es)")
				}
				if ac.aggFunc == "SUM" {
					gs.sums[i] += fv
					if iv, isInt := colVal.(int64); isInt && !gs.sumNonInt[i] {
						// Java verbatim: throws ArithmeticException
						// "long overflow" on SUM(BIGINT) overflow.
						// Mirror via overflow-checked add. Aligned
						// .
						r, ok := functions.AddInt64Checked(gs.sumsI[i], iv)
						if !ok {
							return nil, nil, nil, api.NewErrorf(api.ErrCodeNumericValueOutOfRange,
								"long overflow")
						}
						gs.sumsI[i] = r
					} else {
						gs.sumNonInt[i] = true
					}
				} else {
					gs.avgs[i] += fv
					gs.avgsN[i]++
				}
			case "MIN":
				if err := requireMinMaxNumeric(colVal); err != nil {
					return nil, nil, nil, err
				}
				if gs.mins[i] == nil || functions.CompareValues(colVal, gs.mins[i]) < 0 {
					gs.mins[i] = colVal
				}
			case "MAX":
				if err := requireMinMaxNumeric(colVal); err != nil {
					return nil, nil, nil, err
				}
				if gs.maxes[i] == nil || functions.CompareValues(colVal, gs.maxes[i]) > 0 {
					gs.maxes[i] = colVal
				}
			}
		}
	}
	// SQL spec: ungrouped aggregate over an empty input still emits one row
	// (COUNT=0, SUM/MIN/MAX/AVG=NULL). Materialise a synthetic empty group so
	// the emit loop produces that row. The HAVING-present empty case is NOT
	// handled here on purpose — see the note below.
	//
	// NOTE on the parallel pipeline: this whole map-aggregate executor is part
	// of the LEGACY embedded SQL interpreter (execSelect → execSelectQueryFull
	// → execSelectJoin/cte_scan → aggregateMapRows). Real data queries do NOT
	// reach it: connection.QueryContext routes every SELECT through the Cascades
	// generator (planSelectCascades). The interpreter survives only as a
	// fallback for (a) INFORMATION_SCHEMA system-table queries — a Go-only
	// extension Java rejects entirely, so there is NO cross-engine reference to
	// conform to — and (b) explain-only plan rendering. So the empty-group +
	// HAVING behaviour here is moot for conformance: the Cascades path handles
	// and pins it to Java 4.12 (agg_empty_count_having_passes /
	// having_count_star_eq_zero_empty). Eliminating this ~3k-line parallel
	// pipeline (route INFORMATION_SCHEMA through Cascades, delete the
	// interpreter) is tracked in TODO.md — the right fix is removal, not
	// keeping two aggregate executors in sync.
	if !hasGroups && len(groupOrder) == 0 && sq.havingExpr == nil {
		groups[""] = &mapGroupState{
			groupVals: nil,
			counts:    make([]int64, len(sq.aggCols)),
			sums:      make([]float64, len(sq.aggCols)),
			sumsI:     make([]int64, len(sq.aggCols)),
			sumNonInt: make([]bool, len(sq.aggCols)),
			mins:      make([]driver.Value, len(sq.aggCols)),
			maxes:     make([]driver.Value, len(sq.aggCols)),
			avgs:      make([]float64, len(sq.aggCols)),
			avgsN:     make([]int64, len(sq.aggCols)),
		}
		groupOrder = append(groupOrder, "")
	}
	groupColIdx := map[string]int{}
	for i, col := range sq.groupBy {
		groupColIdx[col] = i
		// Register the bare last-segment too so that a SELECT-list
		// `col1` resolves a GROUP BY written as `x.col1`, and vice
		// versa. First-wins on collision: if two GROUP BY keys share
		// the same bare name (`GROUP BY x.col1, y.col1`), the first
		// takes the bare slot; queries like `SELECT col1` over such a
		// group are ambiguous to begin with.
		if ref := parseColRef(col); ref.isQualified() {
			if _, exists := groupColIdx[ref.bare()]; !exists {
				groupColIdx[ref.bare()] = i
			}
		}
	}
	// emitIdx lists the aggCols positions that appear in cols/data:
	// visible columns first, then non-visible columns (harvested from
	// ORDER BY / HAVING) so the sort can find them via colIdx. Caller
	// strips data rows to the first `visibleCount` columns after the
	// sort runs.
	emitIdx := make([]int, 0, len(sq.aggCols))
	for i, ac := range sq.aggCols {
		if ac.visible {
			emitIdx = append(emitIdx, i)
		}
	}
	visibleCount := len(emitIdx)
	for i, ac := range sq.aggCols {
		if !ac.visible {
			emitIdx = append(emitIdx, i)
		}
	}
	cols = make([]string, len(emitIdx))
	colTypes = make([]string, len(emitIdx))
	for out, i := range emitIdx {
		ac := sq.aggCols[i]
		cols[out] = ac.outName
		// JOIN / CTE multi-source: no single msgDesc to consult, so
		// SUM/MIN/MAX over a column ref falls back to BIGINT (matches
		// the previous JOIN-path heuristic). COUNT → BIGINT and
		// AVG → DOUBLE land correctly without msgDesc.
		t := aggregateResultJDBCType(ac, nil)
		if t == "" {
			t = "BIGINT"
		}
		colTypes[out] = t
	}
	_ = visibleCount // surfaced via stripAggregateNonVisible()
	for _, key := range groupOrder {
		gs := groups[key]
		fullVals := make([]driver.Value, len(sq.aggCols))
		rowMap := make(map[string]driver.Value, len(sq.aggCols))
		// Pass 1: populate fullVals + rowMap for all non-outExpr entries.
		// outExpr entries need rowMap fully filled before they can evaluate
		// (they may reference any aggregate or group-by value).
		for i, ac := range sq.aggCols {
			if ac.outExpr != nil {
				continue
			}
			if ac.groupCol != "" {
				idx, ok := groupColIdx[ac.groupCol]
				if !ok {
					// Qualified SELECT against unqualified GROUP BY:
					// try the bare last-segment too. Symmetric with
					// the validation loop above.
					if ref := parseColRef(ac.groupCol); ref.isQualified() {
						idx, ok = groupColIdx[ref.bare()]
					}
				}
				if ok {
					fullVals[i] = gs.groupVals[idx]
				}
			} else {
				switch ac.aggFunc {
				case "COUNT":
					fullVals[i] = gs.counts[i]
				case "SUM":
					// SUM of empty-or-all-NULL input is NULL per SQL standard,
					// not 0. counts[i]>0 means at least one non-null observed.
					// DISTINCT SUM accumulates into sums[i] on first-seen value
					// in the DISTINCT branch, so this path is correct for both
					// the DISTINCT and non-DISTINCT cases.
					//
					// Java alignment (int-preserving SUM): if every observed
					// value was integral, emit int64 — important for
					// `SUM(BIGINT) / COUNT(*)` to integer-divide rather than
					// float-divide. Mixed or non-int inputs fall back to the
					// float64 accumulator.
					if gs.counts[i] > 0 {
						if gs.sumNonInt[i] {
							fullVals[i] = gs.sums[i]
						} else {
							fullVals[i] = gs.sumsI[i]
						}
					}
				case "MIN":
					fullVals[i] = gs.mins[i]
				case "MAX":
					fullVals[i] = gs.maxes[i]
				case "AVG":
					if gs.avgsN[i] > 0 {
						fullVals[i] = gs.avgs[i] / float64(gs.avgsN[i])
					}
				}
			}
			rowMap[ac.outName] = fullVals[i]
		}
		// Seed the group-by column values into rowMap so Pass 2 outExpr
		// entries can reference them (`SELECT x.col1 + 10 FROM ... GROUP
		// BY x.col1`). Pre-fix, only aggCols-outName entries were in
		// rowMap so any reference to a bare group key errored 42703.
		// Populate both the GROUP BY name (qualified or bare as written)
		// and the stripped bare form so the evaluator finds it either way.
		for i, gname := range sq.groupBy {
			if i >= len(gs.groupVals) {
				break
			}
			if _, seen := rowMap[gname]; !seen {
				rowMap[gname] = gs.groupVals[i]
			}
			if ref := parseColRef(gname); ref.isQualified() {
				if _, seen := rowMap[ref.bare()]; !seen {
					rowMap[ref.bare()] = gs.groupVals[i]
				}
			}
		}
		// Pass 2: evaluate outExpr entries against the now-populated rowMap.
		// evalExprOnMap resolves AggregateFunctionCall atoms via rowMap lookup
		// (added alongside this pass) so `SUM(a) + SUM(b)`, `COALESCE(SUM(v), 0)`,
		// and similar shapes work.
		for i, ac := range sq.aggCols {
			if ac.outExpr == nil {
				continue
			}
			v, evalErr := evalExprOnMap(ctx, c, rowMap, ac.outExpr)
			if evalErr != nil {
				return nil, nil, nil, evalErr
			}
			fullVals[i] = v
			rowMap[ac.outName] = v
		}
		if sq.havingExpr != nil {
			ok, hErr := evalHaving(ctx, c, rowMap, sq.havingExpr)
			if hErr != nil {
				return nil, nil, nil, hErr
			}
			if !ok {
				continue
			}
		}
		rowVals := make([]driver.Value, len(emitIdx))
		for out, i := range emitIdx {
			rowVals[out] = fullVals[i]
		}
		data = append(data, rowVals)
	}
	return cols, colTypes, data, nil
}

// stripAggregateNonVisible removes trailing non-visible columns added
// by aggregateMapRows / the proto aggregate emit when ORDER BY or
// HAVING referenced aggregates not in the SELECT list. Counts visible
// entries in sq.aggCols; the emit appends non-visible columns AFTER
// the visible ones, so truncating each row to that length restores the
// user's requested output shape.
//
// No-op when every aggCol is visible (the common case) and when the
// countStar fast path is in play (sq.aggCols is empty — nothing to
// strip; cols already correct).
func stripAggregateNonVisible(sq *selectQuery, cols []string, data [][]driver.Value) ([]string, [][]driver.Value) {
	if len(sq.aggCols) == 0 {
		return cols, data
	}
	hasNonVisible := false
	for _, ac := range sq.aggCols {
		if !ac.visible {
			hasNonVisible = true
			break
		}
	}
	if !hasNonVisible {
		return cols, data
	}
	visibleCount := 0
	for _, ac := range sq.aggCols {
		if ac.visible {
			visibleCount++
		}
	}
	if visibleCount >= len(cols) {
		return cols, data
	}
	cols = cols[:visibleCount]
	for i := range data {
		data[i] = data[i][:visibleCount]
	}
	return cols, data
}
