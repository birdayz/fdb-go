package executor

import (
	"encoding/json"
	"strings"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"google.golang.org/protobuf/proto"
)

// sortPresenceLayout builds a two-leg merged row — L at slots [0,2), R at
// [2,4) — whose R leg is NULL-SUPPLYING, the shape a LEFT JOIN flows.
func sortPresenceLayout(t *testing.T) (values.OrdinalLayout, values.QuantifiedObjectValue, *values.RecordType) {
	t.Helper()

	// The null-supplying leg's own row must be NULLABLE — an unmatched row makes
	// the whole leg object NULL, not merely its columns.
	legType := func(prefix string, nullable bool) *values.RecordType {
		return &values.RecordType{Nullable: nullable, Fields: []values.Field{
			{Name: prefix + "_ID", Ordinal: 0, FieldType: values.NotNullLong},
			{Name: prefix + "_V", Ordinal: 1, FieldType: values.NullableLong},
		}}
	}
	carrierType := &values.RecordType{Fields: []values.Field{
		{Name: "L_ID", Ordinal: 0, FieldType: values.NotNullLong},
		{Name: "L_V", Ordinal: 1, FieldType: values.NullableLong},
		{Name: "R_ID", Ordinal: 2, FieldType: values.NullableLong},
		{Name: "R_V", Ordinal: 3, FieldType: values.NullableLong},
	}}
	left, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("L"), legType("L", false))
	if err != nil {
		t.Fatalf("left source: %v", err)
	}
	right, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier("R"), legType("R", true))
	if err != nil {
		t.Fatalf("right source: %v", err)
	}
	layout, err := values.NewOrdinalLayoutForCarrierType(carrierType, []values.OrdinalTileSpec{
		{Start: 0, Width: 4, Kind: values.OrdinalTileFlat},
	}, []values.OrdinalWindowSpec{
		{Source: left, FieldPaths: [][]int{{0}, {1}}},
		{Source: right, FieldPaths: [][]int{{2}, {3}}, NullSupplying: true},
	})
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	return layout, right, carrierType
}

// TestSortContinuationCarriesNullSupplyingMatchState pins the fact a sort
// continuation cannot recover on its own.
//
// A null-supplying leg's match state is NOT a function of the row's slots: an
// unmatched leg and a matched leg whose columns are all SQL NULL have byte-
// identical slots and opposite meaning. So a buffered row that crosses a page
// boundary must CARRY that state or the binder on the far side has nothing to
// answer with — which is exactly what happened: every buffered row of a
// `LEFT JOIN … ORDER BY` failed on resume with `null-supplying window has no
// row match state`.
//
// The all-NULL slots here are deliberate and are the whole point of the test:
// they are what makes the state unrecoverable, so a fix that "derives" presence
// from the row instead of carrying it still fails this.
func TestSortContinuationCarriesNullSupplyingMatchState(t *testing.T) {
	t.Parallel()

	layout, right, carrierType := sortPresenceLayout(t)

	for _, matched := range []bool{true, false} {
		t.Run(map[bool]string{true: "matched", false: "unmatched"}[matched], func(t *testing.T) {
			t.Parallel()

			presence, err := values.NewWindowMatchPresence([]values.WindowMatch{
				{Source: right, Matched: matched},
			})
			if err != nil {
				t.Fatalf("presence: %v", err)
			}
			row := &PositionalRow{
				Type: carrierType,
				// Every R slot is NULL in BOTH cases — the discriminator is the
				// carried state, never the contents.
				Slots:          []any{int64(1), int64(7), nil, nil},
				Layout:         layout,
				LayoutPresence: presence,
			}

			data, err := encodeSortContinuation(
				recordlayer.NewBytesContinuation([]byte("INNER")), []QueryResult{{Positional: row}}, false)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			_, got, _, err := decodeSortContinuation(data, nil, layout)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(got) != 1 || got[0].Positional == nil {
				t.Fatalf("decoded %d rows, want 1 positional", len(got))
			}
			if got[0].Positional.LayoutPresence == nil {
				t.Fatalf("resumed row carries no match state; the binder cannot read a " +
					"null-supplying window without it, which is the failure this pins")
			}
			gotMatched, known := got[0].Positional.LayoutPresence.MatchState(right)
			if !known {
				t.Fatalf("resumed match state for the null-supplying leg is UNKNOWN")
			}
			if gotMatched != matched {
				t.Fatalf("resumed match state = %v, want %v — an unmatched leg served as "+
					"matched (or the reverse) is wrong rows, not a slower plan", gotMatched, matched)
			}
		})
	}
}

// TestSortContinuationWithoutMatchStateIsRejected pins the OTHER direction: a
// token that does not carry the state is refused, never served with a guess.
//
// Both halves are needed. A decoder that accepts a stateless token and defaults
// the leg to matched (or unmatched) turns a loud resume failure into silently
// wrong rows, which is strictly worse than the bug being fixed.
func TestSortContinuationWithoutMatchStateIsRejected(t *testing.T) {
	t.Parallel()

	layout, right, carrierType := sortPresenceLayout(t)
	presence, err := values.NewWindowMatchPresence([]values.WindowMatch{{Source: right, Matched: false}})
	if err != nil {
		t.Fatalf("presence: %v", err)
	}
	row := &PositionalRow{
		Type:           carrierType,
		Slots:          []any{int64(1), int64(7), nil, nil},
		Layout:         layout,
		LayoutPresence: presence,
	}
	data, err := encodeSortContinuation(nil, []QueryResult{{Positional: row}}, true)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	// Strip the carried state, reproducing a token written before it was
	// carried — the shape whose rows used to reach the binder stateless.
	var msg gen.MemorySortContinuation
	if err := proto.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(msg.Records) != 1 {
		t.Fatalf("encoded %d records, want 1", len(msg.Records))
	}
	var sorted gen.SortedRecord
	if err := proto.Unmarshal(msg.Records[0], &sorted); err != nil {
		t.Fatalf("sorted record: %v", err)
	}
	var wrapper []json.RawMessage
	if err := json.Unmarshal(sorted.Message, &wrapper); err != nil {
		t.Fatalf("payload: %v", err)
	}
	var positional map[string]any
	if err := json.Unmarshal(wrapper[2], &positional); err != nil {
		t.Fatalf("positional payload: %v", err)
	}
	if _, carried := positional["p"]; !carried {
		t.Fatal("encoder wrote no match state for a null-supplying layout; this test " +
			"would pass vacuously, proving nothing about the decoder")
	}
	delete(positional, "p")
	rewritten, err := json.Marshal([]any{nil, false, positional})
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	sorted.Message = rewritten
	reSorted, err := proto.Marshal(&sorted)
	if err != nil {
		t.Fatalf("marshal sorted record: %v", err)
	}
	msg.Records[0] = reSorted
	stripped, err := proto.Marshal(&msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	_, _, _, err = decodeSortContinuation(stripped, nil, layout)
	if err == nil {
		t.Fatal("a token carrying no match state for a null-supplying layout decoded " +
			"successfully; it must be refused, because the only alternative to carrying " +
			"the state is guessing it, and a guessed leg is wrong rows")
	}
	if !strings.Contains(err.Error(), "match state") {
		t.Errorf("rejection said %q; it must name the missing match state so the reader "+
			"is not sent looking at column alignment", err)
	}
}
