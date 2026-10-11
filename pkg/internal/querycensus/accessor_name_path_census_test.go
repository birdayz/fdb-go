package querycensus

import (
	"strings"
	"testing"
)

// TestAccessorPathTrimKeepsProducerFrames pins that the stack trimmer does not
// eat the frames it exists to surface.
func TestAccessorPathTrimKeepsProducerFrames(t *testing.T) {
	t.Parallel()
	raw := "goroutine 42 [running]:\n" +
		"fdb.dev/pkg/.../values.RecordAccessorPathDottedWitness(...)\n" +
		"\t/src/pkg/recordlayer/query/plan/cascades/values/accessor_name_path_census.go:180 +0x1\n" +
		"fdb.dev/pkg/.../cascades.orderingSatisfiesGroupingKeys(...)\n" +
		"\t/src/pkg/recordlayer/query/plan/cascades/rule_implement_streaming_agg.go:225 +0x93\n"
	got := accessorPathTrim(raw)
	if !strings.Contains(got, "rule_implement_streaming_agg.go:225") {
		t.Fatalf("the trimmer dropped the producer frame: %q\nIt exists to remove the "+
			"census's OWN frames; removing the caller's defeats the point.", got)
	}
	if strings.Contains(got, "accessor_name_path_census.go") {
		t.Fatalf("the trimmer kept the census's own frame: %q", got)
	}
	if strings.Contains(got, "goroutine ") {
		t.Fatalf("the trimmer kept the goroutine header: %q", got)
	}
}
