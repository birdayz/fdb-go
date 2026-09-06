package embedded

import (
	"fmt"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/functions"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query/expr"
	"fdb.dev/pkg/relational/core/query/logical"
	"fdb.dev/pkg/relational/core/query/semantic"
	"fdb.dev/pkg/relational/core/query/semantic/rlcatalog"
)

// inlineValuesColumnDefinition is one node of the recursive
// UidListWithNestings declaration after an inline VALUES alias. Name is already
// normalized by the parser authority (quoted identifiers preserve their case;
// bare identifiers are upper-cased). Children is nil when the declaration did
// not author a nested field list.
type inlineValuesColumnDefinition struct {
	Name     string
	Children []inlineValuesColumnDefinition
}

// inlineValuesColumnDefinitions returns the SQL-visible positional schema
// authored after an inline VALUES table alias. The declaration is a recursive
// schema, not a flat list: A(B, C, W(X, Y, Z)) names three top-level slots and
// the three fields of W. Its shape is checked later against the exact common
// row type, after all VALUES rows have participated in type promotion.
func inlineValuesColumnDefinitions(item *antlrgen.InlineTableItemContext, width int) ([]inlineValuesColumnDefinition, error) {
	if item == nil || item.InlineTableDefinition() == nil ||
		item.InlineTableDefinition().UidListWithNestingsInParens() == nil {
		definitions := make([]inlineValuesColumnDefinition, width)
		for i := range definitions {
			definitions[i].Name = values.OrdinalFieldName(i)
		}
		return definitions, nil
	}
	list := item.InlineTableDefinition().UidListWithNestingsInParens().UidListWithNestings()
	if list == nil {
		return nil, api.NewError(api.ErrCodeSyntaxError, "inline VALUES column definition is empty")
	}
	definitions, err := parseInlineValuesColumnDefinitions(list)
	if err != nil {
		return nil, err
	}
	if len(definitions) != width {
		return nil, api.NewErrorf(api.ErrCodeSyntaxError,
			"inline VALUES declares %d columns for %d values", len(definitions), width)
	}
	return definitions, nil
}

func parseInlineValuesColumnDefinitions(list antlrgen.IUidListWithNestingsContext) ([]inlineValuesColumnDefinition, error) {
	if list == nil {
		return nil, api.NewError(api.ErrCodeSyntaxError, "inline VALUES nested column definition is empty")
	}
	items := list.AllUidWithNestings()
	if len(items) == 0 {
		return nil, api.NewError(api.ErrCodeSyntaxError, "inline VALUES nested column definition is empty")
	}
	definitions := make([]inlineValuesColumnDefinition, len(items))
	for i, item := range items {
		if item == nil || item.Uid() == nil {
			return nil, api.NewError(api.ErrCodeSyntaxError, "inline VALUES column name is missing")
		}
		definitions[i].Name = functions.NormalizeIdentifier(item.Uid().GetText())
		if nested := item.UidListWithNestingsInParens(); nested != nil {
			children, err := parseInlineValuesColumnDefinitions(nested.UidListWithNestings())
			if err != nil {
				return nil, err
			}
			definitions[i].Children = children
		}
	}
	return definitions, nil
}

// retagInlineValuesColumnType applies an authored nested column definition to
// one exact common VALUES-column type. Naming is positional and copy-on-write:
// the input Type graph is never mutated. A nested declaration can name either
// a RECORD directly or the RECORD element of an ARRAY; every other shape is a
// loud type error rather than an erased/Unknown semantic column.
func retagInlineValuesColumnType(
	typ values.Type,
	definition inlineValuesColumnDefinition,
) (values.Type, error) {
	if typ == nil {
		return nil, fmt.Errorf("column %q has nil type", definition.Name)
	}
	if len(definition.Children) == 0 {
		return typ, nil
	}
	switch typed := typ.(type) {
	case *values.RecordType:
		return retagInlineValuesRecordType(typed, definition.Children)
	case *values.ArrayType:
		element, ok := typed.ElementType.(*values.RecordType)
		if !ok || element == nil {
			return nil, fmt.Errorf("column %q declares nested fields on non-record array element %v",
				definition.Name, typed.ElementType)
		}
		retagged, err := retagInlineValuesRecordType(element, definition.Children)
		if err != nil {
			return nil, fmt.Errorf("column %q array element: %w", definition.Name, err)
		}
		return values.NewArrayType(typed.Nullable, retagged), nil
	default:
		return nil, fmt.Errorf("column %q declares nested fields on non-record type %v",
			definition.Name, typ)
	}
}

func retagInlineValuesRecordType(
	record *values.RecordType,
	definitions []inlineValuesColumnDefinition,
) (*values.RecordType, error) {
	if record == nil {
		return nil, fmt.Errorf("nested column definition targets a nil record type")
	}
	if len(record.Fields) != len(definitions) {
		return nil, fmt.Errorf("nested column definition declares %d fields for record width %d",
			len(definitions), len(record.Fields))
	}
	fields := make([]values.Field, len(record.Fields))
	for i, field := range record.Fields {
		if field.Ordinal != i {
			return nil, fmt.Errorf("record field %d carries malformed ordinal %d", i, field.Ordinal)
		}
		fieldType, err := retagInlineValuesColumnType(field.FieldType, definitions[i])
		if err != nil {
			return nil, err
		}
		fields[i] = values.Field{
			Name:      definitions[i].Name,
			FieldType: fieldType,
			Ordinal:   i,
		}
	}
	// The definition renames the FIELDS and keeps the record's NAME, as Java's
	// TypeUtils.setFieldNames does (fromFieldsWithName when the record is
	// named, fromFields when it is not): a `STRUCT RECORD (3 AS p, 4 AS q)`
	// under `a(w(x, y))` is still a record named RECORD, with fields X and Y,
	// and an anonymous row stays anonymous. Minting the SQL kind "RECORD" as
	// every row's name made all VALUES records share one descriptor name, so
	// two rows of different shapes (`VALUES ((3,4)) AS a(w(x,y)), VALUES ((5))
	// AS b(v(z))`) failed to compile into one result descriptor and the
	// driver handed both back as raw maps; rejecting a named source instead
	// refused what Java accepts.
	return &values.RecordType{
		RecordName: record.RecordName,
		Nullable:   record.Nullable,
		Fields:     fields,
		Legs:       append([]values.RecordTypeLeg(nil), record.Legs...),
	}, nil
}

// buildInlineValuesLogical lowers one parsed inline table to the distinct
// exact LogicalInlineValues leaf. Every row is normalized to one common
// positional record type before the array is published; Unknown and erased
// types never reach a semantic source or later QOV.
func buildInlineValuesLogical(
	item *antlrgen.InlineTableItemContext,
	alias, binding string,
	md *recordlayer.RecordMetaData,
) (*logical.LogicalInlineValues, error) {
	if item == nil || alias == "" {
		return nil, api.NewError(api.ErrCodeInvalidParameter,
			"inline VALUES source requires a parsed table and correlation alias")
	}
	rowContexts := item.AllRecordConstructorForInlineTable()
	if len(rowContexts) == 0 {
		return nil, api.NewError(api.ErrCodeSyntaxError, "inline VALUES source has no rows")
	}

	var catalog semantic.Catalog = semantic.NewInMemoryCatalog()
	if md != nil {
		catalog = rlcatalog.Wrap(md)
	}
	resolver := expr.New(semantic.NewAnalyzer(catalog, false), semantic.NewScope(nil))

	width := len(rowContexts[0].AllExpressionWithOptionalName())
	if width == 0 {
		return nil, api.NewError(api.ErrCodeSyntaxError, "inline VALUES row has no columns")
	}
	definitions, err := inlineValuesColumnDefinitions(item, width)
	if err != nil {
		return nil, err
	}
	rows := make([][]values.Value, len(rowContexts))
	common := make([]values.Type, width)
	for rowIndex, rowContext := range rowContexts {
		cells := rowContext.AllExpressionWithOptionalName()
		if len(cells) != width {
			return nil, api.NewErrorf(api.ErrCodeCannotConvertType,
				"inline VALUES row %d has %d columns; row 0 has %d", rowIndex, len(cells), width)
		}
		rows[rowIndex] = make([]values.Value, width)
		for ordinal, cell := range cells {
			value, walkErr := resolver.WalkExpressionForProjection(cell.Expression())
			if walkErr != nil {
				if mapped := mapPredicateWalkError(walkErr); mapped != nil {
					return nil, mapped
				}
				return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation,
					"unsupported inline VALUES expression: %v", walkErr)
			}
			rows[rowIndex][ordinal] = value
			valueType := inlineValuesFoldType(value)
			if common[ordinal] == nil {
				common[ordinal] = valueType
			} else {
				common[ordinal] = values.MaximumType(common[ordinal], valueType)
			}
			if common[ordinal] == nil {
				return nil, api.NewErrorf(api.ErrCodeCannotConvertType,
					"inline VALUES column %q has incompatible row types", definitions[ordinal].Name)
			}
		}
	}

	rowFields := make([]values.Field, width)
	for ordinal, columnType := range common {
		if columnType == nil || values.IsUnresolved(columnType) || columnType.Code() == values.TypeCodeNull ||
			columnType.Code() == values.TypeCodeNone {
			return nil, api.NewErrorf(api.ErrCodeCannotConvertType,
				"inline VALUES column %q has no exact common type", definitions[ordinal].Name)
		}
		retagged, retagErr := retagInlineValuesColumnType(columnType, definitions[ordinal])
		if retagErr != nil {
			return nil, api.NewErrorf(api.ErrCodeCannotConvertType,
				"inline VALUES column %q: %v", definitions[ordinal].Name, retagErr)
		}
		common[ordinal] = retagged
		rowFields[ordinal] = values.Field{
			Name: definitions[ordinal].Name, Ordinal: ordinal, FieldType: retagged,
		}
	}
	rowType := &values.RecordType{Fields: rowFields}
	if _, exactErr := values.SnapshotExactType(rowType); exactErr != nil {
		return nil, api.NewErrorf(api.ErrCodeCannotConvertType,
			"inline VALUES common row is not exact: %v", exactErr)
	}

	rowValues := make([]values.Value, len(rows))
	for rowIndex, row := range rows {
		fields := make([]values.RecordConstructorField, width)
		for ordinal, value := range row {
			normalized, normalizeErr := normalizeInlineValuesValue(value, common[ordinal])
			if normalizeErr != nil {
				return nil, api.NewErrorf(api.ErrCodeCannotConvertType,
					"inline VALUES row %d column %q: %v", rowIndex, definitions[ordinal].Name, normalizeErr)
			}
			fields[ordinal] = values.RecordConstructorField{Name: definitions[ordinal].Name, Value: normalized}
		}
		rowValue := values.NewRawRecordConstructorValue(fields...)
		if !rowValue.Type().Equals(rowType) {
			return nil, api.NewErrorf(api.ErrCodeCannotConvertType,
				"inline VALUES row %d normalized to %s, want %s", rowIndex, rowValue.Type(), rowType)
		}
		rowValues[rowIndex] = rowValue
	}

	collection := values.NewArrayConstructorValue(rowType, rowValues)
	source, err := logical.NewInlineValues(alias, collection)
	if err != nil {
		return nil, api.NewErrorf(api.ErrCodeCannotConvertType, "inline VALUES source: %v", err)
	}
	source.Binding = binding
	return source, nil
}

func inlineValuesFoldType(value values.Value) values.Type {
	if value == nil {
		return nil
	}
	typ := value.Type()
	if _, isNull := value.(*values.NullValue); isNull && (typ == nil || values.IsUnresolved(typ)) {
		return values.NullType
	}
	return typ
}

// normalizeInlineValuesValue injects the common-column promotion into the
// actual runtime literal, recursing through arrays and records. The final
// wrapper states widened nullability exactly without evaluating a second
// branch (selector zero is a fixed, exact machine literal).
func normalizeInlineValuesValue(value values.Value, target values.Type) (values.Value, error) {
	if value == nil || target == nil {
		return nil, fmt.Errorf("nil value or target type")
	}
	if _, isNull := value.(*values.NullValue); isNull {
		return values.NewNullValue(target), nil
	}
	if value.Type().Equals(target) {
		return value, nil
	}

	var normalized values.Value
	switch targetTyped := target.(type) {
	case *values.ArrayType:
		array, ok := value.(*values.ArrayConstructorValue)
		if !ok {
			break
		}
		elements := make([]values.Value, len(array.Elements))
		for i, element := range array.Elements {
			child, err := normalizeInlineValuesValue(element, targetTyped.ElementType)
			if err != nil {
				return nil, err
			}
			elements[i] = child
		}
		normalized = values.NewArrayConstructorValue(targetTyped.ElementType, elements)
	case *values.RecordType:
		record, ok := value.(*values.RecordConstructorValue)
		if !ok || len(record.Fields) != len(targetTyped.Fields) {
			break
		}
		fields := make([]values.RecordConstructorField, len(record.Fields))
		for i := range fields {
			child, err := normalizeInlineValuesValue(record.Fields[i].Value, targetTyped.Fields[i].FieldType)
			if err != nil {
				return nil, err
			}
			fields[i] = values.RecordConstructorField{Name: targetTyped.Fields[i].Name, Value: child}
		}
		recordValue := values.NewRawRecordConstructorValue(fields...)
		recordValue.SetTypeName(targetTyped.RecordName)
		normalized = recordValue
	}
	if normalized == nil {
		maximum := values.MaximumType(inlineValuesFoldType(value), target)
		if maximum == nil || !maximum.Equals(target) {
			return nil, fmt.Errorf("source type %s is not promotable to %s", value.Type(), target)
		}
		normalized = values.NewPromoteValue(value, target)
	}
	if normalized.Type().Equals(target) {
		return normalized, nil
	}
	if !target.IsNullable() || normalized.Type().IsNullable() ||
		!values.WithNullability(normalized.Type(), true).Equals(target) {
		return nil, fmt.Errorf("normalization produced %s instead of %s", normalized.Type(), target)
	}
	selector := &values.ConstantValue{Value: int64(0), Typ: values.NotNullLong}
	return values.NewPickValue(selector,
		[]values.Value{normalized, values.NewNullValue(target)}, target), nil
}

func inlineValuesScopeSource(source *logical.LogicalInlineValues) (semantic.ScopeSource, bool) {
	if source == nil {
		return semantic.ScopeSource{}, false
	}
	row, ok := source.ResultType().(*values.RecordType)
	if !ok {
		return semantic.ScopeSource{}, false
	}
	columns := make([]semantic.Column, len(row.Fields))
	for i, field := range row.Fields {
		column, exact := semanticColumnFromExactType(field.Name, field.FieldType)
		if !exact {
			return semantic.ScopeSource{}, false
		}
		columns[i] = column
	}
	alias := semantic.FromNormalized(source.Alias)
	correlation := source.Binding
	if correlation == "" {
		correlation = alias.Name()
	}
	return semantic.ScopeSource{
		Table: &semantic.StaticTable{
			TableName:    semantic.FromSegments([]string{source.Alias}, false),
			TableColumns: columns,
		},
		Alias:           alias,
		CorrelationName: correlation,
	}, true
}

func parsedInlineValuesScopeSource(
	item *antlrgen.InlineTableItemContext,
	alias, binding string,
	md *recordlayer.RecordMetaData,
) (semantic.ScopeSource, bool) {
	source, err := buildInlineValuesLogical(item, alias, binding, md)
	if err != nil {
		return semantic.ScopeSource{}, false
	}
	return inlineValuesScopeSource(source)
}

func selectHasInlineValuesSource(sq *selectQuery) bool {
	if sq == nil {
		return false
	}
	if sq.inlineValues != nil {
		return true
	}
	for _, join := range sq.joins {
		if join.inlineValues != nil {
			return true
		}
	}
	return false
}
