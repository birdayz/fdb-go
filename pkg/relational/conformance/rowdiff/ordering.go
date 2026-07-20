package rowdiff

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// The ORDERING axis extends the cost/ordering invariant family (cost.go) onto
// the sort-elimination property a pure row-multiset diff is blind to. A plan
// that carries an in-memory sort ON TOP OF an input stream that is ALREADY in
// the requested order is a defect: RemoveSortRule (Java) / the sort-elimination
// path (Go) should have dropped the sort, and a plan that keeps it burns an
// O(N log N) materialize-and-sort for nothing. It is exactly the ordinal-binding
// class of bug — the plan returns the right rows the wrong (needlessly
// expensive) way — that the row diff cannot see.
//
// The invariant is deliberately FALSE-POSITIVE-FREE. It fires ONLY when the
// ordering an input stream provides can be PROVEN from the physical plan types
// plus the generator's fixed schema, and that provided ordering exactly covers
// (as a prefix) the query's ORDER BY — same column, same direction, same NULL
// placement per key. Nothing is inferred from EXPLAIN text; the derivation reads
// structure (scan direction, index key columns, equality-bound prefix) directly.
// A shape whose ordering cannot be proven yields NO finding (observe-only), so a
// missed redundancy is the worst case — never a false alarm in a nightly net.
//
// Derivation rules (per the physical plan types the harness has; properties.
// EstimateOrdering operates on Cascades expressions, not this physical tree):
//   - RecordQueryScanPlan is a primary-key scan → provides [pk] order (this
//     generator's pk is always the single column ID), reversed if reverse.
//   - RecordQueryIndexPlan on index [c1..cn] over pk → provides
//     [c1..cn, pk...] order, with the LEADING equality-bound prefix dropped
//     (those columns are constant across the scan), reversed if reverse.
//   - PredicatesFilter / FetchFromPartialRecord / TypeFilter preserve their
//     child's order (residual filter, PK fetch, and type filter are all
//     order-preserving pass-throughs), so the walk descends through them to the
//     leaf scan.
//   - Anything else (union, join, apply/semi-join, another sort, projection, …)
//     ends the derivation with "unknown" → no finding.

// ordCol is one component of a proven row ordering: a column, its direction, and
// where NULLs fall. NULL placement is only load-bearing for a NULLABLE column;
// for a NOT NULL column (see orderColNotNull) it is ignored in the match.
type ordCol struct {
	col        string // UPPER-CASE column name; "ID" for the primary key
	desc       bool
	nullsFirst bool
}

// checkPlanOrdering returns ordering-invariant violations for the plan of q.
// The only invariant is REDUNDANT SORT (see the file header): a single-table
// ORDER BY query whose plan carries an in-memory sort over an input the harness
// can PROVE already yields the ORDER BY order. Empty result = clean.
//
// Gated to PLAIN single-table queries (no join / union / derived / aggregate):
// ordering derivation for those shapes is not airtight from the physical tree
// alone, so they are left to the observe-only sweep telemetry rather than risk a
// false positive.
func checkPlanOrdering(p plans.RecordQueryPlan, q Query) []string {
	if !singleTablePlain(q) || len(q.OrderBy) == 0 {
		return nil
	}
	req := requestedOrdering(q.OrderBy)
	if req == nil {
		return nil
	}
	var out []string
	forEachInMemorySort(p, func(s *plans.RecordQueryInMemorySortPlan) {
		// Guard: only reason about a sort that IS the query's ORDER BY sort —
		// its keys line up 1:1 with the ORDER BY (column + direction). In a
		// plain single-table query the only sort a correct plan can carry is the
		// ORDER BY sort, but checking it keeps the invariant honest if that ever
		// changes (a mismatch skips, never a false flag).
		if !sortKeysMatchOrderBy(s.GetSortKeys(), q.OrderBy) {
			return
		}
		prov := providedOrdering(s.GetInner())
		if prov == nil {
			return // unprovable input order → observe-only, no finding
		}
		if orderingCoversPrefix(req, prov) {
			out = append(out,
				"redundant in-memory sort: the sort's input scan already provides the ORDER BY order (sort-elimination should have dropped it)")
		}
	})
	return out
}

// singleTablePlain reports whether q is a plain single-table SELECT: no join,
// 3-way, union, derived source, or aggregate. A correlated EXISTS / scalar
// subquery in the WHERE is allowed — it does not change the outer row order, and
// if it lowers to an apply/semi-join node the ordering walk simply bails there.
func singleTablePlain(q Query) bool {
	return q.Join == nil && q.ThreeWay == nil && q.Union == nil &&
		q.Derived == nil && q.Agg == nil
}

// requestedOrdering translates the query's ORDER BY into the proven-ordering
// vocabulary. Returns nil if any key is table-qualified (a join key — never
// happens for a single-table query, defensive).
func requestedOrdering(keys []OrderKey) []ordCol {
	out := make([]ordCol, 0, len(keys))
	for _, k := range keys {
		if k.Qual != "" {
			return nil
		}
		out = append(out, ordCol{
			col:        strings.ToUpper(k.Col),
			desc:       k.Desc,
			nullsFirst: resolveNullsFirst(k.Nulls, k.Desc),
		})
	}
	return out
}

// resolveNullsFirst resolves an ORDER BY key's NULL placement to a concrete
// nulls-first bool, matching the engine's Java/FDB-parity default: ascending →
// NULLS FIRST, descending → NULLS LAST (tuple order — a NULL encodes as the
// lowest byte, so a forward scan reads NULLs first). Explicit FIRST/LAST win.
func resolveNullsFirst(placement NullsPlacement, desc bool) bool {
	switch placement {
	case NullsFirst:
		return true
	case NullsLast:
		return false
	default:
		return !desc
	}
}

// providedOrdering returns the row ordering p's output stream is PROVABLY in, or
// nil when the harness cannot prove one. Only leaf scans (and order-preserving
// unary wrappers above them) yield a non-nil ordering; every other node kind
// returns nil, so a caller never asserts on an unproven order.
func providedOrdering(p plans.RecordQueryPlan) []ordCol {
	switch n := p.(type) {
	case *plans.RecordQueryScanPlan:
		// Primary-key scan. This generator's primary key is always the single
		// column ID (see gen.go DDL: "PRIMARY KEY (id)"), so a forward scan
		// emits rows in ID-ascending order, a reverse scan in ID-descending.
		// ID is NOT NULL, so nulls placement is irrelevant.
		rev := n.IsReverse()
		return []ordCol{{col: "ID", desc: rev, nullsFirst: !rev}}

	case *plans.RecordQueryIndexPlan:
		cols := upperAll(n.GetColumnNames())
		pk := upperAll(n.GetPKColumnNames())
		// Empty index-key or pk metadata → cannot prove the order (pk is empty
		// for a fan-out/createsDuplicates index, where the sorted suffix breaks;
		// this generator has none, but bail rather than guess).
		if len(cols) == 0 || len(pk) == 0 {
			return nil
		}
		// Drop the LEADING equality-bound prefix: those index columns are held
		// constant by the scan (col = literal), so they contribute nothing to
		// the output ordering. The first non-equality column onward, then the
		// primary key, is the ordered suffix.
		k := leadingEqualityCount(n.GetScanComparisons())
		if k > len(cols) {
			k = len(cols)
		}
		ordered := make([]string, 0, len(cols)-k+len(pk))
		ordered = append(ordered, cols[k:]...)
		ordered = append(ordered, pk...)
		rev := n.IsReverse()
		out := make([]ordCol, 0, len(ordered))
		for _, c := range ordered {
			out = append(out, ordCol{col: c, desc: rev, nullsFirst: !rev})
		}
		return out

	case *plans.RecordQueryPredicatesFilterPlan,
		*plans.RecordQueryFetchFromPartialRecordPlan,
		*plans.RecordQueryTypeFilterPlan:
		// Order-preserving unary pass-throughs: descend to the single child.
		ch := p.GetChildren()
		if len(ch) != 1 {
			return nil
		}
		return providedOrdering(ch[0])
	}
	return nil
}

// leadingEqualityCount counts the leading comparison ranges that are exact
// equalities (col = literal). Sargable index matching binds a left-to-right
// prefix, so the equality-bound columns are always the leading ones.
func leadingEqualityCount(ranges []*predicates.ComparisonRange) int {
	k := 0
	for _, r := range ranges {
		if r == nil || r.GetRangeType() != predicates.ComparisonRangeEquality {
			break
		}
		k++
	}
	return k
}

// orderingCoversPrefix reports whether the requested ordering is a PREFIX of the
// provided ordering under exact per-key equality — same column, same direction,
// and (for a nullable column) same NULL placement. A stream physically in the
// provided order is, by definition, in any prefix of it, so a sort that
// re-imposes such a prefix is pure overhead.
func orderingCoversPrefix(req, prov []ordCol) bool {
	if len(req) > len(prov) {
		return false
	}
	for i := range req {
		r, p := req[i], prov[i]
		if r.col != p.col || r.desc != p.desc {
			return false
		}
		// NULL placement only matters for a column that can actually hold NULLs.
		if !orderColNotNull(r.col) && r.nullsFirst != p.nullsFirst {
			return false
		}
	}
	return true
}

// orderColNotNull reports whether a sort column is NOT NULL in this generator's
// fixed schema (gen.go genTable): the primary key ID and the column B are NOT
// NULL; A, C, S are nullable. For a NOT NULL column there are no NULLs to place,
// so any requested NULLS FIRST/LAST is trivially satisfied by any scan direction.
// Treating a nullable column as NOT NULL would be the only way this could add a
// false positive, so the set is kept to the two columns the schema guarantees.
func orderColNotNull(col string) bool {
	return col == "ID" || col == "B"
}

// sortKeysMatchOrderBy reports whether an in-memory sort's keys line up 1:1 with
// the query's ORDER BY — same count, same column (display Field), same direction
// per key. Used only to confirm a sort IS the ORDER BY sort before reasoning
// about its redundancy; a mismatch skips the sort (never a false flag).
func sortKeysMatchOrderBy(keys []plans.SortKey, orderBy []OrderKey) bool {
	if len(keys) != len(orderBy) {
		return false
	}
	for i := range keys {
		if !strings.EqualFold(keys[i].Field, orderBy[i].Col) {
			return false
		}
		if keys[i].Desc != orderBy[i].Desc {
			return false
		}
	}
	return true
}

// forEachInMemorySort invokes fn for every in-memory sort node in the plan tree.
func forEachInMemorySort(p plans.RecordQueryPlan, fn func(*plans.RecordQueryInMemorySortPlan)) {
	if p == nil {
		return
	}
	if s, ok := p.(*plans.RecordQueryInMemorySortPlan); ok {
		fn(s)
	}
	for _, c := range p.GetChildren() {
		forEachInMemorySort(c, fn)
	}
}

// upperAll upper-cases every string in a slice (index/pk column names arrive in
// the DDL's lower case; ORDER BY columns are compared upper-cased).
func upperAll(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.ToUpper(s)
	}
	return out
}
