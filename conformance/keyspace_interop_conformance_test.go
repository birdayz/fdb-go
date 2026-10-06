package conformance_test

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/keyspace"
)

// The relational key space is Java's RelationalKeyspaceProvider byte for byte:
// a schema's store is (domain, database, schema), the domain a directory of
// the FDB directory layer and the names interned in the domain's interning
// layer, and the catalog is (NULL, NULL, 0). Both engines share one cluster
// here, so a schema either one creates is found by the other at the same
// prefix, and its rows are read across engines.
var _ = Describe("Relational key space shared with Java", func() {
	It("resolves and reads a schema either engine created", func() {
		ctx := context.Background()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		clusterFilePath := writeClusterFileToTemp(clusterFile)
		name := "KSINTEROP_" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")
		var created struct {
			Created bool `json:"created"`
		}
		Expect(java.InvokeAs(ctx, "createSchemaTemplatePersistentJava", map[string]any{
			"clusterFile": clusterFile, "templateName": name,
			"schemaTemplateBody": "CREATE TABLE T (id BIGINT, PRIMARY KEY (id))",
		}, &created)).To(Succeed())
		defer func() {
			var dropped struct {
				Dropped bool `json:"dropped"`
			}
			_ = java.InvokeAs(context.Background(), "dropSchemaTemplatePersistentJava", map[string]any{
				"clusterFile": clusterFile, "templateName": name,
			}, &dropped)
		}()
		ks := keyspace.New(subspace.Sub())
		rdb := recordlayer.NewFDBDatabase(sharedDB)
		prefixOf := func(ints []int) []byte {
			out := make([]byte, len(ints))
			for i, b := range ints {
				out[i] = byte(b)
			}
			return out
		}
		open := func(dbPath, schema string) *sql.DB {
			db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s", dbPath, clusterFilePath, schema))
			Expect(err).NotTo(HaveOccurred())
			return db
		}

		// Java creates the database and schema; Go finds the store at Java's
		// prefix and writes a row Java's SQL then counts.
		var store struct {
			DbPath      string `json:"dbPath"`
			SchemaName  string `json:"schemaName"`
			StorePrefix []int  `json:"storePrefix"`
		}
		Expect(java.InvokeAs(ctx, "wsjOpenStoreJava", map[string]any{"clusterFile": clusterFile, "templateName": name}, &store)).To(Succeed())
		defer func() {
			var dropped struct {
				Dropped bool `json:"dropped"`
			}
			_ = java.InvokeAs(context.Background(), "wsjDropDatabaseJava", map[string]any{"clusterFile": clusterFile, "dbPath": store.DbPath}, &dropped)
		}()
		goSS, err := ks.LookupSchemaSubspace(ctx, rdb, store.DbPath, store.SchemaName)
		Expect(err).NotTo(HaveOccurred())
		Expect(goSS.Bytes()).To(Equal(prefixOf(store.StorePrefix)), "Go resolves Java's store prefix")
		jdb := open(store.DbPath, store.SchemaName)
		defer jdb.Close()
		_, err = jdb.ExecContext(ctx, "INSERT INTO T VALUES (41)")
		Expect(err).NotTo(HaveOccurred())
		var javaSees struct {
			StorePrefix []int `json:"storePrefix"`
			Rows        int64 `json:"rows"`
		}
		Expect(java.InvokeAs(ctx, "schemaStorePrefixJava", map[string]any{
			"clusterFile": clusterFile, "dbPath": store.DbPath, "schemaName": store.SchemaName,
		}, &javaSees)).To(Succeed())
		Expect(javaSees.Rows).To(Equal(int64(1)), "Java reads the row Go wrote into Java's store")

		// Go creates a database and a schema over the template Java created;
		// Java resolves the same prefix and reads Go's rows.
		goDB := "/TEST/KSGO_" + strings.ReplaceAll(uuid.New().String()[:8], "-", "")
		sys := open("/__SYS", "CATALOG")
		defer sys.Close()
		_, err = sys.ExecContext(ctx, `CREATE DATABASE "`+goDB+`"`)
		Expect(err).NotTo(HaveOccurred())
		defer func() { _, _ = sys.ExecContext(context.Background(), `DROP DATABASE IF EXISTS "`+goDB+`"`) }()
		_, err = sys.ExecContext(ctx, `CREATE SCHEMA "`+goDB+`/S" WITH TEMPLATE "`+name+`"`)
		Expect(err).NotTo(HaveOccurred())
		gdb := open(goDB, "S")
		defer gdb.Close()
		_, err = gdb.ExecContext(ctx, "INSERT INTO T VALUES (1), (2)")
		Expect(err).NotTo(HaveOccurred())
		goSS, err = ks.LookupSchemaSubspace(ctx, rdb, goDB, "S")
		Expect(err).NotTo(HaveOccurred())
		Expect(java.InvokeAs(ctx, "schemaStorePrefixJava", map[string]any{
			"clusterFile": clusterFile, "dbPath": goDB, "schemaName": "S",
		}, &javaSees)).To(Succeed())
		Expect(prefixOf(javaSees.StorePrefix)).To(Equal(goSS.Bytes()), "Java resolves Go's store prefix")
		Expect(javaSees.Rows).To(Equal(int64(2)), "Java reads the rows Go wrote into Go's store")
	})
})
