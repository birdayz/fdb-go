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

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/relational/core/metadata"
)

// RFC-257 WS-J step 4 (ws-j-design.md section 2): CREATE SCHEMA's order and the
// catalog's saveSchema under each SchemaExistsBehavior, arm by arm, through both
// engines; each outcome's SQLSTATE and message must be the target's. Each
// engine runs the arms on its own names (the Go driver keeps its catalog and
// stores on a Go-only keyspace, TODO.md "Go SQL driver stores the relational
// catalog and user schemas on a Go-only keyspace"), and the outcomes are
// compared with each engine's prefix replaced by one placeholder.
var _ = Describe("WS-J existence policies answer as the target", func() {
	It("CREATE SCHEMA's precedence and saveSchema's four behaviours", func() {
		ctx := context.Background()
		java := NewJavaInvoker()
		clusterFile, err := sharedContainer.ClusterFile(ctx)
		Expect(err).NotTo(HaveOccurred())
		clusterFilePath := writeClusterFileToTemp(clusterFile)
		defer os.Remove(clusterFilePath)
		suffix := strings.ToUpper(strings.ReplaceAll(uuid.New().String()[:8], "-", ""))

		type engine struct {
			prefix      string
			ddl         func(stmts ...string) []string
			bareStore   func(dbPath, schema, template string)
			copyVersion func(template string, from, to int)
			saveSchema  func(dbPath, schema, template string, version int, behavior string) string
		}

		javaEngine := engine{
			prefix: "WSJEPJ" + suffix,
			ddl: func(stmts ...string) []string {
				var out map[string]any
				Expect(java.InvokeAs(ctx, "wsjCatalogDdlJava", map[string]any{
					"clusterFile": clusterFile, "statements": stmts,
				}, &out)).To(Succeed())
				var got []string
				for _, o := range out["outcomes"].([]any) {
					got = append(got, o.(string))
				}
				return got
			},
			bareStore: func(dbPath, schema, template string) {
				Expect(java.InvokeAs(ctx, "wsjCreateBareStoreJava", map[string]any{
					"clusterFile": clusterFile, "dbPath": dbPath, "schemaName": schema, "templateName": template,
				}, nil)).To(Succeed())
			},
			copyVersion: func(template string, from, to int) {
				Expect(java.InvokeAs(ctx, "wsjCopyTemplateVersionJava", map[string]any{
					"clusterFile": clusterFile, "templateName": template, "fromVersion": from, "toVersion": to,
				}, nil)).To(Succeed())
			},
			saveSchema: func(dbPath, schema, template string, version int, behavior string) string {
				var out map[string]any
				Expect(java.InvokeAs(ctx, "wsjSaveSchemaJava", map[string]any{
					"clusterFile": clusterFile, "dbPath": dbPath, "schemaName": schema, "templateName": template,
					"version": version, "behavior": behavior,
				}, &out)).To(Succeed())
				return out["outcome"].(string)
			},
		}

		sysDB, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql:///__SYS?cluster_file=%s", clusterFilePath))
		Expect(err).NotTo(HaveOccurred())
		defer sysDB.Close()
		db := recordlayer.NewFDBDatabase(sharedDB)
		ks := keyspace.New(subspace.Sub())
		goCat, err := catalog.NewRecordLayerStoreCatalog(ks.CatalogSubspace())
		Expect(err).NotTo(HaveOccurred())
		outcome := func(err error) string {
			if err == nil {
				return "OK"
			}
			var ae *api.Error
			if errors.As(err, &ae) {
				return "ERROR " + string(ae.Code) + " " + ae.Message
			}
			return "ERROR (not an api.Error) " + err.Error()
		}
		behaviors := map[string]api.SchemaExistsBehavior{
			"ERROR": api.SchemaExistsError, "ERROR_IF_DIFFERENT": api.SchemaExistsErrorIfDifferent,
			"DO_NOTHING": api.SchemaExistsDoNothing, "UPGRADE": api.SchemaExistsUpgrade,
		}
		goEngine := engine{
			prefix: "WSJEPG" + suffix,
			ddl: func(stmts ...string) []string {
				var got []string
				for _, stmt := range stmts {
					_, err := sysDB.ExecContext(ctx, stmt)
					got = append(got, outcome(err))
				}
				return got
			},
			bareStore: func(dbPath, schema, template string) {
				ss, err := ks.SchemaSubspace(dbPath, schema)
				Expect(err).NotTo(HaveOccurred())
				_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					t, err := goCat.SchemaTemplateCatalog().LoadSchemaTemplateAtVersion(catalog.NewFDBTransaction(rtx), template, 1)
					if err != nil {
						return nil, err
					}
					md := t.(*metadata.RecordLayerSchemaTemplate).Underlying()
					_, err = recordlayer.NewStoreBuilder().SetContext(rtx).SetSubspace(ss).SetMetaDataProvider(md).Create()
					return nil, err
				})
				Expect(err).NotTo(HaveOccurred())
			},
			copyVersion: func(template string, from, to int) {
				_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					tx := catalog.NewFDBTransaction(rtx)
					t, err := goCat.SchemaTemplateCatalog().LoadSchemaTemplateAtVersion(tx, template, from)
					if err != nil {
						return nil, err
					}
					next, err := metadata.NewRecordLayerSchemaTemplateWithVersion(template, t.(*metadata.RecordLayerSchemaTemplate).Underlying(), to)
					if err != nil {
						return nil, err
					}
					return nil, goCat.SchemaTemplateCatalog().CreateTemplate(tx, next)
				})
				Expect(err).NotTo(HaveOccurred())
			},
			saveSchema: func(dbPath, schema, template string, version int, behavior string) string {
				b, ok := behaviors[behavior]
				Expect(ok).To(BeTrue(), behavior)
				_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					tx := catalog.NewFDBTransaction(rtx)
					t, err := goCat.SchemaTemplateCatalog().LoadSchemaTemplateAtVersion(tx, template, version)
					if err != nil {
						return nil, err
					}
					return nil, goCat.SaveSchema(tx, t.GenerateSchema(dbPath, schema), false, b)
				})
				return outcome(err)
			},
		}

		order := []string{"ERROR", "ERROR_IF_DIFFERENT", "DO_NOTHING", "UPGRADE"}
		// arms runs every arm on one engine and returns its labelled outcomes,
		// with the engine's prefix replaced by P.
		arms := func(e engine) []string {
			p := e.prefix
			// Java's keyspace takes a database path of two levels (/domain/name).
			dbPath, t, t2 := "/TEST/"+p+"_D", p+"_T", p+"_T2"
			var out []string
			add := func(label string, got ...string) {
				for i, g := range got {
					out = append(out, fmt.Sprintf("%s[%d]: %s", label, i, strings.ReplaceAll(g, p, "P")))
				}
			}
			defer e.ddl("DROP DATABASE IF EXISTS "+dbPath, "DROP SCHEMA TEMPLATE IF EXISTS "+t, "DROP SCHEMA TEMPLATE IF EXISTS "+t2)

			add("setup", e.ddl(
				"CREATE DATABASE "+dbPath,
				"CREATE SCHEMA TEMPLATE "+t+" CREATE TABLE X(ID BIGINT, PRIMARY KEY(ID))",
				"CREATE SCHEMA "+dbPath+"/S WITH TEMPLATE "+t)...)
			add("CREATE SCHEMA precedence", e.ddl(
				// a missing database and a missing template
				"CREATE SCHEMA /TEST/"+p+"_NODB/S WITH TEMPLATE "+p+"_NOTMPL",
				// a missing template over an existing schema
				"CREATE SCHEMA "+dbPath+"/S WITH TEMPLATE "+p+"_NOTMPL",
				// a duplicate
				"CREATE SCHEMA "+dbPath+"/S WITH TEMPLATE "+t)...)

			e.bareStore(dbPath, "S2", t)
			add("a store under no catalog row", e.ddl("CREATE SCHEMA "+dbPath+"/S2 WITH TEMPLATE "+t)...)

			e.copyVersion(t, 1, 2)
			add("bind S3 at version 2", e.saveSchema(dbPath, "S3", t, 2, "ERROR"))
			for _, b := range order {
				add("a lower version, "+b, e.saveSchema(dbPath, "S3", t, 1, b))
			}
			for _, b := range order {
				add("the same version, "+b, e.saveSchema(dbPath, "S3", t, 2, b))
			}
			add("a template", e.ddl("CREATE SCHEMA TEMPLATE "+t2+" CREATE TABLE Y(ID BIGINT, PRIMARY KEY(ID))")...)
			for _, b := range order {
				add("a different template, "+b, e.saveSchema(dbPath, "S3", t2, 1, b))
			}
			add("a higher version, UPGRADE", e.saveSchema(dbPath, "S", t, 2, "UPGRADE"))
			add("a higher version, ERROR_IF_DIFFERENT", e.saveSchema(dbPath, "S", t, 1, "ERROR_IF_DIFFERENT"))

			add("drop the bound template", e.ddl("DROP SCHEMA TEMPLATE "+t)...)
			add("CREATE SCHEMA over a gone version", e.ddl("CREATE SCHEMA "+dbPath+"/S WITH TEMPLATE "+t2)...)
			for _, b := range order {
				add("a save over a gone version, "+b, e.saveSchema(dbPath, "S3", t2, 1, b))
			}
			return out
		}

		javaOut := arms(javaEngine)
		goOut := arms(goEngine)
		for i := range javaOut {
			GinkgoWriter.Printf("WSJEXIST java %s\n", javaOut[i])
			if i < len(goOut) {
				GinkgoWriter.Printf("WSJEXIST go   %s\n", goOut[i])
			}
		}
		// The population: 3 setup, 3 precedence, 1 store, 1 bind, 3 x 4 saves, 1
		// template, 2 higher, 1 drop, 1 create, 4 gone saves.
		Expect(javaOut).To(HaveLen(29))
		for _, o := range javaOut[:3] {
			Expect(o).To(HaveSuffix(": OK"), "the target's setup")
		}
		Expect(goOut).To(Equal(javaOut))
	})
})
