package cascades

import (
	"fmt"
	"io"
	"sync"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The MERGE-SLOT TYPING census.
//
// It counts one thing, at the one site that decides it: what each slot of a
// positional merge (`positional_merge.go`) ends up STATING. The interesting
// outcome is the MISS — a slot entering the merged record constructor stating
// no type — because that failure is silent: the reference degrades to
// source-relative, a source-relative operand pushed into a leg's scan evaluates
// to NULL against the build-bound row, and the join returns zero rows with no
// error.
//
// It used to be a rider on the leg-local bakeability census, which measured the
// three-quantifier NLJ arm's merged-leg reads. That arm and its census are
// retired (RFC-235); the positional merge is not, so its instrument is kept
// rather than dropped. A census deleted alongside the thing it happened to
// share a file with is an unwatched live path.
//
// It is GATED (values.LegIdentityCensusEnabled) like its siblings. The site is
// inside a planner rule, so totals count RULE FIRINGS, not queries.
type mergeSlotClass int

const (
	// mergeSlotClassScavenged: the quantifier stated no row and legRowTypes
	// recovered one. Correct, but it means the producer did not state it.
	mergeSlotClassScavenged mergeSlotClass = iota
	// mergeSlotClassTyped: the quantifier stated a ROW. The intended path.
	mergeSlotClassTyped
	// mergeSlotClassUntyped: the slot states NO type at all. The residual, and
	// the class whose growth is the alarm.
	mergeSlotClassUntyped
	// mergeSlotClassScalar: the quantifier stated a NON-ROW type. An unnest
	// ELEMENT flows one array element and is correct, which is why the census
	// classifies finer than the record-type gate at the call site can.
	mergeSlotClassScalar
)

func (c mergeSlotClass) String() string {
	switch c {
	case mergeSlotClassScavenged:
		return "Scavenged"
	case mergeSlotClassTyped:
		return "Typed"
	case mergeSlotClassUntyped:
		return "Untyped"
	case mergeSlotClassScalar:
		return "Scalar"
	default:
		return fmt.Sprintf("mergeSlotClass(%d)", int(c))
	}
}

// MergeSlotTypingCounts is the census tally. The four classes must partition
// Slots; that identity is what makes a share of them meaningful.
type MergeSlotTypingCounts struct {
	Typed     int
	Scavenged int
	Scalar    int
	Untyped   int
	Slots     int
}

var (
	mergeSlotMu     sync.Mutex
	mergeSlotCounts MergeSlotTypingCounts
)

func classifyMergeSlot(slotType values.Type, scavenged bool) mergeSlotClass {
	switch {
	case scavenged:
		return mergeSlotClassScavenged
	case isRecordSlotType(slotType):
		return mergeSlotClassTyped
	case slotType == nil || slotType.Code() == values.TypeCodeUnknown:
		return mergeSlotClassUntyped
	default:
		return mergeSlotClassScalar
	}
}

// recordMergeSlotTyping counts one positional-merge slot by what it states.
func recordMergeSlotTyping(_ values.CorrelationIdentifier, slotType values.Type, scavenged bool) {
	class := classifyMergeSlot(slotType, scavenged)
	mergeSlotMu.Lock()
	defer mergeSlotMu.Unlock()
	mergeSlotCounts.Slots++
	switch class {
	case mergeSlotClassScavenged:
		mergeSlotCounts.Scavenged++
	case mergeSlotClassTyped:
		mergeSlotCounts.Typed++
	case mergeSlotClassUntyped:
		mergeSlotCounts.Untyped++
	case mergeSlotClassScalar:
		mergeSlotCounts.Scalar++
	}
}

// MergeSlotTypingCensus returns the current tally.
func MergeSlotTypingCensus() MergeSlotTypingCounts {
	mergeSlotMu.Lock()
	defer mergeSlotMu.Unlock()
	return mergeSlotCounts
}

// ResetMergeSlotTypingCensus zeroes the tally.
func ResetMergeSlotTypingCensus() {
	mergeSlotMu.Lock()
	defer mergeSlotMu.Unlock()
	mergeSlotCounts = MergeSlotTypingCounts{}
}

// FormatMergeSlotTypingCensus renders the tally for a harness report.
func FormatMergeSlotTypingCensus() string {
	c := MergeSlotTypingCensus()
	return fmt.Sprintf("merge-slot typing: %d slots (typed %d, scavenged %d, scalar %d, untyped %d)",
		c.Slots, c.Typed, c.Scavenged, c.Scalar, c.Untyped)
}

// AssertMergeSlotTypingCensus checks the census against `floor` slots, reports
// its reasoning to w, and returns whether it FAILED.
//
// The polarity matches its siblings and the censusGate contract
// (census_gate_reporting_test.go): run returns FAILED, not OK. Getting it
// backwards makes a clean census report as a failure, which is how this was
// caught — the gate fired on 22,362 slots with a holding partition and zero
// untyped.
//
// Two directions, and they are not the same alarm. The PARTITION identity going
// wrong means the instrument miscounts, so every share it reports is
// meaningless. UNTYPED going non-zero means a live slot lost its row type,
// which is the silent zero-rows defect. The floor guards the third case: a
// collapsed denominator makes `Untyped == 0` true for the wrong reason.
func AssertMergeSlotTypingCensus(w io.Writer, c MergeSlotTypingCounts, floor int) bool {
	failed := false
	if got := c.Typed + c.Scavenged + c.Scalar + c.Untyped; got != c.Slots {
		failed = true
		fmt.Fprintf(w, "MERGE-SLOT CENSUS FAIL: Typed(%d)+Scavenged(%d)+Scalar(%d)+Untyped(%d) = %d, "+
			"but Slots = %d. The four classes must PARTITION the denominator; without that "+
			"identity the Untyped share below is a number about nothing.\n",
			c.Typed, c.Scavenged, c.Scalar, c.Untyped, got, c.Slots)
	}
	if c.Slots < floor {
		failed = true
		fmt.Fprintf(w, "MERGE-SLOT CENSUS FAIL: Slots = %d, want >= %d. The census went QUIET, "+
			"which prints identically to a clean measurement: Untyped == 0 is then true because "+
			"nothing was measured, not because nothing lost its type.\n", c.Slots, floor)
	}
	if c.Untyped != 0 {
		failed = true
		fmt.Fprintf(w, "MERGE-SLOT CENSUS FAIL: Untyped = %d, want 0. A merge slot that states no "+
			"type degrades its reference to source-relative, which evaluates to NULL against the "+
			"build-bound row — the join returns zero rows and reports success.\n", c.Untyped)
	}
	return failed
}

func isRecordSlotType(t values.Type) bool {
	_, isRecord := t.(*values.RecordType)
	return isRecord
}
