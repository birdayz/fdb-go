package rowdiff

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
// THE INVARIANT IS FALSE-POSITIVE-FREE AND INCOMPLETE, and the second half is
// as deliberate as the first.
//
// It fires ONLY when the ordering an input stream provides can be PROVEN, and it
// proves it by asking the SAME TWO PREDICATES THE PLANNER ASKS — never a local
// re-statement of either:
//
//	plans.EqualityPinsSinglePhysicalKey  — may LATER coordinates claim order
//	                                       THROUGH this one?
//	values.TypeTerminatesOrderingClaim   — may THIS coordinate claim its OWN
//	                                       order at all?
//
// Consulting them is what makes the detector honest, and it is also what makes it
// incomplete. Both are CONSERVATIVE BY DESIGN: TypeTerminatesOrderingClaim is a
// function of the TYPE alone, so it terminates on any FLOAT/DOUBLE coordinate,
// including one whose scanned range could not actually reach both NaN blocks
// (plans/ordering.go:420 states this outright — NaN intrudes for an "unbound OR
// RANGE-BOUND float coordinate"). So this detector INHERITS THE PLANNER'S
// CONSERVATISM AND UNDER-REPORTS BY CONSTRUCTION.
//
// That is the correct trade, and the reason is the whole point of the axis. This
// detector's job is to find where the engine kept a sort ITS OWN RULES SAY IT DID
// NOT NEED — that is a bug. Where the engine keeps a sort its rules say it DOES
// need, and those rules are more conservative than reality permits, that is a
// MISSED OPTIMIZATION: real, worth recording, and categorically not a nightly-net
// red. A detector that knew more than the planner would report the planner's
// documented conservatism as a defect, and a safety net that alarms on
// documented conservatism trains people to ignore it.
//
// Measured: of 16 mismatch occurrences this axis reported across 4 seeds, 12 were
// its own false positives, produced by two hand-rolled copies of rules that had
// drifted from the canonical ones. Nothing is inferred from EXPLAIN text; the
// derivation reads structure (scan direction, index key columns, per-coordinate
// binding and key-component TYPE) directly. A shape whose ordering cannot be
// proven yields NO finding, so a missed redundancy is the worst case — never a
// false alarm in a nightly net.
//
// Derivation rules (per the physical plan types the harness has; properties.
// EstimateOrdering operates on Cascades expressions, not this physical tree):
//   - RecordQueryScanPlan is a primary-key scan → provides [pk] order (this
//     generator's pk is always the single column ID), reversed if reverse.
//   - RecordQueryIndexPlan — and RecordQueryCoveringIndexPlan, which the access
//     path wraps around EVERY index-backed scan and which delegates each of
//     these facts to the scan it wraps — on index [c1..cn] over pk → provides
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

// The facts the index-scan ordering derivation reads — reverseness, the index
// key columns, the trimmed primary key, the scan comparisons and both physical
// type vectors — are all facts about the INDEX SCAN, so the derivation reaches
// them through plans.IndexPlanOf rather than through a locally-declared
// interface listing them. See providedOrdering's index arm for why the local
// interface was retired.

// providedOrdering returns the row ordering p's output stream is PROVABLY in, or
// nil when the harness cannot prove one. Only leaf scans (and order-preserving
// unary wrappers above them) yield a non-nil ordering; every other node kind
// returns nil, so a caller never asserts on an unproven order.
func providedOrdering(p plans.RecordQueryPlan) []ordCol {
	leaf := orderingLeaf(p)
	switch n := leaf.(type) {
	case *plans.RecordQueryScanPlan:
		// Primary-key scan. This generator's primary key is always the single
		// column ID (see gen.go DDL: "PRIMARY KEY (id)"), so a forward scan
		// emits rows in ID-ascending order, a reverse scan in ID-descending.
		// ID is NOT NULL, so nulls placement is irrelevant.
		rev := n.IsReverse()
		return []ordCol{{col: "ID", desc: rev, nullsFirst: !rev}}

	case *plans.RecordQueryIndexPlan, *plans.RecordQueryCoveringIndexPlan:
		// BOTH index shapes, because the access path emits
		// Fetch(Covering(IndexScan)) for every index-backed access (RFC-220) —
		// a bare index plan reaches here only from an ordered-scan rule. Matching
		// the bare plan alone does not make this detector wrong, it makes it
		// ABSENT on the index shapes: providedOrdering answers nil, the caller
		// reads that as "input order unprovable, no finding", and the axis
		// reports clean because it stopped executing. The covering plan reads the
		// same physical range as its inner and emits one row per entry in the
		// same order, so every fact below reads off the SCAN itself, recovered
		// through plans.IndexPlanOf.
		//
		// Reading the inner scan rather than a locally-declared facts interface
		// is the convergence: this file briefly carried its own
		// `indexOrderingSource` listing the six delegating accessors, which made
		// a SECOND surface that had to be kept in step with the covering plan's
		// delegation — and a fact added to the scan but not to the interface
		// goes missing silently, which is the failure this whole arm exists to
		// avoid. The carrier hands back the scan and every fact is read off
		// that, so there is nothing to keep in step.
		src, ok := plans.IndexPlanOf(leaf)
		if !ok {
			return nil
		}
		cols := upperAll(src.GetColumnNames())
		pk := upperAll(src.GetPKColumnNames())
		// Empty index-key or pk metadata → cannot prove the order (pk is empty
		// for a fan-out/createsDuplicates index, where the sorted suffix breaks;
		// this generator has none, but bail rather than guess).
		if len(cols) == 0 || len(pk) == 0 {
			return nil
		}
		rev := src.IsReverse()
		comps := src.GetScanComparisons()
		keyTypes := src.GetKeyComponentTypes()
		pkTypes := src.GetPrimaryKeyComponentTypes()

		// Walk the key columns left to right, asking the CANONICAL predicates the
		// two questions the planner asks — never a local re-statement of either.
		// This walk used to hand-roll one of them (count the leading equalities,
		// drop them, assume everything after is ordered) and got both wrong:
		//
		//   plans.EqualityPinsSinglePhysicalKey — may LATER coordinates claim
		//     order THROUGH this one? A zero-valued float equality answers NO: it
		//     widens across both signed-zero blocks, so the suffix RESTARTS at the
		//     block boundary. The old leadingEqualityCount counted it as pinning,
		//     dropped the column, and then claimed the PK suffix was ordered.
		//
		//   values.TypeTerminatesOrderingClaim — may THIS coordinate claim its own
		//     order at all? A FLOAT/DOUBLE answers NO: NaN packs into two disjoint
		//     blocks at opposite ends of the key space while the comparator ranks
		//     all NaN payloads as one greatest value, so key order and value order
		//     disagree and the tie class spans two ranges. The old walk never asked.
		ordered := make([]string, 0, len(cols)+len(pk))
		suffixClaimable := true
		for i, c := range cols {
			var cr *predicates.ComparisonRange
			if i < len(comps) {
				cr = comps[i]
			}
			if plans.EqualityPinsSinglePhysicalKey(cr) {
				// FIXED coordinate: contributes no order of its own, and the
				// columns after it remain claimable.
				continue
			}
			if i < len(keyTypes) && values.TypeTerminatesOrderingClaim(keyTypes[i]) {
				// This coordinate cannot claim its own order, so neither can
				// anything after it. Everything proven BEFORE it still stands.
				suffixClaimable = false
				break
			}
			ordered = append(ordered, c)
			// A binding that CONSTRAINS but does not pin a single key (an
			// inequality, or a zero-float equality) leaves this column ordered but
			// stops the suffix: later coordinates restart within each admitted
			// block. A nil or EMPTY range is an ABSENT binding — it constrains
			// nothing, so the scan is fully ordered on this column and the suffix
			// survives. (The canonical rule says the same of nil at
			// plans.EqualityPinsSinglePhysicalKey; Empty is the same case reached
			// through a different constructor.)
			if cr != nil && !cr.IsEmpty() {
				suffixClaimable = false
				break
			}
		}
		if suffixClaimable {
			for j, c := range pk {
				if j < len(pkTypes) && values.TypeTerminatesOrderingClaim(pkTypes[j]) {
					break
				}
				ordered = append(ordered, c)
			}
		}
		out := make([]ordCol, 0, len(ordered))
		for _, c := range ordered {
			out = append(out, ordCol{col: c, desc: rev, nullsFirst: !rev})
		}
		return out
	}
	return nil
}

// orderingLeaf descends the order-preserving unary pass-throughs above a scan —
// residual filter, primary-key fetch, type filter — and returns the first node
// that is not one. It is the ONE descent: providedOrdering derives from its
// result, and the sweep's vacuity floor censuses it, so the floor cannot report
// an arm as reached that the derivation does not actually read.
//
// Anything that is not a listed pass-through is returned as-is and ends the
// derivation there (union, join, another sort, projection, …) — including a
// pass-through with a child count other than one, which is a shape this walk
// does not understand and must not guess at.
func orderingLeaf(p plans.RecordQueryPlan) plans.RecordQueryPlan {
	for {
		switch p.(type) {
		case *plans.RecordQueryPredicatesFilterPlan,
			*plans.RecordQueryFetchFromPartialRecordPlan,
			*plans.RecordQueryTypeFilterPlan:
		default:
			return p
		}
		ch := p.GetChildren()
		if len(ch) != 1 {
			return p
		}
		p = ch[0]
	}
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
// the query's ORDER BY — same count, same resolved top-level UNQUALIFIED column,
// and same direction per key. Used only to confirm a sort IS the ORDER BY sort
// before reasoning about its redundancy; a mismatch skips the sort (never a
// false flag).
//
// SortKey.Field is display-only and now renders the complete resolved access
// (for example T_RD.B#2), so it is not an identity channel. RFC-232's ValueExpr
// is the authority: it must be an admitted one-step FieldValue, and the ORDER BY
// name must resolve to that same ordinal in the FieldValue's exact root record.
// A nil/unresolved/nested ValueExpr fails closed instead of reviving a leaf-name
// fallback.
//
// A QUALIFIED key is still REFUSED. The generated OrderKey qualifier names a SQL
// leg (L/R/M), while the physical FieldValue root carries a planner correlation;
// this layer has no authority mapping one namespace to the other. Treating the
// resolved leaf ordinal as sufficient would again conflate `l.id, r.id` with
// `r.id, l.id`.
//
// Two callers' fences keep qualified keys away from here today (checkPlanOrdering
// gates on singleTablePlain, and requestedOrdering answers nil for a qualified
// key), so nothing has ever reached the hole. Neither fence belongs to this
// function, and a guard whose correctness lives entirely in its caller is one
// caller away from being wrong; the refusal below makes the function answerable
// for its own contract.
func sortKeysMatchOrderBy(keys []plans.SortKey, orderBy []OrderKey) bool {
	if len(keys) != len(orderBy) {
		return false
	}
	for i := range keys {
		if orderBy[i].Qual != "" {
			return false
		}
		field, ok := values.AsFieldValue(keys[i].ValueExpr)
		if !ok {
			return false
		}
		path := field.Path()
		if path == nil || path.Len() != 1 {
			return false
		}
		accessor, ok := path.Accessor(0)
		if !ok {
			return false
		}
		root := field.ChildValue()
		if root == nil {
			return false
		}
		recordType, ok := root.Type().(*values.RecordType)
		if !ok || recordType == nil {
			return false
		}
		// The root type's slots carry the descriptor's own spelling, so the
		// EXACT name has to be tried first — folding first would miss every
		// non-upper descriptor and quietly report "ordering not proven",
		// weakening the harness rather than failing it. The folded retry keeps
		// the guard working for the flat generator, which writes canonical SQL
		// identifiers in upper case.
		ordinal, ok := recordType.FieldIndexUnique(orderBy[i].Col)
		if !ok {
			ordinal, ok = recordType.FieldIndexUnique(strings.ToUpper(orderBy[i].Col))
		}
		if !ok || accessor.Ordinal() != ordinal {
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
