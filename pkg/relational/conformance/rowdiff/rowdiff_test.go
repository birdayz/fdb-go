package rowdiff

// Pure-planner / pure-oracle tests: no FDB. The comparator sensitivity
// tests are RFC-182 OQ-5's permanent net — they prove the diff layer flags
// synthetic mismatches, so a future all-green run can be trusted.

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
)

func TestGenerate_Deterministic(t *testing.T) {
	t.Parallel()
	for _, seed := range []uint64{1, 7, 12345, 1 << 40} {
		a, b := Generate(seed), Generate(seed)
		if a.DDL() != b.DDL() || a.InsertSQL() != b.InsertSQL() {
			t.Fatalf("seed %d: schema/data not deterministic", seed)
		}
		if len(a.Queries) != len(b.Queries) {
			t.Fatalf("seed %d: query count differs", seed)
		}
		for i := range a.Queries {
			for _, proj := range a.Projections() {
				if a.SQL(a.Queries[i], proj) != b.SQL(b.Queries[i], proj) {
					t.Fatalf("seed %d query %d: SQL not deterministic", seed, i)
				}
			}
		}
	}
}

func TestTemplates_PlanIntoRequiredFamilies(t *testing.T) {
	t.Parallel()
	for _, tpl := range Templates() {
		if err := CheckTemplateFamily(tpl); err != nil {
			t.Errorf("%v", err)
		}
	}
}

func TestOracle_FiltersAndSorts(t *testing.T) {
	t.Parallel()
	tpl := Templates()[0] // intersection: A=1 AND C=2 over the fixed 24 rows
	got, err := OracleRows(tpl.Case, tpl.Case.Queries[0], []string{"ID"})
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	// A = i%3 == 1 and C = i%5 == 2 → i ≡ 1 (mod 3) and i ≡ 2 (mod 5)
	// → i ≡ 7 (mod 15) → i ∈ {7, 22} → IDs {8, 23}.
	if len(got) != 2 || got[0]["ID"] != int64(8) || got[1]["ID"] != int64(23) {
		t.Fatalf("oracle rows = %v, want IDs [8 23]", got)
	}
}

func TestOracle_NullSemantics(t *testing.T) {
	t.Parallel()
	c := &Case{
		Table: templateTable(),
		Rows: []Row{
			{"ID": int64(1), "A": nil, "B": int64(0), "C": int64(0), "S": "x", "F": false},
			{"ID": int64(2), "A": int64(5), "B": int64(0), "C": int64(0), "S": "x", "F": false},
		},
	}
	// A <> 5: NULL row must NOT qualify (UNKNOWN drops, SQL semantics).
	q := Query{Where: leaf("A", predicates.ComparisonNotEquals, int64(5))}
	got, err := OracleRows(c, q, []string{"ID"})
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("A<>5 over {NULL, 5} must return 0 rows, got %v", got)
	}
	// A IS NULL: only the NULL row.
	q = Query{Where: leaf("A", predicates.ComparisonIsNull, nil)}
	got, err = OracleRows(c, q, []string{"ID"})
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	if len(got) != 1 || got[0]["ID"] != int64(1) {
		t.Fatalf("A IS NULL must return ID 1, got %v", got)
	}
}

func TestDiffRows_SensitivityToSyntheticMismatches(t *testing.T) {
	t.Parallel()
	cols := []string{"ID", "A"}
	r := func(id, a int64) Row { return Row{"ID": id, "A": a} }

	// Equal multisets → no finding.
	if d := diffRows([]Row{r(1, 5), r(2, 6)}, cols, []Row{r(2, 6), r(1, 5)}, Query{}); d != "" {
		t.Fatalf("equal multisets flagged: %s", d)
	}
	// Missing row (the dropped-residual class) → finding.
	if d := diffRows([]Row{r(1, 5)}, cols, []Row{r(1, 5), r(2, 6)}, Query{}); d == "" {
		t.Fatal("missing row not flagged")
	}
	// Extra row → finding.
	if d := diffRows([]Row{r(1, 5), r(2, 6), r(3, 7)}, cols, []Row{r(1, 5), r(2, 6)}, Query{}); d == "" {
		t.Fatal("extra row not flagged")
	}
	// Same count, different value → finding.
	if d := diffRows([]Row{r(1, 5), r(2, 9)}, cols, []Row{r(1, 5), r(2, 6)}, Query{}); d == "" {
		t.Fatal("value divergence not flagged")
	}
	// Type divergence at same printed value → finding (typed keys).
	if d := diffRows([]Row{{"ID": int64(1), "A": "5"}}, cols, []Row{{"ID": int64(1), "A": int64(5)}}, Query{}); d == "" {
		t.Fatal("type divergence not flagged")
	}
	// Ordered comparison: same multiset, wrong order → finding.
	ordered := Query{OrderBy: []OrderKey{{Col: "ID"}}}
	if d := diffRows([]Row{r(2, 6), r(1, 5)}, cols, []Row{r(1, 5), r(2, 6)}, ordered); d == "" {
		t.Fatal("order violation not flagged")
	}
	if d := diffRows([]Row{r(1, 5), r(2, 6)}, cols, []Row{r(1, 5), r(2, 6)}, ordered); d != "" {
		t.Fatalf("correct order flagged: %s", d)
	}
}

func TestIsInfraError_Classification(t *testing.T) {
	t.Parallel()
	// INFRA: the known transport/context signatures.
	for _, err := range []error{
		context.Canceled,
		fmt.Errorf("wrap: %w", context.DeadlineExceeded),
		driver.ErrBadConn,
		&net.OpError{Op: "dial", Err: errors.New("refused")},
	} {
		if !isInfraError(err) {
			t.Errorf("%v must classify as INFRA", err)
		}
	}
	// FINDING: anything else — an engine error must never hide behind INFRA.
	for _, err := range []error{
		errors.New("42601: syntax error"),
		errors.New("0AF00: could not plan query"),
	} {
		if isInfraError(err) {
			t.Errorf("%v must stay a finding, not INFRA", err)
		}
	}
}

func TestOracle_P2Leaves(t *testing.T) {
	t.Parallel()
	c := &Case{
		Table: templateTable(),
		Rows: []Row{
			{"ID": int64(1), "A": int64(3), "B": int64(0), "C": int64(0), "S": "alpha", "F": false},
			{"ID": int64(2), "A": int64(7), "B": int64(0), "C": int64(0), "S": "beta", "F": false},
			{"ID": int64(3), "A": nil, "B": int64(0), "C": int64(0), "S": "gamma", "F": false},
		},
	}
	ids := func(q Query) []int64 {
		t.Helper()
		rows, err := OracleRows(c, q, []string{"ID"})
		if err != nil {
			t.Fatalf("oracle: %v", err)
		}
		var out []int64
		for _, r := range rows {
			out = append(out, r["ID"].(int64))
		}
		return out
	}

	// IN: NULL row never qualifies; members do.
	got := ids(Query{Where: &BoolNode{Leaf: &Pred{Col: "A", Op: predicates.ComparisonIn, InList: []any{int64(3), int64(9)}}}})
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("IN: got %v, want [1]", got)
	}
	// BETWEEN: inclusive bounds; NULL drops.
	got = ids(Query{Where: &BoolNode{Leaf: &Pred{Col: "A", Op: predicates.ComparisonGreaterThanEq, Lit: int64(3), BetweenHi: int64(7), IsBetween: true}}})
	if len(got) != 2 {
		t.Fatalf("BETWEEN 3 AND 7: got %v, want [1 2]", got)
	}
	// LIKE: engine pattern semantics (% and _).
	got = ids(Query{Where: &BoolNode{Leaf: &Pred{Col: "S", Op: predicates.ComparisonLike, Lit: "%eta"}}})
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("LIKE %%eta: got %v, want [2]", got)
	}
}

func TestOracle_NullsPlacementAndDistinct(t *testing.T) {
	t.Parallel()
	c := &Case{
		Table: templateTable(),
		Rows: []Row{
			{"ID": int64(1), "A": int64(5), "B": int64(0), "C": int64(0), "S": "x", "F": false},
			{"ID": int64(2), "A": nil, "B": int64(0), "C": int64(0), "S": "x", "F": false},
			{"ID": int64(3), "A": int64(1), "B": int64(0), "C": int64(0), "S": "y", "F": false},
		},
	}
	firstID := func(q Query) int64 {
		t.Helper()
		rows, err := OracleRows(c, q, []string{"ID"})
		if err != nil || len(rows) == 0 {
			t.Fatalf("oracle: %v rows=%v", err, rows)
		}
		return rows[0]["ID"].(int64)
	}

	// Default ASC → NULLS FIRST (Java/FDB tuple order).
	if id := firstID(Query{OrderBy: []OrderKey{{Col: "A"}, {Col: "ID"}}}); id != 2 {
		t.Fatalf("ASC default: first ID %d, want 2 (NULL first)", id)
	}
	// Explicit NULLS LAST overrides.
	if id := firstID(Query{OrderBy: []OrderKey{{Col: "A", Nulls: NullsLast}, {Col: "ID"}}}); id != 3 {
		t.Fatalf("ASC NULLS LAST: first ID %d, want 3", id)
	}
	// Default DESC → NULLS LAST.
	if id := firstID(Query{OrderBy: []OrderKey{{Col: "A", Desc: true}, {Col: "ID"}}}); id != 1 {
		t.Fatalf("DESC default: first ID %d, want 1 (5 first, NULL last)", id)
	}

	// DISTINCT over a narrow projection dedups projected rows.
	rows, err := OracleRows(c, Query{Distinct: true}, []string{"S"})
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("DISTINCT s: got %d rows, want 2", len(rows))
	}
}

func TestDiffRows_LimitMembership(t *testing.T) {
	t.Parallel()
	cols := []string{"ID"}
	r := func(id int64) Row { return Row{"ID": id} }
	oracle := []Row{r(1), r(2), r(3)}

	// min(k,|M|): engine must return exactly 2 for LIMIT 2 over |M|=3.
	if d := diffRows([]Row{r(1), r(3)}, cols, oracle, Query{Limit: 2}); d != "" {
		t.Fatalf("valid 2-subset flagged: %s", d)
	}
	// Any 2-subset is valid, but a non-member is not.
	if d := diffRows([]Row{r(1), r(9)}, cols, oracle, Query{Limit: 2}); d == "" {
		t.Fatal("non-member row not flagged")
	}
	// Duplicate consumption: engine may not repeat a row the oracle has once.
	if d := diffRows([]Row{r(1), r(1)}, cols, oracle, Query{Limit: 2}); d == "" {
		t.Fatal("duplicated row not flagged")
	}
	// |M| < k clamp: engine must return ALL 3, not fewer.
	if d := diffRows([]Row{r(1), r(2)}, cols, oracle, Query{Limit: 10}); d == "" {
		t.Fatal("|M|<k short result not flagged")
	}
	if d := diffRows([]Row{r(1), r(2), r(3)}, cols, oracle, Query{Limit: 10}); d != "" {
		t.Fatalf("|M|<k full result flagged: %s", d)
	}
	// Ordered + LIMIT: exact prefix required.
	ordered := Query{OrderBy: []OrderKey{{Col: "ID"}}, Limit: 2}
	if d := diffRows([]Row{r(1), r(2)}, cols, oracle, ordered); d != "" {
		t.Fatalf("valid ordered prefix flagged: %s", d)
	}
	if d := diffRows([]Row{r(1), r(3)}, cols, oracle, ordered); d == "" {
		t.Fatal("non-prefix ordered result not flagged")
	}
}

func TestOracle_NotAndColumnComparison(t *testing.T) {
	t.Parallel()
	c := &Case{
		Table: templateTable(),
		Rows: []Row{
			{"ID": int64(1), "A": int64(5), "B": int64(3), "C": int64(0), "S": "x", "F": false},
			{"ID": int64(2), "A": int64(2), "B": int64(9), "C": int64(0), "S": "x", "F": false},
			{"ID": int64(3), "A": nil, "B": int64(4), "C": int64(0), "S": "x", "F": false},
		},
	}
	ids := func(q Query) []int64 {
		t.Helper()
		rows, err := OracleRows(c, q, []string{"ID"})
		if err != nil {
			t.Fatalf("oracle: %v", err)
		}
		var out []int64
		for _, r := range rows {
			out = append(out, r["ID"].(int64))
		}
		return out
	}

	// Column-vs-column: A > B holds only for id 1 (5>3); the NULL row is UNKNOWN.
	got := ids(Query{Where: &BoolNode{Leaf: &Pred{Col: "A", Op: predicates.ComparisonGreaterThan, RhsCol: "B"}}})
	if len(got) != 1 || got[0] != 1 {
		t.Fatalf("A > B: got %v, want [1]", got)
	}
	// NOT on a leaf: NOT (A > B) is TRUE only for id 2; the NULL row stays
	// UNKNOWN under Kleene negation and must NOT appear.
	got = ids(Query{Where: &BoolNode{Leaf: &Pred{Col: "A", Op: predicates.ComparisonGreaterThan, RhsCol: "B", Negated: true}}})
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("NOT (A > B): got %v, want [2] (NULL row must stay UNKNOWN)", got)
	}
	// NOT on a whole node.
	got = ids(Query{Where: &BoolNode{Not: true, And: true, Kids: []*BoolNode{
		{Leaf: &Pred{Col: "A", Op: predicates.ComparisonEquals, Lit: int64(5)}},
	}}})
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("NOT (A = 5): got %v, want [2]", got)
	}
	// NOT IN: negation over a membership list, NULL still UNKNOWN.
	got = ids(Query{Where: &BoolNode{Leaf: &Pred{
		Col: "A", Op: predicates.ComparisonIn, InList: []any{int64(5)}, Negated: true,
	}}})
	if len(got) != 1 || got[0] != 2 {
		t.Fatalf("A NOT IN (5): got %v, want [2]", got)
	}
}

func TestDiffRows_Offset(t *testing.T) {
	t.Parallel()
	cols := []string{"ID"}
	r := func(id int64) Row { return Row{"ID": id} }
	oracle := []Row{r(1), r(2), r(3), r(4)}
	ordered := func(limit, offset int) Query {
		return Query{OrderBy: []OrderKey{{Col: "ID"}}, Limit: limit, Offset: offset}
	}

	// OFFSET 1 LIMIT 2 over [1 2 3 4] → [2 3].
	if d := diffRows([]Row{r(2), r(3)}, cols, oracle, ordered(2, 1)); d != "" {
		t.Fatalf("valid offset window flagged: %s", d)
	}
	if d := diffRows([]Row{r(1), r(2)}, cols, oracle, ordered(2, 1)); d == "" {
		t.Fatal("engine ignoring OFFSET not flagged")
	}
	// OFFSET past the end yields nothing.
	if d := diffRows(nil, cols, oracle, ordered(2, 10)); d != "" {
		t.Fatalf("offset past end should yield no rows: %s", d)
	}
	if d := diffRows([]Row{r(1)}, cols, oracle, ordered(2, 10)); d == "" {
		t.Fatal("rows past a beyond-end offset not flagged")
	}
}

func TestOracle_SelfJoin(t *testing.T) {
	t.Parallel()
	// C points at another row's ID, so `L.C = R.ID` is a meaningful join.
	c := &Case{
		Table: templateTable(),
		Rows: []Row{
			{"ID": int64(1), "A": int64(0), "B": int64(0), "C": int64(2), "S": "x", "F": false},
			{"ID": int64(2), "A": int64(1), "B": int64(0), "C": int64(3), "S": "y", "F": false},
			{"ID": int64(3), "A": int64(0), "B": int64(0), "C": nil, "S": "z", "F": false},
		},
	}
	q := Query{
		Join:    &JoinSpec{LeftCol: "C", RightCol: "ID", Inner: true},
		OrderBy: []OrderKey{{Col: "ID", Qual: "L"}, {Col: "ID", Qual: "R"}},
	}
	proj := []string{"L.ID", "R.ID"}
	got, err := OracleRows(c, q, proj)
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	// 1→2 and 2→3 match; row 3 has C = NULL and joins to nothing.
	if len(got) != 2 {
		t.Fatalf("self-join produced %d rows, want 2: %v", len(got), got)
	}
	if got[0]["L_ID"] != int64(1) || got[0]["R_ID"] != int64(2) {
		t.Errorf("row 0 = %v, want L_ID=1 R_ID=2", got[0])
	}
	if got[1]["L_ID"] != int64(2) || got[1]["R_ID"] != int64(3) {
		t.Errorf("row 1 = %v, want L_ID=2 R_ID=3", got[1])
	}

	// A qualified single-sided filter narrows one side only.
	q.Where = &BoolNode{Leaf: &Pred{Col: "S", Op: predicates.ComparisonEquals, Lit: "y", Qual: "L"}}
	got, err = OracleRows(c, q, proj)
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	if len(got) != 1 || got[0]["L_ID"] != int64(2) {
		t.Fatalf("filtered self-join = %v, want the single L_ID=2 row", got)
	}
}

func TestJoinSQL_RendersUniqueOutputAliases(t *testing.T) {
	t.Parallel()
	c := &Case{Table: templateTable()}
	q := Query{Join: &JoinSpec{LeftCol: "C", RightCol: "ID", Inner: true}}
	sqlText := c.SQL(q, []string{"L.ID", "R.ID"})
	// Both sides project ID; without distinct aliases the harness's
	// name-keyed rows would collapse them and weaken every join comparison.
	if !strings.Contains(sqlText, "l.id AS l_id") || !strings.Contains(sqlText, "r.id AS r_id") {
		t.Fatalf("join projection must alias both sides uniquely: %s", sqlText)
	}
	if !strings.Contains(sqlText, "JOIN t_rd AS r ON l.c = r.id") {
		t.Fatalf("unexpected join rendering: %s", sqlText)
	}
	// The comma form must carry the join equality in the WHERE instead.
	q.Join.Inner = false
	sqlText = c.SQL(q, []string{"L.ID", "R.ID"})
	if !strings.Contains(sqlText, "t_rd AS l, t_rd AS r") || !strings.Contains(sqlText, "WHERE l.c = r.id") {
		t.Fatalf("comma-join must move the equality into WHERE: %s", sqlText)
	}
}

// TestOracle_Aggregates pins the ONE oracle path that restates SQL
// semantics instead of sharing the engine's (see AggSpec). Every rule it
// restates is asserted here, because a wrong aggregate oracle produces
// false findings that waste a reader's trust.
func TestOracle_Aggregates(t *testing.T) {
	t.Parallel()
	c := &Case{
		Table: templateTable(),
		Rows: []Row{
			{"ID": int64(1), "A": int64(5), "B": int64(1), "C": int64(0), "S": "x", "F": false},
			{"ID": int64(2), "A": nil, "B": int64(1), "C": int64(0), "S": "x", "F": false},
			{"ID": int64(3), "A": int64(7), "B": int64(2), "C": int64(0), "S": "x", "F": false},
			{"ID": int64(4), "A": nil, "B": nil, "C": int64(0), "S": "x", "F": false},
		},
	}
	one := func(q Query) Row {
		t.Helper()
		rows, err := OracleRows(c, q, nil)
		if err != nil {
			t.Fatalf("oracle: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("want exactly 1 row, got %d: %v", len(rows), rows)
		}
		return rows[0]
	}

	// COUNT(*) counts rows INCLUDING null-valued ones; COUNT(col) does not.
	if got := one(Query{Agg: &AggSpec{Func: AggCountStar}})["AGG"]; got != int64(4) {
		t.Errorf("COUNT(*) = %v, want 4", got)
	}
	if got := one(Query{Agg: &AggSpec{Func: AggCountCol, Col: "A"}})["AGG"]; got != int64(2) {
		t.Errorf("COUNT(A) = %v, want 2 (NULLs excluded)", got)
	}
	// SUM/MIN/MAX skip NULLs.
	if got := one(Query{Agg: &AggSpec{Func: AggSum, Col: "A"}})["AGG"]; got != int64(12) {
		t.Errorf("SUM(A) = %v, want 12", got)
	}
	if got := one(Query{Agg: &AggSpec{Func: AggMin, Col: "A"}})["AGG"]; got != int64(5) {
		t.Errorf("MIN(A) = %v, want 5", got)
	}

	// A scalar aggregate over an EMPTY input still returns one row:
	// COUNT → 0, SUM → NULL.
	never := &BoolNode{Leaf: &Pred{Col: "A", Op: predicates.ComparisonEquals, Lit: int64(999)}}
	if got := one(Query{Agg: &AggSpec{Func: AggCountStar}, Where: never})["AGG"]; got != int64(0) {
		t.Errorf("COUNT(*) over empty = %v, want 0", got)
	}
	if got := one(Query{Agg: &AggSpec{Func: AggSum, Col: "A"}, Where: never})["AGG"]; got != nil {
		t.Errorf("SUM over empty = %v, want NULL", got)
	}

	// GROUPED: a NULL key is its OWN group, and an all-NULL group's SUM is NULL.
	rows, err := OracleRows(c, Query{Agg: &AggSpec{Func: AggSum, Col: "A", GroupBy: "B"}}, nil)
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("GROUP BY B: want 3 groups (1, 2, NULL), got %d: %v", len(rows), rows)
	}
	byKey := map[any]any{}
	for _, r := range rows {
		byKey[r["G"]] = r["AGG"]
	}
	if byKey[int64(1)] != int64(5) {
		t.Errorf("group B=1: SUM(A) = %v, want 5 (the NULL A skipped)", byKey[int64(1)])
	}
	if byKey[nil] != nil {
		t.Errorf("group B=NULL: SUM(A) = %v, want NULL (its only row has A NULL)", byKey[nil])
	}
	// A grouped aggregate over an empty input returns NO rows.
	rows, err = OracleRows(c, Query{Agg: &AggSpec{Func: AggCountStar, GroupBy: "B"}, Where: never}, nil)
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("grouped aggregate over empty input = %v, want no rows", rows)
	}

	// HAVING filters on the aggregate; a NULL aggregate never passes.
	rows, err = OracleRows(c, Query{Agg: &AggSpec{
		Func: AggSum, Col: "A", GroupBy: "B", HavingOn: true,
		Having: &Pred{Op: predicates.ComparisonGreaterThan, Lit: int64(6)},
	}}, nil)
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	if len(rows) != 1 || rows[0]["G"] != int64(2) {
		t.Errorf("HAVING SUM(A) > 6 = %v, want just the B=2 group", rows)
	}
}

// TestJoinProjections_AliasesAreUnique guards the harness's own blind spot:
// rows are keyed by output column NAME, so two projected columns that
// collapse to one alias would silently compare only one of them — the same
// duplicate-name failure mode as the bug this work was written to catch.
// Uniqueness currently holds by construction (a fixed column pool × two
// sides); this asserts it instead of trusting it.
func TestJoinProjections_AliasesAreUnique(t *testing.T) {
	t.Parallel()
	for _, seed := range []uint64{1, 2, 3, 500000, 700001} {
		c := Generate(seed)
		for _, proj := range c.joinProjections() {
			seen := map[string]string{}
			for _, qualified := range proj {
				alias := joinOutputAlias(qualified)
				if prev, dup := seen[alias]; dup {
					t.Fatalf("seed %d: %q and %q both alias to %q — the comparison would silently collapse",
						seed, prev, qualified, alias)
				}
				seen[alias] = qualified
			}
		}
	}
}

// TestKnownGaps_LedgerIsNarrow guards the suppression hole: the known-gap
// ledger converts a real engine failure into a DECLINE, so an over-broad
// matcher silently hides bugs. Every near-miss below must stay a FINDING.
func TestKnownGaps_LedgerIsNarrow(t *testing.T) {
	t.Parallel()
	const nestedIn = "SELECT * FROM t WHERE a IN (1,2) AND b = 3 AND c IN (4,5)"
	planInvariantInJoin := errors.New("XX000: malformed query plan: plan-invariant: non-leaf plan *plans.RecordQueryInJoinPlan has no children")

	// The PRODUCTION ledger is empty (its one entry was retired when the
	// nested-IN gap was fixed), so nothing may be declined — including the
	// failure that entry used to cover.
	if gap := matchKnownGap(nestedIn, planInvariantInJoin); gap != nil {
		t.Fatalf("ledger declined %q, but the nested-IN gap is FIXED — a retired entry must not linger", gap.name)
	}

	// The narrowness cases below run against an INJECTED ledger holding the
	// retired entry verbatim. Testing the empty production ledger would pass
	// vacuously and stop guarding the suppression hole the day someone adds
	// a real entry; this keeps the matcher's shape under test regardless.
	ledger := []knownGap{{
		name: "nested-IN over an intersection extracts InJoin(<nil>)",
		pin:  "TestNestedIn_OverIntersection",
		matches: func(sqlText string, err error) bool {
			return strings.Contains(err.Error(), "plan-invariant") &&
				strings.Contains(err.Error(), "RecordQueryInJoinPlan") &&
				strings.Count(strings.ToUpper(sqlText), " IN (") >= 2
		},
	}}
	if matchGapIn(ledger, nestedIn, planInvariantInJoin) == nil {
		t.Fatal("the injected entry must match its own documented shape")
	}
	for _, tc := range []struct {
		name string
		sql  string
		err  error
	}{
		{
			// Same error, ONE IN — a different (undocumented) shape.
			name: "single_IN_stays_a_finding",
			sql:  "SELECT * FROM t WHERE a IN (1,2) AND b = 3",
			err:  planInvariantInJoin,
		},
		{
			// Same shape, a DIFFERENT plan node — not the documented gap.
			name: "other_plan_node_stays_a_finding",
			sql:  nestedIn,
			err:  errors.New("XX000: plan-invariant: non-leaf plan *plans.RecordQueryPredicatesFilterPlan has no children"),
		},
		{
			// Same shape, a wholly different error class.
			name: "other_error_stays_a_finding",
			sql:  nestedIn,
			err:  errors.New("XX000: result row carries no positional output row aligned to column ID"),
		},
		{
			// A ROW divergence must never be declinable — no error at all.
			name: "row_mismatch_stays_a_finding",
			sql:  nestedIn,
			err:  errors.New("row count: engine 3, oracle expects 2"),
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if gap := matchGapIn(ledger, tc.sql, tc.err); gap != nil {
				t.Fatalf("ledger over-matched (%q) — this must stay a finding", gap.name)
			}
		})
	}
}

func TestFinalizeResult_MismatchPrecedence(t *testing.T) {
	t.Parallel()
	// A confirmed mismatch must never be masked by a later infra failure on
	// the same seed.
	res := finalizeResult(&SeedResult{
		Mismatches: []*Mismatch{{Seed: 1, Detail: "x"}},
		InfraErr:   errors.New("late transport failure"),
	})
	if res.Kind != OutcomeMismatch {
		t.Fatalf("mismatch+infra seed: Kind = %v, want OutcomeMismatch", res.Kind)
	}
	if finalizeResult(&SeedResult{InfraErr: errors.New("x")}).Kind != OutcomeInfra {
		t.Fatal("infra-only seed must be OutcomeInfra")
	}
	if finalizeResult(&SeedResult{}).Kind != OutcomeOK {
		t.Fatal("clean seed must be OutcomeOK")
	}
}

func TestRenderSQL_Shapes(t *testing.T) {
	t.Parallel()
	c := Generate(42)
	for _, q := range c.Queries {
		for _, proj := range c.ProjectionsFor(q) {
			s := c.SQL(q, proj)
			if !strings.HasPrefix(s, "SELECT ") || !strings.Contains(s, " FROM t_rd") {
				t.Fatalf("malformed SQL: %s", s)
			}
		}
	}
	if !strings.Contains(c.DDL(), "CREATE TABLE T_RD") {
		t.Fatalf("malformed DDL: %s", c.DDL())
	}
	if !strings.HasPrefix(c.InsertSQL(), "INSERT INTO T_RD VALUES (") {
		t.Fatalf("malformed INSERT: %s", c.InsertSQL()[:60])
	}
}

func TestRenderLiteral_StringEscaping(t *testing.T) {
	t.Parallel()
	if got := renderLiteral("it's"); got != "'it''s'" {
		t.Fatalf("escaping: got %s", got)
	}
	if got := renderLiteral(nil); got != "NULL" {
		t.Fatalf("nil: got %s", got)
	}
}
