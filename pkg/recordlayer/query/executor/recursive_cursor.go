package executor

import (
	"bytes"
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
)

// This file is the 1:1 port of Java's RecursiveCursor
// (cursors/RecursiveCursor.java, tag 4.12.11.0): a cursor that flattens a
// tree of cursors. A root cursor seeds the output and each element maps
// into a child cursor, kept as a SINGLE STACK of open cursors (one per
// depth). Every emitted row carries a RecursiveContinuation proto — one
// LevelCursor per open depth holding that level's child continuation plus
// a CHECK VALUE for the next level's value (the primary key), so a resume
// re-descends the same path and DISCARDS the proposed deeper continuations
// when the data changed underneath (Java addChildNode's check-value
// mismatch arm) instead of resuming into the wrong subtree.

// recursiveChildCursorFn produces the children of value at 1-based depth,
// resuming from continuation (nil = start). Java ChildCursorFunction.
type recursiveChildCursorFn func(value QueryResult, depth int, continuation []byte) (recordlayer.RecordCursor[QueryResult], error)

// recursiveCheckValueFn recognizes changes to the database since a
// continuation (Java checkValueFunction) — nil result means "no check".
type recursiveCheckValueFn func(value QueryResult) []byte

// recursiveNode mirrors Java RecursiveCursor.RecursiveNode.
type recursiveNode struct {
	value      QueryResult
	checkValue []byte // pending validation on a continuation-restored node
	charged    int64  // this slot's byte charge (released on pop)

	emitPending bool

	childContBefore recordlayer.RecordCursorContinuation
	childContAfter  recordlayer.RecordCursorContinuation
	childCursor     recordlayer.RecordCursor[QueryResult]
}

type recursiveCursor struct {
	childCursorFn recursiveChildCursorFn
	checkValueFn  recursiveCheckValueFn
	nodes         []*recursiveNode
	currentDepth  int
	preorder      bool

	// The node stack is a DEPTH-growing buffer holding one parent row per
	// open level — the RFC-130 live-bytes model charges each slot on push
	// and releases it on pop, so a deep recursion of wide rows trips the
	// byte budget at its true high-water residency while a wide-shallow
	// traversal (rows visited then popped) stays cheap. nil = uncharged.
	state *recordlayer.ExecuteState

	exhausted bool
	closed    bool
}

// newRecursiveCursor mirrors Java RecursiveCursor.create: rootCursorFn is
// invoked here (depth 0); childCursorFn drives every deeper level lazily.
func newRecursiveCursor(
	rootCursorFn func(continuation []byte) (recordlayer.RecordCursor[QueryResult], error),
	childCursorFn recursiveChildCursorFn,
	checkValueFn recursiveCheckValueFn,
	continuation []byte,
	preorder bool,
	state *recordlayer.ExecuteState,
) (*recursiveCursor, error) {
	c := &recursiveCursor{childCursorFn: childCursorFn, checkValueFn: checkValueFn, preorder: preorder, state: state}
	if continuation == nil {
		rootCursor, err := rootCursorFn(nil)
		if err != nil {
			return nil, err
		}
		c.nodes = append(c.nodes, &recursiveNode{childContBefore: &recordlayer.StartContinuation{}, childContAfter: &recordlayer.StartContinuation{}, childCursor: rootCursor})
		return c, nil
	}
	var parsed gen.RecursiveContinuation
	if err := proto.Unmarshal(continuation, &parsed); err != nil {
		// Java: RecordCoreException("error parsing continuation").
		return nil, &recordlayer.ContinuationParseError{Message: "recursive cursor: error parsing continuation", RawBytes: continuation, Cause: err}
	}
	var checkValueFromPrior []byte
	for depth, level := range parsed.GetLevels() {
		var prior recordlayer.RecordCursorContinuation
		if level.Continuation != nil {
			prior = recordlayer.NewBytesContinuation(level.GetContinuation())
		} else {
			prior = &recordlayer.StartContinuation{}
		}
		if depth == 0 {
			priorBytes, _ := prior.ToBytes()
			rootCursor, err := rootCursorFn(priorBytes)
			if err != nil {
				return nil, err
			}
			c.nodes = append(c.nodes, &recursiveNode{childContBefore: prior, childContAfter: prior, childCursor: rootCursor})
		} else {
			// Java RecursiveNode.forContinuation: value arrives later via
			// withCheckedValue once the parent re-yields it.
			c.nodes = append(c.nodes, &recursiveNode{childContBefore: prior, childContAfter: prior, checkValue: checkValueFromPrior})
		}
		checkValueFromPrior = level.GetCheckValue()
	}
	if len(c.nodes) == 0 {
		// RecursiveCursor never emits a zero-level token — terminal
		// continuations serialize as nil — so a protobuf-valid token with
		// no levels (unknown-field-only, foreign, truncated) is CORRUPT:
		// treating it as exhausted silently served zero rows.
		return nil, &recordlayer.ContinuationParseError{Message: "recursive cursor: continuation carries no levels", RawBytes: continuation}
	}
	return c, nil
}

// recursiveValue is Java RecursiveCursor.RecursiveValue — depth is 1-based
// from the root; isLeaf reports no descendants.
type recursiveValue struct {
	value  QueryResult
	depth  int
	isLeaf bool
}

// OnNext is Java's recursionLoop driven synchronously.
func (c *recursiveCursor) OnNext(ctx context.Context) (recordlayer.RecordCursorResult[QueryResult], error) {
	r, err := c.onNextValue(ctx)
	if err != nil {
		return recordlayer.RecordCursorResult[QueryResult]{}, err
	}
	if !r.HasNext() {
		return recordlayer.NewResultNoNext[QueryResult](r.GetNoNextReason(), r.GetContinuation()), nil
	}
	// The DFS plan consumes only the value (Java .map(RecursiveValue::getValue)).
	return recordlayer.NewResultWithValue(r.GetValue().value, r.GetContinuation()), nil
}

func (c *recursiveCursor) onNextValue(ctx context.Context) (recordlayer.RecordCursorResult[recursiveValue], error) {
	if c.exhausted {
		return recordlayer.NewResultNoNext[recursiveValue](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
	}
	for {
		depth := c.currentDepth
		node := c.nodes[depth]
		if node.childCursor == nil {
			contBytes, err := node.childContBefore.ToBytes()
			if err != nil {
				return recordlayer.RecordCursorResult[recursiveValue]{}, err
			}
			cur, err := c.childCursorFn(node.value, depth, contBytes)
			if err != nil {
				return recordlayer.RecordCursorResult[recursiveValue]{}, err
			}
			node.childCursor = cur
		}
		childResult, err := node.childCursor.OnNext(ctx)
		if err != nil {
			return recordlayer.RecordCursorResult[recursiveValue]{}, err
		}
		if childResult.HasNext() {
			node.childContAfter = childResult.GetContinuation()
			if err := c.addChildNode(childResult.GetValue()); err != nil {
				return recordlayer.RecordCursorResult[recursiveValue]{}, err
			}
			if c.preorder && node.emitPending {
				cont, err := c.buildContinuation(depth + 1)
				if err != nil {
					return recordlayer.RecordCursorResult[recursiveValue]{}, err
				}
				node.emitPending = false
				return recordlayer.NewResultWithValue(recursiveValue{value: node.value, depth: depth, isLeaf: false}, cont), nil
			}
			continue
		}
		if !childResult.GetNoNextReason().IsSourceExhausted() {
			// Out-of-band stop: checkpoint. For preorder with an un-emitted
			// parent, build at the parent depth so resumption emits it
			// before descending.
			buildDepth := depth + 1
			if node.emitPending && c.preorder {
				buildDepth = depth
			}
			cont, err := c.buildContinuation(buildDepth)
			if err != nil {
				return recordlayer.RecordCursorResult[recursiveValue]{}, err
			}
			return recordlayer.NewResultNoNext[recursiveValue](childResult.GetNoNextReason(), cont), nil
		}
		// Child exhausted (in-band): pop this level and everything above it.
		_ = node.childCursor.Close()
		node.childCursor = nil
		c.releaseNodes(depth)
		c.currentDepth = depth - 1
		if depth == 0 {
			c.exhausted = true
			return recordlayer.NewResultNoNext[recursiveValue](recordlayer.SourceExhausted, &recordlayer.EndContinuation{}), nil
		}
		parentNode := c.nodes[depth-1]
		parentNode.childContBefore = parentNode.childContAfter
		if node.emitPending {
			cont, err := c.buildContinuation(depth)
			if err != nil {
				return recordlayer.RecordCursorResult[recursiveValue]{}, err
			}
			node.emitPending = false
			return recordlayer.NewResultWithValue(recursiveValue{value: node.value, depth: depth, isLeaf: true}, cont), nil
		}
	}
}

// addChildNode is Java's addChildNode: descend to the child value, merging
// with a continuation-restored node when its check value matches (or when
// no check is possible) and DISCARDING the proposed deeper continuations
// when the data changed underneath.
func (c *recursiveCursor) addChildNode(value QueryResult) error {
	charge, err := c.chargeNode(value)
	if err != nil {
		return err
	}
	c.currentDepth++
	if c.currentDepth < len(c.nodes) {
		continuationChildNode := c.nodes[c.currentDepth]
		discarded := false
		if c.checkValueFn != nil && continuationChildNode.checkValue != nil {
			actualCheckValue := c.checkValueFn(value)
			if actualCheckValue != nil && !bytes.Equal(continuationChildNode.checkValue, actualCheckValue) {
				c.releaseNodes(c.currentDepth)
				discarded = true
			}
		}
		if !discarded {
			// Java withCheckedValue(value, !isPreorder): a restored node in
			// PREORDER was already emitted before its children on the prior
			// page — never re-emit; in POSTORDER it still emits after them.
			continuationChildNode.value = value
			continuationChildNode.checkValue = nil
			continuationChildNode.emitPending = !c.preorder
			continuationChildNode.charged = charge
			return nil
		}
	}
	c.nodes = append(c.nodes, &recursiveNode{
		value:           value,
		emitPending:     true,
		charged:         charge,
		childContBefore: &recordlayer.StartContinuation{},
		childContAfter:  &recordlayer.StartContinuation{},
	})
	return nil
}

// chargeNode charges one stack slot's row against the byte budget.
func (c *recursiveCursor) chargeNode(value QueryResult) (int64, error) {
	// HasMemLimit, not nil: the default unlimited setting supplies a
	// non-nil state with memLimit 0, and estimateQueryResultBytes (proto
	// sizing + key packing) per node is pure waste when accounting is off.
	if c.state == nil || !c.state.HasMemLimit() {
		return 0, nil
	}
	n := estimateQueryResultBytes(value)
	if err := c.state.ChargeMemory(n); err != nil {
		return 0, err
	}
	return n, nil
}

// releaseNodes drops nodes[from:], returning their charges to the budget.
func (c *recursiveCursor) releaseNodes(from int) {
	for _, node := range c.nodes[from:] {
		if c.state != nil && node.charged > 0 {
			c.state.ReleaseMemory(node.charged)
		}
	}
	c.nodes = c.nodes[:from]
}

// buildContinuation is Java's buildContinuation: LevelCursor[i] carries
// nodes[i].childContinuationBefore, and — when a check function exists —
// the check value of nodes[i+1].value for i < depth-1.
func (c *recursiveCursor) buildContinuation(depth int) (recordlayer.RecordCursorContinuation, error) {
	if depth <= 0 {
		return nil, fmt.Errorf("recursive cursor: buildContinuation depth %d", depth)
	}
	msg := &gen.RecursiveContinuation{}
	for i := 0; i < depth; i++ {
		level := &gen.RecursiveContinuation_LevelCursor{}
		contBytes, err := c.nodes[i].childContBefore.ToBytes()
		if err != nil {
			return nil, err
		}
		if contBytes != nil {
			level.Continuation = contBytes
		}
		if c.checkValueFn != nil && i < depth-1 {
			if checkValue := c.checkValueFn(c.nodes[i+1].value); checkValue != nil {
				level.CheckValue = checkValue
			}
		}
		msg.Levels = append(msg.Levels, level)
	}
	out, err := proto.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("recursive cursor: marshal continuation: %w", err)
	}
	return recordlayer.NewBytesContinuation(out), nil
}

func (c *recursiveCursor) Close() error {
	c.closed = true
	var firstErr error
	for _, node := range c.nodes {
		if c.state != nil && node.charged > 0 {
			c.state.ReleaseMemory(node.charged)
			node.charged = 0
		}
		if node.childCursor != nil {
			if err := node.childCursor.Close(); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (c *recursiveCursor) IsClosed() bool { return c.closed }

var _ recordlayer.RecordCursor[QueryResult] = (*recursiveCursor)(nil)
