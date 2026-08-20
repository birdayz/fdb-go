package recordlayer

import (
	"fmt"
)

// validateSlidingWindowIndexes is the port of Java's
// SlidingWindowIndexMaintainerFactory.SlidingWindowIndexValidator.validate
// (SlidingWindowIndexMaintainerFactory.java:212-236).
//
// Java reaches this validator only through the registry, which installs it
// exactly when isSlidingWindowIndex(index) holds — VECTOR type AND a row-number
// window predicate somewhere on a conjunctive path. The same gate is applied
// here, and it is why two of Java's six arms (non-vector delegate, missing
// predicate) are defensive rather than reachable: they restate the gate. They
// are ported anyway, because the gate and the validator are separate pieces of
// code and a future change to one must not silently disarm the other.
//
// A non-vector index that carries a row-number window predicate is NOT refused,
// and that is deliberate. Java does not refuse it either — the registry simply
// hands back the undecorated factory, the predicate's per-record answer is
// `true`, and the index ends up holding every record. Refusing here would make
// Go reject metadata Java accepts, i.e. make a Java-authored store unopenable,
// which is the failure wire compatibility exists to prevent. The index is still
// safe to read: the row-window arm is never folded to a tautology, and
// indexPredicateToQueryPredicate refuses to convert it, so the planner excludes
// the candidate rather than serving it as a full index.
func validateSlidingWindowIndexes(md *RecordMetaData) error {
	for _, idx := range md.indexes {
		if !isSlidingWindowIndex(idx) {
			continue
		}
		if err := validateSlidingWindowIndex(md, idx); err != nil {
			return err
		}
	}
	return nil
}

// isSlidingWindowIndex reports whether an index gets sliding-window decoration.
// Matches Java's SlidingWindowIndexMaintainerFactory.isSlidingWindowIndex
// (:91-93): VECTOR type AND a row-number window predicate reachable through AND.
//
// canonicalIndexType is used rather than a raw string compare so that the
// index-type aliases the rest of the dispatch honours are honoured here too;
// a decoration decision that disagreed with createIndexMaintainer's switch
// would validate one index and maintain a different one.
func isSlidingWindowIndex(idx *Index) bool {
	return canonicalIndexType(idx.Type) == IndexTypeVector && idx.HasRowNumberWindowPredicate()
}

func validateSlidingWindowIndex(md *RecordMetaData, idx *Index) error {
	recordTypes := md.RecordTypesForIndex(idx)

	if len(recordTypes) == 0 {
		return &MetaDataError{Message: "sliding window index delegate is defined on an empty set of types"}
	}
	if len(recordTypes) != 1 {
		return &MetaDataError{Message: fmt.Sprintf(
			"sliding window index delegate has multiple types (index %s)", idx.Name)}
	}
	// Java's third arm. RecordType.IsSynthetic() is a constant false in this
	// port (synthetic record types are not modelled), so this arm cannot fire —
	// but the shape it guards against is refused EARLIER and more loudly:
	// indexFromProto rejects an index naming a record type that is not in the
	// union descriptor with "unknown record type %q referenced by index %q", and
	// a joined/unnested type never becomes a *RecordType. Ported for 1:1
	// fidelity so that modelling synthetic types later re-arms the check
	// automatically rather than leaving a hole nobody remembers.
	for _, rt := range recordTypes {
		if rt.IsSynthetic() {
			return &MetaDataError{Message: fmt.Sprintf(
				"sliding window index is on synthetic record types (index %s)", idx.Name)}
		}
	}
	// Defensive: restates the decoration gate above.
	if canonicalIndexType(idx.Type) != IndexTypeVector {
		return &MetaDataError{Message: "sliding window index can only be defined on vector indexes"}
	}
	// Defensive: restates the decoration gate above.
	if !idx.HasPredicate() {
		return &MetaDataError{Message: "attempt to create sliding window index without index predicate"}
	}
	if idx.IsUnique() {
		return &MetaDataError{Message: "sliding window index does not support unique indexes"}
	}
	if err := validateRowNumberWindowPlacement(idx.predicateProto); err != nil {
		return err
	}
	// Java's maintainer constructor throws when the window declaration cannot be
	// read back with its narrower lookup, and when the ordering/partition key
	// expressions cannot be built. Validating here turns "the metadata loads and
	// then every save fails" into "the metadata does not load", which is the
	// same trade the TEXT-index tokenizer validation above makes.
	spec, err := idx.RowNumberWindowSpec()
	if err != nil {
		return err
	}
	if _, err := spec.OrderingKey(); err != nil {
		return &MetaDataError{Message: fmt.Sprintf(
			"sliding window index %q: ordering key: %v", idx.Name, err)}
	}
	if _, err := spec.PartitionKey(); err != nil {
		return &MetaDataError{Message: fmt.Sprintf(
			"sliding window index %q: partition key: %v", idx.Name, err)}
	}
	return nil
}
