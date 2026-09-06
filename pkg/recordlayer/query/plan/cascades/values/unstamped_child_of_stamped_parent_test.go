package values

import (
	"strings"
	"testing"
)

// TestAStampedParentWithAnUnstampedChildFailsTheQuery pins what happens when
// the plan-time bake reaches a parent record constructor and NOT its record
// typed child.
//
// That combination is REACHABLE, and how was measured rather than argued. It is
// not the "poisoning in between a parent and its child" this comment once
// claimed: within one value tree the walk visits a parent immediately before a
// FIRST-field child, so there is no gap there to poison. What reaches it is an
// ARRAY-ELEMENT promotion over a child whose own type cannot be synthesised,
// AND a promotion target that is itself synthesisable because unification erased
// the offending name. Both are needed: leave the same bad name on both elements
// and the target keeps it, the parent cannot stamp either, and the query answers
// as a uniform raw map instead. TODO.md, "A record literal protobuf cannot name
// reaches a stamped parent", carries the closure, and
// TestFDB_ArrayOfRecordLiteralsDescriptorOutcomes measures it with the controls
// that make it attributable — including the same array with nothing stamped
// above it, which answers RAGGED instead. Read that table rather than any
// summary of it, this one included: four rounds each summarised it wrongly, and
// the rows are what survived.
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
