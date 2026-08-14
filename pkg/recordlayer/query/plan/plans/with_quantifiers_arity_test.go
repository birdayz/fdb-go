package plans

import (
	"errors"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestPlanWithQuantifiersRejectsWrongArityAndRebuildsValidShape(t *testing.T) {
	t.Parallel()

	scan := func(name string) RecordQueryPlan {
		return mustChecked(t, func() (*RecordQueryScanPlan, error) {
			return NewRecordQueryScanPlan([]string{name}, exactTestRecordType(), false)
		})
	}
	replacements := []RecordQueryPlan{
		scan("replacement-a"),
		scan("replacement-b"),
		scan("replacement-c"),
		scan("replacement-d"),
	}
	quantifiers := func(children ...RecordQueryPlan) []expressions.Quantifier {
		return QuantifiersOverPlans(children)
	}

	tests := []struct {
		name             string
		plan             RecordQueryPlan
		valid            []expressions.Quantifier
		wantChildren     []RecordQueryPlan
		bad              [][]expressions.Quantifier
		wantSameReceiver bool
	}{
		{
			name:             "leaf",
			plan:             scan("leaf"),
			valid:            nil,
			bad:              [][]expressions.Quantifier{quantifiers(replacements[0])},
			wantSameReceiver: true,
		},
		{
			name: "unary",
			plan: mustChecked(t, func() (*RecordQueryLimitPlan, error) {
				return NewRecordQueryLimitPlan(scan("unary-original"), 10, 0)
			}),
			valid:        quantifiers(replacements[0]),
			wantChildren: replacements[:1],
			bad: [][]expressions.Quantifier{
				nil,
				quantifiers(replacements[0], replacements[1]),
			},
		},
		{
			name: "binary",
			plan: mustChecked(t, func() (*RecordQueryFlatMapPlan, error) {
				return NewRecordQueryFlatMapPlan(
					scan("binary-outer"),
					scan("binary-inner"),
					values.NamedCorrelationIdentifier("outer"),
					values.NamedCorrelationIdentifier("inner"),
					&values.ConstantValue{Value: int64(0), Typ: values.NotNullLong},
					false,
				)
			}),
			valid:        quantifiers(replacements[0], replacements[1]),
			wantChildren: replacements[:2],
			bad: [][]expressions.Quantifier{
				quantifiers(replacements[0]),
				quantifiers(replacements[0], replacements[1], replacements[2]),
			},
		},
		{
			name: "nary",
			plan: mustChecked(t, func() (*RecordQueryUnionPlan, error) {
				return NewRecordQueryUnionPlan([]RecordQueryPlan{
					scan("nary-a"),
					scan("nary-b"),
					scan("nary-c"),
				})
			}),
			valid:        quantifiers(replacements[0], replacements[1], replacements[2]),
			wantChildren: replacements[:3],
			bad: [][]expressions.Quantifier{
				quantifiers(replacements[0], replacements[1]),
				quantifiers(replacements[0], replacements[1], replacements[2], replacements[3]),
			},
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			originalChildren := append([]RecordQueryPlan(nil), tc.plan.GetChildren()...)
			for _, bad := range tc.bad {
				rebuilt, err := tc.plan.WithQuantifiers(bad)
				if err == nil {
					t.Fatalf("WithQuantifiers accepted %d quantifiers", len(bad))
				}
				if !errors.Is(err, expressions.ErrQuantifierArity) {
					t.Fatalf("WithQuantifiers error = %v, want errors.Is(ErrQuantifierArity)", err)
				}
				if rebuilt != nil {
					t.Fatalf("WithQuantifiers returned %T after rejecting %d quantifiers", rebuilt, len(bad))
				}
				assertPlanChildrenAre(t, tc.plan, originalChildren)
			}

			rebuilt, err := tc.plan.WithQuantifiers(tc.valid)
			if err != nil {
				t.Fatalf("WithQuantifiers(valid): %v", err)
			}
			if rebuilt == nil {
				t.Fatal("WithQuantifiers(valid) returned nil")
			}
			if tc.wantSameReceiver != (rebuilt == tc.plan) {
				t.Fatalf("WithQuantifiers(valid) same receiver = %v, want %v", rebuilt == tc.plan, tc.wantSameReceiver)
			}
			rebuiltPlan, ok := rebuilt.(RecordQueryPlan)
			if !ok {
				t.Fatalf("WithQuantifiers(valid) returned %T, want RecordQueryPlan", rebuilt)
			}
			assertPlanChildrenAre(t, rebuiltPlan, tc.wantChildren)
		})
	}
}

func assertPlanChildrenAre(t *testing.T, plan RecordQueryPlan, want []RecordQueryPlan) {
	t.Helper()
	got := plan.GetChildren()
	if len(got) != len(want) {
		t.Fatalf("GetChildren() has %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("GetChildren()[%d] = %T %p, want %T %p", i, got[i], got[i], want[i], want[i])
		}
	}
}
