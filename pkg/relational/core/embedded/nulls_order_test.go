package embedded

// RFC-164 §5: ORDER BY ... NULLS LAST/FIRST must not be silently dropped. A
// forward index scan provides ASC NULLS FIRST; it must NOT satisfy an explicit
// ASC NULLS LAST request, so the InMemorySort is retained (else rows come back
// NULLs-first — wrong order).

import (
	"strings"
	"testing"
)

func TestNullsOrder_ExplicitPlacementRetainsSort(t *testing.T) {
	t.Parallel()
	const schema = `
CREATE TABLE T (id BIGINT NOT NULL, a BIGINT, b BIGINT, PRIMARY KEY (id))
CREATE INDEX idx_ab ON T(a, b)`

	// Ascending natural placement (NULLS FIRST) IS satisfied by the forward index
	// scan → sort elided. The fix must stay surgical: it must NOT start retaining
	// the sort for these. (DESC isn't reverse-scan-elided on this data-access path
	// independently of NULLs, so it isn't a clean elision control here.)
	for _, tc := range []struct{ name, sql string }{
		{"asc_default", "SELECT id FROM t WHERE a = 5 ORDER BY b"},
		{"asc_explicit_nulls_first", "SELECT id FROM t WHERE a = 5 ORDER BY b ASC NULLS FIRST"},
	} {
		t.Run(tc.name+"_elided", func(t *testing.T) {
			plan, err := PlanQueryForTest(tc.sql, schema, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			t.Logf("plan: %s", plan)
			if strings.Contains(plan, "InMemorySort") {
				t.Errorf("natural NULL placement should be satisfied by the index (no InMemorySort): %s\n  sql: %s", plan, tc.sql)
			}
		})
	}

	// Non-natural placements cannot be served by the index ordering → sort kept.
	for _, tc := range []struct{ name, sql string }{
		{"asc_nulls_last", "SELECT id FROM t WHERE a = 5 ORDER BY b ASC NULLS LAST"},
		{"desc_nulls_first", "SELECT id FROM t WHERE a = 5 ORDER BY b DESC NULLS FIRST"},
	} {
		t.Run(tc.name+"_retained", func(t *testing.T) {
			plan, err := PlanQueryForTest(tc.sql, schema, nil)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			t.Logf("plan: %s", plan)
			if !strings.Contains(plan, "InMemorySort") {
				t.Errorf("explicit non-natural NULL placement must retain the sort (InMemorySort), got: %s\n  sql: %s", plan, tc.sql)
			}
		})
	}

	// Multi-key: a non-natural NULL placement on ANY key (here the trailing b)
	// must retain the sort, while an all-natural multi-key request still elides.
	// Exercises the per-part null-placement check in rich_ordering.Satisfies.
	t.Run("multikey_trailing_nulls_last_retained", func(t *testing.T) {
		plan, err := PlanQueryForTest("SELECT id FROM t ORDER BY a, b ASC NULLS LAST", schema, nil)
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		t.Logf("plan: %s", plan)
		if !strings.Contains(plan, "InMemorySort") {
			t.Errorf("multi-key with trailing NULLS LAST must retain the sort, got: %s", plan)
		}
	})
	t.Run("multikey_all_natural_elided", func(t *testing.T) {
		plan, err := PlanQueryForTest("SELECT id FROM t ORDER BY a, b", schema, nil)
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		t.Logf("plan: %s", plan)
		if strings.Contains(plan, "InMemorySort") {
			t.Errorf("all-natural multi-key should be satisfied by the index (no InMemorySort), got: %s", plan)
		}
	})
}
