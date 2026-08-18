package executor

import (
	"bytes"
	"context"
	"fmt"
	"math"

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
// (ImplementDistinctFinalRule) sets RecordQueryDistinctPlan.streaming only when
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
	// base is the COMMITTED set this page resumed from, shared and READ-ONLY:
	// this cursor never writes to it, which is what makes re-executing a page
	// idempotent when the FDB retry loop replays the same continuation. Nil on
	// the scratch-less path, where seen holds everything.
	base *seenLayer
	// seen is this page's PRIVATE delta — the keys first sighted on this page.
	// Charged and released per page. On the scratch-less path it is instead the
	// whole set, rebuilt from the continuation bytes.
	seen *boundedSet[string]
	// keyer selects the identity being deduplicated. Value DISTINCT uses the
	// packed positional row; unordered primary-key DISTINCT uses the packed
	// QueryResult.PrimaryKey. Continuation and memory behavior are shared.
	keyer queryResultDistinctKeyer
	// order is THIS PAGE's new keys in INSERTION order (append-only), or the
	// whole set on the scratch-less path. seen (a map) answers Contains in
	// O(1); order gives each continuation an O(1), reallocation-safe SNAPSHOT:
	// it captures len(order), and order[:n] is an immutable prefix (append
	// never mutates earlier elements), so a continuation names exactly the keys
	// emitted through its own result regardless of when ToBytes runs or how far
	// the cursor later advances — no lazy read of the live set (the streaming
	// path's eager packLast, done symmetrically here). Insertion order also
	// makes the encoding deterministic without a sort.
	order  []string
	closed bool
	// scratch, when non-nil, is where a continuation publishes this page's
	// delta over base instead of serializing the whole set. adoptedToken is the
	// entry this cursor resumed from; it is recorded on every state this cursor
	// publishes, and is dropped only when one of those successors is itself
	// ADOPTED — minting a successor proves nothing, because the attempt that
	// minted it can still fail and its retry needs this very entry.
	scratch      *ExecutionScratch
	adoptedToken int64
	// entry/entryToken are this cursor's SINGLE scratch entry, shared by every
	// continuation it mints (each carrying its own prefix length).
	entry      *distinctResumeState
	entryToken int64
	// handed is how much of this cursor's delta charge the entry has taken over.
	// It is NOT the same as seen.Charged(): the hand-over happens when a
	// continuation is serialized, and the cursor goes on sighting keys after the
	// last one. The difference is what the cursor still owns and must release at
	// close — retirement gives back only what the entry recorded.
	handed int64
	// resumeBytes/resumeInner are the continuation this cursor was resumed
	// from. A page that consumed no row AND left the inner at the byte-identical
	// position it started at re-emits resumeBytes verbatim instead of minting:
	// identical logical state must serialize to identical bytes, or the paging
	// loop's stall detector (bytes.Equal against the previous continuation,
	// cascades_generator.go:2025) can never fire and the statement spins
	// fetching empty pages forever. Inner cursors really do report an unchanged
	// non-end position (positionReplayCursor re-emits its incoming token on a
	// truncated replay, and emits StartContinuation before its first row), so
	// this is a live shape, not a hypothetical.
	resumeBytes []byte
	resumeInner []byte
	// lastNoNext replays the terminal result on a contract-violating re-call.
	lastNoNext *recordlayer.RecordCursorResult[QueryResult]
	// narrowed, when non-nil, restricts the dedup to the EXEMPT SUBSET. See
	// narrowedDedup.
	narrowed *narrowedDedup
}

// narrowedDedup is the R3 residual: the planner proved that a secondary UNIQUE
// index already guarantees at most one row per key value EXCEPT on its exempt
// set — the rows carrying a NULL key component (under NULLS DISTINCT the
// uniqueness check is skipped, so one NULL prefix legitimately holds arbitrarily
// many entries) or a NaN one (raw encodings the index holds as distinct tuple
// keys, which packedDedupKey canonicalizes to a single value).
//
// So only exempt rows need deduplicating. A non-exempt row can duplicate neither
// another non-exempt row (the index's uniqueness) nor an exempt one (exemptness
// is a property of the key value, so their keys differ by construction), and it
// passes through with nothing retained and no key packed.
//
// The seen-set is therefore a SUBSET of the full one on every input — identical
// where every row is exempt, EMPTY on the ordinary case of a nullable column
// holding no NULLs. There is no input on which this is worse, which is why it
// replaces the full dedup unconditionally rather than being costed against it.
type narrowedDedup struct {
	// exemptSlots are the dedup-key slot positions holding the proving index's
	// key components. Nil means test EVERY slot: a conservative
	// over-approximation of the exempt set, which retains more rows than
	// necessary and stays sound. Under-approximating would drop rows, so the
	// unstatable case fails toward the full behaviour.
	exemptSlots []int
}

// isExempt reports whether a row falls in the proving index's exempt set and so
// must enter the seen-set.
//
// The test runs on the row's RAW SLOTS, BEFORE any dedup key is packed. That
// ordering is the whole performance argument: a non-exempt row — every row, on
// an ordinary table — costs one nil/NaN check and never reaches the tuple
// encoder or the set. An implementation that packs first and tests after is
// correct and delivers almost none of the benefit.
//
// Signed zero is deliberately NOT exempt. packedDedupKey packs -0.0 and +0.0
// verbatim and the index holds them as two keys, so the two agree: both the
// operator and the elision keep the pair. It is only NaN where storage identity
// is FINER than the dedup key's value identity.
func (n *narrowedDedup) isExempt(row QueryResult) bool {
	slots := row.Positional.Slots
	// len, not a nil check. An EMPTY non-nil slice states "no slot is ever
	// exempt", which makes every row pass through retaining nothing — the
	// operator emits duplicates. That is the one direction this type must never
	// take, and it differs from the nil case by nothing a reader would notice.
	// Both spellings mean "no statable positions", and both route to the
	// test-every-slot over-approximation.
	if len(n.exemptSlots) == 0 {
		for _, slot := range slots {
			if slotIsExempt(slot) {
				return true
			}
		}
		return false
	}
	for _, position := range n.exemptSlots {
		if position < 0 || position >= len(slots) {
			// A position the row cannot supply states nothing, so the row is
			// treated as exempt and deduplicated — the full behaviour, which is
			// the direction that cannot drop a row.
			return true
		}
		if slotIsExempt(slots[position]) {
			return true
		}
	}
	return false
}

func slotIsExempt(slot any) bool {
	switch typed := slot.(type) {
	case nil:
		// NULL: the uniqueness check was skipped for this entry entirely.
		return true
	case float64:
		return math.IsNaN(typed)
	case float32:
		return math.IsNaN(float64(typed))
	}
	return false
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
		// R3: a non-exempt row is provably unique already, so it is emitted
		// without packing a key or touching the seen-set. This is the residual
		// dedup's entire cost on an ordinary table.
		if c.narrowed != nil && !c.narrowed.isExempt(row) {
			wrapped, werr := c.wrapContinuation(result.GetContinuation())
			if werr != nil {
				return recordlayer.RecordCursorResult[QueryResult]{}, werr
			}
			return recordlayer.NewResultWithValue(row, wrapped), nil
		}
		if c.keyer == nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, fmt.Errorf(
				"distinct-hash cursor: nil key function",
			)
		}
		key, err := c.keyer(row)
		if err != nil {
			return recordlayer.RecordCursorResult[QueryResult]{}, err
		}
		// A key in the COMMITTED set was emitted by an earlier page. Checked
		// before the delta and WITHOUT touching it: the committed set is shared
		// with the entry a retry of this page would re-adopt, so this page must
		// only ever read it.
		if c.base.Contains(key) {
			continue
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

// parkSeen publishes THIS CURSOR's delta over the committed base and returns
// the token naming it. Returns 0 when the execution carries no scratch, which
// routes the continuation to the self-contained (quadratic) encoding.
//
// Publishing happens at most ONCE per cursor. Every continuation this cursor
// produces is a prefix of the same order slice, so they all name this one entry
// and carry their own prefix length; an enclosing operator that serializes a
// child continuation on every emitted row (limitEnvelopeCursor) therefore adds
// one scratch entry, not one per row, while each of those continuations stays
// independently resumable.
//
// Nothing is evicted here. The attempt doing the publishing may still fail, and
// its retry resumes from c.adoptedToken — so that entry has to survive until a
// SUCCESSOR is adopted (ExecutionScratch rule 2).
func (c *distinctHashCursor) parkSeen() int64 {
	if c.scratch == nil {
		return 0
	}
	if c.entry == nil {
		c.entry = &distinctResumeState{base: c.base, prev: c.adoptedToken}
		c.entryToken = c.scratch.parkDistinct(c.entry, c.seen.st)
	}
	// Hand this page's delta charge to the entry. The entry outlives the cursor,
	// so the bytes must stay charged until the entry retires — otherwise an
	// accumulating shape is invisible to the budget, which is what let a
	// per-outer-row leak grow silently.
	//
	// Tracked as a DELTA against handed rather than assigned, so the hand-over
	// and the close-time release of the remainder always partition the cursor's
	// charge exactly: a park that runs after the close (an enclosing
	// continuation serialized late) hands over nothing, instead of claiming
	// bytes the close already returned.
	c.entry.charged += c.seen.Charged() - c.handed
	c.handed = c.seen.Charged()
	// Re-publish the live slice: append REALLOCATES, so the entry would
	// otherwise keep pointing at a stale array and a later continuation's
	// prefix would name keys the entry cannot see. Copying nothing here is the
	// point — only the slice header is refreshed.
	c.entry.order = c.order
	return c.entryToken
}

// releaseUnhanded returns the delta charge this cursor still OWNS — everything
// it sighted beyond what its entry took over — and marks it handed so it can be
// returned exactly once.
//
// It is what the close hook releases. A cursor that parked keeps sighting keys
// after its last continuation was serialized (a filter draining a rejected tail
// before a correlated inner exhausts is the ordinary way there), and retirement
// gives back only what the ENTRY recorded, so releasing nothing at all left
// that tail charged to the statement for its whole life — repeated inners then
// fail a valid query on the budget for bytes nothing holds. Releasing
// seen.Charged() instead would free the entry's half too, and an accumulating
// shape would grow invisibly again; the split is what makes both true at once.
func (c *distinctHashCursor) releaseUnhanded() int64 {
	rest := c.seen.Charged() - c.handed
	c.handed = c.seen.Charged()
	return rest
}

// Close does NOT retire this cursor's scratch entry, and must not: closing is a
// CURSOR lifecycle event, and retirement follows NAMEABILITY. Even exhaustion
// does not imply un-nameability — aggregateCursor.emitFinal emits its final row
// carrying a RETAINED earlier inner continuation (streaming_cursors.go:393-401),
// so a token minted before this cursor exhausted is still named after it, and
// reclaiming here made that valid continuation fail to resume. Retirement is
// ExecutionScratch.SweepAfterPage's job.
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
	if c := d.cursor; c != nil && c.scratch != nil && d.n == 0 &&
		c.resumeBytes != nil && bytes.Equal(d.inner, c.resumeInner) {
		// NO PROGRESS: not one key sighted, and the inner is at the byte-
		// identical position this cursor resumed from. The logical state is the
		// one the incoming continuation already describes, so hand back exactly
		// those bytes. Minting a fresh token here would make two identical
		// states serialize differently and blind the paging loop's stall
		// detector, which compares bytes — the statement would fetch empty
		// pages forever instead of reporting the resource limit.
		return c.resumeBytes, nil
	}
	if d.token == 0 && d.cursor != nil {
		d.token = d.cursor.parkSeen()
	}
	if d.token != 0 {
		token := d.token
		n := int32(d.n)
		cont := &gen.DistinctHashContinuation{
			InnerContinuation: d.inner,
			StateToken:        &token,
			StateDeltaN:       &n,
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
