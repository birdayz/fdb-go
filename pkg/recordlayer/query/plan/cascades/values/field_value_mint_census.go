package values

import (
	"fmt"
	"io"
	"runtime"
	"sort"
	"strings"
	"sync"
)

// The FIELD-VALUE MINT census: who builds a LAZY FieldValue, and what they put
// in its Field.
//
// WHY THE MINT AND NOT THE CONSUMER. The consumer-side census
// (accessor_name_path_census.go) established that flat-dotted lazy names reach
// the one match-domain identity function — 3 calls in 269 979 — and that the two
// distinct witnesses are `q$50765.AID#0` and `q$51013.AID#0`. Those are not
// column names. They are rendered Explain labels: correlation, dot, column,
// `#ordinal`, which is exactly what explainValueOrdinals emits (values.go:1796-
// 1827) and exactly how Java spells a FieldPath (FieldValue.java:431). So a
// DISPLAY RENDERING is being stored as a NAME, and the guard that catches it is
// eight consumers downstream of whatever does that. Fixing consumers one at a
// time would be eight partial fixes for one defect.
//
// WHAT THIS CAN AND CANNOT SEE, stated because the coverage is partial by
// construction and a partial denominator quoted as a whole one is how a census
// lies. FieldValue is minted two ways: through the constructors in values.go,
// which this hooks, and through bare struct literals, which nothing can hook
// centrally. Enumerated at the time of writing with
//
//	grep -rn '&FieldValue{\|values\.FieldValue{' pkg/ | grep -v '_test\.go' | wc -l
//
// there are 57 such literals in non-test code. SIX of them are the constructor
// bodies themselves and are already counted through the constructor hook;
// hooking those again would double-count and corrupt the denominator.
//
// TWENTY-THREE LITERALS ARE HOOKED DIRECTLY: all 12 in cascades_translator.go,
// all 4 in pullup.go, the 2 non-constructor literals in values.go, the 3 in
// logical_predicate.go, and the 2 in plans/ordering.go. The last two are where
// the Explain-rendered producer turned out to live.
//
// FOUR POST-CONSTRUCTION MUTATIONS are hooked too, and finding them is the
// reason this census cannot be described as a MINT census without qualification:
// FieldValue.Field is a plain exported string field and four sites ASSIGN to it
// after construction (logical_predicate.go:9417, cascades_translator.go:4016 and
// :7335, unnest_gather.go:399). A node can therefore acquire a name its
// constructor never saw, which no hook at construction can observe. Enumerate
// them with: grep -rn '\.Field = ' pkg/ | grep -v '_test\.go'
//
// TWENTY-EIGHT LITERALS REMAIN UNHOOKED and are OUT OF SCOPE for this pass, not
// covered by it: replace.go (3), max_match_map.go (3), match_info_merge.go (3),
// index_onsource.go (2), primary_key_translation.go (2), and singletons
// elsewhere. A zero class means "no mint of this shape among the 33 sites this
// census can see", never "no mint of this shape" -- the same trap one level down
// from the one this census exists to avoid, so the dump states it on every run
// rather than leaving it in a comment nobody reads with the numbers.
//
// THE DENOMINATOR IS COUNTED INDEPENDENTLY of the classes, so the two can
// disagree and the disagreement is visible rather than absorbed. A class vector
// that sums to less than the total means a mint took a path no arm classified,
// which is a bug in this census and not a fact about the code.
//
// GATED by LegIdentityCensusEnabled as the first statement of the hook.
// Disabled, a mint pays one atomic load. FieldValue construction is hot — the
// consumer census alone saw 270 000 calls to a single function — so the
// gated-off cost is measured rather than asserted; see
// BenchmarkFieldValueMintGateOff.

// FieldMintClass is one lazy mint's bucket. These partition the mints this
// census sees, and the partition is checked against an independent total.
type FieldMintClass int

const (
	// FieldMintLazyBare: a lazy Field with a plain, undotted name. The ordinary
	// plan-time carrier — a match candidate or an ordering hint.
	FieldMintLazyBare FieldMintClass = iota

	// FieldMintLazyDotted: a lazy Field containing '.' but no '#'. Ambiguous
	// between a real nested path (addr.city) and an alias-qualified leaf
	// (T.city), which is the ambiguity AccessorNamePath refuses to resolve.
	FieldMintLazyDotted

	// FieldMintLazyExplainRendered: a lazy Field containing BOTH '.' and '#' —
	// the shape explainValueOrdinals produces and nothing else does. This is the
	// sharp class. A name is data the planner resolves; an Explain rendering is
	// output. Feeding output back in as identity is a layering violation, not
	// merely a missing ordinal, and it is the one class whose non-zero is a
	// defect rather than debt.
	FieldMintLazyExplainRendered

	// FieldMintLazyEmpty: a lazy Field with no name at all.
	FieldMintLazyEmpty

	// FieldMintBaked: Resolved is non-nil, so the node carries an ordinal
	// identity and its Field is display only — the state the whole migration is
	// moving toward. Counted so the lazy classes have something to be a fraction
	// OF.
	FieldMintBaked

	fieldMintClassCount
)

func (c FieldMintClass) String() string {
	switch c {
	case FieldMintLazyBare:
		return "lazy-bare"
	case FieldMintLazyDotted:
		return "lazy-DOTTED"
	case FieldMintLazyExplainRendered:
		return "lazy-EXPLAIN-RENDERED"
	case FieldMintLazyEmpty:
		return "lazy-empty"
	case FieldMintBaked:
		return "baked"
	default:
		return "unknown"
	}
}

var (
	fieldMintMu sync.Mutex
	// fieldMintTotal is the INDEPENDENT denominator: incremented once per hook
	// call, before any classification, so a mint that no arm classifies still
	// lands here and the partition check can fail.
	fieldMintTotal  int
	fieldMintCounts [fieldMintClassCount]int
	// fieldMintOrigins keeps one stack per distinct (class, name) for the two
	// dotted classes only. Those are rare by every measurement so far; capturing
	// stacks for lazy-bare would be a profiler, not a census.
	fieldMintOrigins = map[FieldMintClass]map[string]struct{}{}
)

// fieldMintOriginCap bounds the stack set. Anything approaching it means the
// dotted classes stopped being rare, which is itself the finding.
const fieldMintOriginCap = 64

// ClassifyFieldMint is the pure classifier, split from the counter so it can be
// exercised without process-global state. A classification only reachable
// through a mutation is a classification no test can pin.
func ClassifyFieldMint(field string, baked bool) FieldMintClass {
	if baked {
		return FieldMintBaked
	}
	if field == "" {
		return FieldMintLazyEmpty
	}
	if strings.Contains(field, ".") {
		// '#' is the ordinal discriminator explainValueOrdinals appends. A name
		// that carries one did not come from a schema; it came from a renderer.
		if strings.Contains(field, "#") {
			return FieldMintLazyExplainRendered
		}
		return FieldMintLazyDotted
	}
	return FieldMintLazyBare
}

// NoteFieldValueMint records one FieldValue construction. The gate is the FIRST
// statement: disabled, this is one atomic load and a return.
func NoteFieldValueMint(field string, baked bool) {
	if !LegIdentityCensusEnabled() {
		return
	}
	class := ClassifyFieldMint(field, baked)
	var origin string
	if class == FieldMintLazyDotted || class == FieldMintLazyExplainRendered {
		var buf [4096]byte
		origin = field + "\n" + accessorPathTrim(string(buf[:runtime.Stack(buf[:], false)]))
	}
	fieldMintMu.Lock()
	defer fieldMintMu.Unlock()
	// Denominator first, before any guard that could skip it.
	fieldMintTotal++
	fieldMintCounts[class]++
	if origin != "" {
		// PER-CLASS cap. A shared cap lets a common class crowd out a rare one:
		if fieldMintOrigins[class] == nil {
			fieldMintOrigins[class] = map[string]struct{}{}
		}
		if len(fieldMintOrigins[class]) < fieldMintOriginCap {
			fieldMintOrigins[class][origin] = struct{}{}
		}
	}
}

// FieldValueMintCensus returns the independent total and the class vector.
func FieldValueMintCensus() (total int, counts [fieldMintClassCount]int) {
	fieldMintMu.Lock()
	defer fieldMintMu.Unlock()
	return fieldMintTotal, fieldMintCounts
}

// FieldValueMintOrigins returns the captured producer stacks for the dotted
// classes.
func FieldValueMintOrigins() []string {
	fieldMintMu.Lock()
	defer fieldMintMu.Unlock()
	out := make([]string, 0, 16)
	for class, m := range fieldMintOrigins {
		for k := range m {
			out = append(out, class.String()+" :: "+k)
		}
	}
	sort.Strings(out)
	return out
}

// ResetFieldValueMintCensus clears the counters.
func ResetFieldValueMintCensus() {
	fieldMintMu.Lock()
	defer fieldMintMu.Unlock()
	fieldMintTotal = 0
	fieldMintCounts = [fieldMintClassCount]int{}
	fieldMintOrigins = map[FieldMintClass]map[string]struct{}{}
}

// DumpFieldValueMintCensus renders the census, with the partition check and the
// vacuity guard ahead of any zero-class claim.
func DumpFieldValueMintCensus(w io.Writer, label string) {
	total, counts := FieldValueMintCensus()
	fmt.Fprintf(w, "\n[%s] field-value MINT census (33 hook sites: 6 constructors + 23 literals + 4 post-construction Field mutations; 28 literals elsewhere OUT OF SCOPE and uncounted): total %d\n",
		label, total)
	sum := 0
	for i := FieldMintClass(0); i < fieldMintClassCount; i++ {
		fmt.Fprintf(w, "  %-24s %d\n", i, counts[i])
		sum += counts[i]
	}
	if sum != total {
		fmt.Fprintf(w, "  PARTITION BROKEN: classes sum to %d against an independently "+
			"counted total of %d — %d mint(s) took a path no arm classified, which is a "+
			"bug in this census rather than a fact about the code\n", sum, total, total-sum)
	}
	if total == 0 {
		fmt.Fprintf(w, "  VACUOUS: no constructor mint was observed at all, so every zero "+
			"above is the absence of a population rather than the absence of a shape. "+
			"Nothing may be concluded from this run.\n")
		return
	}
	if counts[FieldMintLazyExplainRendered] == 0 && counts[FieldMintLazyDotted] == 0 {
		fmt.Fprintf(w, "  no dotted mint among the 33 hooked sites over %d mints. The "+
			"consumer census sees flat-dotted values reach AccessorNamePath, so if this "+
			"stays zero the producer is in the 28 UNHOOKED literals listed in this "+
			"file's header — this census does not cover them and its zero is not "+
			"evidence about them\n", total)
	}
	for _, o := range FieldValueMintOrigins() {
		fmt.Fprintf(w, "  dotted mint origin:\n%s\n", o)
	}
}

// ---- the ANSWERING comparison -------------------------------------------
//
// AccessorNamePath DECLINES on a flat-dotted name, which is safe: the caller
// falls back to a residual filter and the rows stay right. CanBridgeOrderingFieldValues
// is the other shape, and it is the one that matters. It requires Child == nil
// on both sides — which is exactly the flat-dotted form — and then returns
// strings.EqualFold(ColumnNameValue(left), ColumnNameValue(right)): an ANSWER,
// not a decline.
//
// So a value whose Field is a rendered Explain label reaches a name equality
// that will report "these are different columns" when the same column spelled
// plainly would have bridged. That is the ratchet's second failure mode — one
// column reached two ways treated as two — and its consequence here is a
// redundant sort rather than wrong rows.
//
// WHETHER IT HAPPENS IS MEASURED, not inferred from the shape. The counts below
// are per bridge attempt where at least one side is dotted, split by what the
// bridge ANSWERED. A dotted attempt that answered false is the defect; a dotted
// attempt that answered true means two renderings matched each other.

type BridgeDottedClass int

const (
	// BridgeDottedAnsweredFalse: at least one side was flat-dotted and the bridge
	// reported NOT the same column. The suspect class.
	BridgeDottedAnsweredFalse BridgeDottedClass = iota
	// BridgeDottedAnsweredTrue: at least one side was flat-dotted and the bridge
	// matched anyway — two identical renderings.
	BridgeDottedAnsweredTrue
	bridgeDottedClassCount
)

func (c BridgeDottedClass) String() string {
	switch c {
	case BridgeDottedAnsweredFalse:
		return "dotted-ANSWERED-false"
	case BridgeDottedAnsweredTrue:
		return "dotted-answered-true"
	default:
		return "unknown"
	}
}

var (
	bridgeDottedMu        sync.Mutex
	bridgeDottedAttempts  int
	bridgeDottedCounts    [bridgeDottedClassCount]int
	bridgeDottedWitnesses = map[string]int{}
)

// NoteOrderingBridgeDotted records a bridge attempt in which at least one side
// carried a flat-dotted name. Gate first; disabled it is one atomic load.
func NoteOrderingBridgeDotted(leftName, rightName string, answered bool) {
	if !LegIdentityCensusEnabled() {
		return
	}
	if !strings.Contains(leftName, ".") && !strings.Contains(rightName, ".") {
		return
	}
	class := BridgeDottedAnsweredFalse
	if answered {
		class = BridgeDottedAnsweredTrue
	}
	bridgeDottedMu.Lock()
	defer bridgeDottedMu.Unlock()
	bridgeDottedAttempts++
	bridgeDottedCounts[class]++
	if len(bridgeDottedWitnesses) < fieldMintOriginCap {
		bridgeDottedWitnesses[leftName+"  <=>  "+rightName]++
	}
}

// OrderingBridgeDottedCensus returns the attempt total and class vector.
func OrderingBridgeDottedCensus() (int, [bridgeDottedClassCount]int, map[string]int) {
	bridgeDottedMu.Lock()
	defer bridgeDottedMu.Unlock()
	ws := make(map[string]int, len(bridgeDottedWitnesses))
	for k, v := range bridgeDottedWitnesses {
		ws[k] = v
	}
	return bridgeDottedAttempts, bridgeDottedCounts, ws
}

// ResetOrderingBridgeDottedCensus clears the counters.
func ResetOrderingBridgeDottedCensus() {
	bridgeDottedMu.Lock()
	defer bridgeDottedMu.Unlock()
	bridgeDottedAttempts = 0
	bridgeDottedCounts = [bridgeDottedClassCount]int{}
	bridgeDottedWitnesses = map[string]int{}
}

// DumpOrderingBridgeDottedCensus renders it, vacuity guard first.
func DumpOrderingBridgeDottedCensus(w io.Writer, label string) {
	total, counts, ws := OrderingBridgeDottedCensus()
	fmt.Fprintf(w, "\n[%s] ordering-bridge DOTTED census (attempts where a side was flat-dotted): total %d\n",
		label, total)
	for i := BridgeDottedClass(0); i < bridgeDottedClassCount; i++ {
		fmt.Fprintf(w, "  %-24s %d\n", i, counts[i])
	}
	if total == 0 {
		fmt.Fprintf(w, "  no flat-dotted value reached the ordering bridge at all, so the "+
			"ANSWERING path is not exercised by this corpus. The decline at "+
			"AccessorNamePath is the only place these values are read, and the wrong-answer "+
			"hazard is latent rather than live. A non-zero here arms it.\n")
		return
	}
	names := make([]string, 0, len(ws))
	for n := range ws {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "    %s  x%d\n", n, ws[n])
	}
}
