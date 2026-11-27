package predicates

import "testing"

func TestLiteralRangeEnclosure(t *testing.T) {
	t.Parallel()
	cmp := func(op ComparisonType, n int64) Comparison { return NewLiteralComparison(op, n) }
	for _, tc := range []struct {
		name         string
		outer, inner []Comparison
		want         bool
	}{
		{"stronger lower", []Comparison{cmp(ComparisonGreaterThan, 0)}, []Comparison{cmp(ComparisonGreaterThan, 10)}, true},
		{"weaker lower", []Comparison{cmp(ComparisonGreaterThan, 10)}, []Comparison{cmp(ComparisonGreaterThan, 0)}, false},
		{"equal open", []Comparison{cmp(ComparisonGreaterThan, 0)}, []Comparison{cmp(ComparisonGreaterThan, 0)}, true},
		{"closed includes open", []Comparison{cmp(ComparisonGreaterThanEq, 0)}, []Comparison{cmp(ComparisonGreaterThan, 0)}, true},
		{"open excludes closed", []Comparison{cmp(ComparisonGreaterThan, 0)}, []Comparison{cmp(ComparisonGreaterThanEq, 0)}, false},
		{"upper narrower", []Comparison{cmp(ComparisonLessThan, 10)}, []Comparison{cmp(ComparisonLessThanOrEq, 5)}, true},
		{"upper open excludes closed", []Comparison{cmp(ComparisonLessThan, 10)}, []Comparison{cmp(ComparisonLessThanOrEq, 10)}, false},
		{"upper closed includes open", []Comparison{cmp(ComparisonLessThanOrEq, 10)}, []Comparison{cmp(ComparisonLessThan, 10)}, true},
		{"bounded singleton", []Comparison{cmp(ComparisonGreaterThan, 0), cmp(ComparisonLessThan, 10)}, []Comparison{cmp(ComparisonEquals, 5)}, true},
		{"outside singleton", []Comparison{cmp(ComparisonEquals, 5)}, []Comparison{cmp(ComparisonEquals, 6)}, false},
		{"no lower", []Comparison{cmp(ComparisonGreaterThan, 0)}, []Comparison{cmp(ComparisonLessThan, 10)}, false},
		{"no upper", []Comparison{cmp(ComparisonLessThan, 10)}, []Comparison{cmp(ComparisonGreaterThan, 0)}, false},
		{"full outer", nil, []Comparison{cmp(ComparisonEquals, 5)}, true},
		{"full inner", []Comparison{cmp(ComparisonEquals, 5)}, nil, false},
		{"null excluded by upper", []Comparison{cmp(ComparisonLessThan, 10)}, []Comparison{{Type: ComparisonIsNull}}, false},
		{"null only", []Comparison{{Type: ComparisonIsNull}}, []Comparison{{Type: ComparisonIsNull}}, true},
		{"not null", []Comparison{{Type: ComparisonIsNotNull}}, []Comparison{cmp(ComparisonGreaterThan, 0)}, true},
		{"not null does not imply positive", []Comparison{cmp(ComparisonGreaterThan, 0)}, []Comparison{{Type: ComparisonIsNotNull}}, false},
		{"null excludes nonnull", []Comparison{{Type: ComparisonIsNull}}, []Comparison{cmp(ComparisonEquals, 5)}, false},
		{"empty query", nil, []Comparison{cmp(ComparisonGreaterThan, 10), cmp(ComparisonLessThan, 0)}, false},
		{"mixed reversed bound", []Comparison{NewLiteralComparison(ComparisonLessThan, float64(9007199254740992))}, []Comparison{NewLiteralComparison(ComparisonLessThan, int64(9007199254740993))}, false},
		{"same float domain", []Comparison{NewLiteralComparison(ComparisonGreaterThan, float64(0))}, []Comparison{NewLiteralComparison(ComparisonGreaterThan, float64(10))}, true},
		{"mixed float precision", []Comparison{NewLiteralComparison(ComparisonGreaterThan, float32(0))}, []Comparison{NewLiteralComparison(ComparisonGreaterThan, float64(10))}, false},
		{"mixed intersection", nil, []Comparison{NewLiteralComparison(ComparisonGreaterThan, int64(0)), NewLiteralComparison(ComparisonLessThan, float64(10))}, false},
		{"mixed rounded bound", []Comparison{NewLiteralComparison(ComparisonGreaterThan, int64(9007199254740993))}, []Comparison{NewLiteralComparison(ComparisonGreaterThan, float64(9007199254740992))}, false},
		{"exact large int", []Comparison{cmp(ComparisonGreaterThan, 9007199254740993)}, []Comparison{cmp(ComparisonGreaterThan, 9007199254740992)}, false},
		{"parameter", []Comparison{cmp(ComparisonGreaterThan, 0)}, []Comparison{{Type: ComparisonGreaterThan, ParameterName: "P"}}, false},
		{"unsupported", []Comparison{cmp(ComparisonNotEquals, 0)}, []Comparison{cmp(ComparisonEquals, 5)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := NewRangeConstraints(tc.outer, nil).Encloses(NewRangeConstraints(tc.inner, nil)); got != tc.want {
				t.Fatalf("encloses=%t, want %t", got, tc.want)
			}
		})
	}
	deferred := []Comparison{{Type: ComparisonGreaterThan, ParameterName: "P"}}
	if NewRangeConstraints(nil, deferred).Encloses(EmptyRangeConstraints()) {
		t.Fatal("deferred candidate constraint ignored")
	}
	if !EmptyRangeConstraints().Encloses(NewRangeConstraints(nil, deferred)) {
		t.Fatal("deferred query can only narrow full range")
	}
}
