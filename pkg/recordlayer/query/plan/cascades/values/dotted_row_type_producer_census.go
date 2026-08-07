package values

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// The DOTTED ROW-TYPE PRODUCER census.
//
// It answers ONE question, and it is the question a leg-table population has to
// answer before it can be called safe: **how many code paths derive a RecordType
// for the row whose columns are dotted `LEG.COL` labels, and is
// `RecordConstructorValue.Type()` one of them?**
//
// WHY IT EXISTS. `refineRowTypes` treats a populated leg table against an empty
// one as a CONFLICT and declines the refinement (pinned in
// expressions.TestLegTablePopulation_*). So populating the leg table on a row
// type is only safe if EVERY producer of that row populates it. A plan that says
// "populate it at the seed's own derivation and leave
// `RecordConstructorValue.Type()` untouched" is therefore making a claim about
// the producer set — that the generic constructor path is not one of them — and
// that claim was asserted rather than measured.
//
// The DISCRIMINATOR is the column NAMES, not the construction site, and that is
// deliberate: the hazard is two paths deriving the SAME ROW, so the row has to be
// identified by what it looks like rather than by who built it. A row is "dotted"
// here when at least one field name carries a qualifier — the shape
// `clustered_outer_scalar.go` mints (`leg.binding + "." + COL`, and
// `UPPER(innerAlias) + "." + scalarCol`), which is exactly the shape the
// executor's dotted leg-column reader answers on.
//
// The dot test excludes RENDERED COMPOSITE names for the reason
// executor.isDottedQualifiedName states: a rendered record type contains the dots
// of every qualified column inside it (`{_0: C.ID#0}`), so a bare dot test would
// classify an ordinary one-column leg as dotted.
//
// GATED by LegIdentityCensusEnabled, like every census on this path. Counts are
// per Type() derivation, so read the totals as traffic and the WITNESSES as the
// answer.

const dottedRowTypeWitnessCap = 64

var (
	dottedRowTypeMu        sync.Mutex
	dottedRowTypeDotted    int
	dottedRowTypePlain     int
	dottedRowTypeWitnesses []string
)

// nameIsQualified reports whether a field name carries a dotted qualifier, as
// opposed to merely containing a dot. Mirrors executor.isDottedQualifiedName —
// a rendered composite is delimited, a qualifier never is.
func nameIsQualified(name string) bool {
	if strings.HasPrefix(name, "{") || strings.HasPrefix(name, "[") {
		return false
	}
	i := strings.IndexByte(name, '.')
	return i > 0 && i < len(name)-1
}

// RecordDottedRowTypeDerivation counts ONE RecordType derivation by the generic
// record-constructor path, cut by whether the row it describes is the DOTTED
// `LEG.COL` shape. Callers must guard on LegIdentityCensusEnabled().
func RecordDottedRowTypeDerivation(fields []Field) {
	dotted := false
	for i := range fields {
		if nameIsQualified(fields[i].Name) {
			dotted = true
			break
		}
	}
	dottedRowTypeMu.Lock()
	defer dottedRowTypeMu.Unlock()
	if !dotted {
		dottedRowTypePlain++
		return
	}
	dottedRowTypeDotted++
	if len(dottedRowTypeWitnesses) >= dottedRowTypeWitnessCap {
		return
	}
	names := make([]string, 0, len(fields))
	for i := range fields {
		names = append(names, fields[i].Name)
	}
	w := "[" + strings.Join(names, " ") + "]"
	for _, seen := range dottedRowTypeWitnesses {
		if seen == w {
			return
		}
	}
	dottedRowTypeWitnesses = append(dottedRowTypeWitnesses, w)
}

// DottedRowTypeProducerCensus reports the counts and the distinct dotted row
// shapes the generic path derived.
func DottedRowTypeProducerCensus() (dotted, plain int, witnesses []string) {
	dottedRowTypeMu.Lock()
	defer dottedRowTypeMu.Unlock()
	out := make([]string, len(dottedRowTypeWitnesses))
	copy(out, dottedRowTypeWitnesses)
	return dottedRowTypeDotted, dottedRowTypePlain, out
}

// ResetDottedRowTypeProducerCensus clears the counters.
func ResetDottedRowTypeProducerCensus() {
	dottedRowTypeMu.Lock()
	defer dottedRowTypeMu.Unlock()
	dottedRowTypeDotted, dottedRowTypePlain, dottedRowTypeWitnesses = 0, 0, nil
}

// FormatDottedRowTypeProducerCensus renders the census for a harness to log.
func FormatDottedRowTypeProducerCensus() string {
	dotted, plain, witnesses := DottedRowTypeProducerCensus()
	var b strings.Builder
	fmt.Fprintf(&b, "dotted row-type producers (RecordConstructorValue.Type derivations): "+
		"DOTTED %d, plain %d", dotted, plain)
	if len(witnesses) == 0 {
		b.WriteString("\n  distinct dotted row shapes: NONE — the generic constructor path " +
			"derives no LEG.COL-shaped row")
		return b.String()
	}
	sorted := append([]string{}, witnesses...)
	sort.Strings(sorted)
	fmt.Fprintf(&b, "\n  distinct dotted row shapes (%d, cap %d):\n    %s",
		len(sorted), dottedRowTypeWitnessCap, strings.Join(sorted, "\n    "))
	return b.String()
}

// DottedRowTypeProducerFloor is the minimum number of derivations this census
// must see over a whole run.
//
// The finding this census can produce is a ZERO on the DOTTED counter, and a
// zero over an unreached instrument is indistinguishable from a zero over a
// measured-clean one. `plain` is what floors it: `RecordConstructorValue.Type()`
// is called constantly by any query that constructs a record, so a run reporting
// no derivations at all is reporting a broken counter.
type DottedRowTypeProducerFloor struct {
	Derivations int
}

// AssertDottedRowTypeProducerCensus checks the floor and reports the finding.
//
// It does NOT hard-zero the DOTTED counter. A non-zero is not a defect today —
// it is the answer to a design question, and the answer determines WHERE a leg
// table must be attached, not whether the tree is broken. Failing the build on it
// would convert a measurement into a veto on a change nobody has made yet.
func AssertDottedRowTypeProducerCensus(w io.Writer, floor *DottedRowTypeProducerFloor) bool {
	dotted, plain, _ := DottedRowTypeProducerCensus()
	if floor == nil || floor.Derivations == 0 {
		return false
	}
	if dotted+plain >= floor.Derivations {
		return false
	}
	fmt.Fprintf(w, "DOTTED ROW-TYPE PRODUCER CENSUS FAIL: %d derivation(s) over the whole\n"+
		"  run, want >= %d.\n"+
		"  This census's usable finding is a ZERO on its DOTTED counter — the claim that\n"+
		"  the generic RecordConstructorValue.Type path never derives a LEG.COL-shaped\n"+
		"  row, and therefore is not a second producer of the row a leg-table population\n"+
		"  would target. An unreached counter reports that zero vacuously.\n"+
		"  WHAT A COLLAPSE RE-ARMS: the producer-set claim behind any plan to populate\n"+
		"  RecordType.Legs at one derivation site while leaving this path alone.\n"+
		"  refineRowTypes DECLINES a populated table against an empty one, so a second\n"+
		"  unpopulated producer is a plan-level conflict, not a missed optimisation.\n",
		dotted+plain, floor.Derivations)
	return true
}
