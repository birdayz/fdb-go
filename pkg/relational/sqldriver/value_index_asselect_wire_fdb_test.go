package sqldriver_test

import (
	"bytes"
	"context"
	"fmt"
	"sort"
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

// TestFDB_AsSelectValueIndex_WireEntries is RFC-202 gate (b2) at the Go
// store: records written through a store whose metadata came from the
// AS-SELECT index DDL produce raw index entries with the exact tuple
// encoding the key expression mandates —
//
//   - a plain multi-column index writes (a, b, <primaryKey…>) with an empty
//     tuple value;
//   - a covering (KeyWithValue) index writes only the KEY part (+ primary
//     key) into the entry key and packs the value columns into the FDB
//     VALUE, exactly like Java's StandardIndexMaintainer for
//     KeyWithValueExpression roots;
//   - a DESC index's key element is the TupleOrdering encoding (a byte
//     string), whose byte order INVERTS the ascending order — the wire
//     artefact order_desc_nulls_last exists to produce
//     (OrderFunctionKeyExpressionFactory.java:44-48).
//
// Expected keys are computed from first principles (hand-built tuples packed
// under the index subspace), not by re-running the maintainer.
func TestFDB_AsSelectValueIndex_WireEntries(t *testing.T) {
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
		CREATE TABLE T (id bigint, a bigint, b string, c bigint, PRIMARY KEY(id))
		CREATE INDEX plain_ab AS SELECT a, b FROM t ORDER BY a, b
		CREATE INDEX cover_abc AS SELECT a, b, c FROM t ORDER BY a
		CREATE INDEX desc_a AS SELECT a FROM t ORDER BY a DESC`)
	if err != nil {
		t.Fatalf("schema DDL: %v", err)
	}
	md := tmpl.Underlying()
	desc := md.GetRecordType("T").Descriptor

	rec := func(id, a int64, b string, c int64) proto.Message {
		m := dynamicpb.NewMessage(desc)
		m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(desc.Fields().ByName("A"), protoreflect.ValueOfInt64(a))
		m.Set(desc.Fields().ByName("B"), protoreflect.ValueOfString(b))
		m.Set(desc.Fields().ByName("C"), protoreflect.ValueOfInt64(c))
		return m
	}
	rows := []struct {
		id, a int64
		b     string
		c     int64
	}{
		{1, 5, "x", 100},
		{2, 3, "y", 200},
		{3, 7, "x", 300},
	}

	// The primary key tuple as the store evaluates it (RecordTypeKey prefix +
	// id) — evaluated through the metadata's own primary key expression, an
	// independently-tested production path.
	pkTuple := func(id int64, a int64, b string, c int64) tuple.Tuple {
		stored := &recordlayer.FDBStoredRecord[proto.Message]{RecordType: md.GetRecordType("T"), Record: rec(id, a, b, c)}
		pkVals, evalErr := md.GetRecordType("T").PrimaryKey.Evaluate(stored, stored.Record)
		if evalErr != nil {
			t.Fatalf("pk eval: %v", evalErr)
		}
		if len(pkVals) != 1 {
			t.Fatalf("pk eval produced %d tuples", len(pkVals))
		}
		out := make(tuple.Tuple, len(pkVals[0]))
		for i, v := range pkVals[0] {
			out[i] = v
		}
		return out
	}

	scanIndex := func(rtx *recordlayer.FDBRecordContext, store *recordlayer.FDBRecordStore, name string) (keys [][]byte, vals [][]byte) {
		idx := md.GetIndex(name)
		if idx == nil {
			t.Fatalf("index %s not in metadata", name)
		}
		sub := store.IndexSubspace(idx)
		kvs, rerr := rtx.Transaction().GetRange(fdb.KeyRange{
			Begin: fdb.Key(sub.Bytes()),
			End:   fdb.Key(append(sub.Bytes(), 0xFF)),
		}, fdb.RangeOptions{}).GetSliceWithError()
		if rerr != nil {
			t.Fatalf("scan %s: %v", name, rerr)
		}
		for _, kv := range kvs {
			keys = append(keys, kv.Key)
			vals = append(vals, kv.Value)
		}
		return keys, vals
	}

	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		for _, r := range rows {
			if _, e := store.SaveRecord(rec(r.id, r.a, r.b, r.c)); e != nil {
				return nil, e
			}
		}

		// ---- PLAIN_AB: entry key = (a, b, pk…), value = empty tuple.
		keys, vals := scanIndex(rtx, store, "PLAIN_AB")
		idxSub := store.IndexSubspace(md.GetIndex("PLAIN_AB"))
		var wantKeys [][]byte
		for _, r := range rows {
			entry := append(tuple.Tuple{r.a, r.b}, pkTuple(r.id, r.a, r.b, r.c)...)
			wantKeys = append(wantKeys, idxSub.Pack(entry))
		}
		sort.Slice(wantKeys, func(i, j int) bool { return bytes.Compare(wantKeys[i], wantKeys[j]) < 0 })
		if len(keys) != len(wantKeys) {
			return nil, fmt.Errorf("PLAIN_AB: %d entries, want %d", len(keys), len(wantKeys))
		}
		for i := range keys {
			if !bytes.Equal(keys[i], wantKeys[i]) {
				return nil, fmt.Errorf("PLAIN_AB entry %d key mismatch:\n got %x\nwant %x", i, keys[i], wantKeys[i])
			}
			if len(vals[i]) != 0 {
				return nil, fmt.Errorf("PLAIN_AB entry %d has non-empty value %x — a plain value index stores an empty tuple", i, vals[i])
			}
		}

		// ---- COVER_ABC (keyWithValue(concat(A,B,C),1)): entry key = (a, pk…),
		// value = packed (b, c).
		keys, vals = scanIndex(rtx, store, "COVER_ABC")
		idxSub = store.IndexSubspace(md.GetIndex("COVER_ABC"))
		type kv struct{ k, v []byte }
		var wantKVs []kv
		for _, r := range rows {
			entry := append(tuple.Tuple{r.a}, pkTuple(r.id, r.a, r.b, r.c)...)
			wantKVs = append(wantKVs, kv{
				k: idxSub.Pack(entry),
				v: tuple.Tuple{r.b, r.c}.Pack(),
			})
		}
		sort.Slice(wantKVs, func(i, j int) bool { return bytes.Compare(wantKVs[i].k, wantKVs[j].k) < 0 })
		if len(keys) != len(wantKVs) {
			return nil, fmt.Errorf("COVER_ABC: %d entries, want %d", len(keys), len(wantKVs))
		}
		for i := range keys {
			if !bytes.Equal(keys[i], wantKVs[i].k) {
				return nil, fmt.Errorf("COVER_ABC entry %d key mismatch:\n got %x\nwant %x — the KeyWithValue split point must keep the value columns OUT of the entry key", i, keys[i], wantKVs[i].k)
			}
			if !bytes.Equal(vals[i], wantKVs[i].v) {
				return nil, fmt.Errorf("COVER_ABC entry %d value mismatch:\n got %x\nwant %x — the value columns ride in the FDB value", i, vals[i], wantKVs[i].v)
			}
		}

		// ---- DESC_A: the key element is the TupleOrdering byte-string
		// encoding, and the entries' BYTE order is the DESCENDING a order:
		// a=7 (id 3), a=5 (id 1), a=3 (id 2).
		keys, _ = scanIndex(rtx, store, "DESC_A")
		if len(keys) != 3 {
			return nil, fmt.Errorf("DESC_A: %d entries, want 3", len(keys))
		}
		idxSub = store.IndexSubspace(md.GetIndex("DESC_A"))
		wantIDOrder := []int64{3, 1, 2}
		for i, k := range keys {
			elems, uErr := idxSub.Unpack(fdb.Key(k))
			if uErr != nil {
				return nil, fmt.Errorf("DESC_A entry %d unpack: %v", i, uErr)
			}
			if len(elems) < 2 {
				return nil, fmt.Errorf("DESC_A entry %d has %d elements, want >= 2", i, len(elems))
			}
			if _, isBytes := elems[0].([]byte); !isBytes {
				return nil, fmt.Errorf("DESC_A entry %d leading element is %T, want the TupleOrdering []byte encoding — a DESC column's entry bytes must differ from the plain ascending field", i, elems[0])
			}
			gotID, ok := elems[len(elems)-1].(int64)
			if !ok || gotID != wantIDOrder[i] {
				return nil, fmt.Errorf("DESC_A scan position %d has primary key %v, want id %d — the index must sort DESCENDING by a", i, elems[len(elems)-1], wantIDOrder[i])
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
