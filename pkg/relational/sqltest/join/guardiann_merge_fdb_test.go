package sqltest

import (
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"

	"fdb.dev/pkg/relational/sqltest/testkit"

	"google.golang.org/protobuf/types/dynamicpb"

	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/vectorcodec"
	"fdb.dev/pkg/relational/core/embedded"
)

// Without in-transaction maintenance a GUARDIANN index queues its splits,
// counts them in the secondary subspace (VectorIndexTaskCounts) and asks for a
// merge; OnlineIndexer.MergeIndexes drains the queue under the merge lock.
func TestFDB_GuardiannDeferredMerge(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tmpl, err := embedded.BuildSchemaTemplateFromDDL(`CREATE SCHEMA TEMPLATE gm
		create table docs(id bigint, embedding vector(2, double), primary key (id))
		create vector index docsIdx using guardiann on docs(embedding)
		options (metric = euclidean_metric, primary_cluster_min = 2, primary_cluster_max = 10, collapse_min_duplicates = 5)`)
	if err != nil {
		t.Fatalf("DDL: %v", err)
	}
	md := tmpl.Underlying()
	idx := md.GetIndex("DOCSIDX")
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(testkit.ClusterFile())
	if err != nil {
		t.Fatal(err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	desc := md.GetRecordType("DOCS").Descriptor
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
	// queued reads the task keys and the recorded count.
	queued := func() (tasks int, count int64) {
		inStore(func(s *recordlayer.FDBRecordStore) error {
			tr, _ := fdb.PrefixRange(s.IndexSubspace(idx).Sub(int64(0x07)).Bytes())
			kvs, err := s.Context().Transaction().GetRange(tr, fdb.RangeOptions{}).GetSliceWithError()
			if err != nil {
				return err
			}
			tasks = len(kvs)
			v, err := s.Context().Transaction().Get(fdb.Key(s.IndexSecondarySubspace(idx).Sub(int64(0)).Pack(tuple.Tuple{}))).Get()
			if len(v) == 8 {
				count = int64(binary.LittleEndian.Uint64(v))
			}
			return err
		})
		return tasks, count
	}
	requested := false
	for lo := 0; lo < 60; lo += 20 {
		inStore(func(s *recordlayer.FDBRecordStore) error {
			for i := lo; i < lo+20; i++ {
				m := dynamicpb.NewMessage(desc)
				m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(int64(i)))
				v := []float64{float64(i%3) * 100, float64(i)}
				m.Set(desc.Fields().ByName("EMBEDDING"), protoreflect.ValueOfBytes(vectorcodec.Serialize(v)))
				if _, err := s.SaveRecord(m); err != nil {
					return err
				}
			}
			requested = requested || len(s.GetIndexDeferredMaintenanceControl().GetMergeRequiredIndexes()) > 0
			return nil
		})
	}
	tasks, count := queued()
	if tasks == 0 || int64(tasks) != count || !requested {
		t.Fatalf("queued %d tasks, count %d, merge requested %v: want queued tasks, counted, and a merge request", tasks, count, requested)
	}
	indexer, err := recordlayer.NewOnlineIndexerBuilder().SetDatabase(db).SetMetaData(md).SetIndex(idx).SetSubspace(ks).Build()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if tasks, _ = queued(); tasks == 0 {
			break
		}
		if err := indexer.MergeIndexes(ctx); err != nil {
			t.Fatalf("merge: %v", err)
		}
	}
	if tasks, count = queued(); tasks != 0 || count != 0 {
		t.Fatalf("after merging: %d tasks, count %d", tasks, count)
	}
	inStore(func(s *recordlayer.FDBRecordStore) error {
		res, err := s.SearchVectorIndex(idx, []float64{200, 41}, 3, 100)
		if err != nil {
			return err
		}
		// The table's primary key leads with its record type key.
		if pk := res[0].PrimaryKey; pk[len(pk)-1] != int64(41) {
			return fmt.Errorf("nearest to (200, 41) = %v", pk)
		}
		return nil
	})
}
