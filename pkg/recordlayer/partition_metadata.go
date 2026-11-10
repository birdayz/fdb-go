package recordlayer

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"fdb.dev/pkg/recordlayer/internal/partition"
)

// PartitionSpec is an immutable interval specification derived from required keys.
// It is not a separately persisted flag.
type PartitionSpec struct {
	sourceColumn  string
	intervalWidth int64
}

func (p *PartitionSpec) SourceColumn() string { return p.sourceColumn }
func (p *PartitionSpec) IntervalWidth() int64 { return p.intervalWidth }
func (p *PartitionSpec) Coordinate(value int64) (int64, error) {
	return partition.Coordinate(value, p.intervalWidth)
}

// PartitionMetadataError reports an unsupported or inconsistent partition layout.
type PartitionMetadataError struct {
	RecordType string
	IndexName  string
	Message    string
	Cause      error
}

func (e *PartitionMetadataError) Error() string {
	return fmt.Sprintf("partition metadata: %s (record_type=%q, index=%q)", e.Message, e.RecordType, e.IndexName)
}

func (e *PartitionMetadataError) Unwrap() error { return e.Cause }

// PartitionSpec derives the specification from the canonical physical primary key.
// An unpartitioned record type returns nil, nil.
func (rt *RecordType) PartitionSpec() (*PartitionSpec, error) {
	return partitionSpecForPrimaryKey(rt.Name, rt.PrimaryKey)
}

func partitionSpecForPrimaryKey(name string, primaryKey KeyExpression) (*PartitionSpec, error) {
	functions := partitionFunctions(primaryKey)
	if len(functions) == 0 {
		return nil, nil
	}
	bad := func(message string) (*PartitionSpec, error) {
		return nil, &PartitionMetadataError{RecordType: name, Message: message}
	}
	columns := normalizeKeyForPositions(primaryKey)
	if len(functions) != 1 || len(columns) < 3 {
		return bad("primary key must be (RecordTypeKey, partition function, logical key...)")
	}
	if _, ok := primaryKey.(*CompositeKeyExpression); !ok {
		return bad("partitioned primary key must be a concatenation")
	}
	if _, ok := columns[0].(*RecordTypeKeyExpression); !ok {
		return bad("record type discriminator must precede the partition coordinate")
	}
	p, ok := columns[1].(*PartitionFunctionKeyExpression)
	if !ok || p.name != FunctionNamePartitionV1 {
		return bad("primary key requires the supported single-valued partition function")
	}
	spec, err := partitionArguments(p.arguments)
	if err != nil {
		return nil, &PartitionMetadataError{RecordType: name, Message: err.Error(), Cause: err}
	}
	if !ordinaryPartitionSource(columns[2:], spec.sourceColumn) {
		return bad("logical primary key must include the source as an ordinary key column")
	}
	return spec, nil
}

func ordinaryPartitionSource(columns []KeyExpression, source string) bool {
	for _, column := range columns {
		if field, ok := column.(*FieldKeyExpression); ok && field.fieldName == source && field.fanType == FanTypeNone && field.nullStandin != NullStandinNotNull {
			return true
		}
	}
	return false
}

func (b *RecordMetaDataBuilder) validatePartitionMetadata() error {
	specs := make(map[string]*PartitionSpec)
	for _, name := range slices.Sorted(maps.Keys(b.recordTypes)) {
		rt := b.recordTypes[name]
		spec, err := partitionSpecForPrimaryKey(name, rt.PrimaryKey)
		if err != nil {
			return err
		}
		if spec != nil {
			specs[name] = spec
			if b.recordCountKey != nil {
				return &PartitionMetadataError{RecordType: name, Message: "store-level record counts are not supported"}
			}
			if len(rt.multiTypeIndexes) != 0 || len(b.universalIndexes) != 0 {
				return &PartitionMetadataError{RecordType: name, Message: "universal and multi-type indexes are not supported"}
			}
			for _, idx := range rt.indexes {
				if err := validatePartitionIndex(name, rt.PrimaryKey, spec, idx); err != nil {
					return err
				}
			}
		}
	}
	if len(specs) != 0 {
		for _, name := range slices.Sorted(maps.Keys(b.recordTypes)) {
			if err := validatePartitionTypeIsolation(name, b.recordTypes[name].PrimaryKey); err != nil {
				return err
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(b.indexes)) {
		idx := b.indexes[name]
		if len(partitionFunctions(idx.RootExpression)) == 0 {
			continue
		}
		found := false
		for rtName, rt := range b.recordTypes {
			if specs[rtName] != nil && slices.Contains(rt.indexes, idx) {
				found = true
			}
		}
		if !found {
			return &PartitionMetadataError{IndexName: name, Message: "partition function requires a partitioned record type"}
		}
	}
	if len(partitionFunctions(b.recordCountKey)) != 0 {
		return &PartitionMetadataError{Message: "partition function is not supported in store-level counts"}
	}
	return nil
}

func validatePartitionTypeIsolation(name string, primaryKey KeyExpression) error {
	columns := normalizeKeyForPositions(primaryKey)
	if len(columns) != 0 {
		if _, ok := columns[0].(*RecordTypeKeyExpression); ok {
			return nil
		}
	}
	return &PartitionMetadataError{RecordType: name, Message: "all record types in a partitioned store require a leading record type discriminator"}
}

func validatePartitionIndex(name string, primaryKey KeyExpression, spec *PartitionSpec, idx *Index) error {
	bad := func(message string) error {
		return &PartitionMetadataError{RecordType: name, IndexName: idx.Name, Message: message}
	}
	if idx.Type != IndexTypeValue && idx.Type != IndexTypeText {
		return bad("only single-table VALUE and TEXT indexes are supported")
	}
	if rowNumberWindowOf(idx.predicateProto) != nil {
		return bad("windowed indexes are not supported")
	}
	functions := partitionFunctions(idx.RootExpression)
	columns := normalizeKeyForPositions(idx.RootExpression)
	keyColumns := columns
	if covering, ok := idx.RootExpression.(*KeyWithValueExpression); ok {
		if covering.splitPoint < 1 || covering.splitPoint > len(columns) {
			return bad("covering index must keep the partition coordinate in its key")
		}
		keyColumns = columns[:covering.splitPoint]
	}
	pkColumns := normalizeKeyForPositions(primaryKey)
	if len(functions) != 1 || len(keyColumns) == 0 || !keyExpressionEquals(keyColumns[0], pkColumns[1]) {
		return bad("local index must begin with the identical partition function")
	}
	if functions[0].name != FunctionNamePartitionV1 {
		return bad("unknown partition function version")
	}
	hasTypeKey := false
	walkPartitionExpressions(idx.RootExpression, func(expr KeyExpression) {
		if _, ok := expr.(*RecordTypeKeyExpression); ok {
			hasTypeKey = true
		}
	})
	if hasTypeKey {
		return bad("single-table local indexes must not repeat the record type discriminator")
	}
	positions := buildPrimaryKeyComponentPositions(idx.RootExpression, primaryKey)
	if covering, ok := idx.RootExpression.(*KeyWithValueExpression); ok {
		for _, position := range positions {
			if position >= covering.splitPoint {
				return bad("covering index cannot trim a primary key column from included values")
			}
		}
	}
	if idx.Type == IndexTypeText {
		grouping, ok := idx.RootExpression.(*GroupingKeyExpression)
		if !ok || grouping.wholeKey.ColumnSize()-grouping.groupedCount < 1 {
			return bad("TEXT index must group by the partition coordinate")
		}
		for _, position := range positions {
			if position == textFieldPosition(idx.RootExpression) {
				return bad("TEXT index cannot trim a primary key column from the tokenized body")
			}
		}
	} else if _, grouped := idx.RootExpression.(*GroupingKeyExpression); grouped {
		return bad("grouped VALUE indexes are not supported")
	}
	if idx.IsUnique() && !ordinaryPartitionSource(keyColumns[1:], spec.sourceColumn) {
		return bad("unique index must include the source as an ordinary key column")
	}
	return nil
}

func partitionFunctions(expr KeyExpression) []*FunctionKeyExpression {
	var functions []*FunctionKeyExpression
	walkPartitionExpressions(expr, func(expr KeyExpression) {
		var function *FunctionKeyExpression
		switch e := expr.(type) {
		case *FunctionKeyExpression:
			function = e
		case *PartitionFunctionKeyExpression:
			function = &e.FunctionKeyExpression
		}
		if function != nil && strings.HasPrefix(function.name, "fdb_partition_") {
			functions = append(functions, function)
		}
	})
	return functions
}

func walkPartitionExpressions(expr KeyExpression, visit func(KeyExpression)) {
	if expr == nil {
		return
	}
	visit(expr)
	var children []KeyExpression
	switch e := expr.(type) {
	case *CompositeKeyExpression:
		children = e.expressions
	case *NestingKeyExpression:
		children = []KeyExpression{e.child}
	case *GroupingKeyExpression:
		children = []KeyExpression{e.wholeKey}
	case *KeyWithValueExpression:
		children = []KeyExpression{e.innerKey}
	case *FunctionKeyExpression:
		children = []KeyExpression{e.arguments}
	case *PartitionFunctionKeyExpression:
		children = []KeyExpression{e.arguments}
	case *CardinalityFunctionKeyExpression:
		children = []KeyExpression{e.arguments}
	case *DimensionsKeyExpression:
		children = []KeyExpression{e.WholeKey}
	case *SplitKeyExpression:
		children = []KeyExpression{e.joined}
	case *ListKeyExpression:
		children = e.children
	}
	for _, child := range children {
		walkPartitionExpressions(child, visit)
	}
}
