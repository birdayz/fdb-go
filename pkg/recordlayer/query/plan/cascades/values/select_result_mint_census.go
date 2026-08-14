package values

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// The SELECT RESULT-VALUE MINT census.
//
// It measures the site the FlatMap producer census structurally cannot see: the
// one that BUILDS a select expression's result value, as opposed to the ones
// that flow it.
//
// WHY THE PRODUCER CENSUS IS NOT ENOUGH, and why this is a correction rather
// than an addition. That census attributes a value to the FlatMap construction
// that handed it to a plan, and it reported the untyped population at
// implementExistentialSelect and yieldExistsFlatMap. Those two sites do not
// build anything: they flow sel.GetResultValue() verbatim, exactly as Java's
// three constructions flow selectExpression.getResultValue()
// (ImplementNestedLoopJoinRule.java:187,201,214). Reading their counts as
// production is the inference-instead-of-measurement the census family exists to
// kill — it names a courier as the author.
//
// The author is whatever fills sel.GetResultValue(), and Java's guarantee there
// is STRUCTURAL: GraphExpansion.java:401 builds it as
// overQuantifier.getFlowedObjectValue(), QuantifiedObjectValue.of has no untyped
// overload (QuantifiedObjectValue.java:187), and Quantifier.getFlowedObjectType
// is a Verify.verify plus requireNonNull (Quantifier.java:801-810). Java cannot
// express an untyped one at all. Go's SQL translator mints one directly. That
// mint is what this census counts, and until it was counted the divergence was
// booked against sites that only carried it.
//
// The census also registers the minted value's IDENTITY, so a downstream witness
// can name the MINT rather than the courier. A value is a pointer and it is
// stored unchanged, so identity survives the memo's copies — the same argument
// the FlatMap producer census makes for keying on the value.
//
// GATED by LegIdentityCensusEnabled, like every census on this path.

// SelectResultMintSite names one non-test select-result-value construction.
//
// The identity is the SITE, not the file position: line numbers on this path
// have already invalidated several written-down attributions of this population.
type SelectResultMintSite int

const (
	// SelectResultMintExistsSelect is the SQL translator's EXISTS-bearing select
	// builder, which mints NewQuantifiedObjectValue(outerQ.GetAlias()) — a bare
	// UNTYPED QuantifiedObjectValue — whenever no result override is supplied.
	// This is the site Java's GraphExpansion.java:401 types unconditionally.
	SelectResultMintExistsSelect SelectResultMintSite = iota

	SelectResultMintSiteCount
)

func (s SelectResultMintSite) String() string {
	if s == SelectResultMintExistsSelect {
		return "translator buildExistsSelect(MINT)"
	}
	return "unknown"
}

// SelectResultMintCounters is the census's state.
type SelectResultMintCounters struct {
	// Calls counts every mint, per site.
	Calls [SelectResultMintSiteCount]int
	// TypedQOV / UntypedQOV split the QOV-shaped mints by whether they carry a
	// real flowed type. UntypedQOV is the DIVERGENCE population — see the floors.
	TypedQOV   [SelectResultMintSiteCount]int
	UntypedQOV [SelectResultMintSiteCount]int
	// OtherRV counts a mint that is not a QOV at all.
	OtherRV [SelectResultMintSiteCount]int
	// Shapes records the distinct spellings per site, for the same reason the
	// sibling censuses keep witnesses: a bucket that moves without a spelling
	// change is a different event from one that gains a spelling.
	Shapes [SelectResultMintSiteCount]map[string]int
}

const selectResultMintShapeCap = 128

var (
	selectResultMintMu     sync.Mutex
	selectResultMintCounts SelectResultMintCounters
	selectResultMintOrigin map[Value]SelectResultMintSite
)

// RecordSelectResultMint counts ONE select-result-value construction and
// registers its identity.
//
// The gate is the FIRST statement and it returns: with the census off this site
// must cost one atomic load and nothing else. The shape spelling below is a
// Sprintf per select expression built, which is planning-path work nothing
// consumes when the census is disabled.
func RecordSelectResultMint(site SelectResultMintSite, rv Value) {
	if !LegIdentityCensusEnabled() {
		return
	}
	if site < 0 || site >= SelectResultMintSiteCount || rv == nil {
		return
	}
	shape := fmt.Sprintf("%T", rv)
	qov, isQOV := rv.(*quantifiedObjectValue)
	if isQOV {
		shape = fmt.Sprintf("QOV(typed=%t %s)", quantifiedObjectValueCarriesAType(qov), describeMintedQOVType(qov))
	}

	selectResultMintMu.Lock()
	defer selectResultMintMu.Unlock()
	c := &selectResultMintCounts
	c.Calls[site]++
	switch {
	case !isQOV:
		c.OtherRV[site]++
	case quantifiedObjectValueCarriesAType(qov):
		c.TypedQOV[site]++
	default:
		c.UntypedQOV[site]++
	}
	if c.Shapes[site] == nil {
		c.Shapes[site] = map[string]int{}
	}
	if _, known := c.Shapes[site][shape]; known || len(c.Shapes[site]) < selectResultMintShapeCap {
		c.Shapes[site][shape]++
	}
	if selectResultMintOrigin == nil {
		selectResultMintOrigin = map[Value]SelectResultMintSite{}
	}
	if _, seen := selectResultMintOrigin[rv]; !seen {
		selectResultMintOrigin[rv] = site
	}
}

// quantifiedObjectValueCarriesAType reports whether a QOV carries a REAL flowed
// type rather than the UnknownType placeholder.
//
// The discriminator is the type CODE and not pointer identity against the
// UnknownType singleton: an equivalent unknown built elsewhere is just as
// untyped, and keying on the shared variable would call it typed. `Typ != nil`
// would be a tautology — every constructible QOV has a non-nil Typ.
func quantifiedObjectValueCarriesAType(qov *quantifiedObjectValue) bool {
	return qov.FlowedType() != nil && qov.FlowedType().Code() != TypeCodeUnknown
}

// describeMintedQOVType spells the flowed type, with a record's ARITY, because
// the boolean above and the spelling answer different questions: a mint counted
// as typed-but-not-a-row is a different fact from one carrying a real row.
func describeMintedQOVType(qov *quantifiedObjectValue) string {
	if qov.FlowedType() == nil {
		return "<nil>"
	}
	if rt, ok := qov.FlowedType().(*RecordType); ok {
		return fmt.Sprintf("RecordType(%d)", len(rt.Fields))
	}
	return qov.FlowedType().Code().String()
}

// SelectResultMintOriginOf reports the site that MINTED rv, and whether any did.
// Callers must guard on LegIdentityCensusEnabled().
func SelectResultMintOriginOf(rv Value) (SelectResultMintSite, bool) {
	if rv == nil {
		return 0, false
	}
	selectResultMintMu.Lock()
	defer selectResultMintMu.Unlock()
	site, ok := selectResultMintOrigin[rv]
	return site, ok
}

// SelectResultMintCensus reports the counters.
func SelectResultMintCensus() SelectResultMintCounters {
	selectResultMintMu.Lock()
	defer selectResultMintMu.Unlock()
	return copySelectResultMintCounters(selectResultMintCounts)
}

// copySelectResultMintCounters DEEP-copies the shape maps.
//
// The arrays copy by assignment; the maps inside them do not. Returning the
// struct by value alone would hand a caller a live view of state a concurrent
// planner is still writing — which is exactly the hazard the "render an EXPLICIT
// state" split below exists to remove, and it would defeat it silently.
func copySelectResultMintCounters(c SelectResultMintCounters) SelectResultMintCounters {
	out := c
	for s := range out.Shapes {
		if c.Shapes[s] == nil {
			continue
		}
		m := make(map[string]int, len(c.Shapes[s]))
		for k, v := range c.Shapes[s] {
			m[k] = v
		}
		out.Shapes[s] = m
	}
	return out
}

// ResetSelectResultMintCensus clears the counters and the origin registry.
func ResetSelectResultMintCensus() {
	selectResultMintMu.Lock()
	defer selectResultMintMu.Unlock()
	selectResultMintCounts = SelectResultMintCounters{}
	selectResultMintOrigin = nil
}

// FormatSelectResultMintCensus renders the census for a harness to log.
func FormatSelectResultMintCensus() string {
	return formatSelectResultMintCounters(SelectResultMintCensus())
}

func formatSelectResultMintCounters(c SelectResultMintCounters) string {
	var b strings.Builder
	b.WriteString("Select result-value MINTS (per construction):")
	for s := SelectResultMintSite(0); s < SelectResultMintSiteCount; s++ {
		fmt.Fprintf(&b, "\n  %-34s calls %d | typedQOV %d | UNTYPED-QOV %d | other %d",
			s, c.Calls[s], c.TypedQOV[s], c.UntypedQOV[s], c.OtherRV[s])
		shapes := make([]string, 0, len(c.Shapes[s]))
		for sh, n := range c.Shapes[s] {
			shapes = append(shapes, fmt.Sprintf("x%d %s", n, sh))
		}
		sort.Strings(shapes)
		for _, sh := range shapes {
			fmt.Fprintf(&b, "\n      shape %s", sh)
		}
	}
	return b.String()
}

// SelectResultMintFloors is the mint census's gate.
//
// BOTH fields are FLOORS, including the untyped one, and that polarity reads
// backwards until you know why: an untyped QOV is a Java divergence, so the
// number going DOWN is the good direction and still fails. A shrinking count
// cannot be told from a darkening site, and a divergence nobody is measuring is
// how a population becomes invisible. Lower it deliberately, having decided
// which of the two happened.
//
// Zero means NOT FLOORED, for both fields. That overload is deliberate and it is
// the same one every floor set on this path carries: a nil-able per-site floor
// would need a pointer array and would buy one distinction — "floored at
// exactly zero" — that no site wants, because a floor of zero is satisfied by
// every possible state and asserts nothing.
type SelectResultMintFloors struct {
	Calls           [SelectResultMintSiteCount]int
	UntypedQOVFloor [SelectResultMintSiteCount]int
}

// AssertSelectResultMintCensus checks the per-site call floors and the
// untyped-population floors.
func AssertSelectResultMintCensus(w io.Writer, floors *SelectResultMintFloors) bool {
	return assertSelectResultMintCounters(w, SelectResultMintCensus(), floors)
}

// assertSelectResultMintCounters is the assertion logic over an EXPLICIT counter
// state, so each arm can be driven from a test without driving the planner into
// the state that would produce it — the split every census on this path makes.
func assertSelectResultMintCounters(w io.Writer, c SelectResultMintCounters, floors *SelectResultMintFloors) bool {
	failed := false
	// The PARTITION runs first and unconditionally: it holds over any population,
	// so a NARROWED run (-test.run) keeps it while dropping the floors, exactly as
	// the sibling censuses split their structural checks from their calibrations.
	for s := SelectResultMintSite(0); s < SelectResultMintSiteCount; s++ {
		if got := c.TypedQOV[s] + c.UntypedQOV[s] + c.OtherRV[s]; got != c.Calls[s] {
			failed = true
			fmt.Fprintf(w, "SELECT MINT CENSUS FAIL: %s: typed(%d) + untyped(%d) + other(%d) = %d,\n"+
				"  but calls = %d. Every mint takes exactly one arm, so a gap is an arm that\n"+
				"  returns before recording.\n",
				s, c.TypedQOV[s], c.UntypedQOV[s], c.OtherRV[s], got, c.Calls[s])
		}
	}
	if floors == nil {
		return failed
	}
	for s := SelectResultMintSite(0); s < SelectResultMintSiteCount; s++ {
		if floors.UntypedQOVFloor[s] == 0 || c.UntypedQOV[s] >= floors.UntypedQOVFloor[s] {
			continue
		}
		failed = true
		fmt.Fprintf(w, "SELECT MINT CENSUS FAIL: %s minted %d UNTYPED QOV result value(s),\n"+
			"  want >= %d. This is a FLOOR on a DIVERGENCE and the DROP is what fails.\n"+
			"  Java cannot build an untyped QOV at all — QuantifiedObjectValue.of has no\n"+
			"  untyped overload (QuantifiedObjectValue.java:187) and getFlowedObjectType is\n"+
			"  a Verify.verify plus requireNonNull (Quantifier.java:801-810) — so this is a\n"+
			"  real gap and the floor keeps it COUNTED rather than blessing it.\n"+
			"  A drop is either the gap CLOSING (say so, re-measure, move the floor down\n"+
			"  deliberately) or the site going DARK, and those are indistinguishable from a\n"+
			"  smaller number.\n"+
			"  census: %s\n", s, c.UntypedQOV[s], floors.UntypedQOVFloor[s], formatSelectResultMintCounters(c))
	}
	for s := SelectResultMintSite(0); s < SelectResultMintSiteCount; s++ {
		if c.Calls[s] >= floors.Calls[s] {
			continue
		}
		failed = true
		fmt.Fprintf(w, "SELECT MINT CENSUS FAIL: %s made %d mint(s), want >= %d — the site has\n"+
			"  gone dark, so every zero recorded beside it is measuring an absence of\n"+
			"  traffic rather than an absence of the shape.\n"+
			"  census: %s\n", s, c.Calls[s], floors.Calls[s], formatSelectResultMintCounters(c))
	}
	return failed
}
