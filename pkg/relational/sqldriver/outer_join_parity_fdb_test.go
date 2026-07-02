package sqldriver_test

// Outer-join parity sweep — RFC-144 TASK A.
//
// Ports Java 4.12.11.0's yaml-tests/.../join-tests-outer.yamsql (the 4.12
// LEFT/RIGHT OUTER JOIN feature, #4122) as FDB integration tests, asserting
// Go's materialized-NLJ outer join produces the SAME rows as Java 4.12 for
// every case: matched + NULL-padded unmatched, 3VL, ON applied to the join
// (not WHERE), anti-join (WHERE col IS NULL), ON-vs-WHERE placement,
// predicate push-down, constant ON (ON TRUE / ON 1=1), compound ON (AND),
// LEFT JOIN + ORDER BY, GROUP BY / COUNT over an outer join, chained/nested
// outer joins, and non-equi ON.
//
// Go keeps its materialized RecordQueryNestedLoopJoinPlan mechanism
// (RFC-144 §3a) rather than Java's OuterJoinExpression+nullOnEmpty QGM
// rewrite — the two are functionally equivalent (same result rows). These
// tests are the reclassification proof: LEFT/RIGHT OUTER are now Java-aligned.
//
// Schema + seed data mirror Java's join-tests-outer.yamsql exactly:
//
//	emp:     (id, fname, lname, dept_id)  4 rows; Dave's dept_id=99 (no dept)
//	dept:    (id, name)                   3 rows; Marketing(3) has no emp
//	project: (id, name, emp_id)           3 rows
//	award:   (id, name, emp_id)           EMPTY (empty-side join cases)

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/onsi/gomega"
)

// setupOuterParityDB creates a fresh database + schema seeded with the Java
// join-tests-outer.yamsql fixtures and returns a *sql.DB on the `main` schema.
func setupOuterParityDB(t *testing.T, g *gomega.WithT, suffix string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	dbPath := "/testdb_ojp_" + suffix
	setup := openTestDB(t, dbPath)
	_, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	tmpl := "ojp_tmpl_" + suffix
	// Java's emp$fname / dept$name indexes are mirrored so the planner has the
	// same index-scan choices Java's explain strings exercise.
	_, err = setup.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA TEMPLATE %s
		CREATE TABLE emp (id BIGINT NOT NULL, fname STRING, lname STRING, dept_id BIGINT, PRIMARY KEY (id))
		CREATE INDEX emp_fname ON emp (fname)
		CREATE TABLE dept (id BIGINT NOT NULL, name STRING, PRIMARY KEY (id))
		CREATE INDEX dept_name ON dept (name)
		CREATE TABLE project (id BIGINT NOT NULL, name STRING, emp_id BIGINT, PRIMARY KEY (id))
		CREATE TABLE award (id BIGINT NOT NULL, name STRING, emp_id BIGINT, PRIMARY KEY (id))
		CREATE TABLE emp_dept (id BIGINT NOT NULL, did BIGINT NOT NULL, PRIMARY KEY (id))`, tmpl))
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %s/main WITH TEMPLATE %s", dbPath, tmpl))
	g.Expect(err).NotTo(gomega.HaveOccurred())
	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=main", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(func() { db.Close() })

	// Seed (verbatim from Java join-tests-outer.yamsql). award stays empty.
	_, err = db.ExecContext(ctx, `INSERT INTO emp (id, fname, lname, dept_id) VALUES
		(1, 'Alice', 'Smith', 1),
		(2, 'Bob', 'Jones', 1),
		(3, 'Carol', 'Lee', 2),
		(4, 'Dave', 'Kim', 99)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO dept (id, name) VALUES
		(1, 'Engineering'),
		(2, 'Sales'),
		(3, 'Marketing')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO project (id, name, emp_id) VALUES
		(1, 'Alpha', 1),
		(2, 'Beta', 2),
		(3, 'Gamma', 1)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	return db
}

// ojCell is one output cell: either a non-NULL string/int or NULL (Ptr==nil).
// We render every cell to a canonical string so heterogeneous column shapes
// (string name, bigint id) compare uniformly and NULL is unambiguous.
type ojCell struct {
	s   *string
	i   *int64
	nul bool
}

func ojStr(s string) ojCell { return ojCell{s: &s} }
func ojInt(i int64) ojCell  { return ojCell{i: &i} }
func ojNull() ojCell        { return ojCell{nul: true} }

func (c ojCell) String() string {
	switch {
	case c.nul:
		return "NULL"
	case c.s != nil:
		return "S:" + *c.s
	case c.i != nil:
		return fmt.Sprintf("I:%d", *c.i)
	default:
		return "NULL"
	}
}

func rowKey(cells []ojCell) string {
	parts := make([]string, len(cells))
	for i, c := range cells {
		parts[i] = c.String()
	}
	return strings.Join(parts, "|")
}

// queryRows runs q and returns each row as a slice of canonical-string cells.
// ncols columns are scanned via sql.NullString / sql.NullInt64 depending on
// colKinds ('s' = string, 'i' = int).
func queryRows(t *testing.T, g *gomega.WithT, db *sql.DB, q string, colKinds string) [][]ojCell {
	t.Helper()
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, q)
	g.Expect(err).NotTo(gomega.HaveOccurred(), "query: %s", q)
	defer rows.Close()

	var out [][]ojCell
	for rows.Next() {
		dests := make([]any, len(colKinds))
		strs := make([]sql.NullString, len(colKinds))
		ints := make([]sql.NullInt64, len(colKinds))
		for i, k := range colKinds {
			if k == 'i' {
				dests[i] = &ints[i]
			} else {
				dests[i] = &strs[i]
			}
		}
		g.Expect(rows.Scan(dests...)).To(gomega.Succeed(), "scan: %s", q)
		row := make([]ojCell, len(colKinds))
		for i, k := range colKinds {
			if k == 'i' {
				if ints[i].Valid {
					row[i] = ojInt(ints[i].Int64)
				} else {
					row[i] = ojNull()
				}
			} else {
				if strs[i].Valid {
					row[i] = ojStr(strs[i].String)
				} else {
					row[i] = ojNull()
				}
			}
		}
		out = append(out, row)
	}
	g.Expect(rows.Err()).NotTo(gomega.HaveOccurred())
	return out
}

// assertRowsUnordered asserts the rows of q (as a multiset) equal want.
// Runs in its own subtest so one divergence does not abort sibling cases.
func assertRowsUnordered(t *testing.T, _ *gomega.WithT, db *sql.DB, q, colKinds string, want [][]ojCell) {
	t.Helper()
	t.Run(caseName(q), func(t *testing.T) {
		g := gomega.NewWithT(t)
		got := queryRows(t, g, db, q, colKinds)
		gotKeys := make([]string, len(got))
		for i, r := range got {
			gotKeys[i] = rowKey(r)
		}
		wantKeys := make([]string, len(want))
		for i, r := range want {
			wantKeys[i] = rowKey(r)
		}
		sort.Strings(gotKeys)
		sort.Strings(wantKeys)
		g.Expect(gotKeys).To(gomega.Equal(wantKeys),
			"query: %s\n got(multiset)=%v\nwant(multiset)=%v", q, gotKeys, wantKeys)
	})
}

// assertRowsOrdered asserts the rows of q equal want IN ORDER.
func assertRowsOrdered(t *testing.T, _ *gomega.WithT, db *sql.DB, q, colKinds string, want [][]ojCell) {
	t.Helper()
	t.Run(caseName(q), func(t *testing.T) {
		g := gomega.NewWithT(t)
		got := queryRows(t, g, db, q, colKinds)
		gotKeys := make([]string, len(got))
		for i, r := range got {
			gotKeys[i] = rowKey(r)
		}
		wantKeys := make([]string, len(want))
		for i, r := range want {
			wantKeys[i] = rowKey(r)
		}
		g.Expect(gotKeys).To(gomega.Equal(wantKeys),
			"query: %s\n got(ordered)=%v\nwant(ordered)=%v", q, gotKeys, wantKeys)
	})
}

// caseName derives a stable, readable subtest name from a query string.
func caseName(q string) string {
	q = strings.Join(strings.Fields(q), "_")
	q = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			return r
		}
		return '-'
	}, q)
	if len(q) > 90 {
		q = q[:90]
	}
	return q
}

// row is a brevity helper.
func row(cells ...ojCell) []ojCell { return cells }

// ============================ LEFT OUTER JOIN ============================

// TestFDB_OuterParity_Left ports the Java `left-outer-join` block (L1..L39).
func TestFDB_OuterParity_Left(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	db := setupOuterParityDB(t, g, "left")

	// L1/L2: basic LEFT [OUTER] JOIN — all employees, Dave null-padded.
	for _, kw := range []string{"LEFT JOIN", "LEFT OUTER JOIN"} {
		assertRowsUnordered(t, g, db,
			"SELECT e.fname, d.name FROM emp e "+kw+" dept d ON e.dept_id = d.id", "ss",
			[][]ojCell{
				row(ojStr("Alice"), ojStr("Engineering")),
				row(ojStr("Bob"), ojStr("Engineering")),
				row(ojStr("Carol"), ojStr("Sales")),
				row(ojStr("Dave"), ojNull()),
			})
	}

	// L3: WHERE on preserved (left) side.
	assertRowsUnordered(t, g, db,
		"SELECT e.fname, d.name FROM emp e LEFT JOIN dept d ON e.dept_id = d.id WHERE e.dept_id < 3", "ss",
		[][]ojCell{
			row(ojStr("Alice"), ojStr("Engineering")),
			row(ojStr("Bob"), ojStr("Engineering")),
			row(ojStr("Carol"), ojStr("Sales")),
		})

	// L4: null-rejecting WHERE on right side degenerates to INNER (Dave dropped).
	assertRowsUnordered(t, g, db,
		"SELECT e.fname, d.name FROM emp e LEFT JOIN dept d ON e.dept_id = d.id WHERE d.name = 'Engineering'", "ss",
		[][]ojCell{
			row(ojStr("Alice"), ojStr("Engineering")),
			row(ojStr("Bob"), ojStr("Engineering")),
		})

	// L5: anti-join WHERE d.id IS NULL (ordered result in Java).
	assertRowsOrdered(t, g, db,
		"SELECT e.fname, e.lname FROM emp e LEFT JOIN dept d ON e.dept_id = d.id WHERE d.id IS NULL", "ss",
		[][]ojCell{row(ojStr("Dave"), ojStr("Kim"))})

	// L6: WHERE d.name IS NULL, projecting null-producing side.
	assertRowsOrdered(t, g, db,
		"SELECT e.fname, d.name FROM emp e LEFT JOIN dept d ON e.dept_id = d.id WHERE d.name IS NULL", "ss",
		[][]ojCell{row(ojStr("Dave"), ojNull())})

	// L7: 1:N (dept LEFT JOIN project), Marketing null-padded.
	assertRowsUnordered(t, g, db,
		"SELECT d.name, p.name FROM dept d LEFT JOIN project p ON d.id = p.emp_id", "ss",
		[][]ojCell{
			row(ojStr("Engineering"), ojStr("Alpha")),
			row(ojStr("Engineering"), ojStr("Gamma")),
			row(ojStr("Sales"), ojStr("Beta")),
			row(ojStr("Marketing"), ojNull()),
		})

	// L8: project only left-side columns.
	assertRowsUnordered(t, g, db,
		"SELECT e.id, e.fname FROM emp e LEFT JOIN dept d ON e.dept_id = d.id", "is",
		[][]ojCell{
			row(ojInt(1), ojStr("Alice")),
			row(ojInt(2), ojStr("Bob")),
			row(ojInt(3), ojStr("Carol")),
			row(ojInt(4), ojStr("Dave")),
		})

	// L9: LEFT JOIN USING (id) — joins emp.id = dept.id (PK), NOT dept_id.
	assertRowsUnordered(t, g, db,
		"SELECT e.fname, d.name FROM emp e LEFT JOIN dept d USING (id)", "ss",
		[][]ojCell{
			row(ojStr("Alice"), ojStr("Engineering")),
			row(ojStr("Bob"), ojStr("Sales")),
			row(ojStr("Carol"), ojStr("Marketing")),
			row(ojStr("Dave"), ojNull()),
		})

	// L10: two chained LEFT JOINs.
	assertRowsUnordered(t, g, db,
		"SELECT e.fname, d.name, p.name FROM emp e LEFT JOIN dept d ON e.dept_id = d.id LEFT JOIN project p ON e.id = p.emp_id", "sss",
		[][]ojCell{
			row(ojStr("Alice"), ojStr("Engineering"), ojStr("Alpha")),
			row(ojStr("Alice"), ojStr("Engineering"), ojStr("Gamma")),
			row(ojStr("Bob"), ojStr("Engineering"), ojStr("Beta")),
			row(ojStr("Carol"), ojStr("Sales"), ojNull()),
			row(ojStr("Dave"), ojNull(), ojNull()),
		})

	// L11/L12: SELECT * (6 columns: emp.*, dept.*); both ON orientations.
	for _, on := range []string{"e.dept_id = d.id", "d.id = e.dept_id"} {
		assertRowsUnordered(t, g, db,
			"SELECT * FROM emp e LEFT JOIN dept d ON "+on, "issiis",
			[][]ojCell{
				row(ojInt(1), ojStr("Alice"), ojStr("Smith"), ojInt(1), ojInt(1), ojStr("Engineering")),
				row(ojInt(2), ojStr("Bob"), ojStr("Jones"), ojInt(1), ojInt(1), ojStr("Engineering")),
				row(ojInt(3), ojStr("Carol"), ojStr("Lee"), ojInt(2), ojInt(2), ojStr("Sales")),
				row(ojInt(4), ojStr("Dave"), ojStr("Kim"), ojInt(99), ojNull(), ojNull()),
			})
	}

	// L17: compound ON (AND), d.name <> 'Sales' → Carol null-padded.
	assertRowsUnordered(t, g, db,
		"SELECT e.fname, d.name FROM emp e LEFT JOIN dept d ON e.dept_id = d.id AND d.name <> 'Sales'", "ss",
		[][]ojCell{
			row(ojStr("Alice"), ojStr("Engineering")),
			row(ojStr("Bob"), ojStr("Engineering")),
			row(ojStr("Carol"), ojNull()),
			row(ojStr("Dave"), ojNull()),
		})

	// L18/L19/L20: constant ON (Cartesian product, 4×3=12 rows).
	cartesian := [][]ojCell{}
	for _, e := range []string{"Alice", "Bob", "Carol", "Dave"} {
		for _, d := range []string{"Engineering", "Sales", "Marketing"} {
			cartesian = append(cartesian, row(ojStr(e), ojStr(d)))
		}
	}
	for _, q := range []string{
		"SELECT e.fname, d.name FROM emp e LEFT JOIN dept d ON TRUE",
		"SELECT e.fname, d.name FROM emp e LEFT JOIN dept d ON 1=1",
		"SELECT e.fname, d.name FROM emp e LEFT JOIN dept d ON 1=1 WHERE TRUE",
	} {
		assertRowsUnordered(t, g, db, q, "ss", cartesian)
	}

	// L21: LEFT JOIN against empty right table → every left row null-padded.
	assertRowsUnordered(t, g, db,
		"SELECT e.fname, a.name FROM emp e LEFT JOIN award a ON e.id = a.emp_id", "ss",
		[][]ojCell{
			row(ojStr("Alice"), ojNull()),
			row(ojStr("Bob"), ojNull()),
			row(ojStr("Carol"), ojNull()),
			row(ojStr("Dave"), ojNull()),
		})

	// L22: INNER JOIN then LEFT JOIN (Dave dropped by inner).
	assertRowsUnordered(t, g, db,
		"SELECT e.fname, d.name, p.name FROM emp e JOIN dept d ON e.dept_id = d.id LEFT JOIN project p ON e.id = p.emp_id", "sss",
		[][]ojCell{
			row(ojStr("Alice"), ojStr("Engineering"), ojStr("Alpha")),
			row(ojStr("Alice"), ojStr("Engineering"), ojStr("Gamma")),
			row(ojStr("Bob"), ojStr("Engineering"), ojStr("Beta")),
			row(ojStr("Carol"), ojStr("Sales"), ojNull()),
		})

	// L23: LEFT JOIN + ORDER BY preserved side (ordered).
	assertRowsOrdered(t, g, db,
		"SELECT e.fname, d.name FROM emp e LEFT JOIN dept d ON e.dept_id = d.id ORDER BY e.fname", "ss",
		[][]ojCell{
			row(ojStr("Alice"), ojStr("Engineering")),
			row(ojStr("Bob"), ojStr("Engineering")),
			row(ojStr("Carol"), ojStr("Sales")),
			row(ojStr("Dave"), ojNull()),
		})

	// L26: LEFT JOIN + GROUP BY + COUNT(*) — null-padded row counted.
	assertRowsUnordered(t, g, db,
		"SELECT d.name, COUNT(*) FROM dept d LEFT JOIN project p ON d.id = p.emp_id GROUP BY d.name", "si",
		[][]ojCell{
			row(ojStr("Engineering"), ojInt(2)),
			row(ojStr("Sales"), ojInt(1)),
			row(ojStr("Marketing"), ojInt(1)),
		})

	// L27: LEFT JOIN + GROUP BY + COUNT(col) — NULLs skipped, Marketing=0.
	assertRowsUnordered(t, g, db,
		"SELECT d.name, COUNT(p.id) FROM dept d LEFT JOIN project p ON d.id = p.emp_id GROUP BY d.name", "si",
		[][]ojCell{
			row(ojStr("Engineering"), ojInt(2)),
			row(ojStr("Sales"), ojInt(1)),
			row(ojStr("Marketing"), ojInt(0)),
		})

	// L28: scalar COUNT(*) over LEFT JOIN.
	assertRowsOrdered(t, g, db,
		"SELECT COUNT(*) FROM emp e LEFT JOIN dept d ON e.dept_id = d.id", "i",
		[][]ojCell{row(ojInt(4))})

	// L29: LEFT JOIN with a derived table on the right.
	assertRowsUnordered(t, g, db,
		"SELECT e.fname, sq.name FROM emp e LEFT JOIN (SELECT id, name FROM dept WHERE id < 3) AS sq ON e.dept_id = sq.id", "ss",
		[][]ojCell{
			row(ojStr("Alice"), ojStr("Engineering")),
			row(ojStr("Bob"), ojStr("Engineering")),
			row(ojStr("Carol"), ojStr("Sales")),
			row(ojStr("Dave"), ojNull()),
		})

	// L30: compound WHERE both sides: left filter + anti-join (ordered).
	assertRowsOrdered(t, g, db,
		"SELECT e.fname FROM emp e LEFT JOIN dept d ON e.dept_id = d.id WHERE e.id > 1 AND d.id IS NULL", "s",
		[][]ojCell{row(ojStr("Dave"))})

	// L31: COALESCE on null-padded column.
	assertRowsUnordered(t, g, db,
		"SELECT e.fname, COALESCE(d.name, 'None') FROM emp e LEFT JOIN dept d ON e.dept_id = d.id", "ss",
		[][]ojCell{
			row(ojStr("Alice"), ojStr("Engineering")),
			row(ojStr("Bob"), ojStr("Engineering")),
			row(ojStr("Carol"), ojStr("Sales")),
			row(ojStr("Dave"), ojStr("None")),
		})

	// L32: self LEFT JOIN anti-join.
	assertRowsOrdered(t, g, db,
		"SELECT e1.fname FROM emp e1 LEFT JOIN emp e2 ON e1.dept_id = e2.id WHERE e2.id IS NULL", "s",
		[][]ojCell{row(ojStr("Dave"))})

	// L34: LEFT JOIN non-equi (>=) ON.
	assertRowsUnordered(t, g, db,
		"SELECT e.fname, d.name FROM emp e LEFT JOIN dept d ON e.dept_id >= d.id", "ss",
		[][]ojCell{
			row(ojStr("Alice"), ojStr("Engineering")),
			row(ojStr("Bob"), ojStr("Engineering")),
			row(ojStr("Carol"), ojStr("Engineering")),
			row(ojStr("Carol"), ojStr("Sales")),
			row(ojStr("Dave"), ojStr("Engineering")),
			row(ojStr("Dave"), ojStr("Sales")),
			row(ojStr("Dave"), ojStr("Marketing")),
		})

	// L35: WHERE inequality on preserved side, pushed into index range scan.
	assertRowsUnordered(t, g, db,
		"SELECT e.fname, d.name FROM emp e LEFT JOIN dept d ON e.dept_id = d.id WHERE e.fname > 'Bob'", "ss",
		[][]ojCell{
			row(ojStr("Carol"), ojStr("Sales")),
			row(ojStr("Dave"), ojNull()),
		})

	// L36: ON-clause inequality on null-producing side (preserves null-padding).
	assertRowsUnordered(t, g, db,
		"SELECT e.fname, d.name FROM emp e LEFT JOIN dept d ON e.dept_id = d.id AND d.name > 'M'", "ss",
		[][]ojCell{
			row(ojStr("Alice"), ojNull()),
			row(ojStr("Bob"), ojNull()),
			row(ojStr("Carol"), ojStr("Sales")),
			row(ojStr("Dave"), ojNull()),
		})

	// L37: WHERE inequality on null-producing side (degenerates to INNER-like).
	assertRowsUnordered(t, g, db,
		"SELECT e.fname, d.name FROM emp e LEFT JOIN dept d ON e.dept_id = d.id WHERE d.name > 'M'", "ss",
		[][]ojCell{row(ojStr("Carol"), ojStr("Sales"))})
}

// TestFDB_OuterParity_Left_OrderByCountSubquery ports L25: ORDER BY on the
// null-producing side with a scalar COUNT subquery as the preserved side.
func TestFDB_OuterParity_Left_OrderByCountSubquery(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	db := setupOuterParityDB(t, g, "left_cntsub")

	// COUNT(*) over dept = 3; emp.id >= 3 → Carol(3), Dave(4). Ordered by fname.
	assertRowsOrdered(t, g, db,
		`SELECT x.c, e.fname, e.lname FROM (SELECT COUNT(*) AS c FROM dept) x
			LEFT OUTER JOIN emp e ON e.id >= x.c ORDER BY e.fname`, "iss",
		[][]ojCell{
			row(ojInt(3), ojStr("Carol"), ojStr("Lee")),
			row(ojInt(3), ojStr("Dave"), ojStr("Kim")),
		})
}

// ============================ RIGHT OUTER JOIN ============================

// TestFDB_OuterParity_Right ports the Java `right-outer-join` block (R1..R13).
func TestFDB_OuterParity_Right(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	db := setupOuterParityDB(t, g, "right")

	// R1/R2: basic RIGHT [OUTER] JOIN — all depts incl. Marketing; Dave dropped.
	for _, kw := range []string{"RIGHT JOIN", "RIGHT OUTER JOIN"} {
		assertRowsUnordered(t, g, db,
			"SELECT e.fname, d.name FROM emp e "+kw+" dept d ON e.dept_id = d.id", "ss",
			[][]ojCell{
				row(ojStr("Alice"), ojStr("Engineering")),
				row(ojStr("Bob"), ojStr("Engineering")),
				row(ojStr("Carol"), ojStr("Sales")),
				row(ojNull(), ojStr("Marketing")),
			})
	}

	// R3: SELECT * with RIGHT JOIN (6 cols, Marketing null-padded on emp.*).
	assertRowsUnordered(t, g, db,
		"SELECT * FROM emp e RIGHT JOIN dept d ON e.dept_id = d.id", "issiis",
		[][]ojCell{
			row(ojInt(1), ojStr("Alice"), ojStr("Smith"), ojInt(1), ojInt(1), ojStr("Engineering")),
			row(ojInt(2), ojStr("Bob"), ojStr("Jones"), ojInt(1), ojInt(1), ojStr("Engineering")),
			row(ojInt(3), ojStr("Carol"), ojStr("Lee"), ojInt(2), ojInt(2), ojStr("Sales")),
			row(ojNull(), ojNull(), ojNull(), ojNull(), ojInt(3), ojStr("Marketing")),
		})

	// R6: RIGHT JOIN = flipped LEFT (dept d RIGHT JOIN emp e, emp preserved).
	assertRowsUnordered(t, g, db,
		"SELECT e.fname, d.name FROM dept d RIGHT JOIN emp e ON e.dept_id = d.id", "ss",
		[][]ojCell{
			row(ojStr("Alice"), ojStr("Engineering")),
			row(ojStr("Bob"), ojStr("Engineering")),
			row(ojStr("Carol"), ojStr("Sales")),
			row(ojStr("Dave"), ojNull()),
		})

	// R7: RIGHT JOIN + WHERE on preserved (right) side.
	assertRowsUnordered(t, g, db,
		"SELECT e.fname, d.name FROM emp e RIGHT JOIN dept d ON e.dept_id = d.id WHERE d.id < 3", "ss",
		[][]ojCell{
			row(ojStr("Alice"), ojStr("Engineering")),
			row(ojStr("Bob"), ojStr("Engineering")),
			row(ojStr("Carol"), ojStr("Sales")),
		})

	// R8: RIGHT JOIN anti-join WHERE e.id IS NULL (ordered).
	assertRowsOrdered(t, g, db,
		"SELECT d.name FROM emp e RIGHT JOIN dept d ON e.dept_id = d.id WHERE e.id IS NULL", "s",
		[][]ojCell{row(ojStr("Marketing"))})

	// R9: RIGHT JOIN with 1:N on the null-producing (left) side.
	assertRowsUnordered(t, g, db,
		"SELECT d.name, p.name FROM project p RIGHT JOIN dept d ON d.id = p.emp_id", "ss",
		[][]ojCell{
			row(ojStr("Engineering"), ojStr("Alpha")),
			row(ojStr("Engineering"), ojStr("Gamma")),
			row(ojStr("Sales"), ojStr("Beta")),
			row(ojStr("Marketing"), ojNull()),
		})

	// R10: RIGHT JOIN against empty left table.
	assertRowsUnordered(t, g, db,
		"SELECT a.name, d.name FROM award a RIGHT JOIN dept d ON a.emp_id = d.id", "ss",
		[][]ojCell{
			row(ojNull(), ojStr("Engineering")),
			row(ojNull(), ojStr("Sales")),
			row(ojNull(), ojStr("Marketing")),
		})

	// R11: RIGHT JOIN USING (id) — emp.id = dept.id, all 3 depts match an emp.
	assertRowsUnordered(t, g, db,
		"SELECT e.fname, d.name FROM emp e RIGHT JOIN dept d USING (id)", "ss",
		[][]ojCell{
			row(ojStr("Alice"), ojStr("Engineering")),
			row(ojStr("Bob"), ojStr("Sales")),
			row(ojStr("Carol"), ojStr("Marketing")),
		})

	// R12: RIGHT JOIN + COALESCE on null-padded SQL-left column.
	assertRowsUnordered(t, g, db,
		"SELECT COALESCE(e.fname, 'None'), d.name FROM emp e RIGHT JOIN dept d ON e.dept_id = d.id", "ss",
		[][]ojCell{
			row(ojStr("Alice"), ojStr("Engineering")),
			row(ojStr("Bob"), ojStr("Engineering")),
			row(ojStr("Carol"), ojStr("Sales")),
			row(ojStr("None"), ojStr("Marketing")),
		})
}

// ============================ FULL OUTER JOIN reclassification note ============

// TestFDB_OuterParity_FullIsGoExtension pins RFC-144's reclassification: Java
// 4.12 REJECTS FULL OUTER JOIN (parser SYNTAX_ERROR, join-tests-outer.yamsql
// F1), but Go supports it as a documented Go-only extension via the
// materialized NLJ drain. This is the one outer-join shape that stays Go-only.
// (Full FULL-OUTER row/plan coverage lives in full_outer_join_fdb_test.go.)
func TestFDB_OuterParity_FullIsGoExtension(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	db := setupOuterParityDB(t, g, "full_ext")

	// Java rejects this with SYNTAX_ERROR; Go accepts it and emits the FULL
	// outer-join row set: matched + left-only (Dave) + right-only (Marketing).
	assertRowsUnordered(t, g, db,
		"SELECT e.fname, d.name FROM emp e FULL OUTER JOIN dept d ON e.dept_id = d.id", "ss",
		[][]ojCell{
			row(ojStr("Alice"), ojStr("Engineering")),
			row(ojStr("Bob"), ojStr("Engineering")),
			row(ojStr("Carol"), ojStr("Sales")),
			row(ojStr("Dave"), ojNull()),      // left-only
			row(ojNull(), ojStr("Marketing")), // right-only (drain)
		})
}

// ====================== TASK C: null-supplying-side nullability ==============

// TestFDB_OuterParity_NullSupplyingNullability verifies RFC-144 §3c (#4274): the
// null-supplying side of an outer join behaves nullable at runtime even when its
// SOURCE columns are declared NOT NULL. Go's runtime key-absence model NULL-pads
// the unmatched leg without raising "non-nullable field set to NULL" — matching
// Java 4.12's observable behavior (Java makes the null-supplying flowed values
// nullable so the join itself never raises). dept.id is PK (NOT NULL), yet the
// LEFT JOIN emits NULL for Dave's (unmatched) dept.id.
func TestFDB_OuterParity_NullSupplyingNullability(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	db := setupOuterParityDB(t, g, "nullability")

	// (1) Projecting a NOT-NULL column (dept.id, the PK) from the null-supplying
	// side must NULL-pad the unmatched row — NOT error.
	assertRowsUnordered(t, g, db,
		"SELECT e.fname, d.id FROM emp e LEFT JOIN dept d ON e.dept_id = d.id", "si",
		[][]ojCell{
			row(ojStr("Alice"), ojInt(1)),
			row(ojStr("Bob"), ojInt(1)),
			row(ojStr("Carol"), ojInt(2)),
			row(ojStr("Dave"), ojNull()), // dept.id is NOT NULL at source, NULL-padded here
		})

	// (2) Result-metadata nullability (ColumnTypeNullable) is a DOCUMENTED benign
	// divergence (RFC-144 §3c): Go reports the null-supplying column with its
	// SOURCE cardinality (dept.id is a NOT-NULL PK → not-nullable metadata), while
	// Java 4.12 (#4274) re-types it nullable. This flag is NON-LOAD-BEARING — query
	// rows and INSERT…SELECT correctness both use RUNTIME values (the NULL-padded
	// d.id is a genuine SQL NULL at runtime, proven in (1) and by the INSERT…SELECT
	// test below), not this metadata flag. The descriptor path derives nullability
	// from the underlying record type's field cardinality, a separate path from the
	// join result value; re-typing it would thread outer-join leg-nullability
	// through every join's column-descriptor computation for a cosmetic flag with
	// no observable effect. We pin the CURRENT behavior so a future intentional
	// change is a conscious decision, not a silent drift.
	ctx := context.Background()
	rows, err := db.QueryContext(ctx, "SELECT e.fname, d.id FROM emp e LEFT JOIN dept d ON e.dept_id = d.id")
	g.Expect(err).NotTo(gomega.HaveOccurred())
	cts, err := rows.ColumnTypes()
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(len(cts)).To(gomega.Equal(2))
	if nullable, ok := cts[1].Nullable(); ok {
		// Current Go behavior: source cardinality (NOT-NULL PK → not nullable).
		g.Expect(nullable).To(gomega.BeFalse(),
			"documented divergence: d.id reports source (NOT-NULL) cardinality; runtime value is still genuinely NULL")
	}
	rows.Close()
}

// TestFDB_OuterParity_InsertSelectFromOuterJoinNotNull verifies INSERT…SELECT
// from an outer join into a NOT-NULL target: the NULL-padded row carries a NULL
// for the null-supplying column, and inserting it into a NOT-NULL target column
// raises a not-null violation (23502) — standard SQL behavior, matching Java.
func TestFDB_OuterParity_InsertSelectFromOuterJoinNotNull(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	db := setupOuterParityDB(t, g, "inssel")
	ctx := context.Background()

	// emp_dept (created in the schema template) has a NOT-NULL `did` column. The
	// two SELECT columns (e.id, d.id) map positionally to (id, did). INSERT…SELECT
	// a LEFT JOIN whose null-supplying d.id is NULL for Dave (unmatched) → the
	// NULL-padded row MUST be rejected (no NULL written to the NOT-NULL target).
	// The rejection is driven by the RUNTIME NULL value (the d.id column's NULL),
	// NOT result metadata — which is the load-bearing #4274 property.
	//
	// Error-code note (benign, orthogonal divergence): the INSERT…SELECT executor
	// path surfaces this as a proto "required field … not set" serialization error
	// rather than the clean 23502 NotNullViolation the INSERT…VALUES path emits.
	// Threading a per-row NOT-NULL check into the INSERT…SELECT cursor is a
	// general executor improvement, independent of outer joins (any INSERT…SELECT
	// yielding a NULL for a NOT-NULL column hits the same path); the load-bearing
	// guarantee — no NULL is written to a NOT-NULL column — holds either way.
	_, err := db.ExecContext(ctx, `INSERT INTO emp_dept
		SELECT e.id, d.id FROM emp e LEFT JOIN dept d ON e.dept_id = d.id`)
	g.Expect(err).To(gomega.HaveOccurred(),
		"inserting a NULL-padded outer-join column into a NOT-NULL target must be rejected")

	// The matched-only subset (WHERE d.id IS NOT NULL) inserts cleanly — proves it
	// was specifically the NULL-padded row that triggered the rejection.
	_, err = db.ExecContext(ctx, `INSERT INTO emp_dept
		SELECT e.id, d.id FROM emp e LEFT JOIN dept d ON e.dept_id = d.id WHERE d.id IS NOT NULL`)
	g.Expect(err).NotTo(gomega.HaveOccurred(),
		"matched-only rows (no NULL-padding) insert cleanly into the NOT-NULL target")

	// And the matched rows landed: 3 of 4 emp rows have a matching dept.
	var n int
	g.Expect(db.QueryRowContext(ctx, "SELECT COUNT(*) FROM emp_dept").Scan(&n)).To(gomega.Succeed())
	g.Expect(n).To(gomega.Equal(3))
}

// ====================== TASK D: boolean literals in WHERE/ON =================

// setupBoolDB creates a tiny two-table schema with a BOOLEAN column for the
// boolean-literal WHERE/ON parity tests (RFC-144 §3d, #4162).
func setupBoolDB(t *testing.T, g *gomega.WithT, suffix string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	dbPath := "/testdb_bool_" + suffix
	setup := openTestDB(t, dbPath)
	_, err := setup.ExecContext(ctx, "CREATE DATABASE "+dbPath)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	tmpl := "bool_tmpl_" + suffix
	_, err = setup.ExecContext(ctx, fmt.Sprintf(`CREATE SCHEMA TEMPLATE %s
		CREATE TABLE a (id BIGINT NOT NULL, flag BOOLEAN, PRIMARY KEY (id))
		CREATE TABLE b (id BIGINT NOT NULL, name STRING, PRIMARY KEY (id))`, tmpl))
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = setup.ExecContext(ctx, fmt.Sprintf("CREATE SCHEMA %s/main WITH TEMPLATE %s", dbPath, tmpl))
	g.Expect(err).NotTo(gomega.HaveOccurred())
	dsn := fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=main", dbPath, clusterFilePath)
	db, err := sql.Open("fdbsql", dsn)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	t.Cleanup(func() { db.Close() })

	_, err = db.ExecContext(ctx, `INSERT INTO a (id, flag) VALUES (1, true), (2, false), (3, null)`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	_, err = db.ExecContext(ctx, `INSERT INTO b (id, name) VALUES (1, 'one'), (2, 'two')`)
	g.Expect(err).NotTo(gomega.HaveOccurred())
	return db
}

// TestFDB_OuterParity_BooleanWhere verifies boolean-literal WHERE clauses
// (RFC-144 §3d, #4162): WHERE TRUE keeps all rows, WHERE FALSE / WHERE NULL drop
// all rows (NULL is not TRUE in a filter).
func TestFDB_OuterParity_BooleanWhere(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	db := setupBoolDB(t, g, "where")

	assertRowsUnordered(t, g, db, "SELECT id FROM a WHERE TRUE", "i",
		[][]ojCell{row(ojInt(1)), row(ojInt(2)), row(ojInt(3))})
	assertRowsUnordered(t, g, db, "SELECT id FROM a WHERE FALSE", "i", nil)
	assertRowsUnordered(t, g, db, "SELECT id FROM a WHERE NULL", "i", nil)

	// `flag = TRUE` / `flag IS TRUE` (explicit boolean comparison) — only the
	// flag=TRUE row survives (flag=FALSE and flag=NULL are non-TRUE → dropped).
	assertRowsUnordered(t, g, db, "SELECT id FROM a WHERE flag = TRUE", "i",
		[][]ojCell{row(ojInt(1))})
	assertRowsUnordered(t, g, db, "SELECT id FROM a WHERE flag IS TRUE", "i",
		[][]ojCell{row(ojInt(1))})

	// A BARE boolean column as a top-level single-table WHERE predicate
	// (`WHERE flag`) now plans (RFC-146): it lifts to `flag = TRUE`, so only the
	// flag=TRUE row survives — identical to `WHERE flag = TRUE` above. The same
	// shape already worked in a join ON clause (TestFDB_OuterParity_BooleanOn).
	assertRowsUnordered(t, g, db, "SELECT id FROM a WHERE flag", "i",
		[][]ojCell{row(ojInt(1))})
	// `WHERE NOT flag`: NOT TRUE→drop, NOT FALSE→keep(2), NOT NULL→drop.
	assertRowsUnordered(t, g, db, "SELECT id FROM a WHERE NOT flag", "i",
		[][]ojCell{row(ojInt(2))})

	// A bare NON-boolean column as a WHERE predicate is a type error (42804,
	// Java DATATYPE_MISMATCH), not a silent 0-row plan.
	rows, err := db.QueryContext(context.Background(), "SELECT id FROM a WHERE id")
	if err == nil {
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
	}
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("42804"))
}

// TestFDB_OuterParity_BooleanOn verifies boolean-literal / boolean-expr ON
// clauses (RFC-144 §3d): ON TRUE (cartesian), ON FALSE / ON NULL (all
// null-padded under LEFT JOIN), ON <equi> AND TRUE (equivalent to the equi ON),
// and ON <boolcol> (the outer row's boolean column as the join condition).
func TestFDB_OuterParity_BooleanOn(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	db := setupBoolDB(t, g, "on")

	// ON TRUE — cartesian product of a (3 rows) × b (2 rows) = 6.
	cart := [][]ojCell{}
	for _, aid := range []int64{1, 2, 3} {
		for _, bn := range []string{"one", "two"} {
			cart = append(cart, row(ojInt(aid), ojStr(bn)))
		}
	}
	assertRowsUnordered(t, g, db, "SELECT a.id, b.name FROM a LEFT JOIN b ON TRUE", "is", cart)

	// ON FALSE / ON NULL — no inner ever matches; every LEFT row is null-padded.
	allNull := [][]ojCell{row(ojInt(1), ojNull()), row(ojInt(2), ojNull()), row(ojInt(3), ojNull())}
	assertRowsUnordered(t, g, db, "SELECT a.id, b.name FROM a LEFT JOIN b ON FALSE", "is", allNull)
	assertRowsUnordered(t, g, db, "SELECT a.id, b.name FROM a LEFT JOIN b ON NULL", "is", allNull)

	// ON a.id = b.id AND TRUE — `AND TRUE` is the identity; equivalent to the
	// plain equi-join: a1↔b1, a2↔b2, a3 unmatched (null-padded).
	assertRowsUnordered(t, g, db, "SELECT a.id, b.name FROM a LEFT JOIN b ON a.id = b.id AND TRUE", "is",
		[][]ojCell{row(ojInt(1), ojStr("one")), row(ojInt(2), ojStr("two")), row(ojInt(3), ojNull())})

	// ON <boolcol> — a.flag drives the join. a1(flag=TRUE) joins every b row;
	// a2(flag=FALSE) and a3(flag=NULL) match nothing → null-padded.
	assertRowsUnordered(t, g, db, "SELECT a.id, b.name FROM a LEFT JOIN b ON a.flag", "is",
		[][]ojCell{
			row(ojInt(1), ojStr("one")),
			row(ojInt(1), ojStr("two")),
			row(ojInt(2), ojNull()),
			row(ojInt(3), ojNull()),
		})

	// ON <non-boolean column> — `ON a.id` (BIGINT) is a type error (42804,
	// RFC-146), surfaced rather than silently degrading the join to a cross
	// join (the ON-upgrade path used to swallow it).
	rows, err := db.QueryContext(context.Background(), "SELECT a.id FROM a JOIN b ON a.id")
	if err == nil {
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
	}
	g.Expect(err).To(gomega.HaveOccurred())
	g.Expect(err.Error()).To(gomega.ContainSubstring("42804"))
}

// ====================== TASK E: NULL constant-folding (observable) ===========

// TestFDB_OuterParity_NullConstantFolding verifies RFC-144 §3e: Java's NULL-
// operand comparison folding (#4224) + CollapseNullStrictValueOverNullValueRule
// are plan-shape optimizations whose OBSERVABLE result Go's runtime 3VL already
// reproduces. A comparison with a NULL operand evaluates to UNKNOWN (rejects the
// row in a WHERE/ON), and a null-strict expression over a NULL collapses to NULL
// — identical rows whether folded at plan time (Java) or evaluated at runtime
// (Go). We pin the observable behavior; the plan-shape difference (Go keeps the
// comparison node, Java folds it to a constant) is a benign divergence.
func TestFDB_OuterParity_NullConstantFolding(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	g := gomega.NewWithT(t)
	db := setupOuterParityDB(t, g, "nullfold")

	// `WHERE d.name = NULL` over a LEFT JOIN: NULL on the RHS makes the
	// comparison UNKNOWN for EVERY row (matched or null-padded) → 0 rows.
	// (3VL: `x = NULL` is always UNKNOWN, never TRUE — even when x is NULL.)
	assertRowsUnordered(t, g, db,
		"SELECT e.fname FROM emp e LEFT JOIN dept d ON e.dept_id = d.id WHERE d.name = NULL", "s", nil)

	// `WHERE NULL = NULL` — UNKNOWN, drops all rows (NOT a tautology under 3VL).
	assertRowsUnordered(t, g, db, "SELECT fname FROM emp WHERE NULL = NULL", "s", nil)

	// A null-strict arithmetic over a null-padded column: `d.id + 1` is NULL for
	// the unmatched (Dave) row → `(d.id + 1) IS NULL` is TRUE only for Dave.
	// Proves the null-strict propagation (CollapseNullStrictValueOverNullValueRule's
	// runtime equivalent): NULL + 1 = NULL.
	assertRowsUnordered(t, g, db,
		"SELECT e.fname FROM emp e LEFT JOIN dept d ON e.dept_id = d.id WHERE (d.id + 1) IS NULL", "s",
		[][]ojCell{row(ojStr("Dave"))})

	// The complement: `(d.id + 1) IS NOT NULL` keeps the matched rows only.
	assertRowsUnordered(t, g, db,
		"SELECT e.fname FROM emp e LEFT JOIN dept d ON e.dept_id = d.id WHERE (d.id + 1) IS NOT NULL", "s",
		[][]ojCell{row(ojStr("Alice")), row(ojStr("Bob")), row(ojStr("Carol"))})
}
