package sqltest

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
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
	"fdb.dev/pkg/relational/sqltest/testkit"
	"google.golang.org/protobuf/proto"
)

const sqlOpaqueCandidacyType = "sqltest_opaque_candidacy_v1"

func init() {
	recordlayer.RegisterIndexMaintainerFactory(sqlOpaqueCandidacyFactory{})
}

type sqlOpaqueCandidacyFactory struct{}

func (sqlOpaqueCandidacyFactory) IndexTypes() []string                          { return []string{sqlOpaqueCandidacyType} }
func (sqlOpaqueCandidacyFactory) IsIdempotent(*recordlayer.Index) bool          { return true }
func (sqlOpaqueCandidacyFactory) ValidateIndexOptions(*recordlayer.Index) error { return nil }
func (sqlOpaqueCandidacyFactory) ValidateChangedOptions(_, _ *recordlayer.Index, _ map[string]bool) error {
	return nil
}

func (sqlOpaqueCandidacyFactory) NewIndexMaintainer(state recordlayer.IndexMaintainerState) (recordlayer.IndexMaintainer, error) {
	return &sqlOpaqueCandidacyMaintainer{StandardIndexMaintainer: *recordlayer.NewStandardIndexMaintainer(state)}, nil
}

type sqlOpaqueCandidacyMaintainer struct {
	recordlayer.StandardIndexMaintainer
}

// Each evaluated key has a hashed posting, not a VALUE (key, primary-key)
// tuple. The 0x7e discriminator is deliberately not a tuple type code.
func (m *sqlOpaqueCandidacyMaintainer) Update(oldRecord, newRecord *recordlayer.FDBStoredRecord[proto.Message]) error {
	for _, image := range []struct {
		record *recordlayer.FDBStoredRecord[proto.Message]
		remove bool
	}{{oldRecord, true}, {newRecord, false}} {
		entries, err := m.FilteredIndexEntries(image.record)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			digest := sha256.Sum256(tuple.Tuple{entry.Key(), image.record.PrimaryKey}.Pack())
			key := append(bytes.Clone(m.IndexSubspace().Bytes()), 0x7e)
			key = append(key, digest[:]...)
			if image.remove {
				m.Transaction().Clear(fdb.Key(key))
			} else {
				m.Transaction().Set(fdb.Key(key), []byte("opaque-posting-v1"))
			}
		}
	}
	return nil
}

func (m *sqlOpaqueCandidacyMaintainer) UpdateWhileWriteOnly(oldRecord, newRecord *recordlayer.FDBStoredRecord[proto.Message]) error {
	return m.Update(oldRecord, newRecord)
}

func sqlCandidacyTemplate(t *testing.T, name, indexType string) *metadata.RecordLayerSchemaTemplate {
	t.Helper()
	b := metadata.NewSchemaTemplateBuilder().SetName(name)
	b.AddTable("T", []metadata.ColumnSpec{
		metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
		metadata.NewColumnSpec("TAGS", api.NewArrayType(api.NewLongType(false), true), 2),
		metadata.NewColumnSpec("A", api.NewLongType(true), 3),
		metadata.NewColumnSpec("B", api.NewLongType(true), 4),
	}, []string{"ID"})
	b.AddFanOutIndex("T", "T_TAGS", "TAGS")
	b.AddIndex("T", "T_A", []string{"A"}, false)
	b.AddIndex("T", "T_B", []string{"B"}, false)
	tmpl, err := b.Build()
	if err != nil {
		t.Fatalf("build %s: %v", name, err)
	}
	for _, name := range []string{"T_TAGS", "T_B"} {
		tmpl.Underlying().GetIndex(name).Type = indexType
	}
	return tmpl
}

// Read back the catalog's metadata and the physical entries, so fallback cannot
// pass merely because the custom indexes were absent, empty, or non-readable.
func sqlCandidacyCheckEntries(t *testing.T, ctx context.Context, h *testkit.FleetHarness, dbPath, schema, template, indexType string) {
	t.Helper()
	var md *recordlayer.RecordMetaData
	h.MustRun(t, "load candidacy template", func(txn api.Transaction) error {
		stored, err := h.Cat.SchemaTemplateCatalog().LoadSchemaTemplate(txn, template)
		if err != nil {
			return err
		}
		md = stored.(*metadata.RecordLayerSchemaTemplate).Underlying()
		return nil
	})
	ss, err := h.Ks.LookupSchemaSubspace(ctx, h.DB, dbPath, schema)
	if err != nil {
		t.Fatalf("lookup %s: %v", schema, err)
	}
	_, err = h.DB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetSubspace(ss).SetMetaDataProvider(md).Open()
		if err != nil {
			return nil, err
		}
		for _, spec := range []struct {
			name string
			rows int
		}{{"T_TAGS", 6}, {"T_B", 5}, {"T_A", 5}} {
			idx := md.GetIndex(spec.name)
			wantType := indexType
			if spec.name == "T_A" {
				wantType = recordlayer.IndexTypeValue
			}
			if idx == nil || idx.Type != wantType {
				return nil, fmt.Errorf("%s/%s: index %v, want type %s", schema, spec.name, idx, wantType)
			}
			if state := store.GetIndexState(spec.name); state != recordlayer.IndexStateReadable {
				return nil, fmt.Errorf("%s/%s: state %v, want READABLE", schema, spec.name, state)
			}
			indexSubspace := store.IndexSubspace(idx)
			kvs, err := rc.Transaction().GetRange(indexSubspace, fdb.RangeOptions{}).GetSliceWithError()
			if err != nil {
				return nil, err
			}
			if len(kvs) != spec.rows {
				return nil, fmt.Errorf("%s/%s: %d entries, want %d", schema, spec.name, len(kvs), spec.rows)
			}
			for _, kv := range kvs {
				suffix := kv.Key[len(indexSubspace.Bytes()):]
				_, unpackErr := tuple.Unpack(suffix)
				if wantType == sqlOpaqueCandidacyType {
					if len(suffix) != 1+sha256.Size || suffix[0] != 0x7e || unpackErr == nil || !bytes.Equal(kv.Value, []byte("opaque-posting-v1")) {
						return nil, fmt.Errorf("%s/%s: not an opaque hashed posting: key=%x value=%x", schema, spec.name, suffix, kv.Value)
					}
				} else if unpackErr != nil {
					return nil, fmt.Errorf("%s/%s: invalid VALUE tuple: %w", schema, spec.name, unpackErr)
				}
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("verify populated %s indexes: %v", schema, err)
	}
}

func sqlCandidacyRows(t *testing.T, ctx context.Context, db *sql.DB, query string, ordered bool) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		values := make([]int64, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatalf("%s: scan: %v", query, err)
		}
		parts := make([]string, len(values))
		for i, value := range values {
			parts[i] = fmt.Sprint(value)
		}
		got = append(got, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: rows: %v", query, err)
	}
	if !ordered {
		sort.Strings(got)
	}
	return got
}

func TestFDB_CustomIndexCandidacyFallsBackWithoutReadingOpaquePostings(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatal(err)
	}
	name := "CUSTOM_CANDIDACY_" + strings.ToUpper(hex.EncodeToString(suffix[:]))
	dbPath := "/FRL/" + name
	fdb.MustAPIVersion(730)
	raw, err := fdb.OpenDatabase(testkit.ClusterFile())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(raw.Close)
	ks := keyspace.New(subspace.Sub())
	cat, err := catalog.NewRecordLayerStoreCatalog(ks.CatalogSubspace())
	if err != nil {
		t.Fatal(err)
	}
	h := &testkit.FleetHarness{DB: recordlayer.NewFDBDatabase(raw), Cat: cat, Ks: ks}
	fixtures := []struct {
		schema, indexType string
		template          *metadata.RecordLayerSchemaTemplate
	}{{schema: "OPAQUE", indexType: sqlOpaqueCandidacyType}, {schema: "VALUE", indexType: recordlayer.IndexTypeValue}}
	for i := range fixtures {
		fixtures[i].template = sqlCandidacyTemplate(t, name+"_"+fixtures[i].schema, fixtures[i].indexType)
	}
	h.MustRun(t, "create candidacy twins", func(txn api.Transaction) error {
		if err := cat.Initialize(txn); err != nil {
			return err
		}
		if err := ddl.NewCreateDatabaseConstantAction(dbPath, cat).Execute(txn); err != nil {
			return err
		}
		for _, fixture := range fixtures {
			if err := ddl.NewSaveSchemaTemplateConstantAction(fixture.template, cat.SchemaTemplateCatalog()).Execute(txn); err != nil {
				return err
			}
			if err := ddl.NewCreateSchemaConstantAction(dbPath, fixture.schema, fixture.template.MetadataName(), cat, ks).Execute(txn); err != nil {
				return err
			}
		}
		return nil
	})
	for _, fixture := range fixtures {
		db := testkit.FleetOpen(t, dbPath, fixture.schema)
		testkit.MustExecCtx(t, db, ctx, "INSERT INTO T (ID, TAGS, A, B) VALUES "+
			"(1, [9,9,4], 3, 30), (2, [8], 7, 3), (3, [9], 3, 20), (4, [], 8, 10), (5, [4,9], 9, -5)")
		sqlCandidacyCheckEntries(t, ctx, h, dbPath, fixture.schema, fixture.template.MetadataName(), fixture.indexType)
		for _, tc := range []struct {
			name, query, index, fallback string
			want                         []string
			ordered                      bool
		}{
			{"equality", "SELECT R.ID FROM T AS R WHERE R.B = 3", "T_B", "Scan(T)", []string{"2"}, false},
			{"fanout", "SELECT R.ID FROM T AS R WHERE EXISTS (SELECT E FROM R.TAGS AS E WHERE E = 9)", "T_TAGS", "Explode", []string{"1", "3", "5"}, false},
			{"ordering", "SELECT R.ID, R.B FROM T AS R ORDER BY R.B", "T_B", "InMemorySort", []string{"5|-5", "2|3", "4|10", "3|20", "1|30"}, true},
			{"value_control", "SELECT R.ID FROM T AS R WHERE R.A = 3", "T_A", "", []string{"1", "3"}, false},
		} {
			var explain string
			if err := db.QueryRowContext(ctx, "EXPLAIN "+tc.query).Scan(&explain); err != nil {
				t.Fatalf("%s/%s EXPLAIN: %v", fixture.schema, tc.name, err)
			}
			if fixture.indexType == sqlOpaqueCandidacyType && tc.name != "value_control" {
				if strings.Contains(explain, "T_TAGS") || strings.Contains(explain, "T_B") || !strings.Contains(explain, tc.fallback) {
					t.Fatalf("%s/%s: want %s fallback without opaque indexes: %s", fixture.schema, tc.name, tc.fallback, explain)
				}
			} else if !strings.Contains(explain, tc.index) || !strings.Contains(explain, "IndexScan") {
				t.Fatalf("%s/%s: VALUE positive control must execute %s: %s", fixture.schema, tc.name, tc.index, explain)
			}
			if fixture.indexType == recordlayer.IndexTypeValue && tc.ordered && strings.Contains(explain, "InMemorySort") {
				t.Fatalf("VALUE ordering control did not use index order: %s", explain)
			}
			if got := sqlCandidacyRows(t, ctx, db, tc.query, tc.ordered); !slices.Equal(got, tc.want) {
				t.Fatalf("%s/%s rows = %v, want %v; plan: %s", fixture.schema, tc.name, got, tc.want, explain)
			}
		}
	}
}
