package cascades

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The LEG-LOCAL BAKEABILITY census.
//
// It measures ONE question, at the one site that still packs a leg into a column
// name: when `rebaseOuterLegValue`'s leg-match arm re-anchors a leg-correlated
// read onto the merge correlation as `QOV(merged)."LEG.COL"`, could that read
// instead have been baked LEG-LOCALLY — `ofOrdinal(QOV(leg), i)` over the leg's
// own row — and left on its own alias?
//
// Why the question decides something. Java never re-anchors: its FlatMap binds one
// correlation per quantifier (RecordQueryFlatMapPlan.java:135-140 over the parent
// chain at Bindings.java:116-134), so a leg-correlated read resolves as a plain
// map lookup plus an ordinal (QuantifiedObjectValue.java:84-85) and no name is
// consulted. Go's two-level NLJ→FlatMap lowering bound only the join's own alias,
// which is what made the qualified name necessary. Once each leg of the merged row
// binds under its own correlation, the re-anchoring has an alternative — but only
// for reads whose leg row type is known and whose column resolves in it. This
// census counts exactly that, so the channel's retirement rests on a measurement
// of the live traffic rather than on the shape of the one arm that is easy to read.
//
// It is GATED (LegIdentityCensusEnabled) like the leg-identity census it sits
// beside: the site is inside a Cascades rule, so its totals count RULE FIRINGS,
// not queries — the memo may explore one rule once or many times per query. Read
// the absolute numbers as a planning artifact; the ratio of bakeable to
// unbakeable is the fact about the corpus.
const legLocalBakeWitnessCap = 24

type legLocalBakeCounters struct {
	// Total is every firing of the leg-match arm.
	Total int
	// Bakeable: the leg QOV states a record row type AND the read's column
	// resolves in it, so `ofOrdinal(QOV(leg), i)` is constructible.
	Bakeable int
	// UntypedLeg: the leg QOV flows no record type, so no leg-local ordinal can
	// be resolved. These are the reads that would have to DECLINE.
	UntypedLeg int
	// UnderivableLegs counts LEGS (not reads) whose physical plan states no row
	// layout at all, so every read correlated to them falls through.
	UnderivableLegs int
	// ColumnAbsent: the leg is typed but the read's column is not one of its
	// declared columns — a read whose name does not name a column of the leg it
	// claims to come from.
	ColumnAbsent int
}

var (
	legLocalBakeMu        sync.Mutex
	legLocalBakeCounts    legLocalBakeCounters
	legLocalBakeWitnesses []string
)

// recordLegLocalBakeability classifies one firing of the leg-match arm.
//
// legTyp is the leg quantifier's flowed type as the reference carries it, and
// column is the read's column name as the arm would have folded it into the
// qualified key. Deliberately takes the SAME inputs the arm itself has in hand, so
// the census cannot answer a different question than the one the arm decides.
// available names the leg layouts the caller could derive from the legs' physical
// plans. It is carried into the UNTYPED-LEG witness because the interesting case
// is not "no layouts existed" but "layouts existed and this leg was not among
// them" — a reference correlated to an alias no chosen leg plan carries, which is
// a different defect from a leg whose plan yields no row type.
func recordLegLocalBakeability(leg values.CorrelationIdentifier, legTyp values.Type, column string, available ...string) {
	legLocalBakeMu.Lock()
	defer legLocalBakeMu.Unlock()
	legLocalBakeCounts.Total++
	rt, isRT := legTyp.(*values.RecordType)
	if !isRT {
		legLocalBakeCounts.UntypedLeg++
		addLegLocalBakeWitness(fmt.Sprintf("UNTYPED-LEG %s.%s (leg type %v; derivable leg layouts %v)", leg.Name(), column, legTyp, available))
		return
	}
	if _, found := rt.FieldIndex(column); !found {
		legLocalBakeCounts.ColumnAbsent++
		addLegLocalBakeWitness(fmt.Sprintf("COLUMN-ABSENT %s.%s (leg columns %v)", leg.Name(), column, recordTypeFieldNames(rt)))
		return
	}
	legLocalBakeCounts.Bakeable++
}

// addLegLocalBakeWitness retains distinct witnesses up to the cap. Caller holds
// the mutex.
func addLegLocalBakeWitness(w string) {
	if len(legLocalBakeWitnesses) >= legLocalBakeWitnessCap {
		return
	}
	for _, seen := range legLocalBakeWitnesses {
		if seen == w {
			return
		}
	}
	legLocalBakeWitnesses = append(legLocalBakeWitnesses, w)
}

// legTypeOrUntyped picks the layout the census should classify a fallen-through
// read against: the leg's PHYSICAL row type when the caller could derive one (the
// read is then classified COLUMN-ABSENT — a name that does not name a column of
// the leg it claims), otherwise the reference's own untyped quantifier object (the
// read is UNTYPED-LEG — no layout existed at all). Distinguishing the two is the
// whole point: they are different residues with different fixes.
func legTypeOrUntyped(legType *values.RecordType, haveLegType bool, refType values.Type) values.Type {
	if haveLegType && legType != nil {
		return legType
	}
	return refType
}

// recordUnderivableLegLayout names a leg whose physical plan states no row
// layout, by the plan's own shape.
//
// This is the residue's cause rather than its symptom. The bakeability counters
// say how many reads still need the qualified name; this says WHY — which plan
// shapes the layout derivation has no arm for. Kept separate from the counters
// because it is per-LEG, not per-READ: one underivable leg accounts for every
// read correlated to it.
func recordUnderivableLegLayout(alias values.CorrelationIdentifier, plan any) {
	legLocalBakeMu.Lock()
	defer legLocalBakeMu.Unlock()
	legLocalBakeCounts.UnderivableLegs++
	addLegLocalBakeWitness(fmt.Sprintf("NO-LAYOUT leg %s: plan %T states no row layout", alias.Name(), plan))
}

func recordTypeFieldNames(rt *values.RecordType) []string {
	out := make([]string, len(rt.Fields))
	for i, f := range rt.Fields {
		out[i] = f.Name
	}
	return out
}

// LegLocalBakeCensus reports the counters and the retained witnesses.
//
// A run in which Total > 0 and Bakeable == Total says the qualified-name channel
// is carrying nothing the leg-local bake could not carry, which is what licenses
// deleting it. A nonzero UntypedLeg or ColumnAbsent names the shapes that would
// have to decline instead, and the witnesses identify them.
func LegLocalBakeCensus() (legLocalBakeCounters, []string) {
	legLocalBakeMu.Lock()
	defer legLocalBakeMu.Unlock()
	out := make([]string, len(legLocalBakeWitnesses))
	copy(out, legLocalBakeWitnesses)
	return legLocalBakeCounts, out
}

// FormatLegLocalBakeCensus renders the census for a harness to log.
func FormatLegLocalBakeCensus() string {
	c, witnesses := LegLocalBakeCensus()
	var b strings.Builder
	fmt.Fprintf(&b, "leg-local bakeability: total %d, bakeable %d, untypedLeg %d, columnAbsent %d, underivableLegs %d",
		c.Total, c.Bakeable, c.UntypedLeg, c.ColumnAbsent, c.UnderivableLegs)
	if len(witnesses) > 0 {
		sorted := append([]string{}, witnesses...)
		sort.Strings(sorted)
		fmt.Fprintf(&b, "\n  witnesses:\n    %s", strings.Join(sorted, "\n    "))
	}
	return b.String()
}

func legLocalTypeKeys(m map[values.CorrelationIdentifier]*values.RecordType) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k.Name())
	}
	sort.Strings(out)
	return out
}
