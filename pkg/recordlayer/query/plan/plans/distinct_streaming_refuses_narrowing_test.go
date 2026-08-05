package plans

// R3's narrowing and the STREAMING executor do not compose, and the failure
// mode is silent: executeDistinct returns from its Streaming branch before it
// ever asks whether the plan was narrowed, so a streaming plan carrying the flag
// dedups every row exactly as an un-narrowed one would — while EXPLAIN renders
// `narrowed-by` and every acceptance criterion written against that string reads
// the optimization as fired.
//
// The plan therefore refuses to carry the flag in streaming mode, which is the
// only place the two can be kept from disagreeing: the executor cannot fix it
// (it is right to stream), and EXPLAIN cannot fix it (it is right to render what
// the plan says).

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func narrowingRefusalInner() RecordQueryPlan {
	return NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
}

// TestStreamingDistinctRefusesNarrowedDedup: the flag is not set, so nothing
// downstream can read a narrowing that the executor will not perform.
func TestStreamingDistinctRefusesNarrowedDedup(t *testing.T) {
	t.Parallel()

	streaming := NewRecordQueryDistinctPlan(narrowingRefusalInner())
	streaming.Streaming = true

	narrowed := streaming.WithNarrowedDedup("IDX_EMAIL", []int{0})
	if narrowed.IsNarrowedDedup() {
		t.Fatal("a streaming distinct accepted the R3 narrowing; the streaming " +
			"executor returns before narrowedDedupFor is consulted, so the flag " +
			"would claim an optimization that never runs")
	}
	if narrowed.GetDistinctProofIndexName() != "" {
		t.Fatalf("streaming distinct stamped a proof dependency %q it does not rest on",
			narrowed.GetDistinctProofIndexName())
	}
	if got := narrowed.Explain(); strings.Contains(got, "narrowed-by") {
		t.Fatalf("EXPLAIN renders %q on a streaming shape; an acceptance criterion "+
			"asserting narrowed-by would pass while the full dedup ran", got)
	}
}

// TestHashDistinctAcceptsNarrowedDedup is the control. The refusal must be about
// STREAMING and nothing else: on the hash path the narrowing is exactly what
// empties the seen-set, and refusing it there would silently drop the
// optimization the RFC is for.
func TestHashDistinctAcceptsNarrowedDedup(t *testing.T) {
	t.Parallel()

	hash := NewRecordQueryDistinctPlan(narrowingRefusalInner())
	if hash.Streaming {
		t.Fatal("precondition: the default distinct is the hash executor")
	}

	narrowed := hash.WithNarrowedDedup("IDX_EMAIL", []int{0})
	if !narrowed.IsNarrowedDedup() {
		t.Fatal("the hash distinct must accept the R3 narrowing")
	}
	if got := narrowed.Explain(); !strings.Contains(got, "narrowed-by:IDX_EMAIL") {
		t.Fatalf("EXPLAIN = %q, want it to name the licensing index", got)
	}
}
