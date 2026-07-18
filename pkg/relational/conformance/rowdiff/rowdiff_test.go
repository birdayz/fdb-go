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
