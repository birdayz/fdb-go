package executor

import (
	"context"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// distinctStreamCursor deduplicates a SORTED QueryResult stream by the packed
// dedup key (distinctKey), emitting a row only when its key differs from the
// last EMITTED row's key. Only that last key rides the continuation (a
// gen.DedupContinuation: innerContinuation + lastValue), so a duplicate run
// whose occurrences straddle a page break is NOT re-admitted on resume — the
// resume-clean counterpart to executeDistinct's fresh-per-page hash-set
// (TODO.md C5).
//
// REQUIRES the inner ordered so equal rows are adjacent. The planner
// (ImplementDistinctFinalRule) sets RecordQueryDistinctPlan.Streaming only when
// it guarantees that ordering (an ordered index today; an inserted sort in the
// follow-up). Over UNORDERED input this would silently drop non-adjacent
// duplicates and keep adjacent ones — a wrong-rows hazard — hence the flag gate.
//
// SELECT DISTINCT is a Go-only read-side extension (Java's fdb-relational does
// not dedup it — see executeDistinct), so this continuation is GO-INTERNAL:
// there is no Java wire format to match. Shares the DedupContinuation proto and
// the adjacent-dedup shape with recordlayer.DedupCursor; specialised here to
// carry the packed dedup KEY (not a whole reconstructed row) as lastValue.
type distinctStreamCursor struct {
	inner   recordlayer.RecordCursor[QueryResult]
	lastKey string
	hasLast bool
	closed  bool
	// lastNoNext replays the terminal result on a contract-violating re-call
	// (the cached no-next result), never re-pulling the inner.
	lastNoNext *recordlayer.RecordCursorResult[QueryResult]
}

func (c *distinctStreamCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	if c.closed {
		return recordlayer.NewResultNoNext[QueryResult](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
	}
	if c.lastNoNext != nil {
		return *c.lastNoNext, nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		result, err := c.inner.OnNext(ctx)
		if err != nil {
			return result, err
		}
		if !result.HasNext() {
			wrapped, werr := c.wrapContinuation(result.GetContinuation())
			if werr != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, werr
			}
			res := recordlayer.NewResultNoNext[QueryResult](result.GetNoNextReason(), wrapped)
			c.lastNoNext = &res
			return res, nil
		}
		row := result.GetValue()
		key, err := distinctKey(row)
		if err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		// Skip a duplicate of the last emitted row (adjacent on sorted input).
		if c.hasLast && key == c.lastKey {
			continue
		}
		c.lastKey = key
		c.hasLast = true
		wrapped, werr := c.wrapContinuation(result.GetContinuation())
		if werr != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, werr
		}
		return recordlayer.NewResultWithValue(row, wrapped), nil
	}
}

// wrapContinuation encodes the inner position plus the last emitted key as a
// DedupContinuation. An ended inner needs no dedup state — resume finds nothing
// past it — so it collapses to EndContinuation, matching DedupCursor.
func (c *distinctStreamCursor) wrapContinuation(inner recordlayer.RecordCursorContinuation) (recordlayer.RecordCursorContinuation, error) {
	if inner == nil || inner.IsEnd() {
		return &recordlayer.EndContinuation{}, nil
	}
	innerBytes, err := inner.ToBytes()
	if err != nil {
		return nil, fmt.Errorf("distinct-stream continuation: %w", err)
	}
	return &distinctStreamContinuation{inner: innerBytes, lastKey: c.packLast()}, nil
}

// packLast returns the last emitted key as bytes, or nil when nothing has been
// emitted yet (mirrors DedupCursor.packLast: a nil lastValue on decode means
// "no prior key", so the first resumed row is never wrongly skipped).
func (c *distinctStreamCursor) packLast() []byte {
	if !c.hasLast {
		return nil
	}
	return []byte(c.lastKey)
}

func (c *distinctStreamCursor) Close() error {
	c.closed = true
	if c.inner != nil {
		return c.inner.Close()
	}
	return nil
}

func (c *distinctStreamCursor) IsClosed() bool { return c.closed }

// distinctStreamContinuation lazily marshals the DedupContinuation at ToBytes()
// time. IsEnd is false: a non-end inner means there may be more rows to dedup.
type distinctStreamContinuation struct {
	inner   []byte
	lastKey []byte
}

func (d *distinctStreamContinuation) ToBytes() ([]byte, error) {
	cont := &gen.DedupContinuation{
		InnerContinuation: d.inner,
		LastValue:         d.lastKey,
	}
	return cont.MarshalVT()
}

func (d *distinctStreamContinuation) IsEnd() bool { return false }

var _ recordlayer.RecordCursor[QueryResult] = (*distinctStreamCursor)(nil)
