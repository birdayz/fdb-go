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

// TestFDB_AsSelectVersionIndex_WireEntries is RFC-202 S4's wire gate: records
// written through a store whose VERSION indexes came from the AS-SELECT DDL
// produce raw index entries whose tuple encoding is Java's
// VersionIndexMaintainer layout —
//
//   - the version key column packs as a VERSIONSTAMP tuple element (code
//     0x33: 10-byte global + 2-byte local), completed by the commit;
//   - in the compound covering shape keyWithValue(concat(COL2, version,
//     COL3, COL4), 3) the versionstamp sits INSIDE the entry key at its
//     declared position (after COL2, before COL3), the split keeps COL4 in
//     the FDB value;
//   - the versionstamp equals the RECORD's stored version byte-for-byte, so
//     an index read and a record read report one version.
//
// Expected keys are computed from the records' own post-commit versions and
// hand-built tuples — never by re-running the maintainer.
func TestFDB_AsSelectVersionIndex_WireEntries(t *testing.T) {
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

	tmpl, err := embedded.BuildSchemaTemplateFromDDL(`CREATE SCHEMA TEMPLATE version_wire_tpl
		CREATE TABLE T (id bigint, col2 string, col3 bigint, col4 bigint, PRIMARY KEY(id))
		CREATE INDEX simple_v AS SELECT "__ROW_VERSION" FROM t ORDER BY "__ROW_VERSION"
		CREATE INDEX compound_v AS SELECT col2, "__ROW_VERSION", col3, col4 FROM t ORDER BY col2, "__ROW_VERSION", col3
		WITH OPTIONS(store_row_versions=true)`)
	if err != nil {
		t.Fatalf("schema DDL: %v", err)
	}
	md := tmpl.Underlying()
	if !md.IsStoreRecordVersions() {
		t.Fatal("template did not enable store_row_versions")
	}
	desc := md.GetRecordType("T").Descriptor

	rec := func(id int64, col2 string, col3, col4 int64) proto.Message {
		m := dynamicpb.NewMessage(desc)
		m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(desc.Fields().ByName("COL2"), protoreflect.ValueOfString(col2))
		m.Set(desc.Fields().ByName("COL3"), protoreflect.ValueOfInt64(col3))
		m.Set(desc.Fields().ByName("COL4"), protoreflect.ValueOfInt64(col4))
		return m
	}
	rows := []struct {
		id         int64
		col2       string
		col3, col4 int64
	}{
		{1, "a", 30, 40},
		{2, "b", 31, 41},
	}

	// Save in ONE transaction (distinct local versions under one commit
	// version), then verify in a SECOND transaction against the records'
	// committed versions.
	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		for _, r := range rows {
			if _, e := store.SaveRecord(rec(r.id, r.col2, r.col3, r.col4)); e != nil {
				return nil, e
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
		if sErr != nil {
			return nil, sErr
		}

		// The committed record versions, from the record store itself.
		versionStamp := func(id int64) tuple.Versionstamp {
			rt := md.GetRecordType("T")
			pkVals, evalErr := rt.PrimaryKey.Evaluate(&recordlayer.FDBStoredRecord[proto.Message]{RecordType: rt, Record: rec(id, "", 0, 0)}, rec(id, "", 0, 0))
			if evalErr != nil {
				t.Fatalf("pk eval: %v", evalErr)
			}
			pk := make(tuple.Tuple, len(pkVals[0]))
			for i, v := range pkVals[0] {
				pk[i] = v
			}
			ver, vErr := store.LoadRecordVersion(pk, false)
			if vErr != nil {
				t.Fatalf("LoadRecordVersion(%d): %v", id, vErr)
			}
			if ver == nil {
				t.Fatalf("record %d has no stored version", id)
			}
			raw := ver.ToBytes()
			vs := tuple.Versionstamp{UserVersion: uint16(ver.GetLocalVersion())}
			copy(vs.TransactionVersion[:], raw[:10])
			return vs
		}
		pkTuple := func(id int64) tuple.Tuple {
			rt := md.GetRecordType("T")
			pkVals, evalErr := rt.PrimaryKey.Evaluate(&recordlayer.FDBStoredRecord[proto.Message]{RecordType: rt, Record: rec(id, "", 0, 0)}, rec(id, "", 0, 0))
			if evalErr != nil {
				t.Fatalf("pk eval: %v", evalErr)
			}
			out := make(tuple.Tuple, len(pkVals[0]))
			for i, v := range pkVals[0] {
				out[i] = v
			}
			return out
		}
		scanIndex := func(name string) (keys, vals [][]byte) {
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

		// ---- SIMPLE_V: entry key = (versionstamp, pk…), empty value.
		keys, vals := scanIndex("SIMPLE_V")
		idxSub := store.IndexSubspace(md.GetIndex("SIMPLE_V"))
		var wantKeys [][]byte
		for _, r := range rows {
			entry := append(tuple.Tuple{versionStamp(r.id)}, pkTuple(r.id)...)
			wantKeys = append(wantKeys, idxSub.Pack(entry))
		}
		sort.Slice(wantKeys, func(i, j int) bool { return bytes.Compare(wantKeys[i], wantKeys[j]) < 0 })
		if len(keys) != len(wantKeys) {
			return nil, fmt.Errorf("SIMPLE_V: %d entries, want %d", len(keys), len(wantKeys))
		}
		for i := range keys {
			if !bytes.Equal(keys[i], wantKeys[i]) {
				return nil, fmt.Errorf("SIMPLE_V entry %d key mismatch:\n got %x\nwant %x — the version column must pack as the record's committed versionstamp", i, keys[i], wantKeys[i])
			}
			if len(vals[i]) != 0 {
				return nil, fmt.Errorf("SIMPLE_V entry %d has non-empty value %x", i, vals[i])
			}
		}

		// ---- COMPOUND_V (keyWithValue(concat(COL2, version, COL3, COL4), 3)):
		// entry key = (col2, versionstamp, col3, pk…) — the version INSIDE the
		// key at its declared position — value = packed (col4).
		keys, vals = scanIndex("COMPOUND_V")
		idxSub = store.IndexSubspace(md.GetIndex("COMPOUND_V"))
		type kvPair struct{ k, v []byte }
		var wantKVs []kvPair
		for _, r := range rows {
			entry := append(tuple.Tuple{r.col2, versionStamp(r.id), r.col3}, pkTuple(r.id)...)
			wantKVs = append(wantKVs, kvPair{
				k: idxSub.Pack(entry),
				v: tuple.Tuple{r.col4}.Pack(),
			})
		}
		sort.Slice(wantKVs, func(i, j int) bool { return bytes.Compare(wantKVs[i].k, wantKVs[j].k) < 0 })
		if len(keys) != len(wantKVs) {
			return nil, fmt.Errorf("COMPOUND_V: %d entries, want %d", len(keys), len(wantKVs))
		}
		for i := range keys {
			if !bytes.Equal(keys[i], wantKVs[i].k) {
				return nil, fmt.Errorf("COMPOUND_V entry %d key mismatch:\n got %x\nwant %x — the versionstamp must sit at the key's declared split position (COL2, version, COL3)", i, keys[i], wantKVs[i].k)
			}
			if !bytes.Equal(vals[i], wantKVs[i].v) {
				return nil, fmt.Errorf("COMPOUND_V entry %d value mismatch:\n got %x\nwant %x — COL4 rides in the FDB value (split 3)", i, vals[i], wantKVs[i].v)
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
