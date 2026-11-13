package embedded

import (
	"strconv"

	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/gen"
	"fdb.dev/internal/unicode15"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/functions"
	"fdb.dev/pkg/relational/core/metadata"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
)

// parseTextIndexDefinition implements the Go-only FULLTEXT INDEX syntax. The
// partition prefix precedes the one grouped text column in persisted keys.
func parseTextIndexDefinition(def *antlrgen.TextIndexDefinitionContext, b *metadata.Builder) error {
	indexName := functions.NormalizeIdentifier(def.GetIndexName().GetText())
	tableName := functions.NormalizeIdentifier(def.GetSource().GetText())
	columns := def.IndexColumnList().AllIndexColumnSpec()
	if len(columns) != 1 {
		return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"fulltext index %q: exactly one indexed column is required", indexName)
	}
	if err := rejectIndexOrderClause(columns, "fulltext index", indexName); err != nil {
		return err
	}
	options, err := parseTextIndexOptions(def.TextIndexOptions(), indexName)
	if err != nil {
		return err
	}
	var partitions []antlrgen.IIndexColumnSpecContext
	if clause := def.IndexPartitionClause(); clause != nil {
		partitions = clause.AllIndexColumnSpec()
		if err := rejectIndexOrderClause(partitions, "fulltext index PARTITION BY", indexName); err != nil {
			return err
		}
	}
	tmpl, err := b.Build()
	if err != nil {
		return err
	}
	md := tmpl.Underlying()
	if md == nil {
		return api.NewErrorf(api.ErrCodeInternalError, "fulltext index %q: missing metadata", indexName)
	}
	var rt *recordlayer.RecordType
	if source, ok := def.GetSource().(*antlrgen.FullIdContext); ok && len(source.AllUid()) > 1 {
		rt = md.RecordTypes()[tableName]
	} else {
		rt = md.GetRecordType(tableName)
	}
	if rt == nil || rt.Descriptor == nil {
		return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"fulltext index %q references unknown table %q", indexName, tableName)
	}
	resolve := func(spec antlrgen.IIndexColumnSpecContext, text bool) (recordlayer.KeyExpression, error) {
		name := functions.NormalizeIdentifier(spec.GetColumnName().GetText())
		storage, err := recordlayer.ToProtoBufCompliantName(name)
		if err != nil {
			return nil, err
		}
		field := rt.Descriptor.Fields().ByName(protoreflect.Name(storage))
		if field == nil {
			return nil, api.NewErrorf(api.ErrCodeUndefinedColumn,
				"fulltext index %q: unknown column %q", indexName, name)
		}
		unsupportedMessage := field.Kind() == protoreflect.MessageKind && (text || field.Message() == nil || string(field.Message().FullName()) != functions.UUIDProtoMessageName)
		if field.IsList() || field.IsMap() || unsupportedMessage || field.Kind() == protoreflect.GroupKind || (text && field.Kind() != protoreflect.StringKind) {
			return nil, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"fulltext index %q: column %q must be scalar%s", indexName, name, textIndexColumnRequirement(text))
		}
		return recordlayer.Field(storage), nil
	}
	text, err := resolve(columns[0], true)
	if err != nil {
		return err
	}
	if rt.PrimaryKey != nil {
		for _, primaryField := range rt.PrimaryKey.FieldNames() {
			if primaryField == text.FieldNames()[0] {
				return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate, "fulltext index %q: the tokenized column cannot be part of the primary key", indexName)
			}
		}
	}
	groups := make([]recordlayer.KeyExpression, 0, len(partitions))
	for _, spec := range partitions {
		group, err := resolve(spec, false)
		if err != nil {
			return err
		}
		groups = append(groups, group)
	}
	b.AddGeneratedIndex(string(rt.Descriptor.Name()), indexName,
		recordlayer.GroupBy(text, groups...), recordlayer.IndexTypeText, false, options, nil)
	return nil
}

// parseTextIndexOptions reads the Go-only OPTIONS of a FULLTEXT INDEX into the
// Record Layer's text index options. As parseVectorIndexOptions: an unknown
// option is 0A000, a duplicate or a value of the wrong kind 42601. The
// tokenizer must be registered and the version within its range, as the TEXT
// index maintainer requires.
func parseTextIndexOptions(ctx antlrgen.ITextIndexOptionsContext, indexName string) (map[string]string, error) {
	octx, ok := ctx.(*antlrgen.TextIndexOptionsContext)
	if !ok || octx == nil {
		return nil, nil
	}
	opts := map[string]string{}
	for _, o := range octx.AllTextIndexOption() {
		oc, ok := o.(*antlrgen.TextIndexOptionContext)
		if !ok {
			continue
		}
		name := unicode15.ToLowerString(oc.GetOptionName().GetText())
		var key string
		switch name {
		case "tokenizer":
			key = recordlayer.IndexOptionTextTokenizerName
		case "tokenizer_version":
			key = recordlayer.IndexOptionTextTokenizerVersion
		default:
			return nil, api.NewErrorf(api.ErrCodeUnsupportedOperation, "unsupported fulltext index option '%s'", name)
		}
		if _, dup := opts[key]; dup {
			return nil, api.NewErrorf(api.ErrCodeSyntaxError, "duplicate fulltext index option '%s'", name)
		}
		bad := func(text string) error {
			return api.NewErrorf(api.ErrCodeSyntaxError, "invalid value '%s' for fulltext index option '%s'", text, name)
		}
		switch str, num := oc.GetStringValue(), oc.GetIntValue(); {
		case key == recordlayer.IndexOptionTextTokenizerName && str != nil:
			opts[key] = functions.StripStringLiteralQuotes(str.GetText())
		case key == recordlayer.IndexOptionTextTokenizerVersion && num != nil:
			n, err := strconv.ParseInt(num.GetText(), 10, 32)
			if err != nil {
				return nil, bad(num.GetText())
			}
			opts[key] = strconv.FormatInt(n, 10)
		case str != nil:
			return nil, bad(str.GetText())
		default:
			return nil, bad(num.GetText())
		}
	}
	// An absent name is the default tokenizer; an empty one names none.
	name, named := opts[recordlayer.IndexOptionTextTokenizerName]
	tokenizer, err := recordlayer.GetTextTokenizer(name)
	if err != nil || (named && name == "") {
		return nil, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate, "fulltext index %q: unknown tokenizer '%s'", indexName, name)
	}
	if v, ok := opts[recordlayer.IndexOptionTextTokenizerVersion]; ok {
		n, _ := strconv.Atoi(v)
		if err := recordlayer.ValidateTokenizerVersion(tokenizer, n); err != nil {
			return nil, api.WrapErrorf(err, api.ErrCodeInvalidSchemaTemplate, "fulltext index %q", indexName)
		}
	}
	return opts, nil
}

// SQL text access supports only direct scalar coordinates; unsupported key
// expressions retain ordinary record filtering rather than losing identity.
func textIndexScalarColumns(key *gen.KeyExpression) ([]string, bool) {
	if key == nil {
		return nil, false
	}
	switch {
	case key.Field != nil:
		if key.Field.GetFieldName() == "" || key.Field.GetFanType() != gen.Field_SCALAR {
			return nil, false
		}
		// NOT_NULL substitutes protobuf defaults for absent fields, but SQL reads NULL.
		// Matching only the name would consume predicates with different truth values.
		if key.Field.GetNullInterpretation() == gen.Field_NOT_NULL {
			return nil, false
		}
		return []string{recordlayer.ToUserIdentifier(key.Field.GetFieldName())}, true
	case key.Then != nil:
		var columns []string
		for _, child := range key.Then.GetChild() {
			part, ok := textIndexScalarColumns(child)
			if !ok {
				return nil, false
			}
			columns = append(columns, part...)
		}
		return columns, len(columns) > 0
	case key.Grouping != nil:
		return textIndexScalarColumns(key.Grouping.GetWholeKey())
	default:
		return nil, false
	}
}

func textIndexColumnRequirement(text bool) string {
	if text {
		return " string"
	}
	return ""
}
