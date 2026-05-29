package embedded

import (
	"context"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/plans"
)

// PlanCacheEvent classifies how the plan cache participated in a single
// Plan() call. Mirrors Java's RelationalLoggingUtil.PlanCacheEvent
// ({SKIP, HIT, MISS, INCONCLUSIVE}).
type PlanCacheEvent int

const (
	// PlanCacheInconclusive is the zero value: planning errored before a
	// cache decision was reached (Java: INCONCLUSIVE).
	PlanCacheInconclusive PlanCacheEvent = iota
	// PlanCacheSkip means the plan was produced but deliberately not cached
	// (LIMIT/OFFSET query) or no cache is configured.
	PlanCacheSkip
	// PlanCacheHit means the plan was served from the cache.
	PlanCacheHit
	// PlanCacheMiss means the plan was freshly built and stored in the cache.
	PlanCacheMiss
)

func (e PlanCacheEvent) String() string {
	switch e {
	case PlanCacheSkip:
		return "skip"
	case PlanCacheHit:
		return "hit"
	case PlanCacheMiss:
		return "miss"
	default:
		return "inconclusive"
	}
}

// MaxLoggedSQLLength bounds the SQL text carried in a PlanGenerationInfo so a
// pathological query can't blow up a log line.
const MaxLoggedSQLLength = 1024

// PlanGenerationInfo is the diagnostic record emitted once per Plan() call.
// The field set mirrors the keys Java adds to its KeyValueLogMessage in
// RelationalLoggingUtil.publishPlanGenerationLogs. There is deliberately no
// scalar "estimated cost": the Cascades cost model is a comparator, not a
// number — plan identity is PlanHash + PlanExplain, matching Java.
type PlanGenerationInfo struct {
	// SQL is the query text, truncated to MaxLoggedSQLLength.
	SQL string
	// PlanHash is the deterministic hash of the chosen physical plan tree,
	// or 0 when no physical plan was produced (e.g. planning error).
	PlanHash uint64
	// PlanExplain is the plan's Explain() text, or "" when no plan was produced.
	PlanExplain string
	// PlanningDuration is the wall-clock time spent in this planning call.
	PlanningDuration time.Duration
	// Cache records how the plan cache participated.
	Cache PlanCacheEvent
	// CacheNumEntries is the plan cache's current size (Java:
	// primaryCacheNumEntries), or 0 when no cache is configured.
	CacheNumEntries int
	// SlowQuery is true when PlanningDuration exceeded the connection's
	// slow-query threshold.
	SlowQuery bool
	// Err is the planning error, or nil on success.
	Err error
}

// PlanGenerationLogger receives one callback per Plan() call. A nil logger is
// silent. Sampling and log-level policy are the handler's responsibility: the
// engine always emits, and the handler decides volume and sink. (Java keeps
// the same split — the engine builds the record unconditionally; SLF4J level
// and any sampling live outside the planner.)
type PlanGenerationLogger interface {
	LogPlanGeneration(ctx context.Context, info PlanGenerationInfo)
}

// truncateSQL returns sql truncated to at most MaxLoggedSQLLength runes,
// appending a marker when truncation occurred. Rune-safe: never splits a
// multi-byte UTF-8 sequence.
func truncateSQL(sql string) string {
	if len(sql) <= MaxLoggedSQLLength {
		// Fast path: byte length already within bound implies rune count is too.
		return sql
	}
	runes := []rune(sql)
	if len(runes) <= MaxLoggedSQLLength {
		return sql
	}
	return string(runes[:MaxLoggedSQLLength]) + "…(truncated)"
}

// planLogScope accumulates the diagnostic record for one planning call and
// emits it on finish. A nil scope is a no-op throughout, so a nil logger (the
// production default) costs nothing beyond a nil compare in each method plus
// the deferred closure allocation at the call site.
type planLogScope struct {
	g     *cascadesGenerator
	ctx   context.Context
	sql   string
	start time.Time
	plan  plans.RecordQueryPlan
	cache PlanCacheEvent
}

// beginPlanLog starts a logging scope, or returns nil when no logger is
// configured (logging disabled → zero work beyond the nil compare).
func (g *cascadesGenerator) beginPlanLog(ctx context.Context, sql string) *planLogScope {
	if g.c == nil || g.c.planLogger == nil {
		return nil
	}
	return &planLogScope{
		g:     g,
		ctx:   ctx,
		sql:   sql,
		start: time.Now(),
		cache: PlanCacheInconclusive,
	}
}

// setPlan records the chosen physical plan (for hash + explain at finish).
func (s *planLogScope) setPlan(p plans.RecordQueryPlan) {
	if s != nil {
		s.plan = p
	}
}

// setCache records how the plan cache participated.
func (s *planLogScope) setCache(e PlanCacheEvent) {
	if s != nil {
		s.cache = e
	}
}

// finish computes the remaining fields and dispatches to the logger. Safe to
// call on a nil scope. Mirrors Java's RelationalLoggingUtil call in the
// PlanGenerator finally block.
func (s *planLogScope) finish(err error) {
	if s == nil {
		return
	}
	info := PlanGenerationInfo{
		SQL:              truncateSQL(s.sql),
		PlanningDuration: time.Since(s.start),
		Cache:            s.cache,
		Err:              err,
	}
	if s.plan != nil {
		info.PlanHash = plans.PlanHash(s.plan)
		info.PlanExplain = s.plan.Explain()
	}
	// Match Java's RelationalLoggingUtil: primaryCacheNumEntries is reported
	// only for HIT/MISS (the cache was consulted/mutated); SKIP and
	// INCONCLUSIVE carry 0.
	if s.g.cache != nil && (s.cache == PlanCacheHit || s.cache == PlanCacheMiss) {
		info.CacheNumEntries = s.g.cache.Len()
	}
	if thresh := s.g.c.slowQueryThresholdMicros; thresh > 0 {
		info.SlowQuery = info.PlanningDuration.Microseconds() > thresh
	}
	s.g.c.planLogger.LogPlanGeneration(s.ctx, info)
}
