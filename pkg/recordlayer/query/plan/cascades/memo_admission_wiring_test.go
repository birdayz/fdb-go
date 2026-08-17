package cascades

import (
	"context"
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// foreignRelationalExpression is a RelationalExpression the repository manifest
// does not know. It is the shape the closed-registry admission exists to refuse:
// an OPEN interface implemented outside the known set, whose methods the memo
// would otherwise call while hashing.
type foreignRelationalExpression struct {
	expressions.RelationalExpression
}

// TestMemoAdmissionIsWiredIntoTheMemo pins that exact admission RUNS, rather
// than merely existing.
//
// The checked root constructors (cascades.InitialOf and friends) were built to
// reject exact-type disagreement before an expression is hashed — RFC-232 says
// so in as many words — and then had ZERO production callers while the
// unchecked expressions.InitialOf had 52. Machinery that is only reachable from
// its own tests passes its own tests forever and protects nothing, and the
// gap sat on the memo's hot path: memoizeLeaf called HashCodeWithoutChildren
// and EqualsWithoutChildren on a candidate first and minted the Reference after.
//
// So this drives the PRODUCTION path — a planner run — and requires the run to
// end with the admission error rather than produce a plan. The two arms are the
// two halves of the split:
//
//   - the REGISTRY arm reaches MemoizeExpression's pre-hash check, which is
//     what stands between an open-method call and a foreign implementation;
//   - the CLEAN arm is the vacuity guard. Without it, an admission that
//     rejected everything would satisfy the first arm perfectly.
func TestMemoAdmissionIsWiredIntoTheMemo(t *testing.T) {
	t.Parallel()

	t.Run("a foreign expression is refused before it is hashed", func(t *testing.T) {
		t.Parallel()
		m := NewMemo(expressions.InitialOf(fixtureScan("admission-wiring-root")))
		if err := m.AdmissionErr(); err != nil {
			t.Fatalf("a memo over a well-formed root already reports %v; the arm below would then "+
				"pass for the wrong reason", err)
		}
		m.MemoizeExpression(&foreignRelationalExpression{RelationalExpression: fixtureScan("admission-wiring-foreign")})
		err := m.AdmissionErr()
		if err == nil {
			t.Fatal("memoizing an expression outside the repository manifest was accepted. The memo " +
				"calls HashCodeWithoutChildren and EqualsWithoutChildren on it while looking for a " +
				"duplicate, so the registry check has to happen BEFORE that lookup, not after")
		}
		var resolution *values.ResolutionError
		if !errors.As(err, &resolution) {
			t.Fatalf("admission error is %T (%v), want a *values.ResolutionError so a caller can "+
				"triage the family without matching on the message", err, err)
		}
		if !strings.Contains(err.Error(), "manifest") {
			t.Errorf("admission error %q does not name the closed manifest, which is the one fact "+
				"a reader needs to know what to add", err)
		}
	})

	t.Run("a foreign expression ends the RUN rather than producing a plan", func(t *testing.T) {
		t.Parallel()

		// THE ARM THAT COVERS THE DRAIN, and the root has to be CLEAN for it to
		// cover anything. The registry arm above proves the memo RECORDS a
		// violation; nothing proved the planner ACTS on it.
		//
		// A foreign ROOT does not test this. The task drivers call
		// prepareReferenceMemberBatch themselves (unified_tasks.go:458/603/725,
		// expression_matcher.go:157, implementation_rule.go:310), so a foreign
		// root fails the run through a rule-call failure whether or not the drain
		// exists — a test written that way passes with the drain deleted, which
		// is how this one was first written and what deleting the drain proved.
		//
		// The uncovered path is a memo that recorded a violation while the run
		// would otherwise SUCCEED: MemoizeExpression refuses at the registry gate,
		// hands back a deliberately garbage wrapper, and nothing else fails. Then
		// the drain is the only thing standing between that memo and a returned
		// plan. So: clean root, violation recorded out of band, run it.
		memo := NewMemo(expressions.InitialOf(fixtureScan("admission-drain-clean-root")))
		memo.MemoizeExpression(&foreignRelationalExpression{
			RelationalExpression: fixtureScan("admission-drain-foreign"),
		})
		if memo.AdmissionErr() == nil {
			t.Fatal("precondition failed: memoizing a foreign expression recorded no admission " +
				"error, so this arm would be asserting against a memo with nothing to drain")
		}
		planner := NewPlanner(nil, nil)
		planner.memo = memo
		plan, _, err := planner.PlanWithContext(context.Background(), memo.root)
		if err == nil {
			t.Fatal("a planning run over an expression outside the repository manifest SUCCEEDED. " +
				"The memo records the violation; the run loop must drain it into capErr so the " +
				"run ends, or admission reports a problem nobody acts on")
		}
		if plan != nil {
			t.Fatalf("the failed run still returned a plan (%T). A plan built over a memo that "+
				"refused one of its own expressions is exactly the silent wrong answer this guards", plan)
		}
		var resolution *values.ResolutionError
		if !errors.As(err, &resolution) {
			t.Fatalf("run error is %T (%v), want the admission *values.ResolutionError to survive "+
				"the trip through capErr rather than being flattened into a generic failure", err, err)
		}
		if !strings.Contains(err.Error(), "manifest") {
			t.Errorf("run error %q does not name the closed manifest", err)
		}
	})

	t.Run("an ordinary planning run admits cleanly", func(t *testing.T) {
		t.Parallel()
		root := expressions.InitialOf(fixtureScan("admission-wiring-clean"))
		planner := NewPlanner(nil, nil)
		_, tasks, err := planner.PlanWithContext(context.Background(), root)
		if err != nil {
			t.Fatalf("a well-formed singleton failed to plan: %v — admission is refusing "+
				"expressions the repository does know", err)
		}
		if tasks == 0 {
			t.Fatal("the run executed no tasks, so it never reached the memo and this arm is vacuous")
		}
		if admitErr := planner.Memo().AdmissionErr(); admitErr != nil {
			t.Fatalf("a clean run recorded admission error %v", admitErr)
		}
	})
}
