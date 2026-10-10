//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/keyspace"
)

// Separate schemas let both drivers' enum records and index entries be compared
// byte-for-byte without either writer masking the other's representation.
var _ = Describe("RFC-257 WS-J enum columns written and read by both engines", func() {
	It("stores, indexes, reads and refuses as the target", func() {
		ctx := context.Background()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		clusterFilePath := writeClusterFileToTemp(clusterFile)
		defer os.Remove(clusterFilePath)
		p := "WSJENUM" + strings.ToUpper(strings.ReplaceAll(uuid.New().String()[:8], "-", ""))
		const body = "create type as enum mood('JOYFUL', 'HAPPY', 'SAD') " +
			"create table t(id bigint, m mood, primary key(id)) create index t_m as select m from t order by m"
		rows := []string{"(1, 'JOYFUL')", "(2, NULL)", "(3, 'SAD')", "(4, 'HAPPY')"}
		refused := []string{"(5, 'ANGRY')", "(6, 0)"}
		const query = "SELECT id, m FROM t ORDER BY id"

		// The target.
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
			return out["outcome"].(string)
		}
		for _, r := range rows {
			Expect(javaExec("INSERT INTO t VALUES "+r)).To(Equal("OK 1"), "Java: %s", r)
		}
		var javaRecords struct {
			Entries []string `json:"entries"`
		}
		Expect(java.InvokeAs(ctx, "wsjRecordEntriesJava", map[string]any{
			"clusterFile": clusterFile, "dbPath": javaDB, "schemaName": "S",
		}, &javaRecords)).To(Succeed())
		var javaIndex struct {
			Entries []struct {
				Hex string `json:"hex"`
			} `json:"entries"`
		}
		Expect(java.InvokeAs(ctx, "wsjIndexEntriesJava", map[string]any{
			"clusterFile": clusterFile, "dbPath": javaDB, "schemaName": "S", "indexName": "T_M",
		}, &javaIndex)).To(Succeed())
		var javaRows struct {
			Rows [][]any `json:"rows"`
		}
		Expect(java.InvokeAs(ctx, "wsjQueryJava", map[string]any{
			"clusterFile": clusterFile, "dbPath": javaDB, "schemaName": "S", "querySql": query,
		}, &javaRows)).To(Succeed())

		// Go.
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
		for _, r := range rows {
			_, err := gdb.ExecContext(ctx, "INSERT INTO t VALUES "+r)
			Expect(err).NotTo(HaveOccurred(), "Go: %s", r)
		}
		storeKVs := func(prefix tuple.Tuple, withValue bool) []string {
			ss, err := keyspace.New(subspace.Sub()).LookupSchemaSubspace(ctx, recordlayer.NewFDBDatabase(sharedDB), goDBPath, "S")
			Expect(err).NotTo(HaveOccurred())
			rng := ss.Sub(prefix...)
			var out []string
			_, err = sharedDB.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
				kvs, err := tr.GetRange(rng, fdb.RangeOptions{}).GetSliceWithError()
				if err != nil {
					return nil, err
				}
				for _, kv := range kvs {
					e := hex.EncodeToString(kv.Key[len(rng.Bytes()):])
					if withValue {
						e += "=" + hex.EncodeToString(kv.Value)
					}
					out = append(out, e)
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			return out
		}
		goRecords := storeKVs(tuple.Tuple{int64(1)}, true)
		goIndex := storeKVs(tuple.Tuple{int64(2), "T_M"}, false)
		rs, err := gdb.QueryContext(ctx, query)
		Expect(err).NotTo(HaveOccurred())
		var goRows [][]any
		for rs.Next() {
			var id int64
			var m sql.NullString
			Expect(rs.Scan(&id, &m)).To(Succeed())
			var mv any
			if m.Valid {
				mv = m.String
			}
			goRows = append(goRows, []any{float64(id), mv})
		}
		Expect(rs.Err()).NotTo(HaveOccurred())
		_ = rs.Close()

		// Both relational stores write through StoreConfig.DEFAULT_RELATIONAL_SERIALIZER
		// (Go: embedded.defaultRelationalSerializer): a record too small to
		// compress is stored behind the one-byte prefix PREFIX_CLEAR, 02, then
		// the union message. The prefix is asserted on each side, and the
		// records, prefix included, are compared whole (RFC-257
		// serializer-design.md).
		GinkgoWriter.Printf("WSJENUM records java=%v go=%v\n", javaRecords.Entries, goRecords)
		Expect(goRecords).To(HaveLen(len(rows)))
		Expect(javaRecords.Entries).To(HaveLen(len(rows)))
		for _, e := range javaRecords.Entries {
			key, value, ok := strings.Cut(e, "=")
			Expect(ok).To(BeTrue())
			Expect(value).To(HavePrefix("02"), "Java's record %s is not behind PREFIX_CLEAR", key)
		}
		for _, e := range goRecords {
			key, value, _ := strings.Cut(e, "=")
			Expect(value).To(HavePrefix("02"), "Go's record %s is not behind PREFIX_CLEAR", key)
		}
		Expect(goRecords).To(Equal(javaRecords.Entries), "the records Go wrote differ from Java's")
		var javaIndexHex []string
		for _, e := range javaIndex.Entries {
			javaIndexHex = append(javaIndexHex, e.Hex)
		}
		GinkgoWriter.Printf("WSJENUM index java=%v go=%v\n", javaIndexHex, goIndex)
		Expect(goIndex).To(HaveLen(len(rows)))
		Expect(goIndex).To(Equal(javaIndexHex), "the index entries Go wrote differ from Java's")
		const want = "[[1 JOYFUL] [2 <nil>] [3 SAD] [4 HAPPY]]"
		GinkgoWriter.Printf("WSJENUM read java=%v go=%v\n", javaRows.Rows, goRows)
		Expect(fmt.Sprint(javaRows.Rows)).To(Equal(want), "Java's read")
		Expect(fmt.Sprint(goRows)).To(Equal(want), "Go's read")

		goOutcome := func(err error) string {
			if err == nil {
				return "OK"
			}
			var ae *api.Error
			Expect(errors.As(err, &ae)).To(BeTrue(), "not an api.Error: %v", err)
			return "ERROR " + string(ae.Code) + " " + ae.Message
		}
		for _, r := range refused {
			javaOut := javaExec("INSERT INTO t VALUES " + r)
			_, goErr := gdb.ExecContext(ctx, "INSERT INTO t VALUES "+r)
			GinkgoWriter.Printf("WSJENUM refuse %s java=%s go=%s\n", r, javaOut, goOutcome(goErr))
			f := strings.SplitN(javaOut, " ", 4) // ERROR <state> <class> <message>
			Expect(f).To(HaveLen(4), "Java answered %q", javaOut)
			Expect(goOutcome(goErr)).To(Equal("ERROR "+f[1]+" "+f[3]), "the refusal of %s", r)
		}
	})

	// An enum is read by its name wherever it sits: MessageTuple.sanitizeField
	// names an EnumValueDescriptor for a column, for each element of an array
	// (MessageTuple.java:71), and for a struct's attribute (RowStruct over the
	// nested message). And an UPDATE assigns an enum column as the target's
	// PromoteValue.computePromotionsTrie admits (RecordQueryUpdatePlan.updatePlan):
	// a string through STRING_TO_ENUM, an enum only of an equal type (Type.Enum
	// compares values, not names), anything else INCOMPATIBLE_TYPE, refused while
	// planning, so whether a row matches does not matter.
	It("reads a nested enum by name and coerces UPDATE SET as the target", func() {
		ctx := context.Background()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		clusterFilePath := writeClusterFileToTemp(clusterFile)
		defer os.Remove(clusterFilePath)
		p := "WSJENUMN" + strings.ToUpper(strings.ReplaceAll(uuid.New().String()[:8], "-", ""))
		const body = "create type as enum mood('JOYFUL', 'HAPPY', 'SAD') create type as enum other('HAPPY', 'SAD') " +
			"create type as enum same('JOYFUL', 'HAPPY', 'SAD') create type as struct s(k mood, n bigint) " +
			"create table t(id bigint, i integer, b bigint, str string, m mood, o other, sm same, st s, " +
			"ms mood array, sts s array, u uuid, f float, d double, primary key(id))"
		inserts := []string{
			"INSERT INTO t VALUES (1, 1, 2, 'SAD', 'JOYFUL', 'SAD', 'HAPPY', ('SAD', 7), ['HAPPY', 'SAD'], [('JOYFUL', 1), ('SAD', 2)], NULL, CAST(1.5 AS FLOAT), 2.5)",
			"INSERT INTO t VALUES (2, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL, NULL)",
		}
		reads := []string{
			"SELECT id, st, ms, sts FROM t ORDER BY id",
			"SELECT id, st.k FROM t ORDER BY id",
			"SELECT id, m, sm FROM t ORDER BY id",
			// A record the query constructs is built as a message, so its enum
			// is named from the descriptor, as a stored struct's is. Named with
			// AS, which both engines honour; the unnamed spelling is CQ-87's
			// (goDivergences).
			"SELECT id, (m AS m, id AS id) FROM t ORDER BY id",
			"SELECT id, (st.k AS k, m AS m) FROM t ORDER BY id",
			"SELECT id, (m, id) FROM t ORDER BY id",
			// An element of an unnested array of structs, projected whole, is
			// the one shape measured to reach the client as a positional row:
			// its enum arrives as a number at rowstruct.materializeOrdinalValue,
			// which names it. Measured with a probe at rowstruct.NewOrdinal over
			// 18 read shapes (constructed records, derived tables, joins, a star
			// over a derived table, an array of records, unnested elements):
			// only `SELECT t.id, x FROM t, t.sts AS x` reached it, once per
			// element; the record built from the element's fields next to it is
			// a message. Java cannot unnest the enum array itself ("quantifier
			// does not flow records"), so that shape is absent.
			"SELECT t.id, x FROM t, t.sts AS x",
			"SELECT t.id, (x.k AS k, x.n AS n) FROM t, t.sts AS x",
		}
		// INSERT admits each VALUES cell as ExpressionVisitor.coerceValueIfNecessary
		// does (PromoteValue.inject: a NULL takes the column's type), and the
		// whole row as RecordQueryInsertPlan.insertPlan does
		// (computePromotionsTrie over the flowed row, an INSERT … SELECT's too).
		insertAdmissions := []string{
			"INSERT INTO t(id, i) VALUES (10, 2147483648)",
			"INSERT INTO t(id, i) VALUES (11, 5)",
			"INSERT INTO t(id, b) VALUES (12, 5)",
			"INSERT INTO t(id, f) VALUES (13, 1.5)",
			"INSERT INTO t(id, f) VALUES (14, CAST(1.5 AS FLOAT))",
			"INSERT INTO t(id, f) VALUES (15, CAST(1.0e308 AS FLOAT))",
			"INSERT INTO t(id, d) VALUES (17, 3)",
			"INSERT INTO t(id, str) VALUES (18, 1)",
			"INSERT INTO t(id, b) VALUES (19, 'x')",
			"INSERT INTO t(id, u) VALUES (20, NULL)",
			"INSERT INTO t(id, m) VALUES (21, 'SAD')",
			"INSERT INTO t(id, m) VALUES (22, 1)",
			"INSERT INTO t(id, st) VALUES (23, ('SAD', 1.5))",
			"INSERT INTO t(id, ms) VALUES (24, ['SAD'])",
			"INSERT INTO t(id, ms) VALUES (25, [1])",
			"INSERT INTO t SELECT id + 100, b, b, str, m, o, sm, st, ms, sts, u, f, d FROM t WHERE id = 2",
			"INSERT INTO t SELECT id + 200, i, b, str, m, o, sm, st, ms, sts, u, d, d FROM t WHERE id = 2",
			"INSERT INTO t SELECT id + 300, i, b, str, m, o, sm, st, ms, sts, u, f, f FROM t WHERE id = 2",
			"INSERT INTO t SELECT id + 400, i, b, str, o, o, sm, st, ms, sts, u, f, d FROM t WHERE id = 2",
			"INSERT INTO t SELECT id + 500, i, b, str, m, o, sm, st, ms, sts, NULL, f, d FROM t WHERE id = 2",
			"INSERT INTO t SELECT id + 600, i FROM t WHERE id = 2",
			"INSERT INTO t SELECT id + 700, i FROM t WHERE id = 999",
		}
		updates := []string{
			"UPDATE t SET m = 1 WHERE id = 1",
			"UPDATE t SET m = 1 WHERE id = 999",
			"UPDATE t SET m = i WHERE id = 1",
			"UPDATE t SET m = o WHERE id = 1",
			"UPDATE t SET m = o WHERE id = 999",
			"UPDATE t SET m = 'ANGRY' WHERE id = 1",
			"UPDATE t SET m = str WHERE id = 1",
			"UPDATE t SET m = sm WHERE id = 1",
			"UPDATE t SET m = m WHERE id = 1",
			"UPDATE t SET m = NULL WHERE id = 2",
			"UPDATE t SET str = m WHERE id = 1",
			"UPDATE t SET m = 'HAPPY' WHERE id = 2",
			"UPDATE t SET sm = 'SAD' WHERE id = 2",
			"UPDATE t SET o = m WHERE id = 999",
			"UPDATE t SET m = st.k WHERE id = 1",
			"UPDATE t SET i = b WHERE id = 999",
			"UPDATE t SET i = 5 WHERE id = 1",
			"UPDATE t SET i = 3000000000 WHERE id = 999",
			"UPDATE t SET b = i WHERE id = 1",
			"UPDATE t SET str = 1 WHERE id = 999",
			"UPDATE t SET b = 1.5 WHERE id = 999",
			"UPDATE t SET b = 'x' WHERE id = 999",
			"UPDATE t SET st = ('HAPPY', 3) WHERE id = 1",
			"UPDATE t SET st = (1, 3) WHERE id = 999",
			"UPDATE t SET ms = ['SAD'] WHERE id = 999",
			"UPDATE t SET ms = ['SAD'] WHERE id = 1",
			"UPDATE t SET ms = [1] WHERE id = 999",
			"UPDATE t SET sts = [('HAPPY', 3)] WHERE id = 1",
			"UPDATE t SET u = NULL WHERE id = 999",
			"UPDATE t SET u = '5ea7a4c2-54a4-4f7a-9d0e-2d1b0b3f8e10' WHERE id = 2",
			"UPDATE t SET u = 1 WHERE id = 999",
			"UPDATE t SET st = NULL WHERE id = 999",
			"UPDATE t SET sts = NULL WHERE id = 999",
			"UPDATE t SET sts = [] WHERE id = 999",
			"UPDATE t SET i = NULL WHERE id = 999",
			"UPDATE t SET f = 100.5 WHERE id = 999",
			"UPDATE t SET f = 100.5 WHERE id = 1",
			"UPDATE t SET d = 100.5 WHERE id = 1",
			"UPDATE t SET f = 3 WHERE id = 999",
			"UPDATE t SET f = b WHERE id = 999",
			"UPDATE t SET f = d WHERE id = 999",
			"UPDATE t SET d = f WHERE id = 1",
			"UPDATE t SET i = 1.0 WHERE id = 999",
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
			return out["outcome"].(string)
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
		// Java answers "ERROR <state> <class> <message>", Go "ERROR <code>
		// <message>": the state and the message are what both carry.
		javaShape := func(o string) string {
			if f := strings.SplitN(o, " ", 4); f[0] == "ERROR" && len(f) == 4 {
				return "ERROR " + f[1] + " " + f[3]
			}
			return o
		}

		// Four statements the target fails on with an internal error, not a
		// designed refusal; Go answers each as the target was built to, and
		// the four are pinned here so a target that fixes any reddens this.
		//  - An enum into a string column: computePromotionsTrie reaches the
		//    bare Verify.verify(typeCodes equal) past its primitive arm
		//    (PromoteValue.java:374), VerifyException, XX000 "null". Go refuses
		//    it as INCOMPATIBLE_TYPE (values.CheckPromotionsTrie).
		//  - A string array into an enum array, once a row matches: planning
		//    admits it (STRING_TO_ENUM per element; with no row it answers
		//    OK 0 on both), but MessageHelpers.coerceArray hands an element a
		//    descriptor only when the element is a MESSAGE, so STRING_TO_ENUM
		//    casts a null EnumDescriptor: NullPointerException, XXXXX "null".
		//    Go stores the names.
		//  - A string into a UUID column, once a row matches: STRING_TO_UUID
		//    yields a java.util.UUID, which the update sets into the column's
		//    tuple_fields.UUID message field unconverted (protobuf reflection
		//    refuses it). Go stores the UUID.
		//  - A NULL projected by an INSERT … SELECT into a UUID column:
		//    UnsupportedOperationException "should not be called", XXXXX. Go
		//    answers what the target's own UPDATE answers for a NULL into a
		//    UUID (no NULL_TO_UUID promotion): INCOMPATIBLE_TYPE.
		const incompatible = "ERROR 22000 A value cannot be assigned to a variable because the type of the value does not match the type of the variable and cannot be promoted to the type of the variable."
		targetBugs := map[string][2]string{
			"UPDATE t SET str = m WHERE id = 1":      {"ERROR XX000 null", incompatible},
			"UPDATE t SET ms = ['SAD'] WHERE id = 1": {"ERROR XXXXX null", "OK 1"},
			"UPDATE t SET u = '5ea7a4c2-54a4-4f7a-9d0e-2d1b0b3f8e10' WHERE id = 2": {
				"ERROR XXXXX Wrong object type used with protocol message reflection.\nField number: 11, field java type: MESSAGE, value type: java.util.UUID\n",
				"OK 1",
			},
			"INSERT INTO t SELECT id + 500, i, b, str, m, o, sm, st, ms, sts, NULL, f, d FROM t WHERE id = 2": {
				"ERROR XXXXX should not be called", incompatible,
			},
		}
		goDivergences := map[string][2]string{}
		var diffs []string
		compared := 0
		compare := func(what, j, g string) {
			compared++
			GinkgoWriter.Printf("WSJENUMN %s\n  java=%s\n  go  =%s\n", what, j, g)
			if want, ok := goDivergences[what]; ok {
				if j != want[0] || g != want[1] {
					diffs = append(diffs, fmt.Sprintf("%s: java=%s go=%s, pinned java=%s go=%s", what, j, g, want[0], want[1]))
				}
				return
			}
			if want, ok := targetBugs[what]; ok {
				if j != want[0] || g != want[1] {
					diffs = append(diffs, fmt.Sprintf("%s: java=%s go=%s, pinned java=%s go=%s", what, j, g, want[0], want[1]))
				}
				return
			}
			if j != g {
				diffs = append(diffs, fmt.Sprintf("%s: java=%s go=%s", what, j, g))
			}
		}
		for _, stmt := range inserts {
			compare(stmt, javaShape(javaExec(stmt)), goExec(stmt))
		}
		for _, q := range reads {
			compare(q, javaRead(q), goRead(q))
		}
		// The names themselves, not only agreement: an enum in a struct, in an
		// array, and in a struct in an array.
		const names = "[[1 map[K:SAD N:7] [HAPPY SAD] [map[K:JOYFUL N:1] map[K:SAD N:2]]] [2 <nil> <nil> <nil>]]"
		if got := goRead(reads[0]); got != names {
			diffs = append(diffs, fmt.Sprintf("%s: go=%s, want the names %s", reads[0], got, names))
		}
		// The same through a record the query builds and through an unnested
		// element (the positional row path).
		for q, want := range map[string]string{
			reads[3]: "[[1 map[ID:1 M:JOYFUL]] [2 map[ID:2]]]",
			reads[4]: "[[1 map[K:SAD M:JOYFUL]] [2 map[]]]",
			reads[6]: "[[1 map[K:JOYFUL N:1]] [1 map[K:SAD N:2]]]",
		} {
			if got := goRead(q); got != want {
				diffs = append(diffs, fmt.Sprintf("%s: go=%s, want the names %s", q, got, want))
			}
		}
		for _, stmt := range insertAdmissions {
			compare(stmt, javaShape(javaExec(stmt)), goExec(stmt))
		}
		for _, stmt := range updates {
			compare(stmt, javaShape(javaExec(stmt)), goExec(stmt))
		}
		const state = "SELECT id, i, b, str, m, o, sm, st, sts, f, d FROM t ORDER BY id"
		compare("after the updates: "+state, javaRead(state), goRead(state))
		// The two columns the target's crashes left apart.
		const apart = "SELECT id, ms, u FROM t WHERE id < 10 ORDER BY id"
		Expect(javaRead(apart)).To(Equal("[[1 [HAPPY SAD] <nil>] [2 <nil> <nil>]]"))
		Expect(goRead(apart)).To(Equal("[[1 [SAD] <nil>] [2 <nil> 5ea7a4c2-54a4-4f7a-9d0e-2d1b0b3f8e10]]"))
		Expect(diffs).To(BeEmpty())
		Expect(compared).To(Equal(len(inserts) + len(reads) + len(insertAdmissions) + len(updates) + 1))
	})
})

// The enum DDL refusals, code and text, on both engines: a type name taken
// twice (the enum is registered before any struct or table, Java's clause
// loop), and a value name no escape makes a protobuf identifier.
var _ = Describe("RFC-257 WS-J enum DDL refusals", func() {
	It("refuses as the target, with its code and text", func() {
		ctx := context.Background()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		clusterFilePath := writeClusterFileToTemp(clusterFile)
		defer os.Remove(clusterFilePath)
		sysDB, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", clusterFilePath))
		Expect(err).NotTo(HaveOccurred())
		defer sysDB.Close()
		p := "WSJENUMR" + strings.ToUpper(strings.ReplaceAll(uuid.New().String()[:8], "-", ""))
		bodies := []string{
			"create type as enum t('A') create table t(id bigint, primary key(id))",
			"create type as enum s('A') create type as struct s(x bigint) create table u(id bigint, primary key(id))",
			"create type as enum mood('O''NEIL') create table t(id bigint, m mood, primary key(id))",
		}
		var diffs []string
		for i, body := range bodies {
			tmpl := fmt.Sprintf("%s_%d", p, i)
			var out map[string]any
			Expect(java.InvokeAs(ctx, "wsjCatalogDdlJava", map[string]any{
				"clusterFile": clusterFile, "statements": []string{"CREATE SCHEMA TEMPLATE " + tmpl + "_J " + body},
			}, &out)).To(Succeed())
			j := out["outcomes"].([]any)[0].(string)
			g := "OK"
			if _, err := sysDB.ExecContext(ctx, "CREATE SCHEMA TEMPLATE "+tmpl+"_G "+body); err != nil {
				var ae *api.Error
				Expect(errors.As(err, &ae)).To(BeTrue(), "not an api.Error: %v", err)
				g = "ERROR " + string(ae.Code) + " " + ae.Message
			}
			GinkgoWriter.Printf("WSJENUMR %s\n  java=%s\n  go  =%s\n", body, j, g)
			Expect(j).To(HavePrefix("ERROR "), "the target accepted %s", body)
			if j != g {
				diffs = append(diffs, fmt.Sprintf("%s: java=%s go=%s", body, j, g))
			}
		}
		Expect(diffs).To(BeEmpty())
	})
})

// wsjEnumJSONShape renders a value the Go driver returned the way the Java
// runner's JSON decodes the same value: a struct as its attributes by name, an
// array element by element, an integer as a float64.
func wsjEnumJSONShape(v any) any {
	switch x := v.(type) {
	case api.Struct:
		out := map[string]any{}
		md := x.MetaData()
		for i := 1; i <= x.AttributeCount(); i++ {
			name, err := md.AttributeName(i)
			Expect(err).NotTo(HaveOccurred())
			a, err := x.Attribute(i)
			Expect(err).NotTo(HaveOccurred())
			// The Java side's JSON drops a null struct member (Gson's default
			// serialization skips a JsonNull member; an array keeps its nulls),
			// so a null attribute is dropped here too.
			if a == nil {
				continue
			}
			out[name] = wsjEnumJSONShape(a)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = wsjEnumJSONShape(e)
		}
		return out
	case int64:
		return float64(x)
	case int32:
		return float64(x)
	}
	return v
}
