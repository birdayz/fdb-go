package values

import (
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"
)

// datumBinder binds one correlation to a DATUM — a value that is not an
// OrdinalRow. This is the runtime shape the leg adapter produces for a
// bare-scalar `_0` carrier: it unwraps the one-slot row and binds Slots[0]
// directly (executor.isBareScalarRow), so a STRUCT unnest element arrives here
// as one raw proto message rather than as a row of its members.
type datumBinder struct {
	id    CorrelationIdentifier
	datum any
}

func (b *datumBinder) GetCorrelationBinding(id CorrelationIdentifier) (any, bool) {
	if id == b.id {
		return b.datum, true
	}
	return nil, false
}

// innerFixtureMessage returns the nested `Inner` message (sub = 42) on its own —
// the element a struct-array unnest binds, as opposed to the Outer record that
// carries it.
func innerFixtureMessage(t *testing.T) protoreflect.Message {
	t.Helper()
	outer := nestedFixtureMessage(t)
	recFD := outer.Descriptor().Fields().ByName("rec")
	return outer.Get(recFD).Message()
}

// TestFieldValue_DatumBinding_AppliesPathRemainder pins that a correlation bound
// to a DATUM still applies the accessors BEYOND the root.
//
// A datum binding IS the root read — the leg adapter already consumed the slot-0
// carrier — so a path's remainder is the part that still has work to do.
// Returning the datum whole silently dropped it, and the drop was invisible to
// every single-accessor reference (for which the remainder is empty). The shape
// that exposed it is a struct-element lateral unnest: `orders.items AS i` binds
// one proto message under `I`, and `i.sku` is the two-accessor path `/I/SKU`, so
// the whole element was served where the member was asked for. In a PREDICATE
// that compares to a string the whole element is never equal, so
// `WHERE i.sku = 'x'` returned ZERO rows rather than a wrong one.
//
// Both correlated arms are driven separately — *RowEvalContext carrying a
// binder, and a bare CorrelationBinder — because they are two distinct sites
// with the same defect, and a test that reddens only one of them would leave the
// other free to regress.
func TestFieldValue_DatumBinding_AppliesPathRemainder(t *testing.T) {
	t.Parallel()
	corr := NamedCorrelationIdentifier("I")
	elem := innerFixtureMessage(t)

	// `/I/SUB`: the root names the binding (consumed by the binding itself),
	// SUB names the member. Unpinned, exactly as the resolver mints it.
	ref := &FieldValue{
		Field: "SUB",
		Typ:   NullableLong,
		Child: NewQuantifiedObjectValueOfType(corr, UnknownType),
		Resolved: &FieldPath{Accessors: []ResolvedAccessor{
			{Field: "I", Ordinal: 0},
			{Field: "SUB", Ordinal: 0},
		}},
	}

	t.Run("RowEvalContext correlation arm", func(t *testing.T) {
		t.Parallel()
		got, err := ref.Evaluate(&RowEvalContext{
			Correlations: &datumBinder{id: corr, datum: elem},
		})
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if got != int64(42) {
			t.Errorf("got %#v, want int64(42) — the datum's SUB member. A proto message "+
				"here means the path remainder was dropped and the whole element was served.", got)
		}
	})

	t.Run("bare CorrelationBinder arm", func(t *testing.T) {
		t.Parallel()
		got, err := ref.Evaluate(&datumBinder{id: corr, datum: elem})
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if got != int64(42) {
			t.Errorf("got %#v, want int64(42) — the datum's SUB member. A proto message "+
				"here means the path remainder was dropped and the whole element was served.", got)
		}
	})

	// The SINGLE-accessor reference is the datum convention itself: a bare
	// scalar unnest element (`t.arr AS v`, `WHERE v = 1`) has no remainder, and
	// must still resolve to the datum untouched. This is what makes applying the
	// remainder a strict extension rather than a change of contract — assert it
	// on a VALUE, not merely as agreement with the arm above.
	t.Run("single-accessor reference still serves the whole datum", func(t *testing.T) {
		t.Parallel()
		scalar := &FieldValue{
			Field:    "V",
			Typ:      NullableLong,
			Child:    NewQuantifiedObjectValueOfType(corr, UnknownType),
			Resolved: &FieldPath{Accessors: []ResolvedAccessor{{Field: "V", Ordinal: 0}}},
		}
		got, err := scalar.Evaluate(&RowEvalContext{
			Correlations: &datumBinder{id: corr, datum: int64(7)},
		})
		if err != nil {
			t.Fatalf("evaluate: %v", err)
		}
		if got != int64(7) {
			t.Errorf("got %#v, want int64(7) — a remainder-free path serves the datum verbatim", got)
		}
	})
}
