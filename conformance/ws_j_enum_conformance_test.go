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
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/keyspace"
)

// WS-J step 5 (design section 5): an enum column written and read by each
// engine. The Go SQL driver and Java's keep their catalogs on different
// keyspaces today (TODO.md "Go SQL driver stores the relational catalog and
// user schemas on a Go-only keyspace", F11), so neither driver can open a
// schema the other created. What is compared instead is everything a shared
// store would expose: each engine creates the same template through its own
// DDL and inserts the same rows through its own driver, and the stored records
// and the enum index's entries must be byte-equal relative to each store; the
// same query must answer the same rows in each (a value by its name,
// RowStruct.getString's spelling); an undeclared name and an integer must be
// refused alike. Byte-equal writes are what let each engine read the other's.
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
			ss, err := keyspace.New(subspace.Sub()).SchemaSubspace(goDBPath, "S")
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

		// Java's relational store writes through TransformedRecordSerializer
		// (StoreConfig.DEFAULT_RELATIONAL_SERIALIZER): a record too small to
		// compress is stored behind the one-byte prefix PREFIX_CLEAR, 02. The Go
		// driver writes the message bare, which Java's decodePrefix reads as
		// unprefixed (TransformedRecordSerializerPrefix.java:81-83), while Go
		// cannot read the prefix at all (TODO.md "The Go record layer cannot
		// read a record Java wrote through TransformedRecordSerializer"). The
		// prefix is asserted here, not skipped, and the MESSAGES compared: this
		// spec reddens when the serializer port lands and must be restated then.
		GinkgoWriter.Printf("WSJENUM records java=%v go=%v\n", javaRecords.Entries, goRecords)
		Expect(goRecords).To(HaveLen(len(rows)))
		Expect(javaRecords.Entries).To(HaveLen(len(rows)))
		javaMessages := make([]string, len(javaRecords.Entries))
		for i, e := range javaRecords.Entries {
			key, value, ok := strings.Cut(e, "=")
			Expect(ok).To(BeTrue())
			Expect(value).To(HavePrefix("02"), "Java's record %s is not behind PREFIX_CLEAR", key)
			javaMessages[i] = key + "=" + strings.TrimPrefix(value, "02")
		}
		for _, e := range goRecords {
			_, value, _ := strings.Cut(e, "=")
			Expect(value).NotTo(HavePrefix("02"), "Go's record carries a serializer prefix: the serializer port landed; restate this spec")
		}
		Expect(goRecords).To(Equal(javaMessages), "the records Go wrote differ from Java's")
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
})
