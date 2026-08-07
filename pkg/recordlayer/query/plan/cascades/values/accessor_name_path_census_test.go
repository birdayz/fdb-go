package values

import (
	"strings"
	"testing"
)

// The accessor-name-path census, driven directly.
//
// A census whose classifier is only reachable through a real planner run is a
// census nobody can show is correct, and its numbers are then quoted as evidence
// anyway. These drive every class from constructed values, with no FDB and no
// planner, so the classification is a tested function rather than an assumption
// riding on 270k observations.
//
// The DECLINE-lazy-dotted arm gets the most attention because it is the one on
// the field-decision ratchet and the one whose corpus count (3 of 269 979) is
// small enough that a classifier bug could hide the entire population.

// accessorCensusOn enables the census for one test and restores it after.
// Serial by necessity: the counters are process-global, which is exactly why the
// classifier is exercised here rather than only through the corpus.
func accessorCensusOn(t *testing.T) {
	t.Helper()
	prev := LegIdentityCensusEnabled()
	SetLegIdentityCensusEnabled(true)
	ResetAccessorPathCensus()
	t.Cleanup(func() {
		ResetAccessorPathCensus()
		SetLegIdentityCensusEnabled(prev)
	})
}

func TestAccessorPathCensus_ClassifiesEveryArm(t *testing.T) {
	root := NewQuantifiedObjectValue(NamedCorrelationIdentifier("q1"))

	for _, tc := range []struct {
		name  string
		build func() Value
		want  AccessorPathClass
		why   string
	}{
		{
			"lazy plain name",
			func() Value { return &FieldValue{Field: "AID", Child: root} },
			AccessorPathOKHasLazy,
			"a lazy accessor's name is the only identity it has",
		},
		{
			"lazy flat-dotted name",
			func() Value { return &FieldValue{Field: "q$1.AID#0", Child: root} },
			AccessorPathDeclineDotted,
			"THE RATCHET ARM: refused rather than split",
		},
		{
			"lazy empty name",
			func() Value { return &FieldValue{Field: "", Child: root} },
			AccessorPathDeclineEmptyName,
			"no name and no resolution is no identity at all",
		},
		{
			"not a column reference",
			func() Value { return root },
			AccessorPathDeclineNotAColumn,
			"the walk reached a root with no accessors",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accessorCensusOn(t)
			AccessorNamePath(tc.build())
			got := AccessorPathCensus()
			if got[tc.want] != 1 {
				t.Fatalf("%s (%s): class %v counted %d, want 1.\nfull vector: %v\n"+
					"A misclassified arm makes every number this census reports about "+
					"that arm meaningless, and the DECLINE-lazy-dotted count in "+
					"particular is small enough (3 in 270k on the corpus) that a "+
					"classifier bug and an empty population look identical.",
					tc.name, tc.why, tc.want, got[tc.want], got)
			}
		})
	}
}

// TestAccessorPathCensus_DottedWitnessNamesTheString pins that the witness set
// records the string VERBATIM.
//
// The name is the entire investigative value of this arm: `addr.city` (a real
// nested path arriving lazy) and `q$1.AID#0` (a rendered Explain label stored as
// a name) are the same class and completely different bugs, and only the string
// separates them. A witness set that normalised, truncated or deduplicated away
// the ordinal suffix would have made the corpus finding unreadable.
func TestAccessorPathCensus_DottedWitnessNamesTheString(t *testing.T) {
	accessorCensusOn(t)
	root := NewQuantifiedObjectValue(NamedCorrelationIdentifier("q1"))
	AccessorNamePath(&FieldValue{Field: "q$50765.AID#0", Child: root})
	AccessorNamePath(&FieldValue{Field: "ADDR.CITY", Child: root})
	AccessorNamePath(&FieldValue{Field: "q$50765.AID#0", Child: root})

	ws := AccessorPathDottedWitnesses()
	if ws["q$50765.AID#0"] != 2 || ws["ADDR.CITY"] != 1 {
		t.Fatalf("witness set = %v, want q$50765.AID#0 x2 and ADDR.CITY x1.\n"+
			"The string is what distinguishes a real nested path arriving lazy from "+
			"a display rendering stored as a name — the same census class, two "+
			"different defects. Losing the exact spelling loses the diagnosis.", ws)
	}
	if origins := AccessorPathDottedOrigins(); len(origins) == 0 {
		t.Fatal("no producer stack was captured for a dotted decline. The class " +
			"count says a display name reached the match-domain identity; only the " +
			"stack says who built it, and with a population this small the stack is " +
			"the whole investigation.")
	}
}

// TestAccessorPathCensus_BakedPathIsNotADecline pins the direction that keeps
// this census honest about the CONVERSION population.
//
// OK-all-baked is the number that says how much of this function's traffic
// already has an ordinal identity available and is being compared by name
// anyway — the RFC-187 §8 population. If a baked path were misclassified as
// lazy, that number would collapse and the conversion would look unnecessary.
func TestAccessorPathCensus_BakedPathIsNotADecline(t *testing.T) {
	accessorCensusOn(t)
	root := NewQuantifiedObjectValue(NamedCorrelationIdentifier("q1"))
	baked := &FieldValue{
		Field:    "AID",
		Child:    root,
		Resolved: &FieldPath{Accessors: []ResolvedAccessor{{Field: "AID", Ordinal: 0}}},
	}
	path, ok := AccessorNamePath(baked)
	if !ok || len(path) != 1 || path[0] != "AID" {
		t.Fatalf("a baked single-accessor path returned (%v, %v), want ([AID], true)", path, ok)
	}
	got := AccessorPathCensus()
	if got[AccessorPathOKAllBaked] != 1 || got[AccessorPathOKHasLazy] != 0 {
		t.Fatalf("baked path classified as %v, want exactly one OK-all-baked.\n"+
			"That class is the count of comparisons that COULD be ordinal-based "+
			"today; misclassifying it as lazy would understate the conversion "+
			"population and read as 'nothing to convert'.", got)
	}
}

// TestAccessorPathCensus_DisabledRecordsNothing pins the gate. A census that
// counted while disabled would tax every plan-time comparison in production and
// would also make the corpus numbers depend on whatever else had run first.
func TestAccessorPathCensus_DisabledRecordsNothing(t *testing.T) {
	prev := LegIdentityCensusEnabled()
	SetLegIdentityCensusEnabled(false)
	ResetAccessorPathCensus()
	t.Cleanup(func() {
		ResetAccessorPathCensus()
		SetLegIdentityCensusEnabled(prev)
	})
	root := NewQuantifiedObjectValue(NamedCorrelationIdentifier("q1"))
	AccessorNamePath(&FieldValue{Field: "q$1.AID#0", Child: root})
	if got := AccessorPathCensus(); got != [accessorPathClassCount]int{} {
		t.Fatalf("the census recorded %v while DISABLED. Production pays one atomic "+
			"load per call and nothing else; counting here would tax every plan-time "+
			"column comparison.", got)
	}
	if ws := AccessorPathDottedWitnesses(); len(ws) != 0 {
		t.Fatalf("witnesses recorded while disabled: %v", ws)
	}
}

// TestAccessorPathTrimKeepsProducerFrames pins that the stack trimmer does not
// eat the frames it exists to surface.
func TestAccessorPathTrimKeepsProducerFrames(t *testing.T) {
	t.Parallel()
	raw := "goroutine 42 [running]:\n" +
		"fdb.dev/pkg/.../values.RecordAccessorPathDottedWitness(...)\n" +
		"\t/src/pkg/recordlayer/query/plan/cascades/values/accessor_name_path_census.go:180 +0x1\n" +
		"fdb.dev/pkg/.../cascades.orderingSatisfiesGroupingKeys(...)\n" +
		"\t/src/pkg/recordlayer/query/plan/cascades/rule_implement_streaming_agg.go:225 +0x93\n"
	got := accessorPathTrim(raw)
	if !strings.Contains(got, "rule_implement_streaming_agg.go:225") {
		t.Fatalf("the trimmer dropped the producer frame: %q\nIt exists to remove the "+
			"census's OWN frames; removing the caller's defeats the point.", got)
	}
	if strings.Contains(got, "accessor_name_path_census.go") {
		t.Fatalf("the trimmer kept the census's own frame: %q", got)
	}
	if strings.Contains(got, "goroutine ") {
		t.Fatalf("the trimmer kept the goroutine header: %q", got)
	}
}
