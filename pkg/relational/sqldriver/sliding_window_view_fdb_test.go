package sqldriver_test

import (
	"context"
	"fmt"
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
)

// A vector index over a view that QUALIFYs ROW_NUMBER() OVER (PARTITION BY p
// ORDER BY t) <= K keeps only each partition's K smallest t: the DDL stores
// Java's RowNumberWindowPredicate and the sliding-window maintainer holds the
// window, re-electing from the overflow when a windowed row is deleted.
func TestFDB_SlidingWindowVectorIndexOverView(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmpl, err := embedded.BuildSchemaTemplateFromDDL(`CREATE SCHEMA TEMPLATE sw
		create table docs(zone string, id bigint, t bigint, embedding vector(3, double), primary key (zone, id))
		create view docsView as select embedding, zone, id from docs qualify row_number() over (partition by zone order by t) <= 2
		create vector index docsIdx using hnsw on docsView(embedding) partition by(zone) options (metric = euclidean_metric)`)
	if err != nil {
		t.Fatalf("DDL: %v", err)
	}
	md := tmpl.Underlying()
	idx := md.GetIndex("DOCSIDX")
	if idx == nil || !idx.HasRowNumberWindowPredicate() {
		t.Fatalf("index lacks its window predicate: %v", idx)
	}
	spec, err := idx.RowNumberWindowSpec()
	if err != nil || spec.String() != "QualifyRowNumber(PARTITION BY ZONE ORDER BY T, ASC) <= 2" {
		t.Fatalf("window spec %v %v", spec, err)
	}

	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatal(err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	desc := md.GetRecordType("DOCS").Descriptor
	rec := func(id, ts int64, vec []float64) proto.Message {
		m := dynamicpb.NewMessage(desc)
		m.Set(desc.Fields().ByName("ZONE"), protoreflect.ValueOfString("z"))
		m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(desc.Fields().ByName("T"), protoreflect.ValueOfInt64(ts))
		m.Set(desc.Fields().ByName("EMBEDDING"), protoreflect.ValueOfBytes(recordlayer.SerializeVector(vec)))
		return m
	}
	inStore := func(f func(*recordlayer.FDBRecordStore) error) {
		t.Helper()
		if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			if sErr != nil {
				return nil, sErr
			}
			return nil, f(store)
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Rows 1 and 2 are the window (smallest t); 3 is the overflow, although
	// it is the nearest to the query vector.
	var firstKey tuple.Tuple
	inStore(func(s *recordlayer.FDBRecordStore) error {
		for i, r := range []proto.Message{rec(1, 10, []float64{0, 1, 0}), rec(2, 20, []float64{0, 0, 1}), rec(3, 30, []float64{1, 0, 0})} {
			saved, err := s.SaveRecord(r)
			if err != nil {
				return err
			}
			if i == 0 {
				firstKey = saved.PrimaryKey
			}
		}
		return nil
	})
	knn := func() string {
		plan, err := embedded.PlanRecordQueryWithMetadata(`SELECT id FROM docs WHERE zone = 'z'
			QUALIFY ROW_NUMBER() OVER (PARTITION BY zone ORDER BY euclidean_distance(embedding, [1.0, 0.0, 0.0])) <= 3`, md, nil)
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		if !strings.Contains(plan.Explain(), "VectorIndexScan") {
			t.Fatalf("not a vector scan: %s", plan.Explain())
		}
		var ids []int64
		inStore(func(s *recordlayer.FDBRecordStore) error {
			cur, err := executor.ExecutePlan(ctx, plan, s, executor.EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
			if err != nil {
				return err
			}
			defer cur.Close()
			rows, err := executor.CollectAll(ctx, cur)
			for _, r := range rows {
				ids = append(ids, executor.RowValue(r).(map[string]any)["ID"].(int64))
			}
			return err
		})
		return fmt.Sprint(ids)
	}
	if got := knn(); got != "[1 2]" {
		t.Errorf("window before delete: %s, want [1 2]", got)
	}
	inStore(func(s *recordlayer.FDBRecordStore) error {
		deleted, err := s.DeleteRecord(firstKey)
		if err == nil && !deleted {
			err = fmt.Errorf("row 1 not deleted")
		}
		return err
	})
	if got := knn(); got != "[3 2]" {
		t.Errorf("window after delete (3 re-elected): %s, want [3 2]", got)
	}
}
