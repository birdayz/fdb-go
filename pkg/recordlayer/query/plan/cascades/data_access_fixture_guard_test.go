package cascades

// The data-access fixtures mint their ordering keys through dataAccessTestKey,
// which PANICS when the column it is handed is not a field of dataAccessTestRow.
// That guard is the only thing standing between a typo'd fixture column and a
// test that passes for the wrong reason: bakeOrderingColumnIn falls back to a
// name-only Value, both ordering comparators DECLINE a name-only key, and the
// intersection or ordering match the test is about silently stops forming.
//
// A guard nothing exercises is a guard nobody knows is wired. These tests
// construct the miss on purpose and assert the panic fires — and, first, assert
// that the fallback the guard exists to catch is really there, so the guard is
// load-bearing rather than decorative.

import (
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// dataAccessFixtureMissingColumn is a name dataAccessTestRow cannot state. It is
// checked rather than assumed: if a future fixture ever declared a column by this
// name, every assertion below would invert and the file would pass vacuously.
const dataAccessFixtureMissingColumn = "NOT_A_COLUMN_OF_THE_FIXTURE_ROW"

// recoverPanicMessage runs fn and returns the panic value's message, or "" if fn
// returned normally.
func recoverPanicMessage(fn func()) (message string, panicked bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			panicked = true
			if s, ok := recovered.(string); ok {
				message = s
			} else if err, ok := recovered.(error); ok {
				message = err.Error()
			}
		}
	}()
	fn()
	return "", false
}

// TestDataAccessFixtureKeyGuardPanicsOnAMissingColumn is the net for
// dataAccessTestKey's guard.
func TestDataAccessFixtureKeyGuardPanicsOnAMissingColumn(t *testing.T) {
	t.Parallel()

	if _, unique := uniqueUpperFieldIndex(
		dataAccessTestRow, dataAccessFixtureMissingColumn); unique {
		t.Fatalf("test setup: dataAccessTestRow now declares a column named %q, so "+
			"it is no longer a MISS and neither assertion in this file tests the "+
			"guard. Pick a name the row does not state.",
			dataAccessFixtureMissingColumn)
	}

	// The premise, asserted rather than assumed: the guard is only load-bearing
	// because the unguarded bake silently yields a key both comparators decline.
	// If bakeOrderingColumnIn ever started failing loudly on its own, the guard
	// would be redundant — but this file would still pass, so say which it is.
	unguarded := bakeOrderingColumnIn(dataAccessTestRow, dataAccessFixtureMissingColumn)
	if values.StatesOrderingColumn(unguarded) {
		t.Fatalf("test setup: bakeOrderingColumnIn(%q) now STATES a column identity "+
			"(%q) for a column the row does not declare.\n\n"+
			"The guard below then protects nothing, and every fixture key is "+
			"addressable whether or not its column exists. Find out how a missing "+
			"column acquired a layout before deleting the guard.",
			dataAccessFixtureMissingColumn, values.ExplainValue(unguarded))
	}

	message, panicked := recoverPanicMessage(func() {
		dataAccessTestKey(dataAccessFixtureMissingColumn)
	})
	if !panicked {
		t.Fatalf("dataAccessTestKey(%q) returned instead of panicking.\n\n"+
			"The column is not a field of dataAccessTestRow, so the key it minted "+
			"states no column identity and both ordering comparators decline it. A "+
			"fixture built on that key does not test less than it claims — it tests "+
			"NOTHING, and it passes. The guard is what turns that into a loud "+
			"failure naming the missing column.", dataAccessFixtureMissingColumn)
	}
	// The message is asserted, not just the panic: its whole value is telling the
	// next person WHICH column is missing and WHERE to add it. A guard that panics
	// with something unhelpful costs the debugging session it was meant to save.
	for _, want := range []string{
		dataAccessFixtureMissingColumn,
		"dataAccessTestRow",
		"newDataAccessTestRow",
		"dataAccessTestIndexNames",
		"DECLINE",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("dataAccessTestKey's panic message does not mention %q.\n\n"+
				"got: %s", want, message)
		}
	}
}

// TestDataAccessFixtureOrdinalGuardPanicsOnAMissingColumn is the net for
// dataAccessTestOrdinal's guard — the same fixture invariant reached through the
// other helper, which resolves a SLOT rather than minting a key.
//
// It is a separate net because the two helpers are the two ways a fixture can name
// a column, and only one of them was reachable through dataAccessTestKey: a
// fixture that needs a bare ordinal (to bake into dataAccessTestDomain by hand)
// calls dataAccessTestOrdinal and never touches dataAccessTestKey's guard.
//
// The returned ordinal on a miss is -1, asserted below rather than described,
// because that value is the reason the guard cannot be a silent early return: -1
// is a NAME-ONLY accessor, exactly the shape statesColumnPath refuses, so a
// fixture baking it gets an unaddressable key and a vacuous pass. The panic is
// what makes the miss visible.
func TestDataAccessFixtureOrdinalGuardPanicsOnAMissingColumn(t *testing.T) {
	t.Parallel()

	ordinal, unique := uniqueUpperFieldIndex(
		dataAccessTestRow, dataAccessFixtureMissingColumn)
	if unique {
		t.Fatalf("test setup: dataAccessTestRow now declares %q; this is no longer "+
			"a miss", dataAccessFixtureMissingColumn)
	}
	if ordinal >= 0 {
		t.Errorf("uniqueUpperFieldIndex returned ordinal %d for a column the row "+
			"does not declare.\n\n"+
			"A non-negative ordinal for a missing column is worse than the panic "+
			"this test asserts: it addresses SOME column of the row, so a fixture "+
			"that skipped the guard would compare a different column and look "+
			"correct. Expected a negative (name-only) ordinal.", ordinal)
	}

	message, panicked := recoverPanicMessage(func() {
		dataAccessTestOrdinal(dataAccessFixtureMissingColumn)
	})
	if !panicked {
		t.Fatalf("dataAccessTestOrdinal(%q) returned %d instead of panicking.\n\n"+
			"uniqueUpperFieldIndex could not state a unique slot for it, so that "+
			"value is not an addressable ordinal in dataAccessTestRow. A fixture "+
			"baking it into dataAccessTestDomain gets a key both ordering "+
			"comparators decline, and its assertions then hold over a match that "+
			"never formed.", dataAccessFixtureMissingColumn, ordinal)
	}
	for _, want := range []string{
		dataAccessFixtureMissingColumn,
		"dataAccessTestRow",
		"exactly",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("dataAccessTestOrdinal's panic message does not mention %q.\n\n"+
				"got: %s", want, message)
		}
	}
}

// TestDataAccessFixtureGuardsAcceptEveryDeclaredIndexColumn is the positive
// direction. A guard that panicked on everything would satisfy both tests above
// while making the whole fixture unusable, and the declared-index list is exactly
// the population the guard's message tells the reader to extend.
func TestDataAccessFixtureGuardsAcceptEveryDeclaredIndexColumn(t *testing.T) {
	t.Parallel()

	for _, index := range dataAccessTestIndexNames {
		for position := 0; position < dataAccessTestColumnsPerIndex; position++ {
			column := dataAccessTestColumn(index, position)

			if _, panicked := recoverPanicMessage(func() {
				dataAccessTestKey(column)
			}); panicked {
				t.Errorf("dataAccessTestKey panicked on %q, a column of declared "+
					"index %q.\n\n"+
					"Every name in dataAccessTestIndexNames must resolve, or the "+
					"guard rejects the very fixtures it was written to serve.",
					column, index)
			}
			if _, panicked := recoverPanicMessage(func() {
				dataAccessTestOrdinal(column)
			}); panicked {
				t.Errorf("dataAccessTestOrdinal panicked on %q, a column of declared "+
					"index %q", column, index)
			}
		}
	}
}
