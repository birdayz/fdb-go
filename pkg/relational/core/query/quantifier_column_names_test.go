package query

import (
	"slices"
	"testing"
)

func TestQuantifierColumnNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		labels, want []string
	}{
		{[]string{"ID", "V"}, []string{"ID", "V"}},
		{[]string{"ID", "ID"}, []string{"_0", "_1"}},
		{[]string{"ID", "V", "ID", "V"}, []string{"_0", "_1", "_2", "_3"}},
		{[]string{"ID", "ID", "V"}, []string{"_0", "_1", "V"}},
		{[]string{"", "ID"}, []string{"_0", "ID"}},
		{[]string{"_7", "ID"}, []string{"_7", "ID"}},
		{[]string{"_7", "ID", "ID"}, []string{"_0", "_1", "_2"}},
		{[]string{"_x", "ID", "ID"}, []string{"_x", "_1", "_2"}},
		{nil, nil},
	} {
		if got := QuantifierColumnNames(tc.labels); !slices.Equal(got, tc.want) {
			t.Errorf("QuantifierColumnNames(%q) = %q, want %q", tc.labels, got, tc.want)
		}
	}
}
