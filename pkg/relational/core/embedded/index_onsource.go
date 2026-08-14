package embedded

import (
	"fmt"
	"strings"

	"google.golang.org/protobuf/reflect/protoreflect"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/functions"
	"fdb.dev/pkg/relational/core/metadata"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/query"
	queryddl "fdb.dev/pkg/relational/core/query/ddl"
	"fdb.dev/pkg/relational/core/query/logical"
)

// The ON-source index front end — the Go port of Java's
// OnSourceIndexGenerator (tag 4.12.11.0,
// fdb-relational-core/.../recordlayer/query/ddl/OnSourceIndexGenerator.java).
//
// `CREATE INDEX i ON t(k1 [ASC|DESC [NULLS FIRST|LAST]], …) INCLUDE (v1, …)`
// is NOT a second index-construction path: Java synthesizes the logical shape
// `SELECT k1…kn, v1…vm FROM t ORDER BY k1 [dir], …` (a projection of the key
// columns followed by the deduplicated INCLUDE columns over the source access,
// under a sort built from the key columns' order clauses,
// OnSourceIndexGenerator.java:172-229) and delegates to
// MaterializedViewIndexGenerator (:227-228). This file does exactly that: it
// builds the same Project-over-Sort-over-Scan plan the AS-SELECT twin would
// plan and hands it to the same generator (RFC-202 D2), so both DDL forms
// produce byte-identical index metadata for equivalent declarations.

// onSourceIndexedColumn is Java's OnSourceIndexGenerator.IndexedColumn: the
// normalized column identifier plus its ordering, defaults applied exactly as
// parseColSpec does (OnSourceIndexGenerator.java:280-301) — no order clause is
// ascending nulls-first; an order clause with no NULLS token defaults DESC to
// NULLS LAST and ASC to NULLS FIRST.
type onSourceIndexedColumn struct {
	name         string
	isDescending bool
	isNullsLast  bool
}

// parseOnSourceColSpec ports IndexedColumn.parseColSpec
// (OnSourceIndexGenerator.java:280-301). The grammar also admits a bare
// `NULLS FIRST|LAST` with no ASC/DESC (orderClause alternative 2); Java's
// orderContext.DESC() is null there, so it reads as ascending with the
// explicit NULLS choice.
func parseOnSourceColSpec(spec antlrgen.IIndexColumnSpecContext) (onSourceIndexedColumn, error) {
	sc, ok := spec.(*antlrgen.IndexColumnSpecContext)
	if !ok {
		return onSourceIndexedColumn{}, api.NewErrorf(api.ErrCodeInternalError,
			"unexpected index column spec node %T", spec)
	}
	col := onSourceIndexedColumn{
		name: functions.StripIdentifierQuotes(sc.GetColumnName().GetText()),
	}
	oc := sc.OrderClause()
	if oc == nil {
		return col, nil
	}
	col.isDescending = oc.DESC() != nil
	if oc.NULLS() == nil {
		col.isNullsLast = col.isDescending
	} else {
		col.isNullsLast = oc.LAST() != nil
	}
	return col, nil
}

// parseOnSourceIndexDefinition handles
// `CREATE [UNIQUE] INDEX name ON table(columns…) [INCLUDE (columns…)] [OPTIONS(…)]`.
//
// Mirrors DdlVisitor.visitIndexOnSourceDefinition (DdlVisitor.java:223-257):
// build the metadata registered so far, parse key and INCLUDE columns, and
// run the OnSourceIndexGenerator port below.
func parseOnSourceIndexDefinition(def *antlrgen.IndexOnSourceDefinitionContext, b *metadata.Builder) error {
	indexName := functions.StripIdentifierQuotes(def.GetIndexName().GetText())
	if err := rejectReservedIndexName(indexName); err != nil {
		return err
	}
	tableName := functions.StripIdentifierQuotes(def.GetSource().GetText())
	unique := def.UNIQUE() != nil
	// OPTIONS(LEGACY_EXTREMUM_EVER) — Java reads it as a presence flag
	// (DdlVisitor.java:233-234). It can only matter to an aggregate arm, which
	// the ON-source shape never reaches, but the flag is threaded exactly as
	// Java threads it.
	useLegacyExtremum := false
	if io, ok := def.IndexOptions().(*antlrgen.IndexOptionsContext); ok {
		for _, opt := range io.AllIndexOption() {
			if oc, ok := opt.(*antlrgen.IndexOptionContext); ok && oc.LEGACY_EXTREMUM_EVER() != nil {
				useLegacyExtremum = true
			}
		}
	}

	var keyCols []onSourceIndexedColumn
	if cl := def.IndexColumnList(); cl != nil {
		for _, spec := range cl.AllIndexColumnSpec() {
			col, err := parseOnSourceColSpec(spec)
			if err != nil {
				return err
			}
			keyCols = append(keyCols, col)
		}
	}
	if len(keyCols) == 0 {
		return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"index %q has no columns", indexName)
	}
	// INCLUDE columns duplicating a key column are filtered out before the
	// projection is formed (OnSourceIndexGenerator.java:173-176) — without the
	// filter `ON t(a) INCLUDE(a)` would build a duplicated column and a
	// shifted covering split point (RFC-202 D12).
	keySet := make(map[string]bool, len(keyCols))
	for _, k := range keyCols {
		keySet[k.name] = true
	}
	var valueCols []string
	if inc, ok := def.IncludeClause().(*antlrgen.IncludeClauseContext); ok {
		if ul := inc.UidList(); ul != nil {
			for _, uid := range ul.AllUid() {
				name := functions.StripIdentifierQuotes(uid.GetText())
				if !keySet[name] {
					valueCols = append(valueCols, name)
				}
			}
		}
	}

	// Plan against the metadata built so far — Java's metadataBuilder.build()
	// + replaceSchemaTemplate (DdlVisitor.java:223-224), identical to the
	// AS-SELECT front end.
	tmpl, err := b.Build()
	if err != nil {
		return err
	}
	md := tmpl.Underlying()
	if md == nil {
		return api.NewErrorf(api.ErrCodeInternalError,
			"index %q: schema template built without metadata", indexName)
	}
	rt := md.RecordTypes()[tableName]
	if rt == nil || rt.Descriptor == nil {
		return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"index %q references unknown table %q", indexName, tableName)
	}
	fields := rt.Descriptor.Fields()
	rowFields := make([]values.Field, 0, fields.Len()+1)
	for i := 0; i < fields.Len(); i++ {
		field := fields.Get(i)
		rowFields = append(rowFields, values.Field{
			Name:      strings.ToUpper(string(field.Name())),
			Ordinal:   i,
			FieldType: query.TargetTypeForFD(field),
		})
	}
	if md.IsStoreRecordVersions() && fields.ByName(protoreflect.Name(values.PseudoFieldRowVersion)) == nil {
		rowFields = append(rowFields, values.Field{
			Name:      values.PseudoFieldRowVersion,
			Ordinal:   len(rowFields),
			FieldType: values.NullableVersion,
		})
	}
	rowQOV, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier(tableName),
		&values.RecordType{RecordName: string(rt.Descriptor.Name()), Fields: rowFields},
	)
	if err != nil {
		return api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
			"index %q cannot type table %q exactly: %v", indexName, tableName, err)
	}

	// One resolved Column per identifier, reused by projection and sort —
	// Java's originalOutputMap yields ONE Column instance per name
	// (OnSourceIndexGenerator.java:183-194), and the generator's ordering
	// functions are identity-keyed (RFC-202 D12), so instance reuse is
	// load-bearing, not an optimisation.
	resolvedByName := make(map[string]values.FieldValue)
	resolve := func(name string) (values.FieldValue, error) {
		if fv, ok := resolvedByName[name]; ok {
			return fv, nil
		}
		ordinal := -1
		if fd := fields.ByName(protoreflect.Name(name)); fd != nil {
			ordinal = fd.Index()
		} else if name == values.PseudoFieldRowVersion && md.IsStoreRecordVersions() {
			// The planner-facing layout appends the __ROW_VERSION pseudo-field
			// one past the descriptor's fields when the template stores row
			// versions and no REAL column shadows it (real-column-wins — the
			// ByName branch above; Java: RecordMetaData.getPlannerType →
			// Type.Record.addPseudoFields, RecordMetaData.java:732-739).
			ordinal = fields.Len()
		} else {
			// Java: Assert.notNullUnchecked(column, ErrorCode.UNDEFINED_COLUMN,
			// "could not find " + identifier) — OnSourceIndexGenerator.java:203,209.
			return nil, api.NewErrorf(api.ErrCodeUndefinedColumn, "could not find %s", name)
		}
		resolved, resolveErr := values.ResolveFieldOrdinals(rowQOV, []int{ordinal})
		if resolveErr != nil {
			return nil, api.NewErrorf(api.ErrCodeInvalidSchemaTemplate,
				"index %q cannot resolve column %q: %v", indexName, name, resolveErr)
		}
		fv, ok := values.AsFieldValue(resolved)
		if !ok {
			return nil, api.NewErrorf(api.ErrCodeInternalError,
				"index %q resolved column %q to %T", indexName, name, resolved)
		}
		resolvedByName[name] = fv
		return fv, nil
	}

	// Projection = key columns ++ deduplicated INCLUDE columns, in order
	// (OnSourceIndexGenerator.java:196-204); ORDER BY = the key columns with
	// their parsed directions (:206-211).
	names := make([]string, 0, len(keyCols)+len(valueCols))
	vals := make([]values.Value, 0, len(keyCols)+len(valueCols))
	sortKeys := make([]logical.SortKey, 0, len(keyCols))
	for _, k := range keyCols {
		fv, rerr := resolve(k.name)
		if rerr != nil {
			return fmt.Errorf("index %q: %w", indexName, rerr)
		}
		names = append(names, strings.ToUpper(k.name))
		vals = append(vals, fv)
		dir := logical.SortAsc
		if k.isDescending {
			dir = logical.SortDesc
		}
		sortKeys = append(sortKeys, logical.SortKey{
			Expr:       strings.ToUpper(k.name),
			Dir:        dir,
			NullsFirst: !k.isNullsLast,
			Value:      fv,
		})
	}
	for _, v := range valueCols {
		fv, rerr := resolve(v)
		if rerr != nil {
			return fmt.Errorf("index %q: %w", indexName, rerr)
		}
		names = append(names, strings.ToUpper(v))
		vals = append(vals, fv)
	}

	// The same Project-over-Sort-over-Scan layering the AS-SELECT front end
	// plans (and index_ddl_order_by_shape_test.go pins) — Java's
	// LogicalOperator.generateSort over the reconstructed select
	// (OnSourceIndexGenerator.java:214-227).
	op := &logical.LogicalProject{
		Input:           logical.NewSort(logical.NewScan(tableName, ""), sortKeys),
		Projections:     names,
		Aliases:         make([]string, len(names)),
		ProjectedValues: vals,
	}
	gi, err := queryddl.Generate(op, md, queryddl.Options{UseLegacyExtremumEver: useLegacyExtremum})
	if err != nil {
		return fmt.Errorf("index %q: %w", indexName, err)
	}
	b.AddGeneratedIndex(gi.TableName, indexName, gi.Root, gi.IndexType, unique, gi.Options, gi.Predicate)
	return nil
}
