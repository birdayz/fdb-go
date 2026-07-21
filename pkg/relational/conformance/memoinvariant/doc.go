// Package memoinvariant is RFC-184 W4: a generative memo-invariant harness.
//
// TestCorpusPlanReachability (RFC-183) checks the right memo properties but
// only over the FIXED yamsql corpus — it can find only the shapes someone
// thought to write a query for. W4 extends the RFC-182 generative row-soundness
// idea to the MEMO level: over seeded-random (schema, data, query) shapes the
// corpus does not contain, it asserts the memo invariants directly —
// reachability, arity (child-count vs. reported quantifiers), and identity/hash
// agreement (EqualsPlanWithoutChildren-equal nodes must share
// HashCodeWithoutChildren).
//
// W4 is what LICENSES RFC-184 W2 (plans store children only as quantifiers):
// W2 is sound only if every compensating rule memoizes-then-ranges the
// quantifier over the compensated reference. Corpus reachability = 0 is
// necessary but not sufficient — it proves lockstep for the written queries,
// not for shapes nobody wrote. So this harness must land and cover the
// compensation sites (FlatMap, RecursiveDfsJoin, InJoin, UnorderedUnion,
// PredicatesFilter, Projection) before W2 collapses anything.
//
// This package is a TEST HARNESS ONLY — it contains no engine code. The
// invariant checkers and the mutation-proofs that prove they fire live in the
// package's _test.go files.
package memoinvariant
