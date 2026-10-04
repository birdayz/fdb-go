package metadata

import (
	"fmt"
	"testing"
)

// Orders measured on Java 4.14.2.0 vector index DDL (WS-J oracle).
func TestJavaHashMapOrder(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		first, then []string
		want        string
	}{
		{
			[]string{"hnswMetric", "hnswRaBitQNumExBits", "hnswUseRaBitQ"},
			[]string{"hnswNumDimensions"},
			"[hnswUseRaBitQ hnswNumDimensions hnswRaBitQNumExBits hnswMetric]",
		},
		{
			[]string{"vectorEngine", "hnswMetric", "guardiannPrimaryClusterMin", "guardiannPrimaryClusterMax", "guardiannCollapseMinDuplicates"},
			[]string{"hnswNumDimensions"},
			"[guardiannPrimaryClusterMax hnswNumDimensions guardiannCollapseMinDuplicates guardiannPrimaryClusterMin vectorEngine hnswMetric]",
		},
		{[]string{"hnswMetric"}, []string{"hnswNumDimensions"}, "[hnswNumDimensions hnswMetric]"},
		{
			nil,
			[]string{"V_IOT_EXPENSIVE", "V_IOT_ELECTRONICS", "V_IOT_MULTI_FILTER"},
			"[V_IOT_ELECTRONICS V_IOT_MULTI_FILTER V_IOT_EXPENSIVE]",
		},
	} {
		if got := fmt.Sprint(javaHashMapOrder(c.first, c.then)); got != c.want {
			t.Errorf("%v+%v = %s, want %s", c.first, c.then, got, c.want)
		}
	}
}
