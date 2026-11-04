package recordlayer

import "testing"

func TestTextIndexRootDuplicates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		root KeyExpression
		want bool
	}{
		{"unset", nil, false},
		{"scalar", GroupBy(Field("body")), false},
		{"scalar partition", GroupBy(Field("body"), Field("tenant")), false},
		{"fanout text", GroupBy(FanOut("body")), true},
		{"fanout partition", GroupBy(Field("body"), FanOut("tenant")), true},
	} {
		index := NewTextIndex(tc.name, tc.root)
		if got := index.RootExpressionCreatesDuplicates(); got != tc.want {
			t.Fatalf("%s: root duplicates=%v want=%v", tc.name, got, tc.want)
		}
		if tc.root != nil && !index.CreatesDuplicates() {
			t.Fatalf("%s: generic TEXT index scan must still report duplicates", tc.name)
		}
	}
}
