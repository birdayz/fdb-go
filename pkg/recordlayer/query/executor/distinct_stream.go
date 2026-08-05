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

// distinctHashCursor is the shared resume-clean hash executor for unordered
// value DISTINCT and unordered primary-key DISTINCT. Its injected keyer picks
// the identity; the cursor charges the packed keys to a bounded set and carries
// that set across page boundaries through gen.DistinctHashContinuation, so a
// duplicate spanning a page boundary is not re-admitted. It does not require
// ordered input.
//
// The set is carried BY REFERENCE when the execution supplies an
// ExecutionScratch — the continuation names it with a token and the live set is
// handed to the next page untouched. Carrying it BY VALUE is what a
// self-contained continuation would require and is quadratic in the number of
// pages (see ExecutionScratch for the measurement); that encoding survives only
// for a scratch-less execution, which no production path performs.
//
// The set is bounded by the statement memory budget it charges against, so a
// high-cardinality DISTINCT instead fails LOUDLY on the budget (never silent
// wrong rows). This continuation is Go-internal; there is no Java wire format
// to match — Java's equivalent plan keeps no dedup state across a resume at all
// and re-admits the duplicate.
type queryResultDistinctKeyer func(QueryResult) (string, error)

type distinctHashCursor struct {
	inner recordlayer.RecordCursor[QueryResult]
	seen  *boundedSet[string]
	// keyer selects the identity being deduplicated. Value DISTINCT uses the
	// packed positional row; unordered primary-key DISTINCT uses the packed
	// QueryResult.PrimaryKey. Continuation and memory behavior are shared.
	keyer queryResultDistinctKeyer
	// order is the distinct keys in INSERTION order (append-only). seen (a map)
	// answers Contains in O(1); order gives each continuation an O(1),
	// reallocation-safe SNAPSHOT: it captures len(order), and order[:n] is an
	// immutable prefix (append never mutates earlier elements), so a
	// continuation encodes exactly the keys emitted through its own result
	// regardless of when ToBytes runs or how far the cursor later advances — no
	// lazy read of the live set (the streaming path's eager packLast, done
	// symmetrically here). Insertion order also makes the encoding deterministic
	// without a sort.
	order  []string
	closed bool
	// scratch, when non-nil, is where a continuation parks the LIVE seen-set
	// for the next page instead of serializing it. adoptedToken is the entry
	// this cursor resumed from and parkedToken the entry it last wrote; a new
	// park drops whichever of the two is outstanding, so the scratch holds at
	// most one entry per live cursor and a continuation stays resumable until
	// its own successor is minted.
	scratch      *ExecutionScratch
	adoptedToken int64
	parkedToken  int64
	// lastNoNext replays the terminal result on a contract-violating re-call.
	lastNoNext *recordlayer.RecordCursorResult[QueryResult]
}

func (c *distinctHashCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
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
		if c.keyer == nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, fmt.Errorf(
				"distinct-hash cursor: nil key function",
			)
		}
		key, err := c.keyer(row)
		if err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		// Add charges the key's bytes against the statement budget on FIRST
		// sight and returns added=false for a duplicate; a budget breach is a
		// loud error, never a silent drop.
		added, aerr := c.seen.Add(key, int64(len(key)))
		if aerr != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, aerr
		}
		if !added {
			continue
		}
		c.order = append(c.order, key)
		wrapped, werr := c.wrapContinuation(result.GetContinuation())
		if werr != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, werr
		}
		return recordlayer.NewResultWithValue(row, wrapped), nil
	}
}

// wrapContinuation snapshots the inner position plus the seen-set into a
// DistinctHashContinuation. An ended inner needs no dedup state (resume finds
// nothing past it) so it collapses to EndContinuation. The snapshot is the
// insertion-order prefix order[:n] captured by length: order is append-only, so
// that prefix is immutable and the continuation encodes exactly the keys
// emitted through THIS result — independent of when ToBytes runs or how far the
// cursor advances afterward (no aliasing of the live set).
func (c *distinctHashCursor) wrapContinuation(inner recordlayer.RecordCursorContinuation) (recordlayer.RecordCursorContinuation, error) {
	if inner == nil || inner.IsEnd() {
		return &recordlayer.EndContinuation{}, nil
	}
	innerBytes, err := inner.ToBytes()
	if err != nil {
		return nil, fmt.Errorf("distinct-hash continuation: %w", err)
	}
	return &distinctHashContinuation{
		inner:  innerBytes,
		order:  c.order,
		n:      len(c.order),
		cursor: c,
	}, nil
}

// parkSeen hands the live seen-set to the scratch under a fresh token, for the
// continuation identified by the insertion-order prefix length n. Returns 0
// when the execution carries no scratch, which routes the continuation to the
// self-contained (quadratic) encoding.
//
// n is passed rather than read from the cursor because the continuation was
// snapshotted at a specific prefix; a cursor that advanced afterward must not
// silently widen what its earlier continuation claimed to have emitted.
func (c *distinctHashCursor) parkSeen(n int) int64 {
	if c.scratch == nil {
		return 0
	}
	drop := c.parkedToken
	if drop == 0 {
		drop = c.adoptedToken
	}
	token := c.scratch.parkDistinct(&distinctResumeState{
		seen:    c.seen,
		order:   c.order,
		n:       n,
		charged: c.seen.Charged(),
		prev:    drop,
	})
	c.parkedToken = token
	return token
}

func (c *distinctHashCursor) Close() error {
	c.closed = true
	if c.inner != nil {
		return c.inner.Close()
	}
	return nil
}

func (c *distinctHashCursor) IsClosed() bool { return c.closed }

// distinctHashContinuation marshals the DistinctHashContinuation at ToBytes()
// time. With a scratch it parks the live seen-set and names it with a token, so
// the bytes are O(1) in the number of keys emitted; without one it falls back
// to writing the captured insertion-order prefix order[:n] (immutable — see
// wrapContinuation), which is self-contained and quadratic across pages.
// Insertion order is already deterministic, so no sort is needed.
//
// The token is minted at most ONCE per continuation object and cached, so
// repeated ToBytes calls are idempotent — the same bytes, no second scratch
// entry, and the first token stays valid.
type distinctHashContinuation struct {
	inner  []byte
	order  []string
	n      int
	cursor *distinctHashCursor
	token  int64
}

func (d *distinctHashContinuation) ToBytes() ([]byte, error) {
	if d.token == 0 && d.cursor != nil {
		d.token = d.cursor.parkSeen(d.n)
	}
	if d.token != 0 {
		token := d.token
		cont := &gen.DistinctHashContinuation{
			InnerContinuation: d.inner,
			StateToken:        &token,
		}
		return cont.MarshalVT()
	}
	keys := d.order[:d.n]
	seenBytes := make([][]byte, len(keys))
	for i, k := range keys {
		seenBytes[i] = []byte(k)
	}
	cont := &gen.DistinctHashContinuation{
		InnerContinuation: d.inner,
		SeenKeys:          seenBytes,
	}
	return cont.MarshalVT()
}

func (d *distinctHashContinuation) IsEnd() bool { return false }

var _ recordlayer.RecordCursor[QueryResult] = (*distinctHashCursor)(nil)
