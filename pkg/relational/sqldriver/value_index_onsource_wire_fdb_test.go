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

// TestFDB_OnSourceIndex_WireEntries is RFC-202 S6's wire gate: records written
// through a store whose metadata came from the ON-source index DDL produce the
// exact raw entries Java writes for the same declarations —
//
//   - `ON T(a, b)` writes (a, b, <primaryKey…>) with an empty value;
//   - `ON T(a) INCLUDE (b, c)` is a covering KeyWithValue index: only the key
//     column (+ primary key) in the entry key, the INCLUDE columns packed into
//     the FDB value;
//   - `ON T(a DESC)`'s key element is the TupleOrdering byte-string encoding
//     (OrderFunctionKeyExpressionFactory.java:44-48 →
//     OrderFunctionKeyExpression.evaluateFunction → TupleOrdering.pack with
//     Direction.DESC_NULLS_LAST), pinned here against HAND-DERIVED byte
//     vectors so the test cannot pass by agreeing with a broken Go encoder.
//
// Expected keys are computed from first principles (hand-built tuples packed
// under the index subspace), never by re-running the maintainer.
func TestFDB_OnSourceIndex_WireEntries(t *testing.T) {
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
		CREATE INDEX on_plain ON T(a, b)
		CREATE INDEX on_cover ON T(a) INCLUDE (b, c)
		CREATE INDEX on_desc ON T(a DESC)`)
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

	pkTuple := func(id, a int64, b string, c int64) tuple.Tuple {
		stored := &recordlayer.FDBStoredRecord[proto.Message]{Record: rec(id, a, b, c)}
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

	// TupleOrdering DESC_NULLS_LAST of the single-element tuple (a), derived
	// BY HAND from Java's TupleOrdering.invert (TupleOrdering.java): the tuple
	// packing (0x15, a) is bit-flipped, repacked 7 bits per byte (high bit 0),
	// and closed with a pad byte plus a terminator 0x80|(npad<<4). For a=3:
	// pack=15 03 → flip=EA FC → 7-bit stream 1110101|0111111|00 → 75 3F, pad
	// (00<<5)|11111=1F, terminator D0. Same derivation for 5 and 7.
	descEncoding := map[int64][]byte{
		3: {0x75, 0x3F, 0x1F, 0xD0},
		5: {0x75, 0x3E, 0x5F, 0xD0},
		7: {0x75, 0x3E, 0x1F, 0xD0},
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

		// ---- ON_PLAIN: entry key = (a, b, pk…), value = empty.
		keys, vals := scanIndex(rtx, store, "ON_PLAIN")
		idxSub := store.IndexSubspace(md.GetIndex("ON_PLAIN"))
		var wantKeys [][]byte
		for _, r := range rows {
			entry := append(tuple.Tuple{r.a, r.b}, pkTuple(r.id, r.a, r.b, r.c)...)
			wantKeys = append(wantKeys, idxSub.Pack(entry))
		}
		sort.Slice(wantKeys, func(i, j int) bool { return bytes.Compare(wantKeys[i], wantKeys[j]) < 0 })
		if len(keys) != len(wantKeys) {
			return nil, fmt.Errorf("ON_PLAIN: %d entries, want %d", len(keys), len(wantKeys))
		}
		for i := range keys {
			if !bytes.Equal(keys[i], wantKeys[i]) {
				return nil, fmt.Errorf("ON_PLAIN entry %d key mismatch:\n got %x\nwant %x", i, keys[i], wantKeys[i])
			}
			if len(vals[i]) != 0 {
				return nil, fmt.Errorf("ON_PLAIN entry %d has non-empty value %x", i, vals[i])
			}
		}

		// ---- ON_COVER (keyWithValue(concat(A,B,C),1)): entry key = (a, pk…),
		// value = packed (b, c). The INCLUDE columns must stay OUT of the key.
		keys, vals = scanIndex(rtx, store, "ON_COVER")
		idxSub = store.IndexSubspace(md.GetIndex("ON_COVER"))
		type kvPair struct{ k, v []byte }
		var wantKVs []kvPair
		for _, r := range rows {
			entry := append(tuple.Tuple{r.a}, pkTuple(r.id, r.a, r.b, r.c)...)
			wantKVs = append(wantKVs, kvPair{
				k: idxSub.Pack(entry),
				v: tuple.Tuple{r.b, r.c}.Pack(),
			})
		}
		sort.Slice(wantKVs, func(i, j int) bool { return bytes.Compare(wantKVs[i].k, wantKVs[j].k) < 0 })
		if len(keys) != len(wantKVs) {
			return nil, fmt.Errorf("ON_COVER: %d entries, want %d", len(keys), len(wantKVs))
		}
		for i := range keys {
			if !bytes.Equal(keys[i], wantKVs[i].k) {
				return nil, fmt.Errorf("ON_COVER entry %d key mismatch:\n got %x\nwant %x — INCLUDE columns leaked into the entry key", i, keys[i], wantKVs[i].k)
			}
			if !bytes.Equal(vals[i], wantKVs[i].v) {
				return nil, fmt.Errorf("ON_COVER entry %d value mismatch:\n got %x\nwant %x — the INCLUDE columns ride in the FDB value", i, vals[i], wantKVs[i].v)
			}
		}

		// ---- ON_DESC: entry key = (tupleOrdering(a), pk…) with the exact
		// hand-derived DESC_NULLS_LAST bytes, and byte order = descending a.
		keys, vals = scanIndex(rtx, store, "ON_DESC")
		idxSub = store.IndexSubspace(md.GetIndex("ON_DESC"))
		var wantDesc [][]byte
		for _, r := range rows {
			entry := append(tuple.Tuple{descEncoding[r.a]}, pkTuple(r.id, r.a, r.b, r.c)...)
			wantDesc = append(wantDesc, idxSub.Pack(entry))
		}
		sort.Slice(wantDesc, func(i, j int) bool { return bytes.Compare(wantDesc[i], wantDesc[j]) < 0 })
		if len(keys) != len(wantDesc) {
			return nil, fmt.Errorf("ON_DESC: %d entries, want %d", len(keys), len(wantDesc))
		}
		for i := range keys {
			if !bytes.Equal(keys[i], wantDesc[i]) {
				return nil, fmt.Errorf("ON_DESC entry %d key mismatch:\n got %x\nwant %x — the DESC column's entry bytes must be Java's TupleOrdering DESC_NULLS_LAST encoding", i, keys[i], wantDesc[i])
			}
			if len(vals[i]) != 0 {
				return nil, fmt.Errorf("ON_DESC entry %d has non-empty value %x", i, vals[i])
			}
		}
		// Scan order sanity: descending a ⇒ ids 3 (a=7), 1 (a=5), 2 (a=3).
		wantIDOrder := []int64{3, 1, 2}
		for i, k := range keys {
			elems, uErr := idxSub.Unpack(fdb.Key(k))
			if uErr != nil {
				return nil, fmt.Errorf("ON_DESC entry %d unpack: %v", i, uErr)
			}
			gotID, ok := elems[len(elems)-1].(int64)
			if !ok || gotID != wantIDOrder[i] {
				return nil, fmt.Errorf("ON_DESC scan position %d has primary key %v, want id %d", i, elems[len(elems)-1], wantIDOrder[i])
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
