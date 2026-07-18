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
		for _, proj := range c.Projections() {
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
