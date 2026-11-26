package yamsql

import "testing"

func TestModFloatZeroNaNExpectation(t *testing.T) {
	t.Parallel()
	scenario, err := Load("testdata/mod_float_zero.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(scenario.Tests) != 3 {
		t.Fatalf("MOD scenario has %d queries, want 3", len(scenario.Tests))
	}
	want := scenario.Tests[0].Rows
	if diff := diffRows(want, [][]any{{"NaN", "NaN", "NaN", "NaN"}}, false); diff != "" {
		t.Fatal(diff)
	}
	for slot := range 4 {
		row := []any{"NaN", "NaN", "NaN", "NaN"}
		row[slot] = "1.0"
		if diffRows(want, [][]any{row}, false) == "" {
			t.Fatalf("finite substitution in slot %d passed the NaN expectation", slot)
		}
	}
}
