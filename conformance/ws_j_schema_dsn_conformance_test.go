//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/keyspace"
)

// The connection's schema option (RFC-257 follow-up, TODO.md "The Go driver
// folds an unquoted ?schema= connection value"): which schema name CREATE
// SCHEMA stores for an unquoted path, and which ?schema= spellings reach it,
// through each engine's own driver. Java's parseConnectionQueryString upper-
// cases the option name and keeps the value (RecordLayerStorageCluster.java:
// 76-124); what its DDL stores for the path is what this measures.
var _ = Describe("RFC-257 the DSN's schema option reaches the schema Java's does", func() {
	It("stored names and the spellings that connect", func() {
		ctx := context.Background()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		clusterFilePath := writeClusterFileToTemp(clusterFile)
		defer os.Remove(clusterFilePath)
		suffix := strings.ToUpper(strings.ReplaceAll(uuid.New().String()[:8], "-", ""))

		type engine struct {
			prefix string
			ddl    func(stmts ...string) []string
			// ddlOn runs the statements on a connection to (dbPath, schema).
			ddlOn func(dbPath, schema string, stmts ...string) []string
			names func(dbPath string) []string
			query func(dbPath, schema string) string
		}
		outcome := func(err error) string {
			if err == nil {
				return "OK"
			}
			var ae *api.Error
			if errors.As(err, &ae) {
				return "ERROR " + string(ae.Code) + " " + ae.Message
			}
			var je *JavaError
			if errors.As(err, &je) {
				return "ERROR " + je.SQLState + " " + je.Message
			}
			return "ERROR " + err.Error()
		}

		javaOutcomes := func(out map[string]any) []string {
			var got []string
			for _, o := range out["outcomes"].([]any) {
				got = append(got, o.(string))
			}
			return got
		}
		javaEngine := engine{
			prefix: "WSJDSNJ" + suffix,
			ddl: func(stmts ...string) []string {
				var out map[string]any
				Expect(java.InvokeAs(ctx, "wsjCatalogDdlJava", map[string]any{"clusterFile": clusterFile, "statements": stmts}, &out)).To(Succeed())
				return javaOutcomes(out)
			},
			ddlOn: func(dbPath, schema string, stmts ...string) []string {
				var out map[string]any
				Expect(java.InvokeAs(ctx, "wsjCatalogDdlOnJava", map[string]any{
					"clusterFile": clusterFile, "dbPath": dbPath, "schemaName": schema, "statements": stmts,
				}, &out)).To(Succeed())
				return javaOutcomes(out)
			},
			names: func(dbPath string) []string {
				var out map[string]any
				Expect(java.InvokeAs(ctx, "wsjListSchemasJava", map[string]any{"clusterFile": clusterFile, "dbPath": dbPath}, &out)).To(Succeed())
				var got []string
				for _, n := range out["names"].([]any) {
					got = append(got, n.(string))
				}
				return got
			},
			query: func(dbPath, schema string) string {
				var out map[string]any
				return outcome(java.InvokeAs(ctx, "wsjQueryJava", map[string]any{
					"clusterFile": clusterFile, "dbPath": dbPath, "schemaName": schema, "querySql": "SELECT * FROM X",
				}, &out))
			},
		}

		sysDB, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", clusterFilePath))
		Expect(err).NotTo(HaveOccurred())
		defer sysDB.Close()
		db := recordlayer.NewFDBDatabase(sharedDB)
		goCat, err := catalog.NewRecordLayerStoreCatalog(keyspace.New(subspace.Sub()).CatalogSubspace())
		Expect(err).NotTo(HaveOccurred())
		// goDDL reports what wsjDdlOn reports: a statement with a result set its
		// first column sorted, "ROWS a,b"; any other OK or its error.
		goDDL := func(conn *sql.DB, stmts ...string) []string {
			var got []string
			for _, stmt := range stmts {
				if !strings.HasPrefix(strings.ToUpper(stmt), "SHOW ") {
					_, err := conn.ExecContext(ctx, stmt)
					got = append(got, outcome(err))
					continue
				}
				rows, err := conn.QueryContext(ctx, stmt)
				if err != nil {
					got = append(got, outcome(err))
					continue
				}
				var firsts []string
				cols, err := rows.Columns()
				Expect(err).NotTo(HaveOccurred())
				for rows.Next() {
					vals := make([]any, len(cols))
					ptrs := make([]any, len(cols))
					for i := range vals {
						ptrs[i] = &vals[i]
					}
					Expect(rows.Scan(ptrs...)).To(Succeed())
					firsts = append(firsts, fmt.Sprint(vals[0]))
				}
				Expect(rows.Err()).NotTo(HaveOccurred())
				_ = rows.Close()
				sort.Strings(firsts)
				got = append(got, "ROWS "+strings.Join(firsts, ","))
			}
			return got
		}
		goEngine := engine{
			prefix: "WSJDSNG" + suffix,
			ddl: func(stmts ...string) []string {
				return goDDL(sysDB, stmts...)
			},
			ddlOn: func(dbPath, schema string, stmts ...string) []string {
				conn, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", dbPath, clusterFilePath, schema))
				Expect(err).NotTo(HaveOccurred())
				defer conn.Close()
				return goDDL(conn, stmts...)
			},
			names: func(dbPath string) []string {
				var got []string
				_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					rs, err := goCat.ListSchemasInDatabase(catalog.NewFDBTransaction(rtx), dbPath, nil)
					if err != nil {
						return nil, err
					}
					defer rs.Close()
					got = nil
					for rs.Next() {
						name, err := rs.StringByName("SCHEMA_NAME")
						if err != nil {
							return nil, err
						}
						got = append(got, name)
					}
					return nil, rs.Err()
				})
				Expect(err).NotTo(HaveOccurred())
				return got
			},
			query: func(dbPath, schema string) string {
				conn, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", dbPath, clusterFilePath, schema))
				Expect(err).NotTo(HaveOccurred())
				defer conn.Close()
				rows, err := conn.QueryContext(ctx, "SELECT * FROM X")
				if err == nil {
					for rows.Next() {
					}
					err = rows.Err()
					_ = rows.Close()
				}
				return outcome(err)
			},
		}

		run := func(e engine) []string {
			p := e.prefix
			dbPath, t := "/TEST/"+p+"_D", p+"_T"
			// The four template spellings (a quoted one in upper and one in
			// mixed case), and three only dropped.
			lc, q, mx := strings.ToLower(t)+"_lc", `"`+t+`_Q"`, t+"_Mx"
			qm := `"` + t + `_qMx"`
			dq, dlc, dqm := `"`+t+`_DQ"`, strings.ToLower(t)+"_dlc", `"`+t+`_dqMx"`
			defer e.ddl("DROP DATABASE IF EXISTS "+dbPath, "DROP SCHEMA TEMPLATE IF EXISTS "+t,
				"DROP SCHEMA TEMPLATE IF EXISTS "+lc, "DROP SCHEMA TEMPLATE IF EXISTS "+q,
				"DROP SCHEMA TEMPLATE IF EXISTS "+mx, "DROP SCHEMA TEMPLATE IF EXISTS "+dq,
				"DROP SCHEMA TEMPLATE IF EXISTS "+dlc, "DROP SCHEMA TEMPLATE IF EXISTS "+qm,
				"DROP SCHEMA TEMPLATE IF EXISTS "+dqm)
			var out []string
			add := func(label string, got ...string) {
				for i, g := range got {
					// A listing holds the whole cluster's names: keep this run's.
					if rest, ok := strings.CutPrefix(g, "ROWS "); ok {
						var mine []string
						for _, name := range strings.Split(rest, ",") {
							if strings.Contains(strings.ToUpper(name), p) {
								mine = append(mine, name)
							}
						}
						g = "ROWS " + strings.Join(mine, ",")
					}
					g = strings.ReplaceAll(strings.ReplaceAll(g, p, "P"), strings.ToLower(p), "p")
					out = append(out, fmt.Sprintf("%s[%d]: %s", label, i, g))
				}
			}
			add("create", e.ddl(
				"CREATE DATABASE "+dbPath,
				"CREATE SCHEMA TEMPLATE "+t+" CREATE TABLE X(ID BIGINT, PRIMARY KEY(ID))",
				"CREATE SCHEMA "+dbPath+"/test1 WITH TEMPLATE "+t,
				"CREATE SCHEMA "+dbPath+"/UP2 WITH TEMPLATE "+t,
				"CREATE SCHEMA "+dbPath+"/Mixed3 WITH TEMPLATE "+t)...)
			names := e.names(dbPath)
			sort.Strings(names)
			add("stored names", strings.Join(names, ","))
			for _, s := range []string{"test1", "TEST1", "UP2", "up2", "Mixed3", "MIXED3", "mixed3"} {
				add("?schema="+s, e.query(dbPath, s))
			}
			// DROP SCHEMA takes a path. A bare uid names no database, and Java
			// refuses it (DdlVisitor.java:598-600) whatever database the
			// connection is on; the schema it names stays.
			add("bare DROP SCHEMA, on the database", e.ddlOn(dbPath, "MIXED3", "DROP SCHEMA up2")...)
			add("bare DROP SCHEMA, on the catalog", e.ddl("DROP SCHEMA test1")...)
			names = e.names(dbPath)
			sort.Strings(names)
			add("stored names after the bare drops", strings.Join(names, ","))
			// Template names are identifiers (DdlVisitor.visitUid at :495, :573
			// and :607): an unquoted name folds, a quoted one keeps its case.
			add("template names", e.ddl(
				"CREATE SCHEMA TEMPLATE "+lc+" CREATE TABLE X(ID BIGINT, PRIMARY KEY(ID))",
				"CREATE SCHEMA TEMPLATE "+q+" CREATE TABLE X(ID BIGINT, PRIMARY KEY(ID))",
				"CREATE SCHEMA TEMPLATE "+mx+" CREATE TABLE X(ID BIGINT, PRIMARY KEY(ID))",
				"SHOW SCHEMA TEMPLATES",
				"CREATE SCHEMA "+dbPath+"/T5 WITH TEMPLATE "+strings.ToUpper(lc),
				"CREATE SCHEMA "+dbPath+"/T6 WITH TEMPLATE "+q,
				"CREATE SCHEMA "+dbPath+"/T7 WITH TEMPLATE "+strings.ToLower(mx),
				"CREATE SCHEMA "+dbPath+`/T8 WITH TEMPLATE "`+mx+`"`,
				// A quoted mixed-case name keeps its case: only the quoted
				// spelling reaches it, and the unquoted one folds past it.
				"CREATE SCHEMA TEMPLATE "+qm+" CREATE TABLE X(ID BIGINT, PRIMARY KEY(ID))",
				"CREATE SCHEMA "+dbPath+"/T9 WITH TEMPLATE "+qm,
				"CREATE SCHEMA "+dbPath+"/T10 WITH TEMPLATE "+strings.Trim(qm, `"`))...)
			add("template drops", e.ddl(
				"CREATE SCHEMA TEMPLATE "+dq+" CREATE TABLE X(ID BIGINT, PRIMARY KEY(ID))",
				"CREATE SCHEMA TEMPLATE "+dlc+" CREATE TABLE X(ID BIGINT, PRIMARY KEY(ID))",
				"DROP SCHEMA TEMPLATE "+dq,
				"DROP SCHEMA TEMPLATE "+strings.ToUpper(dlc),
				"CREATE SCHEMA TEMPLATE "+dqm+" CREATE TABLE X(ID BIGINT, PRIMARY KEY(ID))",
				"DROP SCHEMA TEMPLATE "+strings.Trim(dqm, `"`),
				"DROP SCHEMA TEMPLATE "+dqm,
				"SHOW SCHEMA TEMPLATES")...)
			// A missing database: the schema's absence is reported as the
			// database's.
			add("a missing database", e.query("/TEST/"+p+"_NODB", "TEST1"))
			// A database path in lower case: what CREATE DATABASE stores, and
			// which spelling of the path a connection reaches it by.
			lower := "/test/" + strings.ToLower(p) + "_low"
			add("lower-case database", e.ddl(
				"CREATE DATABASE "+lower,
				"CREATE SCHEMA "+lower+"/s4 WITH TEMPLATE "+t)...)
			add("lower-case database, as created", e.query(lower, "S4"))
			add("lower-case database, upper-cased", e.query(strings.ToUpper(lower), "S4"))
			add("lower-case drops", e.ddl("DROP SCHEMA "+lower+"/s4", "DROP DATABASE "+lower)...)
			e.ddl("DROP DATABASE IF EXISTS "+lower, "DROP DATABASE IF EXISTS "+strings.ToUpper(lower))
			return out
		}
		javaOut := run(javaEngine)
		goOut := run(goEngine)
		for i := range javaOut {
			GinkgoWriter.Printf("WSJDSN java %s\n", javaOut[i])
			if i < len(goOut) {
				GinkgoWriter.Printf("WSJDSN go   %s\n", goOut[i])
			}
		}
		Expect(javaOut).To(HaveLen(5 + 1 + 7 + 3 + 11 + 8 + 1 + 2 + 2 + 2))
		Expect(goOut).To(Equal(javaOut))
	})
})

// SHOW DATABASES WITH PREFIX is a declared divergence (DIVERGENCES.md, "A
// connection to a database that does not exist yet opens"): Java reads the
// prefix (MetadataPlanVisitor.visitShowDatabasesStatement) and its
// CatalogQueryFactory then lists every database ("TODO(bfines) make use of this
// prefix"); Go honours the scope. Each side is pinned by itself, so either
// engine moving reddens this: Java's listing reaches past the prefix (it holds
// /__SYS), Go's holds exactly the database under it, whichever case the
// prefix is written in.
var _ = Describe("RFC-257 SHOW DATABASES WITH PREFIX: Java lists every database, Go the prefix", func() {
	It("each engine's listing", func() {
		ctx := context.Background()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		clusterFilePath := writeClusterFileToTemp(clusterFile)
		defer os.Remove(clusterFilePath)
		p := "WSJSHOW" + strings.ToUpper(strings.ReplaceAll(uuid.New().String()[:8], "-", ""))

		var out map[string]any
		javaDDL := func(stmts ...string) []string {
			Expect(java.InvokeAs(ctx, "wsjCatalogDdlJava", map[string]any{"clusterFile": clusterFile, "statements": stmts}, &out)).To(Succeed())
			var got []string
			for _, o := range out["outcomes"].([]any) {
				got = append(got, o.(string))
			}
			return got
		}
		jdb := "/TEST/" + p + "J"
		defer javaDDL("DROP DATABASE IF EXISTS " + jdb)
		got := javaDDL("CREATE DATABASE "+jdb, "SHOW DATABASES WITH PREFIX "+strings.ToLower(jdb))
		GinkgoWriter.Printf("WSJSHOW java %v\n", got)
		Expect(got[0]).To(Equal("OK"))
		Expect(got[1]).To(HavePrefix("ROWS "))
		javaRows := strings.Split(strings.TrimPrefix(got[1], "ROWS "), ",")
		Expect(javaRows).To(ContainElement(jdb))
		Expect(javaRows).To(ContainElement("/__SYS"), "Java's listing no longer reaches past the prefix: "+
			"it honours the scope now, and Go's divergence note is stale")

		sysDB, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", clusterFilePath))
		Expect(err).NotTo(HaveOccurred())
		defer sysDB.Close()
		gdb := "/TEST/" + p + "G"
		_, err = sysDB.ExecContext(ctx, "CREATE DATABASE "+strings.ToLower(gdb))
		Expect(err).NotTo(HaveOccurred())
		defer sysDB.ExecContext(ctx, "DROP DATABASE IF EXISTS "+gdb)
		for _, prefix := range []string{strings.ToLower(gdb), gdb} {
			rows, err := sysDB.QueryContext(ctx, "SHOW DATABASES WITH PREFIX "+prefix)
			Expect(err).NotTo(HaveOccurred())
			var goRows []string
			for rows.Next() {
				var name string
				Expect(rows.Scan(&name)).To(Succeed())
				goRows = append(goRows, name)
			}
			Expect(rows.Err()).NotTo(HaveOccurred())
			_ = rows.Close()
			GinkgoWriter.Printf("WSJSHOW go %s %v\n", prefix, goRows)
			Expect(goRows).To(Equal([]string{gdb}), "Go's SHOW DATABASES WITH PREFIX %s", prefix)
		}
	})
})
