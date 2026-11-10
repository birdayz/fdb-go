package sqltest

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/catalog"
	"fdb.dev/pkg/relational/core/ddl"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/core/metadata"
	"fdb.dev/pkg/relational/sqltest/testkit"
	"google.golang.org/protobuf/proto"
)

type partitionBridgeRow struct {
	id, source, p10, p20 int64
	body                 string
}

var partitionBridgeRows = []partitionBridgeRow{
	{1, 21, 2, 1, "red blue"},
	{2, -1, -1, -1, "blue"},
	{1, -11, -2, -1, "red"},
	{3, 0, 0, 0, "green"},
	{4, 9, 0, 0, "red boundary"},
	{5, 10, 1, 0, "blue boundary"},
	{6, -10, -1, -1, "red six"},
	{7, math.MinInt64, -922337203685477581, -461168601842738791, "red minimum"},
	{8, math.MaxInt64, 922337203685477580, 461168601842738790, "blue maximum"},
	{9, 19, 1, 0, "blue nineteen"},
	{10, 20, 2, 1, "red twenty"},
	{11, -20, -2, -1, "blue minus"},
	{12, -21, -3, -2, "green minus"},
}

type partitionBridgeFixture struct {
	schema      string
	template    *metadata.RecordLayerSchemaTemplate
	width       int64
	sourceFirst bool
}

func partitionBridgeTemplate(t *testing.T, name string, catalogVersion int, width int64, sourceFirst bool, proof string) *metadata.RecordLayerSchemaTemplate {
	t.Helper()
	logicalNames := []string{"ID", "SOURCE"}
	if sourceFirst {
		logicalNames[0], logicalNames[1] = logicalNames[1], logicalNames[0]
	}
	builder := metadata.NewSchemaTemplateBuilder().SetName(name)
	for _, table := range []string{"DOCS", "PLAIN"} {
		builder.AddTable(table, []metadata.ColumnSpec{
			metadata.NewColumnSpec("ID", api.NewLongType(false), 1),
			metadata.NewColumnSpec("SOURCE", api.NewLongType(false), 2),
			metadata.NewColumnSpec("BODY", api.NewStringType(true), 3),
		}, logicalNames)
	}
	base, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	union := base.Underlying().GetUnionDescriptor()
	b := recordlayer.NewRecordMetaDataBuilder().SetRecordsWithUnionName(union.ParentFile(), string(union.Name())).SetVersion(1).SetSplitLongRecords(true)
	for _, table := range []string{"DOCS", "PLAIN"} {
		rt := base.Underlying().GetRecordType(table)
		b.GetRecordType(table).SetPrimaryKey(rt.PrimaryKey).SetRecordTypeKey(rt.GetRecordTypeKey())
		var prefix []recordlayer.KeyExpression
		if table == "DOCS" {
			prefix = []recordlayer.KeyExpression{recordlayer.PartitionExpr(recordlayer.Field("SOURCE"), width)}
			pk := []recordlayer.KeyExpression{recordlayer.RecordTypeKey(), prefix[0]}
			for _, field := range logicalNames {
				pk = append(pk, recordlayer.Field(field))
			}
			b.GetRecordType(table).SetPrimaryKey(recordlayer.Concat(pk...))
		}
		var value recordlayer.KeyExpression = recordlayer.Field("BODY")
		if table == "DOCS" && proof == "LITERAL" {
			value = recordlayer.Concat(value, recordlayer.Literal(int64(99)))
		}
		if len(prefix) > 0 {
			value = recordlayer.Concat(prefix[0], value)
		}
		idx := recordlayer.NewIndex(table+"_VALUE", value)
		if table == "DOCS" && proof == "PREDICATE" {
			if err := idx.SetPredicateProto(&gen.Predicate{ValuePredicate: &gen.ValuePredicate{Value: []string{"ID"}, Comparison: &gen.Comparison{SimpleComparison: &gen.SimpleComparison{Type: gen.ComparisonType_GREATER_THAN.Enum(), Operand: &gen.Value{LongValue: proto.Int64(0)}}}}}); err != nil {
				t.Fatal(err)
			}
		}
		b.AddIndex(table, idx)
		cover := append(append([]recordlayer.KeyExpression{}, prefix...), recordlayer.Field("ID"), recordlayer.Field("BODY"))
		b.AddIndex(table, recordlayer.NewIndex(table+"_COVER", recordlayer.KeyWithValue(recordlayer.Concat(cover...), len(prefix)+1)))
		groups := append(append([]recordlayer.KeyExpression{}, prefix...), recordlayer.Field("ID"))
		b.AddIndex(table, recordlayer.NewTextIndex(table+"_TEXT", recordlayer.GroupBy(recordlayer.Field("BODY"), groups...)))
		unique := append(append([]recordlayer.KeyExpression{}, prefix...), recordlayer.Field("SOURCE"), recordlayer.Field("ID"))
		b.AddIndex(table, recordlayer.NewIndex(table+"_UNIQUE", recordlayer.Concat(unique...)).SetUnique())
	}
	md, err := b.Build()
	if err != nil {
		t.Fatal(err)
	}
	tmpl, err := metadata.NewRecordLayerSchemaTemplateWithVersion(name, md, catalogVersion)
	if err != nil {
		t.Fatal(err)
	}
	return tmpl
}

func partitionBridgeKey(fixture partitionBridgeFixture, rt *recordlayer.RecordType, row partitionBridgeRow, partitioned bool) tuple.Tuple {
	key := tuple.Tuple{rt.GetRecordTypeKey()}
	if partitioned {
		p := row.p10
		if fixture.width == 20 {
			p = row.p20
		}
		key = append(key, p)
	}
	if fixture.sourceFirst {
		return append(key, row.source, row.id)
	}
	return append(key, row.id, row.source)
}

func partitionBridgeSnapshot(t *testing.T, ctx context.Context, h *testkit.FleetHarness, dbPath, schema string) []fdb.KeyValue {
	t.Helper()
	ss, err := h.Ks.LookupSchemaSubspace(ctx, h.DB, dbPath, schema)
	if err != nil {
		t.Fatal(err)
	}
	var snapshot []fdb.KeyValue
	_, err = h.DB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		var err error
		snapshot, err = rc.Transaction().GetRange(ss, fdb.RangeOptions{}).GetSliceWithError()
		return nil, err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot) == 0 {
		t.Fatal("empty physical snapshot")
	}
	return snapshot
}

func partitionBridgePhysical(t *testing.T, ctx context.Context, h *testkit.FleetHarness, dbPath string, fixture partitionBridgeFixture, docs []partitionBridgeRow) {
	t.Helper()
	ss, err := h.Ks.LookupSchemaSubspace(ctx, h.DB, dbPath, fixture.schema)
	if err != nil {
		t.Fatal(err)
	}
	serializer, err := recordlayer.NewTransformedRecordSerializerBuilder().SetEncryptWhenSerializing(false).SetCompressWhenSerializing(true).SetCompressionLevel(recordlayer.DeflaterDefaultCompression).SetWriteValidationRatio(0).Build()
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.DB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetSubspace(ss).SetMetaDataProvider(fixture.template.Underlying()).SetSerializer(serializer).Open()
		if err != nil {
			return nil, err
		}
		for _, table := range []string{"DOCS", "PLAIN"} {
			rt := fixture.template.Underlying().GetRecordType(table)
			rows := docs
			if table == "PLAIN" {
				rows = partitionBridgeRows
			}
			stored, err := recordlayer.AsList(ctx, store.ScanRecordsByType(table, nil, recordlayer.ForwardScan()))
			if err != nil || len(stored) != len(rows) {
				return nil, fmt.Errorf("%s physical bodies=%d, want %d: %v", table, len(stored), len(rows), err)
			}
			for _, row := range rows {
				key := partitionBridgeKey(fixture, rt, row, table == "DOCS")
				record, err := store.LoadRecord(key)
				if err != nil || record == nil {
					return nil, fmt.Errorf("%s/%s missing physical key %v: %v", fixture.schema, table, key, err)
				}
				fields := record.Record.ProtoReflect()
				if fields.Get(rt.Descriptor.Fields().ByName("BODY")).String() != row.body || !reflect.DeepEqual(record.PrimaryKey, key) {
					return nil, fmt.Errorf("wrong physical record %v", key)
				}
				body, err := rc.Transaction().Get(ss.Sub(recordlayer.RecordKey).Pack(append(append(tuple.Tuple{}, key...), int64(0)))).Get()
				if err != nil || len(body) == 0 {
					return nil, fmt.Errorf("no physical body at %v: %v", key, err)
				}
				valueKey := tuple.Tuple{row.body, rt.GetRecordTypeKey()}
				if fixture.sourceFirst {
					valueKey = append(valueKey, row.source, row.id)
				} else {
					valueKey = append(valueKey, row.id, row.source)
				}
				if table == "DOCS" {
					valueKey = append(tuple.Tuple{key[1]}, valueKey...)
				}
				value, err := rc.Transaction().Get(store.IndexSubspace(fixture.template.Underlying().GetIndex(table + "_VALUE")).Pack(valueKey)).Get()
				if err != nil || value == nil {
					return nil, fmt.Errorf("missing VALUE key %v: %v", valueKey, err)
				}
				coverKey := tuple.Tuple{row.id, rt.GetRecordTypeKey(), row.source}
				if table == "DOCS" {
					coverKey = append(tuple.Tuple{key[1]}, coverKey...)
				}
				covered, err := rc.Transaction().Get(store.IndexSubspace(fixture.template.Underlying().GetIndex(table + "_COVER")).Pack(coverKey)).Get()
				if err != nil || !bytes.Equal(covered, (tuple.Tuple{row.body}).Pack()) {
					return nil, fmt.Errorf("wrong covering key/value %v = %x: %v", coverKey, covered, err)
				}
			}
			for _, name := range []string{table + "_VALUE", table + "_COVER", table + "_TEXT", table + "_UNIQUE"} {
				idx := fixture.template.Underlying().GetIndex(name)
				if store.GetIndexState(name) != recordlayer.IndexStateReadable {
					return nil, fmt.Errorf("index %s not readable", name)
				}
				kvs, err := rc.Transaction().GetRange(store.IndexSubspace(idx), fdb.RangeOptions{}).GetSliceWithError()
				if err != nil {
					return nil, err
				}
				if len(kvs) < len(rows) || idx.Type != recordlayer.IndexTypeText && len(kvs) != len(rows) {
					return nil, fmt.Errorf("%s entries=%d, rows=%d", name, len(kvs), len(rows))
				}
				if table == "DOCS" && idx.Type == recordlayer.IndexTypeText {
					for _, kv := range kvs {
						entry, err := store.IndexSubspace(idx).Unpack(kv.Key)
						if err != nil || len(entry) != 5 {
							return nil, fmt.Errorf("invalid physical TEXT entry %v: %v", entry, err)
						}
						if _, ok := entry[0].(int64); !ok {
							return nil, fmt.Errorf("TEXT coordinate is not BIGINT: %v", entry)
						}
						if _, ok := entry[2].(string); !ok || entry[3] != rt.GetRecordTypeKey() {
							return nil, fmt.Errorf("TEXT grouping/token/RT positions shifted: %v", entry)
						}
					}
				}
			}
		}
		plainType := fixture.template.Underlying().GetRecordType("PLAIN")
		plainProtocol, err := rc.Transaction().GetRange(ss.Sub("fdb_go.interval_partitions", int64(1), plainType.GetRecordTypeKey()), fdb.RangeOptions{}).GetSliceWithError()
		if err != nil || len(plainProtocol) != 0 {
			return nil, fmt.Errorf("ordinary writes acquired partition protocol state: %v, %v", plainProtocol, err)
		}
		rt := fixture.template.Underlying().GetRecordType("DOCS")
		protocol := ss.Sub("fdb_go.interval_partitions", int64(1), rt.GetRecordTypeKey())
		headerBytes, err := rc.Transaction().Get(protocol.Pack(tuple.Tuple{"header"})).Get()
		if err != nil {
			return nil, err
		}
		header, err := tuple.Unpack(headerBytes)
		if err != nil || len(header) != 5 || header[0] != int64(1) || header[1] != int64(2) || header[2] != int64(3) || header[3] != fixture.width {
			return nil, fmt.Errorf("bad immutable routing header %v: %v", header, err)
		}
		incarnation, ok := header[4].([]byte)
		if !ok || len(incarnation) != 16 {
			return nil, fmt.Errorf("bad incarnation %v", header[4])
		}
		epoch, err := rc.Transaction().Get(protocol.Pack(tuple.Tuple{"epoch"})).Get()
		if err != nil || len(epoch) == 0 {
			return nil, fmt.Errorf("missing separate topology epoch: %v", err)
		}
		for _, row := range partitionBridgeRows {
			p := row.p10
			if fixture.width == 20 {
				p = row.p20
			}
			for _, spec := range []struct {
				key   string
				value tuple.Tuple
			}{{"active", tuple.Tuple{int64(1)}}, {"fence", tuple.Tuple{int64(1), true}}} {
				value, err := rc.Transaction().Get(protocol.Pack(tuple.Tuple{spec.key, p})).Get()
				if err != nil || !bytes.Equal(value, spec.value.Pack()) {
					return nil, fmt.Errorf("%s(%d)=%x, want %x: %v", spec.key, p, value, spec.value.Pack(), err)
				}
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func partitionBridgeCatalog(t *testing.T, ctx context.Context, h *testkit.FleetHarness, dbPath string, fixture partitionBridgeFixture) {
	t.Helper()
	_, err := h.DB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
		meta := catalog.NewCatalogDatabaseMetaData(catalog.CatalogDatabaseMetaDataOptions{StoreCatalog: h.Cat, NewTransaction: func() api.Transaction { return catalog.NewFDBTransaction(rc) }})
		for _, table := range []string{"DOCS", "PLAIN"} {
			rs, err := meta.PrimaryKeys(ctx, dbPath, fixture.schema, table)
			if err != nil {
				return nil, err
			}
			var got []string
			for rs.Next() {
				name, _ := rs.StringByName("COLUMN_NAME")
				seq, _ := rs.LongByName("KEY_SEQ")
				got = append(got, fmt.Sprintf("%s:%d", name, seq))
			}
			if err := rs.Err(); err != nil {
				return nil, err
			}
			rs.Close()
			want := []string{"ID:1", "SOURCE:2"}
			if fixture.sourceFirst {
				want = []string{"SOURCE:1", "ID:2"}
			}
			if table == "PLAIN" {
				want = []string{"PRIMARY_KEY:1"}
			}
			if !reflect.DeepEqual(got, want) {
				return nil, fmt.Errorf("%s catalog PK=%v, want %v", table, got, want)
			}
		}
		rs, err := meta.IndexInfo(ctx, dbPath, fixture.schema, "DOCS", false, false)
		if err != nil {
			return nil, err
		}
		var names []string
		for rs.Next() {
			name, _ := rs.StringByName("INDEX_NAME")
			col, _ := rs.StringByName("COLUMN_NAME")
			nonUnique, _ := rs.BooleanByName("NON_UNIQUE")
			if col != "" || nonUnique == (name == "DOCS_UNIQUE") {
				return nil, fmt.Errorf("index-info placeholder/uniqueness changed: %s,%s,%v", name, col, nonUnique)
			}
			names = append(names, name)
		}
		rs.Close()
		sort.Strings(names)
		if !reflect.DeepEqual(names, []string{"DOCS_COVER", "DOCS_TEXT", "DOCS_UNIQUE", "DOCS_VALUE"}) {
			return nil, fmt.Errorf("catalog indexes=%v", names)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	tables, _ := fixture.template.Tables()
	for _, table := range tables {
		if table.MetadataName() != "DOCS" {
			continue
		}
		view := table.(*metadata.RecordLayerTable)
		if !reflect.DeepEqual(view.LogicalPrimaryKey().FieldNames(), map[bool][]string{false: {"ID", "SOURCE"}, true: {"SOURCE", "ID"}}[fixture.sourceFirst]) {
			t.Fatal("adapter logical PK lost declared order")
		}
		for _, index := range table.Indexes() {
			view := index.(*metadata.RecordLayerIndex)
			projection := view.PartitionIndexLayout()
			if projection == nil || view.Underlying() != projection.PhysicalIndex() {
				t.Fatalf("missing physical index projection: %s", view.MetadataName())
			}
			var want []int
			var columns []string
			var logical recordlayer.KeyExpression
			switch view.MetadataName() {
			case "DOCS_VALUE":
				want = []int{2, 0, 3, 4}
				columns = []string{"BODY"}
				logical = recordlayer.Field("BODY")
			case "DOCS_COVER":
				want = []int{2, 0, 1, 3}
				columns = []string{"ID"}
				logical = recordlayer.KeyWithValue(recordlayer.Concat(recordlayer.Field("ID"), recordlayer.Field("BODY")), 1)
			case "DOCS_TEXT":
				want = []int{3, 0, 1, 4}
				columns = []string{"ID", "BODY"}
				logical = recordlayer.GroupBy(recordlayer.Field("BODY"), recordlayer.Field("ID"))
			case "DOCS_UNIQUE":
				want = []int{3, 0, 2, 1}
				columns = []string{"SOURCE", "ID"}
				logical = recordlayer.Concat(recordlayer.Field("SOURCE"), recordlayer.Field("ID"))
			}
			if fixture.sourceFirst {
				want[2], want[3] = want[3], want[2]
			}
			if !reflect.DeepEqual(projection.PrimaryKeyEntryOrdinals(), want) {
				t.Fatalf("%s physical entry positions=%v want %v", view.MetadataName(), projection.PrimaryKeyEntryOrdinals(), want)
			}
			if !reflect.DeepEqual(view.IndexColumnNames(), columns) {
				t.Fatalf("logical index key columns=%v want %v", view.IndexColumnNames(), columns)
			}
			if !proto.Equal(view.LogicalExpression().ToKeyExpression(), logical.ToKeyExpression()) {
				t.Fatalf("%s full logical expression lost its structure or included values: %v", view.MetadataName(), view.LogicalExpression().ToKeyExpression())
			}
			if cover, ok := view.LogicalExpression().(*recordlayer.KeyWithValueExpression); ok && cover.SplitPoint() != 1 {
				t.Fatal("covering split not decremented")
			}
			if grouping, ok := view.LogicalExpression().(*recordlayer.GroupingKeyExpression); ok && (grouping.GetGroupingCount() != 1 || grouping.GetGroupedCount() != 1) {
				t.Fatal("TEXT grouping/token counts shifted")
			}
		}
	}
}

func partitionBridgeQueryRows(t *testing.T, ctx context.Context, conn *sql.Conn, query string, args ...any) []string {
	t.Helper()
	rows, err := conn.QueryContext(ctx, query, args...)
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
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		parts := make([]string, len(values))
		for i, value := range values {
			parts[i] = fmt.Sprint(value)
		}
		got = append(got, strings.Join(parts, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return got
}

func TestFDB_PartitionLogicalPhysicalKeyBridges(t *testing.T) {
	t.Parallel()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	for _, sourceFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "ID_SOURCE", true: "SOURCE_ID"}[sourceFirst], func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
			defer cancel()
			h := testkit.NewFleetHarness(t)
			var suffix [8]byte
			if _, err := rand.Read(suffix[:]); err != nil {
				t.Fatal(err)
			}
			name := "PARTITION_BRIDGE_" + strings.ToUpper(hex.EncodeToString(suffix[:]))
			dbPath := "/FRL/" + name
			fixtures := []partitionBridgeFixture{
				{schema: "W10", template: partitionBridgeTemplate(t, name+"_W10", 1, 10, sourceFirst, ""), width: 10, sourceFirst: sourceFirst},
				{schema: "W20", template: partitionBridgeTemplate(t, name+"_W20", 1, 20, sourceFirst, ""), width: 20, sourceFirst: sourceFirst},
			}
			if fixtures[0].template.Underlying().Version() != 10 || fixtures[1].template.Underlying().Version() != 10 {
				t.Fatal("width fixtures must have equal storage versions (seed 1 + split flag + 8 indexes)")
			}
			h.MustRun(t, "create persisted partition bridge schemas", func(tx api.Transaction) error {
				if err := h.Cat.Initialize(tx); err != nil {
					return err
				}
				if err := ddl.NewCreateDatabaseConstantAction(dbPath, h.Cat).Execute(tx); err != nil {
					return err
				}
				for _, fixture := range fixtures {
					if err := ddl.NewSaveSchemaTemplateConstantAction(fixture.template, h.Cat.SchemaTemplateCatalog()).Execute(tx); err != nil {
						return err
					}
					if err := ddl.NewCreateSchemaConstantAction(dbPath, fixture.schema, fixture.template.MetadataName(), h.Cat, h.Ks).Execute(tx); err != nil {
						return err
					}
				}
				return nil
			})
			templateCatalog := h.Cat.SchemaTemplateCatalog().(*catalog.RecordLayerStoreSchemaTemplateCatalog)
			var original *gen.MetaData
			h.MustRun(t, "read original partition template", func(tx api.Transaction) error {
				var err error
				original, err = templateCatalog.LoadTemplateProto(tx, fixtures[0].template.MetadataName(), 1)
				return err
			})
			changed := partitionBridgeTemplate(t, fixtures[0].template.MetadataName(), 2, 20, sourceFirst, "")
			_, alterationErr := h.DB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
				return nil, ddl.NewSaveSchemaTemplateConstantAction(changed, templateCatalog).Execute(catalog.NewFDBTransaction(rc))
			})
			var schemaError *api.Error
			var evolutionError *recordlayer.MetaDataEvolutionError
			if !errors.As(alterationErr, &schemaError) || schemaError.Code != api.ErrCodeInvalidSchemaTemplate || !errors.As(alterationErr, &evolutionError) {
				t.Fatalf("partition-width alteration must retain typed evolution refusal: %v", alterationErr)
			}
			h.MustRun(t, "verify refused width alteration left metadata unchanged", func(tx api.Transaction) error {
				stored, err := templateCatalog.LoadTemplateProto(tx, fixtures[0].template.MetadataName(), 1)
				if err != nil {
					return err
				}
				latest, err := templateCatalog.LoadSchemaTemplate(tx, fixtures[0].template.MetadataName())
				if err != nil {
					return err
				}
				if !proto.Equal(original, stored) || latest.Version() != 1 {
					return fmt.Errorf("refused width alteration changed prior template metadata")
				}
				return nil
			})
			db := testkit.FleetOpen(t, dbPath, "W10")
			logger := &testkit.SyncCaptureLogger{}
			var cache *embedded.RelationalPlanCache
			conn := testkit.PinEmbeddedConn(t, db, func(ec *embedded.EmbeddedConnection) { cache = ec.SharedPlanCache(); ec.SetPlanLogger(logger) })
			setSchema := func(schema string) {
				t.Helper()
				if err := conn.Raw(func(raw any) error { return raw.(*embedded.EmbeddedConnection).SetSchema(schema) }); err != nil {
					t.Fatal(err)
				}
			}
			for _, fixture := range fixtures {
				setSchema(fixture.schema)
				for _, table := range []string{"DOCS", "PLAIN"} {
					for _, row := range partitionBridgeRows {
						if _, err := conn.ExecContext(ctx, "INSERT INTO "+table+" (ID,SOURCE,BODY) VALUES (?,?,?)", row.id, row.source, row.body); err != nil {
							t.Fatal(err)
						}
					}
				}
				partitionBridgePhysical(t, ctx, h, dbPath, fixture, partitionBridgeRows)
				partitionBridgeCatalog(t, ctx, h, dbPath, fixture)
				for _, tc := range []struct {
					query string
					want  []string
					sort  bool
				}{
					{"SELECT ID,SOURCE FROM DOCS WHERE ID IN (1,2) AND SOURCE > -2 ORDER BY ID,SOURCE LIMIT 1", []string{"1|21"}, true},
					{"SELECT ID FROM DOCS WHERE TEXT_CONTAINS_ALL(BODY,'red') AND SOURCE < 0 ORDER BY ID", []string{"1", "6", "7"}, true},
					{"SELECT ID FROM DOCS WHERE SOURCE >= -10 AND SOURCE <= 10 ORDER BY ID DESC LIMIT 2", []string{"6", "5"}, true},
					{"SELECT COUNT(*) FROM DOCS", []string{"13"}, false},
					{"SELECT DISTINCT ID FROM DOCS ORDER BY ID", []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11", "12"}, false},
					{"SELECT ID FROM DOCS WHERE BODY = 'red blue'", []string{"1"}, false},
				} {
					var explain string
					if err := conn.QueryRowContext(ctx, "EXPLAIN "+tc.query).Scan(&explain); err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(explain, "Scan(DOCS") || strings.Contains(explain, "IndexScan") || strings.Contains(explain, "DOCS_VALUE") || strings.Contains(explain, "DOCS_COVER") || strings.Contains(explain, "DOCS_TEXT") || strings.Contains(explain, "DOCS_UNIQUE") {
						t.Fatalf("unbound local access path: %s", explain)
					}
					if tc.sort && !strings.Contains(explain, "InMemorySort") {
						t.Fatalf("missing required global sort: %s", explain)
					}
					if strings.Contains(tc.query, "WHERE") && !strings.Contains(explain, "PredicatesFilter") {
						t.Fatalf("lost original residual: %s", explain)
					}
					if got := partitionBridgeQueryRows(t, ctx, conn, tc.query); !reflect.DeepEqual(got, tc.want) {
						t.Fatalf("%s: %v want %v", tc.query, got, tc.want)
					}
				}
				var plainPlan string
				if err := conn.QueryRowContext(ctx, "EXPLAIN SELECT ID FROM PLAIN WHERE BODY = 'red blue'").Scan(&plainPlan); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(plainPlan, "IndexScan(PLAIN_VALUE") {
					t.Fatalf("plain index control lost: %s", plainPlan)
				}
				if err := conn.Raw(func(raw any) error {
					return raw.(*embedded.EmbeddedConnection).SetOption(api.OptExecutionScannedRowsLimit, int64(2))
				}); err != nil {
					t.Fatal(err)
				}
				got := partitionBridgeQueryRows(t, ctx, conn, "SELECT SOURCE FROM DOCS WHERE TEXT_CONTAINS_ALL(BODY,'red')")
				sort.Strings(got)
				want := []string{"-10", "-11", "-9223372036854775808", "20", "21", "9"}
				sort.Strings(want)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("residual holes across tiny pages: %v", got)
				}
				if err := conn.Raw(func(raw any) error {
					ec := raw.(*embedded.EmbeddedConnection)
					ec.SetFailOnScanLimitReached(true)
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				rows, limitErr := conn.QueryContext(ctx, "SELECT ID FROM DOCS WHERE SOURCE = 21 ORDER BY ID")
				if rows != nil {
					for rows.Next() {
					}
					if limitErr == nil {
						limitErr = rows.Err()
					}
					rows.Close()
				}
				var resource *api.Error
				if !errors.As(limitErr, &resource) || resource.Code != api.ErrCodeExecutionLimitReached {
					t.Fatalf("scan resource limit returned a falsely complete result: %v", limitErr)
				}
				if err := conn.Raw(func(raw any) error {
					ec := raw.(*embedded.EmbeddedConnection)
					ec.SetFailOnScanLimitReached(false)
					ec.SetOptions(api.NewOptionsBuilder().Build())
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				cache.Invalidate()
				before := cache.Counts()
				start := len(logger.Snapshot())
				stmt, err := conn.PrepareContext(ctx, "SELECT ID FROM DOCS WHERE SOURCE = ?")
				if err != nil {
					t.Fatal(err)
				}
				for i, source := range []int64{21, -11, 9} {
					var id int64
					if err := stmt.QueryRowContext(ctx, source).Scan(&id); err != nil || id != []int64{1, 1, 4}[i] {
						t.Fatalf("prepared SOURCE=%d: %d,%v", source, id, err)
					}
				}
				stmt.Close()
				after := cache.Counts()
				events := logger.Snapshot()[start:]
				if after.TertiaryMiss-before.TertiaryMiss != 1 || after.TertiaryHit-before.TertiaryHit != 2 || len(events) != 3 || events[0].Cache != embedded.PlanCacheMiss || events[1].Cache != embedded.PlanCacheHit || events[2].Cache != embedded.PlanCacheHit {
					t.Fatalf("inherited width prevented attributable prepared reuse: before=%+v after=%+v events=%+v", before, after, events)
				}
			}
			// Fresh template names are required: changing an existing template's
			// partition width is forbidden metadata evolution. Both storage versions
			// are equal; the unit cache regression independently fixes the outer name.
			cache.Invalidate()
			start := len(logger.Snapshot())
			for _, spec := range []struct {
				schema     string
				source, id int64
			}{{"W10", 21, 1}, {"W20", -11, 1}, {"W10", 9, 4}} {
				setSchema(spec.schema)
				if got := testkit.QueryIDs(t, conn, "SELECT ID FROM DOCS WHERE SOURCE = ?", spec.source); !reflect.DeepEqual(got, []int64{spec.id}) {
					t.Fatalf("width-scoped rows: %v", got)
				}
			}
			events := logger.Snapshot()[start:]
			if len(events) != 3 || events[0].Cache != embedded.PlanCacheMiss || events[1].Cache != embedded.PlanCacheMiss || events[2].Cache != embedded.PlanCacheHit {
				t.Fatalf("persisted width/schema cache scopes conflated: %+v", events)
			}
			for _, fixture := range fixtures {
				setSchema(fixture.schema)
				key := func(id, source int64) *api.KeySet {
					k := api.NewKeySet()
					_, _ = k.SetKeyColumns(map[string]any{"ID": id, "SOURCE": source})
					return k
				}
				rawDirect := func(fn func(api.DirectAccessStatement) error) error {
					return conn.Raw(func(raw any) error { return fn(raw.(*embedded.EmbeddedConnection).DirectAccess()) })
				}
				if err := rawDirect(func(s api.DirectAccessStatement) error {
					rs, err := s.ExecuteGet(ctx, "DOCS", key(1, 21), nil)
					if err != nil {
						return err
					}
					defer rs.Close()
					if !rs.Next() {
						return fmt.Errorf("logical Get missed physical partition")
					}
					body, _ := rs.StringByName("BODY")
					if body != "red blue" || rs.Next() {
						return fmt.Errorf("logical Get returned wrong row")
					}
					return rs.Err()
				}); err != nil {
					t.Fatal(err)
				}
				if err := rawDirect(func(s api.DirectAccessStatement) error {
					rs, err := s.ExecuteScan(ctx, "DOCS", key(1, 21), api.NewOptionsBuilder().Set(api.OptMaxRows, 1).Build())
					if err != nil {
						return err
					}
					defer rs.Close()
					n := 0
					for rs.Next() {
						n++
					}
					if n != 1 {
						return fmt.Errorf("complete-key Scan rows=%d", n)
					}
					return rs.Err()
				}); err != nil {
					t.Fatal(err)
				}
				before := partitionBridgeSnapshot(t, ctx, h, dbPath, fixture.schema)
				first := "ID"
				if sourceFirst {
					first = "SOURCE"
				}
				prefix := api.NewKeySet()
				_, _ = prefix.SetKeyColumn(first, int64(1))
				for _, partial := range []*api.KeySet{api.EmptyKeySet(), prefix} {
					for _, deleteRange := range []bool{false, true} {
						err := rawDirect(func(s api.DirectAccessStatement) error {
							if deleteRange {
								_, err := s.ExecuteDeleteRange(ctx, "DOCS", partial, nil)
								return err
							}
							rs, err := s.ExecuteScan(ctx, "DOCS", partial, nil)
							if rs != nil {
								rs.Close()
							}
							return err
						})
						var unsupported *api.Error
						if !errors.As(err, &unsupported) || unsupported.Code != api.ErrCodeUnsupportedOperation {
							t.Fatalf("partial direct refusal=%v", err)
						}
					}
				}
				for _, scan := range []bool{false, true} {
					err := rawDirect(func(s api.DirectAccessStatement) error {
						opts := api.NewOptionsBuilder().Set(api.OptIndexHint, "DOCS_VALUE").Build()
						var rs api.ResultSet
						var err error
						if scan {
							rs, err = s.ExecuteScan(ctx, "DOCS", api.EmptyKeySet(), opts)
						} else {
							rs, err = s.ExecuteGet(ctx, "DOCS", api.EmptyKeySet(), opts)
						}
						if rs != nil {
							rs.Close()
						}
						return err
					})
					var unsupported *api.Error
					if !errors.As(err, &unsupported) || unsupported.Code != api.ErrCodeUnsupportedOperation {
						t.Fatalf("unbound direct index refusal=%v", err)
					}
				}
				if after := partitionBridgeSnapshot(t, ctx, h, dbPath, fixture.schema); !reflect.DeepEqual(after, before) {
					t.Fatal("direct refusals changed persisted bytes")
				}
				// Ordinary paging retains declared-key order and consumed-page continuations.
				var cont api.Continuation
				var plain []string
				pages := 0
				for ; pages < 16; pages++ {
					err := rawDirect(func(s api.DirectAccessStatement) error {
						opts := api.NewOptionsBuilder().Set(api.OptMaxRows, 1)
						if cont != nil {
							opts.Set(api.OptContinuation, cont)
						}
						rs, err := s.ExecuteScan(ctx, "PLAIN", api.EmptyKeySet(), opts.Build())
						if err != nil {
							return err
						}
						defer rs.Close()
						for rs.Next() {
							id, _ := rs.LongByName("ID")
							source, _ := rs.LongByName("SOURCE")
							plain = append(plain, fmt.Sprintf("%d|%d", id, source))
						}
						cont, err = rs.Continuation()
						return err
					})
					if err != nil {
						t.Fatal(err)
					}
					if api.AtEnd(cont) {
						break
					}
				}
				ordered := append([]partitionBridgeRow(nil), partitionBridgeRows...)
				sort.Slice(ordered, func(i, j int) bool {
					a, b := ordered[i], ordered[j]
					if sourceFirst {
						return a.source < b.source || a.source == b.source && a.id < b.id
					}
					return a.id < b.id || a.id == b.id && a.source < b.source
				})
				var wantPlain []string
				for _, row := range ordered {
					wantPlain = append(wantPlain, fmt.Sprintf("%d|%d", row.id, row.source))
				}
				if pages < 2 || !api.AtEnd(cont) || !reflect.DeepEqual(plain, wantPlain) {
					t.Fatalf("plain paging changed: pages=%d rows=%v want=%v", pages, plain, wantPlain)
				}
				if result, err := conn.ExecContext(ctx, "DELETE FROM DOCS WHERE ID = 4 AND SOURCE = 9"); err != nil {
					t.Fatal(err)
				} else if n, _ := result.RowsAffected(); n != 1 {
					t.Fatalf("SQL DELETE rows=%d", n)
				}
				if err := rawDirect(func(s api.DirectAccessStatement) error {
					n, err := s.ExecuteDelete(ctx, "DOCS", key(1, -11), nil)
					if err == nil && n != 1 {
						return fmt.Errorf("logical Delete rows=%d", n)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if err := rawDirect(func(s api.DirectAccessStatement) error {
					n, err := s.ExecuteDeleteRange(ctx, "DOCS", key(5, 10), nil)
					if err == nil && n != 1 {
						return fmt.Errorf("complete-key DeleteRange rows=%d", n)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				var remaining []partitionBridgeRow
				for _, row := range partitionBridgeRows {
					if row.id == 4 || row.id == 5 || row.id == 1 && row.source == -11 {
						continue
					}
					remaining = append(remaining, row)
				}
				partitionBridgePhysical(t, ctx, h, dbPath, fixture, remaining)
				if got := partitionBridgeQueryRows(t, ctx, conn, "SELECT ID,SOURCE FROM DOCS WHERE ID = 1"); !reflect.DeepEqual(got, []string{"1|21"}) {
					t.Fatalf("neighbor deleted: %v", got)
				}
			}
			// Actual literal keys and partial predicates remain exact-binding
			// proof controls even while local access paths are conservatively gated.
			for _, proof := range []string{"LITERAL", "PREDICATE"} {
				tmpl := partitionBridgeTemplate(t, name+"_"+proof, 1, 10, sourceFirst, proof)
				h.MustRun(t, "create proof-control schema", func(tx api.Transaction) error {
					if err := ddl.NewSaveSchemaTemplateConstantAction(tmpl, h.Cat.SchemaTemplateCatalog()).Execute(tx); err != nil {
						return err
					}
					return ddl.NewCreateSchemaConstantAction(dbPath, proof, tmpl.MetadataName(), h.Cat, h.Ks).Execute(tx)
				})
				setSchema(proof)
				if _, err := conn.ExecContext(ctx, "INSERT INTO DOCS (ID,SOURCE,BODY) VALUES (1,21,'red'),(2,-11,'blue')"); err != nil {
					t.Fatal(err)
				}
				cache.Invalidate()
				start := len(logger.Snapshot())
				for i, source := range []int64{21, -11, -11} {
					want := int64(2)
					if i == 0 {
						want = 1
					}
					if got := testkit.QueryIDs(t, conn, "SELECT ID FROM DOCS WHERE SOURCE = ?", source); !reflect.DeepEqual(got, []int64{want}) {
						t.Fatalf("%s proof rows=%v", proof, got)
					}
				}
				events := logger.Snapshot()[start:]
				if len(events) != 3 || events[0].Cache != embedded.PlanCacheMiss || events[1].Cache != embedded.PlanCacheMiss || events[2].Cache != embedded.PlanCacheHit {
					t.Fatalf("%s proof restriction lost: %+v", proof, events)
				}
			}
		})
	}
}
