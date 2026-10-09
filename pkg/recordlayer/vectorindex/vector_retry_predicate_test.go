package vectorindex

import (
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/recordlayer"
)

// TestVectorRefusalsAreNotRetriable pins that the vector index's refusals are
// terminal under both of the record layer's retry rules, which retry only an
// FDB error with a retryable code or a RecordCoreRetriableTransactionError
// somewhere in the chain: neither may be reachable from these errors.
func TestVectorRefusalsAreNotRetriable(t *testing.T) {
	t.Parallel()
	for name, err := range map[string]error{
		"an unsplittable cluster": &ClusterUnsplittableError{},
		"a capability refusal":    &VectorCapabilityError{},
	} {
		var fdbErr fdb.Error
		var fdbErrPtr *fdb.Error
		var retriable *recordlayer.RecordCoreRetriableTransactionError
		if errors.As(err, &fdbErr) || errors.As(err, &fdbErrPtr) || errors.As(err, &retriable) {
			t.Errorf("%s reaches a retriable cause: %v", name, err)
		}
	}
}
