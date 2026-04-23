package embedded

import (
	"strings"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
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
// unspecified (IN-list chains, aggregate paths). Empty sq.orderBy →
// true: nothing to satisfy. aliasToUnderlying may be nil when the
// SELECT has no aliases; the lookup falls through to the direct col
// match.
func naturalOrderSatisfies(orderBy []orderByClause, naturalOrder []string, aliasToUnderlying map[string]string) bool {
	return naturalOrderSatisfiesDir(orderBy, naturalOrder, aliasToUnderlying, true /*ascending*/)
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
// user actually asked for DESC).
func naturalOrderSatisfiesReverse(orderBy []orderByClause, naturalOrder []string, aliasToUnderlying map[string]string) bool {
	if len(orderBy) == 0 {
		return false
	}
	return naturalOrderSatisfiesDir(orderBy, naturalOrder, aliasToUnderlying, false /*descending*/)
}

func naturalOrderSatisfiesDir(orderBy []orderByClause, naturalOrder []string, aliasToUnderlying map[string]string, ascending bool) bool {
	if len(naturalOrder) == 0 {
		return false
	}
	for i, ob := range orderBy {
		if ob.ascending != ascending {
			return false
		}
		// NULLS handling. Tuple order: NULLs sort first under FDB's
		// tuple encoding. Forward scan emits NULLs first → compatible
		// with ASC NULLS FIRST (and unspecified). Reverse scan emits
		// NULLs LAST → compatible with DESC NULLS LAST (and
		// unspecified). Any explicit NULL ordering opposite to the
		// direction's native order forces the in-memory sort.
		if ascending {
			if ob.nullsFirst != nil && !*ob.nullsFirst {
				return false
			}
		} else {
			if ob.nullsFirst != nil && *ob.nullsFirst {
				return false
			}
		}
		if i >= len(naturalOrder) {
			return false
		}
		obCol := ob.colName
		if underlying, isAlias := aliasToUnderlying[strings.ToUpper(obCol)]; isAlias {
			obCol = underlying
		}
		if !strings.EqualFold(obCol, naturalOrder[i]) {
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

// scanPropsForOrder picks the ScanProperties a pushdown branch should
// feed into its cursor builder based on sq.orderBy + the branch's
// natural emission order. Returns ReverseScan when the ORDER BY is a
// non-empty all-DESC prefix of naturalOrder (and forward otherwise,
// including the empty-orderBy case). The bool result signals whether
// reverse was applied — the sort / LIMIT-early-term logic downstream
// needs it to accept DESC-prefix matches too.
func scanPropsForOrder(orderBy []orderByClause, naturalOrder []string, aliasToUnderlying map[string]string) (recordlayer.ScanProperties, bool) {
	if naturalOrderSatisfiesReverse(orderBy, naturalOrder, aliasToUnderlying) {
		return recordlayer.ReverseScan(), true
	}
	return recordlayer.ForwardScan(), false
}
