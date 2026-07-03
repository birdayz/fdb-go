package values

import "fmt"

// AssertOrdinalJoinSeed is the LOUD RFC-173 ordinal-join seed-shape validator:
// the Slice 2 translator calls it on every ordinal join RC it builds, where
// the pristine shape IS guaranteed by construction — every field a BAKED
// FieldValue over a leg QuantifiedObjectValue flowing a *RecordType, exactly
// TWO consecutive full-coverage leg runs with baked ordinals 0..width-1
// ascending. Any violation panics: at the SEED a malformed ordinal RC is
// unconditionally a planner bug (review W3a-1 ruling: strictness lives
// seed-time, where legitimate result-value rewrites — wrapper merges, folded
// projections, partial coverage — cannot yet have happened; the executor's
// cursor-side ordinalJoinSpans probe DECLINES those shapes, never panics).
//
// Lives in values (not executor) because the TRANSLATOR is the caller of
// record — the standing review condition on the W3b seed: a seed flip that
// lands without this assert is a NAK on sight.
func AssertOrdinalJoinSeed(rc *RecordConstructorValue) {
	if rc == nil || len(rc.Fields) == 0 {
		panic("RFC-173 ordinal join seed malformed: empty RC")
	}
	type run struct {
		alias   CorrelationIdentifier
		legType *RecordType
		width   int
	}
	var runs []run
	for i, f := range rc.Fields {
		fv, isFV := f.Value.(*FieldValue)
		if !isFV || fv.Resolved == nil || !fv.Resolved.FrontierPinned {
			// FrontierPinned required: the seed's constructor is
			// NewFieldValueOfOrdinal, which pins — an UNPINNED baked node here
			// (the recursive-CTE wrap shape) is not a join-seed reference and
			// its presence means the wrong constructor built the seed.
			panic(fmt.Sprintf("RFC-173 ordinal join seed malformed: field %d (%q) is %T (baked=%v) — the seed bakes EVERY leg column with the frontier-pinned constructor", i, f.Name, f.Value, isFV && fv.Resolved != nil))
		}
		acc, single := fv.Resolved.Single()
		if !single {
			// The S2-era seed is SINGLE-accessor by construction (contract
			// ruling #2); a fused multi-accessor path in a join seed means a
			// compose fired where it must not (the W1 bake gate failed).
			panic(fmt.Sprintf("RFC-173 ordinal join seed malformed: field %d (%q) carries a %d-accessor path — the seed bakes single-accessor leg references only", i, f.Name, len(fv.Resolved.Accessors)))
		}
		qov, isQOV := fv.Child.(*QuantifiedObjectValue)
		if !isQOV {
			panic(fmt.Sprintf("RFC-173 ordinal join seed malformed: field %d (%q) is baked over a %T child, want *QuantifiedObjectValue (the leg reference)", i, f.Name, fv.Child))
		}
		legType, isRT := qov.Type().(*RecordType)
		if !isRT {
			panic(fmt.Sprintf("RFC-173 ordinal join seed malformed: field %d (%q) leg %s flows %T, want *RecordType", i, f.Name, qov.Correlation, qov.Type()))
		}
		ord := acc.Ordinal
		if len(runs) == 0 || runs[len(runs)-1].alias != qov.Correlation {
			if ord != 0 {
				panic(fmt.Sprintf("RFC-173 ordinal join seed malformed: leg %s run starts at field %d with baked ordinal %d, want 0 — run ordinals must be exactly 0..width-1 ascending", qov.Correlation, i, ord))
			}
			runs = append(runs, run{alias: qov.Correlation, legType: legType, width: 1})
		} else {
			cur := &runs[len(runs)-1]
			if ord != cur.width {
				panic(fmt.Sprintf("RFC-173 ordinal join seed malformed: leg %s field %d baked at ordinal %d, want %d — run ordinals must be exactly 0..width-1 ascending (no gaps, no reorders)", qov.Correlation, i, ord, cur.width))
			}
			cur.width++
		}
	}
	if len(runs) < 2 {
		panic(fmt.Sprintf("RFC-173 ordinal join seed malformed: %d leg runs — a join seed concatenates at least two legs (the S3 fulcrum lifted the exactly-2 wedge)", len(runs)))
	}
	for _, r := range runs {
		if r.width != len(r.legType.Fields) {
			panic(fmt.Sprintf("RFC-173 ordinal join seed malformed: leg %s run covers %d columns but its leg type has %d — the seed concatenates FULL legs", r.alias, r.width, len(r.legType.Fields)))
		}
	}
}
