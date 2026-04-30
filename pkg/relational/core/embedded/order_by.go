package embedded

import (
	"context"
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
)

// ORDER BY elimination + reverse-scan direction selection.
//
// When a SELECT ships with an ORDER BY clause, the post-scan in-memory
// sort is avoidable if the pushdown cursor already emits rows in a
// direction compatible with that ORDER BY. Two cases:
//
//   1. ORDER BY is a non-empty all-ASC prefix of the cursor's natural
//      order → forward scan satisfies it, no sort needed.
//   2. ORDER BY is a non-empty all-DESC prefix of the cursor's natural
//      order → the cursor is built with ReverseScan, emission is the
//      reverse of natural order, which IS the DESC ordering the user
//      asked for.
//
// In both cases LIMIT early-termination also activates: because rows
// stream out in final output order, the scan loop stops once
// OFFSET + LIMIT rows have accumulated.
//
// **Equality-prefix relaxation.** A WHERE clause that equates a column
// to a constant literal makes that column zero-width in the emission
// order: every emitted row has the same value, so ORDER BY on that
// column is trivially satisfied, and the column can be stripped from
// naturalOrder when checking prefix match. `WHERE a = 1 ORDER BY b, c`
// on PK (a, b, c) now eliminates the sort even though orderBy[0] = b
// ≠ naturalOrder[0] = a. Non-leading equated columns are also stripped
// — post-filter they're constant across the emitted rows, so
// `WHERE b = 5 ORDER BY a, c` matches (a, b, c) with b invisible.
//
// Helpers here are pure — no EmbeddedConnection receivers — so they're
// easy to share with future plan/physical operators as Phase 1c of
// RFC 021 moves exec* bodies out of connection.go.

// naturalOrderSatisfies reports whether sq.orderBy is a prefix of the
// chosen scan cursor's natural emission order (aliases resolved),
// with every clause ASC and no explicit NULLS LAST. Shared by the
// ORDER BY elimination check (skip the in-memory sort) and the LIMIT
// early-termination check (stop reading the cursor once enough rows
// are accumulated).
//
// Empty naturalOrder → false: the cursor's emission order is
// unspecified (IN-list chains, aggregate paths). Empty sq.orderBy (or
// every orderBy clause on an equated col) → true: nothing to satisfy.
// aliasToUnderlying may be nil when the SELECT has no aliases; the
// lookup falls through to the direct col match. equatedCols may be
// nil (no WHERE equalities — falls through to the original prefix
// check).
func naturalOrderSatisfies(orderBy []orderByClause, naturalOrder []string, equatedCols map[string]bool, aliasToUnderlying map[string]string) bool {
	return naturalOrderSatisfiesDir(orderBy, naturalOrder, equatedCols, aliasToUnderlying, true /*ascending*/)
}

// naturalOrderSatisfiesReverse is the DESC counterpart. Returns true
// when sq.orderBy is a non-empty prefix of naturalOrder with every
// clause DESC and compatible NULLS handling. Under a reverse scan the
// cursor emits rows in the exact reverse of its forward natural
// order, which is precisely what an all-DESC prefix ORDER BY asks
// for — no in-memory sort needed, and LIMIT still early-terminates
// against the reversed emission.
//
// Empty sq.orderBy → false (the ASC variant would say "nothing to
// satisfy, use forward"; the reverse variant only applies when the
// user actually asked for DESC). Every orderBy clause on an equated
// column also returns false — reverse-scan cost is pointless when no
// clause drives a real direction; forward scan handles it fine.
func naturalOrderSatisfiesReverse(orderBy []orderByClause, naturalOrder []string, equatedCols map[string]bool, aliasToUnderlying map[string]string) bool {
	if len(orderBy) == 0 {
		return false
	}
	if allOrderByEquated(orderBy, equatedCols, aliasToUnderlying) {
		return false
	}
	return naturalOrderSatisfiesDir(orderBy, naturalOrder, equatedCols, aliasToUnderlying, false /*descending*/)
}

func naturalOrderSatisfiesDir(orderBy []orderByClause, naturalOrder []string, equatedCols map[string]bool, aliasToUnderlying map[string]string, ascending bool) bool {
	if len(naturalOrder) == 0 {
		return false
	}
	// Strip orderBy clauses on equated cols first — direction / NULLS
	// on a constant is a no-op. If everything was equated, the ORDER
	// BY is trivially satisfied by any emission order.
	ob := orderBy
	if len(equatedCols) > 0 && len(orderBy) > 0 {
		ob = ob[:0:0] // new backing array — don't mutate caller's slice
		for _, obc := range orderBy {
			c := obc.colName
			if underlying, isAlias := aliasToUnderlying[strings.ToUpper(c)]; isAlias {
				c = underlying
			}
			if equatedCols[strings.ToUpper(c)] {
				continue
			}
			ob = append(ob, obc)
		}
	}
	if len(ob) == 0 {
		// Empty from the start OR every clause was on an equated col.
		// Either way there's nothing the cursor has to order; any
		// emission order satisfies an empty effective ORDER BY.
		return true
	}
	// Strip equated cols from naturalOrder — post-filter, every
	// emitted row has the same value for them, so they're zero-width
	// in the emission order.
	na := naturalOrder
	if len(equatedCols) > 0 {
		na = na[:0:0]
		for _, col := range naturalOrder {
			if equatedCols[strings.ToUpper(col)] {
				continue
			}
			na = append(na, col)
		}
	}
	if len(na) == 0 {
		// Every natural-order dim was equated, but orderBy still has
		// a non-equated clause (caught by the ob filter above). That
		// clause references a col the cursor doesn't order by →
		// in-memory sort is required.
		return false
	}
	for i, obc := range ob {
		if obc.ascending != ascending {
			return false
		}
		// NULLS handling. Tuple order: NULLs sort first under FDB's
		// tuple encoding. Forward scan emits NULLs first → compatible
		// with ASC NULLS FIRST (and unspecified). Reverse scan emits
		// NULLs LAST → compatible with DESC NULLS LAST (and
		// unspecified). Any explicit NULL ordering opposite to the
		// direction's native order forces the in-memory sort.
		if ascending {
			if obc.nullsFirst != nil && !*obc.nullsFirst {
				return false
			}
		} else {
			if obc.nullsFirst != nil && *obc.nullsFirst {
				return false
			}
		}
		if i >= len(na) {
			return false
		}
		obCol := obc.colName
		if underlying, isAlias := aliasToUnderlying[strings.ToUpper(obCol)]; isAlias {
			obCol = underlying
		}
		// Strip the table-qualifier prefix (`<alias>.<col>` → `<col>`)
		// for comparison with the bare natural-order col list. The
		// scan loop populates the row map with both qualified and bare
		// forms; the natural-order check is bare-col-keyed so we
		// follow that convention. nightshift-60.
		if dot := strings.LastIndex(obCol, "."); dot >= 0 {
			obCol = obCol[dot+1:]
		}
		if !strings.EqualFold(obCol, na[i]) {
			return false
		}
	}
	return true
}

// allOrderByEquated reports whether every orderBy clause references
// an equated column (post alias resolution). When true, the ORDER BY
// is semantically a no-op — every clause sorts by a constant — and
// the reverse-scan direction decision collapses to "forward is fine".
// Returns false on empty inputs.
func allOrderByEquated(orderBy []orderByClause, equatedCols map[string]bool, aliasToUnderlying map[string]string) bool {
	if len(equatedCols) == 0 || len(orderBy) == 0 {
		return false
	}
	for _, obc := range orderBy {
		c := obc.colName
		if underlying, isAlias := aliasToUnderlying[strings.ToUpper(c)]; isAlias {
			c = underlying
		}
		// Strip table-qualifier prefix to match the bare-keyed
		// equatedCols. Same convention as naturalOrderSatisfiesDir.
		// nightshift-60.
		if dot := strings.LastIndex(c, "."); dot >= 0 {
			c = c[dot+1:]
		}
		if !equatedCols[strings.ToUpper(c)] {
			return false
		}
	}
	return true
}

// buildOrderByAliases extracts the SELECT-list alias map that
// naturalOrderSatisfies / naturalOrderSatisfiesReverse consult to
// resolve `ORDER BY <alias>` to the underlying column. Equivalent to
// the scan-setup block further down but hoisted so the pushdown-chain
// direction decision can use it.
func buildOrderByAliases(sq *selectQuery) map[string]string {
	if len(sq.projAliases) == 0 {
		return nil
	}
	aliases := make(map[string]string, len(sq.projAliases))
	for i, colName := range sq.projCols {
		if i < len(sq.projAliases) && sq.projAliases[i] != "" {
			aliases[strings.ToUpper(sq.projAliases[i])] = colName
		}
	}
	return aliases
}

// pkOrderingSatisfiesOrderBy reports whether a pushdown branch whose
// natural emission order is the table's PK columns can satisfy the
// user's ORDER BY clause (forward or reverse). Empty ORDER BY and
// all-equated ORDER BY are trivially satisfied. Used to gate the PK
// range / composite-range / composite-prefix / secondary-index-equality
// branches so they decline when their PK emission order doesn't match
// the requested ORDER BY — letting the chain fall through to a strategy
// (eventually `tryIndexScanForOrdering`) that does. nightshift-60.
func pkOrderingSatisfiesOrderBy(orderBy []orderByClause, pkCols []string, equatedCols map[string]bool, aliasToUnderlying map[string]string) bool {
	if len(orderBy) == 0 {
		return true
	}
	if len(equatedCols) > 0 && allOrderByEquated(orderBy, equatedCols, aliasToUnderlying) {
		return true
	}
	return scanSatisfiesOrderBy(orderBy, pkCols, equatedCols, aliasToUnderlying)
}

// indexBranchSatisfiesOrderBy is the secondary-index-branch flavour of
// scanSatisfiesOrderBy. Computes the candidate (idxCols + pkCols)
// emission order for the supplied secondary index, then asks whether
// the user's ORDER BY is satisfied by it forward or reverse. Returns
// false (declining the branch) when idx is nil — that case was always
// inert in the existing code paths anyway. nightshift-60.
func indexBranchSatisfiesOrderBy(idx *recordlayer.Index, pkCols []string, orderBy []orderByClause, equatedCols map[string]bool, aliasToUnderlying map[string]string) bool {
	if idx == nil {
		return false
	}
	idxNaturalOrder := append(append([]string{}, secondaryIndexColumns(idx)...), pkCols...)
	return scanSatisfiesOrderBy(orderBy, idxNaturalOrder, equatedCols, aliasToUnderlying)
}

// scanSatisfiesOrderBy reports whether a pushdown branch's natural
// emission order (forward or reverse) satisfies the user's ORDER BY
// clause. Used as a gating predicate on each secondary-index branch in
// the scan-strategy chain — when the candidate index's emission order
// doesn't match what ORDER BY needs, the branch declines and the chain
// falls through to the next strategy (eventually the full-PK fallback,
// which always emits in pkCols order). This is the Go equivalent of
// fdb-relational's Cascades planner picking a scan whose Ordering
// property satisfies the requested ordering — the rejection of an
// "ORDER BY non-natural col" query emerges from no branch satisfying,
// not from an explicit "throw if no match" check. nightshift-60.
func scanSatisfiesOrderBy(orderBy []orderByClause, naturalOrder []string, equatedCols map[string]bool, aliasToUnderlying map[string]string) bool {
	if naturalOrderSatisfies(orderBy, naturalOrder, equatedCols, aliasToUnderlying) {
		return true
	}
	return naturalOrderSatisfiesReverse(orderBy, naturalOrder, equatedCols, aliasToUnderlying)
}

// scanPropsForOrder picks the ScanProperties a pushdown branch should
// feed into its cursor builder based on sq.orderBy + the branch's
// natural emission order. Returns ReverseScan when the ORDER BY is a
// non-empty all-DESC prefix of naturalOrder (and forward otherwise,
// including the empty-orderBy case). The bool result signals whether
// reverse was applied — the sort / LIMIT-early-term logic downstream
// needs it to accept DESC-prefix matches too.
func scanPropsForOrder(orderBy []orderByClause, naturalOrder []string, equatedCols map[string]bool, aliasToUnderlying map[string]string) (recordlayer.ScanProperties, bool) {
	if naturalOrderSatisfiesReverse(orderBy, naturalOrder, equatedCols, aliasToUnderlying) {
		return recordlayer.ReverseScan(), true
	}
	return recordlayer.ForwardScan(), false
}

// collectEquatedCols returns a case-insensitive set (UPPER-cased keys)
// of bare column names that the WHERE expression equates to a
// constant literal at the top level (AND-conjunction of leaves).
// Returns nil when where is nil or contains any OR/XOR/NOT (can't
// assume the equality holds for every emitted row). `col = NULL`
// literals are excluded — NULL equality is UNKNOWN under 3VL and
// doesn't make the column constant.
func collectEquatedCols(ctx context.Context, c *EmbeddedConnection, where antlrgen.IWhereExprContext) map[string]bool {
	if where == nil {
		return nil
	}
	leaves, ok := flattenAndPredicates(where.Expression())
	if !ok {
		return nil
	}
	var equated map[string]bool
	for _, leaf := range leaves {
		op, col, val, ok := extractColOpLiteral(ctx, c, leaf)
		if !ok || op != "=" || val == nil {
			continue
		}
		if equated == nil {
			equated = make(map[string]bool)
		}
		equated[strings.ToUpper(col)] = true
	}
	return equated
}
