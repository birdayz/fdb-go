package sqldriver_test

import (
	"context"
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
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/core/embedded"
)

// redundantReaderAlias is the leg alias whose binder-produced reads the two proofs
// in this package perturb.
//
// It is the corpus's ONLY reader: over a full sqldriver run, every lookup that
// resolves to a bindMergedOuterLegs window is a read of this alias, on the
// correlated-EXISTS inner-shadow shape built below.
//
// The alias alone is NOT what the census gate excludes. The exclusion is
// registered under the full read identity — this alias plus the merged row's leg
// layout, measured by each proof rather than spelled out — because `ST` is a plain
// table name and an exclusion keyed on it would excuse any future multi-leg read
// of anything so named, load-bearing or not.
const redundantReaderAlias = "ST"

// mergedLegReaderShapeWant is the answer, stated independently of any route
// through the binder: the EXISTS folds constant-true, so all three ST×OT pairs
// survive. These are the rows TestFDB_ExistsInnerShadow pins as the live Java
// 4.12.11.0 behaviour.
var mergedLegReaderShapeWant = []string{"K=50", "K=50", "K=50"}

// mergedLegReaderShape is a live store and plan for the ONE merged-row reader
// shape the corpus produces — a faithful reproduction of
// `foldable_colliding_answers` in TestFDB_ExistsInnerShadow: same descriptors,
// same rows, same query, same planning entry point.
type mergedLegReaderShape struct {
	db   *recordlayer.FDBDatabase
	md   *recordlayer.RecordMetaData
	ks   subspace.Subspace
	plan plans.RecordQueryPlan
	sql  string
}

// newMergedLegReaderShape builds that store and plan under a subspace private to
// the calling test.
//
// It is ONE authority because two proofs perturb this shape differently — the
// redundancy pin declines the binder's window, the wrong-window instrument aims it
// at a sibling leg — and each registers its result under the merged-row LAYOUT it
// measured. Two hand-maintained copies of the fixture can drift into two different
// layouts while both tests stay green, and the drift is silent: each proof would
// go on excusing a shape the other never ran, which is precisely the alias-vs-shape
// confusion the census's keying exists to prevent.
//
// The shape had to be MEASURED rather than guessed, and that is why the fixture is
// this specific query. The obvious cousin (`colliding_plain`, the same `FROM ST,
// OT` outer with a genuinely-read correlated predicate) binds the identical
// windows and reads NONE of them. The difference is the FOLD: `COALESCE(1,
// ST."C")` folds constant, so the colliding reference never survives into the join
// predicate and the inner is planned as its own two-source merge — the arrangement
// that ends up consulting the outer leg's binding. A pin on the cousin would have
// passed while testing nothing, which is why both callers assert their reads
// happened.
//
// `FROM ST, OT` is the two-leg merged outer row (ST's three columns at [0,3), OT's
// two at [3,5)).
func newMergedLegReaderShape(ctx context.Context, t *testing.T) *mergedLegReaderShape {
	t.Helper()

	fdb.MustAPIVersion(730)
	rawDB, err := fdb.OpenDatabase(clusterFilePath)
	if err != nil {
		t.Fatal(err)
	}
	db := recordlayer.NewFDBDatabase(rawDB)
	ks := subspace.FromBytes(tuple.Tuple{t.Name()}.Pack())

	rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	optl := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	i64 := descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()
	msg := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
	pkg := "fdb.test.mlbredundancy"
	tn := func(n string) *string { return proto.String("." + pkg + "." + n) }
	fdp := &descriptorpb.FileDescriptorProto{
		Name: proto.String("mlbredundancy_test.proto"), Package: proto.String(pkg), Syntax: proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("ST"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("ID"), Number: proto.Int32(1), Label: optl, Type: i64},
				{Name: proto.String("C"), Number: proto.Int32(2), Label: optl, Type: i64},
				{Name: proto.String("ARR"), Number: proto.Int32(3), Label: rep, Type: i64},
			}},
			{Name: proto.String("OT"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("ID"), Number: proto.Int32(1), Label: optl, Type: i64},
				{Name: proto.String("K"), Number: proto.Int32(2), Label: optl, Type: i64},
			}},
			{Name: proto.String("UnionDescriptor"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("_ST"), Number: proto.Int32(1), Label: optl, Type: msg, TypeName: tn("ST")},
				{Name: proto.String("_OT"), Number: proto.Int32(2), Label: optl, Type: msg, TypeName: tn("OT")},
			}},
		},
	}
	fd, fErr := protodesc.NewFile(fdp, nil)
	if fErr != nil {
		t.Fatal(fErr)
	}
	mb := recordlayer.NewRecordMetaDataBuilder().SetRecords(fd)
	mb.GetRecordType("ST").SetPrimaryKey(recordlayer.Field("ID"))
	mb.GetRecordType("OT").SetPrimaryKey(recordlayer.Field("ID"))
	md, mErr := mb.Build()
	if mErr != nil {
		t.Fatal(mErr)
	}

	stDesc := md.GetRecordType("ST").Descriptor
	otDesc := md.GetRecordType("OT").Descriptor
	stRow := func(id, c int64, arr ...int64) *dynamicpb.Message {
		m := dynamicpb.NewMessage(stDesc)
		m.Set(stDesc.Fields().ByName("ID"), protoreflect.ValueOfInt64(id))
		m.Set(stDesc.Fields().ByName("C"), protoreflect.ValueOfInt64(c))
		vals := make([]protoreflect.Value, 0, len(arr))
		for _, v := range arr {
			vals = append(vals, protoreflect.ValueOfInt64(v))
		}
		setArrayField(m, stDesc.Fields().ByName("ARR"), vals...)
		return m
	}
	otRow := dynamicpb.NewMessage(otDesc)
	otRow.Set(otDesc.Fields().ByName("ID"), protoreflect.ValueOfInt64(1000))
	otRow.Set(otDesc.Fields().ByName("K"), protoreflect.ValueOfInt64(50))

	if _, err := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).
			SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
		if sErr != nil {
			return nil, sErr
		}
		for _, m := range []*dynamicpb.Message{
			stRow(1, 100, 10, 200), stRow(2, 5, 20, 300), stRow(3, 1000, 4), otRow,
		} {
			if _, e := store.SaveRecord(m); e != nil {
				return nil, e
			}
		}
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}

	const q = `SELECT OT."K" FROM ST, OT WHERE EXISTS ` +
		`(SELECT 1 FROM OT AS "OI", ST WHERE COALESCE(1, ST."C") = 1)`

	plan, perr := embedded.PlanRecordQueryWithMetadata(q, md, nil)
	if perr != nil {
		t.Fatalf("plan %q: %v", q, perr)
	}
	return &mergedLegReaderShape{db: db, md: md, ks: ks, plan: plan, sql: q}
}

// soleRead returns the single entry of a one-entry tally, or the zero value when
// the tally does not have exactly one.
func soleRead(tally map[executor.MergedRowRead]int) (executor.MergedRowRead, int) {
	if len(tally) != 1 {
		return executor.MergedRowRead{}, 0
	}
	for k, n := range tally {
		return k, n
	}
	return executor.MergedRowRead{}, 0
}
