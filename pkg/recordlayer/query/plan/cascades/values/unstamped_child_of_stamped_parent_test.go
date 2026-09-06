package values

import (
	"strings"
	"testing"
)

// TestAStampedParentWithAnUnstampedChildFailsTheQuery pins what happens when
// the plan-time bake reaches a parent record constructor and NOT its record
// typed child.
//
// That combination is constructible because the two facts are independent. A
// parent's descriptor is synthesised from its own inferred type, which already
// carries the child's shape, so the parent can be stamped without the child's
// own constructor having been stamped. The plan walk is pre-order, so a parent
// is stamped before the subtrees under it are visited, and a duplicate-name row
// anywhere in between poisons the repository for everything after it (TODO.md,
// "A join row that names one field twice leaves its plan's rows unstamped").
//
// The cost here is NOT the one the booking describes. Everywhere else an
// unstamped constructor degrades a struct to a raw map and keeps its values —
// descriptor identity, not data. A stamped parent has no such fallback: it
// builds a message, and its child hands it a map, which cannot be stored in a
// message field. The query FAILS instead of answering in a weaker type.
//
// This test exists because a comment claimed the surrounding code was "only on
// the stamped path by construction". That is true of the PARENT and says
// nothing about the child, and the difference is the difference between a
// degraded answer and no answer. Pinned rather than reasoned about.
func TestAStampedParentWithAnUnstampedChildFailsTheQuery(t *testing.T) {
	t.Parallel()

	child := NewRecordConstructorValue(
		RecordConstructorField{Name: "X", Value: NewBooleanValue(true)},
	)
	parent := NewRecordConstructorValue(
		RecordConstructorField{Name: "CH", Value: child},
	)
	stampRecordConstructorForMessageTest(t, parent)

	if child.MessageDescriptor() != nil {
		t.Fatal("the child was stamped too, so this test no longer builds the mixed shape it names")
	}
	if parent.MessageDescriptor() == nil {
		t.Fatal("the parent is unstamped, so this test measures the ordinary degraded path instead")
	}

	got, err := parent.Evaluate(nil)
	if err == nil {
		t.Fatalf("a stamped parent over an unstamped child evaluated to %#v with no error — the "+
			"mixed shape now answers, so the failure this pins is gone and the homes that say the "+
			"cost is descriptor identity rather than data are right without qualification", got)
	}
	if !strings.Contains(err.Error(), "in message field") {
		t.Fatalf("a stamped parent over an unstamped child failed with %v, want the "+
			"cannot-store-in-message-field refusal: a different error means the mixed shape is "+
			"refused somewhere else now and this pin no longer describes where", err)
	}
}
