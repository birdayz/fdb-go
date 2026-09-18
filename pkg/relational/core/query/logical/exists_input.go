package logical

import (
	"fmt"
	"slices"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ExistsInput owns the lowered child of an existential edge. Its exact row is
// captured from the producer, not reconstructed from SQL column labels. The
// enclosing translator consumes this same Reference and the child's independent
// scalar registrations. Copies of the attachment share the immutable input.
//
// Mirrors Java ExpressionVisitor.visitExistsExpressionAtom: the existential
// quantifier ranges over the query operator's already-constructed Reference.
// Consumer admission is a separate obligation of the owning clause.
type ExistsInput struct {
	reference *expressions.Reference
	row       values.ExactTypeHandle
	scalars   []ScalarSubquery
}

func NewExistsInput(reference *expressions.Reference, scalars []ScalarSubquery) (*ExistsInput, error) {
	if reference == nil || reference.Get() == nil || reference.Get().GetResultValue() == nil {
		return nil, fmt.Errorf("EXISTS input has no result producer")
	}
	row, err := values.SnapshotExactType(reference.Get().GetResultValue().Type())
	if err != nil {
		return nil, fmt.Errorf("EXISTS input has no exact result type: %w", err)
	}
	return &ExistsInput{reference: reference, row: row, scalars: slices.Clone(scalars)}, nil
}

func (i *ExistsInput) Reference() *expressions.Reference { return i.reference }

func (i *ExistsInput) ResultType() values.Type { return i.row.Type() }

func (i *ExistsInput) Scalars() []ScalarSubquery { return slices.Clone(i.scalars) }
