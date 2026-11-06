package sqltest

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/relational/core/embedded"
	"fdb.dev/pkg/relational/sqltest/testkit"
)

// Metadata-created indexes exercise SQL eligibility without DDL flattening the key.
func fulltextMetadataStore(t *testing.T, md *recordlayer.RecordMetaData, records ...proto.Message) (*recordlayer.FDBDatabase, subspace.Subspace) {
	t.Helper()
	if testkit.ClusterFile() == "" {
		t.Skip("FDB not available (no Docker)")
	}
	fdb.MustAPIVersion(730)
	raw, err := fdb.OpenDatabase(testkit.ClusterFile())
	if err != nil {
		t.Fatal(err)
	}
	database := recordlayer.NewFDBDatabase(raw)
	space := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	_, err = database.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(space).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		for _, record := range records {
			if _, err := store.SaveRecord(record); err != nil {
				return nil, err
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return database, space
}

func checkFulltextMetadataQuery(t *testing.T, database *recordlayer.FDBDatabase, space subspace.Subspace, md *recordlayer.RecordMetaData, query string, indexed bool, want []int64) {
	t.Helper()
	plan, err := embedded.PlanRecordQueryWithMetadata(query, md, nil)
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	if got := strings.Contains(plan.Explain(), "TextIndexScan"); got != indexed {
		t.Fatalf("%s: indexed=%v want=%v plan=%s", query, got, indexed, plan.Explain())
	}
	result, err := database.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(space).Open()
		if err != nil {
			return nil, err
		}
		cursor, err := executor.ExecutePlan(context.Background(), plan, store, executor.EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
		if err != nil {
			return nil, err
		}
		defer cursor.Close()
		rows, err := executor.CollectAll(context.Background(), cursor)
		if err != nil {
			return nil, err
		}
		var ids []int64
		for _, row := range rows {
			ids = append(ids, row.Positional.Slots[0].(int64))
		}
		return ids, nil
	})
	if err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	if got := result.([]int64); !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: got %v want %v", query, got, want)
	}
}

func fulltextMessage(t *testing.T, md *recordlayer.RecordMetaData, id int64, fields map[string]any) *dynamicpb.Message {
	t.Helper()
	desc := md.GetRecordType("DOCS").Descriptor
	message := dynamicpb.NewMessage(desc)
	message.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
	for name, value := range fields {
		field := desc.Fields().ByName(protoreflect.Name(name))
		if field == nil {
			t.Fatalf("missing field %s", name)
		}
		message.Set(field, protoreflect.ValueOf(value))
	}
	return message
}

func TestFDBFullTextTokenizerUpgradeRetainsHistoricalPostings(t *testing.T) {
	t.Parallel()
	factory := fulltextSQLVersionFactory
	if err := recordlayer.GlobalTextTokenizerRegistry().Register(factory); err != nil {
		t.Fatal(err)
	}
	template, err := embedded.BuildSchemaTemplateFromDDL("CREATE TABLE docs(id BIGINT, body STRING, PRIMARY KEY(id)) CREATE FULLTEXT INDEX body_text ON docs(body)")
	if err != nil {
		t.Fatal(err)
	}
	md := template.Underlying()
	md.GetIndex("BODY_TEXT").SetOption(recordlayer.IndexOptionTextTokenizerName, factory.Name()).SetOption(recordlayer.IndexOptionTextTokenizerVersion, "0")
	database, space := fulltextMetadataStore(t, md, fulltextMessage(t, md, 1, map[string]any{"BODY": "red blue"}))
	metadata, err := md.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	metadata.Version = proto.Int32(int32(md.Version() + 1))
	upgraded, err := recordlayer.RecordMetaDataFromProto(metadata)
	if err != nil {
		t.Fatal(err)
	}
	upgraded.GetIndex("BODY_TEXT").SetOption(recordlayer.IndexOptionTextTokenizerVersion, "1")
	// Opening newer metadata must not rewrite the record's version-0 postings.
	for _, tc := range []struct {
		term string
		want []int64
	}{{"red", nil}, {"blue", []int64{1}}} {
		query := "SELECT id FROM docs WHERE TEXT_CONTAINS_ALL(body,'" + tc.term + "','" + factory.Name() + "') ORDER BY id"
		checkFulltextMetadataQuery(t, database, space, upgraded, query, false, tc.want)
	}
}

func TestFDBFullTextOmittedPositionsEligibility(t *testing.T) {
	t.Parallel()
	template, err := embedded.BuildSchemaTemplateFromDDL("CREATE TABLE docs(id BIGINT, body STRING, PRIMARY KEY(id)) CREATE FULLTEXT INDEX body_text ON docs(body)")
	if err != nil {
		t.Fatal(err)
	}
	md := template.Underlying()
	md.GetIndex("BODY_TEXT").SetOption(recordlayer.IndexOptionTextOmitPositions, "true")
	database, space := fulltextMetadataStore(t, md,
		fulltextMessage(t, md, 1, map[string]any{"BODY": "red blue prefix prelude"}),
		fulltextMessage(t, md, 2, map[string]any{"BODY": "red other blue prefix"}),
		fulltextMessage(t, md, 3, map[string]any{"BODY": "blue prelude"}))
	for _, tc := range []struct {
		predicate string
		indexed   bool
		want      []int64
	}{
		{"TEXT_CONTAINS_ALL(body,'red blue')", true, []int64{1, 2}},
		{"TEXT_CONTAINS_ANY(body,'red')", true, []int64{1, 2}},
		{"TEXT_CONTAINS_PREFIX(body,'pre')", true, []int64{1, 2, 3}},
		{"TEXT_CONTAINS_PHRASE(body,'red blue')", false, []int64{1}},
		{"TEXT_CONTAINS_ALL_WITHIN(body,'red blue',1)", false, []int64{1}},
		{"TEXT_CONTAINS_ALL_PREFIXES(body,'pr prel')", false, []int64{1, 3}},
		{"TEXT_CONTAINS_ANY_PREFIX(body,'re pre')", false, []int64{1, 2, 3}},
	} {
		checkFulltextMetadataQuery(t, database, space, md, "SELECT id FROM docs WHERE "+tc.predicate+" ORDER BY id", tc.indexed, tc.want)
	}
}

func TestFDBFullTextNullStandinCoordinates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		text    recordlayer.NullStandin
		group   recordlayer.NullStandin
		indexed bool
		rawIDs  []int64
	}{
		{"ordinary", recordlayer.NullStandinNull, recordlayer.NullStandinNull, true, []int64{2}},
		{"unique null", recordlayer.NullStandinNullUnique, recordlayer.NullStandinNullUnique, true, []int64{2}},
		{"not null group", recordlayer.NullStandinNull, recordlayer.NullStandinNotNull, false, []int64{1, 2}},
		{"not null text", recordlayer.NullStandinNotNull, recordlayer.NullStandinNull, false, []int64{2, 4}},
		{"not null both", recordlayer.NullStandinNotNull, recordlayer.NullStandinNotNull, false, []int64{1, 2, 4, 7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			template, err := embedded.BuildSchemaTemplateFromDDL("CREATE TABLE docs(id BIGINT, tenant BIGINT, body STRING, PRIMARY KEY(id)) CREATE FULLTEXT INDEX body_text ON docs(body) PARTITION BY (tenant)")
			if err != nil {
				t.Fatal(err)
			}
			md := template.Underlying()
			md.GetIndex("BODY_TEXT").RootExpression = recordlayer.GroupBy(
				recordlayer.FieldWithNullStandin("BODY", recordlayer.FanTypeNone, tc.text),
				recordlayer.FieldWithNullStandin("TENANT", recordlayer.FanTypeNone, tc.group))
			serialized, err := md.ToProto()
			if err != nil {
				t.Fatal(err)
			}
			for _, message := range serialized.GetRecords().GetMessageType() {
				if message.GetName() == "DOCS" {
					for _, field := range message.GetField() {
						if field.GetName() == "BODY" {
							field.DefaultValue = proto.String("red")
						}
					}
				}
			}
			md, err = recordlayer.RecordMetaDataFromProto(serialized)
			if err != nil {
				t.Fatal(err)
			}
			body := md.GetRecordType("DOCS").Descriptor.Fields().ByName("BODY")
			if !body.HasDefault() || body.Default().String() != "red" {
				t.Fatal("fixture lost its declared text default")
			}
			database, space := fulltextMetadataStore(t, md,
				fulltextMessage(t, md, 1, map[string]any{"BODY": "red"}),
				fulltextMessage(t, md, 2, map[string]any{"TENANT": int64(0), "BODY": "red"}),
				fulltextMessage(t, md, 3, map[string]any{"TENANT": int64(7), "BODY": "red"}),
				fulltextMessage(t, md, 4, map[string]any{"TENANT": int64(0)}),
				fulltextMessage(t, md, 5, map[string]any{"TENANT": int64(0), "BODY": "blue"}),
				fulltextMessage(t, md, 6, map[string]any{"TENANT": int64(0), "BODY": ""}),
				fulltextMessage(t, md, 7, nil))
			// Java's NOT_NULL maintenance substitutes defaults; SQL must not borrow that presence rule.
			result, err := database.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(space).Open()
				if err != nil {
					return nil, err
				}
				comparison := recordlayer.NewTextComparison(recordlayer.TextContainsAll, "red", "", "")
				cursor, err := recordlayer.NewTextScan(md.GetIndex("BODY_TEXT"), tuple.Tuple{int64(0)}, comparison, nil).Scan(store, nil, recordlayer.NewScanProperties(recordlayer.DefaultExecuteProperties()))
				if err != nil {
					return nil, err
				}
				entries, err := recordlayer.AsList(context.Background(), cursor)
				if err != nil {
					return nil, err
				}
				var ids []int64
				for _, entry := range entries {
					key := entry.PrimaryKey()
					ids = append(ids, key[len(key)-1].(int64))
				}
				return ids, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if ids := result.([]int64); !reflect.DeepEqual(ids, tc.rawIDs) {
				t.Fatalf("raw default-substituting postings=%v want=%v", ids, tc.rawIDs)
			}
			for _, query := range []struct {
				where   string
				indexed bool
				want    []int64
			}{
				{"tenant=0 AND TEXT_CONTAINS_ALL(body,'red')", tc.indexed, []int64{2}},
				{"tenant=7 AND TEXT_CONTAINS_ALL(body,'red')", tc.indexed, []int64{3}},
				{"tenant=0 AND TEXT_CONTAINS_ALL(body,'blue')", tc.indexed, []int64{5}},
				{"tenant IS NULL AND TEXT_CONTAINS_ALL(body,'red')", false, []int64{1}},
				{"TEXT_CONTAINS_ALL(body,'red')", false, []int64{1, 2, 3}},
				{"tenant=0 AND body IS NULL", false, []int64{4}},
				{"tenant=0 AND body=''", false, []int64{6}},
			} {
				checkFulltextMetadataQuery(t, database, space, md, "SELECT id FROM docs WHERE "+query.where+" ORDER BY id", query.indexed, query.want)
			}
		})
	}
}

func TestFDBFullTextMetadataCoordinates(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"nested text", "nested partition", "computed partition", "flat dot"} {
		t.Run(shape, func(t *testing.T) {
			t.Parallel()
			template, err := embedded.BuildSchemaTemplateFromDDL(`CREATE TYPE AS STRUCT payload (body STRING, tenant BIGINT) CREATE TABLE docs(id BIGINT, body STRING, tenant BIGINT, p payload, "P.BODY" STRING, PRIMARY KEY(id)) CREATE FULLTEXT INDEX body_text ON docs(body)`)
			if err != nil {
				t.Fatal(err)
			}
			md := template.Underlying()
			index := md.GetIndex("BODY_TEXT")
			flatDot, err := recordlayer.ToProtoBufCompliantName("P.BODY")
			if err != nil {
				t.Fatal(err)
			}
			switch shape {
			case "nested text":
				index.RootExpression = recordlayer.GroupBy(recordlayer.Nest("P", recordlayer.Field("BODY")))
			case "nested partition":
				index.RootExpression = recordlayer.GroupBy(recordlayer.Field("BODY"), recordlayer.Nest("P", recordlayer.Field("TENANT")))
			case "computed partition":
				index.RootExpression = recordlayer.GroupBy(recordlayer.Field("BODY"), recordlayer.FunctionExpr("subtract", recordlayer.Field("TENANT")))
			case "flat dot":
				index.RootExpression = recordlayer.GroupBy(recordlayer.Field(flatDot))
			}
			var records []proto.Message
			for _, tc := range []struct {
				id                   int64
				body, nested, dot    string
				tenant, nestedTenant int64
			}{
				{1, "red", "blue", "green", 1, 2}, {2, "blue", "red", "red", 2, 1}, {3, "red", "red", "blue", 1, 2},
			} {
				message := fulltextMessage(t, md, tc.id, map[string]any{"BODY": tc.body, "TENANT": tc.tenant, flatDot: tc.dot})
				parent := message.NewField(message.Descriptor().Fields().ByName("P")).Message()
				parent.Set(parent.Descriptor().Fields().ByName("BODY"), protoreflect.ValueOfString(tc.nested))
				parent.Set(parent.Descriptor().Fields().ByName("TENANT"), protoreflect.ValueOfInt64(tc.nestedTenant))
				message.Set(message.Descriptor().Fields().ByName("P"), protoreflect.ValueOfMessage(parent))
				records = append(records, message)
			}
			database, space := fulltextMetadataStore(t, md, records...)
			checkFulltextMetadataQuery(t, database, space, md, "SELECT id FROM docs WHERE tenant=1 AND TEXT_CONTAINS_ALL(body,'red') ORDER BY id", false, []int64{1, 3})
			checkFulltextMetadataQuery(t, database, space, md, "SELECT id FROM docs WHERE TEXT_CONTAINS_ALL(p.body,'red') ORDER BY id", false, []int64{2, 3})
			checkFulltextMetadataQuery(t, database, space, md, `SELECT id FROM docs WHERE TEXT_CONTAINS_ALL("P.BODY",'red') ORDER BY id`, shape == "flat dot", []int64{2})
		})
	}
}
