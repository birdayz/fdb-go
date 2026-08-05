package executor

// A mid-group aggregate continuation minted by an OLDER binary must resume into
// the SAME group under this one.
//
// The aggregate continuation carries the in-progress group's packed key
// (encodeAggGroupKey). Those bytes were produced by whatever group-key encoding
// the minting binary had; the very next row after a resume is keyed by THIS
// binary's encoder. If the saved bytes are installed as the current key, the two
// encodings are compared against each other, a group break is reported that is
// not one, and the partial group is finalized on its own — the same group is
// emitted TWICE, from an upgrade alone.
//
// Canonicalizing NaN in the group key is exactly such an encoding change, and
// the ordinary case is enough to trigger it: Go's math.NaN() is
// 0x7ff8000000000001, not the canonical 0x7ff8000000000000, so ANY pre-change
// token whose in-progress group was a NaN carries a key this binary would never
// produce. It does not take an exotic payload.
//
// The fix is that withPartialState re-derives the key from the continuation's
// decoded keyVals, which ride typed and lossless for this purpose, and ignores
// the saved bytes. These tests construct the legacy token's key BY HAND — packed
// the pre-canonicalization way — because that is the only way to prove a binary
// that no longer exists is still compatible.

import (
	"math"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// groupKeyCursor builds a cursor with one grouping key over a single-column row
// type, which is all these tests need: they call withPartialState and
// packGroupKey directly rather than running the scan.
func groupKeyCursor(t *testing.T) *aggregateCursor {
	t.Helper()
	rowType := values.NewRecordType("", false, []values.Field{
		{Name: "D", FieldType: values.NullableDouble, Ordinal: 0},
	})
	qov := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("T"), rowType)
	key, err := values.NewFieldValueOfOrdinal(qov, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal: %v", err)
	}
	return &aggregateCursor{
		groupingKeys: []values.Value{key},
		aggregates:   []expressions.AggregateSpec{{Function: expressions.AggCount}},
	}
}

// legacyPackedGroupKey is the group key as the PRE-canonicalization binary
// packed it: the float's bits straight into the tuple, NaN payload and all.
func legacyPackedGroupKey(v float64) string {
	return string(tuple.Tuple{v}.Pack())
}

func TestWithPartialStateRederivesLegacyNaNGroupKey(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		nan  float64
	}{
		// The ordinary case, and the one that makes this a real upgrade
		// hazard rather than a corner: this is what math.NaN() produces, so
		// any pre-change token with a NaN group hits it.
		{name: "go_default_nan", nan: math.Float64frombits(0x7ff8000000000001)},
		// The sign-bit-set quiet NaN that (+Inf)+(-Inf) yields.
		{name: "negative_nan", nan: math.Float64frombits(0xfff8000000000000)},
		// An arbitrary payload, to show the fix is not a two-value special case.
		{name: "payload_nan", nan: math.Float64frombits(0x7ff123456789abcd)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := groupKeyCursor(t)

			legacyKey := legacyPackedGroupKey(tc.nan)
			// The canonical key THIS binary produces for the same logical
			// group — what the next row after the resume will be keyed by.
			currentKey, err := packGroupKey([]any{tc.nan})
			if err != nil {
				t.Fatalf("packGroupKey: %v", err)
			}
			if legacyKey == currentKey {
				t.Fatalf("the legacy and current encodings agree for %s, so this case cannot "+
					"express the hazard — pick a NaN whose payload is not the canonical one", tc.name)
			}

			gs := c.newGroupState()
			gs.count = 7
			c.withPartialState(legacyKey, []any{tc.nan}, gs)

			if c.currentGroupKey == legacyKey {
				t.Fatalf("the resumed cursor kept the SAVED key verbatim. The next row is keyed " +
					"by this binary's encoder, so the very next row of the SAME group reports a " +
					"group break and the group is emitted twice — a wrong answer produced by " +
					"upgrading alone")
			}
			if c.currentGroupKey != currentKey {
				t.Errorf("resumed group key = %x, want %x (re-derived from the continuation's "+
					"keyVals through this binary's encoder)", c.currentGroupKey, currentKey)
			}
			// The partial accumulator must survive intact — re-deriving the key
			// must not cost the rows already counted into the group.
			if c.current == nil || c.current.count != 7 {
				t.Errorf("partial state lost across the resume: %+v", c.current)
			}
			// And the surfaced value keeps the ORIGINAL payload: the group's
			// identity is canonicalized, the value it reports is not.
			if len(c.currentKeyVals) != 1 {
				t.Fatalf("keyVals = %+v, want one", c.currentKeyVals)
			}
			got, ok := c.currentKeyVals[0].(float64)
			if !ok || math.Float64bits(got) != math.Float64bits(tc.nan) {
				t.Errorf("surfaced group value = %#016x, want %#016x — canonicalization is the "+
					"group's IDENTITY, not the value it reports",
					math.Float64bits(got), math.Float64bits(tc.nan))
			}
		})
	}
}

// The re-derivation must not disturb the cases that were already correct.
func TestWithPartialStateRederiveIsInertOnOrdinaryKeys(t *testing.T) {
	t.Parallel()

	t.Run("finite_double_key_is_unchanged", func(t *testing.T) {
		t.Parallel()
		c := groupKeyCursor(t)
		want, err := packGroupKey([]any{2.5})
		if err != nil {
			t.Fatalf("packGroupKey: %v", err)
		}
		// For a finite double the legacy and current encodings AGREE, so
		// re-derivation must be a no-op rather than merely landing somewhere.
		if legacyPackedGroupKey(2.5) != want {
			t.Fatalf("finite keys must encode identically in both binaries; %x vs %x",
				legacyPackedGroupKey(2.5), want)
		}
		c.withPartialState(want, []any{2.5}, c.newGroupState())
		if c.currentGroupKey != want {
			t.Errorf("finite group key changed across resume: %x, want %x", c.currentGroupKey, want)
		}
	})

	// Signed zeros are two groups, and a resume must not merge them. This is the
	// direction opposite to the NaN fix, and it rides the same code.
	t.Run("signed_zeros_stay_distinct_across_resume", func(t *testing.T) {
		t.Parallel()
		negZero := math.Copysign(0, -1)
		c := groupKeyCursor(t)
		c.withPartialState(legacyPackedGroupKey(negZero), []any{negZero}, c.newGroupState())
		posKey, err := packGroupKey([]any{0.0})
		if err != nil {
			t.Fatalf("packGroupKey: %v", err)
		}
		if c.currentGroupKey == posKey {
			t.Error("a -0.0 group resumed onto the +0.0 key; Double.equals keeps them distinct, " +
				"and an aggregate index stores them as two physical entries Java reads the same way")
		}
	})

	// A scalar aggregation (COUNT(*) with no GROUP BY) has no keyVals; the
	// re-derivation must leave it completely alone rather than manufacture a key.
	t.Run("scalar_mode_keeps_its_empty_key", func(t *testing.T) {
		t.Parallel()
		c := groupKeyCursor(t)
		c.scalarMode = true
		c.withPartialState("", nil, c.newGroupState())
		if c.currentGroupKey != "" {
			t.Errorf("scalar-mode group key = %x, want empty", c.currentGroupKey)
		}
	})
}
