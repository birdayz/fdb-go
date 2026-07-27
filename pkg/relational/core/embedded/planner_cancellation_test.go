package embedded

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/query"
	"fdb.dev/pkg/relational/core/query/logical"
)

func TestCascadesGeneratorPreCanceledPlanPreservesCancellation(t *testing.T) {
	t.Parallel()

	for _, sql := range []string{
		"SELECT 1",
		"EXPLAIN SELECT 1",
	} {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()

			// Each subtest owns its generator: a cascadesGenerator carries a
			// plan cache, and sharing one across parallel subtests would make
			// this a concurrency test of the cache instead of the cancellation
			// test it is.
			generator := newCascadesGenerator(&EmbeddedConnection{})
			cause := errors.New("test planning cancellation")
			ctx, cancel := context.WithCancelCause(context.Background())
			cancel(cause)

			plan, err := generator.Plan(ctx, sql)
			if plan != nil {
				t.Fatalf("Plan() plan = %T, want nil", plan)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Plan() error = %v, want context.Canceled", err)
			}
			if !errors.Is(err, cause) {
				t.Fatalf("Plan() error = %v, want cancellation cause %v", err, cause)
			}
		})
	}
}

func TestPlanScalarSubqueryPlansPreservesPlannerCancellation(t *testing.T) {
	t.Parallel()

	// The helper checks the context once at entry and once before each
	// subquery, so the third check is the sub-planner's own entry guard.
	// Cancel there so the error under test is the one PlanWithContext raised.
	ctx := newCancelAfterChecksContext(3)
	subqueries := []query.ScalarSubqueryPlan{{
		Alias: values.NamedCorrelationIdentifier("scalar"),
		Plan:  logical.NewScan("T", "t"),
	}}

	planned, err := planScalarSubqueryPlans(ctx, subqueries, nil, nil, plannerOptionsFrom(nil))
	if planned != nil {
		t.Fatalf("planScalarSubqueryPlans() plans = %v, want nil", planned)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("planScalarSubqueryPlans() error = %v, want context.Canceled", err)
	}
	var relationalErr *api.Error
	if errors.As(err, &relationalErr) && relationalErr.Code == api.ErrCodeUnsupportedQuery {
		t.Fatalf("planScalarSubqueryPlans() converted cancellation to SQLSTATE %s: %v",
			api.ErrCodeUnsupportedQuery, err)
	}
	// Every layer detects cancellation with the same helper and wraps the same
	// context.Canceled with the same cause, so the returned error cannot say
	// WHICH layer saw it — and a check count cannot either, since the count is
	// what triggers the cancellation in the first place. Pin the layer
	// directly: if an earlier context check is ever added ahead of the
	// sub-planner, the third check fires in a shallower frame, the sub-planner
	// is never entered, no recorded site names it, and this assertion fails
	// instead of passing vacuously.
	sites := ctx.cancelSites()
	if len(sites) == 0 {
		t.Fatal("context never reported cancellation")
	}
	inSubPlanner := slices.ContainsFunc(sites, func(site string) bool {
		return strings.Contains(site, "cascades.(*Planner).plan")
	})
	if !inSubPlanner {
		t.Fatalf("cancellation was never detected inside the sub-planner; "+
			"observed %d detection site(s):\n%s",
			len(sites), strings.Join(sites, "\n--- next detection site ---\n"))
	}
}

// cancelAfterChecksContext models a context canceled concurrently between
// planning layers. It remains live for the first cancelAt-1 Err checks, then
// transitions permanently to context.Canceled.
//
// It also records the call stack of every check that observes the
// cancellation. Those stacks are the only evidence of which planning layers
// detected it: the layers all wrap the identical context.Canceled, so neither
// the returned error nor the check count distinguishes them. Keeping the whole
// chain rather than just the first site costs nothing and makes a failure
// report the layers that did run, not only the one that ran first.
type cancelAfterChecksContext struct {
	context.Context

	cancelAt int32
	checks   atomic.Int32
	done     chan struct{}
	once     sync.Once

	mu    sync.Mutex
	sites []string
}

func newCancelAfterChecksContext(cancelAt int32) *cancelAfterChecksContext {
	return &cancelAfterChecksContext{
		Context:  context.Background(),
		cancelAt: cancelAt,
		done:     make(chan struct{}),
	}
}

func (c *cancelAfterChecksContext) Done() <-chan struct{} {
	return c.done
}

func (c *cancelAfterChecksContext) Err() error {
	if c.checks.Add(1) < c.cancelAt {
		return nil
	}
	buf := make([]byte, 8192)
	site := string(buf[:runtime.Stack(buf, false)])
	c.mu.Lock()
	c.sites = append(c.sites, site)
	c.mu.Unlock()
	c.once.Do(func() {
		close(c.done)
	})
	return context.Canceled
}

// cancelSites returns the call stack of every Err check that reported
// cancellation, in observation order. Empty if cancellation was never observed.
func (c *cancelAfterChecksContext) cancelSites() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.sites)
}
