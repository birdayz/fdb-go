package sqldriver_test

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/embedded"
)

// TestFDB_AsSelectSparseIndex_WireEntries is RFC-202 S5's end-to-end proof:
// an AS-SELECT index with a WHERE clause is SPARSE on the wire — the
// DDL-produced predicate proto (getTopLevelPredicate →
// IndexPredicate.fromQueryPredicate, MaterializedViewIndexGenerator.java:169-172)
// is consumed by the maintainer (shouldIndexThisRecord,
// IndexPredicate.java:77-85; Go: index_maintainer.go's predicate check), so
// only matching records produce index entries, and an UPDATE that moves a
// record across the predicate boundary inserts/removes its entry.
//
// Two predicate shapes ride the same test because they exercise the two
// wire-visible storage branches: a plain comparison (stored as a
// ValuePredicate), and an OR of comparisons (the DNF-stored branch,
// sparse-index-tests.yamsql i6's shape) — proving the produced proto is
// evaluable by the consumer for both.
func TestFDB_AsSelectSparseIndex_WireEntries(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())

	tmpl, err := embedded.BuildSchemaTemplateFromDDL(`
		CREATE TABLE T (id bigint, a bigint, PRIMARY KEY(id))
		CREATE INDEX sparse_gt AS SELECT a FROM t WHERE a > 100
		CREATE INDEX sparse_or AS SELECT a FROM t WHERE a < 40 OR a > 90`)
	if err != nil {
		t.Fatalf("schema DDL: %v", err)
	}
	md := tmpl.Underlying()
	for _, name := range []string{"SPARSE_GT", "SPARSE_OR"} {
		idx := md.GetIndex(name)
		if idx == nil {
			t.Fatalf("index %s not in metadata", name)
		}
		if idx.GetPredicateProto() == nil {
			t.Fatalf("index %s carries no predicate — the WHERE was dropped", name)
		}
	}
	desc := md.GetRecordType("T").Descriptor
	rec := func(id, a int64) proto.Message {
		m := dynamicpb.NewMessage(desc)
		m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(desc.Fields().ByName("A"), protoreflect.ValueOfInt64(a))
		return m
	}

	// indexedIDs scans the raw index subspace and returns the primary keys
	// (last tuple element) present, in scan order.
	indexedIDs := func(rtx *recordlayer.FDBRecordContext, store *recordlayer.FDBRecordStore, name string) ([]int64, error) {
		sub := store.IndexSubspace(md.GetIndex(name))
		kvs, rerr := rtx.Transaction().GetRange(fdb.KeyRange{
			Begin: fdb.Key(sub.Bytes()),
			End:   fdb.Key(append(sub.Bytes(), 0xFF)),
		}, fdb.RangeOptions{}).GetSliceWithError()
		if rerr != nil {
			return nil, rerr
		}
		var ids []int64
		for _, kv := range kvs {
			elems, uErr := sub.Unpack(fdb.Key(kv.Key))
			if uErr != nil {
				return nil, uErr
			}
			id, ok := elems[len(elems)-1].(int64)
			if !ok {
				return nil, fmt.Errorf("%s entry pk element is %T", name, elems[len(elems)-1])
			}
			ids = append(ids, id)
		}
		return ids, nil
	}

	assertIDs := func(got []int64, want ...int64) error {
		if len(got) != len(want) {
			return fmt.Errorf("indexed ids %v, want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				return fmt.Errorf("indexed ids %v, want %v", got, want)
			}
		}
		return nil
	}

	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		// a values: id1=10 (or-match via <40), id2=50 (no match anywhere),
		// id3=95 (or-match via >90), id4=150 (gt-match AND or-match).
		for _, r := range []struct{ id, a int64 }{{1, 10}, {2, 50}, {3, 95}, {4, 150}} {
			if _, e := store.SaveRecord(rec(r.id, r.a)); e != nil {
				return nil, e
			}
		}

		// SPARSE_GT (a > 100): only id4.
		ids, iErr := indexedIDs(rtx, store, "SPARSE_GT")
		if iErr != nil {
			return nil, iErr
		}
		if aErr := assertIDs(ids, 4); aErr != nil {
			return nil, fmt.Errorf("SPARSE_GT after insert: %w — a sparse index must omit non-matching records", aErr)
		}

		// SPARSE_OR (a < 40 OR a > 90): id1 (a=10), id3 (a=95), id4 (a=150) —
		// scan order is by a: 10, 95, 150.
		ids, iErr = indexedIDs(rtx, store, "SPARSE_OR")
		if iErr != nil {
			return nil, iErr
		}
		if aErr := assertIDs(ids, 1, 3, 4); aErr != nil {
			return nil, fmt.Errorf("SPARSE_OR after insert: %w — the stored DNF predicate must gate entries disjunct-by-disjunct", aErr)
		}

		// UPDATE across the boundary: id2 50→200 now matches both; id4
		// 150→60 now matches neither.
		if _, e := store.SaveRecord(rec(2, 200)); e != nil {
			return nil, e
		}
		if _, e := store.SaveRecord(rec(4, 60)); e != nil {
			return nil, e
		}
		ids, iErr = indexedIDs(rtx, store, "SPARSE_GT")
		if iErr != nil {
			return nil, iErr
		}
		if aErr := assertIDs(ids, 2); aErr != nil {
			return nil, fmt.Errorf("SPARSE_GT after update: %w — crossing the predicate boundary must insert/remove the entry", aErr)
		}
		ids, iErr = indexedIDs(rtx, store, "SPARSE_OR")
		if iErr != nil {
			return nil, iErr
		}
		// a values now: id1=10 (match), id3=95 (match), id2=200 (match); by a:
		// 10, 95, 200 → ids 1, 3, 2. id4 (a=60) matches neither disjunct.
		if aErr := assertIDs(ids, 1, 3, 2); aErr != nil {
			return nil, fmt.Errorf("SPARSE_OR after update: %w", aErr)
		}

		// DELETE a matching record: its entry must go. The primary key tuple
		// is evaluated through the metadata's own primary key expression (it
		// may carry a record-type prefix).
		stored := &recordlayer.FDBStoredRecord[proto.Message]{Record: rec(2, 200)}
		pkVals, pkErr := md.GetRecordType("T").PrimaryKey.Evaluate(stored, stored.Record)
		if pkErr != nil || len(pkVals) != 1 {
			return nil, fmt.Errorf("pk eval: %v (%d tuples)", pkErr, len(pkVals))
		}
		pk := make(tuple.Tuple, len(pkVals[0]))
		for i, v := range pkVals[0] {
			pk[i] = v
		}
		if _, e := store.DeleteRecord(pk); e != nil {
			return nil, e
		}
		ids, iErr = indexedIDs(rtx, store, "SPARSE_GT")
		if iErr != nil {
			return nil, iErr
		}
		if aErr := assertIDs(ids); aErr != nil {
			return nil, fmt.Errorf("SPARSE_GT after delete: %w", aErr)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
