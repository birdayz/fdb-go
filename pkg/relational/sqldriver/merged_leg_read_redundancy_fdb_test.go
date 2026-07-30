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
// proves redundant.
//
// It is the corpus's ONLY reader: over a full sqldriver run, every lookup that
// resolves to a bindMergedOuterLegs window is a read of this alias, on the
// correlated-EXISTS inner-shadow shape reproduced below.
//
// The alias alone is NOT what the gate excludes. The exclusion is registered
// under the full read identity — this alias plus the merged row's leg layout,
// measured here rather than spelled out — because `ST` is a plain table name and
// an exclusion keyed on it would excuse any future multi-leg read of anything so
// named, load-bearing or not.
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
// below; this test's own active route adds three more of the same shape, so a
// full run reports six. Without an exclusion the gate is permanently red and says
// nothing; with an exclusion resting on prose it is an assertion nobody checks.
//
// So the exclusion is granted HERE, by measurement, and it is registered under the
// read's FULL identity — alias plus the merged row's leg layout — so it excuses
// the shape that was proven and not every future read of a name as common as `ST`.
//
// # WHICH GUARDS CARRY THE LICENSE — read this before strengthening anything
//
// The load-bearing guards are, in order:
//
//   - route A read a binder window, on EXACTLY ONE merged-row shape. Without a
//     read the shape has stopped reaching the binder and every comparison below
//     is between two identical routes; this is the guard that caught the first
//     draft of this test, which pinned a plausible cousin (`colliding_plain`)
//     that binds the same windows and reads none of them. Without "exactly one"
//     the registration covers a proper subset of what the run measured.
//   - route B read NO binder window. Proves the bypass actually took; otherwise
//     both runs are the same run.
//   - the bypass was a MISS, on every declined lookup. The window shadows nothing
//     here, so declining it leaves the alias resolving to NOTHING — the strictly
//     strongest perturbation available, and the one whose survival means the
//     binding is not carrying the answer. If the binder ever starts SHADOWING on
//     this shape the bypass silently weakens into "compare two live routes", so it
//     is asserted rather than assumed.
//   - want, stated independently of both routes. Route A is checked against the
//     answer TestFDB_ExistsInnerShadow pins as live Java 4.12.11.0 behaviour, not
//     merely against route B.
//
// The route-A-vs-route-B row comparison is NOT one of them, on this shape. It is
// VACUOUS here and saying otherwise would be the failure this file exists to
// prevent. `COALESCE(1, ST."C")` folds to a constant at plan time, so the value
// behind the alias never reaches an answer: no perturbation of the binding — a
// miss, the wrong window, every window at offset 0 — can move a row. Both of the
// mutations this invites were run and both leave the test GREEN: binding every
// window at offset 0, and rotating each leg's alias onto its SIBLING's window.
// That is the expected outcome of a folded shape, not a hole to be plugged by
// tightening the comparison. The comparison is kept because it is what
// GENERALISES: on any future shape where the value survives the fold, a binding
// that resolves to nothing and changes rows fails here first, and the guard costs
// nothing.
//
// The four guards above DO mutate. Neutering the binder (`return ec`) fails the
// first; ignoring the bypass fails the second; making the binder shadow on this
// shape fails the third.
//
// What the exclusion therefore rests on is the conjunction: the read happens, the
// binding is the alias's ONLY route, removing it entirely still yields the rows
// Java gives. Registration happens only on the passing path, and the gate's
// exclusion IS that registration, so any of the above failing turns these same
// reads into a red gate in the same run. The license cannot outlive its proof.
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
	// alias under test, and reports the rows plus a sink holding what THIS
	// execution read.
	//
	// The sink, not a before/after delta over the process-global census: the census
	// is process-wide and this suite is parallel, so a delta also counts whatever
	// TestFDB_ExistsInnerShadow — which reads the same alias out of the same merged
	// shape — happened to read in the same window. Every guard below is an exact
	// count of one execution.
	run := func(bypass bool) ([]string, *executor.MergedLegReadSink) {
		t.Helper()
		sink := executor.NewMergedLegReadSink()
		var out []string
		if _, eerr := db.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, sErr := recordlayer.NewStoreBuilder().SetContext(rtx).
				SetMetaDataProvider(md).SetSubspace(ks).Open()
			if sErr != nil {
				return nil, sErr
			}
			evalCtx := executor.EmptyEvaluationContext().WithMergedLegReadSink(sink)
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
		return out, sink
	}

	// ROUTE A: the binder's window serves the read.
	active, activeSink := run(false)

	// GUARD 1. The shape must actually exercise the binder, or both routes are the
	// same route and nothing below licenses anything. It must exercise it on
	// EXACTLY ONE merged-row shape, because that shape is what gets registered:
	// two shapes and the registration would cover one of them while the gate went
	// on seeing the other.
	activeReads := activeSink.Reads()
	proven, provenReads := soleRead(activeReads)
	if proven.Alias != redundantReaderAlias || provenReads <= 0 {
		t.Fatalf("this execution did not produce exactly one multi-leg reader shape for %q.\n"+
			"  reads this execution made: %v\n"+
			"  The exclusion this test licenses is registered under the ONE shape it\n"+
			"  proved. Zero reads means it is licensing nothing — the gate would go on\n"+
			"  excusing the corpus's reads on the strength of a comparison that never\n"+
			"  touched them. More than one means the registration covers a proper subset\n"+
			"  of what this run measured. Re-derive the reader shape from the census\n"+
			"  before the exclusion stands.\n  sql: %s\n  plan: %s",
			redundantReaderAlias, activeReads, q, plan.Explain())
	}

	// ROUTE B: the binder's window is declined for this alias, so the reference
	// resolves however it resolves without it.
	bypassed, bypassedSink := run(true)

	// GUARD 2. The bypass took: no read of this alias resolved to a binder window
	// while it was declined.
	if bypassedReads := bypassedSink.Reads(); len(bypassedReads) != 0 {
		t.Fatalf("the bypass did not take: read(s) of %q still resolved to a "+
			"binder window while it was bypassed (%v).\n"+
			"  Both runs therefore took the SAME route and their agreement proves "+
			"nothing. The bypass rides EvaluationContext, so the likely cause is an "+
			"execution path that builds a FRESH context instead of copying this one.",
			redundantReaderAlias, bypassedReads)
	}

	// GUARD 3, AND THE ONE THAT CARRIES THE CLAIM. What the declined lookups got
	// back was NOTHING: the window shadows nothing on this shape, so route B is the
	// alias resolving to no binding at all — the strongest perturbation available,
	// not merely a swap to a second live route. It is asserted because it can
	// change under us: the day the binder starts shadowing here, `w.shadowed` is
	// handed back instead and the comparison below quietly becomes a much weaker
	// claim while staying green.
	misses, handoffs := bypassedSink.BypassOutcomes()
	if misses[proven] <= 0 || len(handoffs) != 0 {
		t.Fatalf("route B was not the alias resolving to NOTHING for %q.\n"+
			"  declined lookups that resolved to nothing: %v\n"+
			"  declined lookups handed a SHADOWED binding: %v\n"+
			"  Zero misses means the bypass never reached the lookup this test aims\n"+
			"  at, so route B is not the route it claims to be. A non-empty handoff\n"+
			"  means the binder now SHADOWS on this shape: route B stopped being\n"+
			"  \"the binding is gone\" and became \"the binding came from somewhere\n"+
			"  else\", which is a strictly weaker thing to survive and does not\n"+
			"  license the exclusion this test grants.",
			redundantReaderAlias, misses, handoffs)
	}

	// The correct answer, stated independently of either route: the EXISTS folds
	// constant-true, so every ST×OT pair survives. These are the rows
	// TestFDB_ExistsInnerShadow pins as the live Java 4.12.11.0 answer.
	want := []string{"map[K:50]", "map[K:50]", "map[K:50]"}
	if fmt.Sprint(active) != fmt.Sprint(want) {
		t.Fatalf("with the binder ACTIVE the rows are wrong: %v, want %v\n  sql: %s\n  plan: %s",
			active, want, q, plan.Explain())
	}

	// GUARD 4. The alias resolving to NOTHING left the rows unchanged.
	//
	// On THIS shape that is guaranteed by the fold and the comparison cannot fail:
	// `COALESCE(1, ST."C")` is constant before execution, so no state of the
	// binding — right window, wrong window, or no binding at all — is reachable
	// from a row. Both mutations this invites (every window at offset 0; each leg's
	// alias rotated onto its sibling's window) were run and leave this GREEN, which
	// is the expected outcome of a folded shape rather than a gap.
	//
	// It stays because it is the assertion that GENERALISES. Guards 1-3 establish
	// that the read happens and that route B removed the binding entirely; this one
	// says what that removal cost, and on any future shape whose value survives the
	// fold it is the assertion that fails first.
	if fmt.Sprint(active) != fmt.Sprint(bypassed) {
		t.Fatalf("REMOVING THE BINDING CHANGED THE ROWS for %q.\n"+
			"  binder window active:   %v\n"+
			"  binder window bypassed (alias resolves to NOTHING): %v\n\n"+
			"  THIS REVOKES AN EXCLUSION. assertMergedLegBindingCensus excuses the "+
			"corpus's reads of %q out of this merged shape from the activation "+
			"criterion, and the license is this test: the binding is the alias's only "+
			"route AND deleting it still yields the answer Java gives.\n\n"+
			"  A difference means the value behind the binding now reaches an answer, "+
			"so the binder is LOAD-BEARING. Do not relax this assertion. The exclusion "+
			"must come out of the gate, the binder's correctness becomes a real "+
			"invariant (the wrong-window mutation that is green today must be made to "+
			"fail), and the shadowing and first-claim-wins semantics in DIVERGENCES.md "+
			"stop being unobservable and need justifying against this consumer.\n"+
			"  sql: %s\n  plan: %s",
			redundantReaderAlias, active, bypassed, redundantReaderAlias, q, plan.Explain())
	}

	// Registered only on the passing path, and only for the SHAPE this run proved:
	// the gate's exclusion IS this registration, so any failure above removes it in
	// the same run, and a read of this alias out of any other merged row is not
	// covered by it.
	executor.RegisterRedundantMergedLegReader(proven, t.Name())
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
