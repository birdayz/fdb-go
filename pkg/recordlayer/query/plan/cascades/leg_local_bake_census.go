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
func recordLegLocalBakeability(leg values.CorrelationIdentifier, legTyp values.Type, column string) {
	legLocalBakeMu.Lock()
	defer legLocalBakeMu.Unlock()
	legLocalBakeCounts.Total++
	rt, isRT := legTyp.(*values.RecordType)
	if !isRT {
		legLocalBakeCounts.UntypedLeg++
		addLegLocalBakeWitness(fmt.Sprintf("UNTYPED-LEG %s.%s (leg type %v)", leg.Name(), column, legTyp))
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
	fmt.Fprintf(&b, "leg-local bakeability: total %d, bakeable %d, untypedLeg %d, columnAbsent %d",
		c.Total, c.Bakeable, c.UntypedLeg, c.ColumnAbsent)
	if len(witnesses) > 0 {
		sorted := append([]string{}, witnesses...)
		sort.Strings(sorted)
		fmt.Fprintf(&b, "\n  witnesses:\n    %s", strings.Join(sorted, "\n    "))
	}
	return b.String()
}
