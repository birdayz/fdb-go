package executor

import (
	"context"
	"testing"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// TestExecuteProjection_ShadowAndOutputNames_RFC173 is the @claude P2
// carry-forward, due at Slice 1: a dedicated e2e shadow test for the PROJECTION
// producer (analogous to TestBuildCoveringRow_ShadowAndCollision_RFC173P2),
// driven through the real executeProjection cursor machinery (ExecutePlan over a
// temp-table inner — the established storeless pattern).
//
// It pins, per emitted row:
//  1. frontier propagation — the projection's input carried a Positional, so its
//     output must too (the Slice 1 emission gate lets the frontier flow through);
//  2. the emitted positional row mirrors the emitted name-keyed Datum
//     field-for-field (shadowMismatch == "") — both representations are still
//     emitted during coexistence and consumers read both at different points;
//  3. the positional TYPE is named by the projection's OUTPUT names
//     (alias-preferring posNames: a renamed column carries the ALIAS, matching
//     what a downstream ordinal consumer resolves), while the Datum still carries
//     the source key plus the alias as a secondary key.
func TestExecuteProjection_ShadowAndOutputNames_RFC173(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	evalCtx := EmptyEvaluationContext()
	alias := values.NamedCorrelationIdentifier("proj_shadow_tt")

	// Two frontier rows: Datum + Positional agree (well-formed non-join rows).
	tt := evalCtx.GetOrCreateTempTable(alias, nil)
	inType := positionalTypeFromNames([]string{"ID", "V"})
	for _, r := range []struct{ id, v int64 }{{1, 10}, {2, 20}} {
		if err := tt.Add(QueryResult{
			Datum:      map[string]any{"ID": r.id, "V": r.v},
			Positional: &PositionalRow{Type: inType, Slots: []any{r.id, r.v}},
		}); err != nil {
			t.Fatalf("temp table add: %v", err)
		}
	}

	// SELECT id, v AS renamed FROM tt — one bare column, one renamed.
	proj := plans.NewRecordQueryProjectionPlanWithAliases(
		[]values.Value{
			values.NewFlatFieldValue("ID", values.UnknownType),
			values.NewFlatFieldValue("V", values.UnknownType),
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
		// (3) Positional TYPE = OUTPUT names (alias-preferring).
		fields := qr.Positional.Type.Fields
		if len(fields) != 2 || fields[0].Name != "ID" || fields[1].Name != "RENAMED" {
			t.Fatalf("row %d: positional type = %v, want [ID RENAMED] (posNames must be OUTPUT/alias names)", i, fields)
		}
		// (2) Positional mirrors Datum field-for-field.
		m, ok := qr.Datum.(map[string]any)
		if !ok {
			t.Fatalf("row %d: projection Datum is %T, want map", i, qr.Datum)
		}
		if bad := shadowMismatch(qr.Positional, m); bad != "" {
			t.Fatalf("row %d: positional/Datum shadow mismatch on field %q (positional=%v datum=%v)",
				i, bad, qr.Positional.Slots, m)
		}
		// Values themselves: the renamed slot carries V's value.
		if v, _ := qr.Positional.Get(1); v != wantV[i] {
			t.Fatalf("row %d: RENAMED slot = %v, want %d", i, v, wantV[i])
		}
		// Coexistence: the Datum still carries the source key AND the alias key.
		if m["V"] != wantV[i] || m["RENAMED"] != wantV[i] {
			t.Fatalf("row %d: Datum coexistence keys wrong: %v", i, m)
		}
	}
}
