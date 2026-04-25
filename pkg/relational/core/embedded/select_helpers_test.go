package embedded

// Direct unit tests for the pure helpers in select_helpers.go.
// Per RFC-025 §"Strong unit-test coverage per package": these
// helpers had only incidental coverage via integration tests that
// drive a JOIN / CTE / aggregate query through the executor. Direct
// tests catch regressions where they originate (e.g. a wrong
// qualified-key format from cteRowsToMaps surfaces immediately
// instead of as a downstream JOIN-resolution failure).

import (
	"database/sql/driver"
	"testing"
)

// ----- cteRowsToMaps -----------------------------------------------------

// TestCteRowsToMaps_DualKeyForm pins the contract: every output map
// holds each value under BOTH a bare-column key (`col`) and an
// alias-qualified key (`alias.col`). JOIN evaluation reads either
// form depending on whether the SQL reference was qualified.
func TestCteRowsToMaps_DualKeyForm(t *testing.T) {
	t.Parallel()
	cte := &cteData{
		cols: []string{"id", "name"},
		rows: [][]driver.Value{
			{int64(1), "alice"},
			{int64(2), "bob"},
		},
	}
	got := cteRowsToMaps(cte, "u")
	if len(got) != 2 {
		t.Fatalf("len: got %d, want 2", len(got))
	}
	for i, want := range []struct {
		id   int64
		name string
	}{{1, "alice"}, {2, "bob"}} {
		// Bare-column form.
		if v, ok := got[i]["id"]; !ok || v.(int64) != want.id {
			t.Errorf("row[%d][\"id\"]: got (%v, %v), want (%d, true)", i, v, ok, want.id)
		}
		if v, ok := got[i]["name"]; !ok || v.(string) != want.name {
			t.Errorf("row[%d][\"name\"]: got (%v, %v), want (%s, true)", i, v, ok, want.name)
		}
		// Alias-qualified form.
		if v, ok := got[i]["u.id"]; !ok || v.(int64) != want.id {
			t.Errorf("row[%d][\"u.id\"]: got (%v, %v), want (%d, true)", i, v, ok, want.id)
		}
		if v, ok := got[i]["u.name"]; !ok || v.(string) != want.name {
			t.Errorf("row[%d][\"u.name\"]: got (%v, %v), want (%s, true)", i, v, ok, want.name)
		}
	}
}

// TestCteRowsToMaps_EmptyRows pins the boundary: a CTE with cols but
// zero rows produces an empty slice, not nil and not a slice with
// empty maps.
func TestCteRowsToMaps_EmptyRows(t *testing.T) {
	t.Parallel()
	cte := &cteData{cols: []string{"id"}, rows: nil}
	got := cteRowsToMaps(cte, "x")
	if got == nil {
		t.Fatal("expected non-nil empty slice")
	}
	if len(got) != 0 {
		t.Fatalf("len: got %d, want 0", len(got))
	}
}

// TestCteRowsToMaps_NullValuesPreserved pins NULL handling: a nil
// driver.Value in the source row appears under both key forms in
// the output map. Map-key absence vs nil-value semantics matter for
// the JOIN evaluator's "column missing" detection.
func TestCteRowsToMaps_NullValuesPreserved(t *testing.T) {
	t.Parallel()
	cte := &cteData{
		cols: []string{"id", "deleted_at"},
		rows: [][]driver.Value{{int64(1), nil}},
	}
	got := cteRowsToMaps(cte, "u")
	if len(got) != 1 {
		t.Fatalf("len: got %d, want 1", len(got))
	}
	for _, key := range []string{"deleted_at", "u.deleted_at"} {
		v, ok := got[0][key]
		if !ok {
			t.Errorf("key %q: missing from map (want present + nil)", key)
		}
		if v != nil {
			t.Errorf("key %q: got %v, want nil", key, v)
		}
	}
}

// TestCteRowsToMaps_EmptyAlias pins behaviour with an empty alias
// string: the qualified form uses ".col" which is unusual but
// consistent. JOIN evaluation never produces an empty alias in
// practice — this test pins the boundary to guard against future
// callers that might accidentally pass "" and expect graceful
// handling.
func TestCteRowsToMaps_EmptyAlias(t *testing.T) {
	t.Parallel()
	cte := &cteData{
		cols: []string{"id"},
		rows: [][]driver.Value{{int64(1)}},
	}
	got := cteRowsToMaps(cte, "")
	if got[0]["id"] != int64(1) {
		t.Fatalf("bare key missing")
	}
	if _, ok := got[0][".id"]; !ok {
		t.Fatalf("empty-alias qualifier produces .col key; got %v", got[0])
	}
}

// ----- poisonAmbiguousBareCols ------------------------------------------

// TestPoisonAmbiguousBareCols_ReplacesBareWithMarker pins the core
// invariant: bare keys in the ambiguous set get replaced with
// ambiguousColumnMarker; qualified keys (alias.col) and unrelated
// bare keys are untouched.
func TestPoisonAmbiguousBareCols_ReplacesBareWithMarker(t *testing.T) {
	t.Parallel()
	row := map[string]driver.Value{
		"id":     int64(1), // ambiguous — present in both a + b
		"a.id":   int64(1),
		"b.id":   int64(99),
		"name":   "alice", // not ambiguous — should stay
		"a.name": "alice",
	}
	ambiguous := map[string]bool{"id": true}
	poisonAmbiguousBareCols(row, ambiguous)

	v, ok := row["id"].(ambiguousColumnMarker)
	if !ok {
		t.Fatalf("row[\"id\"]: got %T (%v), want ambiguousColumnMarker", row["id"], row["id"])
	}
	if v.Col != "id" {
		t.Errorf("marker.Col: got %q, want id", v.Col)
	}
	// Qualified keys unchanged.
	if row["a.id"].(int64) != 1 || row["b.id"].(int64) != 99 {
		t.Errorf("qualified id values changed: a.id=%v b.id=%v", row["a.id"], row["b.id"])
	}
	// Non-ambiguous bare key unchanged.
	if row["name"].(string) != "alice" {
		t.Errorf("non-ambiguous name changed: %v", row["name"])
	}
}

// TestPoisonAmbiguousBareCols_AmbiguousMissingFromRow pins the
// no-op case: an ambiguous-set entry that's NOT a key in the row
// is silently skipped (poisoning a non-existent key would extend
// the row, which would be wrong).
func TestPoisonAmbiguousBareCols_AmbiguousMissingFromRow(t *testing.T) {
	t.Parallel()
	row := map[string]driver.Value{"id": int64(1)}
	ambiguous := map[string]bool{"name": true} // not in row
	poisonAmbiguousBareCols(row, ambiguous)
	if _, ok := row["name"]; ok {
		t.Fatalf("missing-from-row ambiguous key should not be added")
	}
	if row["id"].(int64) != 1 {
		t.Fatalf("untouched key should be preserved")
	}
}

// TestPoisonAmbiguousBareCols_EmptyAmbiguousSet pins another no-op
// boundary: empty/nil ambiguous set leaves the row entirely
// untouched.
func TestPoisonAmbiguousBareCols_EmptyAmbiguousSet(t *testing.T) {
	t.Parallel()
	for _, ambiguous := range []map[string]bool{nil, {}} {
		row := map[string]driver.Value{"id": int64(1), "name": "alice"}
		poisonAmbiguousBareCols(row, ambiguous)
		if row["id"].(int64) != 1 || row["name"].(string) != "alice" {
			t.Fatalf("empty ambiguous set should not touch row, got %v", row)
		}
	}
}

// TestPoisonAmbiguousBareCols_PreservesQualifiedAmbiguousKey pins a
// subtlety: even if an ambiguous KEY happens to be qualified
// (`a.id`), poisoning replaces it. The caller's contract is "the
// ambiguous set names which keys to poison"; it's the caller's job
// to populate the set with bare names only. This test pins the
// helper's literal behaviour so a future caller bug that puts
// qualified names into the ambiguous set surfaces visibly.
func TestPoisonAmbiguousBareCols_PreservesQualifiedAmbiguousKey(t *testing.T) {
	t.Parallel()
	row := map[string]driver.Value{
		"id":   int64(1),
		"a.id": int64(99),
	}
	ambiguous := map[string]bool{"a.id": true} // qualified — caller bug
	poisonAmbiguousBareCols(row, ambiguous)
	if _, ok := row["a.id"].(ambiguousColumnMarker); !ok {
		t.Fatalf("helper poisons whatever's in the ambiguous set; got %T", row["a.id"])
	}
	// Bare "id" untouched.
	if row["id"].(int64) != 1 {
		t.Errorf("bare id should be untouched")
	}
}
