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
// NOT zero. Four full-corpus runs read DOTTED 683 (plain 157511), DOTTED 841
// (plain 157935), DOTTED 681 (plain 157699) and DOTTED 743 (plain 157905) — an
// observed spread of 681..841 — and the witness shapes (`[K C.CV]`, `[AID K C.CV]`,
// `[TID K C.C.CV]`, `[ID NAME O.I.QTY]`) carry exactly the labels the executor's
// dotted arm answers on. `RecordConstructorValue.Type()` IS a producer of the
// dotted row. The integer is corpus-traffic dependent and both readings are
// legitimate; the load-bearing fact is "not zero".
//
// AND THAT FACT IS NOW ASSERTED, by DottedRowTypeProducerFloor.Dotted. It was not,
// for as long as the only floor was the total: plain runs ~230x dotted, so DOTTED
// could return to zero with the total still three orders of magnitude clear, and
// the refutation the placement decision below rests on would have evaporated
// against a green build.
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

// dottedRowTypeCounters is the census state, held apart from the process globals
// so every arm below — both classes, the witness cap, the witness dedup, and both
// floors — can be driven from a unit test over state it states literally.
type dottedRowTypeCounters struct {
	Dotted    int
	Plain     int
	Witnesses []string
}

// record classifies ONE derivation and folds it in.
func (c *dottedRowTypeCounters) record(fields []Field) {
	dotted := false
	for i := range fields {
		if nameIsQualified(fields[i].Name) {
			dotted = true
			break
		}
	}
	if !dotted {
		c.Plain++
		return
	}
	c.Dotted++
	if len(c.Witnesses) >= dottedRowTypeWitnessCap {
		return
	}
	names := make([]string, 0, len(fields))
	for i := range fields {
		names = append(names, fields[i].Name)
	}
	w := "[" + strings.Join(names, " ") + "]"
	for _, seen := range c.Witnesses {
		if seen == w {
			return
		}
	}
	c.Witnesses = append(c.Witnesses, w)
}

var (
	dottedRowTypeMu     sync.Mutex
	dottedRowTypeCounts dottedRowTypeCounters
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
	dottedRowTypeMu.Lock()
	defer dottedRowTypeMu.Unlock()
	dottedRowTypeCounts.record(fields)
}

// dottedRowTypeSnapshot deep-copies the process counters.
func dottedRowTypeSnapshot() dottedRowTypeCounters {
	dottedRowTypeMu.Lock()
	defer dottedRowTypeMu.Unlock()
	out := dottedRowTypeCounts
	out.Witnesses = make([]string, len(dottedRowTypeCounts.Witnesses))
	copy(out.Witnesses, dottedRowTypeCounts.Witnesses)
	return out
}

// DottedRowTypeProducerCensus reports the counts and the distinct dotted row
// shapes the generic path derived.
func DottedRowTypeProducerCensus() (dotted, plain int, witnesses []string) {
	c := dottedRowTypeSnapshot()
	return c.Dotted, c.Plain, c.Witnesses
}

// ResetDottedRowTypeProducerCensus clears the counters.
func ResetDottedRowTypeProducerCensus() {
	dottedRowTypeMu.Lock()
	defer dottedRowTypeMu.Unlock()
	dottedRowTypeCounts = dottedRowTypeCounters{}
}

// FormatDottedRowTypeProducerCensus renders the process census for a harness to
// log.
func FormatDottedRowTypeProducerCensus() string {
	return formatDottedRowTypeProducerCensus(dottedRowTypeSnapshot())
}

// formatDottedRowTypeProducerCensus renders EXPLICIT state.
func formatDottedRowTypeProducerCensus(c dottedRowTypeCounters) string {
	dotted, plain, witnesses := c.Dotted, c.Plain, c.Witnesses
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

// DottedRowTypeProducerFloor carries the two populations this census refuses to
// let collapse silently. They are DIFFERENT claims and neither can stand in for
// the other.
//
//   - Derivations floors the TOTAL traffic (dotted + plain) and guards the
//     INSTRUMENT. A run reporting no derivations at all is reporting a broken
//     counter, not a quiet corpus. `plain` is what floors it in practice:
//     `RecordConstructorValue.Type()` is called by any query that constructs a
//     record, and both full-corpus readings clear 100 by three orders of
//     magnitude.
//
//   - Dotted floors the FINDING, and it is the reason this struct has two fields.
//     The finding is "DOTTED is not zero" — that `RecordConstructorValue.Type()`
//     IS a producer of the `LEG.COL`-shaped row, refuting the producer-set claim
//     RFC-212 §3.4 originally asserted rather than measured, and relocating the
//     leg-table population to this path (§1.1, §3.5). Derivations cannot watch
//     that: plain outnumbers dotted about 230:1 (157699 to 681), so DOTTED could return to zero
//     and the total floor would still pass by three orders of magnitude. The
//     finding was live, load-bearing and unasserted; this is the assertion.
//
// ALARM DIRECTION ON Dotted: COLLAPSE, and it is that way round precisely because
// the expected value moved. The claim this census was built to test was DOTTED ==
// 0; the measurement refuted it, so zero stopped being the steady state and a
// floor — not a hard zero — is what a refuted zero turns into. A return to zero
// now means either the corpus stopped reaching the dotted path or the
// discriminator broke, and in both cases §1.1's placement decision has quietly
// lost the evidence it rests on while the build stays green.
//
// If a later change makes zero legitimate again — the dotted `LEG.COL` row
// retired, the executor's dotted arm gone — the direction INVERTS with it: the
// alarm becomes growth and the guard becomes a hard zero. Reconcile it with the
// new expected value; do not lower it to whatever the run produced, and do not
// delete it, which would leave the revival unwatched.
//
// A nil floor, and equally a zero-valued one, is a NO-OP by construction: the
// shape a `-test.run`-narrowed corpus needs, where a whole-run population claim
// has nothing it can honestly decide.
type DottedRowTypeProducerFloor struct {
	Derivations int
	Dotted      int
}

// AssertDottedRowTypeProducerCensus checks the process census against a floor.
func AssertDottedRowTypeProducerCensus(w io.Writer, floor *DottedRowTypeProducerFloor) bool {
	return assertDottedRowTypeProducerCensusState(w, dottedRowTypeSnapshot(), floor)
}

// assertDottedRowTypeProducerCensusState is the whole decision over EXPLICIT
// state.
func assertDottedRowTypeProducerCensusState(w io.Writer, c dottedRowTypeCounters, floor *DottedRowTypeProducerFloor) bool {
	if floor == nil {
		return false
	}
	failed := false
	if floor.Derivations > 0 && c.Dotted+c.Plain < floor.Derivations {
		failed = true
		fmt.Fprintf(w, "DOTTED ROW-TYPE PRODUCER CENSUS FAIL: %d derivation(s) over the whole\n"+
			"  run, want >= %d.\n"+
			"  ALARM DIRECTION: collapse. This floor guards the READING, not the finding. A\n"+
			"  collapse to near-zero total traffic means the counter stopped being reached, so\n"+
			"  any later re-measurement of the producer set is reporting a broken instrument.\n"+
			"  WHAT A COLLAPSE RE-ARMS: the ability to re-check WHERE RecordType.Legs must be\n"+
			"  populated. refineRowTypes DECLINES a populated table against an empty one, so a\n"+
			"  population attached anywhere but this path is a plan-level conflict.\n",
			c.Dotted+c.Plain, floor.Derivations)
	}
	if floor.Dotted > 0 && c.Dotted < floor.Dotted {
		failed = true
		fmt.Fprintf(w, "DOTTED ROW-TYPE PRODUCER CENSUS FAIL: DOTTED %d, want >= %d.\n"+
			"  ALARM DIRECTION: collapse — and note the direction INVERTED from what this census\n"+
			"  was built to check. The original claim was that RecordConstructorValue.Type is NOT\n"+
			"  a producer of the LEG.COL-shaped row, i.e. that DOTTED would be ZERO. It was\n"+
			"  measured at 681..841 over four full-corpus runs, refuting it, and RFC-212 §1.1\n"+
			"  relocated the leg-table population onto this path on exactly that evidence.\n"+
			"  WHAT A COLLAPSE RE-ARMS: that placement decision. The total-derivations floor\n"+
			"  CANNOT see this — plain outnumbers dotted roughly 230:1, so DOTTED can fall to\n"+
			"  zero with the total still three orders of magnitude clear. Either the corpus\n"+
			"  stopped reaching the dotted path or nameIsQualified stopped recognising it; find\n"+
			"  out which before touching this number. If a change made a zero LEGITIMATE, invert\n"+
			"  this guard to a hard zero rather than lowering it — a lowered floor watches\n"+
			"  nothing and a deleted one leaves the revival unseen.\n",
			c.Dotted, floor.Dotted)
	}
	if failed {
		fmt.Fprintf(w, "  census: %s\n", formatDottedRowTypeProducerCensus(c))
	}
	return failed
}
