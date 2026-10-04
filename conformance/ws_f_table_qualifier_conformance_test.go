//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/pkg/relational/api"
)

// RFC-257 WS-F 9.2: a table's qualifier. The target reads the one qualifier a
// table name may carry as the name of the connection's SCHEMA TEMPLATE, the
// semantic analyzer's metadata catalog, compared exactly
// (SemanticAnalyzer.tableExists and getTable, metadataCatalog.getName()).
// Where a table is required (INSERT, UPDATE, DELETE: getTable) any other
// qualifier is 42F00 "Unknown schema template <q>" and more than one is an
// internal error; where a FROM source is looked up (LogicalOperator's
// generateAccess: a CTE, then tableExists, then a view, then a function, then
// a correlated field) a qualified name that is not the template's falls
// through to the correlated reading, which is what makes `FROM a.arr` over an
// alias `a` work.
//
// Each statement is run on both engines, with `{T}` standing for each engine's
// template name and `{t}` for it in lower case, and must answer the same;
// then the rows must read the same.
var _ = Describe("RFC-257 WS-F: a table's qualifier is its schema template's name", func() {
	It("resolves a qualified table name as the target does", func() {
		ctx := context.Background()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		clusterFilePath := writeClusterFileToTemp(clusterFile)
		defer os.Remove(clusterFilePath)
		p := "WSFQUAL" + strings.ToUpper(strings.ReplaceAll(uuid.New().String()[:8], "-", ""))
		const body = `create table w(id bigint, f bigint, arr bigint array, primary key(id)) ` +
			`create type as struct s(f bigint, g string) ` +
			`create table x(id bigint, f bigint, x s, primary key(id)) ` +
			`create table y(id bigint, h s, primary key(id)) ` +
			`create table h(id bigint, f bigint, primary key(id)) ` +
			`create type as struct sst(ss bigint) ` +
			`create table ss(id bigint, ss sst, primary key(id)) ` +
			`create type as struct nn(sk bigint, co string) ` +
			`create table nt(id bigint, sk bigint, n nn, primary key(id)) ` +
			`create type as struct na(arr bigint array) ` +
			`create table t2(id bigint, n na, primary key(id)) ` +
			`create type as struct e(b bigint) ` +
			`create table ra(id bigint, items e array, primary key(id))`
		setup := []string{
			`INSERT INTO w VALUES (1, 1, [10, 11])`,
			`INSERT INTO w VALUES (2, 2, [20])`,
			`INSERT INTO x VALUES (1, 5, (50, 'x'))`,
			`INSERT INTO y VALUES (1, (60, 'y'))`,
			`INSERT INTO h VALUES (1, 7)`,
			`INSERT INTO ss VALUES (1, (8))`,
			`INSERT INTO t2 VALUES (1, ([10, 20]))`,
			`INSERT INTO nt VALUES (1, 1, (11, 'aa'))`,
			`INSERT INTO ra VALUES (1, [(3), (4)])`,
		}
		queries := []string{
			`SELECT id FROM {T}.w ORDER BY id`,
			`SELECT id FROM S.w ORDER BY id`,
			`SELECT id FROM nosuch.w ORDER BY id`,
			`SELECT id FROM a.b.w ORDER BY id`,
			`SELECT id FROM {t}.w ORDER BY id`,
			`SELECT id FROM "{t}".w ORDER BY id`,
			`SELECT id FROM "{T}".w ORDER BY id`,
			`SELECT w.id FROM {T}.w ORDER BY id`,
			`SELECT {T}.w.id FROM {T}.w ORDER BY id`,
			`SELECT a.id FROM {T}.w AS a ORDER BY id`,
			`SELECT id FROM w WHERE id IN (SELECT id FROM {T}.w) ORDER BY id`,
			`WITH c AS (SELECT id FROM w) SELECT id FROM {T}.c`,
			`SELECT id FROM {T}.nosuch`,
			`SELECT id FROM S.nosuch`,
			`SELECT a.id FROM w AS a, a.arr AS x ORDER BY a.id`,
			`SELECT a.id FROM {T}.w AS a, a.arr AS x ORDER BY a.id`,
			`SELECT id FROM w, w.arr AS x ORDER BY id`,
			// An unaliased qualified table's column qualifier, in each clause.
			`SELECT id FROM {T}.w WHERE w.id = 1`,
			`SELECT id FROM {T}.w WHERE {T}.w.id = 1`,
			`SELECT w.* FROM {T}.w ORDER BY id`,
			`SELECT {T}.w.* FROM {T}.w ORDER BY id`,
			`SELECT {T}.w.id FROM {T}.w AS a ORDER BY id`,
			`SELECT id FROM {T}.w ORDER BY w.id`,
			`SELECT id FROM {T}.w ORDER BY {T}.w.id`,
			`SELECT w.id, COUNT(*) FROM {T}.w GROUP BY w.id`,
			`SELECT {T}.w.id, COUNT(*) FROM {T}.w GROUP BY {T}.w.id`,
			`SELECT {T}.w.id, b.id FROM {T}.w, w AS b WHERE {T}.w.id = b.id ORDER BY b.id`,
			`SELECT b.id FROM {T}.w, w AS b WHERE w.id = b.id ORDER BY b.id`,
			`SELECT id FROM {T}.w AS a WHERE EXISTS (SELECT 1 FROM {T}.w WHERE {T}.w.id = a.id) ORDER BY id`,
			`SELECT d.id FROM (SELECT id FROM {T}.w) AS d ORDER BY d.id`,
			// The table named twice before a column: Java's qualified lookup
			// prepends the operator's name to the attribute's qualified name,
			// while the table operator is named (the WHERE), not in the select
			// list.
			`SELECT id FROM w WHERE w.w.id = 1`,
			`SELECT w.w.id FROM w ORDER BY id`,
			`SELECT id FROM {T}.w WHERE w.{T}.w.id = 1`,
			// Java's two lookups: a qualified reading first, the struct-relative
			// one only when it finds nothing.
			`SELECT x.f FROM x`,
			`SELECT id FROM x WHERE x.f = 5`,
			`SELECT x.x.f FROM x`,
			`SELECT h.f FROM y, h`,
			`SELECT ss.ss.ss FROM ss`,
			`SELECT w.w.f FROM w ORDER BY id`,
			// An alias spelled like a struct column: the qualified reading (the
			// alias's flat sk) before the struct-relative one (the column's sk).
			`SELECT n.sk FROM nt AS n ORDER BY n.id`,
			// Two sources carrying the struct column n: two struct-relative
			// candidates, ambiguous in both engines.
			`SELECT n.sk FROM nt AS a, nt AS b`,
			// Both unaliased, one qualified: two names.
			`SELECT {T}.w.id, w.id FROM {T}.w, w WHERE {T}.w.id = w.id ORDER BY w.id`,
			// A table's alias.column path where the table is not a visible
			// source (behind a derived table, the right side of a JOIN): no
			// table and no correlated field.
			`SELECT x FROM (SELECT id FROM w) AS d, w.arr AS x`,
			// An explicit join's legs named by their qualified identifiers.
			`SELECT {T}.w.id, b.id FROM {T}.w JOIN w AS b ON {T}.w.id = b.id ORDER BY b.id`,
			`SELECT {T}.w.id FROM {T}.w LEFT JOIN {T}.h ON {T}.w.id = {T}.h.id ORDER BY {T}.w.id`,
			// An aggregate block's select list, GROUP BY, HAVING and ORDER BY
			// resolve against Java's generateSelectWhere operator, which has
			// preserved names and no name: no doubled reading, and a struct
			// column's field (x.x's f) counts beside the qualified x.f. Its
			// WHERE resolves before, against the named sources.
			`SELECT x.f, COUNT(*) FROM x GROUP BY x.f`,
			`SELECT MAX(x.f) FROM x`,
			`SELECT COUNT(x.f) FROM x`,
			`SELECT COUNT(*) FROM x WHERE x.f = 5`,
			`SELECT x.f FROM x GROUP BY x.f ORDER BY x.f`,
			`SELECT f FROM x GROUP BY f HAVING MAX(x.f) > 0`,
			`SELECT f FROM x GROUP BY f ORDER BY x.f`,
			`SELECT n.sk, COUNT(*) FROM nt AS n GROUP BY n.sk`,
			`SELECT MAX(w.w.f) FROM w`,
			`SELECT w.w.f, COUNT(*) FROM w GROUP BY w.w.f`,
			`SELECT f FROM w GROUP BY f HAVING MAX(w.w.f) > 0`,
			`SELECT COUNT(*) FROM w WHERE w.w.f = 1`,
			`SELECT COUNT(*) FROM x GROUP BY x.id`,
			`SELECT x.id, COUNT(*) FROM x GROUP BY x.id`,
			// Without a GROUP BY the select list is also read against the named
			// sources first (hasAggregations), whose error wins: x.x.f is
			// ambiguous there, and names nothing through the unnamed operator,
			// whose lookup reads a path beginning with the column's bare name
			// from the column.
			`SELECT MAX(x.x.f) FROM x`,
			`SELECT MAX(x.x.f) FROM x GROUP BY x.id`,
			// A HAVING subquery's outer query is that unnamed operator. The
			// target resolves the subquery before it refuses a HAVING EXISTS.
			`SELECT f FROM x GROUP BY f HAVING EXISTS (SELECT 1 FROM y WHERE y.id = x.f)`,
			// An outer join replaces its sources, and every source before it,
			// with one unnamed operator for every later clause; its own ON sees
			// them named unless two or more sources precede it (collapsed).
			`SELECT x.f FROM x LEFT JOIN h ON x.id = h.id`,
			`SELECT x.id FROM x LEFT JOIN h ON x.id = h.id WHERE x.f = 5`,
			`SELECT w.id FROM w LEFT JOIN h ON w.id = h.id WHERE w.w.f = 1`,
			`SELECT w.id FROM w LEFT JOIN h ON w.w.id = h.id ORDER BY w.id`,
			`SELECT x.id FROM x LEFT JOIN h ON x.f = h.id`,
			`SELECT w.w.id FROM w LEFT JOIN h ON w.id = h.id ORDER BY w.id`,
			`SELECT x.f FROM x JOIN h ON x.id = h.id`,
			`SELECT x.x.f FROM x LEFT JOIN h ON x.id = h.id`,
			`SELECT x.x.g FROM x LEFT JOIN h ON x.id = h.id`,
			`SELECT h.f FROM y LEFT JOIN h ON y.id = h.id`,
			`SELECT y.h.f FROM y LEFT JOIN h ON y.id = h.id`,
			`SELECT x.id FROM x LEFT JOIN h ON x.id = h.id WHERE EXISTS (SELECT 1 FROM y WHERE y.id = x.f)`,
			`SELECT x.id FROM x WHERE EXISTS (SELECT 1 FROM y WHERE y.id = x.f)`,
			`SELECT x.id FROM x JOIN y ON x.id = y.id LEFT JOIN h ON x.f = h.id`,
			`SELECT x.id FROM x LEFT JOIN y ON x.id = y.id LEFT JOIN h ON x.f = h.id`,
			`SELECT x.id FROM x LEFT JOIN y ON x.id = y.id JOIN h ON x.f = h.f`,
			`SELECT h.id FROM x LEFT JOIN y ON x.id = y.id JOIN h ON h.f = x.id + 6`,
			// A CTE or derived body names its tables by the template's name,
			// as any block does.
			`WITH c AS (SELECT id FROM {T}.w) SELECT id FROM c`,
			`WITH c AS (SELECT id FROM S.w) SELECT id FROM c ORDER BY id`,
			`SELECT d.id FROM (SELECT id FROM S.w) AS d ORDER BY d.id`,
			// A UNION ALL defines no row order (the target's unordered union
			// answered [[2] [1]] for one run of a two-row union and [[1] [2]] for
			// others), so each union's branches return the same rows.
			`SELECT id FROM {T}.w WHERE id = 1 UNION ALL SELECT id FROM {T}.h`,
			`SELECT f FROM {T}.w WHERE id = 2 UNION ALL SELECT f FROM {T}.w WHERE id = 2`,
			`SELECT id FROM S.w UNION ALL SELECT id FROM {T}.h`,
			`WITH c AS (SELECT id, f FROM {T}.w) SELECT c.f FROM c JOIN h ON c.f = h.f`,
			`WITH c AS (SELECT id, f FROM {T}.w) SELECT COUNT(*) FROM c`,
			`SELECT MAX(d.f) FROM (SELECT f FROM {T}.w) AS d`,
			`WITH c AS (WITH c2 AS (SELECT id FROM {T}.w) SELECT id FROM c2) SELECT id FROM c`,
			// A lateral unnest's alias names its operator: the doubled reading
			// of a scalar element (x.x.x), and of a record element's member
			// (item.item.b), which the nested path through the whole element
			// (named item.item.b, not item.b) makes ambiguous. A FROM path takes
			// the doubled reading too (w.w.arr).
			// Through an unnamed operator Java stops at an attribute's direct
			// match (ss.ss, the struct column), and the whole element of an
			// unnest is named by its bare alias.
			`SELECT ss.ss FROM ss LEFT JOIN h ON ss.id = h.id`,
			`SELECT x.x FROM x LEFT JOIN h ON x.id = h.id`,
			`SELECT x.x.g FROM x`,
			`SELECT COUNT(item.item) FROM ra, ra.items AS item GROUP BY ra.id`,
			`SELECT COUNT(item) FROM ra, ra.items AS item GROUP BY ra.id`,
			`SELECT item.item FROM ra LEFT JOIN h ON ra.id = h.id, ra.items AS item`,
			// A HAVING the walker refuses on its own terms.
			`SELECT f FROM x GROUP BY f HAVING f = 'a'`,
			`SELECT f FROM x GROUP BY f HAVING nosuch = 1`,
			`SELECT x FROM w, w.w.arr AS x`,
			`SELECT x FROM t2, t2.n.arr AS x`,
			// A FROM item's path is read with the column lookup: through a table
			// named by its qualified identifier, and through a prior source's
			// struct column by its bare name; a table named `{T}.w` is not `w`.
			`SELECT x FROM {T}.w, {T}.w.arr AS x`,
			`SELECT x FROM t2, n.arr AS x`,
			`SELECT x FROM {T}.w, w.arr AS x`,
			// A qualified star rebuilds the query through the text builder,
			// which must classify the FROM item as the visitor did.
			`SELECT t2.* FROM t2, n.arr AS x`,
			`SELECT w.* FROM {T}.w AS w, w.arr AS x`,
			// An AT is refused only where the target's generateAccess refuses it:
			// a CTE, table or prior FROM source (42809); an unqualified name that
			// is none of them is an unknown table (42F01); a correlated path takes
			// it when it is an array, at any level, and a non-array path is
			// refused with or without it (42F10); a block's first FROM item that
			// is no correlated path fails as it does without it (42703).
			`SELECT x, p FROM t2, n.arr AS x AT p`,
			`SELECT x, p FROM {T}.w, {T}.w.arr AS x AT p`,
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM h, w.arr AS v AT p WHERE p = 2)`,
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM w.arr AS v AT p WHERE p = 2)`,
			`SELECT x FROM w, w.f AS x AT p`,
			`SELECT x FROM w, w.f AS x`,
			`SELECT 1 FROM w, nosuch AT p`,
			`SELECT 1 FROM w, h AT p`,
			`SELECT 1 FROM w, w AT p`,
			`SELECT 1 FROM w AT p`,
			`SELECT v FROM w.arr AS v AT p`,
			`SELECT v FROM w.arr AS v`,
			// A block's first FROM item carrying AT, inside a subquery or a
			// derived body, is decided in generateAccess order: a CTE (a WITH
			// CTE or any enclosing block's operator, which findCteMaybe walks)
			// or a table is 42809, any other single name 42F01; only a dotted
			// path is correlated. A derived body may read a first-item unnest.
			`SELECT id FROM w WHERE EXISTS (SELECT p FROM arr AT p)`,
			`SELECT d.p FROM w, (SELECT p FROM arr AT p) AS d`,
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM h AT p)`,
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM {T}.h AT p)`,
			`WITH c AS (SELECT id FROM h) SELECT id FROM w WHERE EXISTS (SELECT 1 FROM c AT p)`,
			`WITH c AS (SELECT id FROM h) SELECT 1 FROM w, c AT p`,
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM h, w AT p)`,
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM w AT p)`,
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM nosuch AT p)`,
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM w.arr AS v, (SELECT h.id FROM h WHERE h.f = v) AS d)`,
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM w.arr AS v, (SELECT h.id FROM h WHERE h.f + 3 = v) AS d)`,
			`SELECT id FROM w WHERE EXISTS (SELECT v FROM w.arr AS v, h, w AS z WHERE v = h.f AND z.id = h.id)`,
			`SELECT id FROM t2 WHERE EXISTS (SELECT x FROM n.arr AS x)`,
			// The path is looked up over every level's operators as one list,
			// qualified readings first: a qualified reading at one level is not
			// ambiguous with a struct-relative one at another.
			`SELECT id FROM t2 WHERE EXISTS (SELECT 1 FROM w AS n, n.arr AS x WHERE x = 20)`,
			`SELECT id FROM w AS n WHERE EXISTS (SELECT 1 FROM t2, n.arr AS v WHERE v = 20)`,
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM h AS "w", w.arr AS v WHERE v = 20)`,
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM h AS "W", w.arr AS v WHERE v = 20)`,
			`SELECT item.item.b FROM ra, ra.items AS item`,
			`SELECT item.b FROM ra, ra.items AS item`,
			`SELECT x.x FROM w, w.arr AS x`,
			`SELECT x.x.x FROM w, w.arr AS x`,
			`SELECT item.item FROM ra, ra.items AS item`,
			`SELECT COUNT(item.b) FROM ra, ra.items AS item`,
			`SELECT COUNT(item.item.b) FROM ra, ra.items AS item`,
			// Both unaliased, one qualified, joined on a predicate that is not
			// symmetric, so each column is read from its own leg.
			`SELECT {T}.w.id, w.id FROM {T}.w, w WHERE {T}.w.id = w.id + 1 ORDER BY w.id`,
			// A table with a struct column, named by its qualified identifier:
			// scope_qualified_name_test.go's shapes.
			`SELECT {T}.y.id FROM {T}.y`,
			`SELECT {T}.y.h.f FROM {T}.y`,
			`SELECT y.{T}.y.id FROM {T}.y`,
			`SELECT id FROM {T}.y`,
			`SELECT h.f FROM {T}.y`,
			`SELECT y.id FROM {T}.y`,
			`SELECT {T}.id FROM {T}.y`,
			`SELECT y.{T}.y.h.f FROM {T}.y`,
			`SELECT y.* FROM {T}.y`,
			// Unqualified controls.
			`SELECT id FROM nosuch`,
			`SELECT nosuch.id FROM w`,
		}
		dml := []string{
			`INSERT INTO {T}.w VALUES (3, 3, [30])`,
			`INSERT INTO S.w VALUES (4, 4, [40])`,
			`INSERT INTO nosuch.w VALUES (5, 5, [50])`,
			`INSERT INTO a.b.w VALUES (6, 6, [60])`,
			`UPDATE {T}.w SET f = 30 WHERE id = 3`,
			`UPDATE {t}.w SET f = 31 WHERE id = 3`,
			`UPDATE S.w SET f = 7 WHERE id = 999`,
			`UPDATE {T}.w SET w.f = 32 WHERE id = 3`,
			`UPDATE {T}.w SET {T}.w.f = 33 WHERE id = 3`,
			`DELETE FROM {T}.w WHERE id = 999`,
			`DELETE FROM S.w WHERE id = 999`,
			`DELETE FROM nosuch.w WHERE id = 999`,
			`INSERT INTO w SELECT id + 100, f, arr FROM {T}.w WHERE id = 1`,
			`INSERT INTO w SELECT id + 200, f, arr FROM S.w WHERE id = 1`,
			`UPDATE {T}.w SET f = 34 WHERE w.id = 3`,
			`UPDATE {T}.w SET f = 35 WHERE {T}.w.id = 3`,
			`DELETE FROM {T}.w WHERE w.id = 999`,
			`DELETE FROM {T}.w WHERE {T}.w.id = 999`,
			`INSERT INTO nosuch VALUES (1)`,
			`DELETE FROM nosuch WHERE id = 1`,
			`UPDATE {T}.nosuch SET f = 1 WHERE id = 1`,
			`UPDATE {T}.w SET w.{T}.w.f = 36 WHERE id = 999`,
			`UPDATE w SET f = w.w.f WHERE id = 999`,
			`UPDATE w SET f = 1 WHERE w.w.f = 999`,
			`UPDATE x SET f = x.f WHERE id = 999`,
			`UPDATE x SET f = 1 WHERE x.f = 999`,
			// The doubled reading of a column replaces the path through it.
			`UPDATE ss SET ss.ss.ss = (9) WHERE id = 1`,
			`UPDATE {T}.w SET {T}.w.w.f = 37 WHERE id = 999`,
		}
		reads := []string{`SELECT id, f FROM w ORDER BY id`, `SELECT ss.ss.ss FROM ss`}

		javaT, goT := p+"_JT", p+"_GT"
		subst := func(stmt, tmpl string) string {
			return strings.NewReplacer("{T}", tmpl, "{t}", strings.ToLower(tmpl)).Replace(stmt)
		}
		javaDB := "/TEST/" + p + "_J"
		javaDDL := func(stmts ...string) {
			var out map[string]any
			Expect(java.InvokeAs(ctx, "wsjCatalogDdlJava", map[string]any{"clusterFile": clusterFile, "statements": stmts}, &out)).To(Succeed())
			for i, o := range out["outcomes"].([]any) {
				Expect(o).To(Equal("OK"), "%s", stmts[i])
			}
		}
		javaDDL("CREATE DATABASE "+javaDB, "CREATE SCHEMA TEMPLATE "+javaT+" "+body, "CREATE SCHEMA "+javaDB+"/S WITH TEMPLATE "+javaT)
		defer javaDDL("DROP DATABASE IF EXISTS "+javaDB, "DROP SCHEMA TEMPLATE IF EXISTS "+javaT)
		javaExec := func(stmt string) string {
			var out map[string]any
			Expect(java.InvokeAs(ctx, "wsjExecuteJava", map[string]any{
				"clusterFile": clusterFile, "dbPath": javaDB, "schemaName": "S", "sql": subst(stmt, javaT),
			}, &out)).To(Succeed())
			o := out["outcome"].(string)
			if f := strings.SplitN(o, " ", 4); f[0] == "ERROR" && len(f) == 4 {
				return "ERROR " + f[1] + " " + f[3]
			}
			return o
		}
		javaRead := func(q string) string {
			var out struct {
				Rows [][]any `json:"rows"`
			}
			err := java.InvokeAs(ctx, "wsjQueryJava", map[string]any{
				"clusterFile": clusterFile, "dbPath": javaDB, "schemaName": "S", "querySql": subst(q, javaT),
			}, &out)
			var je *JavaError
			if errors.As(err, &je) {
				return "ERROR " + je.SQLState + " " + je.Message
			}
			Expect(err).NotTo(HaveOccurred(), q)
			return fmt.Sprint(out.Rows)
		}

		goDBPath := "/TEST/" + p + "_G"
		sysDB, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", clusterFilePath))
		Expect(err).NotTo(HaveOccurred())
		defer sysDB.Close()
		for _, stmt := range []string{
			"CREATE DATABASE " + goDBPath, "CREATE SCHEMA TEMPLATE " + goT + " " + body,
			"CREATE SCHEMA " + goDBPath + "/S WITH TEMPLATE " + goT,
		} {
			_, err := sysDB.ExecContext(ctx, stmt)
			Expect(err).NotTo(HaveOccurred(), "%s", stmt)
		}
		defer func() {
			_, _ = sysDB.ExecContext(ctx, "DROP DATABASE IF EXISTS "+goDBPath)
			_, _ = sysDB.ExecContext(ctx, "DROP SCHEMA TEMPLATE IF EXISTS "+goT)
		}()
		gdb, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=S", goDBPath, clusterFilePath))
		Expect(err).NotTo(HaveOccurred())
		defer gdb.Close()
		goErr := func(err error) string {
			var ae *api.Error
			if errors.As(err, &ae) {
				return "ERROR " + string(ae.Code) + " " + ae.Message
			}
			return "ERROR ? " + err.Error()
		}
		goExec := func(stmt string) string {
			res, err := gdb.ExecContext(ctx, subst(stmt, goT))
			if err != nil {
				return goErr(err)
			}
			n, err := res.RowsAffected()
			Expect(err).NotTo(HaveOccurred())
			return fmt.Sprintf("OK %d", n)
		}
		goRead := func(q string) string {
			rs, err := gdb.QueryContext(ctx, subst(q, goT))
			if err != nil {
				return goErr(err)
			}
			defer rs.Close()
			cols, err := rs.Columns()
			Expect(err).NotTo(HaveOccurred())
			var rows [][]any
			for rs.Next() {
				cells := make([]any, len(cols))
				ptrs := make([]any, len(cols))
				for i := range cells {
					ptrs[i] = &cells[i]
				}
				Expect(rs.Scan(ptrs...)).To(Succeed())
				for i := range cells {
					cells[i] = wsjEnumJSONShape(cells[i])
				}
				rows = append(rows, cells)
			}
			if err := rs.Err(); err != nil {
				return goErr(err)
			}
			return fmt.Sprint(rows)
		}
		// A refusal agrees by its SQLSTATE; rows agree literally.
		state := func(o string) string {
			if f := strings.SplitN(o, " ", 3); f[0] == "ERROR" && len(f) >= 2 {
				return "ERROR " + f[1]
			}
			return o
		}

		for _, stmt := range setup {
			j, g := javaExec(stmt), goExec(stmt)
			Expect(j).To(Equal("OK 1"), stmt)
			Expect(g).To(Equal("OK 1"), stmt)
		}
		var diffs []string
		compare := func(stmt, j, g string) {
			GinkgoWriter.Printf("WSFQUAL %s\n  java=%s\n  go  =%s\n", stmt, j, g)
			if state(j) != state(g) {
				diffs = append(diffs, fmt.Sprintf("%s: java=%s go=%s", stmt, j, g))
			}
		}
		answers := map[string]string{}
		for _, q := range queries {
			g := goRead(q)
			answers[q] = g
			compare(q, javaRead(q), g)
		}
		// Pinned with both answers, against the rows the setup wrote (before any
		// DML below).
		//
		// A HAVING EXISTS: the target refuses it as a non-grouping expression
		// (42803), Go at planning (0AF00). TODO.md "HAVING-EXISTS error-surface
		// alignment".
		//
		// A GROUP BY over a derived table, qualified or not, or over a struct
		// column: the target cannot plan it (0AF00); Go answers, a read-side
		// reach the target lacks (DIVERGENCES.md, "A table's qualifier is its
		// schema template's name").
		//
		// (An explicit JOIN's right side spelled as a correlated array is
		// conformance/ws_f_join_unnest_conformance_test.go's.)
		declared := map[string][2]string{
			`SELECT f FROM x GROUP BY f HAVING EXISTS (SELECT 1 FROM y WHERE y.id = 1)`:  {"ERROR 42803", "ERROR 0AF00"},
			`SELECT d.f, COUNT(*) FROM (SELECT f FROM w WHERE id = 1) AS d GROUP BY d.f`: {"ERROR 0AF00", "[[1 1]]"},
			`SELECT ss.ss, COUNT(*) FROM ss GROUP BY ss.ss`:                              {"ERROR 0AF00", "[[map[SS:8] 1]]"},
		}
		for q, want := range declared {
			j, g := javaRead(q), goRead(q)
			GinkgoWriter.Printf("WSFQUAL %s (declared)\n  java=%s\n  go  =%s\n", q, j, g)
			if state(j) != want[0] || state(g) != want[1] {
				diffs = append(diffs, fmt.Sprintf("%s: java=%s go=%s, declared java=%s go=%s", q, j, g, want[0], want[1]))
			}
		}
		for _, stmt := range dml {
			g := goExec(stmt)
			answers[stmt] = g
			compare(stmt, javaExec(stmt), g)
		}
		for _, q := range reads {
			compare(q, javaRead(q), goRead(q))
		}

		// An index definition's query names its table by the template being
		// created, the catalog its DDL resolves against.
		for i, body := range []string{
			`create table w(id bigint, f bigint, primary key(id)) create index ix as select f from {T}.w order by f`,
			`create table w(id bigint, f bigint, primary key(id)) create index ix as select f from S.w order by f`,
			`create table w(id bigint, f bigint, primary key(id)) create index ix as select f from w order by f`,
			`create table w(id bigint, f bigint, primary key(id)) create index ix as select f from nosuch.w order by f`,
		} {
			jt, gt := fmt.Sprintf("%s_JI%d", p, i), fmt.Sprintf("%s_GI%d", p, i)
			var out map[string]any
			Expect(java.InvokeAs(ctx, "wsjCatalogDdlJava", map[string]any{"clusterFile": clusterFile, "statements": []string{
				"CREATE SCHEMA TEMPLATE " + jt + " " + subst(body, jt), "DROP SCHEMA TEMPLATE IF EXISTS " + jt,
			}}, &out)).To(Succeed())
			j := out["outcomes"].([]any)[0].(string)
			if f := strings.SplitN(j, " ", 4); f[0] == "ERROR" && len(f) == 4 {
				j = "ERROR " + f[1] + " " + f[3]
			}
			g := "OK"
			if _, err := sysDB.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+gt+" "+subst(body, gt)); err != nil {
				g = goErr(err)
			}
			_, _ = sysDB.ExecContext(ctx, "DROP SCHEMA TEMPLATE IF EXISTS "+gt)
			answers[body] = g
			compare(body, j, g)
		}
		Expect(diffs).To(BeEmpty(), strings.Join(diffs, "\n"))

		// Go's answers, asserted literally for the shapes this spec is about,
		// so both engines moving together cannot pass as agreement.
		for stmt, want := range map[string]string{
			`create table w(id bigint, f bigint, primary key(id)) create index ix as select f from {T}.w order by f`: "OK",
			`create table w(id bigint, f bigint, primary key(id)) create index ix as select f from S.w order by f`:   "ERROR 42703",
			`SELECT x.f, COUNT(*) FROM x GROUP BY x.f`:                                                               "ERROR 42702",
			`SELECT MAX(w.w.f) FROM w`:                                                                              "ERROR 42703",
			`SELECT COUNT(*) FROM x WHERE x.f = 5`:                                                                  "[[1]]",
			`SELECT MAX(x.x.f) FROM x`:                                                                              "ERROR 42702",
			`SELECT MAX(x.x.f) FROM x GROUP BY x.id`:                                                                "ERROR 42703",
			`SELECT f FROM x GROUP BY f HAVING EXISTS (SELECT 1 FROM y WHERE y.id = x.f)`:                           "ERROR 42702",
			`SELECT x.f FROM x LEFT JOIN h ON x.id = h.id`:                                                          "ERROR 42702",
			`SELECT x.id FROM x LEFT JOIN h ON x.f = h.id`:                                                          "[[1]]",
			`SELECT x.id FROM x JOIN y ON x.id = y.id LEFT JOIN h ON x.f = h.id`:                                    "ERROR 42702",
			`SELECT h.id FROM x LEFT JOIN y ON x.id = y.id JOIN h ON h.f = x.id + 6`:                                "ERROR 42702",
			`SELECT x.id FROM x LEFT JOIN h ON x.id = h.id WHERE EXISTS (SELECT 1 FROM y WHERE y.id = x.f)`:         "ERROR 42702",
			`SELECT y.h.f FROM y LEFT JOIN h ON y.id = h.id`:                                                        "[[60]]",
			`SELECT x.x.g FROM x LEFT JOIN h ON x.id = h.id`:                                                        "ERROR 42703",
			`SELECT item.item.b FROM ra, ra.items AS item`:                                                          "ERROR 42702",
			`SELECT x.x.x FROM w, w.arr AS x`:                                                                       "[[10] [11] [20]]",
			`SELECT x FROM t2, t2.n.arr AS x`:                                                                       "[[10] [20]]",
			`SELECT x, p FROM t2, n.arr AS x AT p`:                                                                  "[[10 1] [20 2]]",
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM h, w.arr AS v AT p WHERE p = 2)`:                          "[[1]]",
			`SELECT x FROM w, w.f AS x AT p`:                                                                        "ERROR 42F10",
			`SELECT 1 FROM w, nosuch AT p`:                                                                          "ERROR 42F01",
			`SELECT 1 FROM w AT p`:                                                                                  "ERROR 42809",
			`SELECT v FROM w.arr AS v AT p`:                                                                         "ERROR 42703",
			`SELECT id FROM t2 WHERE EXISTS (SELECT 1 FROM w AS n, n.arr AS x WHERE x = 20)`:                        "[[1]]",
			`SELECT id FROM w WHERE EXISTS (SELECT p FROM arr AT p)`:                                                "ERROR 42F01",
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM h AT p)`:                                                  "ERROR 42809",
			`WITH c AS (SELECT id FROM h) SELECT 1 FROM w, c AT p`:                                                  "ERROR 42809",
			`SELECT id FROM w WHERE EXISTS (SELECT 1 FROM w.arr AS v, (SELECT h.id FROM h WHERE h.f + 3 = v) AS d)`: "[[1]]",
			`SELECT id FROM w AS n WHERE EXISTS (SELECT 1 FROM t2, n.arr AS v WHERE v = 20)`:                        "[[2]]",
			`SELECT id FROM {T}.w ORDER BY id`:                                                                      "[[1] [2]]",
			`SELECT id FROM S.w ORDER BY id`:                                                                        "ERROR 42703",
			`SELECT id FROM a.b.w ORDER BY id`:                                                                      "ERROR 42703",
			`SELECT w.id FROM {T}.w ORDER BY id`:                                                                    "ERROR 42703",
			`SELECT {T}.w.id FROM {T}.w ORDER BY id`:                                                                "[[1] [2]]",
			`SELECT w.* FROM {T}.w ORDER BY id`:                                                                     "ERROR 42703",
			`SELECT w.w.id FROM w ORDER BY id`:                                                                      "[[1] [2]]",
			`SELECT x.f FROM x`:                                                                                     "[[5]]",
			`SELECT x.x.f FROM x`:                                                                                   "ERROR 42702",
			`SELECT h.f FROM y, h`:                                                                                  "[[7]]",
			`SELECT n.sk FROM nt AS n ORDER BY n.id`:                                                                "[[1]]",
			`SELECT n.sk FROM nt AS a, nt AS b`:                                                                     "ERROR 42702",
			`INSERT INTO {T}.w VALUES (3, 3, [30])`:                                                                 "OK 1",
			`INSERT INTO S.w VALUES (4, 4, [40])`:                                                                   "ERROR 42F00",
			`INSERT INTO a.b.w VALUES (6, 6, [60])`:                                                                 "ERROR XX000",
			`UPDATE {T}.w SET {T}.w.f = 33 WHERE id = 3`:                                                            "OK 1",
			`UPDATE {T}.w SET w.f = 32 WHERE id = 3`:                                                                "ERROR 42703",
			`UPDATE {T}.w SET w.{T}.w.f = 36 WHERE id = 999`:                                                        "OK 0",
			`UPDATE {T}.nosuch SET f = 1 WHERE id = 1`:                                                              "ERROR 42F01",
			`UPDATE ss SET ss.ss.ss = (9) WHERE id = 1`:                                                             "OK 1",
		} {
			got, ok := answers[stmt]
			Expect(ok).To(BeTrue(), "%s is not a statement of this spec", stmt)
			Expect(state(got)).To(Equal(want), stmt)
		}
		// And the messages Go shares with Java's wording.
		for stmt, holds := range map[string]string{
			`SELECT id FROM S.w ORDER BY id`:        "Unknown reference S.W",
			`INSERT INTO S.w VALUES (4, 4, [40])`:   "Unknown schema template S",
			`INSERT INTO a.b.w VALUES (6, 6, [60])`: "Unknown table A.B.W",
		} {
			Expect(answers[stmt]).To(ContainSubstring(holds), stmt)
		}
	})
})
