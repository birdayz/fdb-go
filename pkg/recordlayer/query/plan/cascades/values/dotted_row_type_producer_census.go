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
// the producer set — that the generic constructor path is not one of them.
//
// THE ANSWER, MEASURED. That claim is false, and the counter says so: DOTTED is
// NOT zero. Two full-corpus runs read DOTTED 683 (plain 157511) and DOTTED 841
// (plain 157935), and the witness shapes (`[K C.CV]`, `[AID K C.CV]`,
// `[TID K C.C.CV]`, `[ID NAME O.I.QTY]`) carry exactly the labels the executor's
// dotted arm answers on. `RecordConstructorValue.Type()` IS a producer of the
// dotted row. The integer is corpus-traffic dependent and both readings are
// legitimate; the load-bearing fact is "not zero".
//
// WHAT THAT SETTLES. It does not make the population unsafe — it relocates it.
// The seed sites do not derive this row themselves; they build
// `RecordConstructorField{Name: "LEG.COL"}` values and the only thing that turns
// a constructor into a `RecordType` is `Type()`. So the producer set for the
// seed's row is size ONE and this method is it (RFC-212 §3.5), which makes
// §3.4's every-producer precondition satisfiable by construction — PROVIDED the
// population is attached HERE rather than at a seed-side derivation. RFC-212
// §1.1 was restated on exactly this measurement: carry the leg table on the
// constructor VALUE and propagate it through `Type()`.
//
// Nothing in the tree populates `RecordType.Legs` today, so no populated-vs-empty
// pair exists for `legTablesAgree` to decline on: §11.3's retitling sets `Fields`
// only (`scalar_subquery_seed.go`'s `innerType`), and `legTablesAgree` compares
// lengths first, so empty-vs-empty agrees trivially. The precondition becomes
// load-bearing the moment any change populates `Legs` at a derivation site — at
// which point this path must be populated in the SAME change, or `refineRowTypes`
// will decline the refinement.
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
// The floor guards the INSTRUMENT, not the finding. The finding is already in:
// DOTTED is non-zero (683 and 841 over two full-corpus runs), so this path is a
// producer of the dotted row. What the floor still protects is the reading
// itself — a run reporting no derivations at all is reporting a broken counter,
// not a quiet corpus. `plain` is what floors it: `RecordConstructorValue.Type()`
// is called constantly by any query that constructs a record, and both readings
// clear a floor of 100 by three orders of magnitude, which is the floor working.
type DottedRowTypeProducerFloor struct {
	Derivations int
}

// AssertDottedRowTypeProducerCensus checks the floor and reports the finding.
//
// It does NOT hard-zero the DOTTED counter, and the measurement is why that is
// the right shape rather than a concession: DOTTED came back non-zero (683 and
// 841). A non-zero is not a defect — it is the answer to a design question, and
// the answer determines WHERE a leg table must be attached (at `Type()`, the one
// producer), not whether the tree is broken. Asserting on it would convert a
// measurement into a veto on a change nobody has made yet.
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
		"  This census has already returned its finding: DOTTED is NOT zero (683 and 841\n"+
		"  over two full-corpus runs), so RecordConstructorValue.Type IS a producer of\n"+
		"  the LEG.COL-shaped row a leg-table population would target — and, since the\n"+
		"  seed sites reach a RecordType only THROUGH it, the one producer of that row.\n"+
		"  This floor no longer guards that finding; it guards the READING. A collapse to\n"+
		"  near-zero total traffic means the counter stopped being reached, so any later\n"+
		"  re-measurement of the producer set is reporting a broken instrument.\n"+
		"  WHAT A COLLAPSE RE-ARMS: the ability to re-check WHERE RecordType.Legs must be\n"+
		"  populated. refineRowTypes DECLINES a populated table against an empty one, so a\n"+
		"  population attached anywhere but this path is a plan-level conflict.\n",
		dotted+plain, floor.Derivations)
	return true
}
