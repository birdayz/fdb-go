package recordlayer

// The MIN NULL repair reads the ordinary subspace, and that read must be
// CHARGED to the budget the caller is running under.
//
// The repair used to build its scan properties from scratch — isolation level
// plus a returned-row limit — and a freshly-built ExecuteProperties carries no
// ScanState. No ScanState means no limit: every leaf cursor falls back to a
// private per-cursor counter, so the repair's extra read per NULL-extremum
// group was invisible to the statement's record, byte and time budgets. Thirty
// mixed groups could complete sixty index reads under a forty-record fail
// limit, and the overrun scaled with the DATA rather than with anything the
// caller wrote.
//
// These drive the decision directly rather than through a query, because what
// is being asserted is a property of the ScanProperties HANDED TO THE SCAN, and
// a query-level test can only observe it indirectly — through a limit that
// happens to trip. The fake scan function captures what it was given.
//
// Two fields are deliberately NOT inherited, and each has its own arm below,
// because getting either wrong is silent: an inherited ReturnedRowLimit reads
// more than the one entry needed, and an inherited Skip skips that entry and
// makes the repair report "no non-NULL value in this group" — turning a paging
// OFFSET into a wrong answer.

import (
	"context"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// capturingScan records the properties it was handed and returns an empty
// cursor, which drives the repair's "group holds no non-NULL value" path — the
// shortest route through the function that still performs the scan call.
func capturingScan(got *ScanProperties, calls *int) OrdinaryIndexScanFunc {
	return func(_ TupleRange, p ScanProperties) RecordCursor[*IndexEntry] {
		*got = p
		*calls++
		return Empty[*IndexEntry]()
	}
}

func TestPermutedMinRepairChargesTheCallersBudget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	sharedState := &ScanLimiterState{}
	caller := ExecuteProperties{
		IsolationLevel:         IsolationLevelSerializable,
		ScannedRecordsLimit:    40,
		ScannedBytesLimit:      1 << 20,
		TimeLimit:              5 * time.Second,
		FailOnScanLimitReached: true,
		ScanState:              sharedState,
		// The two that must NOT survive into the repair's read.
		ReturnedRowLimit: 500,
		Skip:             7,
	}

	var got ScanProperties
	calls := 0
	if _, err := PermutedMinIgnoringNulls(ctx, capturingScan(&got, &calls),
		"idx", tuple.Tuple{int64(1)}, 1, 2, caller); err != nil {
		t.Fatalf("repair returned an error: %v", err)
	}
	if calls != 1 {
		t.Fatalf("the repair performed %d scans, want exactly 1 — with 0 the assertions "+
			"below are about a zero value and say nothing", calls)
	}

	p := got.ExecuteProperties

	// THE POINT: the shared counter object must be the caller's, by POINTER.
	// A copy would count the repair's reads into a set nobody checks.
	if p.ScanState != sharedState {
		t.Errorf("the repair's scan did not carry the caller's ScanState (got %p, want %p). "+
			"A read with no shared state is charged to a private per-cursor counter, so it is "+
			"invisible to the statement's record/byte/time budgets and the overrun scales with "+
			"the number of NULL-bearing groups", p.ScanState, sharedState)
	}

	// The limits themselves must survive, or the state is carried but never
	// checked against anything.
	for _, c := range []struct {
		name      string
		got, want any
	}{
		{"ScannedRecordsLimit", p.ScannedRecordsLimit, 40},
		{"ScannedBytesLimit", p.ScannedBytesLimit, int64(1 << 20)},
		{"TimeLimit", p.TimeLimit, 5 * time.Second},
		{"FailOnScanLimitReached", p.FailOnScanLimitReached, true},
		{"IsolationLevel", p.IsolationLevel, IsolationLevelSerializable},
	} {
		if c.got != c.want {
			t.Errorf("%s was not inherited: got %v, want %v", c.name, c.got, c.want)
		}
	}

	// ReturnedRowLimit is overridden to 1: the ordinary subspace is ordered by
	// value within the group, so the FIRST non-NULL entry is the answer and
	// reading the caller's 500 would be waste charged to the caller's budget.
	if p.ReturnedRowLimit != 1 {
		t.Errorf("ReturnedRowLimit = %d, want 1 — the repair needs exactly one entry, and "+
			"inheriting the caller's limit reads more than that against the caller's budget",
			p.ReturnedRowLimit)
	}

	// Skip is cleared. An inherited OFFSET would skip the single entry the
	// repair reads, the repair would conclude the group holds no non-NULL
	// value, and a paging offset would become a wrong ANSWER.
	if p.Skip != 0 {
		t.Errorf("Skip = %d, want 0 — an inherited OFFSET skips the one entry the repair "+
			"reads, so the group reports NULL and the paging offset silently changes the answer",
			p.Skip)
	}
}

// TestPermutedMinRepairRejectsAnAbsentScan keeps the two argument guards live.
// They are the only thing standing between a mis-wired caller and a nil
// dereference inside the scan, and neither is reachable from the query path —
// which is exactly why they need a unit pin rather than corpus coverage.
func TestPermutedMinRepairRejectsAnAbsentScan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	props := ExecuteProperties{IsolationLevel: IsolationLevelSerializable}

	if _, err := PermutedMinIgnoringNulls(ctx, nil, "idx", tuple.Tuple{int64(1)}, 1, 2, props); err == nil {
		t.Error("a nil scan function was accepted")
	}
	var got ScanProperties
	calls := 0
	for _, span := range [][2]int{{-1, 2}, {2, 2}, {3, 1}} {
		if _, err := PermutedMinIgnoringNulls(ctx, capturingScan(&got, &calls),
			"idx", tuple.Tuple{int64(1)}, span[0], span[1], props); err == nil {
			t.Errorf("an invalid value span [%d,%d) was accepted", span[0], span[1])
		}
	}
	if calls != 0 {
		t.Errorf("an invalid span reached the scan %d times; the guards must reject before "+
			"any read is charged", calls)
	}
}
