//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// Java scopes the record-layer probes and clears below to a value's tuple
// range, so a value whose packing another extends (b"\x01" against
// b"\x01\x00\x05", "a" against "a\x00b") is neither a duplicate, nor a holder
// of the same score, nor inside the other's delete prefix; and SUM undoes
// math.MinInt64 by adding it again, modulo 2^64. Where Java's own range is
// [key, strinc(key)) (the standard index clear of deleteRecordsWhere, the
// survivor clear after a resolved violation), the extending value is reached
// in both engines. Each scenario runs the same ops, one transaction each,
// through both engines; verdicts and the store's keyspaces must be equal.
var _ = Describe("Tuple-range index probes match Java", func() {
	const (
		shortS = "a"
		longS  = "a\x00b"
	)
	shortB, longB := []byte{0x01}, []byte{0x01, 0x00, 0x05}

	metaData := func(pk func(name string) recordlayer.KeyExpression, configure func(*recordlayer.RecordMetaDataBuilder), indexes ...*recordlayer.Index) *recordlayer.RecordMetaData {
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(demopb.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(pk("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(pk("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(pk("id"))
		if configure != nil {
			configure(builder)
		}
		for _, idx := range indexes {
			builder.AddIndex("TypedRecord", idx)
		}
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		return md
	}
	byField := func(name string) recordlayer.KeyExpression { return recordlayer.Field(name) }
	typed := func(id int64, b []byte, s string, v int64, price int32) *demopb.TypedRecord {
		return &demopb.TypedRecord{Id: proto.Int64(id), ValBytes: b, ValString: proto.String(s), ValInt64: proto.Int64(v), Price: proto.Int32(price)}
	}

	It("unique, RANK and SUM maintenance over extending packings and MinInt64", func() {
		md := metaData(byField, nil,
			recordlayer.NewIndex("typed$bytes_unique", recordlayer.Field("val_bytes")).SetUnique(),
			recordlayer.NewIndex("typed$string_unique", recordlayer.Field("val_string")).SetUnique(),
			recordlayer.NewRankIndex("typed$string_rank", recordlayer.GroupBy(recordlayer.Field("val_string"))),
			recordlayer.NewSumIndex("typed$sum_int64", recordlayer.Ungrouped(recordlayer.Field("val_int64"))))
		expectOpsAsJava(md, 0, []probeOp{
			saveOp(typed(1, longB, longS, math.MinInt64, 1)),
			saveOp(typed(2, shortB, shortS, 5, 1)),
			saveOp(typed(2, shortB, "z", 5, 1)),       // RANK removes "a"
			saveOp(typed(1, longB, longS, 7, 1)),      // SUM removes MinInt64
			saveOp(typed(3, []byte{0x09}, "z", 1, 1)), // a real duplicate
		}, []string{"ok", "ok", "ok", "ok", "com.apple.foundationdb.record.RecordIndexUniquenessViolation"})
	})

	It("WRITE_ONLY unique violations and their resolution", func() {
		bIdx := recordlayer.NewIndex("typed$bytes_unique", recordlayer.Field("val_bytes")).SetUnique()
		sIdx := recordlayer.NewIndex("typed$string_unique", recordlayer.Field("val_string")).SetUnique()
		md := metaData(byField, nil, bIdx, sIdx)
		ops := []probeOp{{op: "writeOnly:" + bIdx.Name}, {op: "writeOnly:" + sIdx.Name}}
		for id, s := range []string{shortS, shortS, longS, longS} {
			ops = append(ops, saveOp(&demopb.TypedRecord{Id: proto.Int64(int64(id + 1)), ValString: proto.String(s)}))
		}
		ops = append(ops,
			saveOp(&demopb.TypedRecord{Id: proto.Int64(5), ValBytes: longB}),
			saveOp(&demopb.TypedRecord{Id: proto.Int64(6), ValBytes: shortB}),
			// Deletes record 2. Removing the second-to-last violation of "a" then
			// clears Range.startsWith(pack("a")): "a\x00b"'s violations too.
			probeOp{op: "resolve:" + sIdx.Name, arg: tuple.Tuple{tuple.Tuple{shortS}, tuple.Tuple{int64(1)}}.Pack()})
		expectOpsAsJava(md, 0, ops, nil)
	})

	It("TIME_WINDOW_LEADERBOARD removal of a score an extending score follows", func() {
		idx := recordlayer.NewTimeWindowLeaderboardIndex("typed$leaderboard",
			recordlayer.Ungrouped(recordlayer.Concat(recordlayer.Field("price"), recordlayer.Field("val_int64"), recordlayer.Field("val_string"))))
		md := metaData(byField, nil, idx)
		expectOpsAsJava(md, 0, []probeOp{
			{op: "allTimeWindow:" + idx.Name},
			saveOp(typed(1, nil, longS, 1000, 100)),
			saveOp(typed(2, nil, shortS, 1000, 100)),
			{op: "delete", arg: tuple.Tuple{int64(2)}.Pack()},
		}, nil)
	})

	// Format 5 keeps record versions in the legacy RecordVersionKey space.
	for _, format := range []int32{0, 5} {
		It(fmt.Sprintf("deleteRecordsWhere with a prefix another prefix extends (format %d)", format), func() {
			withType := func(name string) recordlayer.KeyExpression {
				if name == "id" {
					return recordlayer.Concat(recordlayer.RecordTypeKey(), recordlayer.Field("val_string"), recordlayer.Field("id"))
				}
				return recordlayer.Concat(recordlayer.RecordTypeKey(), recordlayer.Field(name))
			}
			md := metaData(withType, func(b *recordlayer.RecordMetaDataBuilder) {
				b.SetRecordCountKey(recordlayer.Concat(recordlayer.RecordTypeKey(), recordlayer.Field("val_string"), recordlayer.Field("price")))
				b.SetStoreRecordVersions(true)
			},
				recordlayer.NewIndex("typed$string_price", recordlayer.Concat(recordlayer.Field("val_string"), recordlayer.Field("price"))),
				recordlayer.NewPermutedMaxIndex("typed$permuted_max",
					recordlayer.GroupBy(recordlayer.Field("val_int64"), recordlayer.Field("val_string"), recordlayer.Field("price")), 1))
			expectOpsAsJava(md, format, []probeOp{
				saveOp(typed(1, nil, shortS, 5, 10)),
				saveOp(typed(2, nil, longS, 7, 10)),
				{op: "deleteWhere:TypedRecord:val_string", arg: tuple.Tuple{shortS}.Pack()},
			}, nil)
		})
	}
})

// probeOp is one op of tupleRangeOpsJava; record is the message a "save" writes.
type probeOp struct {
	op     string
	arg    []byte
	record *demopb.TypedRecord
}

func saveOp(r *demopb.TypedRecord) probeOp {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(r)
	Expect(err).NotTo(HaveOccurred())
	return probeOp{op: "save", arg: b, record: r}
}

// expectOpsAsJava runs ops through Java's tupleRangeOpsJava and through Go,
// each op in its own transaction on a store of md at format (0: the default),
// and requires the same verdicts (want, or all "ok" when want is nil) and equal
// keyspaces.
func expectOpsAsJava(md *recordlayer.RecordMetaData, format int32, ops []probeOp, want []string) {
	GinkgoHelper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	clusterFile, err := sharedContainer.ClusterFile(ctx)
	Expect(err).NotTo(HaveOccurred())
	mdProto, err := md.ToProto()
	Expect(err).NotTo(HaveOccurred())
	mdBytes, err := proto.Marshal(mdProto)
	Expect(err).NotTo(HaveOccurred())
	if want == nil {
		for range ops {
			want = append(want, "ok")
		}
	}
	names := make([]string, len(ops))
	args := make([][]int, len(ops))
	for i, op := range ops {
		names[i] = op.op
		args[i] = BytesToIntArray(op.arg)
	}

	javaSS := subspace.Sub(tuple.Tuple{"tuple_range_java", uuid.NewString()}...)
	var java probeDump
	Expect(NewJavaInvoker().InvokeAs(ctx, "tupleRangeOpsJava", map[string]any{
		"clusterFile": clusterFile, "subspace": BytesToIntArray(javaSS.Bytes()), "metaData": BytesToIntArray(mdBytes),
		"recordTypeName": "TypedRecord", "ops": names, "args": args, "formatVersion": format,
	}, &java)).To(Succeed())
	Expect(java.Verdicts).To(Equal(want), "Java")
	Expect(java.Keys).NotTo(BeEmpty())
	Expect(java.KVs).NotTo(BeEmpty())

	goSS := subspace.Sub(tuple.Tuple{"tuple_range_go", uuid.NewString()}...)
	db := recordlayer.NewFDBDatabase(sharedDB)
	var verdicts []string
	for _, op := range ops {
		_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			builder := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(goSS)
			if format != 0 {
				builder.SetFormatVersion(format)
			}
			store, err := builder.CreateOrOpen()
			if err != nil {
				return nil, err
			}
			return nil, applyProbeOp(store, md, op)
		})
		var violation *recordlayer.RecordIndexUniquenessViolationError
		switch {
		case err == nil:
			verdicts = append(verdicts, "ok")
		case errors.As(err, &violation):
			verdicts = append(verdicts, "com.apple.foundationdb.record.RecordIndexUniquenessViolation")
		default:
			verdicts = append(verdicts, err.Error())
		}
	}
	Expect(verdicts).To(Equal(want), "Go")
	got := goProbeDump(ctx, db, goSS)
	Expect(got.Keys).To(Equal(java.Keys), "record and legacy version keys")
	Expect(got.KVs).To(Equal(java.KVs), "index, secondary, count and violation key-value pairs")
}

func applyProbeOp(store *recordlayer.FDBRecordStore, md *recordlayer.RecordMetaData, op probeOp) error {
	parts := strings.Split(op.op, ":")
	switch parts[0] {
	case "save":
		_, err := store.SaveRecord(op.record)
		return err
	case "delete":
		pk, err := tuple.Unpack(op.arg)
		if err != nil {
			return err
		}
		_, err = store.DeleteRecord(pk)
		return err
	case "deleteWhere":
		value, err := tuple.Unpack(op.arg)
		if err != nil {
			return err
		}
		return store.DeleteRecordsWhere(append(tuple.Tuple{md.GetRecordType(parts[1]).GetRecordTypeKey()}, value...))
	case "writeOnly":
		_, err := store.MarkIndexWriteOnly(parts[1])
		return err
	case "resolve":
		t, err := tuple.Unpack(op.arg)
		if err != nil {
			return err
		}
		return store.ResolveUniquenessViolationByDeletion(md.GetIndex(parts[1]), t[0].(tuple.Tuple), t[1].(tuple.Tuple))
	case "allTimeWindow":
		m, err := store.GetIndexMaintainer(md.GetIndex(parts[1]))
		if err != nil {
			return err
		}
		return m.(leaderboardWindowUpdater).PerformWindowUpdate(&recordlayer.TimeWindowLeaderboardWindowUpdate{
			UpdateTimestamp: 500, AllTime: true, Rebuild: recordlayer.TimeWindowRebuildIfOverlappingChanged,
		}, store)
	}
	return fmt.Errorf("unknown op %q", op.op)
}

// probeDump is tupleRangeOpsJava's result: Keys holds the keys of spaces 1 and
// 8, KVs the key-value pairs of spaces 2, 3, 4 and 7, hex relative to the store.
type probeDump struct {
	Verdicts []string   `json:"verdicts"`
	Keys     [][]string `json:"keys"`
	KVs      [][]string `json:"kvs"`
}

func goProbeDump(ctx context.Context, db *recordlayer.FDBDatabase, ss subspace.Subspace) probeDump {
	GinkgoHelper()
	var out probeDump
	_, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		out = probeDump{Keys: [][]string{}, KVs: [][]string{}}
		for _, space := range []int64{1, 8, 2, 3, 4, 7} {
			kvs, err := rtx.Transaction().GetRange(ss.Sub(space), fdb.RangeOptions{}).GetSliceWithError()
			if err != nil {
				return nil, err
			}
			for _, kv := range kvs {
				key := hex.EncodeToString(kv.Key[len(ss.Bytes()):])
				if space == 1 || space == 8 {
					out.Keys = append(out.Keys, []string{key})
				} else {
					out.KVs = append(out.KVs, []string{key, hex.EncodeToString(kv.Value)})
				}
			}
		}
		return nil, nil
	})
	Expect(err).NotTo(HaveOccurred())
	return out
}
