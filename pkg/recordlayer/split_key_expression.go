package recordlayer

import (
	"fmt"

	"fdb.dev/gen"
	"google.golang.org/protobuf/proto"
)

// SplitKeyExpression takes a key expression producing multiple single-element
// results (via FanOut) and splits them into fixed-size batches. Each batch
// concatenates splitSize values into one tuple. For example, 12 values with
// splitSize 3 produces 4 tuples of 3 elements each.
//
// The joined expression must have column size 1 and must create duplicates
// (e.g. FanOut on a repeated field). The total number of values must be an
// even multiple of splitSize.
//
// Matches Java's com.apple.foundationdb.record.metadata.expressions.SplitKeyExpression.
type SplitKeyExpression struct {
	joined    KeyExpression
	splitSize int
}

// Split creates a SplitKeyExpression that batches the joined expression's
// results into groups of splitSize. splitSize must be > 0.
func Split(joined KeyExpression, splitSize int) *SplitKeyExpression {
	if splitSize <= 0 {
		panic(fmt.Sprintf("splitSize must be positive, got %d", splitSize))
	}
	return &SplitKeyExpression{joined: joined, splitSize: splitSize}
}

// Evaluate evaluates the joined expression and splits the results into batches
// of splitSize. Each result from the joined expression must be a single-element
// tuple (from FanOut). The values are flattened, then chunked.
//
// Returns an error if the total number of values is not evenly divisible by
// splitSize, matching Java's RecordCoreException behavior.
func (s *SplitKeyExpression) Evaluate(record *FDBStoredRecord[proto.Message], msg proto.Message) ([][]any, error) {
	results, err := s.joined.Evaluate(record, msg)
	if err != nil {
		return nil, err
	}
	if len(results) == 0 {
		return nil, nil
	}

	// Each result from joined is a single-element tuple (from FanOut).
	// Flatten all values, then chunk into groups of splitSize.
	// Matches Java's split() which iterates unsplit list and appends.
	if len(results)%s.splitSize != 0 {
		return nil, fmt.Errorf("stored value size %d is not an even multiple of %d", len(results), s.splitSize)
	}

	batches := make([][]any, 0, len(results)/s.splitSize)
	for i := 0; i < len(results); i += s.splitSize {
		batch := make([]any, 0, s.splitSize)
		for j := 0; j < s.splitSize; j++ {
			batch = append(batch, results[i+j]...)
		}
		batches = append(batches, batch)
	}
	return batches, nil
}

// FieldNames returns the field names accessed by the joined expression.
func (s *SplitKeyExpression) FieldNames() []string {
	return s.joined.FieldNames()
}

// ColumnSize returns the split size — each batch produces splitSize columns.
// Matches Java's SplitKeyExpression.getColumnSize().
func (s *SplitKeyExpression) ColumnSize() int {
	return s.splitSize
}

// ToKeyExpression serializes SplitKeyExpression to proto.
// Matches Java's SplitKeyExpression.toKeyExpression().
func (s *SplitKeyExpression) ToKeyExpression() *gen.KeyExpression {
	sz := int32(s.splitSize)
	return &gen.KeyExpression{
		Split: &gen.Split{
			Joined:    s.joined.ToKeyExpression(),
			SplitSize: &sz,
		},
	}
}

// GetJoined returns the underlying joined key expression.
func (s *SplitKeyExpression) GetJoined() KeyExpression {
	return s.joined
}

// GetSplitSize returns the number of elements per batch.
func (s *SplitKeyExpression) GetSplitSize() int {
	return s.splitSize
}
