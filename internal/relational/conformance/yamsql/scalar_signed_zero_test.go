package yamsql

import (
	"fmt"
	"testing"
)

func TestScalarSignedZeroExpectation(t *testing.T) {
	t.Parallel()
	scenario, err := Load("testdata/scalar_signed_zero.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(scenario.Tests) != 5 {
		t.Fatalf("signed-zero scenario has %d queries, want 5", len(scenario.Tests))
	}
	for _, tc := range []struct {
		query int
		rows  [][]any
	}{
		{0, [][]any{{"-Infinity", "-Infinity", "-Infinity", "-Infinity", "-Infinity", "-Infinity"}}},
		{1, [][]any{{"-Infinity", "-Infinity", "Infinity"}}},
		{3, [][]any{{"-Infinity", int64(1)}, {"Infinity", int64(1)}}},
		{4, [][]any{{"-Infinity"}, {"Infinity"}}},
	} {
		t.Run(fmt.Sprintf("query%d", tc.query), func(t *testing.T) {
			t.Parallel()
			query := scenario.Tests[tc.query]
			if diff := diffRows(query.Rows, tc.rows, query.Unordered); diff != "" {
				t.Fatal(diff)
			}
			for row, cells := range tc.rows {
				for slot, cell := range cells {
					if _, isString := cell.(string); !isString {
						continue
					}
					mutated := append([][]any(nil), tc.rows...)
					mutated[row] = append([]any(nil), cells...)
					if cell == "-Infinity" {
						mutated[row][slot] = "Infinity"
					} else {
						mutated[row][slot] = "-Infinity"
					}
					if diffRows(query.Rows, mutated, query.Unordered) == "" {
						t.Fatalf("flipped reciprocal sign at row %d slot %d passed the expectation", row, slot)
					}
				}
			}
		})
	}
}
