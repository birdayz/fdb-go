package cascades

import "github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"

// ReferencedFields tracks which FieldValues are referenced by upstream
// operators (predicates, projections, ordering). Pushed top-down via
// PushReferencedFieldsThrough* rules so downstream operators can prune
// columns not needed by the query.
//
// Ports Java's ReferencedFieldsConstraint.ReferencedFields.
type ReferencedFields struct {
	fields map[string]struct{}
}

// NewReferencedFields creates a ReferencedFields from a set of field
// names.
func NewReferencedFields(fields map[string]struct{}) *ReferencedFields {
	if fields == nil {
		fields = map[string]struct{}{}
	}
	cp := make(map[string]struct{}, len(fields))
	for k := range fields {
		cp[k] = struct{}{}
	}
	return &ReferencedFields{fields: cp}
}

// EmptyReferencedFields returns an empty field set.
func EmptyReferencedFields() *ReferencedFields {
	return &ReferencedFields{fields: map[string]struct{}{}}
}

// Contains reports whether the given field name is referenced.
func (r *ReferencedFields) Contains(field string) bool {
	if r == nil {
		return false
	}
	_, ok := r.fields[field]
	return ok
}

// IsEmpty reports whether no fields are referenced.
func (r *ReferencedFields) IsEmpty() bool {
	return r == nil || len(r.fields) == 0
}

// Size returns the number of referenced fields.
func (r *ReferencedFields) Size() int {
	if r == nil {
		return 0
	}
	return len(r.fields)
}

// Fields returns the referenced field names.
func (r *ReferencedFields) Fields() map[string]struct{} {
	if r == nil {
		return nil
	}
	return r.fields
}

// Union returns a new ReferencedFields containing all fields from
// both this and other.
func (r *ReferencedFields) Union(other *ReferencedFields) *ReferencedFields {
	if r == nil || r.IsEmpty() {
		return other
	}
	if other == nil || other.IsEmpty() {
		return r
	}
	merged := make(map[string]struct{}, len(r.fields)+len(other.fields))
	for k := range r.fields {
		merged[k] = struct{}{}
	}
	for k := range other.fields {
		merged[k] = struct{}{}
	}
	return &ReferencedFields{fields: merged}
}

// FieldValuesFromPredicates extracts FieldValue names from a list of
// predicates by walking their Value trees.
func FieldValuesFromPredicates(preds []interface{ Explain() string }) *ReferencedFields {
	fields := map[string]struct{}{}
	for _, p := range preds {
		collectFieldValuesFromPredicate(p, fields)
	}
	return &ReferencedFields{fields: fields}
}

// FieldValuesFromValue extracts FieldValue names from a Value tree.
func FieldValuesFromValue(v values.Value) *ReferencedFields {
	fields := map[string]struct{}{}
	collectFieldNamesFromValue(v, fields)
	return &ReferencedFields{fields: fields}
}

func collectFieldNamesFromValue(v values.Value, out map[string]struct{}) {
	if v == nil {
		return
	}
	if fv, ok := v.(*values.FieldValue); ok {
		out[fv.Field] = struct{}{}
	}
	for _, child := range v.Children() {
		collectFieldNamesFromValue(child, out)
	}
}

func collectFieldValuesFromPredicate(p any, out map[string]struct{}) {
	if p == nil {
		return
	}
	type hasChildren interface{ Children() []any }
	type hasValue interface{ GetValues() []values.Value }

	switch pred := p.(type) {
	case interface{ GetOperand() values.Value }:
		collectFieldNamesFromValue(pred.GetOperand(), out)
	case hasValue:
		for _, v := range pred.GetValues() {
			collectFieldNamesFromValue(v, out)
		}
	}
}
