package executor

import (
	"bytes"
	"context"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// scanRangeMaterializer materializes exactly the physical range selected by
// choices. Implementations must rebuild one tuple workspace from a compact
// evaluated representation; they must not capture one copied prefix per
// Cartesian-product branch.
type scanRangeMaterializer func(choices []uint32) (recordlayer.TupleRange, error)

// scanRangeSetSpec is the cursor-facing half of an evaluated physical range
// set. Comparison evaluation, physical-type coercion, tail construction, and
// canonical fingerprint hashing belong to the binder, not this cursor.
//
// A non-empty spec with no alternativeCounts represents exactly one leaf (the
// all-zero-length odometer position). An explicitly empty spec represents no
// leaf at all and must not provide a materializer.
type scanRangeSetSpec struct {
	fingerprint       []byte
	alternativeCounts []uint32
	empty             bool
	materialize       scanRangeMaterializer
}

// scanRangeLeafFactory opens one physical leaf at its optional inner
// continuation. The properties have the logical scan's shared ScanState but
// have Skip and ReturnedRowLimit cleared. The caller must apply skip and the
// returned-row limit once, outside this cursor and after covering/fetch/row
// shaping.
type scanRangeLeafFactory[T any] func(
	scanRange recordlayer.TupleRange,
	innerContinuation []byte,
	childProperties recordlayer.ScanProperties,
) (recordlayer.RecordCursor[T], error)

type invalidScanRangeSetSpecError struct {
	message string
}

func (e *invalidScanRangeSetSpecError) Error() string {
	return "invalid scan range set: " + e.message
}

type scanRangeSetCursorClosedError struct{}

func (*scanRangeSetCursorClosedError) Error() string { return "cursor is closed" }

// newScanRangeSetCursor constructs one logical cursor over a lazily enumerated
// set of disjoint physical ranges. It validates all static state and any
// continuation before a leaf can be materialized or opened.
//
// Skip and ReturnedRowLimit are intentionally not applied here. See
// scanRangeLeafFactory: those logical result limits belong outside the
// range-set cursor so they cannot reset per physical branch.
func newScanRangeSetCursor[T any](
	spec scanRangeSetSpec,
	continuation []byte,
	properties recordlayer.ScanProperties,
	open scanRangeLeafFactory[T],
) (recordlayer.RecordCursor[T], error) {
	if len(spec.fingerprint) == 0 {
		return nil, &invalidScanRangeSetSpecError{message: "empty fingerprint"}
	}
	for i, count := range spec.alternativeCounts {
		if count == 0 {
			return nil, &invalidScanRangeSetSpecError{
				message: fmt.Sprintf("component %d has no alternatives", i),
			}
		}
	}
	if spec.empty {
		if spec.materialize != nil {
			return nil, &invalidScanRangeSetSpecError{message: "empty spec has a materializer"}
		}
		if continuation != nil {
			return nil, scanRangeSetContinuationError(
				continuation,
				fmt.Errorf("empty range set cannot resume a physical leaf"),
			)
		}
		return recordlayer.Empty[T](), nil
	}
	if spec.materialize == nil {
		return nil, &invalidScanRangeSetSpecError{message: "non-empty spec has no materializer"}
	}
	if open == nil {
		return nil, &invalidScanRangeSetSpecError{message: "nil leaf factory"}
	}

	// ExecuteProperties is value-copied throughout execution, so normalize the
	// pointer once here and then copy the same pointer into every child. A raw
	// struct literal otherwise gives each leaf its own private limiter state.
	executeProperties := properties.ExecuteProperties
	if executeProperties.ScanState == nil {
		executeProperties.ScanState = recordlayer.NewScanLimiterState()
	}
	properties.ExecuteProperties = executeProperties
	childProperties := properties.WithExecuteProperties(executeProperties.ClearSkipAndLimit())

	fingerprint := bytes.Clone(spec.fingerprint)
	alternativeCounts := append([]uint32(nil), spec.alternativeCounts...)
	choices := make([]uint32, len(alternativeCounts))
	var innerContinuation []byte
	if continuation != nil {
		decodedChoices, decodedInner, err := parseScanRangeSetContinuation(
			continuation,
			fingerprint,
			alternativeCounts,
		)
		if err != nil {
			return nil, err
		}
		choices = decodedChoices
		innerContinuation = decodedInner
	}

	return &scanRangeSetCursor[T]{
		fingerprint:       fingerprint,
		alternativeCounts: alternativeCounts,
		materialize:       spec.materialize,
		open:              open,
		logicalProperties: properties,
		childProperties:   childProperties,
		choices:           choices,
		innerContinuation: innerContinuation,
	}, nil
}

func parseScanRangeSetContinuation(
	raw []byte,
	fingerprint []byte,
	alternativeCounts []uint32,
) ([]uint32, []byte, error) {
	var continuation gen.ScanRangeSetContinuation
	if err := continuation.UnmarshalVT(raw); err != nil {
		return nil, nil, scanRangeSetContinuationError(raw, err)
	}
	if !bytes.Equal(continuation.Fingerprint, fingerprint) {
		return nil, nil, scanRangeSetContinuationError(
			raw,
			fmt.Errorf("fingerprint does not match the evaluated range set"),
		)
	}
	if len(continuation.Choices) != len(alternativeCounts) {
		return nil, nil, scanRangeSetContinuationError(
			raw,
			fmt.Errorf(
				"choice arity %d does not match component arity %d",
				len(continuation.Choices),
				len(alternativeCounts),
			),
		)
	}
	for i, choice := range continuation.Choices {
		if choice >= alternativeCounts[i] {
			return nil, nil, scanRangeSetContinuationError(
				raw,
				fmt.Errorf(
					"choice %d at component %d is outside [0,%d)",
					choice,
					i,
					alternativeCounts[i],
				),
			)
		}
	}
	if continuation.ChildStarted == nil {
		return nil, nil, scanRangeSetContinuationError(
			raw,
			fmt.Errorf("child_started is absent"),
		)
	}
	childStarted := continuation.GetChildStarted()
	if !childStarted && continuation.InnerContinuation != nil {
		return nil, nil, scanRangeSetContinuationError(
			raw,
			fmt.Errorf("unopened child carries an inner continuation"),
		)
	}
	return append([]uint32(nil), continuation.Choices...),
		bytes.Clone(continuation.InnerContinuation),
		nil
}

func scanRangeSetContinuationError(raw []byte, cause error) error {
	return &recordlayer.ContinuationParseError{
		Message:  "scan range set: error parsing continuation",
		RawBytes: bytes.Clone(raw),
		Cause:    cause,
	}
}

type scanRangeSetCursor[T any] struct {
	fingerprint       []byte
	alternativeCounts []uint32
	materialize       scanRangeMaterializer
	open              scanRangeLeafFactory[T]

	// logicalProperties retains the limits used by the whole-range branch
	// gate. childProperties shares its ScanState but has only skip/row-limit
	// cleared; every other scan property is preserved.
	logicalProperties recordlayer.ScanProperties
	childProperties   recordlayer.ScanProperties

	choices           []uint32
	innerContinuation []byte
	active            recordlayer.RecordCursor[T]

	// initialPassUsed is page/cursor-scoped. It grants one attempt to this
	// logical range set, not one attempt per factory-created child.
	initialPassUsed bool

	closed      bool
	closeErr    error
	terminalErr error
	lastNoNext  *recordlayer.RecordCursorResult[T]
}

func (c *scanRangeSetCursor[T]) OnNext(
	ctx context.Context,
) (recordlayer.RecordCursorResult[T], error) {
	if c.closed {
		return recordlayer.RecordCursorResult[T]{}, &scanRangeSetCursorClosedError{}
	}
	if c.terminalErr != nil {
		return recordlayer.RecordCursorResult[T]{}, c.terminalErr
	}
	if c.lastNoNext != nil {
		return *c.lastNoNext, nil
	}

	for {
		if err := ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[T]{}, err
		}

		if c.active == nil {
			if c.initialPassUsed {
				if reason, limited := c.limitReason(); limited {
					return c.stopBeforeCurrentChild(reason)
				}
			} else {
				// Consume the one free attempt before materializing/opening the
				// first active leaf. An empty first leaf therefore cannot grant
				// another free attempt to the next branch.
				c.initialPassUsed = true
			}

			scanRange, err := c.materialize(c.choices)
			if err != nil {
				c.terminalErr = fmt.Errorf("scan range set: materialize choices %v: %w", c.choices, err)
				return recordlayer.RecordCursorResult[T]{}, c.terminalErr
			}
			inner := bytes.Clone(c.innerContinuation)
			active, err := c.open(scanRange, inner, c.childProperties)
			if err != nil {
				c.terminalErr = fmt.Errorf("scan range set: open choices %v: %w", c.choices, err)
				return recordlayer.RecordCursorResult[T]{}, c.terminalErr
			}
			if active == nil {
				c.terminalErr = &invalidScanRangeSetSpecError{message: "leaf factory returned a nil cursor"}
				return recordlayer.RecordCursorResult[T]{}, c.terminalErr
			}
			c.active = active
			c.innerContinuation = nil
		}

		result, err := c.active.OnNext(ctx)
		if err != nil {
			c.terminalErr = fmt.Errorf("scan range set: child choices %v: %w", c.choices, err)
			return result, c.terminalErr
		}
		if result.HasNext() {
			continuation, wrapErr := c.wrapCurrentChildContinuation(result.GetContinuation(), true)
			if wrapErr != nil {
				c.terminalErr = wrapErr
				return recordlayer.RecordCursorResult[T]{}, c.terminalErr
			}
			return recordlayer.NewResultWithValue(result.GetValue(), continuation), nil
		}

		if !result.GetNoNextReason().IsSourceExhausted() {
			continuation, wrapErr := c.wrapCurrentChildContinuation(result.GetContinuation(), true)
			if wrapErr != nil {
				c.terminalErr = wrapErr
				return recordlayer.RecordCursorResult[T]{}, c.terminalErr
			}
			stopped := recordlayer.NewResultNoNext[T](result.GetNoNextReason(), continuation)
			c.lastNoNext = &stopped
			return stopped, nil
		}

		// SourceExhausted is the only reason that advances the odometer. Close
		// the exhausted child first; a close failure is terminal and leaves
		// choices unchanged, so no later range can leak through it.
		exhausted := c.active
		c.active = nil
		if err := exhausted.Close(); err != nil {
			c.terminalErr = fmt.Errorf("scan range set: close exhausted choices %v: %w", c.choices, err)
			// Close already ran exactly once and failed. Preserve that same
			// failure for explicit, idempotent Close calls as well as OnNext;
			// clearing active must not make the resource error disappear.
			c.closeErr = c.terminalErr
			return recordlayer.RecordCursorResult[T]{}, c.terminalErr
		}
		c.innerContinuation = nil

		if !advanceScanRangeChoices(c.choices, c.alternativeCounts) {
			ended := recordlayer.NewResultNoNext[T](
				recordlayer.SourceExhausted,
				&recordlayer.EndContinuation{},
			)
			c.lastNoNext = &ended
			return ended, nil
		}
	}
}

func (c *scanRangeSetCursor[T]) stopBeforeCurrentChild(
	reason recordlayer.NoNextReason,
) (recordlayer.RecordCursorResult[T], error) {
	if c.logicalProperties.ExecuteProperties.FailOnScanLimitReached {
		return recordlayer.RecordCursorResult[T]{}, &recordlayer.ScanLimitReachedError{Reason: reason}
	}
	continuation := &scanRangeSetContinuation{
		fingerprint:  bytes.Clone(c.fingerprint),
		choices:      append([]uint32(nil), c.choices...),
		childStarted: false,
	}
	stopped := recordlayer.NewResultNoNext[T](reason, continuation)
	c.lastNoNext = &stopped
	return stopped, nil
}

// limitReason uses the same precedence as the ordinary index and aggregate
// leaf cursors: scanned records, elapsed time, then scanned bytes.
func (c *scanRangeSetCursor[T]) limitReason() (recordlayer.NoNextReason, bool) {
	executeProperties := c.logicalProperties.ExecuteProperties
	state := executeProperties.ScanState
	if executeProperties.ScannedRecordsLimit > 0 &&
		state.RecordsScanned() >= executeProperties.ScannedRecordsLimit {
		return recordlayer.ScanLimitReached, true
	}
	if executeProperties.TimeLimit > 0 && state.Elapsed() >= executeProperties.TimeLimit {
		return recordlayer.TimeLimitReached, true
	}
	if executeProperties.ScannedBytesLimit > 0 &&
		state.BytesScanned() >= executeProperties.ScannedBytesLimit {
		return recordlayer.ByteLimitReached, true
	}
	return recordlayer.SourceExhausted, false
}

func (c *scanRangeSetCursor[T]) wrapCurrentChildContinuation(
	inner recordlayer.RecordCursorContinuation,
	childStarted bool,
) (recordlayer.RecordCursorContinuation, error) {
	if inner == nil {
		return nil, fmt.Errorf("scan range set: child returned a nil continuation")
	}
	if inner.IsEnd() {
		return nil, fmt.Errorf("scan range set: live or limited child returned an end continuation")
	}
	innerBytes, err := inner.ToBytes()
	if err != nil {
		return nil, fmt.Errorf("scan range set: serialize inner continuation: %w", err)
	}
	return &scanRangeSetContinuation{
		fingerprint:       bytes.Clone(c.fingerprint),
		choices:           append([]uint32(nil), c.choices...),
		innerContinuation: bytes.Clone(innerBytes),
		childStarted:      childStarted,
	}, nil
}

func (c *scanRangeSetCursor[T]) Close() error {
	if c.closed {
		return c.closeErr
	}
	c.closed = true
	if c.active != nil {
		active := c.active
		c.active = nil
		c.closeErr = active.Close()
	}
	return c.closeErr
}

func (c *scanRangeSetCursor[T]) IsClosed() bool { return c.closed }

// advanceScanRangeChoices mutates a mixed-radix odometer in lexicographic
// order. The final component changes fastest. A zero-component odometer has
// exactly one state, so it cannot advance.
func advanceScanRangeChoices(choices, alternativeCounts []uint32) bool {
	for i := len(choices) - 1; i >= 0; i-- {
		if choices[i]+1 < alternativeCounts[i] {
			choices[i]++
			for reset := i + 1; reset < len(choices); reset++ {
				choices[reset] = 0
			}
			return true
		}
	}
	return false
}

type scanRangeSetContinuation struct {
	fingerprint       []byte
	choices           []uint32
	innerContinuation []byte
	childStarted      bool
}

func (c *scanRangeSetContinuation) ToBytes() ([]byte, error) {
	childStarted := c.childStarted
	continuation := &gen.ScanRangeSetContinuation{
		Fingerprint:       c.fingerprint,
		Choices:           c.choices,
		InnerContinuation: c.innerContinuation,
		ChildStarted:      &childStarted,
	}
	data, err := continuation.MarshalVT()
	if err != nil {
		return nil, fmt.Errorf("scan range set: marshal continuation: %w", err)
	}
	return data, nil
}

func (*scanRangeSetContinuation) IsEnd() bool { return false }

var _ recordlayer.RecordCursor[struct{}] = (*scanRangeSetCursor[struct{}])(nil)
