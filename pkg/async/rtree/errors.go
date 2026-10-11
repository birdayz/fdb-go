package rtree

import (
	"fmt"

	"fdb.dev/pkg/recordlayer/javanum"
)

// Java's RTree checks no configuration. A minM, maxM or splitS that cannot
// keep the node invariants fails at the step where Java's fails, with the
// unchecked exception Java raises there; these are those exceptions. The
// operation may already have written to the transaction: nothing rolls that
// back, so no item is lost only because the caller does not commit after an
// error, as the record layer does not.

// VerifyError is Guava's VerifyException from one of RTree's Verify.verify
// checks, which carry no message.
type VerifyError struct{}

func (*VerifyError) Error() string { return "verify failed" }

// IndexOutOfBoundsError is Java's IndexOutOfBoundsException.
type IndexOutOfBoundsError struct{ Index, Length int }

func (e *IndexOutOfBoundsError) Error() string {
	return fmt.Sprintf("Index %d out of bounds for length %d", e.Index, e.Length)
}

// NullPointerError is Java's NullPointerException, from Objects.requireNonNull
// with no message.
type NullPointerError struct{}

func (*NullPointerError) Error() string { return "null pointer" }

// NegativeArraySizeError is Java's NegativeArraySizeException.
type NegativeArraySizeError struct{ Size int }

func (e *NegativeArraySizeError) Error() string { return fmt.Sprint(e.Size) }

// ArithmeticError is Java's ArithmeticException.
type ArithmeticError struct{ Message string }

func (e *ArithmeticError) Error() string { return e.Message }

// errNodePacking is AbstractStorageAdapter.checkNode's refusal of a non-root
// node whose size is outside [minM, maxM] (AbstractStorageAdapter.java:180-186).
func errNodePacking() error {
	return &javanum.IllegalStateError{Message: "packing of non-root is out of valid range"}
}

// checkPacking is AbstractStorageAdapter.checkNode: a non-root node holds
// between minM and maxM slots.
func checkPacking(config RTreeConfig, nodeID []byte, size int) error {
	if (size < config.MinM || size > config.MaxM) && !nodeIDEqual(nodeID, rootNodeID) {
		return errNodePacking()
	}
	return nil
}
