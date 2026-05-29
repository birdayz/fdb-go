package embedded

import (
	"context"
	"strings"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser"
	antlrgen "github.com/birdayz/fdb-record-layer-go/pkg/relational/core/parser/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/relational/core/session"
)

// captureLogger records every PlanGenerationInfo it receives.
type captureLogger struct {
	events []PlanGenerationInfo
}

func (c *captureLogger) LogPlanGeneration(_ context.Context, info PlanGenerationInfo) {
	c.events = append(c.events, info)
}

// newLoggingGenerator builds a cascadesGenerator backed by a DB-less
// connection seeded with metadata from schemaDDL, a live plan cache, and the
// given logger. Drives the real planSelectCascades path (fetchTableStatistics
// no-ops to nil default stats when sess.DB == nil), so no FDB is needed.
func newLoggingGenerator(t *testing.T, schemaDDL string, logger PlanGenerationLogger) (*cascadesGenerator, *recordlayer.RecordMetaData) {
	t.Helper()
	tmpl, err := buildSchemaTemplateFromDDL(schemaDDL)
	if err != nil {
		t.Fatalf("schema DDL: %v", err)
	}
	md := tmpl.Underlying()
	conn := &EmbeddedConnection{
		sess:                     &session.Session{Schema: "s"},
		planCache:                NewPlanCache(256),
		planLogger:               logger,
		slowQueryThresholdMicros: defaultSlowQueryThresholdMicros(),
	}
	return newCascadesGenerator(conn), md
}

// parseQuery extracts the first statement's IQueryContext from a SELECT.
func parseQuery(t *testing.T, sql string) antlrgen.IQueryContext {
	t.Helper()
	root, err := parser.Parse(sql)
	if err != nil {
		t.Fatalf("parse %q: %v", sql, err)
	}
	sel := root.Statements().AllStatement()[0].SelectStatement()
	if sel == nil {
		t.Fatalf("not a SELECT: %q", sql)
	}
	q := sel.Query()
	if q == nil {
		t.Fatalf("malformed SELECT: %q", sql)
	}
	return q
}

func TestPlanLogging_MissThenHit(t *testing.T) {
	t.Parallel()
	cap := &captureLogger{}
	g, md := newLoggingGenerator(t, ordersSchema, cap)
	ctx := context.Background()
	sql := "SELECT id, amount FROM orders WHERE id = 1"

	for i := 0; i < 2; i++ {
		q := parseQuery(t, sql)
		if _, err := g.planSelectCascades(ctx, q, md, true); err != nil {
			t.Fatalf("plan %d: %v", i, err)
		}
	}

	if len(cap.events) != 2 {
		t.Fatalf("want 2 events, got %d", len(cap.events))
	}
	first, second := cap.events[0], cap.events[1]
	if first.Cache != PlanCacheMiss {
		t.Errorf("first event cache = %v, want miss", first.Cache)
	}
	if second.Cache != PlanCacheHit {
		t.Errorf("second event cache = %v, want hit", second.Cache)
	}
	if first.PlanHash == 0 || second.PlanHash == 0 {
		t.Errorf("plan hash should be non-zero: %d / %d", first.PlanHash, second.PlanHash)
	}
	if first.PlanHash != second.PlanHash {
		t.Errorf("plan hash differs across miss/hit: %d != %d", first.PlanHash, second.PlanHash)
	}
	if first.PlanExplain == "" || second.PlanExplain == "" {
		t.Errorf("plan explain should be non-empty")
	}
	if first.PlanningDuration <= 0 {
		t.Errorf("planning duration should be positive, got %v", first.PlanningDuration)
	}
	if second.CacheNumEntries != 1 {
		t.Errorf("cache num entries = %d, want 1", second.CacheNumEntries)
	}
	if first.Err != nil || second.Err != nil {
		t.Errorf("unexpected errors: %v / %v", first.Err, second.Err)
	}
}

func TestPlanLogging_SkipOnLimit(t *testing.T) {
	t.Parallel()
	cap := &captureLogger{}
	g, md := newLoggingGenerator(t, ordersSchema, cap)
	q := parseQuery(t, "SELECT id, amount FROM orders WHERE id = 1 LIMIT 5")
	if _, err := g.planSelectCascades(context.Background(), q, md, true); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(cap.events) != 1 {
		t.Fatalf("want 1 event, got %d", len(cap.events))
	}
	if cap.events[0].Cache != PlanCacheSkip {
		t.Errorf("cache = %v, want skip", cap.events[0].Cache)
	}
	// LIMIT query must not be cached.
	if n := g.cache.Len(); n != 0 {
		t.Errorf("cache len = %d, want 0 (LIMIT not cached)", n)
	}
}

func TestPlanLogging_SkipWhenNoCache(t *testing.T) {
	t.Parallel()
	cap := &captureLogger{}
	g, md := newLoggingGenerator(t, ordersSchema, cap)
	g.cache = nil // disable cache
	g.c.planCache = nil
	q := parseQuery(t, "SELECT id, amount FROM orders WHERE id = 1")
	if _, err := g.planSelectCascades(context.Background(), q, md, true); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(cap.events) != 1 {
		t.Fatalf("want 1 event, got %d", len(cap.events))
	}
	if cap.events[0].Cache != PlanCacheSkip {
		t.Errorf("cache = %v, want skip (no cache configured)", cap.events[0].Cache)
	}
	if cap.events[0].CacheNumEntries != 0 {
		t.Errorf("cache num entries = %d, want 0", cap.events[0].CacheNumEntries)
	}
}

func TestPlanLogging_ErrorIsInconclusive(t *testing.T) {
	t.Parallel()
	cap := &captureLogger{}
	g, md := newLoggingGenerator(t, ordersSchema, cap)
	// References a column that doesn't exist → validation/planning error.
	q := parseQuery(t, "SELECT nonexistent_col FROM orders")
	if _, err := g.planSelectCascades(context.Background(), q, md, true); err == nil {
		t.Fatalf("expected an error for unknown column")
	}
	if len(cap.events) != 1 {
		t.Fatalf("want 1 event, got %d", len(cap.events))
	}
	ev := cap.events[0]
	if ev.Err == nil {
		t.Errorf("event Err should be set")
	}
	if ev.Cache != PlanCacheInconclusive {
		t.Errorf("cache = %v, want inconclusive", ev.Cache)
	}
	if ev.PlanHash != 0 {
		t.Errorf("plan hash = %d, want 0 on error", ev.PlanHash)
	}
}

func TestPlanLogging_SlowQueryFlag(t *testing.T) {
	t.Parallel()
	cap := &captureLogger{}
	g, md := newLoggingGenerator(t, ordersSchema, cap)
	g.c.slowQueryThresholdMicros = 1 // 1µs: any real planning exceeds it
	q := parseQuery(t, "SELECT id FROM orders WHERE id = 1")
	if _, err := g.planSelectCascades(context.Background(), q, md, true); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if !cap.events[0].SlowQuery {
		t.Errorf("expected SlowQuery=true with 1µs threshold")
	}

	cap2 := &captureLogger{}
	g2, md2 := newLoggingGenerator(t, ordersSchema, cap2)
	g2.c.slowQueryThresholdMicros = 1 << 40 // absurdly high
	q2 := parseQuery(t, "SELECT id FROM orders WHERE id = 1")
	if _, err := g2.planSelectCascades(context.Background(), q2, md2, true); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if cap2.events[0].SlowQuery {
		t.Errorf("expected SlowQuery=false with huge threshold")
	}
}

func TestPlanLogging_NilLogger(t *testing.T) {
	t.Parallel()
	// No logger: planning must work and the nil-scope path must be safe.
	g, md := newLoggingGenerator(t, ordersSchema, nil)
	q := parseQuery(t, "SELECT id FROM orders WHERE id = 1")
	if _, err := g.planSelectCascades(context.Background(), q, md, true); err != nil {
		t.Fatalf("plan with nil logger: %v", err)
	}
}

func TestPlanLogging_ExplainDoesNotLog(t *testing.T) {
	t.Parallel()
	cap := &captureLogger{}
	g, md := newLoggingGenerator(t, ordersSchema, cap)
	q := parseQuery(t, "SELECT id FROM orders WHERE id = 1")
	// logMetrics=false simulates the EXPLAIN re-entry from computeExplainText.
	if _, err := g.planSelectCascades(context.Background(), q, md, false); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if len(cap.events) != 0 {
		t.Fatalf("EXPLAIN path must emit no events, got %d", len(cap.events))
	}
}

func TestTruncateSQL(t *testing.T) {
	t.Parallel()
	short := "SELECT 1"
	if got := truncateSQL(short); got != short {
		t.Errorf("short SQL changed: %q", got)
	}
	long := strings.Repeat("x", MaxLoggedSQLLength+100)
	got := truncateSQL(long)
	if !strings.HasSuffix(got, "…(truncated)") {
		t.Errorf("long SQL not marked truncated: ...%q", got[len(got)-20:])
	}
	if len([]rune(strings.TrimSuffix(got, "…(truncated)"))) != MaxLoggedSQLLength {
		t.Errorf("truncated length wrong")
	}
	// Rune-safe: multi-byte runes must not be split.
	multi := strings.Repeat("世", MaxLoggedSQLLength+10)
	gotMulti := truncateSQL(multi)
	if !strings.HasSuffix(gotMulti, "…(truncated)") {
		t.Errorf("multibyte SQL not truncated")
	}
	if !strings.ContainsRune(gotMulti, '世') {
		t.Errorf("multibyte rune corrupted")
	}
}

func TestPlanCacheEvent_String(t *testing.T) {
	t.Parallel()
	cases := map[PlanCacheEvent]string{
		PlanCacheInconclusive: "inconclusive",
		PlanCacheSkip:         "skip",
		PlanCacheHit:          "hit",
		PlanCacheMiss:         "miss",
	}
	for e, want := range cases {
		if got := e.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", e, got, want)
		}
	}
}
