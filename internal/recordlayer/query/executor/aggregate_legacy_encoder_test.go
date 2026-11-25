package executor

import (
	"encoding/binary"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"google.golang.org/protobuf/proto"
)

// encodeLegacyAggregateContinuation writes the layout Go wrote before it wrote
// Java's payload, so the legacy reader stays pinned.
func encodeLegacyAggregateContinuation(
	innerCont recordlayer.RecordCursorContinuation,
	groupKey string,
	keyVals []any,
	gs *groupState,
	aggregates []expressions.AggregateSpec,
) ([]byte, error) {
	var innerBytes []byte
	if innerCont != nil {
		var err error
		innerBytes, err = innerCont.ToBytes()
		if err != nil {
			return nil, err
		}
	}

	msg := &gen.AggregateCursorContinuation{
		Continuation: innerBytes,
	}

	if gs != nil {
		var states []*gen.AccumulatorState
		as := &gen.AccumulatorState{}

		// Pack: count, then per-aggregate (count_i, sum_i, sumsI_i, allInt_i, min_i, max_i)
		as.State = append(as.State, &gen.OneOfTypedState{
			State: &gen.OneOfTypedState_Int64State{Int64State: gs.count},
		})
		for i := range aggregates {
			as.State = append(as.State, &gen.OneOfTypedState{
				State: &gen.OneOfTypedState_Int64State{Int64State: gs.counts[i]},
			})
			as.State = append(as.State, &gen.OneOfTypedState{
				State: &gen.OneOfTypedState_DoubleState{DoubleState: gs.sums[i]},
			})
			as.State = append(as.State, &gen.OneOfTypedState{
				State: &gen.OneOfTypedState_Int64State{Int64State: gs.sumsI[i]},
			})
			allIntVal := int64(0)
			if gs.allInt[i] {
				allIntVal = 1
			}
			as.State = append(as.State, &gen.OneOfTypedState{
				State: &gen.OneOfTypedState_Int64State{Int64State: allIntVal},
			})
			// min_i / max_i: one typed value each (nil → a lone nil tag). The
			// typed codec preserves int64/float64 exactly — JSON collapsed both to
			// float64 and then re-narrowed integral doubles to int64, flipping a
			// DOUBLE MIN/MAX's type on resume. An unencodable partial errors
			// (correct-or-loud) rather than silently lose its type on resume.
			minBytes, err := appendContValue(nil, gs.mins[i])
			if err != nil {
				return nil, fmt.Errorf("failed to encode MIN state for aggregate continuation: %w", err)
			}
			maxBytes, err := appendContValue(nil, gs.maxs[i])
			if err != nil {
				return nil, fmt.Errorf("failed to encode MAX state for aggregate continuation: %w", err)
			}
			as.State = append(as.State, &gen.OneOfTypedState{
				State: &gen.OneOfTypedState_BytesState{BytesState: minBytes},
			})
			as.State = append(as.State, &gen.OneOfTypedState{
				State: &gen.OneOfTypedState_BytesState{BytesState: maxBytes},
			})
		}
		// ARRAY_AGG elements ride one trailing slot per ARRAY_AGG, after the
		// fixed layout, so a continuation without one is unchanged.
		for i, agg := range aggregates {
			if agg.Function != expressions.AggArrayAgg {
				continue
			}
			elems := gs.arrays[i]
			if elems == nil {
				elems = []any{}
			}
			b, err := appendContValue(nil, elems)
			if err != nil {
				return nil, fmt.Errorf("failed to encode ARRAY_AGG state for aggregate continuation: %w", err)
			}
			as.State = append(as.State, &gen.OneOfTypedState{
				State: &gen.OneOfTypedState_BytesState{BytesState: b},
			})
		}
		// Bitmap partials use separate trailing slots; existing aggregate layouts stay unchanged.
		for i, agg := range aggregates {
			if agg.Function == expressions.AggBitmapConstructAgg {
				as.State = append(as.State, &gen.OneOfTypedState{State: &gen.OneOfTypedState_BytesState{BytesState: gs.bitmaps[i]}})
			}
		}
		states = append(states, as)

		gkBytes, err := encodeAggGroupKey(groupKey, keyVals)
		if err != nil {
			return nil, fmt.Errorf("failed to encode group key for aggregate continuation: %w", err)
		}
		msg.PartialAggregationResults = &gen.PartialAggregationResult{
			GroupKey:          gkBytes,
			AccumulatorStates: states,
		}
	}

	return proto.Marshal(msg)
}

// encodeAggGroupKey packs the in-progress group's identity into the
// PartialAggregationResult.group_key bytes: the packed group-key bytes VERBATIM
// (raw, not a JSON string — arbitrary tuple bytes must survive to match the
// recomputed key on resume) followed by the typed keyVals (carried unchanged into
// finalizeGroup's output row, so their Go type and precision are preserved). An
// unencodable keyVal errors (correct-or-loud) rather than silently lose type.
func encodeAggGroupKey(groupKey string, keyVals []any) ([]byte, error) {
	buf := binary.AppendUvarint(nil, uint64(len(groupKey)))
	buf = append(buf, groupKey...)
	return appendContSlice(buf, keyVals)
}
