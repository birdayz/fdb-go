package plans

import "sync/atomic"

// A plan's HashCodeWithoutChildren rebuilds a whole structuralKey every call, and the
// memo's insert path calls it once per MEMBER per insert: memoizeLeaf compares
// `member.HashCodeWithoutChildren() == h` for every member of every leaf Reference
// before it is allowed to try equality, and the non-leaf path does the same over its
// candidates. So the hash is not paid once per plan, it is paid once per plan per
// comparison, inside a nested loop.
//
// Java pays it once per OBJECT: AbstractRelationalExpression holds
// `Suppliers.memoize(this::computeHashCodeWithoutChildren)` and exposes it as a final
// `hashCodeWithoutChildren()`. This is that memo.
//
// # Why the memo lives behind a POINTER instead of on the plan
//
// A plan is copied by value all over this package — `cp := *p; cp.field = x; return
// &cp` is the prescribed rebuild form, enforced by pkg/docscheck's
// copy_method_rebuild gate, which fails a field-by-field literal instead because the
// literal silently drops fields added later. Two shipped bugs came from that shape.
//
// That rules out putting an atomic on the plan: every atomic type carries noCopy, so
// `go vet`'s copylocks would reject all ~57 of those copies. It also rules out a plain
// two-word memo on the plan, for a worse reason — a shallow copy inherits it verbatim,
// so the copy would read a hash computed for a DIFFERENT plan as its own, and
// `WithXxx` exists precisely to change identity-bearing fields.
//
// The resolution is that an atomic reached THROUGH a pointer is never itself copied.
// The plan holds a `*hashMemoCell`; copying the plan copies the pointer, and the
// atomic inside the cell stays put. Do not try to hoist the atomic onto the plan.
//
// # The owner check, on BOTH sides
//
// Because a copy SHARES the cell, the cached value has to name the plan it describes.
// A read is a hit only when the recorded owner is this exact plan.
//
// The write is conditional for the same reason, and this half is easy to miss: if a
// copy stored unconditionally it would overwrite the original's entry, the original
// would then miss its own owner check and store back, and the two would evict each
// other every comparison — a ping-pong in the sequential case and a live race on the
// cell in the concurrent one. So a copy that finds the cell owned by someone else
// computes its hash and simply does not memoize. That degrades a copy to the behaviour
// before this memo existed, which is the fail-safe direction.
type hashMemoCell struct {
	// state is swapped whole, so a reader never sees one plan's hash paired with
	// another plan's identity. That pairing would be a wrong ANSWER rather than a
	// wasted recompute, which is why this is atomic rather than two plain fields —
	// Reference.flowedType was the same shape as two plain fields, was safe in the
	// sequential task loop, and raced in tests that share an object across parallel
	// subtests.
	state atomic.Pointer[hashMemoState]
}

// hashMemoState pairs a hash with the plan it was computed for. Immutable once
// stored; a change means storing a fresh one.
type hashMemoState struct {
	owner any
	hash  uint64
}

// cachedStructuralHash reports the memoized hash for owner, which must be the plan
// whose HashCodeWithoutChildren is being answered.
//
// owner is an interface holding a plan POINTER, so the comparison is (type, address):
// two plans of one type differ by address, and a copy differs from its original.
// Boxing a pointer into an interface does not allocate.
func (b *PlanExprBase) cachedStructuralHash(owner any) (uint64, bool) {
	if b.hashMemo == nil {
		return 0, false
	}
	state := b.hashMemo.state.Load()
	if state == nil || state.owner != owner {
		return 0, false
	}
	return state.hash, true
}

// storeStructuralHash records hash for owner, but only when the cell is unclaimed or
// already claimed by owner — see the type doc for why an unconditional store makes two
// sharers evict each other.
// It reports whether the value was actually recorded. Every production caller
// ignores that, and should: failing to memoize is the fail-safe outcome, not an
// error. It is returned so the CAS below can be PINNED — a plain Store passes every
// answer-correctness test ever written against this cell, because both forms give
// each caller its own plan's hash and differ only in how many stores survive. That
// left the CAS a real fix with no regression sentinel, which is the same shape as
// the defects this file's tests exist to catch.
func (b *PlanExprBase) storeStructuralHash(owner any, hash uint64) bool {
	if b.hashMemo == nil {
		return false
	}
	state := b.hashMemo.state.Load()
	if state != nil && state.owner != owner {
		return false
	}
	return b.hashMemo.commit(state, owner, hash)
}

// commit publishes owner's hash, but only if the cell still holds the state the
// caller based its decision on.
//
// CompareAndSwap rather than Store, because the sequence above is check-then-act:
// two goroutines sharing a cell can both observe the same state and both write, and
// the second silently discards the first. Nothing is CORRUPTED by that — reads
// validate the owner and the state is swapped whole — so it costs a wasted recompute
// rather than a wrong answer. But the type doc claims the conditional store prevents
// a live race on the cell, and under a plain Store that claim held only
// per-observation. The CAS makes it hold globally: a store that was overtaken loses,
// and losing is the fail-safe direction (recompute, do not memoize).
//
// It is a separate method taking `prev` EXPLICITLY so the guarantee can be tested
// without racing for it. Driven through storeStructuralHash the contended
// interleaving is rare — whichever goroutine arrives first claims the cell and the
// rest decline as foreign — so a concurrent test passes under a plain Store almost
// every time, which is a sentinel that does not sentinel. Handing `prev` in makes the
// empty→first-store transition reproducible: N callers committing against the same
// observed `prev` must yield exactly one winner, and under a plain Store they all
// win.
func (c *hashMemoCell) commit(prev *hashMemoState, owner any, hash uint64) bool {
	return c.state.CompareAndSwap(prev, &hashMemoState{owner: owner, hash: hash})
}

// newHashMemoCell allocates the cell a plan's base carries for its whole life.
//
// THE POINTER IS WRITTEN EXACTLY ONCE, HERE, AND NEVER REASSIGNED. That is what lets
// the field be a plain pointer with no atomic and no noCopy: no read can race a write
// to it.
//
// State the requirement precisely, because the obvious phrasing is stricter than the
// truth and would misdirect anyone who later needs to move this. The invariant is NOT
// "written only in the constructor" — it is "never written after the object may be
// SHARED". Publication safety, not immutability. A builder doing
// `cp := *p; cp.hashMemo = newHashMemoCell(); return &cp` writes to an object no other
// goroutine can observe yet, so it would need no atomic either.
//
// That hook is real and it is deliberately not taken: giving every copy its own cell
// was measured at ~0.25% of planner time, against loosening the one field whose
// write-once discipline is the entire reason no atomic sits on the plan struct, across
// ~57 copy sites. The measurement and its derivation are in TODO.md under the item
// titled "Next lever for the planner sweep — the copies" — NOT under the memo item
// itself, which carries only the 2.8% figure. Do not re-derive it.
//
// The cell starts EMPTY. It must never be populated during construction: several plans
// finish initialising themselves after their constructor returns — the
// WithQuantifiers rebuild paths write drivingAlias / outputNameOverrides /
// distinctProofIndexName onto a freshly built plan, and those fields are in the key —
// so a hash computed inside the constructor would describe a plan that does not exist
// yet. Laziness is a correctness requirement here, not an optimisation.
//
// Those writes go through a fresh LOCAL rather than through a receiver, which is why
// pkg/docscheck's receiver-write ratchet does not report them and must not: finishing
// construction is what those methods are for. Laziness and that ratchet are two guards
// on ONE failure mode — a structural-key field changing while the plan's pointer stays
// the same, which the owner check compares identity and so cannot detect. The ratchet
// forbids it on a plan that may already be SHARED; laziness is what makes it safe on a
// plan that is not yet PUBLISHED. Remove either and the other stops being sufficient.
func newHashMemoCell() *hashMemoCell {
	return &hashMemoCell{}
}
