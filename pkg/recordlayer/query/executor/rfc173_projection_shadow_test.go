package executor

import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestExecuteProjection_OutputNames_RFC173 drives the real executeProjection
// cursor (ExecutePlan over a temp-table inner — the storeless pattern) and pins,
// per emitted row:
//  1. frontier propagation — the projection's input carried a Positional, so its
//     output must too (the emission gate flows the frontier through);
//  2. the positional TYPE is named by the projection's OUTPUT names
//     (alias-preferring posNames: a renamed column carries the ALIAS, matching
//     what a downstream ordinal consumer resolves).
//
// (The RFC-173 P2 shadow assert — positional mirrors the name-keyed Datum — is
// retired with the name model; the ordinal row is now the sole output row.)
func TestExecuteProjection_OutputNames_RFC173(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	evalCtx := EmptyEvaluationContext()
	alias := values.NamedCorrelationIdentifier("proj_shadow_tt")

	// Two frontier rows.
	tt := evalCtx.GetOrCreateTempTable(alias, nil)
	inType := positionalTypeFromNames([]string{"ID", "V"})
	for _, r := range []struct{ id, v int64 }{{1, 10}, {2, 20}} {
		if err := tt.Add(QueryResult{
			Positional: &PositionalRow{Type: inType, Slots: []any{r.id, r.v}},
		}); err != nil {
			t.Fatalf("temp table add: %v", err)
		}
	}

	// SELECT id, v AS renamed FROM tt — one bare column, one renamed.
	proj := plans.NewRecordQueryProjectionPlanWithAliases(
		[]values.Value{
			values.NewFieldValueWithResolvedOrdinal("ID", 0, values.UnknownType),
			values.NewFieldValueWithResolvedOrdinal("V", 1, values.UnknownType),
		},
		[]string{"", "RENAMED"},
		plans.NewRecordQueryTempTableScanPlan(alias),
	)
	cursor, err := ExecutePlan(ctx, proj, nil, evalCtx, nil, recordlayer.DefaultExecuteProperties())
	if err != nil {
		t.Fatalf("execute projection: %v", err)
	}
	rows, err := CollectAll(ctx, cursor)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}

	wantV := []int64{10, 20}
	for i, qr := range rows {
		// (1) Frontier propagation: input had Positional, output must too.
		if qr.Positional == nil {
			t.Fatalf("row %d: projection over a frontier row must emit a Positional (emission gate broke frontier propagation)", i)
		}
		// (2) Positional TYPE = OUTPUT names (alias-preferring).
		fields := qr.Positional.Type.Fields
		if len(fields) != 2 || fields[0].Name != "ID" || fields[1].Name != "RENAMED" {
			t.Fatalf("row %d: positional type = %v, want [ID RENAMED] (posNames must be OUTPUT/alias names)", i, fields)
		}
		// Values: the renamed slot carries V's value, resolvable by output name.
		if v, _ := qr.Positional.Get(1); v != wantV[i] {
			t.Fatalf("row %d: RENAMED slot = %v, want %d", i, v, wantV[i])
		}
		if v, _ := getByName(qr.Positional, "RENAMED"); v != wantV[i] {
			t.Fatalf("row %d: RENAMED by name = %v, want %d", i, v, wantV[i])
		}
	}
}
