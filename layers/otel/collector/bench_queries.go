package collector

import (
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"fdb.dev/layers/otel"
)

// queryOnly runs the post-ingest query classes on the stored corpus.
func (r *benchRun) queryOnly(ctx context.Context, rep *CollectorBenchReport, exp *otel.Exporter) error {
	if start, ok := ctx.Value(benchmarkIngestStartKey{}).(func()); ok {
		start()
	}
	return r.postQueries(ctx, rep, exp)
}

// postQueries runs every query class over the whole corpus, one iteration
// at a time. With QuerySamples or QueryOnly it reports every iteration: its
// reads and the client requests issued while it ran, which are the query's
// own because nothing else runs.
func (r *benchRun) postQueries(ctx context.Context, rep *CollectorBenchReport, exp *otel.Exporter) error {
	sampled := r.cfg.QuerySamples || r.cfg.QueryOnly
	for _, q := range r.queries {
		if !sampled {
			res := measureQuery(ctx, r.cfg.QueryIterations, func(ctx context.Context, i int) (int, error) {
				return q.run(ctx, r.postTarget(q, i))
			})
			rep.Queries[q.class] = res
			rep.Failures += res.Failures
			continue
		}
		samples := make([]CollectorBenchSample, 0, r.cfg.QueryIterations)
		res := measureQuery(ctx, r.cfg.QueryIterations, func(ctx context.Context, i int) (int, error) {
			t := r.postTarget(q, i)
			w := &benchWork{}
			before := exp.ClientMetrics()
			t0 := time.Now()
			rows, err := q.run(context.WithValue(ctx, benchWorkKey{}, w), t)
			d := time.Since(t0)
			after := exp.ClientMetrics()
			s := CollectorBenchSample{
				Iteration: i, Block: t.block, Conversation: t.conv, Ns: d.Nanoseconds(), Rows: rows, Candidates: w.candidates, Bytes: w.bytes,
				GRVs:         after.TransactionReadVersionsCompleted - before.TransactionReadVersionsCompleted,
				GRVCacheHits: after.GRVCacheHits - before.GRVCacheHits,
				GetRanges:    after.TransactionGetRangeRequests - before.TransactionGetRangeRequests,
				GetValues:    int64(after.ReadLatency.Count) - int64(before.ReadLatency.Count),
			}
			if err != nil {
				s.Error = err.Error()
			}
			samples = append(samples, s)
			return rows, err
		})
		res.Samples = samples
		rep.Queries[q.class] = res
		rep.Failures += res.Failures
	}
	return ctx.Err()
}

type benchWorkKey struct{}

// benchWork is the reads one sampled post-ingest iteration's query charged.
type benchWork struct{ candidates, bytes int64 }

// noteBenchWork adds a query's charged reads to the iteration's benchWork.
func noteBenchWork(ctx context.Context, candidates int, bytes int64) {
	if w, ok := ctx.Value(benchWorkKey{}).(*benchWork); ok {
		w.candidates += int64(candidates)
		w.bytes += bytes
	}
}

// ---------------------------------------------------------------------------
// Queries
// ---------------------------------------------------------------------------

// benchTarget selects what one query iteration asks about: a conversation,
// or for block classes a block and its member count.
type benchTarget struct {
	iter, conv, block, size int
}

// benchQuery is one verified query class.
type benchQuery struct {
	class string
	block bool
	run   func(context.Context, benchTarget) (int, error)
}

// postTarget spreads post-ingest iterations evenly over the whole corpus.
func (r *benchRun) postTarget(q benchQuery, i int) benchTarget {
	t := benchTarget{iter: i}
	if q.block {
		t.block = benchSpread(i, r.cfg.QueryIterations, (r.total+benchBlockSize-1)/benchBlockSize)
		t.size = benchBlockMembers(t.block, r.total)
	} else {
		t.conv = benchSpread(i, r.cfg.QueryIterations, r.total)
	}
	return t
}

// benchLoad accumulates the under-load query results.
type benchLoad struct {
	mu      sync.Mutex
	classes map[string]*benchLoadClass
	done    atomic.Int64
}

type benchLoadClass struct {
	attempts, failures int
	rows               int64
	samples            []int64
	err                string
}

func newBenchLoad(queries []benchQuery) *benchLoad {
	l := &benchLoad{classes: map[string]*benchLoadClass{}}
	for _, q := range queries {
		l.classes[q.class] = &benchLoadClass{}
	}
	return l
}

func (l *benchLoad) record(class string, rows int, d int64, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.classes[class]
	c.attempts++
	if err != nil {
		c.failures++
		if c.err == "" {
			c.err = fmt.Sprintf("iteration %d: %v", c.attempts-1, err)
		}
	} else {
		c.rows += int64(rows)
		c.samples = append(c.samples, d)
	}
	l.done.Add(1)
}

func (l *benchLoad) results(wall time.Duration) map[string]CollectorBenchQuery {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]CollectorBenchQuery, len(l.classes))
	for class, c := range l.classes {
		q := CollectorBenchQuery{Iterations: c.attempts, Failures: c.failures, Rows: c.rows, WallNs: wall.Nanoseconds(), Error: c.err, Histogram: benchHistogram(slices.Clone(c.samples))}
		if secs := wall.Seconds(); secs > 0 {
			q.QPS = float64(len(c.samples)) / secs
		}
		out[class] = q
	}
	return out
}

// queryWorker runs the classes round-robin against committed data only:
// conversations at or below the watermark and blocks wholly below it.
func (r *benchRun) queryWorker(ctx context.Context, stop <-chan struct{}, next *atomic.Int64) {
	idle := time.NewTimer(0)
	defer idle.Stop()
	for {
		select {
		case <-stop:
			return
		default:
		}
		i := int(next.Add(1) - 1)
		q := r.queries[i%len(r.queries)]
		committed := int(r.track.Watermark() + 1)
		state := r.cfg.Seed ^ uint64(i)*0x9e3779b97f4a7c15
		pick := int(splitmix64(&state) >> 1)
		t := benchTarget{iter: i}
		avail := committed
		if q.block {
			avail = committed / benchBlockSize
			t.block, t.size = pick%max(avail, 1), benchBlockSize
		} else {
			t.conv = pick % max(avail, 1)
		}
		if avail == 0 {
			idle.Reset(benchQueryIdle)
			select {
			case <-stop:
				return
			case <-idle.C:
			}
			continue
		}
		t0 := time.Now()
		rows, err := q.run(ctx, t)
		r.load.record(q.class, rows, time.Since(t0).Nanoseconds(), err)
	}
}

func measureQuery(ctx context.Context, iterations int, run func(context.Context, int) (int, error)) CollectorBenchQuery {
	out := CollectorBenchQuery{Iterations: iterations}
	samples := make([]int64, 0, iterations)
	start := time.Now()
	for i := range iterations {
		t0 := time.Now()
		rows, err := run(ctx, i)
		d := time.Since(t0).Nanoseconds()
		if err != nil {
			out.Failures++
			if out.Error == "" {
				out.Error = fmt.Sprintf("iteration %d: %v", i, err)
			}
			continue
		}
		samples = append(samples, d)
		out.Rows += int64(rows)
	}
	wall := time.Since(start)
	out.WallNs = wall.Nanoseconds()
	if secs := wall.Seconds(); secs > 0 {
		out.QPS = float64(len(samples)) / secs
	}
	out.Histogram = benchHistogram(samples)
	return out
}

// benchHistogram uses the nearest-rank percentiles of otel evidence histograms.
func benchHistogram(samples []int64) otel.Histogram {
	if len(samples) == 0 {
		return otel.Histogram{}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	q := func(p int) int64 { return samples[max((len(samples)*p+99)/100-1, 0)] }
	return otel.Histogram{Population: len(samples), Min: samples[0], P50: q(50), P95: q(95), P99: q(99), Max: samples[len(samples)-1]}
}

func benchQueries(cfg CollectorBenchConfig, gen *benchGen, exp *otel.Exporter) []benchQuery {
	n := cfg.TurnsPerConversation
	k := exp.Config().K
	chat := otel.Attribute{Scope: "span", Key: "gen_ai.operation.name", Type: "string", Value: "chat"}
	model := otel.Attribute{Scope: "span", Key: "gen_ai.request.model", Type: "string", Value: "gpt-4o"}
	var out []benchQuery
	if cfg.EnableMessageFullText {
		out = append(out, benchFTSQueries(n, k, gen, exp, chat, model)...)
	}
	return append(out, benchTraceQueries(cfg, n, k, gen, exp, chat, model)...)
}

// benchInBlock checks the trace is exactly the generated trace of a member of block b.
func benchInBlock(gen *benchGen, trace string, b int) bool {
	raw, err := hex.DecodeString(trace)
	if err != nil {
		return false
	}
	c, ok := benchConversationOf(raw)
	return ok && benchBlock(c) == b && trace == gen.traceHex(c)
}

func checkBenchFTSHits(hits []otel.MessageSpanHit, want int, ok func(otel.MessageSpanHit) error) (int, error) {
	if len(hits) != want {
		return len(hits), componentErrorf("%d hits, want %d", len(hits), want)
	}
	seen := map[string]bool{}
	for _, h := range hits {
		key := h.Trace + "/" + h.Span + "/" + h.AttributeKey + "/" + h.MessageID
		if seen[key] {
			return len(hits), componentErrorf("duplicate hit %s", key)
		}
		seen[key] = true
		if err := ok(h); err != nil {
			return len(hits), err
		}
	}
	return len(hits), nil
}

func benchFTSQueries(n, k int, gen *benchGen, exp *otel.Exporter, chat, model otel.Attribute) []benchQuery {
	return []benchQuery{
		{QueryFTSSingleToken, true, func(ctx context.Context, t benchTarget) (int, error) {
			page, err := exp.QueryMessageFullText(ctx, otel.MessageFullTextQuery{Query: fmt.Sprintf("incident_token_%d", t.block), Mode: otel.MessageMatchAll, Role: "user"})
			noteBenchWork(ctx, page.Candidates, page.Bytes)
			if err != nil {
				return 0, err
			}
			// User message j of each conversation is in the input of turns j..n.
			return checkBenchFTSHits(page.Hits, min(t.size*n*(n+1)/2, k), func(h otel.MessageSpanHit) error {
				if h.Role != "user" || h.AttributeKey != benchInputKey || !benchInBlock(gen, h.Trace, t.block) {
					return componentErrorf("unexpected hit %+v", h)
				}
				return nil
			})
		}},
		{QueryFTSPhraseFirstTurn, false, func(ctx context.Context, t benchTarget) (int, error) {
			c, turn := t.conv, t.iter%n+1
			page, err := exp.QueryMessageFullText(ctx, otel.MessageFullTextQuery{Query: fmt.Sprintf("user turn %d conversation %d conv_%d", turn, c, c), Mode: otel.MessageMatchPhrase, FirstTurnOnly: true})
			noteBenchWork(ctx, page.Candidates, page.Bytes)
			if err != nil {
				return 0, err
			}
			trace, span := gen.traceHex(c), gen.spanHex(c, turn)
			return checkBenchFTSHits(page.Hits, 1, func(h otel.MessageSpanHit) error {
				if h.Trace != trace || h.Span != span || h.AttributeKey != benchInputKey || h.Role != "user" {
					return componentErrorf("hit %+v, want conversation %d turn %d", h, c, turn)
				}
				return nil
			})
		}},
		{QueryFTSMultiTurnHistory, false, func(ctx context.Context, t benchTarget) (int, error) {
			c := t.conv
			page, err := exp.QueryMessageFullText(ctx, otel.MessageFullTextQuery{Query: fmt.Sprintf("user turn 1 conversation %d conv_%d", c, c), Mode: otel.MessageMatchPhrase, AttributeKey: benchInputKey})
			noteBenchWork(ctx, page.Candidates, page.Bytes)
			if err != nil {
				return 0, err
			}
			trace := gen.traceHex(c)
			return checkBenchFTSHits(page.Hits, min(n, k), func(h otel.MessageSpanHit) error {
				if h.Trace != trace || h.AttributeKey != benchInputKey || !slices.Equal(h.TurnOrdinals, []int{0}) {
					return componentErrorf("hit %+v outside conversation %d's first message", h, c)
				}
				return nil
			})
		}},
		{QueryFTSSameSpanPreds, true, func(ctx context.Context, t benchTarget) (int, error) {
			page, err := exp.QueryMessageFullText(ctx, otel.MessageFullTextQuery{Query: fmt.Sprintf("incident_token_%d", t.block), FirstTurnOnly: true, Predicates: []otel.Attribute{chat, model}})
			noteBenchWork(ctx, page.Candidates, page.Bytes)
			if err != nil {
				return 0, err
			}
			return checkBenchFTSHits(page.Hits, min(t.size*n, k), func(h otel.MessageSpanHit) error {
				if h.Role != "user" || h.AttributeKey != benchInputKey || !benchInBlock(gen, h.Trace, t.block) {
					return componentErrorf("unexpected hit %+v", h)
				}
				return nil
			})
		}},
	}
}

func benchTraceQueries(cfg CollectorBenchConfig, n, k int, gen *benchGen, exp *otel.Exporter, chat, model otel.Attribute) []benchQuery {
	return []benchQuery{
		{QuerySameSpanConjunction, false, func(ctx context.Context, t benchTarget) (int, error) {
			c := t.conv
			convID := otel.Attribute{Scope: "span", Key: "gen_ai.conversation.id", Type: "string", Value: gen.convID(c)}
			page, err := exp.QuerySameSpan(ctx, otel.SameSpanQuery{Predicates: []otel.Attribute{convID, chat, model}})
			if err != nil {
				return 0, err
			}
			if len(page.Spans) != min(n, k) {
				return len(page.Spans), componentErrorf("%d spans, want %d", len(page.Spans), min(n, k))
			}
			trace := gen.traceHex(c)
			ids := map[string]bool{}
			for turn := 1; turn <= n; turn++ {
				ids[gen.spanHex(c, turn)] = true
			}
			seen := map[string]bool{}
			for _, s := range page.Spans {
				if s.Trace != trace || seen[s.ID] || !ids[s.ID] {
					return len(page.Spans), componentErrorf("unexpected span %+v for conversation %d", s, c)
				}
				seen[s.ID] = true
			}
			return len(page.Spans), nil
		}},
		{QueryTraceExistential3, true, func(ctx context.Context, t benchTarget) (int, error) {
			q := otel.TraceExistentialQuery{
				Clauses:    []otel.Attribute{{Scope: "span", Key: "incident.block", Type: "string", Value: fmt.Sprintf("block_%d", t.block)}},
				LogClauses: []otel.Attribute{{Scope: otel.LogScopeRecord, Key: "incident.bucket", Type: "string", Value: fmt.Sprintf("bucket_%d", t.block)}},
			}
			if cfg.EnableMessageFullText {
				q.MessageClauses = []otel.MessageFullTextQuery{{Query: fmt.Sprintf("resolved_token_%d", t.block), Role: "assistant", FirstTurnOnly: true}}
			}
			res, err := exp.QueryTraceExistential(ctx, q)
			if err != nil {
				return 0, err
			}
			want := min(t.size, k)
			if len(res.Hits) != want {
				return len(res.Hits), componentErrorf("%d traces, want %d", len(res.Hits), want)
			}
			seen := map[string]bool{}
			for _, h := range res.Hits {
				if !benchInBlock(gen, h.Trace, t.block) || seen[h.Trace] {
					return len(res.Hits), componentErrorf("unexpected trace %s for block %d", h.Trace, t.block)
				}
				seen[h.Trace] = true
				if len(h.Witnesses) != 1 || len(h.Witnesses[0]) == 0 || len(h.LogWitnesses) != 1 || len(h.LogWitnesses[0]) == 0 || len(h.MessageWitnesses) != len(q.MessageClauses) {
					return len(res.Hits), componentErrorf("trace %s lacks witnesses", h.Trace)
				}
				for _, w := range h.MessageWitnesses {
					if len(w) == 0 {
						return len(res.Hits), componentErrorf("trace %s lacks message witnesses", h.Trace)
					}
				}
			}
			return len(res.Hits), nil
		}},
		{QueryTraceGraph, false, func(ctx context.Context, t benchTarget) (int, error) {
			c := t.conv
			g, err := exp.TraverseTraceGraph(ctx, gen.traceHex(c))
			if err != nil {
				return 0, err
			}
			if len(g.Spans) != n || len(g.Roots) != 1 || g.Roots[0] != gen.spanHex(c, 1) || g.MaxDepth != n-1 || len(g.Orphans) != 0 || len(g.Cycles) != 0 {
				return len(g.Spans), componentErrorf("conversation %d graph: %d spans, roots %v, max depth %d, orphans %v, cycles %v", c, len(g.Spans), g.Roots, g.MaxDepth, g.Orphans, g.Cycles)
			}
			return len(g.Spans), nil
		}},
		{QueryLogsByTrace, false, func(ctx context.Context, t benchTarget) (int, error) {
			c := t.conv
			logs, err := exp.QueryLogsByTrace(ctx, gen.traceHex(c), "")
			if err != nil {
				return 0, err
			}
			if len(logs) != 1 || logs[0].EventID != gen.logEventID(c) || logs[0].SpanID != gen.spanHex(c, 1) {
				return len(logs), componentErrorf("conversation %d: %d correlated logs, want its one log", c, len(logs))
			}
			return len(logs), nil
		}},
	}
}
