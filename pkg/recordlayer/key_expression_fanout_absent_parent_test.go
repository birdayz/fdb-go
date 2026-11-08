package recordlayer

import (
	"context"
	"encoding/binary"
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/simfdb"
)

// absentParentFanOutFile is Rec{id, p: P{a, tags, n}, tags, ps: repeated P}: a
// singular parent p over a composite with a FanOut child, a top-level tags
// the expression does not read, and a repeated parent ps.
func absentParentFanOutFile(t *testing.T) protoreflect.FileDescriptor {
	t.Helper()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	field := func(name string, n int32, label *descriptorpb.FieldDescriptorProto_Label, typ descriptorpb.FieldDescriptorProto_Type, typeName string) *descriptorpb.FieldDescriptorProto {
		f := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(n), Label: label, Type: typ.Enum()}
		if typeName != "" {
			f.TypeName = proto.String(typeName)
		}
		return f
	}
	str, i64, msg := descriptorpb.FieldDescriptorProto_TYPE_STRING, descriptorpb.FieldDescriptorProto_TYPE_INT64, descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
	fd, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
		Name: proto.String("absent_parent_fanout_test.proto"), Package: proto.String("apf"), Syntax: proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("P"), Field: []*descriptorpb.FieldDescriptorProto{
				field("a", 1, opt, str, ""), field("tags", 2, rep, str, ""), field("n", 3, opt, i64, ""),
			}},
			{Name: proto.String("Rec"), Field: []*descriptorpb.FieldDescriptorProto{
				field("id", 1, opt, i64, ""), field("p", 2, opt, msg, ".apf.P"), field("tags", 3, rep, str, ""),
				field("ps", 4, rep, msg, ".apf.P"),
			}},
			{Name: proto.String("RecordTypeUnion"), Field: []*descriptorpb.FieldDescriptorProto{field("_Rec", 1, opt, msg, ".apf.Rec")}},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return fd
}

// absentParentRec builds a Rec; p is set only when pSet, holding a and pTags.
func absentParentRec(fd protoreflect.FileDescriptor, id int64, pSet bool, pTags, tags []string) *dynamicpb.Message {
	recD, pD := fd.Messages().ByName("Rec"), fd.Messages().ByName("P")
	m := dynamicpb.NewMessage(recD)
	m.Set(recD.Fields().ByName("id"), protoreflect.ValueOfInt64(id))
	if pSet {
		p := dynamicpb.NewMessage(pD)
		p.Set(pD.Fields().ByName("a"), protoreflect.ValueOfString("x"))
		list := p.Mutable(pD.Fields().ByName("tags")).List()
		for _, tag := range pTags {
			list.Append(protoreflect.ValueOfString(tag))
		}
		m.Set(recD.Fields().ByName("p"), protoreflect.ValueOfMessage(p))
	}
	list := m.Mutable(recD.Fields().ByName("tags")).List()
	for _, tag := range tags {
		list.Append(protoreflect.ValueOfString(tag))
	}
	return m
}

// absentParentFanOutExpr is the shape whose result under an absent parent
// changed from earlier Go builds (DIVERGENCES.md, "A fan-out composite under
// an absent parent").
func absentParentFanOutExpr() KeyExpression {
	return Nest("p", Concat(Field("a"), FanOut("tags")))
}

// A composite with a FanOut child, evaluated under an absent singular parent,
// yields no tuples, as in Java 4.14.2.0: NestingKeyExpression.java:77-84
// evaluates the child on the parent's null value, FieldKeyExpression.java:179-180
// answers a null message with getNullResult(), which is Collections.emptyList()
// for FanOut (:229-232), and ThenKeyExpression.java:90-98 crosses the children,
// so one empty child empties the product. Earlier Go evaluated the FanOut child
// as a scalar on the null message and yielded one all-null tuple. A set parent
// is the control.
func TestCompositeFanOutUnderAbsentParentYieldsNoTuples(t *testing.T) {
	t.Parallel()
	fd := absentParentFanOutFile(t)
	expr := absentParentFanOutExpr()
	for _, tc := range []struct {
		name string
		msg  *dynamicpb.Message
		want [][]any
	}{
		{"p unset, no tags", absentParentRec(fd, 1, false, nil, nil), nil},
		{"p unset, record tags", absentParentRec(fd, 1, false, nil, []string{"t0", "t1"}), nil},
		{"p set, no tags", absentParentRec(fd, 1, true, nil, []string{"t0"}), nil},
		{"p set with tags", absentParentRec(fd, 1, true, []string{"t0", "t1"}, nil), [][]any{{"x", "t0"}, {"x", "t1"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := expr.Evaluate(asStored(tc.msg), tc.msg)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 0 || len(tc.want) != 0 {
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("Nest(p, Concat(a, FanOut(tags))) evaluated to %v, want %v", got, tc.want)
				}
			}
		})
	}
	// The composite itself on the null message the absent parent passes it.
	got, err := Concat(Field("a"), FanOut("tags")).Evaluate(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("composite with a FanOut child on a nil message evaluated to %v, want no tuples", got)
	}
}

// On a nil message every child of a composite contributes its getNullResult
// (FieldKeyExpression.java:229-240), whichever evaluator the composite uses
// for it: a Concatenate field under NOT_NULL the empty list, packed as an
// empty nested tuple, not null; the others null. Earlier Go took the null.
func TestCompositeOnNilMessageCrossesEachChildsNullResult(t *testing.T) {
	t.Parallel()
	notNullList := FieldWithNullStandin("tags", FanTypeConcatenate, NullStandinNotNull)
	want := [][]any{{nil, tuple.Tuple{}, nil}}
	for name, expr := range map[string]KeyExpression{
		"composite":             Concat(Field("a"), notNullList, FieldConcatenate("tags")),
		"under absent parent p": Nest("p", Concat(Field("a"), notNullList, FieldConcatenate("tags"))),
	} {
		m := absentParentRec(absentParentFanOutFile(t), 1, false, nil, []string{"t0"})
		var msg proto.Message = m
		if name == "composite" {
			msg = nil
		}
		got, err := expr.Evaluate(asStored(m), msg)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s on a nil message evaluated to %#v, want %#v", name, got, want)
		}
	}
}

// Saving a record whose parent is absent writes no entry for such an index,
// with or without the record's own tags; a record whose parent holds tags
// writes one per tag, and updating it to an absent parent clears them.
func TestCompositeFanOutUnderAbsentParentWritesNoIndexEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fd := absentParentFanOutFile(t)
	builder := NewRecordMetaDataBuilder().SetRecords(fd)
	builder.GetRecordType("Rec").SetPrimaryKey(Field("id"))
	idx := NewIndex("Rec$p_a_tags", absentParentFanOutExpr())
	builder.AddIndex("Rec", idx)
	md, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	env := dst.NewSim(1035)
	env.Buggify = dst.DisabledBuggifier()
	db := NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
	sub := subspace.FromBytes(tuple.Tuple{"absent_parent_fanout"}.Pack())

	entries := func(t *testing.T, save ...proto.Message) []tuple.Tuple {
		t.Helper()
		got, err := db.Run(ctx, func(rctx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().SetContext(rctx).SetMetaDataProvider(md).SetSubspace(sub).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for _, rec := range save {
				if _, err := store.SaveRecord(rec); err != nil {
					return nil, err
				}
			}
			begin, end := store.IndexSubspace(idx).FDBRangeKeys()
			kvs, err := rctx.Transaction().GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
			if err != nil {
				return nil, err
			}
			var keys []tuple.Tuple
			for _, kv := range kvs {
				key, err := store.IndexSubspace(idx).Unpack(kv.Key)
				if err != nil {
					return nil, err
				}
				keys = append(keys, key)
			}
			return keys, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		keys, _ := got.([]tuple.Tuple)
		return keys
	}

	if got := entries(t,
		absentParentRec(fd, 1, false, nil, []string{"t0", "t1"}),
		absentParentRec(fd, 2, false, nil, nil),
	); len(got) != 0 {
		t.Fatalf("records with p unset wrote index entries %v, want none", got)
	}
	want := []tuple.Tuple{{"x", "t0", int64(3)}, {"x", "t1", int64(3)}}
	if got := entries(t, absentParentRec(fd, 3, true, []string{"t0", "t1"}, nil)); !reflect.DeepEqual(got, want) {
		t.Fatalf("record with p's tags wrote index entries %v, want %v", got, want)
	}
	if got := entries(t, absentParentRec(fd, 3, false, nil, []string{"t0"})); len(got) != 0 {
		t.Fatalf("after updating p to unset, index entries %v, want none", got)
	}
}

// withPs sets m's ps to one P per value of a, the ith with n = 10(i+1).
func withPs(fd protoreflect.FileDescriptor, m *dynamicpb.Message, as ...string) *dynamicpb.Message {
	recD, pD := fd.Messages().ByName("Rec"), fd.Messages().ByName("P")
	list := m.Mutable(recD.Fields().ByName("ps")).List()
	for i, a := range as {
		p := dynamicpb.NewMessage(pD)
		p.Set(pD.Fields().ByName("a"), protoreflect.ValueOfString(a))
		p.Set(pD.Fields().ByName("n"), protoreflect.ValueOfInt64(int64(10*(i+1))))
		list.Append(protoreflect.ValueOfMessage(p))
	}
	return m
}

// Counting indexes over these shapes count as Java's
// AtomicMutationIndexMaintainer does (AtomicMutationIndexMaintainer.java:125-147):
// one mutation per index entry. A record whose expression yields no entry (p
// absent, or ps empty) is counted under no key, not under () nor (null, null),
// and one yielding k entries is counted k times: COUNT and COUNT_UPDATES
// over the absent-parent composite, and COUNT over a fan-out nesting grouped
// by its element's field or by nothing; a SUM over the nesting adds every
// element. Insert, an online indexer rebuild, and delete (which COUNT_UPDATES
// ignores) agree.
func TestCountIndexesOverFanOutCountEveryEntry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fd := absentParentFanOutFile(t)
	indexes := []*Index{
		NewCountIndex("Rec$count_p", GroupAll(absentParentFanOutExpr())),
		NewCountUpdatesIndex("Rec$count_updates_p", GroupAll(absentParentFanOutExpr())),
		NewCountIndex("Rec$count_ps", GroupAll(NestFanOut("ps", Field("a")))),
		NewCountIndex("Rec$count_all_ps", GroupAll(NestFanOut("ps", EmptyKey()))),
		NewSumIndex("Rec$sum_ps", Ungrouped(NestFanOut("ps", Field("n")))),
	}
	builder := NewRecordMetaDataBuilder().SetRecords(fd)
	builder.GetRecordType("Rec").SetPrimaryKey(Field("id"))
	for _, idx := range indexes {
		builder.AddIndex("Rec", idx)
	}
	md, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	env := dst.NewSim(1036)
	env.Buggify = dst.DisabledBuggifier()
	db := NewFDBDatabaseWithBackend(simfdb.New(env)).SetEnv(env)
	sub := subspace.FromBytes(tuple.Tuple{"absent_parent_fanout_count"}.Pack())

	// run runs fn on the store and returns every index's counts by key.
	run := func(t *testing.T, what string, fn func(store *FDBRecordStore) error) map[string]map[string]int64 {
		t.Helper()
		got := map[string]map[string]int64{}
		if _, err := db.Run(ctx, func(rctx *FDBRecordContext) (any, error) {
			clear(got)
			store, err := NewStoreBuilder().SetContext(rctx).SetMetaDataProvider(md).SetSubspace(sub).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			if err := fn(store); err != nil {
				return nil, err
			}
			for _, idx := range indexes {
				// From the bare prefix: the key of group (), which a range
				// over the subspace's tuples does not hold.
				_, end := store.IndexSubspace(idx).FDBRangeKeys()
				begin := fdb.Key(store.IndexSubspace(idx).Bytes())
				kvs, err := rctx.Transaction().GetRange(fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
				if err != nil {
					return nil, err
				}
				counts := map[string]int64{}
				for _, kv := range kvs {
					key, err := store.IndexSubspace(idx).Unpack(kv.Key)
					if err != nil {
						return nil, err
					}
					counts[key.String()] = int64(binary.LittleEndian.Uint64(kv.Value))
				}
				got[idx.Name] = counts
			}
			return nil, nil
		}); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		return got
	}
	check := func(t *testing.T, what string, got, want map[string]map[string]int64) {
		t.Helper()
		for _, idx := range indexes {
			if !reflect.DeepEqual(got[idx.Name], want[idx.Name]) {
				t.Errorf("%s: %s counts %v, want %v", what, idx.Name, got[idx.Name], want[idx.Name])
			}
		}
	}

	// Entries: 1 has ps (z); 2 has none; 3 has p's (x, t0) and (x, t1) and
	// ps (x), (x), (y). Four ps entries over three records, two p entries;
	// the ps entries' n sum to 10 + 10 + 20 + 30.
	records := []proto.Message{
		withPs(fd, absentParentRec(fd, 1, false, nil, []string{"t0"}), "z"),
		absentParentRec(fd, 2, false, nil, nil),
		withPs(fd, absentParentRec(fd, 3, true, []string{"t0", "t1"}, nil), "x", "x", "y"),
	}
	inserted := map[string]map[string]int64{
		"Rec$count_p":         {`("x", "t0")`: 1, `("x", "t1")`: 1},
		"Rec$count_updates_p": {`("x", "t0")`: 1, `("x", "t1")`: 1},
		"Rec$count_ps":        {`("x")`: 2, `("y")`: 1, `("z")`: 1},
		"Rec$count_all_ps":    {`()`: 4},
		"Rec$sum_ps":          {`()`: 70},
	}
	check(t, "after insert", run(t, "insert", func(store *FDBRecordStore) error {
		for _, rec := range records {
			if _, err := store.SaveRecord(rec); err != nil {
				return err
			}
		}
		return nil
	}), inserted)

	run(t, "clear", func(store *FDBRecordStore) error {
		for _, idx := range indexes {
			if _, err := store.ClearAndMarkIndexWriteOnly(idx.Name); err != nil {
				return err
			}
		}
		return nil
	})
	for _, idx := range indexes {
		indexer, err := NewOnlineIndexerBuilder().SetDatabase(db).SetMetaData(md).SetIndex(idx).SetSubspace(sub).Build()
		if err != nil {
			t.Fatalf("online indexer for %s: %v", idx.Name, err)
		}
		if _, err := indexer.BuildIndex(ctx); err != nil {
			t.Fatalf("rebuild %s: %v", idx.Name, err)
		}
	}
	check(t, "after an online rebuild", run(t, "read", func(*FDBRecordStore) error { return nil }), inserted)

	check(t, "after delete", run(t, "delete", func(store *FDBRecordStore) error {
		for id := int64(1); id <= 3; id++ {
			if _, err := store.DeleteRecord(tuple.Tuple{id}); err != nil {
				return err
			}
		}
		return nil
	}), map[string]map[string]int64{
		"Rec$count_p":         {`("x", "t0")`: 0, `("x", "t1")`: 0},
		"Rec$count_updates_p": {`("x", "t0")`: 1, `("x", "t1")`: 1},
		"Rec$count_ps":        {`("x")`: 0, `("y")`: 0, `("z")`: 0},
		"Rec$count_all_ps":    {`()`: 0},
		"Rec$sum_ps":          {`()`: 0},
	})
}
