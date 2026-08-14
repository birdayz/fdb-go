package query

import (
	"fmt"
	"strings"

	recordlayer "fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ExactLogicalResultType derives the exact whole-object type flowed by op.
// It is used at the SQL-to-Value boundary, where an existential QOV must be
// typed before the Cascades translator creates its matching quantifier.
func ExactLogicalResultType(op logical.LogicalOperator, md *recordlayer.RecordMetaData) (values.Type, error) {
	typ, err := exactLogicalResultType(op, md)
	if err != nil {
		return nil, err
	}
	if _, err := values.SnapshotExactType(typ); err != nil {
		return nil, fmt.Errorf("logical result type is not exact: %w", err)
	}
	return typ, nil
}

func exactLogicalResultType(op logical.LogicalOperator, md *recordlayer.RecordMetaData) (values.Type, error) {
	if op == nil {
		return nil, fmt.Errorf("cannot type a nil logical operator")
	}
	switch typed := op.(type) {
	case *logical.LogicalFilter:
		return exactLogicalResultType(typed.Input, md)
	case *logical.LogicalSort:
		return exactLogicalResultType(typed.Input, md)
	case *logical.LogicalLimit:
		return exactLogicalResultType(typed.Input, md)
	case *logical.LogicalDistinct:
		return exactLogicalResultType(typed.Input, md)
	case *logical.LogicalProject:
		if len(typed.Projections) != len(typed.ProjectedValues) {
			return nil, fmt.Errorf("projection result has %d labels but %d resolved values", len(typed.Projections), len(typed.ProjectedValues))
		}
		var aggregateInput *values.RecordType
		var positionalInput *values.RecordType
		if typed.AggregateOutputOrdinals != nil || typed.InputOrdinals != nil {
			ordinals := typed.AggregateOutputOrdinals
			if typed.InputOrdinals != nil {
				if typed.AggregateOutputOrdinals != nil {
					return nil, fmt.Errorf("projection cannot carry aggregate and input ordinal contracts together")
				}
				ordinals = typed.InputOrdinals
			}
			if len(ordinals) != len(typed.Projections) {
				return nil, fmt.Errorf("post-aggregate projection has an incomplete output-slot layout")
			}
			inputType, inputErr := exactLogicalResultType(typed.Input, md)
			if inputErr != nil {
				return nil, inputErr
			}
			var ok bool
			positionalInput, ok = inputType.(*values.RecordType)
			if !ok {
				return nil, fmt.Errorf("positional projection input is not a record")
			}
			if typed.AggregateOutputOrdinals != nil {
				aggregateInput = positionalInput
			}
		}
		fields := make([]values.Field, len(typed.ProjectedValues))
		for i, projected := range typed.ProjectedValues {
			var fieldType values.Type
			if projected != nil {
				fieldType = projected.Type()
			} else if aggregateInput != nil {
				ordinal := typed.AggregateOutputOrdinals[i]
				if ordinal < 0 || ordinal >= len(aggregateInput.Fields) {
					return nil, fmt.Errorf("projection result slot %d has no resolved computed value", i)
				}
				fieldType = aggregateInput.Fields[ordinal].FieldType
			} else if positionalInput != nil {
				ordinal := typed.InputOrdinals[i]
				if ordinal < 0 || ordinal >= len(positionalInput.Fields) {
					return nil, fmt.Errorf("projection input slot %d is outside its exact input row", ordinal)
				}
				fieldType = positionalInput.Fields[ordinal].FieldType
			}
			if fieldType == nil {
				return nil, fmt.Errorf("projection result slot %d has no resolved typed value", i)
			}
			name := typed.Projections[i]
			if i < len(typed.Aliases) && typed.Aliases[i] != "" {
				name = typed.Aliases[i]
			} else if i < len(typed.ProjectionRefs) && typed.ProjectionRefs[i].Present {
				// Qualification is parse-tree segment count, never punctuation
				// recovered from a rendering. A quoted one-segment column named
				// "A.B" has Bare="A.B" and must keep that literal dot.
				name = typed.ProjectionRefs[i].Bare
			} else if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
				name = name[dot+1:]
			}
			fields[i] = values.Field{Name: strings.ToUpper(name), Ordinal: i, FieldType: fieldType}
		}
		return &values.RecordType{Fields: fields}, nil
	case *logical.LogicalAggregate:
		fields := make([]values.Field, 0, len(typed.GroupKeys)+len(typed.Calls))
		for i, key := range typed.GroupKeys {
			if key.Value == nil || key.Value.Type() == nil {
				return nil, fmt.Errorf("aggregate group key %d has no exact value", i)
			}
			fields = append(fields, values.Field{
				Name: strings.ToUpper(key.Display), Ordinal: len(fields), FieldType: key.Value.Type(),
			})
		}
		for i, call := range typed.Calls {
			fieldType, typeErr := exactLogicalAggregateCallType(typed, i)
			if typeErr != nil {
				return nil, typeErr
			}
			fields = append(fields, values.Field{
				Name: strings.ToUpper(call.CanonicalName()), Ordinal: len(fields), FieldType: fieldType,
			})
		}
		return &values.RecordType{Fields: fields}, nil
	case *logical.LogicalScan:
		return exactScanResultType(typed, md)
	case *logical.LogicalInlineValues:
		if typed.ResultType() == nil {
			return nil, fmt.Errorf("inline VALUES %q has no exact result row", typed.Alias)
		}
		return typed.ResultType(), nil
	case *logical.LogicalUnnest:
		if typed.CorrelatedCollection == nil {
			return nil, fmt.Errorf("unnest %q has no correlated collection", strings.Join(typed.Segments, "."))
		}
		array, ok := typed.CorrelatedCollection.Type().(*values.ArrayType)
		if !ok || array.ElementType == nil {
			return nil, fmt.Errorf("unnest %q has no exact correlated array element type", strings.Join(typed.Segments, "."))
		}
		if typed.AtAlias == "" {
			return array.ElementType, nil
		}
		return &values.RecordType{Fields: []values.Field{
			{Name: strings.ToUpper(typed.Alias), Ordinal: 0, FieldType: array.ElementType},
			{Name: strings.ToUpper(typed.AtAlias), Ordinal: 1, FieldType: values.NotNullInt},
		}}, nil
	case *logical.LogicalJoin:
		left, err := exactLogicalResultType(typed.Left, md)
		if err != nil {
			return nil, err
		}
		var right values.Type
		if unnest, isLateralUnnest := typed.Right.(*logical.LogicalUnnest); isLateralUnnest && unnest.CorrelatedCollection == nil {
			// A regular lateral FROM leg deliberately carries only its source
			// syntax. Its collection is resolved against the left input when the
			// join is translated, so the standalone LogicalUnnest arm below must
			// remain loud while the enclosing join supplies the missing owner
			// context here. This is the only syntax-only unnest position admitted:
			// a foreign owner, a derived owner, a missing field, or a non-array
			// field all decline instead of minting Unknown.
			right, err = exactLateralUnnestResultType(typed.Left, unnest, md)
		} else {
			right, err = exactLogicalResultType(typed.Right, md)
		}
		if err != nil {
			return nil, err
		}
		return exactJoinResultType(typed, left, right)
	case *logical.LogicalUnion:
		if len(typed.Inputs) == 0 {
			return nil, fmt.Errorf("union has no inputs")
		}
		first, err := exactLogicalResultType(typed.Inputs[0], md)
		if err != nil {
			return nil, err
		}
		for i := 1; i < len(typed.Inputs); i++ {
			branch, branchErr := exactLogicalResultType(typed.Inputs[i], md)
			if branchErr != nil {
				return nil, branchErr
			}
			if !first.Equals(branch) {
				return nil, fmt.Errorf("union branch %d result type disagrees with branch 0", i)
			}
		}
		return first, nil
	case *logical.LogicalCTE:
		result, err := exactLogicalResultType(typed.Main, md)
		if err != nil {
			result, err = exactLogicalResultType(typed.Body, md)
		}
		if err != nil || len(typed.ColumnAliases) == 0 {
			return result, err
		}
		record, ok := result.(*values.RecordType)
		if !ok || len(record.Fields) != len(typed.ColumnAliases) {
			return nil, fmt.Errorf("CTE %q alias arity disagrees with its result type", typed.Name)
		}
		fields := append([]values.Field(nil), record.Fields...)
		for i := range fields {
			fields[i].Name = strings.ToUpper(typed.ColumnAliases[i])
		}
		return &values.RecordType{RecordName: record.RecordName, Nullable: record.Nullable, Fields: fields}, nil
	default:
		return nil, fmt.Errorf("logical operator %T has no exact result-type derivation", op)
	}
}

// exactLateralUnnestResultType derives the exact row contributed by a
// syntax-only LogicalUnnest in the one position where that syntax has an owner:
// the right child of its lateral LogicalJoin. exactUnnestLegColumns is the same
// owner/descriptor/projection authority that types the translated Explode seed;
// using it here prevents the existential QOV from acquiring an element type
// different from the physical unnest. No text fallback or Unknown element is
// admitted.
func exactLateralUnnestResultType(
	left logical.LogicalOperator,
	unnest *logical.LogicalUnnest,
	md *recordlayer.RecordMetaData,
) (values.Type, error) {
	if unnest == nil || unnest.CorrelatedCollection != nil {
		return nil, fmt.Errorf("lateral unnest requires syntax-only collection ownership")
	}
	if len(unnest.Segments) < 2 {
		return nil, fmt.Errorf("lateral unnest %q has no owner-qualified array field", strings.Join(unnest.Segments, "."))
	}
	if unnest.Alias == "" {
		return nil, fmt.Errorf("lateral unnest %q has no element alias", strings.Join(unnest.Segments, "."))
	}
	inlineOwner := findInlineValuesOwner(left, unnest.Segments[0])
	if md == nil && inlineOwner == nil {
		return nil, fmt.Errorf("lateral unnest %q has no record metadata", strings.Join(unnest.Segments, "."))
	}

	fields := (&cascadesTranslator{md: md}).exactUnnestLegColumns(left, unnest)
	if len(fields) == 0 {
		return nil, fmt.Errorf("lateral unnest %q has no exact owner-qualified array field", strings.Join(unnest.Segments, "."))
	}
	if unnest.AtAlias == "" {
		if len(fields) != 1 || !strings.EqualFold(fields[0].Name, unnest.Alias) ||
			fields[0].FieldType == nil || values.IsUnresolved(fields[0].FieldType) {
			return nil, fmt.Errorf("lateral unnest %q has an inexact scalar element layout", strings.Join(unnest.Segments, "."))
		}
		return fields[0].FieldType, nil
	}
	if len(fields) != 2 || !strings.EqualFold(fields[0].Name, unnest.Alias) ||
		!strings.EqualFold(fields[1].Name, unnest.AtAlias) ||
		fields[0].FieldType == nil || values.IsUnresolved(fields[0].FieldType) ||
		fields[1].FieldType == nil || !fields[1].FieldType.Equals(values.NotNullInt) {
		return nil, fmt.Errorf("lateral unnest %q has an inexact AS/AT layout", strings.Join(unnest.Segments, "."))
	}
	return &values.RecordType{Fields: append([]values.Field(nil), fields...)}, nil
}

func exactLogicalAggregateCallType(aggregate *logical.LogicalAggregate, index int) (values.Type, error) {
	if aggregate == nil || index < 0 || index >= len(aggregate.Calls) {
		return nil, fmt.Errorf("aggregate call index %d is outside the native layout", index)
	}
	call := aggregate.Calls[index]
	switch strings.ToUpper(call.Func) {
	case "COUNT", "COUNT(*)":
		return values.NullableLong, nil
	case "AVG":
		return values.NullableDouble, nil
	case "SUM", "MIN", "MAX":
		if index >= len(aggregate.AggregateOperands) || aggregate.AggregateOperands[index] == nil ||
			aggregate.AggregateOperands[index].Type() == nil {
			return nil, fmt.Errorf("aggregate call %d has no exact operand type", index)
		}
		return values.WithNullability(aggregate.AggregateOperands[index].Type(), true), nil
	default:
		return nil, fmt.Errorf("aggregate call %d uses unsupported function %q", index, call.Func)
	}
}

func exactScanResultType(scan *logical.LogicalScan, md *recordlayer.RecordMetaData) (values.Type, error) {
	if scan == nil || md == nil {
		return nil, fmt.Errorf("scan has no record metadata")
	}
	var recordType *recordlayer.RecordType
	if exact := md.GetRecordType(scan.Table); exact != nil {
		recordType = exact
	} else {
		var bestName string
		for name, candidate := range md.RecordTypes() {
			if strings.EqualFold(name, scan.Table) && (recordType == nil || name < bestName) {
				bestName, recordType = name, candidate
			}
		}
	}
	if recordType == nil || recordType.Descriptor == nil {
		return nil, fmt.Errorf("scan table %q has no record descriptor", scan.Table)
	}
	descriptor := recordType.Descriptor
	fields := make([]values.Field, 0, descriptor.Fields().Len()+1)
	for i := 0; i < descriptor.Fields().Len(); i++ {
		field := descriptor.Fields().Get(i)
		fields = append(fields, values.Field{
			Name:      strings.ToUpper(string(field.Name())),
			Ordinal:   i,
			FieldType: TargetTypeForFD(field),
		})
	}
	if md.IsStoreRecordVersions() && descriptor.Fields().ByName(protoreflect.Name(values.PseudoFieldRowVersion)) == nil {
		fields = append(fields, values.Field{Name: values.PseudoFieldRowVersion, Ordinal: len(fields), FieldType: values.NullableVersion})
	}
	return &values.RecordType{RecordName: string(descriptor.Name()), Fields: fields}, nil
}

func exactJoinResultType(join *logical.LogicalJoin, left, right values.Type) (values.Type, error) {
	leftFields, err := logicalLegFields(join.Left, left)
	if err != nil {
		return nil, err
	}
	rightFields, err := logicalLegFields(join.Right, right)
	if err != nil {
		return nil, err
	}
	widenLeft := join.Kind == logical.JoinRight || join.Kind == logical.JoinFull
	widenRight := join.Kind == logical.JoinLeft || join.Kind == logical.JoinFull
	fields := make([]values.Field, 0, len(leftFields)+len(rightFields))
	appendLeg := func(source []values.Field, widen bool) {
		for _, field := range source {
			field.Ordinal = len(fields)
			if widen && !field.FieldType.IsNullable() {
				field.FieldType = values.WithNullability(field.FieldType, true)
			}
			fields = append(fields, field)
		}
	}
	appendLeg(leftFields, widenLeft)
	appendLeg(rightFields, widenRight)
	return &values.RecordType{Fields: fields}, nil
}

func logicalLegFields(op logical.LogicalOperator, typ values.Type) ([]values.Field, error) {
	alias := strings.ToUpper(sourceAlias(op))
	if record, ok := typ.(*values.RecordType); ok {
		fields := make([]values.Field, len(record.Fields))
		for i, field := range record.Fields {
			name := strings.ToUpper(field.Name)
			if alias != "" && !strings.Contains(name, ".") {
				name = alias + "." + name
			}
			fields[i] = values.Field{Name: name, Ordinal: i, FieldType: field.FieldType}
		}
		return fields, nil
	}
	if alias == "" {
		return nil, fmt.Errorf("scalar logical leg %T has no source alias", op)
	}
	return []values.Field{{Name: alias, Ordinal: 0, FieldType: typ}}, nil
}
