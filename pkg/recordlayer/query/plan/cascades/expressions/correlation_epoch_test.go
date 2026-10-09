package expressions

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestReference_CorrelationEpochCoversEveryChange: an ancestor answers its
// correlations from a snapshot validated at the current epoch, so every change
// that can move a descendant's correlations must bump the epoch. The epoch is
// process-wide and a parallel test's bump would hide a site that forgot its
// own, so the checks run in a process of their own.
func TestReference_CorrelationEpochCoversEveryChange(t *testing.T) {
	t.Parallel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	const bench = "BenchmarkCorrelationEpochCoversEveryChange"
	output, err := exec.CommandContext(t.Context(), executable,
		"-test.run=^$", "-test.bench=^"+bench+"$", "-test.benchtime=1x").CombinedOutput()
	if err != nil || !bytes.Contains(output, []byte(bench)) {
		t.Fatalf("isolated epoch check: %v\n%s", err, output)
	}
}

func BenchmarkCorrelationEpochCoversEveryChange(b *testing.B) {
	for _, change := range []string{
		"insert", "insert_final", "prepared_apply", "advance", "prune", "prune_set",
		"clear_final", "remove_exploratory", "absorb", "absorb_duplicates", "absorb_planning_state",
	} {
		if err := ancestorSeesCorrelationChange(change); err != nil {
			b.Fatalf("%s: %v", change, err)
		}
	}
}

// ancestorSeesCorrelationChange reads a root two levels above child, changes
// child's correlations through one mutation, and checks the root's answer
// follows.
func ancestorSeesCorrelationChange(change string) error {
	outer := values.NamedCorrelationIdentifier("outer")
	plain := mustExpression(NewSelectExpression(&values.ConstantValue{Value: int64(7), Typ: values.NotNullLong}, nil, nil))
	dependent := mustExpression(NewSelectExpression(mustQOV(outer), nil, nil))

	child := InitialOf(plain)
	switch change {
	case "advance":
		child = InitialOf(dependent)
		child.InsertFinal(plain)
	case "prune", "prune_set", "clear_final":
		child.InsertFinal(dependent)
		child.InsertFinal(plain)
	case "remove_exploratory":
		child.Insert(dependent)
	}
	// A survivor already holding every member of child forwards it without
	// re-inserting anything, so only Absorb's own bump reports the change.
	var survivor *Reference
	if change == "absorb_duplicates" {
		survivor = InitialOf(dependent)
		survivor.Insert(plain)
	}
	middle := InitialOf(mustExpression(NewLogicalDistinctExpression(ForEachQuantifier(child))))
	root := InitialOf(mustExpression(NewLogicalDistinctExpression(ForEachQuantifier(middle))))
	_, before := root.GetCorrelatedTo()[outer]

	switch change {
	case "insert":
		child.Insert(dependent)
	case "insert_final":
		child.InsertFinal(dependent)
	case "prepared_apply":
		relation, err := values.ExactRelationOf(dependent.GetResultValue().Type())
		if err != nil {
			return err
		}
		if err := child.ApplyPreparedMemberBatch(child.AdmissionView(), relation, []RelationalExpression{dependent}, nil, 0); err != nil {
			return err
		}
	case "advance":
		child.AdvancePlannerStage(StagePlanned)
	case "prune":
		child.PruneWith(plain)
	case "prune_set":
		child.PruneToSet(map[RelationalExpression]struct{}{plain: {}})
	case "clear_final":
		child.ClearFinalMembers()
	case "remove_exploratory":
		if !child.RemoveExploratoryMember(dependent) {
			return fmt.Errorf("member not removed")
		}
	case "absorb":
		InitialOf(dependent).Absorb(child)
	case "absorb_duplicates":
		survivor.Absorb(child)
	case "absorb_planning_state":
		InitialOf(dependent).AbsorbPlanningState(child)
	}
	if _, after := root.GetCorrelatedTo()[outer]; after == before {
		return fmt.Errorf("root kept its correlation to %s at %t across the change", outer.Name(), before)
	}
	return nil
}

// TestReference_CorrelationBaseExtendsOnlyAppends: a group read after member
// appends extends its earlier snapshot with the appended members. An append
// and a removal, or a prune and an append, can leave a lane as long as the base
// recorded, so the base must not survive anything but appends.
func TestReference_CorrelationBaseExtendsOnlyAppends(t *testing.T) {
	t.Parallel()
	outer := values.NamedCorrelationIdentifier("outer")
	constant := func(v int64) RelationalExpression {
		return mustExpression(NewSelectExpression(&values.ConstantValue{Value: v, Typ: values.NotNullLong}, nil, nil))
	}
	dependent := func() RelationalExpression {
		return mustExpression(NewSelectExpression(mustQOV(outer), nil, nil))
	}
	rootOver := func(child *Reference) *Reference {
		middle := InitialOf(mustExpression(NewLogicalDistinctExpression(ForEachQuantifier(child))))
		return InitialOf(mustExpression(NewLogicalDistinctExpression(ForEachQuantifier(middle))))
	}
	correlated := func(root *Reference) bool {
		_, ok := root.GetCorrelatedTo()[outer]
		return ok
	}

	t.Run("appends", func(t *testing.T) {
		t.Parallel()
		child := InitialOf(constant(1))
		root := rootOver(child)
		if correlated(root) {
			t.Fatal("constant child is correlated")
		}
		child.Insert(dependent())
		if !correlated(root) {
			t.Fatal("appended correlated member not seen")
		}
		child.Insert(constant(2))
		child.InsertFinal(constant(3))
		if !correlated(root) {
			t.Fatal("later appends lost the correlated member")
		}
	})
	t.Run("remove_then_append", func(t *testing.T) {
		t.Parallel()
		removed := dependent()
		child := InitialOf(removed)
		child.Insert(constant(1))
		root := rootOver(child)
		if !correlated(root) {
			t.Fatal("correlated member not seen")
		}
		// The append saves the snapshot covering both members as the base; the
		// removal then restores that lane length.
		child.Insert(constant(2))
		if !child.RemoveExploratoryMember(removed) {
			t.Fatal("member not removed")
		}
		if correlated(root) {
			t.Fatal("a removed member's correlation survived at the base's lane length")
		}
	})
	t.Run("prune_then_append", func(t *testing.T) {
		t.Parallel()
		child := InitialOf(constant(1))
		kept := constant(2)
		child.InsertFinal(dependent())
		child.InsertFinal(kept)
		root := rootOver(child)
		if !correlated(root) {
			t.Fatal("correlated final not seen")
		}
		child.PruneWith(kept)
		child.InsertFinal(constant(3))
		if correlated(root) {
			t.Fatal("a pruned final's correlation survived an append of equal lane length")
		}
	})
}
