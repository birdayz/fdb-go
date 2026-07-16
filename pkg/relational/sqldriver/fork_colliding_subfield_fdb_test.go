package sqldriver_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/relational/core/embedded"
)

// TestForkDupAliasRejected pins the upstream 42712 rejection of duplicate
// FROM-source aliases — the reason chainedSpineWalk's dup-owner arm is
// defensive (it fails toward name-based resolution), never load-bearing: a
// spine with two links answering to the same alias is not plannable SQL. All
// three variants (link-vs-link, link-vs-table, case-folded) must stay loud;
// if this pin ever breaks, the walk's defensive arm becomes load-bearing and
// needs its own coverage. Plan-only — no store required.
func TestForkDupAliasRejected(t *testing.T) {
	t.Parallel()
	md := buildChainedUnnestMetadata(t)
	for _, tc := range []struct{ name, q string }{
		{"link_vs_link", `SELECT "Z" FROM T4, T4."SARR" AS "X", "X"."SUBSTRUCT" AS "X", "X"."DEEP" AS "Z"`},
		{"link_vs_table", `SELECT "Y" FROM T4, T4."SARR" AS "T4", "T4"."SUB" AS "Y"`},
		{"case_folded", `SELECT "Z" FROM T4, T4."SARR" AS "X", "X"."SUBSTRUCT" AS "x", "x"."DEEP" AS "Z"`},
	} {
		_, perr := embedded.PlanRecordQueryWithMetadata(tc.q, md, nil)
		if perr == nil {
			t.Fatalf("%s: duplicate alias must be rejected upstream (42712); planned OK instead\n  sql: %s", tc.name, tc.q)
		}
		if !strings.Contains(perr.Error(), "42712") {
			t.Fatalf("%s: want 42712 duplicate-alias rejection, got: %v\n  sql: %s", tc.name, perr, tc.q)
		}
	}
}

// TestFDB_ForkCollidingSubfield proves the SILENT-failure case for a fork
// target field that exists on BOTH the owner's element and the mid link's
// element (E1{SUB2, SS[E2{SUB2}]}): a mis-rooted collection — reading the
// PRECEDING link's element where the OWNER's was named — SUCCEEDS
// structurally and returns the WRONG values SILENTLY: Y's SUB2={100} where
// X's SUB2={1,2} was named. Owner-slot rooting must read X's. Verified to
// have teeth at introduction: with owner-slot rooting replaced by
// preceding-link rooting, colliding_fork fails with rows=[map[W:100]].
func TestFDB_ForkCollidingSubfield(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	ctx := context.Background()
	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatal(err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())

	rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	optl := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	i32 := descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum()
	i64 := descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()
	msg := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
	pkg := "fdb.test.forkcollide"
	tn := func(n string) *string { return proto.String("." + pkg + "." + n) }
	fdp := &descriptorpb.FileDescriptorProto{
		Name: proto.String("forkcollide_test.proto"), Package: proto.String(pkg), Syntax: proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("E2"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("SUB2"), Number: proto.Int32(1), Label: rep, Type: i32},
			}},
			{Name: proto.String("E1"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("SUB2"), Number: proto.Int32(1), Label: rep, Type: i32},
				{Name: proto.String("SS"), Number: proto.Int32(2), Label: rep, Type: msg, TypeName: tn("E2")},
			}},
			{Name: proto.String("TC"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("ID"), Number: proto.Int32(1), Label: optl, Type: i64},
				{Name: proto.String("ARR"), Number: proto.Int32(2), Label: rep, Type: msg, TypeName: tn("E1")},
			}},
			{Name: proto.String("UnionDescriptor"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("_TC"), Number: proto.Int32(1), Label: optl, Type: msg, TypeName: tn("TC")},
			}},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatal(err)
	}
	mb := recordlayer.NewRecordMetaDataBuilder().SetRecords(fd)
	mb.GetRecordType("TC").SetPrimaryKey(recordlayer.Field("ID"))
	md, err := mb.Build()
	if err != nil {
		t.Fatal(err)
	}

	tcDesc := md.GetRecordType("TC").Descriptor
	arrFD := tcDesc.Fields().ByName("ARR")
	e1Desc := arrFD.Message()
	ssFD := e1Desc.Fields().ByName("SS")
	e2Desc := ssFD.Message()

	mkE2 := func(sub2 ...int32) protoreflect.Value {
		m := dynamicpb.NewMessage(e2Desc)
		l := m.NewField(e2Desc.Fields().ByName("SUB2")).List()
		for _, v := range sub2 {
			l.Append(protoreflect.ValueOfInt32(v))
		}
		m.Set(e2Desc.Fields().ByName("SUB2"), protoreflect.ValueOfList(l))
		return protoreflect.ValueOfMessage(m)
	}
	mkE1 := func(sub2 []int32, ss ...protoreflect.Value) protoreflect.Value {
		m := dynamicpb.NewMessage(e1Desc)
		l := m.NewField(e1Desc.Fields().ByName("SUB2")).List()
		for _, v := range sub2 {
			l.Append(protoreflect.ValueOfInt32(v))
		}
		m.Set(e1Desc.Fields().ByName("SUB2"), protoreflect.ValueOfList(l))
		sl := m.NewField(ssFD).List()
		for _, s := range ss {
			sl.Append(s)
		}
		m.Set(ssFD, protoreflect.ValueOfList(sl))
		return protoreflect.ValueOfMessage(m)
	}

	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		// X's SUB2 = {1,2}; Y (SS elem)'s SUB2 = {100}. W = X.SUB2 must read
		// {1,2}; the mis-root at Y reads {100} SILENTLY.
		m := dynamicpb.NewMessage(tcDesc)
		m.Set(tcDesc.Fields().ByName("ID"), protoreflect.ValueOfInt64(1))
		al := m.NewField(arrFD).List()
		al.Append(mkE1([]int32{1, 2}, mkE2(100)))
		m.Set(arrFD, protoreflect.ValueOfList(al))
		if _, e := store.SaveRecord(m); e != nil {
			return nil, e
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	want := func(name, q string, exp []string) {
		t.Helper()
		plan, perr := embedded.PlanRecordQueryWithMetadata(q, md, nil)
		if perr != nil {
			t.Fatalf("%s: plan error: %v\n  sql: %s", name, perr, q)
		}
		var out []string
		_, eerr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).Open()
			if sErr != nil {
				return nil, sErr
			}
			cur, cErr := executor.ExecutePlan(ctx, plan, store, executor.EmptyEvaluationContext(), nil, recordlayer.DefaultExecuteProperties())
			if cErr != nil {
				return nil, cErr
			}
			defer cur.Close()
			rows, rErr := executor.CollectAll(ctx, cur)
			if rErr != nil {
				return nil, rErr
			}
			for _, r := range rows {
				out = append(out, fmt.Sprintf("%v", executor.RowValue(r)))
			}
			return nil, nil
		})
		if eerr != nil {
			t.Fatalf("%s: exec error (a mis-rooted fork collection strands here): %v\n  sql: %s", name, eerr, q)
		}
		sort.Strings(out)
		if fmt.Sprintf("%v", out) != fmt.Sprintf("%v", exp) {
			t.Fatalf("%s: rows = %v, want %v (map[W:100] = the mis-root reading Y's SUB2)\n  sql: %s", name, out, exp, q)
		}
	}

	// The colliding fork, unfiltered and filtered: W must carry X's SUB2 {1,2}.
	want("colliding_fork",
		`SELECT "W" FROM TC, TC."ARR" AS "X", "X"."SS" AS "Y", "X"."SUB2" AS "W"`,
		[]string{"map[W:1]", "map[W:2]"})
	want("colliding_fork_filtered",
		`SELECT "W" FROM TC, TC."ARR" AS "X", "X"."SS" AS "Y", "X"."SUB2" AS "W" WHERE TC."ID" = 1`,
		[]string{"map[W:1]", "map[W:2]"})
	// The straddle over the colliding fork: TC.ID = W → ID=1 matches W=1.
	want("colliding_fork_straddle",
		`SELECT "W" FROM TC, TC."ARR" AS "X", "X"."SS" AS "Y", "X"."SUB2" AS "W" WHERE TC."ID" = "W"`,
		[]string{"map[W:1]"})
	// The LINEAR read of the same colliding field name on the MID element must
	// still read Y's OWN SUB2 (owner-slot rooting resolves Y, not X): {100}.
	want("linear_mid_element_read",
		`SELECT "V" FROM TC, TC."ARR" AS "X", "X"."SS" AS "Y", "Y"."SUB2" AS "V"`,
		[]string{"map[V:100]"})
}
