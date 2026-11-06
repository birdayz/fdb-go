package sqltest

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/embedded"
)

func TestFDBFullTextHistoricalOmittedPositions(t *testing.T) {
	t.Parallel()
	template, err := embedded.BuildSchemaTemplateFromDDL("CREATE TABLE docs(id BIGINT, tenant BIGINT, body STRING, PRIMARY KEY(id)) CREATE FULLTEXT INDEX body_text ON docs(body) PARTITION BY (tenant)")
	if err != nil {
		t.Fatal(err)
	}
	old := template.Underlying()
	old.GetIndex("BODY_TEXT").SetOption(recordlayer.IndexOptionTextOmitPositions, "true")
	database, space := fulltextMetadataStore(t, old,
		fulltextMessage(t, old, 1, map[string]any{"TENANT": int64(1), "BODY": "red other blue"}),
		fulltextMessage(t, old, 2, map[string]any{"TENANT": int64(1), "BODY": "red blue"}))
	serialized, err := old.ToProto()
	if err != nil {
		t.Fatal(err)
	}
	serialized.Version = proto.Int32(int32(old.Version() + 1))
	current, err := recordlayer.RecordMetaDataFromProto(serialized)
	if err != nil {
		t.Fatal(err)
	}
	current.GetIndex("BODY_TEXT").SetOption(recordlayer.IndexOptionTextOmitPositions, "false")
	if current.GetIndex("BODY_TEXT").LastModifiedVersion != old.GetIndex("BODY_TEXT").LastModifiedVersion {
		t.Fatal("option change unexpectedly requests a rebuild")
	}
	// A raw positional scan still accepts both old postings after opening the newer metadata.
	_, err = database.Run(context.Background(), func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(current).SetSubspace(space).Open()
		if err != nil {
			return nil, err
		}
		for _, comparison := range []*recordlayer.TextComparison{recordlayer.NewTextComparison(recordlayer.TextContainsPhrase, "red blue", "", ""), recordlayer.NewTextWithMaxDistanceComparison("red blue", 1, "", "")} {
			cursor, err := recordlayer.NewTextScan(current.GetIndex("BODY_TEXT"), tuple.Tuple{int64(1)}, comparison, nil).Scan(store, nil, recordlayer.NewScanProperties(recordlayer.DefaultExecuteProperties()))
			if err != nil {
				return nil, err
			}
			count := 0
			for {
				row, err := cursor.OnNext(context.Background())
				if err != nil {
					cursor.Close()
					return nil, err
				}
				if !row.HasNext() {
					break
				}
				count++
			}
			cursor.Close()
			if count != 2 {
				t.Fatalf("historical missing positions were rewritten: raw matches=%d want=2", count)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, predicate := range []string{"TEXT_CONTAINS_PHRASE(body,'red blue')", "TEXT_CONTAINS_ALL_WITHIN(body,'red blue',1)"} {
		query := "SELECT id FROM docs WHERE tenant=1 AND " + predicate + " ORDER BY id"
		plan, err := embedded.PlanRecordQueryWithMetadata(query, current, nil)
		if err != nil {
			t.Fatal(err)
		}
		if shape := plan.Explain(); !strings.Contains(shape, "TextIndexScan(BODY_TEXT") || !strings.Contains(shape, "PredicatesFilter") {
			t.Fatalf("historical positions need an exact indexed residual: %s", shape)
		}
		checkFulltextMetadataQuery(t, database, space, current, query, true, []int64{2})
		checkFulltextMetadataQuery(t, database, space, current, "SELECT id FROM docs WHERE tenant>=1 AND tenant<2 AND "+predicate+" ORDER BY id", false, []int64{2})
	}
	if !old.GetIndex("BODY_TEXT").GetBooleanOption(recordlayer.IndexOptionTextOmitPositions, false) {
		t.Fatal("old metadata was mutated")
	}
}

func TestFDBFullTextCaseDistinctCoordinates(t *testing.T) {
	t.Parallel()
	template, err := embedded.BuildSchemaTemplateFromDDL(`CREATE TABLE docs(id BIGINT, BODY STRING, "body" STRING, TENANT BIGINT, "tenant" BIGINT, PRIMARY KEY(id)) CREATE FULLTEXT INDEX body_text ON docs(BODY) PARTITION BY (TENANT)`)
	if err != nil {
		t.Fatal(err)
	}
	md := template.Underlying()
	database, space := fulltextMetadataStore(t, md,
		fulltextMessage(t, md, 1, map[string]any{"BODY": "red", "body": "blue", "TENANT": int64(1), "tenant": int64(2)}),
		fulltextMessage(t, md, 2, map[string]any{"BODY": "blue", "body": "red", "TENANT": int64(2), "tenant": int64(1)}),
		fulltextMessage(t, md, 3, map[string]any{"BODY": "red", "body": "green", "TENANT": int64(2), "tenant": int64(1)}),
		fulltextMessage(t, md, 4, map[string]any{"BODY": "green", "body": "red", "TENANT": int64(1), "tenant": int64(2)}))
	for _, tc := range []struct {
		where   string
		indexed bool
		want    []int64
	}{
		{`TENANT=1 AND TEXT_CONTAINS_ALL(BODY,'red')`, true, []int64{1}},
		{`TENANT=1 AND TEXT_CONTAINS_ALL("body",'red')`, false, []int64{4}},
		{`"tenant"=1 AND TEXT_CONTAINS_ALL(BODY,'red')`, false, []int64{3}},
		{`"tenant"=1 AND TEXT_CONTAINS_ALL("body",'red')`, false, []int64{2}},
	} {
		checkFulltextMetadataQuery(t, database, space, md, "SELECT id FROM docs WHERE "+tc.where+" ORDER BY id", tc.indexed, tc.want)
	}
}
