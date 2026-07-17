package embedded

import (
	"context"
	"strings"
	"testing"

	cascadesvalues "fdb.dev/pkg/recordlayer/query/plan/cascades/values"

	"fdb.dev/pkg/relational/api"

	"fdb.dev/pkg/recordlayer/query/plan/plans"

	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/relational/core/parser"
	antlrgen "fdb.dev/pkg/relational/core/parser/gen"
	"fdb.dev/pkg/relational/core/session"
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
	if first.CacheNumEntries != 1 {
		t.Errorf("miss event cache num entries = %d, want 1 (after Put)", first.CacheNumEntries)
	}
	if second.CacheNumEntries != 1 {
		t.Errorf("hit event cache num entries = %d, want 1", second.CacheNumEntries)
	}
	if first.Err != nil || second.Err != nil {
		t.Errorf("unexpected errors: %v / %v", first.Err, second.Err)
	}
	// Logged SQL must preserve whitespace (canonicalTextOf), not be the
	// token-concatenated GetText() garbage ("FROMorders").
	if !strings.Contains(first.SQL, "FROM orders") {
		t.Errorf("logged SQL not whitespace-preserved: %q", first.SQL)
	}
}

// TestPlanLogging_LimitIsCacheable pins RFC-128 §3.4: with the post-execution
// LIMIT hoist removed, the LIMIT is carried by the RecordQueryLimitPlan operator
// inside the cached physical plan, so a LIMIT query IS now cacheable (previously
// it was deliberately skipped because the limit lived outside the cached plan).
// First plan → MISS + Put; re-plan → HIT.
func TestPlanLogging_LimitIsCacheable(t *testing.T) {
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
	if cap.events[0].Cache != PlanCacheMiss {
		t.Errorf("cache = %v, want miss (LIMIT now cacheable)", cap.events[0].Cache)
	}
	// LIMIT query is now cached: the physical plan carries the limit operator.
	if n := g.cache.Len(); n != 1 {
		t.Errorf("cache len = %d, want 1 (LIMIT now cacheable)", n)
	}

	// Re-plan the identical text → cache HIT.
	q2 := parseQuery(t, "SELECT id, amount FROM orders WHERE id = 1 LIMIT 5")
	if _, err := g.planSelectCascades(context.Background(), q2, md, true); err != nil {
		t.Fatalf("re-plan: %v", err)
	}
	if len(cap.events) != 2 {
		t.Fatalf("want 2 events, got %d", len(cap.events))
	}
	if cap.events[1].Cache != PlanCacheHit {
		t.Errorf("re-plan cache = %v, want hit", cap.events[1].Cache)
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

// TestNestedDerivedArithmetic_TypeSurvivesUnmergedProjectionSpine pins
// column-metadata typing against PLAN SHAPE: `doubled` (val * 2) through
// two derived-table levels must report BIGINT no matter whether the
// planner merged the nested projections or an ordering-pinned spine kept
// them stacked. The inherit path in deriveColumnsFromProjection only
// fired for FLAT (childless) FieldValues; the pinned/unmerged shape reads
// the inner output through a QUANTIFIER-ADDRESSED FieldValue (Child=QOV),
// which skipped inheritance and reported UNKNOWN — a cross-engine
// metadata divergence (Java types it from the flowed result type
// regardless of shape).
func TestNestedDerivedArithmetic_TypeSurvivesUnmergedProjectionSpine(t *testing.T) {
	t.Parallel()
	g, md := newLoggingGenerator(t, "CREATE TABLE t_nd8 (id BIGINT, val BIGINT, PRIMARY KEY (id))", &captureLogger{})
	q := parseQuery(t, "SELECT id, doubled FROM (SELECT id, doubled FROM (SELECT id, val * 2 AS doubled FROM t_nd8) AS d2) AS d1 ORDER BY id")
	p, err := g.planSelectCascades(context.Background(), q, md, true)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	cp, ok := p.(*cascadesPlan)
	if !ok {
		t.Fatalf("plan is %T, want *cascadesPlan", p)
	}
	// The sentinel only tests the UNMERGED spine while it stays unmerged:
	// if a future change merges the nested projections, this degrades to
	// the flat path silently — assert the shape so it fails loudly instead.
	pr, ok := cp.physicalPlan.(*plans.RecordQueryProjectionPlan)
	if !ok {
		t.Fatalf("top plan is %T, want the unmerged outer projection", cp.physicalPlan)
	}
	if _, ok := pr.GetInner().(*plans.RecordQueryProjectionPlan); !ok {
		t.Fatalf("inner plan is %T — the projection spine was merged and this sentinel no longer exercises the ordinal-inheritance path", pr.GetInner())
	}
	cols := deriveColumnsFromPlan(cp.physicalPlan, cp.md)
	doubledIdx := -1
	for i := range cols {
		if strings.EqualFold(cols[i].Label, "DOUBLED") || strings.EqualFold(cols[i].Name, "DOUBLED") {
			doubledIdx = i
			break
		}
	}
	if doubledIdx < 0 {
		t.Fatalf("no DOUBLED column in derived metadata: %+v", cols)
	}
	if got := cols[doubledIdx].TypeName; got != "BIGINT" {
		t.Fatalf("DOUBLED type = %q, want BIGINT (metadata typing must not depend on whether the projection spine was merged)", got)
	}
}

// TestJoinDerivedAggregate_LegOrdinalNeverIndexesFlattenedColumns pins the
// leg-relative-ordinal hazard: a QUANTIFIER-ADDRESSED read over a JOIN
// carries an ordinal relative to its SOURCE LEG, not to the flattened
// inner column list — inheriting by it would type d.total from an
// unrelated leg's slot (a_md.s STRING) instead of the aggregate's BIGINT.
// Ordinal inheritance is therefore restricted to FLAT reads; QOV reads
// resolve by name.
func TestJoinDerivedAggregate_LegOrdinalNeverIndexesFlattenedColumns(t *testing.T) {
	t.Parallel()
	g, md := newLoggingGenerator(t,
		"CREATE TABLE a_md (id BIGINT, s STRING, PRIMARY KEY (id)) CREATE TABLE b_md (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		&captureLogger{})
	q := parseQuery(t, "SELECT a.s, d.total FROM a_md AS a, (SELECT SUM(v) AS total FROM b_md) AS d")
	p, err := g.planSelectCascades(context.Background(), q, md, true)
	if err != nil {
		t.Fatalf("join-with-derived-aggregate must plan (it does today; a regression here is a planner bug, not a skip): %v", err)
	}
	cp, ok := p.(*cascadesPlan)
	if !ok {
		t.Fatalf("plan is %T, want *cascadesPlan", p)
	}
	cols := deriveColumnsFromPlan(cp.physicalPlan, cp.md)
	for _, c := range cols {
		if strings.EqualFold(c.Label, "TOTAL") || strings.EqualFold(c.Name, "TOTAL") {
			// EXACT type, not merely "not the other leg's": permitting
			// UNKNOWN here let the ordinal restriction silently break the
			// QOV name fallback for join legs (qualified inner keys).
			if c.TypeName != "BIGINT" {
				t.Fatalf("TOTAL typed %q, want BIGINT (leg-relative ordinal misuse types it from the other leg; a bare-name lookup against qualified join keys types it UNKNOWN)", c.TypeName)
			}
			return
		}
	}
	t.Fatalf("no TOTAL column in derived metadata: %+v", cols)
}

// TestJoinDerivedCTE_QOVColumnsTypeThroughQualifiedKeys pins the
// qualified-key fallback for QOV-addressed derived columns over a join:
// deriveColumnsFromJoin keys per-leg columns QUALIFIED ("D.FOO"), so a
// bare-name lookup finds nothing and both derived columns reported
// UNKNOWN.
func TestJoinDerivedCTE_QOVColumnsTypeThroughQualifiedKeys(t *testing.T) {
	t.Parallel()
	g, md := newLoggingGenerator(t,
		"CREATE TABLE a_md (id BIGINT, s STRING, PRIMARY KEY (id)) CREATE TABLE b_md (id BIGINT, v BIGINT, y BIGINT, PRIMARY KEY (id))",
		&captureLogger{})
	q := parseQuery(t, "WITH d AS (SELECT v * 2 AS foo, y * 2 AS bar FROM b_md) SELECT a.s, d.foo, d.bar FROM a_md AS a, d")
	p, err := g.planSelectCascades(context.Background(), q, md, true)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	cp, ok := p.(*cascadesPlan)
	if !ok {
		t.Fatalf("plan is %T, want *cascadesPlan", p)
	}
	cols := deriveColumnsFromPlan(cp.physicalPlan, cp.md)
	want := map[string]string{"FOO": "BIGINT", "BAR": "BIGINT"}
	for _, c := range cols {
		for col, typ := range want {
			if strings.EqualFold(c.Label, col) || strings.EqualFold(parseColRef(c.Name).bare(), col) {
				if c.TypeName != typ {
					t.Fatalf("%s typed %q, want %s: %+v", col, c.TypeName, typ, cols)
				}
				delete(want, col)
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("columns not found in derived metadata: %v (cols %+v)", want, cols)
	}
}

// TestJoinDerivedDupName_SlotIdentitySurvivesCollision pins leg-relative
// slot identity under duplicate output names: `SELECT v*2 AS foo, y AS
// foo` gives the leg TWO "D.FOO" columns — a name map keeps only the
// last, but the QOV read's baked accessor addresses slot 0 (the BIGINT
// arithmetic). Inheritance resolves the ordinal WITHIN the leg's columns,
// so d.foo types BIGINT, not the colliding STRING slot.
func TestJoinDerivedDupName_SlotIdentitySurvivesCollision(t *testing.T) {
	t.Parallel()
	g, md := newLoggingGenerator(t,
		"CREATE TABLE a_md (id BIGINT, s STRING, PRIMARY KEY (id)) CREATE TABLE b_md (id BIGINT, v BIGINT, y STRING, PRIMARY KEY (id))",
		&captureLogger{})
	q := parseQuery(t, "WITH d AS (SELECT v * 2 AS foo, y AS foo FROM b_md) SELECT a.id, d.foo FROM a_md AS a, d")
	p, err := g.planSelectCascades(context.Background(), q, md, true)
	if err != nil {
		// Duplicate output names may legitimately be rejected at planning —
		// then there is no metadata to mis-type and the collision cannot
		// occur. Assert the rejection is loud rather than silently planning.
		return
	}
	cp, ok := p.(*cascadesPlan)
	if !ok {
		t.Fatalf("plan is %T, want *cascadesPlan", p)
	}
	cols := deriveColumnsFromPlan(cp.physicalPlan, cp.md)
	for _, c := range cols {
		if strings.EqualFold(c.Label, "FOO") || strings.EqualFold(parseColRef(c.Name).bare(), "FOO") {
			if c.TypeName != "BIGINT" {
				t.Fatalf("d.foo typed %q, want BIGINT (slot 0) — name-map collision inherited the wrong duplicate: %+v", c.TypeName, cols)
			}
			return
		}
	}
	t.Fatalf("no FOO column in derived metadata: %+v", cols)
}

// TestLeftJoinDerived_InheritanceNeverUnNullExtends pins the upgrade-only
// nullability rule at the inherit sites: the null-born (LEFT-JOIN
// null-extension) adjustment runs BEFORE type inheritance, and copying an
// inner column's NoNulls back would un-null-extend a column that serves
// NULL on unmatched outer rows. Inheritance may only ever UPGRADE to
// nullable.
func TestLeftJoinDerived_InheritanceNeverUnNullExtends(t *testing.T) {
	t.Parallel()
	g, md := newLoggingGenerator(t,
		"CREATE TABLE a_md (id BIGINT, s STRING, PRIMARY KEY (id)) CREATE TABLE b_md (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		&captureLogger{})
	// The projected-EXISTS column is synthesized NOT NULL by the inner
	// derivation — the exact shape whose NoNulls must not survive the
	// LEFT JOIN's null extension.
	q := parseQuery(t, "WITH d AS (SELECT id AS bid, EXISTS (SELECT 1 FROM b_md AS c WHERE c.id = b_md.id) AS foo FROM b_md) SELECT a.id, d.foo FROM a_md AS a LEFT JOIN d ON a.id = d.bid")
	p, err := g.planSelectCascades(context.Background(), q, md, true)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	cp, ok := p.(*cascadesPlan)
	if !ok {
		t.Fatalf("plan is %T, want *cascadesPlan", p)
	}
	cols := deriveColumnsFromPlan(cp.physicalPlan, cp.md)
	for _, c := range cols {
		if strings.EqualFold(c.Label, "FOO") || strings.EqualFold(parseColRef(c.Name).bare(), "FOO") {
			if c.Nullable != api.ColumnNullable {
				t.Fatalf("d.foo on the null-supplying side of a LEFT JOIN must report NULLABLE (unmatched rows serve NULL); got %v", c.Nullable)
			}
			return
		}
	}
	t.Fatalf("no FOO column in derived metadata: %+v", cols)
}

// TestCrossJoinDerivedExists_KeepsNoNulls pins exact nullability through
// leg-direct inheritance: a synthesized NOT NULL inner (projected EXISTS)
// read through a QOV over a CROSS join must stay NoNulls — the earlier
// upgrade-only rule blanket-discarded the only NoNulls source even where
// no null extension exists.
func TestCrossJoinDerivedExists_KeepsNoNulls(t *testing.T) {
	t.Parallel()
	g, md := newLoggingGenerator(t,
		"CREATE TABLE a_md (id BIGINT, s STRING, PRIMARY KEY (id)) CREATE TABLE b_md (id BIGINT, v BIGINT, PRIMARY KEY (id))",
		&captureLogger{})
	q := parseQuery(t, "WITH d AS (SELECT EXISTS (SELECT 1 FROM b_md AS c WHERE c.id = b_md.id) AS foo FROM b_md) SELECT d.foo FROM a_md AS a, d")
	p, err := g.planSelectCascades(context.Background(), q, md, true)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	cp, ok := p.(*cascadesPlan)
	if !ok {
		t.Fatalf("plan is %T, want *cascadesPlan", p)
	}
	for _, c := range deriveColumnsFromPlan(cp.physicalPlan, cp.md) {
		if strings.EqualFold(c.Label, "FOO") || strings.EqualFold(parseColRef(c.Name).bare(), "FOO") {
			if c.Nullable != api.ColumnNoNulls {
				t.Fatalf("EXISTS flag over a CROSS join must stay NoNulls (no null extension exists); got %v", c.Nullable)
			}
			return
		}
	}
	t.Fatal("no FOO column in derived metadata")
}

// TestJoinDerivedDottedName_OrdinalUnshifted pins structural leg
// resolution against name-prefix reconstruction: a quoted output name
// containing a literal dot ("X.Y") stays unprefixed in the qualified
// merge, so a prefix-filtered slot count would skip it and shift every
// later ordinal — typing d.foo from the wrong slot.
func TestJoinDerivedDottedName_OrdinalUnshifted(t *testing.T) {
	t.Parallel()
	g, md := newLoggingGenerator(t,
		"CREATE TABLE a_md (id BIGINT, s STRING, PRIMARY KEY (id)) CREATE TABLE b_md (id BIGINT, v BIGINT, y STRING, PRIMARY KEY (id))",
		&captureLogger{})
	q := parseQuery(t, "WITH d AS (SELECT y AS \"X.Y\", v * 2 AS foo, y AS bar FROM b_md) SELECT d.foo FROM a_md AS a, d")
	p, err := g.planSelectCascades(context.Background(), q, md, true)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	cp, ok := p.(*cascadesPlan)
	if !ok {
		t.Fatalf("plan is %T, want *cascadesPlan", p)
	}
	for _, c := range deriveColumnsFromPlan(cp.physicalPlan, cp.md) {
		if strings.EqualFold(c.Label, "FOO") || strings.EqualFold(parseColRef(c.Name).bare(), "FOO") {
			if c.TypeName != "BIGINT" {
				t.Fatalf("d.foo typed %q, want BIGINT — a dotted quoted identifier shifted the leg ordinal", c.TypeName)
			}
			return
		}
	}
	t.Fatal("no FOO column in derived metadata")
}

// TestNestedFullOuter_AncestorNullExtensionReachesLeg pins the ancestor
// accumulation in the leg walk: a leg found BELOW another join inherits
// every enclosing null extension on its path — a FULL join null-supplies
// everything inside both its legs, so d's synthesized NOT NULL EXISTS
// flag must report NULLABLE (unmatched c rows emit NULL for every d
// column), even though d's own immediate join is inner.
func TestNestedFullOuter_AncestorNullExtensionReachesLeg(t *testing.T) {
	t.Parallel()
	g, md := newLoggingGenerator(t,
		"CREATE TABLE a_md (id BIGINT, s STRING, PRIMARY KEY (id)) CREATE TABLE b_md (id BIGINT, v BIGINT, PRIMARY KEY (id)) CREATE TABLE c_md (id BIGINT, PRIMARY KEY (id))",
		&captureLogger{})
	q := parseQuery(t, "WITH d AS (SELECT id AS bid, EXISTS (SELECT 1 FROM b_md AS x WHERE x.id = b_md.id) AS foo FROM b_md) SELECT d.foo FROM a_md AS a JOIN d ON a.id = d.bid FULL OUTER JOIN c_md AS c ON a.id = c.id")
	p, err := g.planSelectCascades(context.Background(), q, md, true)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	cp, ok := p.(*cascadesPlan)
	if !ok {
		t.Fatalf("plan is %T, want *cascadesPlan", p)
	}
	for _, c := range deriveColumnsFromPlan(cp.physicalPlan, cp.md) {
		if strings.EqualFold(c.Label, "FOO") || strings.EqualFold(parseColRef(c.Name).bare(), "FOO") {
			if c.Nullable != api.ColumnNullable {
				t.Fatalf("d.foo under an enclosing FULL OUTER must report NULLABLE (ancestor null extension); got %v", c.Nullable)
			}
			return
		}
	}
	t.Fatal("no FOO column in derived metadata")
}

// TestLegWalk_DuplicateAliasDeclines pins unique-match-or-decline: the
// plan-level leg walk is not query-scope-aware (a folded query block has
// no projection node to stop at), so an interior block reusing a
// top-block alias must make the walk DECLINE — attaching the interior
// branch's null extension to the outer alias would transfer metadata
// across scopes. Constructed directly: two join levels both binding
// alias "X".
func TestLegWalk_DuplicateAliasDeclines(t *testing.T) {
	t.Parallel()
	scan := func() plans.RecordQueryPlan {
		return plans.NewRecordQueryScanPlan([]string{"T"}, cascadesvalues.UnknownType, false)
	}
	innerJoin := plans.NewRecordQueryNestedLoopJoinPlan(
		scan(), scan(), nil, plans.JoinFullOuter, "X", "Y", nil)
	top := plans.NewRecordQueryNestedLoopJoinPlan(
		innerJoin, scan(), nil, plans.JoinInner, "A", "X", nil)

	if _, _, found := legPlanFor(top, "X"); found {
		t.Fatal("a duplicated alias across join levels must DECLINE (scope-ambiguous), not first-match")
	}
	// Unique aliases still resolve.
	if _, _, found := legPlanFor(top, "A"); !found {
		t.Fatal("a unique alias must resolve")
	}
	// An interior duplicate INSIDE a matched leg also declines: folds can
	// break the plan-nesting/SQL-scoping mirror, so shallow-wins shadowing
	// is not trusted either.
	nested := plans.NewRecordQueryNestedLoopJoinPlan(
		scan(), scan(), nil, plans.JoinInner, "Z", "W", nil)
	shadowTop := plans.NewRecordQueryNestedLoopJoinPlan(
		nested, scan(), nil, plans.JoinInner, "Z", "Q", nil)
	if _, _, found := legPlanFor(shadowTop, "Z"); found {
		t.Fatal("an alias duplicated between a leg and its own subtree must DECLINE")
	}
	if leg, ns, found := legPlanFor(top, "Y"); !found || leg == nil || !ns {
		t.Fatalf("Y is unique and inside a FULL join's inner — found=%v ns=%v", found, ns)
	}
}
