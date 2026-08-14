package executor

import (
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// mustExecutorConstruct keeps test plan fixtures concise while preserving the
// production constructor boundary: every fallible constructor still validates
// exact types/arity, and an invalid fixture fails immediately instead of
// retaining a partial plan.
func mustExecutorConstruct[T any](value T, err error) T {
	if err != nil {
		panic(err)
	}
	return value
}

func mustTestQOV(t testing.TB, correlation values.CorrelationIdentifier, flowed values.Type) values.QuantifiedObjectValue {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(correlation, flowed)
	if err != nil {
		t.Fatalf("NewQuantifiedObjectValue(%s): %v", correlation, err)
	}
	return qov
}

func mustTestFieldOrdinal(t testing.TB, child values.Value, ordinal int) values.Value {
	t.Helper()
	field, err := values.ResolveFieldOrdinals(child, []int{ordinal})
	if err != nil {
		t.Fatalf("ResolveFieldOrdinals(%d): %v", ordinal, err)
	}
	return field
}

// exactTestRowType builds the immutable-source row declaration used by unit
// fixtures. Callers name every field and its real SQL type; there is no
// UnknownType filler, so a test cannot accidentally reintroduce the unresolved
// FieldValue state RFC-232 removes.
func exactTestRowType(fields ...values.Field) *values.RecordType {
	copyFields := append([]values.Field(nil), fields...)
	for i := range copyFields {
		copyFields[i].Ordinal = i
	}
	return &values.RecordType{Fields: copyFields}
}

func mustTestFieldAt(
	t testing.TB,
	root *values.RecordType,
	ordinal int,
) values.Value {
	t.Helper()
	qov := mustTestQOV(t, values.UniqueCorrelationIdentifier(), root)
	return mustTestFieldOrdinal(t, qov, ordinal)
}

func mustNamedTestField(t testing.TB, name string, typ values.Type) values.Value {
	t.Helper()
	return mustTestFieldAt(t, exactTestRowType(values.Field{Name: name, FieldType: typ}), 0)
}

// mustConcatPlanResultQOV declares the exact row a non-build join emits when it
// concatenates its two child rows. Keeping this type derived from the children
// makes the fixture follow a changed child shape and preserves duplicate column
// names, just like concatLegPositionals does at runtime.
func mustConcatPlanResultQOV(
	t testing.TB,
	plansToConcat ...plans.RecordQueryPlan,
) values.Value {
	t.Helper()
	var fields []values.Field
	for i, plan := range plansToConcat {
		if plan == nil {
			t.Fatalf("concat result child %d is nil", i)
		}
		recordType, ok := plan.GetResultType().(*values.RecordType)
		if !ok || recordType == nil {
			t.Fatalf("concat result child %d type = %T, want exact record", i, plan.GetResultType())
		}
		for _, field := range recordType.Fields {
			fields = append(fields, values.Field{Name: field.Name, FieldType: field.FieldType})
		}
	}
	return mustTestQOV(t, values.UniqueCorrelationIdentifier(), exactTestRowType(fields...))
}

// mustRetainedJoinResult builds the exact ordinal result program for a join
// that retains every field of both source rows. Null-supplying source QOVs are
// widened at the edge before their fields are resolved, so the plan layout and
// executor build share one nullability authority.
func mustRetainedJoinResult(
	t testing.TB,
	outer, inner plans.RecordQueryPlan,
	outerAlias, innerAlias values.CorrelationIdentifier,
	joinType plans.JoinType,
) values.Value {
	t.Helper()
	if !values.SameLeg(outerAlias, outerAlias) || !values.SameLeg(innerAlias, innerAlias) ||
		values.SameLeg(outerAlias, innerAlias) {
		t.Fatalf("retained join requires two stated, distinct aliases: outer=%q inner=%q", outerAlias, innerAlias)
	}
	outerType, outerOK := outer.GetResultType().(*values.RecordType)
	innerType, innerOK := inner.GetResultType().(*values.RecordType)
	if !outerOK || outerType == nil || !innerOK || innerType == nil {
		t.Fatalf("retained join sources must be exact records: outer=%T inner=%T", outer.GetResultType(), inner.GetResultType())
	}
	var outerFlowed values.Type = outerType
	var innerFlowed values.Type = innerType
	if joinType == plans.JoinFullOuter {
		outerFlowed = values.WithNullability(outerFlowed, true)
	}
	if joinType == plans.JoinLeftOuter || joinType == plans.JoinFullOuter {
		innerFlowed = values.WithNullability(innerFlowed, true)
	}
	outerSource := mustTestQOV(t, outerAlias, outerFlowed)
	innerSource := mustTestQOV(t, innerAlias, innerFlowed)
	fields := make([]values.RecordConstructorField, 0, len(outerType.Fields)+len(innerType.Fields))
	appendSource := func(source values.QuantifiedObjectValue, typ *values.RecordType) {
		for ordinal, field := range typ.Fields {
			resolved, err := values.ResolveOrdinalSeedField(source, ordinal)
			if err != nil {
				t.Fatalf("resolve retained join source %s field %d: %v", source.Correlation(), ordinal, err)
			}
			fields = append(fields, values.RecordConstructorField{Name: field.Name, Value: resolved})
		}
	}
	appendSource(outerSource, outerType)
	appendSource(innerSource, innerType)
	return values.NewRawRecordConstructorValue(fields...)
}

// mustTempTableScan derives the scan's exact flowed type from the already
// materialized test table. A mixed or empty table has no honest single result
// type and fails the fixture instead of introducing an UnknownType escape.
func mustTempTableScan(
	t testing.TB,
	evalCtx *EvaluationContext,
	alias values.CorrelationIdentifier,
) *plans.RecordQueryTempTableScanPlan {
	t.Helper()
	rows := evalCtx.GetOrCreateTempTable(alias, nil).Snapshot()
	if len(rows) == 0 || rows[0].Positional == nil || rows[0].Positional.Type == nil {
		t.Fatalf("temp table %s has no row from which to derive an exact flowed type", alias)
	}
	flowed := values.Type(rows[0].Positional.Type)
	for i := 1; i < len(rows); i++ {
		if rows[i].Positional == nil || rows[i].Positional.Type == nil ||
			!flowed.Equals(rows[i].Positional.Type) {
			t.Fatalf("temp table %s row %d disagrees with exact flowed type %v", alias, i, flowed)
		}
	}
	return mustExecutorConstruct(plans.NewRecordQueryTempTableScanPlan(alias, flowed))
}

// Test helpers for the executor's ordinal PositionalRow row model — there is
// no name-keyed map[string]any row in production. These helpers build and
// read a PositionalRow by column name so tests can stay concise and readable
// (the `dmap`/`rowMap`-style names read as if rows were plain maps).

// dmap builds a QueryResult carrying a PositionalRow from a name->value map.
// Column order is sorted by name for determinism; test reads resolve the slot
// through the row's Type (getByName below), so the order is immaterial.
func dmap(m map[string]any) QueryResult {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	slots := make([]any, len(names))
	for i, n := range names {
		slots[i] = m[n]
	}
	return QueryResult{Positional: &PositionalRow{Type: positionalTypeFromNames(names), Slots: slots}}
}

// dorder builds a QueryResult PositionalRow with an EXPLICIT column order, for
// tests that depend on ordinal position (duplicate names, or ordinal reads).
func dorder(names []string, slots []any) QueryResult {
	return QueryResult{Positional: &PositionalRow{Type: positionalTypeFromNames(names), Slots: slots}}
}

// dscalar builds a QueryResult carrying a 1-slot scalar PositionalRow (the
// bare-scalar row shape — `t.arr AS x` flowing a raw value).
func dscalar(v any) QueryResult {
	return QueryResult{Positional: scalarPositionalRow(v)}
}

// dmapPK is dmap with a primary key attached.
func dmapPK(pk tuple.Tuple, m map[string]any) QueryResult {
	qr := dmap(m)
	qr.PrimaryKey = pk
	return qr
}

// rowMap reads a QueryResult's PositionalRow back into a name->value map, for
// tests that previously asserted against the name-keyed Datum. Nil (no
// PositionalRow) reads as a nil map — indexing it yields the zero value, exactly
// as a name-keyed miss did. Nil-safe.
func rowMap(qr QueryResult) map[string]any {
	return positionalToMap(qr.Positional)
}

// rowMapOK is rowMap with an ok flag mirroring the old
// `m, ok := qr.Datum.(map[string]any)` type assertion — true iff the row carries
// a PositionalRow.
func rowMapOK(qr QueryResult) (map[string]any, bool) {
	if qr.Positional == nil {
		return nil, false
	}
	return positionalToMap(qr.Positional), true
}

// getByName is the TEST-ONLY name read-out: it resolves name → ordinal via the
// row's Type (FieldIndexUnique — a duplicated name DECLINES rather than
// first-matching, so a dup-named row is unreadable here) and reads that slot.
// Production has no
// name-keyed read arm — every reference is baked to an ordinal at plan time;
// tests keep this convenience purely to ASSERT on named columns.
func getByName(pos *PositionalRow, name string) (any, bool) {
	if pos == nil || pos.Type == nil {
		return nil, false
	}
	i, ok := pos.Type.FieldIndexUnique(name)
	if !ok {
		return nil, false
	}
	return pos.Get(i)
}

// rowVal reads a single column by name from a QueryResult's PositionalRow.
func rowVal(qr QueryResult, name string) any {
	if qr.Positional == nil {
		return nil
	}
	v, _ := getByName(qr.Positional, name)
	return v
}

// legRead evaluates the PRODUCTION resolution of an alias-qualified column over
// a leg-windowed positional row: a source-relative BAKED reference
// (QOV(alias).col at the leg-LOCAL ordinal — the resolver's plan-time bind
// against the leg's declared column order) evaluated through
// frontierRowContext's rowLegsBinder. This is what an executing plan does for
// `alias.col`; tests use it to assert leg-window routing end-to-end.
func legRead(pos *PositionalRow, alias, col string) (any, bool) {
	if pos == nil || pos.Type == nil {
		return nil, false
	}
	for _, leg := range pos.Type.Legs {
		if !strings.EqualFold(leg.Name, alias) {
			continue
		}
		end := leg.Start + leg.Width
		if leg.Start < 0 || end > len(pos.Type.Fields) {
			return nil, false
		}
		for k := leg.Start; k < end; k++ {
			if !strings.EqualFold(pos.Type.Fields[k].Name, col) {
				continue
			}
			legType := &values.RecordType{Fields: append([]values.Field(nil), pos.Type.Fields[leg.Start:end]...)}
			for i := range legType.Fields {
				legType.Fields[i].Ordinal = i
			}
			qov, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(alias), legType)
			if err != nil {
				return nil, false
			}
			fv, err := values.ResolveFieldOrdinals(qov, []int{k - leg.Start})
			if err != nil {
				return nil, false
			}
			rowCtx, err := frontierRowContext(pos, nil, false)
			if err != nil {
				return nil, false
			}
			v, err := fv.Evaluate(rowCtx)
			if err != nil {
				return nil, false
			}
			return v, true
		}
		return nil, false
	}
	return nil, false
}

// rowScalar reads the sole slot of a QueryResult's PositionalRow (a scalar row).
func rowScalar(qr QueryResult) any {
	if qr.Positional == nil || len(qr.Positional.Slots) == 0 {
		return nil
	}
	return qr.Positional.Slots[0]
}
