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
			names  func(dbPath string) []string
			query  func(dbPath, schema string) string
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

		javaEngine := engine{
			prefix: "WSJDSNJ" + suffix,
			ddl: func(stmts ...string) []string {
				var out map[string]any
				Expect(java.InvokeAs(ctx, "wsjCatalogDdlJava", map[string]any{"clusterFile": clusterFile, "statements": stmts}, &out)).To(Succeed())
				var got []string
				for _, o := range out["outcomes"].([]any) {
					got = append(got, o.(string))
				}
				return got
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
		goEngine := engine{
			prefix: "WSJDSNG" + suffix,
			ddl: func(stmts ...string) []string {
				var got []string
				for _, stmt := range stmts {
					_, err := sysDB.ExecContext(ctx, stmt)
					got = append(got, outcome(err))
				}
				return got
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
			defer e.ddl("DROP DATABASE IF EXISTS "+dbPath, "DROP SCHEMA TEMPLATE IF EXISTS "+t)
			var out []string
			add := func(label string, got ...string) {
				for i, g := range got {
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
		Expect(javaOut).To(HaveLen(5 + 1 + 7 + 1 + 2 + 2))
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
