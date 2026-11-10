package recordlayer

import (
	"fmt"
	"slices"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/internal/partition"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// PartitionKeyComponentKind distinguishes storage identity from declared columns.
type PartitionKeyComponentKind uint8

const (
	PartitionKeyRecordType PartitionKeyComponentKind = iota
	PartitionKeyCoordinate
	PartitionKeyDeclared
)

// PartitionKeyComponent describes one physical primary-key position. Hidden
// components have LogicalOrdinal -1; Expression retains the physical AST.
type PartitionKeyComponent struct {
	Kind            PartitionKeyComponentKind
	Expression      KeyExpression
	PhysicalOrdinal int
	LogicalOrdinal  int
}

// PartitionKeyLayout is the immutable logical/physical view of a validated
// interval primary key. The underlying record metadata remains physical.
type PartitionKeyLayout struct {
	recordTypeName string
	recordTypeKey  any
	physical       KeyExpression
	spec           *PartitionSpec
	logical        KeyExpression
	components     []PartitionKeyComponent
	sourceOrdinal  int
	sourceField    protoreflect.FieldDescriptor
}

// DescribePartitionKey returns nil, nil only for an ordinary primary key.
// A partition function in an unsupported position or shape is an error.
func DescribePartitionKey(rt *RecordType) (*PartitionKeyLayout, error) {
	if rt == nil {
		return nil, &PartitionMetadataError{Message: "record type is nil"}
	}
	spec, err := rt.PartitionSpec()
	if err != nil {
		return nil, err
	}
	if spec == nil {
		for _, index := range rt.GetAllIndexes() {
			if len(partitionFunctions(index.RootExpression)) != 0 {
				return nil, &PartitionMetadataError{RecordType: rt.Name, IndexName: index.Name, Message: "partition function requires a partitioned record type"}
			}
		}
		return nil, nil
	}
	bad := func(message string) (*PartitionKeyLayout, error) {
		return nil, &PartitionMetadataError{RecordType: rt.Name, Message: message}
	}
	if rt.Descriptor == nil {
		return bad("partitioned record type has no descriptor")
	}
	fd := rt.Descriptor.Fields().ByName(protoreflect.Name(spec.SourceColumn()))
	if fd == nil || fd.Kind() != protoreflect.Int64Kind || fd.IsList() {
		return bad("partition source must be a scalar BIGINT")
	}
	columns := normalizeKeyForPositions(rt.PrimaryKey)
	logical, ok := partitionLogicalSuffix(rt.PrimaryKey, 2)
	if !ok {
		return bad("cannot project the inherited primary key prefix")
	}
	l := &PartitionKeyLayout{recordTypeName: rt.Name, recordTypeKey: rt.GetRecordTypeKey(), physical: rt.PrimaryKey, spec: spec, logical: logical, sourceOrdinal: -1, sourceField: fd}
	for i, column := range columns {
		component := PartitionKeyComponent{Expression: column, PhysicalOrdinal: i, LogicalOrdinal: i - 2, Kind: PartitionKeyDeclared}
		switch i {
		case 0:
			component.Kind, component.LogicalOrdinal = PartitionKeyRecordType, -1
		case 1:
			component.Kind, component.LogicalOrdinal = PartitionKeyCoordinate, -1
		default:
			if field, ok := column.(*FieldKeyExpression); ok && field.fieldName == spec.SourceColumn() && field.fanType == FanTypeNone {
				l.sourceOrdinal = i - 2
			}
		}
		l.components = append(l.components, component)
	}
	return l, nil
}

func partitionLogicalConcat(columns []KeyExpression) KeyExpression {
	switch len(columns) {
	case 0:
		return EmptyKey()
	case 1:
		return columns[0]
	default:
		return Concat(columns...)
	}
}

// Keep whole surviving AST children: rebuilding from normalized positions can
// turn correlated nested fan-out columns into an incorrect Cartesian product.
func partitionLogicalSuffix(expr KeyExpression, count int) (KeyExpression, bool) {
	if count == 0 {
		return expr, true
	}
	if count < 0 || count > expr.ColumnSize() {
		return nil, false
	}
	switch e := expr.(type) {
	case *CompositeKeyExpression:
		var suffix []KeyExpression
		for _, child := range e.expressions {
			if count >= child.ColumnSize() {
				count -= child.ColumnSize()
				continue
			}
			projected, ok := partitionLogicalSuffix(child, count)
			if !ok {
				return nil, false
			}
			suffix = append(suffix, projected)
			count = 0
		}
		return partitionLogicalConcat(suffix), true
	case *GroupingKeyExpression:
		if count > e.GetGroupingCount() {
			return nil, false
		}
		whole, ok := partitionLogicalSuffix(e.wholeKey, count)
		if !ok {
			return nil, false
		}
		return &GroupingKeyExpression{wholeKey: whole, groupedCount: e.groupedCount}, true
	case *KeyWithValueExpression:
		inner, ok := partitionLogicalSuffix(e.innerKey, count)
		if !ok {
			return nil, false
		}
		return KeyWithValue(inner, e.splitPoint-count), true
	default:
		if count == expr.ColumnSize() {
			return EmptyKey(), true
		}
		return nil, false
	}
}

func (l *PartitionKeyLayout) Spec() *PartitionSpec                      { return l.spec }
func (l *PartitionKeyLayout) PhysicalPrimaryKey() KeyExpression         { return l.physical }
func (l *PartitionKeyLayout) LogicalPrimaryKey() KeyExpression          { return l.logical }
func (l *PartitionKeyLayout) SourceField() protoreflect.FieldDescriptor { return l.sourceField }
func (l *PartitionKeyLayout) SourceLogicalOrdinal() int                 { return l.sourceOrdinal }
func (l *PartitionKeyLayout) Components() []PartitionKeyComponent       { return slices.Clone(l.components) }

func (l *PartitionKeyLayout) LogicalToPhysicalOrdinals() []int {
	if len(l.components) < 2 {
		return nil
	}
	ordinals := make([]int, len(l.components)-2)
	for i := range ordinals {
		ordinals[i] = i + 2
	}
	return ordinals
}

// PhysicalKey routes a complete declared key, never an invented partition zero
// or a partial prefix that could name several disjoint physical ranges.
func (l *PartitionKeyLayout) PhysicalKey(logical tuple.Tuple) (tuple.Tuple, error) {
	if l == nil || l.spec == nil {
		return nil, &PartitionMetadataError{Message: "partition key layout is not initialized"}
	}
	if len(logical) != len(l.components)-2 {
		return nil, &PartitionRoutingError{Value: logical, Width: l.spec.IntervalWidth(), Reason: fmt.Sprintf("complete logical key requires %d components", len(l.components)-2)}
	}
	for _, value := range logical {
		if value == nil {
			return nil, &PartitionRoutingError{Value: logical, Width: l.spec.IntervalWidth(), Reason: "complete logical key cannot contain NULL"}
		}
	}
	p, err := partition.Route(logical[l.sourceOrdinal], l.spec.IntervalWidth())
	if err != nil {
		return nil, err
	}
	physical := make(tuple.Tuple, 2, len(logical)+2)
	physical[0], physical[1] = l.recordTypeKey, p
	return append(physical, logical...), nil
}

// PartitionIndexLayout projects only the inherited partition prefix. Physical
// index metadata, predicates, uniqueness and options are not rewritten.
type PartitionIndexLayout struct {
	physical                *Index
	logical                 KeyExpression
	primaryKeyEntryOrdinals []int
	components              []PartitionKeyComponent
}

func (l *PartitionIndexLayout) PhysicalIndex() *Index            { return l.physical }
func (l *PartitionIndexLayout) LogicalExpression() KeyExpression { return l.logical }
func (l *PartitionIndexLayout) PrimaryKeyEntryOrdinals() []int {
	return slices.Clone(l.primaryKeyEntryOrdinals)
}

func (l *PartitionIndexLayout) LogicalPrimaryKeyEntryOrdinals() []int {
	if len(l.primaryKeyEntryOrdinals) < 2 {
		return nil
	}
	return slices.Clone(l.primaryKeyEntryOrdinals[2:])
}

// Components includes included-value expression positions as well as key
// positions. Entry-key positions are reported separately by the PK mapping.
func (l *PartitionIndexLayout) Components() []PartitionKeyComponent {
	return slices.Clone(l.components)
}

// ProjectIndex validates the local expression against this primary key before
// removing its one inherited coordinate, including covering and TEXT wrappers.
func (l *PartitionKeyLayout) ProjectIndex(idx *Index) (*PartitionIndexLayout, error) {
	if l == nil || l.spec == nil {
		return nil, &PartitionMetadataError{Message: "partition key layout is not initialized"}
	}
	if idx == nil {
		return nil, &PartitionMetadataError{RecordType: l.recordTypeName, Message: "index is nil"}
	}
	if err := validatePartitionIndex(l.recordTypeName, l.physical, l.spec, idx); err != nil {
		return nil, err
	}
	// Field expression equality deliberately ignores its NULL stand-in, while
	// the required partition arguments do not. Validate the inherited AST too.
	if _, err := partitionArguments(partitionFunctions(idx.RootExpression)[0].arguments); err != nil {
		return nil, &PartitionMetadataError{RecordType: l.recordTypeName, IndexName: idx.Name, Message: err.Error(), Cause: err}
	}
	columns := normalizeKeyForPositions(idx.RootExpression)
	logical, ok := partitionLogicalSuffix(idx.RootExpression, 1)
	if !ok {
		return nil, &PartitionMetadataError{RecordType: l.recordTypeName, IndexName: idx.Name, Message: "cannot project the inherited index prefix"}
	}
	positions := idx.PrimaryKeyComponentPositions()
	if positions != nil && len(positions) != len(l.components) {
		return nil, &PartitionMetadataError{RecordType: l.recordTypeName, IndexName: idx.Name, Message: "primary key overlap mapping has the wrong arity"}
	}
	for i, position := range positions {
		if position < -1 || position >= idx.RootExpression.ColumnSize() {
			return nil, &PartitionMetadataError{RecordType: l.recordTypeName, IndexName: idx.Name, Message: "primary key overlap mapping is outside the physical index key"}
		}
		if position >= 0 && !keyExpressionEquals(l.components[i].Expression, columns[position]) {
			return nil, &PartitionMetadataError{RecordType: l.recordTypeName, IndexName: idx.Name, Message: "primary key overlap mapping does not identify the physical expression"}
		}
	}
	components := make([]PartitionKeyComponent, len(columns))
	for i, column := range columns {
		components[i] = PartitionKeyComponent{Kind: PartitionKeyDeclared, Expression: column, PhysicalOrdinal: i, LogicalOrdinal: i - 1}
		if i == 0 {
			components[i].Kind = PartitionKeyCoordinate
		}
	}
	return &PartitionIndexLayout{physical: idx, logical: logical, primaryKeyEntryOrdinals: idx.EntryPrimaryKeyPositions(len(l.components)), components: components}, nil
}
