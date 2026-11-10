package sqltest

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/ddl"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/metadata"
	"fdb.dev/pkg/relational/sqltest/testkit"
)

func partitionUpdateTemplate(t *testing.T, name string, sourceFirst bool) *metadata.RecordLayerSchemaTemplate {
	t.Helper()
	names := []string{"ID", "SOURCE"}
	if sourceFirst {
		names[0], names[1] = names[1], names[0]
	}
	builder := metadata.NewSchemaTemplateBuilder().SetName(name)
	for _, table := range []string{"DOCS", "PLAIN"} {
		builder.AddTable(table, []metadata.ColumnSpec{
			metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
			metadata.NewColumnSpec("SOURCE", api.NewLongType(false), 2),
			metadata.NewColumnSpec("V", api.NewLongType(false), 3),
			metadata.NewColumnSpec("BODY", api.NewStringType(true), 4),
		}, names)
	}
	base, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	union := base.Underlying().GetUnionDescriptor()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecordsWithUnionName(union.ParentFile(), string(union.Name())).SetVersion(1).SetSplitLongRecords(true).SetStoreRecordVersions(true)
	for _, table := range []string{"DOCS", "PLAIN"} {
		rt := base.Underlying().GetRecordType(table)
		b.GetRecordType(table).SetPrimaryKey(rt.PrimaryKey).SetRecordTypeKey(rt.GetRecordTypeKey())
		if table == "PLAIN" {
			continue
		}
		p := recordlayer.PartitionExpr(recordlayer.Field("SOURCE"), 10)
		key := []recordlayer.KeyExpression{recordlayer.RecordTypeKey(), p}
		for _, name := range names {
			key = append(key, recordlayer.Field(name))
		}
		b.GetRecordType(table).SetPrimaryKey(recordlayer.Concat(key...))
		b.AddIndex(table, recordlayer.NewIndex("UPDATE_VALUE", recordlayer.Concat(p, recordlayer.Field("V"))))
		b.AddIndex(table, recordlayer.NewIndex("UPDATE_COVER", recordlayer.KeyWithValue(recordlayer.Concat(p, recordlayer.Field("ID"), recordlayer.Field("BODY")), 2)))
		b.AddIndex(table, recordlayer.NewTextIndex("UPDATE_TEXT", recordlayer.GroupBy(recordlayer.Field("BODY"), p, recordlayer.Field("ID"))))
		b.AddIndex(table, recordlayer.NewIndex("UPDATE_UNIQUE", recordlayer.Concat(p, recordlayer.Field("SOURCE"), recordlayer.Field("V"))).SetUnique())
	}
	md, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	template, err := metadata.NewRecordLayerSchemaTemplateWithVersion(name, md, 1)
	if err != nil {
		t.Fatal(err)
	}
	return template
}

func TestFDB_PartitionAtomicUpdates(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	for _, sourceFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "ID_SOURCE", true: "SOURCE_ID"}[sourceFirst], func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			h := testkit.NewFleetHarness(t)
			var suffix [8]byte
			if _, err := rand.Read(suffix[:]); err != nil {
				t.Fatal(err)
			}
			name := "PARTITION_UPDATES_" + strings.ToUpper(hex.EncodeToString(suffix[:]))
			dbPath := "/FRL/" + name
			template := partitionUpdateTemplate(t, name, sourceFirst)
			h.MustRun(t, "persist partition update fixture", func(tx api.Transaction) error {
				if err := h.Cat.Initialize(tx); err != nil {
					return err
				}
				if err := ddl.NewCreateDatabaseConstantAction(dbPath, h.Cat).Execute(tx); err != nil {
					return err
				}
				if err := ddl.NewSaveSchemaTemplateConstantAction(template, h.Cat.SchemaTemplateCatalog()).Execute(tx); err != nil {
					return err
				}
				return ddl.NewCreateSchemaConstantAction(dbPath, "S", name, h.Cat, h.Ks).Execute(tx)
			})
			db := testkit.FleetOpen(t, dbPath, "S")
			conn := testkit.PinEmbeddedConn(t, db, func(*embedded.EmbeddedConnection) {})
			seed := func() {
				t.Helper()
				if _, err := conn.ExecContext(ctx, "DELETE FROM DOCS"); err != nil {
					t.Fatal(err)
				}
				if _, err := conn.ExecContext(ctx, "INSERT INTO DOCS VALUES (1,-11,10,'red'),(1,0,20,'blue'),(2,10,30,'green'),(3,-1,40,'purple'),(4,21,50,'white'),(5,32,60,'black')"); err != nil {
					t.Fatal(err)
				}
			}
			wantRows := func(query string, expected []string, args ...any) {
				t.Helper()
				got := partitionBridgeQueryRows(t, ctx, conn, query, args...)
				sort.Strings(got)
				sort.Strings(expected)
				if !reflect.DeepEqual(got, expected) {
					t.Fatalf("%s: got %v want %v", query, got, expected)
				}
			}
			seed()
			result, err := conn.ExecContext(ctx, "UPDATE DOCS SET V=V+1 WHERE ID=4")
			if err != nil {
				t.Fatal(err)
			}
			if n, err := result.RowsAffected(); err != nil || n != 1 {
				t.Fatalf("same-key count=%d err=%v", n, err)
			}
			wantRows(`UPDATE DOCS SET SOURCE=-12,V=V+1 WHERE ID=1 AND SOURCE=-11 RETURNING "old".SOURCE,"new".SOURCE,"old".V,"new".V`, []string{"-11|-12|10|11"})
			wantRows(`UPDATE DOCS SET SOURCE=10,V=V+1 WHERE ID=1 AND SOURCE=0 RETURNING "new".ID,"new".SOURCE,"new".V`, []string{"1|10|21"})
			wantRows(`UPDATE DOCS SET SOURCE=-1,V=V+1 WHERE ID=2 RETURNING "old".SOURCE,"new".SOURCE`, []string{"10|-1"})
			wantRows(`UPDATE DOCS SET SOURCE=0,V=V+1 WHERE ID=3 RETURNING "old".SOURCE,"new".SOURCE`, []string{"-1|0"})
			prepared, err := conn.PrepareContext(ctx, "UPDATE DOCS SET SOURCE=?,V=V+1 WHERE ID=?")
			if err != nil {
				t.Fatal(err)
			}
			result, err = prepared.ExecContext(ctx, int64(100), int64(5))
			prepared.Close()
			if err != nil {
				t.Fatal(err)
			}
			if n, _ := result.RowsAffected(); n != 1 {
				t.Fatalf("prepared move count=%d", n)
			}
			// Both self-table witnesses match each selected OLD row. EXISTS is
			// supported; an IN-subquery is intentionally not a supported shape.
			wantRows("SELECT ID,SOURCE FROM DOCS D WHERE D.ID=1", []string{"1|-12", "1|10"})
			wantRows(`UPDATE DOCS SET SOURCE=SOURCE+1000,V=V+1 WHERE EXISTS (SELECT 1 FROM DOCS D WHERE D.ID=DOCS.ID AND D.ID=1) RETURNING "old".SOURCE,"new".SOURCE,"old".V,"new".V`, []string{"-12|988|11|12", "10|1010|21|22"})
			wantRows("SELECT ID,SOURCE,V FROM DOCS", []string{"1|988|12", "1|1010|22", "2|-1|31", "3|0|41", "4|21|51", "5|100|61"})
			// Independently name actual physical NEW keys and committed versions.
			ss, err := h.Ks.LookupSchemaSubspace(ctx, h.DB, dbPath, "S")
			if err != nil {
				t.Fatal(err)
			}
			_, err = h.DB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetSubspace(ss).SetMetaDataProvider(template.Underlying()).Open()
				if err != nil {
					return nil, err
				}
				rt := template.Underlying().GetRecordType("DOCS")
				key := func(id, source, p int64) tuple.Tuple {
					if sourceFirst {
						return tuple.Tuple{rt.GetRecordTypeKey(), p, source, id}
					}
					return tuple.Tuple{rt.GetRecordTypeKey(), p, id, source}
				}
				for _, row := range []struct{ id, source, p, v int64 }{{1, 988, 98, 12}, {1, 1010, 101, 22}, {2, -1, -1, 31}, {3, 0, 0, 41}, {4, 21, 2, 51}, {5, 100, 10, 61}} {
					stored, err := store.LoadRecord(key(row.id, row.source, row.p))
					if err != nil || stored == nil {
						return nil, fmt.Errorf("NEW key missing: %+v: %v", row, err)
					}
					if stored.Record.ProtoReflect().Get(rt.Descriptor.Fields().ByName("V")).Int() != row.v || stored.Version == nil || !stored.Version.IsComplete() {
						return nil, fmt.Errorf("wrong NEW row/version: %+v", row)
					}
				}
				for _, old := range []tuple.Tuple{key(1, -11, -2), key(1, -12, -2), key(1, 0, 0), key(1, 10, 1), key(2, 10, 1), key(3, -1, -1), key(5, 32, 3)} {
					stored, err := store.LoadRecord(old)
					if err != nil || stored != nil {
						return nil, fmt.Errorf("OLD key survived: %v: %v", old, err)
					}
				}
				for _, name := range []string{"UPDATE_VALUE", "UPDATE_COVER", "UPDATE_UNIQUE"} {
					index := template.Underlying().GetIndex(name)
					entries, err := recordlayer.AsList(ctx, store.ScanIndex(index, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
					if err != nil || len(entries) != 6 {
						return nil, fmt.Errorf("%s entries=%d: %v", name, len(entries), err)
					}
					for _, entry := range entries {
						stored, err := store.LoadRecord(index.EntryPrimaryKey(entry.Key))
						if err != nil || stored == nil {
							return nil, fmt.Errorf("%s points to stale PK: %v", name, err)
						}
					}
				}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			wantRows("SELECT ID,SOURCE FROM DOCS WHERE TEXT_CONTAINS_ALL(BODY,'red')", []string{"1|988"})

			for _, tc := range []struct {
				name, sql string
				extra     bool
			}{
				{"selected-to-vacate", "UPDATE DOCS SET SOURCE=SOURCE+11 WHERE ID=1", false},
				{"swap", "UPDATE DOCS SET SOURCE=-11-SOURCE WHERE ID=1", false},
				{"cycle", "UPDATE DOCS SET SOURCE=CASE SOURCE WHEN -11 THEN 0 WHEN 0 THEN 10 ELSE -11 END WHERE ID=1", true},
				{"duplicate-final", "UPDATE DOCS SET SOURCE=20 WHERE ID=1", false},
				{"unselected-occupied", "UPDATE DOCS SET ID=2,SOURCE=10 WHERE ID=1 AND SOURCE=-11", false},
				{"NULL", "UPDATE DOCS SET SOURCE=NULL WHERE ID=1", false},
				{"transform", "UPDATE DOCS SET SOURCE=SOURCE/0 WHERE ID=1", false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					seed()
					if tc.extra {
						if _, err := conn.ExecContext(ctx, "INSERT INTO DOCS VALUES (1,10,90,'cycle')"); err != nil {
							t.Fatal(err)
						}
					}
					before := partitionBridgeSnapshot(t, ctx, h, dbPath, "S")
					tx, err := conn.BeginTx(ctx, nil)
					if err != nil {
						t.Fatal(err)
					}
					_, executionErr := tx.ExecContext(ctx, tc.sql)
					if executionErr == nil {
						tx.Rollback()
						t.Fatalf("%s unexpectedly succeeded", tc.sql)
					}
					code := api.ErrCodeUniqueConstraintViolation
					switch tc.name {
					case "NULL":
						// Descriptor scalars are OPTIONAL: NULL reaches partition
						// routing, whose typed error has no SQLSTATE mapping.
						var routing *recordlayer.PartitionRoutingError
						if !errors.As(executionErr, &routing) || routing.Value != nil || routing.Width != 10 || routing.Reason != "source must be a non-NULL BIGINT" {
							tx.Rollback()
							t.Fatalf("want NULL partition source routing refusal, got %T: %v", executionErr, executionErr)
						}
						code = ""
					case "transform":
						code = api.ErrCodeDivisionByZero
					default:
						var collision *recordlayer.RecordAlreadyExistsError
						if !errors.As(executionErr, &collision) {
							tx.Rollback()
							t.Fatalf("want actual physical destination collision, got %T: %v", executionErr, executionErr)
						}
					}
					if got := apiCode(executionErr); got != code {
						tx.Rollback()
						t.Fatalf("%s SQLSTATE=%s want %s: %v", tc.name, got, code, executionErr)
					}
					// Deliberate Commit, not a test-side rollback, proves refusal
					// cannot publish a partial UPDATE regardless of driver policy.
					_ = tx.Commit()
					if got := partitionBridgeSnapshot(t, ctx, h, dbPath, "S"); !reflect.DeepEqual(got, before) {
						t.Fatal("failed UPDATE changed entire committed store")
					}
				})
			}

			for _, limit := range []string{"scan", "memory", "deadline"} {
				t.Run(limit+" pre-effect refusal", func(t *testing.T) {
					seed()
					before := partitionBridgeSnapshot(t, ctx, h, dbPath, "S")
					if err := conn.Raw(func(raw any) error {
						ec := raw.(*embedded.EmbeddedConnection)
						switch limit {
						case "scan":
							ec.SetFailOnScanLimitReached(true)
							return ec.SetOption(api.OptExecutionScannedRowsLimit, int64(1))
						case "memory":
							return ec.SetOption(api.OptMaxStatementMemoryBytes, int64(1))
						case "deadline":
							ec.SetStatementTimeout(time.Nanosecond)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					tx, err := conn.BeginTx(ctx, nil)
					if err != nil {
						t.Fatal(err)
					}
					_, err = tx.ExecContext(ctx, "UPDATE DOCS SET SOURCE=SOURCE+1000,V=V+1")
					if apiCode(err) != api.ErrCodeExecutionLimitReached {
						tx.Rollback()
						t.Fatalf("%s bound did not return a resource refusal: %v", limit, err)
					}
					_ = tx.Commit()
					if err := conn.Raw(func(raw any) error {
						ec := raw.(*embedded.EmbeddedConnection)
						ec.SetFailOnScanLimitReached(false)
						ec.SetStatementTimeout(0)
						if err := ec.SetOption(api.OptExecutionScannedRowsLimit, int64(0)); err != nil {
							return err
						}
						return ec.SetOption(api.OptMaxStatementMemoryBytes, int64(0))
					}); err != nil {
						t.Fatal(err)
					}
					if got := partitionBridgeSnapshot(t, ctx, h, dbPath, "S"); !reflect.DeepEqual(got, before) {
						t.Fatal("resource refusal changed entire committed store")
					}
				})
			}

			t.Run("later UNIQUE failure makes explicit Commit rollback", func(t *testing.T) {
				seed()
				if _, err := conn.ExecContext(ctx, "INSERT INTO DOCS VALUES (99,100,99,'neighbor')"); err != nil {
					t.Fatal(err)
				}
				before := partitionBridgeSnapshot(t, ctx, h, dbPath, "S")
				tx, err := conn.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				_, err = tx.ExecContext(ctx, "UPDATE DOCS SET SOURCE=SOURCE+100,V=99 WHERE ID=1")
				var unique *recordlayer.RecordIndexUniquenessViolationError
				if !errors.As(err, &unique) || unique.IndexName != "UPDATE_UNIQUE" || apiCode(err) != api.ErrCodeUniqueConstraintViolation {
					tx.Rollback()
					t.Fatalf("want later UNIQUE refusal: %v", err)
				}
				if err := tx.Commit(); err == nil {
					t.Fatal("partial partition UPDATE remained committable")
				}
				if got := partitionBridgeSnapshot(t, ctx, h, dbPath, "S"); !reflect.DeepEqual(got, before) {
					t.Fatal("later failure published partial move or activation")
				}
			})

			// Existing ordinary behavior is not broadened by the bridge. The
			// original update_primary_key_probe_test.go remains untouched.
			if _, err := conn.ExecContext(ctx, "INSERT INTO PLAIN VALUES (1,0,1,'plain')"); err != nil {
				t.Fatal(err)
			}
			_, err = conn.ExecContext(ctx, "UPDATE PLAIN SET ID=2 WHERE ID=1")
			var absent *recordlayer.RecordDoesNotExistError
			if !errors.As(err, &absent) || apiCode(err) != api.ErrCodeUnknown {
				t.Fatalf("ordinary PK UPDATE changed behavior: %v", err)
			}
			wantRows("SELECT ID,SOURCE,V FROM PLAIN", []string{"1|0|1"})
		})
	}
}
