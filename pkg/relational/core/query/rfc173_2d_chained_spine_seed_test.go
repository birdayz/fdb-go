package query

// RFC-173 S4 Slice 2d — white-box pins for the chained-spine ordinal boundary.
//
// The ordinal-vs-name-model choice is INVISIBLE at the SQL level for chained
// unnests: plans, rows, result-column labels, and (dual-emitted) Datum keys are
// identical between the two models, and even the outer-filter scan SARG is
// model-independent (the ⊆-outerLegs lazy path precedes the seed-form fork in
// rebaseChainedOuterLegPredicate). So — exactly like rfc173_w4c_unnest_seed_test.go
// — these pins assert the SEED FORM directly off translateChainedUnnestJoin's
// result: an admitted spine's select carries a raw (non-AnchoredJoin) ordinal RC;
// a declined shape falls open to the name-model AnchoredJoin record. Feature-off
// control: with the chainedBaseOrdinalizes recursion arm removed (depth-2 gate),
// the 3/4-link ordinal pins here FAIL — verified once at introduction; these are
// discriminating by construction, unlike a rows/EXPLAIN cert.
//
// The FDB certs (rfc173_s4_3link_filtered_ordinal_fdb_test.go and the chained
// ordinal cert) stay the rows+correctness pins; THESE pin the boundary.

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/types/descriptorpb"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// newChainedSpineTranslator builds a translator over a catalog with the nested
// array shapes the chained-spine pins need: T4 mirrors the FDB certs' schema
// (SARR: ELEM{SUB[], K, SUBSTRUCT: ELEM2{DEEP[], LEAF}} + a shadow scalar SUB),
// and T carries a 4-level nesting (A: AElem{B: BElem{C: CElem{V[], LEAF}}}).
func newChainedSpineTranslator(t *testing.T) *cascadesTranslator {
	t.Helper()
	rep := descriptorpb.FieldDescriptorProto_LABEL_REPEATED.Enum()
	opt := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL.Enum()
	i32 := descriptorpb.FieldDescriptorProto_TYPE_INT32.Enum()
	i64 := descriptorpb.FieldDescriptorProto_TYPE_INT64.Enum()
	msg := descriptorpb.FieldDescriptorProto_TYPE_MESSAGE.Enum()
	pkg := "fdb.test.spine2d"
	tn := func(name string) *string { return proto.String("." + pkg + "." + name) }
	fdp := &descriptorpb.FileDescriptorProto{
		Name:    proto.String("spine2d_test.proto"),
		Package: proto.String(pkg),
		Syntax:  proto.String("proto2"),
		MessageType: []*descriptorpb.DescriptorProto{
			{Name: proto.String("ELEM2"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("DEEP"), Number: proto.Int32(1), Label: rep, Type: i32},
				{Name: proto.String("LEAF"), Number: proto.Int32(2), Label: opt, Type: i64},
			}},
			{Name: proto.String("ELEM"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("SUB"), Number: proto.Int32(1), Label: rep, Type: i32},
				{Name: proto.String("K"), Number: proto.Int32(2), Label: opt, Type: i64},
				{Name: proto.String("SUBSTRUCT"), Number: proto.Int32(3), Label: rep, Type: msg, TypeName: tn("ELEM2")},
			}},
			{Name: proto.String("T4"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("ID"), Number: proto.Int32(1), Label: opt, Type: i64},
				{Name: proto.String("SARR"), Number: proto.Int32(2), Label: rep, Type: msg, TypeName: tn("ELEM")},
				{Name: proto.String("SUB"), Number: proto.Int32(3), Label: opt, Type: i64},
			}},
			{Name: proto.String("CElem"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("V"), Number: proto.Int32(1), Label: rep, Type: i32},
				{Name: proto.String("LEAF"), Number: proto.Int32(2), Label: opt, Type: i64},
			}},
			{Name: proto.String("BElem"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("C"), Number: proto.Int32(1), Label: rep, Type: msg, TypeName: tn("CElem")},
			}},
			{Name: proto.String("AElem"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("B"), Number: proto.Int32(1), Label: rep, Type: msg, TypeName: tn("BElem")},
			}},
			{Name: proto.String("T"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("ID"), Number: proto.Int32(1), Label: opt, Type: i64},
				{Name: proto.String("A"), Number: proto.Int32(2), Label: rep, Type: msg, TypeName: tn("AElem")},
			}},
			{Name: proto.String("UnionDescriptor"), Field: []*descriptorpb.FieldDescriptorProto{
				{Name: proto.String("_T4"), Number: proto.Int32(1), Label: opt, Type: msg, TypeName: tn("T4")},
				{Name: proto.String("_T"), Number: proto.Int32(2), Label: opt, Type: msg, TypeName: tn("T")},
			}},
		},
	}
	fd, err := protodesc.NewFile(fdp, nil)
	if err != nil {
		t.Fatalf("NewFile: %v", err)
	}
	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(fd)
	builder.GetRecordType("T4").SetPrimaryKey(recordlayer.Field("ID"))
	builder.GetRecordType("T").SetPrimaryKey(recordlayer.Field("ID"))
	md, err := builder.Build()
	if err != nil {
		t.Fatalf("build metadata: %v", err)
	}
	return &cascadesTranslator{
		md:              md,
		cteScope:        make(map[string]logical.LogicalOperator),
		cteExprScope:    make(map[string]expressions.RelationalExpression),
		cteColumnsScope: make(map[string][]values.Field),
	}
}

// link appends one lateral-unnest link to a spine.
func link(left logical.LogicalOperator, owner, field, alias string) (*logical.LogicalJoin, *logical.LogicalUnnest) {
	u := &logical.LogicalUnnest{Segments: []string{owner, field}, Alias: alias}
	return inner(left, u), u
}

// seedForm drives the REAL chained dispatch and classifies the resulting
// select's seed: "ordinal" (raw non-AnchoredJoin RC), "name-model" (AnchoredJoin
// record), or "nil" (no select — loud error or non-chained shape).
func seedForm(t *testing.T, tr *cascadesTranslator, j *logical.LogicalJoin, u *logical.LogicalUnnest) string {
	t.Helper()
	sel := tr.translateChainedUnnestJoin(j, u, false)
	if sel == nil {
		return "nil"
	}
	rv, ok := sel.(interface{ GetResultValue() values.Value })
	if !ok {
		t.Fatalf("select %T has no GetResultValue", sel)
	}
	rc, ok := rv.GetResultValue().(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("result value is %T, want *RecordConstructorValue", rv.GetResultValue())
	}
	if rc.AnchoredJoin {
		return "name-model"
	}
	return "ordinal"
}

// TestRFC173S4_2d_ChainedSpineSeedForm pins the seed-form boundary the 2d gate
// lift opened: LINEAR spines ordinalize at ANY depth, filtered or not; forks,
// box bases, and malformed links decline to the name-model record. Each case
// names the boundary condition it pins.
func TestRFC173S4_2d_ChainedSpineSeedForm(t *testing.T) {
	t.Parallel()

	// Spine builders (fresh trees per case — the translator mutates nothing,
	// but sharing nodes across subtests invites accidental coupling).
	linear3 := func() (*logical.LogicalJoin, *logical.LogicalUnnest) {
		l1, _ := link(scan("T4", "T4"), "T4", "SARR", "X")
		l2, _ := link(l1, "X", "SUBSTRUCT", "Y")
		return link(l2, "Y", "DEEP", "Z")
	}
	linear4 := func() (*logical.LogicalJoin, *logical.LogicalUnnest) {
		l1, _ := link(scan("T", "T"), "T", "A", "a")
		l2, _ := link(l1, "a", "B", "b")
		l3, _ := link(l2, "b", "C", "c")
		return link(l3, "c", "V", "v")
	}
	linear2 := func() (*logical.LogicalJoin, *logical.LogicalUnnest) {
		l1, _ := link(scan("T4", "T4"), "T4", "SARR", "X")
		return link(l1, "X", "SUB", "Y")
	}
	topFork := func() (*logical.LogicalJoin, *logical.LogicalUnnest) {
		// W's owner is X, TWO links back (not the preceding Y): the shape that
		// malformed-planned on the first 2d cut (ordinal -1) and would be
		// SILENTLY WRONG with colliding field names.
		l1, _ := link(scan("T4", "T4"), "T4", "SARR", "X")
		l2, _ := link(l1, "X", "SUBSTRUCT", "Y")
		return link(l2, "X", "SUB", "W")
	}
	midFork := func() (*logical.LogicalJoin, *logical.LogicalUnnest) {
		// The fork sits ONE level below the dispatching link: Z↔Y2 is linear,
		// but Y2's owner X is not the preceding Y — a walk over firstBase alone
		// (skipping firstUnnest) would admit it.
		l1, _ := link(scan("T4", "T4"), "T4", "SARR", "X")
		l2, _ := link(l1, "X", "SUBSTRUCT", "Y")
		l3, _ := link(l2, "X", "SUBSTRUCT", "Y2")
		return link(l3, "Y2", "DEEP", "Z")
	}
	twinFork := func() (*logical.LogicalJoin, *logical.LogicalUnnest) {
		// TWO iterations of the SAME array (X, X2), the sub-unnest owned by the
		// FIRST: mis-rooting at X2's element would read the SAME-NAMED SUB off
		// the wrong iteration variable — the silent-wrong-rows variant.
		l1, _ := link(scan("T4", "T4"), "T4", "SARR", "X")
		l2, _ := link(l1, "T4", "SARR", "X2")
		return link(l2, "X", "SUB", "Y")
	}
	boxBase3 := func() (*logical.LogicalJoin, *logical.LogicalUnnest) {
		box := inner(scan("T4", "T4"), scan("T4", "T4C"))
		l1, _ := link(box, "T4", "SARR", "X")
		l2, _ := link(l1, "X", "SUBSTRUCT", "Y")
		return link(l2, "Y", "DEEP", "Z")
	}
	oneSegMid := func() (*logical.LogicalJoin, *logical.LogicalUnnest) {
		// A malformed 1-segment unnest mid-spine (constructible via the
		// AT-source parser path) — the mid-spine Segments<2 check.
		l1 := inner(scan("T4", "T4"), &logical.LogicalUnnest{Segments: []string{"SARR"}, Alias: "X"})
		l2, _ := link(l1, "X", "SUBSTRUCT", "Y")
		return link(l2, "Y", "DEEP", "Z")
	}

	cases := []struct {
		name     string
		build    func() (*logical.LogicalJoin, *logical.LogicalUnnest)
		filtered bool   // simulate the WHERE-merge context (outer-ref conjunct present)
		want     string // seed form
		wantErr  string // required translateErr substring when want == "nil"
	}{
		// The 2d win: linear spines ordinalize at any depth.
		{"linear2_unfiltered", linear2, false, "ordinal", ""},
		{"linear3_unfiltered", linear3, false, "ordinal", ""},
		{"linear4_unfiltered", linear4, false, "ordinal", ""},
		// The corrective-commit core: the box-leg-conjunct decline arm is scoped
		// to BOX bases, so a FILTERED spine still ordinalizes. On the first 2d
		// cut these three were name-model (the certs' "ordinalizes" claim was
		// false attribution) — this is the pin that would have caught it.
		{"linear2_filtered", linear2, true, "ordinal", ""},
		{"linear3_filtered", linear3, true, "ordinal", ""},
		{"linear4_filtered", linear4, true, "ordinal", ""},
		// Fork declines — correct-or-declined, never a mis-rooted ordinal seed.
		{"top_fork_unfiltered", topFork, false, "name-model", ""},
		{"top_fork_filtered", topFork, true, "name-model", ""},
		{"mid_spine_fork", midFork, false, "name-model", ""},
		// Box base declines (c5b territory) — filtered AND unfiltered: the arm
		// scoping must NOT leak the bypass to a genuine box.
		{"box_base_unfiltered", boxBase3, false, "name-model", ""},
		{"box_base_filtered", boxBase3, true, "name-model", ""},
		// TWO iterations of the SAME table-rooted array are rejected LOUDLY by
		// the pre-existing multiple-lateral-unnests guard UPSTREAM of the
		// chained gate — so the twin-fork silent-wrong hazard (mis-rooting the
		// same-named SUB at the wrong iteration variable) is unreachable via
		// SQL. Pinned as loud-not-silent; if that guard is ever lifted, this
		// case must flip to "name-model" (the linearity check declines it).
		{"twin_fork_same_array", twinFork, false, "nil", "multiple lateral array unnests"},
		// A malformed 1-segment link mid-spine dies LOUDLY in chained
		// classification (chainedOwnerElementMessage requires 2 segments) before
		// the gate ever runs — the walk's own Segments<2 check is defense in
		// depth behind that, pinned directly in the walk test below.
		{"one_segment_mid_spine", oneSegMid, false, "nil", "not yet supported"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newChainedSpineTranslator(t)
			tr.unnestOuterConjunctOnBoxLeg = tc.filtered
			j, u := tc.build()
			if got := seedForm(t, tr, j, u); got != tc.want {
				t.Fatalf("seed form = %s, want %s (translateErr: %v)", got, tc.want, tr.translateErr)
			}
			if tc.wantErr == "" {
				return
			}
			// A "nil" case must be LOUD (correct-or-loud): the translate error
			// names the upstream rejection, never a silent no-plan.
			if tr.translateErr == nil || !strings.Contains(tr.translateErr.Error(), tc.wantErr) {
				t.Fatalf("translateErr = %v, want substring %q", tr.translateErr, tc.wantErr)
			}
		})
	}
}

// TestRFC173S4_2d_ChainedBaseOrdinalizes_Walk pins the gate walk itself —
// the recursion's accept/decline per structural shape, independent of seed
// building.
func TestRFC173S4_2d_ChainedBaseOrdinalizes_Walk(t *testing.T) {
	t.Parallel()
	tr := newChainedSpineTranslator(t)

	l1, _ := link(scan("T4", "T4"), "T4", "SARR", "X")
	l2, _ := link(l1, "X", "SUBSTRUCT", "Y")
	l3, _ := link(l2, "Y", "DEEP", "Z")
	forkL2, _ := link(l1, "T4", "SARR", "X2")
	forkAtY, _ := link(forkL2, "X", "SUB", "Y") // Y's owner X is not the preceding X2
	box := inner(scan("T4", "T4"), scan("T4", "T4C"))
	boxL1, _ := link(box, "T4", "SARR", "X")

	cases := []struct {
		name string
		op   logical.LogicalOperator
		want bool
	}{
		{"single_scan", scan("T4", "T4"), true},
		{"one_link_spine", l1, true},
		{"two_link_spine", l2, true},
		{"three_link_spine", l3, true},
		{"fork_in_spine", forkAtY, false},
		{"box_base", box, false},
		{"box_under_first_link", boxL1, false},
		{"non_join_non_scan", &logical.LogicalUnnest{Segments: []string{"T4", "SARR"}}, false},
	}
	for _, tc := range cases {
		if got := tr.chainedBaseOrdinalizes(tc.op); got != tc.want {
			t.Errorf("%s: chainedBaseOrdinalizes = %v, want %v", tc.name, got, tc.want)
		}
	}
}
