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

// RFC-257 WS-J: an UPDATE's SET column is resolved ONCE, against the target's
// columns as the user named them. Java's SemanticAnalyzer resolves the
// identifier against the target Type.Record, whose field names are decoded
// once from the descriptor's storage names, and the FieldPath it builds
// carries the resolved ordinal that RecordQueryUpdatePlan keys its
// transformation trie by (MessageHelpers.transformMessage indexes the field by
// it), so nothing looks the name up again.
//
// The storage escaping is not injective under a second decode: `a$b` is stored
// as a__1b and `a__1b` as a__01b, so decoding the user identifier `a__1b` a
// second time gives `a$b`. The columns here carry escape tokens in their user
// identifiers, in both declaration orders, and one comes from Java's own
// corpus (valid-identifiers.yamsql's `enum_type.enum__1`). Each statement is
// run on both engines against the same DDL and must answer the same, then the
// rows must read the same.
var _ = Describe("RFC-257 WS-J: UPDATE resolves its SET column once", func() {
	It("assigns escape-token columns as the target does, in either declaration order", func() {
		ctx := context.Background()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		clusterFilePath := writeClusterFileToTemp(clusterFile)
		defer os.Remove(clusterFilePath)
		p := "WSJUPDCOL" + strings.ToUpper(strings.ReplaceAll(uuid.New().String()[:8], "-", ""))
		const body = `create table t(id bigint, "a$b" bigint, "a__1b" string, "enum_type.enum__1" bigint, primary key(id)) ` +
			`create table u(id bigint, "a__1b" string, "a$b" bigint, primary key(id)) ` +
			`create table n(id bigint, "a$b" bigint array not null, "a__1b" bigint array, primary key(id)) ` +
			`create type as struct s(f bigint, g string) ` +
			`create table w(id bigint, f bigint, s s, primary key(id)) ` +
			`create type as struct "foo.struct"(S1 bigint, S2 bigint) ` +
			`create table "foo.tableB"("foo.tableB.B1" bigint, "foo.tableB.B2" bigint, "foo.tableB.B3" "foo.struct", primary key("foo.tableB.B1")) ` +
			`create table v(id bigint, f bigint, "v" s, primary key(id)) ` +
			`create table "q"(id bigint, f bigint, Q s, primary key(id)) ` +
			`create type as struct p1(x bigint) create type as struct p2(y bigint, z bigint) ` +
			`create table sp(id bigint, "a$b" p1, "a__1b" p2, primary key(id)) ` +
			`create table x(id bigint, f bigint, X s, primary key(id)) ` +
			`create table "Wq"(id bigint, f bigint, primary key(id))`
		setup := []string{
			`INSERT INTO t VALUES (1, 1, 'x', 1)`,
			`INSERT INTO u VALUES (1, 'x', 1)`,
			`INSERT INTO n VALUES (1, [1], [2])`,
			`INSERT INTO w VALUES (1, 1, (10, 'x'))`,
			`INSERT INTO w VALUES (2, 2, NULL)`,
			`INSERT INTO "foo.tableB" VALUES (1, 20, (4, 40))`,
			`INSERT INTO v VALUES (1, 1, (10, 'x'))`,
			`INSERT INTO "q" VALUES (1, 1, (10, 'x'))`,
			`INSERT INTO sp VALUES (1, (1), (2, 3))`,
			`INSERT INTO x VALUES (1, 1, (10, 'x'))`,
			`INSERT INTO "Wq" VALUES (1, 1)`,
		}
		updates := []string{
			// The column whose user identifier holds an escape token.
			`UPDATE t SET "a__1b" = 'y' WHERE id = 1`,
			`UPDATE u SET "a__1b" = 'y' WHERE id = 1`,
			// The column a second decode would alias onto it, typed as its
			// own BIGINT, whichever is declared first.
			`UPDATE t SET "a$b" = 5 WHERE id = 1`,
			`UPDATE u SET "a$b" = 6 WHERE id = 1`,
			// And refused by its own type while planning, a row matching or not.
			`UPDATE t SET "a$b" = 'z' WHERE id = 999`,
			`UPDATE u SET "a$b" = 'z' WHERE id = 999`,
			`UPDATE u SET "a__1b" = 7 WHERE id = 999`,
			// Java's corpus column: a dot and an escape token.
			`UPDATE t SET "enum_type.enum__1" = 8 WHERE id = 1`,
			// A column neither table has.
			`UPDATE t SET "a__01b" = 'w' WHERE id = 1`,
			// The NOT NULL check and the struct type push-down address the
			// column the statement names: the nullable one of an escape pair,
			// the NOT NULL one, and Java's corpus struct column.
			`UPDATE n SET "a__1b" = NULL WHERE id = 1`,
			`UPDATE n SET "a$b" = NULL WHERE id = 1`,
			// NULL into the NOT NULL array is refused as a row is transformed
			// (Java's coerceObject, NULL_ASSIGNMENT), not while planning.
			`UPDATE n SET "a$b" = NULL WHERE id = 999`,
			`UPDATE "foo.tableB" SET "foo.tableB.B3" = (100, 100) WHERE "foo.tableB.B1" = 1`,
			// A qualified or nested SET column.
			`UPDATE w SET w.f = 6 WHERE id = 1`,
			`UPDATE w SET s.f = 5 WHERE id = 1`,
			`UPDATE w SET w.s.f = 11 WHERE id = 1`,
			`UPDATE w SET s.f = 9 WHERE id = 2`,
			`UPDATE w SET s.f = 'x' WHERE id = 999`,
			`UPDATE w SET nosuch.f = 7 WHERE id = 1`,
			`UPDATE w SET s.nosuch = 5 WHERE id = 1`,
			`UPDATE w SET s = (1, 'a'), s.f = 5 WHERE id = 1`,
			// One column assigned twice.
			`UPDATE w SET f = 1, f = 2 WHERE id = 1`,
			// A column spelled like its table up to case: the exact reading,
			// qualified or not, before any case fold.
			`UPDATE v SET "v".f = 3 WHERE id = 1`,
			`UPDATE "q" SET Q.f = 4 WHERE id = 1`,
			// The struct push-down onto the escape pair's struct columns.
			`UPDATE sp SET "a__1b" = (5, 6) WHERE id = 1`,
			// The WHERE is resolved before the SET list.
			`UPDATE w SET f = 1, f = 2 WHERE nosuch = 1`,
			`UPDATE w SET nosuch = 1 WHERE nosuchb = 1`,
			// Each SET element resolves its column, then its value, in order.
			`UPDATE w SET f = nosuchcol WHERE id = 1`,
			`UPDATE w SET f = nosuchcol, nosuch = 1 WHERE id = 1`,
			// A struct column spelled exactly like its table: both exact readings
			// of `x.f` name a field, and the qualified one is taken; `x.x.f` is
			// the struct's f AND, the table named twice before a top-level
			// column (Java's qualified lookup prepends the operator's name to
			// the attribute's table-qualified name), x's f: ambiguous.
			`UPDATE x SET x.f = 7 WHERE id = 1`,
			`UPDATE x SET x.x.f = 8 WHERE id = 1`,
			`UPDATE x SET x.x.id = 14 WHERE id = 999`,
			`UPDATE w SET w.w.f = 12 WHERE id = 1`,
			`UPDATE w SET w.w.s.f = 13 WHERE id = 999`,
			// The qualifier names the table exactly, as the SELECT scope
			// compares it; it is never folded.
			`UPDATE "Wq" SET WQ.f = 5 WHERE id = 999`,
			`UPDATE "Wq" SET "Wq".f = 5 WHERE id = 1`,
			// A table the statement cannot name is the answer, before a SET
			// value naming no column.
			`UPDATE wq SET f = nosuchcol WHERE id = 1`,
		}
		// Pinned with both answers, so the spec reddens when either engine
		// moves:
		//  - The table itself as the SET column: the target resolves `w` to the
		//    whole row and then casts it to a field (QueryVisitor's
		//    castUnchecked), an internal error, XX000 "expected FieldValue but
		//    got QuantifiedObjectValue". Go answers as for any name that is not
		//    a column of the target: 42703 (DIVERGENCES.md, "where the target
		//    crashes").
		pinned := map[string][2]string{
			`UPDATE w SET w = 5 WHERE id = 999`: {"ERROR XX000", "ERROR 42703"},
		}
		// Go's declared case over-resolution (DIVERGENCES.md "Identifier
		// resolution: Go over-resolves case"): a folded spelling reaches the
		// one column it folds to, where the target, comparing exactly, has no
		// such column. Pinned with both answers; no row matches, so the rows
		// both engines hold stay comparable.
		goOnly := map[string][2]string{
			`UPDATE t SET "A$B" = 9 WHERE id = 999`: {"ERROR 42703", "OK 0"},
			// Both readings fold to a field (V as the table, then f as F; and
			// V as the struct column "v", then f as its F): two candidates,
			// 42702, where the target, comparing exactly, finds neither.
			`UPDATE v SET V."f" = 9 WHERE id = 999`: {"ERROR 42703", "ERROR 42702"},
		}
		reads := []string{
			`SELECT * FROM t ORDER BY id`,
			`SELECT * FROM u ORDER BY id`,
			`SELECT * FROM n ORDER BY id`,
			`SELECT * FROM w ORDER BY id`,
			`SELECT * FROM "foo.tableB"`,
			`SELECT * FROM v ORDER BY id`,
			`SELECT * FROM "q" ORDER BY id`,
			`SELECT * FROM sp ORDER BY id`,
			`SELECT * FROM x ORDER BY id`,
			`SELECT * FROM "Wq" ORDER BY id`,
		}

		javaDB := "/TEST/" + p + "_J"
		javaDDL := func(stmts ...string) {
			var out map[string]any
			Expect(java.InvokeAs(ctx, "wsjCatalogDdlJava", map[string]any{"clusterFile": clusterFile, "statements": stmts}, &out)).To(Succeed())
			for i, o := range out["outcomes"].([]any) {
				Expect(o).To(Equal("OK"), "%s", stmts[i])
			}
		}
		javaDDL("CREATE DATABASE "+javaDB, "CREATE SCHEMA TEMPLATE "+p+"_JT "+body, "CREATE SCHEMA "+javaDB+"/S WITH TEMPLATE "+p+"_JT")
		defer javaDDL("DROP DATABASE IF EXISTS "+javaDB, "DROP SCHEMA TEMPLATE IF EXISTS "+p+"_JT")
		javaExec := func(stmt string) string {
			var out map[string]any
			Expect(java.InvokeAs(ctx, "wsjExecuteJava", map[string]any{
				"clusterFile": clusterFile, "dbPath": javaDB, "schemaName": "S", "sql": stmt,
			}, &out)).To(Succeed())
			// Java answers "ERROR <state> <class> <message>": the state and the
			// message are what both engines carry.
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
			Expect(java.InvokeAs(ctx, "wsjQueryJava", map[string]any{
				"clusterFile": clusterFile, "dbPath": javaDB, "schemaName": "S", "querySql": q,
			}, &out)).To(Succeed())
			return fmt.Sprint(out.Rows)
		}

		goDBPath := "/TEST/" + p + "_G"
		sysDB, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", clusterFilePath))
		Expect(err).NotTo(HaveOccurred())
		defer sysDB.Close()
		for _, stmt := range []string{
			"CREATE DATABASE " + goDBPath, "CREATE SCHEMA TEMPLATE " + p + "_GT " + body,
			"CREATE SCHEMA " + goDBPath + "/S WITH TEMPLATE " + p + "_GT",
		} {
			_, err := sysDB.ExecContext(ctx, stmt)
			Expect(err).NotTo(HaveOccurred(), "%s", stmt)
		}
		defer func() {
			_, _ = sysDB.ExecContext(ctx, "DROP DATABASE IF EXISTS "+goDBPath)
			_, _ = sysDB.ExecContext(ctx, "DROP SCHEMA TEMPLATE IF EXISTS "+p+"_GT")
		}()
		gdb, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=S", goDBPath, clusterFilePath))
		Expect(err).NotTo(HaveOccurred())
		defer gdb.Close()
		goExec := func(stmt string) string {
			res, err := gdb.ExecContext(ctx, stmt)
			if err != nil {
				var ae *api.Error
				if errors.As(err, &ae) {
					return "ERROR " + string(ae.Code) + " " + ae.Message
				}
				return "ERROR ? " + err.Error()
			}
			n, err := res.RowsAffected()
			Expect(err).NotTo(HaveOccurred())
			return fmt.Sprintf("OK %d", n)
		}
		goRead := func(q string) string {
			rs, err := gdb.QueryContext(ctx, q)
			if err != nil {
				return "ERROR " + err.Error()
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
			Expect(rs.Err()).NotTo(HaveOccurred())
			return fmt.Sprint(rows)
		}
		// The SQLSTATE alone decides a refusal's agreement: the planning-time
		// type refusal and the missing column carry each engine's own wording.
		state := func(o string) string {
			if f := strings.SplitN(o, " ", 3); f[0] == "ERROR" && len(f) >= 2 {
				return "ERROR " + f[1]
			}
			return o
		}

		var diffs []string
		for _, stmt := range setup {
			j, g := javaExec(stmt), goExec(stmt)
			GinkgoWriter.Printf("WSJUPDCOL %s\n  java=%s\n  go  =%s\n", stmt, j, g)
			Expect(j).To(Equal("OK 1"), stmt)
			Expect(g).To(Equal("OK 1"), stmt)
		}
		answers := map[string][2]string{}
		for _, stmt := range updates {
			j, g := javaExec(stmt), goExec(stmt)
			answers[stmt] = [2]string{j, g}
			GinkgoWriter.Printf("WSJUPDCOL %s\n  java=%s\n  go  =%s\n", stmt, j, g)
			if state(j) != state(g) {
				diffs = append(diffs, fmt.Sprintf("%s: java=%s go=%s", stmt, j, g))
			}
		}
		for stmt, want := range pinned {
			j, g := javaExec(stmt), goExec(stmt)
			GinkgoWriter.Printf("WSJUPDCOL %s (pinned)\n  java=%s\n  go  =%s\n", stmt, j, g)
			if state(j) != want[0] || state(g) != want[1] {
				diffs = append(diffs, fmt.Sprintf("%s: java=%s go=%s, pinned java=%s go=%s", stmt, j, g, want[0], want[1]))
			}
		}
		for stmt, want := range goOnly {
			j, g := javaExec(stmt), goExec(stmt)
			GinkgoWriter.Printf("WSJUPDCOL %s (declared)\n  java=%s\n  go  =%s\n", stmt, j, g)
			if state(j) != want[0] || state(g) != want[1] {
				diffs = append(diffs, fmt.Sprintf("%s: java=%s go=%s, pinned java=%s go=%s", stmt, j, g, want[0], want[1]))
			}
		}
		for _, q := range reads {
			j, g := javaRead(q), goRead(q)
			GinkgoWriter.Printf("WSJUPDCOL %s\n  java=%s\n  go  =%s\n", q, j, g)
			if j != g {
				diffs = append(diffs, fmt.Sprintf("%s: java=%s go=%s", q, j, g))
			}
		}
		Expect(diffs).To(BeEmpty(), strings.Join(diffs, "\n"))

		// Go's answers, asserted literally, so both engines moving together
		// cannot pass as agreement.
		wantGo := []string{
			"OK 1", "OK 1", "OK 1", "OK 1", // the escape-token columns
			"ERROR 22000", "ERROR 22000", "ERROR 22000", // refused by their own types
			"OK 1", "ERROR 42703", // the corpus column; no such column
			"OK 1", "ERROR XX000", "OK 0", "OK 1", // NULL and the struct push-down
			"OK 1", "OK 1", "OK 1", "OK 1", // qualified and nested
			"ERROR 22000", "ERROR 42703", "ERROR 42703", // a nested type; no such qualifier or field
			"ERROR XX000", "ERROR XXXXX", // a prefix; one field twice
			"OK 1", "OK 1", // a column spelled like its table
			"OK 1",                       // the push-down onto the escape pair
			"ERROR 42703", "ERROR 42703", // the WHERE first
			"ERROR 42703", "ERROR 42703", // an element's value before the next column
			"OK 1", "ERROR 42702", // a struct column spelled exactly like its table
			"OK 0", "OK 1", "ERROR 42703", // the table twice: a top-level column only
			"ERROR 42703", "OK 1", // the qualifier is exact
			"ERROR 42F01", // the table before the value
		}
		Expect(updates).To(HaveLen(len(wantGo)))
		for i, stmt := range updates {
			Expect(state(answers[stmt][1])).To(Equal(wantGo[i]), stmt)
		}
		// The fault Java names, so a change to Java's order reddens even where
		// both engines keep the same SQLSTATE.
		for stmt, column := range map[string]string{
			`UPDATE w SET f = 1, f = 2 WHERE nosuch = 1`:          "NOSUCH",
			`UPDATE w SET nosuch = 1 WHERE nosuchb = 1`:           "NOSUCHB",
			`UPDATE w SET f = nosuchcol, nosuch = 1 WHERE id = 1`: "NOSUCHCOL",
		} {
			Expect(answers[stmt][0]).To(HaveSuffix(" column "+column), stmt)
		}
		// The fault Go names, and the messages Go shares with Java.
		for stmt, holds := range map[string]string{
			`UPDATE w SET f = 1, f = 2 WHERE nosuch = 1`:          `"NOSUCH"`,
			`UPDATE w SET nosuch = 1 WHERE nosuchb = 1`:           `"NOSUCHB"`,
			`UPDATE w SET f = nosuchcol, nosuch = 1 WHERE id = 1`: `"NOSUCHCOL"`,
			`UPDATE n SET "a$b" = NULL WHERE id = 1`:              "A null value cannot be assigned to a variable that is of a non-nullable type.",
			`UPDATE w SET s = (1, 'a'), s.f = 5 WHERE id = 1`:     "The transformations used in an UPDATE statement are ambiguous.",
		} {
			Expect(answers[stmt][1]).To(ContainSubstring(holds), stmt)
		}
		for q, want := range map[string]string{
			reads[0]: "[[1 5 y 8]]",
			reads[1]: "[[1 y 6]]",
			reads[2]: "[[1 [1] <nil>]]",
			reads[3]: "[[1 12 map[F:11 G:x]] [2 2 map[F:9]]]",
			reads[4]: "[[1 20 map[S1:100 S2:100]]]",
			reads[5]: "[[1 1 map[F:3 G:x]]]",
			reads[6]: "[[1 1 map[F:4 G:x]]]",
			reads[7]: "[[1 map[X:1] map[Y:5 Z:6]]]",
			reads[8]: "[[1 7 map[F:10 G:x]]]",
			reads[9]: "[[1 5]]",
		} {
			Expect(goRead(q)).To(Equal(want), q)
		}
	})
})
