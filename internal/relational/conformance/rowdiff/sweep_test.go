package rowdiff

import (
	"runtime"
	"sync"
	"sync/atomic"

	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/core/embedded"
)

// sweepSeeds computes run(seed) for seeds 1..n on every core and hands each
// result to merge in seed order, so counters and first-N samples are exactly
// those of a sequential sweep. run must not touch shared state.
func sweepSeeds[R any](n uint64, run func(seed uint64) R, merge func(seed uint64, r R)) {
	results := make([]R, n)
	var next atomic.Uint64
	var wg sync.WaitGroup
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := next.Add(1) - 1
				if i >= n {
					return
				}
				results[i] = run(i + 1)
			}
		}()
	}
	wg.Wait()
	for i, r := range results {
		merge(uint64(i)+1, r)
	}
}

// sweptPlan is one query of a generated case planned with default statistics.
type sweptPlan struct {
	q    Query
	sql  string
	plan plans.RecordQueryPlan
	err  error
}

// sweptCase is a generated case with every query×projection planned.
type sweptCase struct {
	c     *Case
	ddl   string
	plans []sweptPlan
}

// sharedSweep is one planning of seeds 1..n and the number of callers
// holding it.
type sharedSweep struct {
	get     func() []sweptCase
	holders int
}

var (
	defaultSweepMu sync.Mutex
	defaultSweeps  = map[uint64]*sharedSweep{}
)

// defaultSweep plans seeds 1..n with default statistics. The cost, ordering
// and stats sweeps read the same plans, so callers holding them at the same
// time share one planning. The plans live only while held: release drops the
// caller's hold, the last release forgets them, and a later caller plans
// afresh, so the process does not retain every plan until it exits.
func defaultSweep(n uint64) ([]sweptCase, func()) {
	defaultSweepMu.Lock()
	s, ok := defaultSweeps[n]
	if !ok {
		s = &sharedSweep{get: sync.OnceValue(func() []sweptCase { return planDefaultSweep(n) })}
		defaultSweeps[n] = s
	}
	s.holders++
	defaultSweepMu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			defaultSweepMu.Lock()
			defer defaultSweepMu.Unlock()
			s.holders--
			if s.holders == 0 && defaultSweeps[n] == s {
				delete(defaultSweeps, n)
			}
		})
	}
	return s.get(), release
}

func planDefaultSweep(n uint64) []sweptCase {
	out := make([]sweptCase, n)
	sweepSeeds(n, func(seed uint64) sweptCase {
		c := Generate(seed)
		sc := sweptCase{c: c, ddl: c.DDL()}
		for _, q := range c.Queries {
			for _, proj := range c.ProjectionsFor(q) {
				sql := c.SQL(q, proj)
				plan, err := embedded.PlanPhysicalForTest(sql, sc.ddl, nil)
				sc.plans = append(sc.plans, sweptPlan{q: q, sql: sql, plan: plan, err: err})
			}
		}
		return sc
	}, func(seed uint64, sc sweptCase) { out[seed-1] = sc })
	return out
}
