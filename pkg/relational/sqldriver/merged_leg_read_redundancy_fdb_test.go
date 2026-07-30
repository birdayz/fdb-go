package sqldriver_test

import (
	"context"
	"fmt"
	"sort"
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
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/embedded"
)

// redundantReaderAlias is the leg alias whose binder-produced reads this test
// proves redundant, and which the activation gate therefore excludes.
//
// It is the corpus's ONLY reader: over a full sqldriver run, every lookup that
// resolves to a bindMergedOuterLegs window is a read of this alias, on the
// correlated-EXISTS inner-shadow shape reproduced below.
const redundantReaderAlias = "ST"

// TestFDB_MergedLegBinding_ReaderShapeIsRedundant is the LICENSE for the
// activation criterion's one exclusion, and it is a license rather than a note
// because it is a proof that re-runs.
//
// # What it licenses
//
// The merged-leg binding census alarms when a read resolves to a
// bindMergedOuterLegs window on a MULTI-LEG merged row while no leg-local bake
// produced anything (assertMergedLegBindingCensus). The corpus contains such
// reads today — three over a full run, all on the alias above, from the shape
// below. Without an exclusion the gate is permanently red and says nothing; with
// an exclusion resting on prose it is an assertion nobody checks.
//
// So the exclusion is granted HERE, by measurement: the same query is run down
// BOTH resolution routes in the SAME process — once with the binder's window
// serving the read, once with that window bypassed for this alias — and the rows
// must agree. Agreement is what "not load-bearing" MEANS. Registration happens
// only on the passing path, and the gate's exclusion IS that registration, so a
// divergence fails this test AND turns those same three reads into a red gate in
// the same run. The license cannot outlive its proof.
//
// # Why a context-scoped bypass and not a neuter
//
// A build-tagged or edited-out binder removes the path from the whole binary, so
// the two routes never coexist and cannot be compared — and the comparison is the
// whole content of this test. EvaluationContext.WithMergedLegReadBypass scopes the
// bypass to ONE execution, which also keeps it out of the concurrently running
// tests that share these table names.
//
// # The shape, which had to be MEASURED rather than guessed
//
// A faithful reproduction of `foldable_colliding_answers` in
// TestFDB_ExistsInnerShadow — same descriptors, same rows, same query, same
// planning entry point. That is the shape the corpus's reads come from, and it is
// not the one this test was first written against: the obvious cousin
// (`colliding_plain`, the same `FROM ST, OT` outer with a genuinely-read
// correlated predicate) binds the identical windows and reads NONE of them. The
// difference is the FOLD — `COALESCE(1, ST."C")` folds constant, so the colliding
// reference never survives into the join predicate and the inner is planned as
// its own two-source merge, which is the arrangement that ends up consulting the
// outer leg's binding.
//
// The activeReads guard below exists because of that: a pin on a plausible cousin
// would have passed, licensed the exclusion, and tested nothing.
//
// `FROM ST, OT` is the two-leg merged outer row (ST's three columns at [0,3),
// OT's two at [3,5)). The EXISTS is constant-true, so all three ST×OT pairs
// survive and the answer is K=50 three times.
func TestFDB_MergedLegBinding_ReaderShapeIsRedundant(t *testing.T) {
	t.Parallel()
	if clusterFilePath == "" {
		t.Skip("FDB not available (no Docker)")
	}
	if !values.LegIdentityCensusEnabled() {
		t.Fatal("the leg-identity census gate is OFF, so the bypass is not honoured " +
			"and both runs below take the SAME route — the comparison would pass " +
			"vacuously and license an exclusion it never tested. The sqldriver " +
			"TestMain enables it for the whole run (runUnderLegIdentityCensus).")
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
		l := m.NewField(stDesc.Fields().ByName("ARR")).List()
		for _, v := range arr {
			l.Append(protoreflect.ValueOfInt64(v))
		}
		m.Set(stDesc.Fields().ByName("ARR"), protoreflect.ValueOfList(l))
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

	// run executes the plan, optionally bypassing the binder's window for the
	// alias under test, and reports the rows plus how many binder reads occurred.
	run := func(bypass bool) ([]string, int) {
		t.Helper()
		before := executor.MergedRowLegReads()[redundantReaderAlias]
		var out []string
		if _, eerr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).
				SetMetaDataProvider(md).SetSubspace(ks).Open()
			if sErr != nil {
				return nil, sErr
			}
			evalCtx := executor.EmptyEvaluationContext()
			if bypass {
				evalCtx = evalCtx.WithMergedLegReadBypass(redundantReaderAlias)
			}
			cur, cErr := executor.ExecutePlan(ctx, plan, store, evalCtx, nil,
				recordlayer.DefaultExecuteProperties())
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
		}); eerr != nil {
			t.Fatalf("exec (bypass=%v) %q: %v", bypass, q, eerr)
		}
		sort.Strings(out)
		return out, executor.MergedRowLegReads()[redundantReaderAlias] - before
	}

	// ROUTE A: the binder's window serves the read.
	active, activeReads := run(false)

	// The shape must actually exercise the binder, or both routes are the same
	// route and their agreement licenses nothing. The census is process-global and
	// the suite is parallel, so this is a DELTA across this execution, not a total.
	if activeReads <= 0 {
		t.Fatalf("this shape produced NO read of a bindMergedOuterLegs window for %q "+
			"(delta over the run with the binder active: %d).\n"+
			"  The exclusion this test licenses is for reads that no longer happen\n"+
			"  here, so it is licensing nothing: the gate would go on excusing the\n"+
			"  corpus's reads on the strength of a comparison that never touched them.\n"+
			"  Re-derive the reader shape from the census before the exclusion stands.\n"+
			"  sql: %s\n  plan: %s", redundantReaderAlias, activeReads, q, plan.Explain())
	}

	// ROUTE B: the binder's window is declined for this alias, so the reference
	// resolves however it resolves without it.
	bypassed, bypassedReads := run(true)
	if bypassedReads != 0 {
		t.Fatalf("the bypass did not take: %d read(s) of %q still resolved to a "+
			"binder window while it was bypassed.\n"+
			"  Both runs therefore took the SAME route and their agreement proves "+
			"nothing. The bypass rides EvaluationContext, so the likely cause is an "+
			"execution path that builds a FRESH context instead of copying this one.",
			bypassedReads, redundantReaderAlias)
	}

	// The correct answer, stated independently of either route: the EXISTS folds
	// constant-true, so every ST×OT pair survives. These are the rows
	// TestFDB_ExistsInnerShadow pins as the live Java 4.12.11.0 answer.
	want := []string{"map[K:50]", "map[K:50]", "map[K:50]"}
	if fmt.Sprint(active) != fmt.Sprint(want) {
		t.Fatalf("with the binder ACTIVE the rows are wrong: %v, want %v\n  sql: %s\n  plan: %s",
			active, want, q, plan.Explain())
	}

	// THE PROOF. Two routes, one answer.
	if fmt.Sprint(active) != fmt.Sprint(bypassed) {
		t.Fatalf("THE TWO RESOLUTION ROUTES DISAGREE on %q.\n"+
			"  binder window active:   %v\n"+
			"  binder window bypassed: %v\n\n"+
			"  THIS REVOKES AN EXCLUSION. assertMergedLegBindingCensus excuses the "+
			"corpus's reads of %q from the activation criterion, and the entire "+
			"license for that exclusion is this test: that the binder's window is a "+
			"redundant second route to an answer something else already gives.\n\n"+
			"  Disagreement means it is now LOAD-BEARING. Do not relax this "+
			"assertion. The exclusion must come out of the gate, the binder's "+
			"correctness becomes a real invariant (the wrong-window mutation that is "+
			"green today must be made to fail), and the shadowing and "+
			"first-claim-wins semantics in DIVERGENCES.md stop being unobservable "+
			"and need justifying against this consumer.\n  sql: %s\n  plan: %s",
			redundantReaderAlias, active, bypassed, redundantReaderAlias, q, plan.Explain())
	}

	// Registered only on the passing path: the gate's exclusion IS this
	// registration, so any failure above removes it in the same run.
	executor.RegisterRedundantMergedLegReader(redundantReaderAlias, t.Name())
}
