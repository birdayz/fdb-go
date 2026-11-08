//go:build bazelrunfiles

package conformance_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/ddl"
	"fdb.dev/pkg/relational/core/keyspace"
	"fdb.dev/pkg/relational/core/metadata"
	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// The no-index case makes the required primary-key function, rather than an
// unsupported index or tokenizer, the schema-wide compatibility boundary.
var _ = Describe("PartitionRefusalJavaProbe", func() {
	for _, indexed := range []bool{false, true} {
		It(fmt.Sprintf("refuses the whole mixed schema without writes (local indexes=%t)", indexed), func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			java := NewJavaInvoker()
			clusterFile, err := sharedContainer.ClusterFile(ctx)
			Expect(err).NotTo(HaveOccurred())
			clusterPath, err := sharedContainer.ClusterFilePath(ctx)
			Expect(err).NotTo(HaveOccurred())
			oldRunfile := os.Getenv("PARTITION_OLD_GO_BINARY")
			Expect(oldRunfile).NotTo(BeEmpty(), "the source-pinned old Go binary must be a required test input")
			rf, err := runfiles.New()
			Expect(err).NotTo(HaveOccurred())
			oldBinary, err := rf.Rlocation(oldRunfile)
			Expect(err).NotTo(HaveOccurred())
			db := recordlayer.NewFDBDatabase(sharedDB)
			ks := keyspace.New(subspace.Sub())
			cat, err := catalog.NewRecordLayerStoreCatalog(ks.CatalogSubspace())
			Expect(err).NotTo(HaveOccurred())
			ops := ddl.NewRecordLayerMetadataOperationsFactoryWithKeyspace(cat, ks)
			run := func(fn func(api.Transaction) error) {
				_, err := db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
					txn := catalog.NewFDBTransaction(rc)
					if err := cat.Initialize(txn); err != nil {
						return nil, err
					}
					return nil, fn(txn)
				})
				Expect(err).NotTo(HaveOccurred())
			}
			// Run the same reads and inserts successfully on ordinary metadata
			// first; a transport, catalog or SQL failure is not a refusal proof.
			for _, partitioned := range []bool{false, true} {
				name := "PARTITION_GUARD_" + strings.ToUpper(strings.ReplaceAll(uuid.NewString(), "-", ""))
				path := "/FRL/" + name
				tmpl := partitionProbeTemplate(name, partitioned, indexed)
				md := tmpl.Underlying()
				mdSS := subspace.Sub(name + "_METADATA")
				run(func(txn api.Transaction) error {
					if err := ops.SaveSchemaTemplate(tmpl, api.Options{}).Execute(txn); err != nil {
						return err
					}
					if err := ops.CreateDatabase(path, api.Options{}).Execute(txn); err != nil {
						return err
					}
					return ops.CreateSchema(path, "S", name, api.Options{}).Execute(txn)
				})
				DeferCleanup(func() {
					_, err := db.Run(context.Background(), func(rc *recordlayer.FDBRecordContext) (any, error) {
						txn := catalog.NewFDBTransaction(rc)
						if err := ops.DropDatabase(path, false, api.Options{}).Execute(txn); err != nil {
							return nil, err
						}
						begin, end := mdSS.FDBRangeKeys()
						rc.Transaction().ClearRange(fdb.KeyRange{Begin: begin, End: end})
						return nil, ops.DropSchemaTemplate(name, false, api.Options{}).Execute(txn)
					})
					Expect(err).NotTo(HaveOccurred())
				})
				storeSS, err := ks.SchemaSubspaceIn(ctx, db, path, "S")
				Expect(err).NotTo(HaveOccurred())
				mdProto, err := md.ToProto()
				Expect(err).NotTo(HaveOccurred())
				_, err = db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
					if err := recordlayer.NewFDBMetaDataStore(mdSS).SaveRecordMetaData(rc.Transaction(), mdProto); err != nil {
						return nil, err
					}
					store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetSubspace(storeSS).SetMetaDataProvider(md).Open()
					if err != nil {
						return nil, err
					}
					for _, table := range []string{"DOCS", "PLAIN"} {
						msg := dynamicpb.NewMessage(md.GetRecordType(table).Descriptor)
						msg.Set(msg.Descriptor().Fields().ByName("ID"), protoreflect.ValueOfInt64(1))
						msg.Set(msg.Descriptor().Fields().ByName("BODY"), protoreflect.ValueOfString(table+" seed"))
						if table == "DOCS" {
							msg.Set(msg.Descriptor().Fields().ByName("SOURCE"), protoreflect.ValueOfInt64(-1))
						}
						if _, err := store.SaveRecord(msg); err != nil {
							return nil, err
						}
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())
				// Current Go must load the persisted metadata and read both physical
				// keys. No function unregistration stands in for an older binary.
				_, err = db.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
					p, err := recordlayer.NewFDBMetaDataStore(mdSS).LoadRecordMetaDataProto(rc.Transaction())
					if err != nil {
						return nil, err
					}
					loaded, err := recordlayer.RecordMetaDataFromProto(p)
					if err != nil {
						return nil, err
					}
					store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetSubspace(storeSS).SetMetaDataProvider(loaded).Open()
					if err != nil {
						return nil, err
					}
					docKey := tuple.Tuple{int64(0), int64(-1), int64(1)}
					if partitioned {
						docKey = tuple.Tuple{int64(0), int64(-1), int64(-1), int64(1)}
					}
					for _, pk := range []tuple.Tuple{docKey, {int64(1), int64(1)}} {
						row, err := store.LoadRecord(pk)
						if err != nil {
							return nil, err
						}
						Expect(row).NotTo(BeNil(), "current Go must read %v", pk)
					}
					return nil, nil
				})
				Expect(err).NotTo(HaveOccurred())

				before := partitionProbeSnapshot(ctx)
				audit := func(attempt string) {
					Expect(partitionProbeSnapshot(ctx)).To(Equal(before), attempt+" must not change any user key/value, including catalog, metadata, store header and indexes")
					fmt.Fprintf(GinkgoWriter, "%s: no committed changes across %d user keys\n", attempt, len(before))
				}
				var scan struct {
					Found        bool `json:"found"`
					TotalRecords int  `json:"totalRecords"`
				}
				err = java.InvokeAs(ctx, "loadMetaDataAndScanAllRecordsJava", map[string]any{
					"clusterFile": clusterFile, "mdSubspace": BytesToIntArray(mdSS.Bytes()), "storeSubspace": BytesToIntArray(storeSS.Bytes()),
				}, &scan)
				if partitioned {
					partitionProbeJavaRefusal(err)
					audit("Java Record Layer metadata load / store open")
				} else {
					Expect(err).NotTo(HaveOccurred())
					Expect(scan.Found).To(BeTrue())
					Expect(scan.TotalRecords).To(Equal(2))
				}
				for _, table := range []string{"PLAIN", "DOCS"} {
					query := "SELECT body FROM " + table + " WHERE id = 1"
					insert := "INSERT INTO " + table + " (id, body) VALUES (2, 'Java control')"
					oldInsert := "INSERT INTO " + table + " (id, body) VALUES (3, 'old Go control')"
					if table == "DOCS" {
						insert = "INSERT INTO DOCS (source, id, body) VALUES (-1, 2, 'Java control')"
						oldInsert = "INSERT INTO DOCS (source, id, body) VALUES (-1, 3, 'old Go control')"
					}
					var out struct {
						Rows [][]any `json:"rows"`
					}
					err = java.InvokeAs(ctx, "wsjQueryJava", map[string]any{
						"clusterFile": clusterFile, "dbPath": path, "schemaName": "S", "querySql": query,
					}, &out)
					if partitioned {
						partitionProbeJavaRefusal(err)
						audit("Java Relational " + query)
					} else {
						Expect(err).NotTo(HaveOccurred())
						Expect(out.Rows).To(Equal([][]any{{table + " seed"}}))
					}
					var write struct {
						Outcome string `json:"outcome"`
					}
					err = java.InvokeAs(ctx, "wsjExecuteJava", map[string]any{
						"clusterFile": clusterFile, "dbPath": path, "schemaName": "S", "sql": insert,
					}, &write)
					Expect(err).NotTo(HaveOccurred())
					fmt.Fprintf(GinkgoWriter, "partitioned=%t %s: %s\n", partitioned, insert, write.Outcome)
					if partitioned {
						Expect(write.Outcome).To(HavePrefix("ERROR "))
						Expect(write.Outcome).To(HaveSuffix(" InvalidExpressionException Function not defined"))
						audit("Java Relational " + insert)
					} else {
						Expect(write.Outcome).To(Equal("OK 1"))
					}
					for _, sql := range []string{query, oldInsert} {
						stdout, stderr, err := partitionProbeOldGo(ctx, oldBinary, clusterPath, path, sql)
						fmt.Fprintf(GinkgoWriter, "old Go partitioned=%t %s: stdout=%q stderr=%q error=%v\n", partitioned, sql, stdout, stderr, err)
						if partitioned {
							var exit *exec.ExitError
							Expect(errors.As(err, &exit)).To(BeTrue(), "a failed launch or timeout is not a metadata refusal: %v", err)
							Expect(stderr).To(ContainSubstring("Primary key for DOCS can generate more than one entry"))
							audit("old Go " + sql)
						} else {
							Expect(err).NotTo(HaveOccurred(), stderr)
							if sql == query {
								var rows []map[string]any
								Expect(json.Unmarshal([]byte(stdout), &rows)).To(Succeed())
								Expect(rows).To(Equal([]map[string]any{{"BODY": table + " seed"}}))
							}
						}
					}
					if !partitioned {
						err = java.InvokeAs(ctx, "wsjQueryJava", map[string]any{
							"clusterFile": clusterFile, "dbPath": path, "schemaName": "S", "querySql": "SELECT body FROM " + table + " WHERE id IN (2, 3)",
						}, &out)
						Expect(err).NotTo(HaveOccurred())
						Expect(out.Rows).To(ConsistOf([]any{"Java control"}, []any{"old Go control"}))
					}
				}
			}
		})
	}
})

func partitionProbeTemplate(name string, partitioned, indexed bool) *metadata.RecordLayerSchemaTemplate {
	base, err := metadata.NewSchemaTemplateBuilder().SetName(name).
		AddTable("DOCS", []metadata.ColumnSpec{
			metadata.NewColumnSpec("SOURCE", api.NewLongType(true), 1),
			metadata.NewColumnSpec("ID", api.NewLongType(true), 2),
			metadata.NewColumnSpec("BODY", api.NewStringType(true), 3),
		}, []string{"SOURCE", "ID"}).
		AddTable("PLAIN", []metadata.ColumnSpec{
			metadata.NewColumnSpec("ID", api.NewLongType(true), 1),
			metadata.NewColumnSpec("BODY", api.NewStringType(true), 2),
		}, []string{"ID"}).Build()
	Expect(err).NotTo(HaveOccurred())
	union := base.Underlying().GetUnionDescriptor()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecordsWithUnionName(union.ParentFile(), string(union.Name())).SetSplitLongRecords(true).SetVersion(1)
	for _, table := range []string{"DOCS", "PLAIN"} {
		rt := base.Underlying().GetRecordType(table)
		b.GetRecordType(table).SetPrimaryKey(rt.PrimaryKey).SetRecordTypeKey(rt.GetRecordTypeKey())
	}
	part := recordlayer.PartitionExpr(recordlayer.Field("SOURCE"), 10)
	if partitioned {
		b.GetRecordType("DOCS").SetPrimaryKey(recordlayer.Concat(recordlayer.RecordTypeKey(), part, recordlayer.Field("SOURCE"), recordlayer.Field("ID")))
	}
	if indexed {
		var value, text recordlayer.KeyExpression = recordlayer.Field("BODY"), recordlayer.Field("BODY")
		if partitioned {
			value = recordlayer.Concat(part, value)
			text = recordlayer.GroupBy(text, part)
		}
		b.AddIndex("DOCS", recordlayer.NewIndex("DOCS_VALUE", value))
		b.AddIndex("DOCS", recordlayer.NewTextIndex("DOCS_TEXT", text))
	}
	md, err := b.Build()
	Expect(err).NotTo(HaveOccurred())
	tmpl, err := metadata.NewRecordLayerSchemaTemplate(name, md)
	Expect(err).NotTo(HaveOccurred())
	return tmpl
}

func partitionProbeJavaRefusal(err error) {
	var je *JavaError
	Expect(errors.As(err, &je)).To(BeTrue(), "a transport failure is not a metadata refusal: %v", err)
	// FunctionKeyExpression.create/fromProto and RecordMetaDataBuilder.loadProto
	// in 4.14.2.0 throw/wrap this root cause before any table can be used.
	Expect(je.ExceptionFullClass).To(Equal("com.apple.foundationdb.record.metadata.expressions.KeyExpression$InvalidExpressionException"))
	Expect(je.Message).To(Equal("Function not defined"))
	fmt.Fprintf(GinkgoWriter, "Java refusal: SQLSTATE=%q class=%s message=%q\n", je.SQLState, je.ExceptionFullClass, je.Message)
}

func partitionProbeSnapshot(ctx context.Context) []fdb.KeyValue {
	out, err := sharedDB.ReadTransactCtx(ctx, func(tx fdb.ReadTransaction) (any, error) {
		return tx.GetRange(fdb.KeyRange{Begin: fdb.Key(""), End: fdb.Key("\xff")}, fdb.RangeOptions{}).GetSliceWithError()
	})
	Expect(err).NotTo(HaveOccurred())
	kvs := out.([]fdb.KeyValue)
	Expect(kvs).NotTo(BeEmpty(), "the no-write audit must include populated stores and catalog")
	return kvs
}

func partitionProbeOldGo(ctx context.Context, binary, clusterFile, path, sql string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "sql", "--cluster-file", clusterFile, "--database", path, "--schema", "S", "-o", "json", "-c", sql)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}
