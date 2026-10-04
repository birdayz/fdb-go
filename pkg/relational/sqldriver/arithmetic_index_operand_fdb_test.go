package sqldriver_test

import (
	"context"
	"reflect"
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

// TestFDB_ArithmeticIndex_NonIntegerOperandTruncatesLikeJava pins what the
// target does with `CREATE INDEX i AS SELECT d + d FROM t` over a DOUBLE column.
// Java emits the same key expression: ArithmeticValue lowers through
// getLogicalOperator().name().toLowerCase() (MaterializedViewIndexGenerator
// .java:573-574), which yields "add" for every operand type because the LOGICAL
// operator carries no lane. The registered `add` key function is
// LongArithmethicFunctionKeyExpression, which reads its operands with
// Key.Evaluated.getNullableLong (Key.java:579-582): ANY java.lang.Number is
// accepted through Number.longValue(), so a DOUBLE operand truncates toward zero
// and the insert SUCCEEDS. MEASURED against the live 4.14.2.0 JVM: inserts of
// 1.5, -1.5 and 2.0 succeed (spec "WS-J long arithmetic key functions over
// non-integer operands"), the raw index entries are the truncated long sums,
// byte-equal to the ones a Go writer stores in the same index (spec "WS-J F2b
// index entries written by both engines": -1.5 -> -2, 2.9999 -> 4, NaN -> 0,
// the infinities and 2^63 refused by both), and a query the planner serves from the
// index (`WHERE d + d = 3`, COVERING(ARITH_D [EQUALS promote(@c AS DOUBLE)]))
// returns no row where the record's own d + d is 3.0. An earlier version of this
// test asserted that Java throws ClassCastException on the first insert; that
// was a source reading the measurement refutes.
//
// Go must write the identical entries: a Go writer and a Java writer share the
// index, so an entry Go computes differently (or refuses to compute) is a wire
// divergence. The two "fixes" this shape invites would both diverge:
//   - validating the numeric lane at DDL rejects a statement Java accepts, so a
//     schema template a Java app creates in a shared cluster becomes uncreatable
//     from Go;
//   - emitting a float-capable evaluator invents a key function Java's catalog
//     does not have, producing index bytes Java cannot read.
func TestFDB_ArithmeticIndex_NonIntegerOperandTruncatesLikeJava(t *testing.T) {
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

	tmpl, err := embedded.BuildSchemaTemplateFromDDL(`CREATE SCHEMA TEMPLATE arith_lane_tpl
		CREATE TABLE T (id bigint, d double, n bigint, PRIMARY KEY(id))
		CREATE INDEX arith_d AS SELECT d + d FROM t`)
	if err != nil {
		t.Fatalf("the DDL must be ACCEPTED — Java accepts it and emits the same "+
			"long-only `add`; rejecting here makes a Java-created schema template "+
			"uncreatable from Go: %v", err)
	}
	md := tmpl.Underlying()
	var idx *recordlayer.Index
	for _, i := range md.GetAllIndexes() {
		if i.Name == "ARITH_D" {
			idx = i
		}
	}
	if idx == nil {
		t.Fatal("index ARITH_D was not built")
	}
	want := recordlayer.FunctionExpr("add", recordlayer.Concat(
		recordlayer.Field("D"), recordlayer.Field("D"))).ToKeyExpression()
	if got := idx.RootExpression.ToKeyExpression(); !proto.Equal(got, want) {
		t.Fatalf("key expression drifted from Java's lowering\n got: %v\nwant: %v", got, want)
	}

	desc := md.GetRecordType("T").Descriptor
	ss := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())
	rows := []struct {
		id int64
		d  float64
	}{{1, 1.5}, {2, -1.5}, {3, 2.0}}
	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).
			SetMetaDataProvider(md).SetSubspace(ss).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		for _, r := range rows {
			m := dynamicpb.NewMessage(desc)
			m.Set(desc.Fields().ByName("ID"), protoreflect.ValueOfInt64(r.id))
			m.Set(desc.Fields().ByName("D"), protoreflect.ValueOfFloat64(r.d))
			m.Set(desc.Fields().ByName("N"), protoreflect.ValueOfInt64(2))
			if _, e := store.SaveRecord(proto.Message(m)); e != nil {
				return nil, e
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("insert FAILED; the target accepts it (getNullableLong takes any Number): %v", err)
	}
	// The index entries: Java's truncated long sums, keyed (sum, primary key), the
	// primary key being (record type key 0, id), in tuple order.
	wantEntries := []tuple.Tuple{
		{int64(-2), int64(0), int64(2)},
		{int64(2), int64(0), int64(1)},
		{int64(4), int64(0), int64(3)},
	}
	_, err = db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).
			SetMetaDataProvider(md).SetSubspace(ss).Open()
		if sErr != nil {
			return nil, sErr
		}
		isub := store.IndexSubspace(idx)
		begin, end := isub.FDBRangeKeys()
		kvs, rErr := rtx.Transaction().GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
		if rErr != nil {
			return nil, rErr
		}
		var got []tuple.Tuple
		for _, kv := range kvs {
			tup, uErr := isub.Unpack(kv.Key)
			if uErr != nil {
				return nil, uErr
			}
			got = append(got, tup)
		}
		if len(got) != len(wantEntries) {
			t.Fatalf("index entries = %v, want %v", got, wantEntries)
		}
		for i := range wantEntries {
			if !reflect.DeepEqual(got[i], wantEntries[i]) {
				t.Fatalf("index entry %d = %v (%T), want %v: DOUBLE operands must truncate toward zero exactly as Java's Number.longValue()", i, got[i], got[i][0], wantEntries[i])
			}
		}
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
