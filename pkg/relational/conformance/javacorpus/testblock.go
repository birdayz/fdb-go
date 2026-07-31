package javacorpus

import (
	"context"
	"database/sql"
	"fmt"
	"hash/fnv"
	"strings"

	"fdb.dev/pkg/relational/conformance/javayamsql"
)

// blockOptions is TestBlock.TestBlockOptions after the defaults < preset <
// options-map layering. There is no execution-context layer here: its two
// inputs are a harness-wide seed override and nightly repetition, neither of
// which this runner has.
type blockOptions struct {
	Repetition          int64
	Mode                string
	Seed                int64
	SeedExplicit        bool
	CheckCache          bool
	ConnectionLifecycle string
	StatementType       string
}

// resolveOptions applies TestBlock's layering. The defaults are Java's field
// initialisers, which are NOT the same as "everything absent": a test_block
// with no preset and no options runs each test five times, in parallel, with a
// cache-check pass.
func resolveOptions(b *javayamsql.TestBlock) blockOptions {
	o := blockOptions{
		Repetition:          5,
		Mode:                "parallelized",
		CheckCache:          true,
		ConnectionLifecycle: "test",
		StatementType:       "both",
	}
	switch b.Preset {
	case "single_repetition_ordered", "single_repetition_randomized", "single_repetition_parallelized":
		o.Repetition = 1
		o.CheckCache = false
	case "multi_repetition_ordered", "multi_repetition_randomized", "multi_repetition_parallelized":
		o.Repetition = 5
	}
	switch {
	case strings.HasSuffix(b.Preset, "_ordered"):
		o.Mode = "ordered"
	case strings.HasSuffix(b.Preset, "_randomized"):
		o.Mode = "randomized"
	case strings.HasSuffix(b.Preset, "_parallelized"):
		o.Mode = "parallelized"
	}

	opt := b.Options
	if opt.Mode != "" {
		o.Mode = opt.Mode
	}
	if opt.Repetition != nil {
		o.Repetition = *opt.Repetition
	}
	if opt.Seed != nil {
		o.Seed = *opt.Seed
		o.SeedExplicit = true
	}
	if opt.CheckCache != nil {
		o.CheckCache = *opt.CheckCache
	}
	if opt.ConnectionLifecycle != "" {
		o.ConnectionLifecycle = opt.ConnectionLifecycle
	}
	if opt.StatementType != "" {
		o.StatementType = opt.StatementType
	}
	return o
}

// executable is one scheduled run of one test's whole config chain.
type executable struct {
	test *javayamsql.Test
	// rep is the 0-based repetition index, carried for failure messages.
	rep int64
}

func (r *runner) executeTestBlock(ctx context.Context, resource string, blk *javayamsql.Block) error {
	b := blk.Test
	where := resource + " test_block " + blockName(b)

	if !javayamsql.SupportedAtCurrentVersion(b.Options.SupportedVersion) {
		r.skip(SkipVersionGate, where, "block supported_version")
		return nil
	}

	o := resolveOptions(b)
	switch o.StatementType {
	case "prepared":
		// Running the simple arm here would be a different test, not a
		// partial one: the block exists to exercise the prepared path.
		r.skip(SkipPrepared, where, "statement_type: prepared")
		return nil
	case "both":
		// Java runs both arms and requires the mix to contain each at least
		// once. Only the simple arm runs here, so the prepared half is a
		// counted omission rather than a silent one.
		r.skip(SkipPrepared, where, "statement_type defaults to BOTH; only the simple arm runs")
	}
	if o.CheckCache {
		// The extra pass is cheap; the assertion attached to it is not. Java
		// compares PLAN_CACHE_TERTIARY_HIT before and after and requires
		// exactly +1, which needs a per-connection metric collector.
		r.skip(SkipCheckCache, where, "check_cache pass needs a per-connection plan-cache metric collector")
	}

	target, err := r.resolveConnect(resource, b.Connect)
	if err != nil {
		return fmt.Errorf("%s: connect: %w", where, err)
	}
	db, err := r.open(target)
	if err != nil {
		return err
	}

	// Java builds `repetition` executables per test and then shuffles the
	// flattened list, so the copies of one test do not stay adjacent.
	var execs []executable
	for _, t := range b.Tests {
		for i := int64(0); i < o.Repetition; i++ {
			execs = append(execs, executable{test: t, rep: i})
		}
	}
	if o.Mode != "ordered" {
		shuffle(execs, blockSeed(o, where))
	}

	// connection_lifecycle: BLOCK holds one connection for every executable,
	// TEST takes a fresh one per executable. Mode `parallelized` shares the
	// shuffle with `randomized`; the thread-pool dispatch is not reproduced —
	// concurrency against one cluster is supplied at the coarser grain of
	// t.Parallel over corpus files, which is the same dimension without
	// multiplying live FDB connections by the block size.
	if o.ConnectionLifecycle == "block" {
		conn, err := db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("%s: conn: %w", where, err)
		}
		defer conn.Close()
		for _, e := range execs {
			if err := r.runTest(ctx, conn, where, e); err != nil {
				return err
			}
		}
		return nil
	}
	for _, e := range execs {
		conn, err := db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("%s: conn: %w", where, err)
		}
		err = r.runTest(ctx, conn, where, e)
		conn.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func blockName(b *javayamsql.TestBlock) string {
	if b.Name != "" {
		return b.Name
	}
	return "unnamed"
}

// blockSeed picks the shuffle seed.
//
// Java's default is System.currentTimeMillis(), which cannot be reproduced and
// would make a failure un-rerunnable. A declared `seed:` is honoured; otherwise
// the seed is derived from the block's identity, so the order is stable across
// runs and still differs between blocks.
func blockSeed(o blockOptions, where string) int64 {
	if o.SeedExplicit {
		return o.Seed
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(where))
	return int64(h.Sum64() & 0x7FFFFFFFFFFFFFFF)
}

// shuffle is java.util.Collections.shuffle: Fisher-Yates walking down from the
// end, drawing nextInt(i) at each step.
func shuffle(list []executable, seed int64) {
	rnd := newJavaRandom(seed)
	for i := len(list); i > 1; i-- {
		j := rnd.nextInt(int32(i))
		list[i-1], list[j] = list[j], list[i-1]
	}
}

// runTest ports QueryCommand.executeInternal's single pass over the config
// list, minus the mechanisms later phases add.
func (r *runner) runTest(ctx context.Context, conn *sql.Conn, where string, e executable) error {
	cmd := e.test.Command
	at := fmt.Sprintf("%s line %d", where, cmd.Line)

	if cmd.Kind != javayamsql.CommandQuery {
		r.skip(SkipSchemaCommand, at, string(cmd.Kind))
		return nil
	}

	query, ok := r.adaptQuery(cmd, at)
	if !ok {
		return nil
	}

	// A query with no configs of its own gets Java's synthetic noChecks: it
	// runs, its rows are drained, and nothing is asserted.
	//
	// It must NOT count towards QueriesRun. QueriesRun is the denominator the
	// vacuous-pass guard tests, so counting a noChecks query here would let a
	// file whose ONLY query asserts nothing report a pass — which is precisely
	// the shape of green the guard exists to catch. scenario-tests.yamsql is
	// exactly that file upstream ("# TODO: add data", one config-less query).
	if cmd.ImplicitNoChecks() {
		r.skip(SkipNoChecks, at, "query declares no configs")
		if _, err := execAny(ctx, conn, query); err != nil {
			return fmt.Errorf("%s: %q: %w", at, truncate(query), err)
		}
		return nil
	}

	plan := classifyConfigs(cmd)
	for _, s := range plan.Skips {
		r.skip(s.Class, at, s.Detail)
	}
	if plan.SkipQuery != "" {
		r.skip(plan.SkipQuery, at, truncate(query))
		return nil
	}
	if len(plan.Consuming) == 0 {
		// Every result-consuming config was gated away by an initialVersion
		// marker. Nothing is asserted, and nothing is skipped either: Java's
		// `shouldExecute` guard skips those configs too, and a sticky metadata
		// directive left armed behind them is never read on either side. An
		// inert directive is not a declined capability, so counting it would
		// inflate the ledger with something no engine gap caused.
		return nil
	}
	if len(plan.Consuming) > 1 {
		// Each result-consuming config draws one page. More than one means the
		// query is paginated, which is the continuation surface's job — and
		// that claims any metadata directive on the query with it, since Java
		// re-checks the sticky metadata on EVERY page.
		r.skip(SkipContinuation, at, fmt.Sprintf("%d result-consuming configs require %d pages",
			len(plan.Consuming), len(plan.Consuming)))
		return nil
	}

	cfg := plan.Consuming[0]
	if err := r.runConfig(ctx, conn, at, query, cfg, plan.Metadata); err != nil {
		return err
	}
	r.result.QueriesRun++
	return nil
}

// pendingSkip is a counted skip whose location the caller supplies.
type pendingSkip struct {
	Class  SkipClass
	Detail string
}

// configPlan is the outcome of the pre-pass over one query's config list.
type configPlan struct {
	// SkipQuery, when non-empty, is the class the WHOLE query is skipped under.
	SkipQuery SkipClass
	// Consuming are the result-drawing configs, in source order.
	Consuming []*javayamsql.Config
	// Metadata is the sticky `resultMetadata:` armed before Consuming[0], or
	// nil when none precedes it.
	Metadata *javayamsql.Config
	// Skips are per-directive counted skips that do not claim the query.
	Skips []pendingSkip
}

// classifyConfigs is QueryCommand.executeInternal's walk over the config list,
// minus the execution.
//
// It is separated from the execution so the ORDER-DEPENDENT decisions can be
// asserted without a cluster: which consumer a metadata directive attaches to,
// what an initialVersion marker removes, and which directive claims the query
// are all positional, and every one of them is invisible to a test that can
// only observe the final rows.
func classifyConfigs(cmd *javayamsql.Command) configPlan {
	var plan configPlan

	// The version markers are sticky mode switches, so a config's fate depends
	// on the most recent one before it.
	//
	// `resultMetadata:` is sticky in the same way, and in Java's own sense: it
	// is never executed on its own, it arms an INLINE check that the next
	// result-consuming config performs before drawing rows
	// (QueryCommand.executeInternal's stickyMetadata). A directive that appears
	// AFTER the only consumer therefore attaches to nothing — Java sets the
	// field, the loop ends, and it is never read.
	shouldExecute := true
	var pendingMetadata *javayamsql.Config
	for _, cfg := range cmd.Configs {
		switch cfg.Kind {
		case javayamsql.ConfigInitialVersionAtLeast:
			shouldExecute = javayamsql.SelectedAtCurrentVersion(cfg.Version, nil)
			continue
		case javayamsql.ConfigInitialVersionLessThan:
			shouldExecute = javayamsql.SelectedAtCurrentVersion(nil, cfg.Version)
			continue
		}
		if !shouldExecute {
			continue
		}
		switch cfg.Kind {
		case javayamsql.ConfigSupportedVersion:
			if !javayamsql.SupportedAtCurrentVersion(cfg.Version) {
				plan.SkipQuery = SkipVersionGate
			}
		case javayamsql.ConfigExplain, javayamsql.ConfigExplainContains, javayamsql.ConfigPlanHash:
			plan.Skips = append(plan.Skips, pendingSkip{SkipPlanAssertion, string(cfg.Kind)})
		case javayamsql.ConfigMaxRows:
			// maxRows sets the page size for every config after it. Without a
			// per-page continuation surface the page cannot be bounded, so the
			// query's assertions are not reproducible at all.
			plan.SkipQuery = SkipContinuation
		case javayamsql.ConfigResultMetadata:
			pendingMetadata = cfg
		case javayamsql.ConfigSetup, javayamsql.ConfigSetupReference:
			plan.SkipQuery = SkipTemporaryFunction
		case javayamsql.ConfigDebugger:
			plan.Skips = append(plan.Skips, pendingSkip{SkipDebugger, cfg.Text})
		case javayamsql.ConfigResult, javayamsql.ConfigUnorderedResult,
			javayamsql.ConfigCount, javayamsql.ConfigError:
			if len(plan.Consuming) == 0 {
				plan.Metadata = pendingMetadata
			}
			plan.Consuming = append(plan.Consuming, cfg)
		}
	}
	return plan
}

// metadataIsRead is QueryExecutor.checkInlineMetadataIfPresent's guard: the
// sticky directive is read only when the statement actually produced a result
// set to read it from.
//
// Java returns early otherwise — `!(queryResult instanceof RelationalResultSet)`
// — and QueryConfig.validateConfigs deliberately admits `error:` and `count:`
// as the required consumer, so a corpus file may legally arm a directive that
// nothing ever reads. Treating that as a runner error would reject a file Java
// accepts; leaving it undocumented would make the silence look accidental.
func metadataIsRead(target *javayamsql.Config, rowReturning bool) bool {
	if target == nil {
		return false
	}
	switch target.Kind {
	case javayamsql.ConfigError, javayamsql.ConfigCount:
		// An expected error produces no result set; `count:` produces an update
		// count, which is not one either.
		return false
	}
	return rowReturning
}

// adaptQuery performs Java's adaptToSimpleStatement substitution.
func (r *runner) adaptQuery(cmd *javayamsql.Command, at string) (string, bool) {
	if len(cmd.Segments) == 0 {
		return cmd.Query, true
	}
	out := cmd.Query
	for _, seg := range cmd.Segments {
		if seg.Kind == javayamsql.SegmentUnbound {
			// !r and the generator form of !a need the block's shared
			// java.util.Random, drawn in an order interleaved with the
			// prepared/simple mix and the shuffle. Reproducing that stream is
			// only meaningful once the prepared arm exists.
			r.skip(SkipRandomInjection, at, seg.Body)
			return "", false
		}
		text, err := sqlText(seg.Value)
		if err != nil {
			r.skip(SkipRandomInjection, at, err.Error())
			return "", false
		}
		out = strings.ReplaceAll(out, seg.Raw, text)
	}
	return out, true
}

// runConfig executes the query once and checks the single result-consuming
// config against it, plus the sticky `resultMetadata:` directive armed before
// it, if any.
func (r *runner) runConfig(ctx context.Context, conn *sql.Conn, at, query string, cfg, meta *javayamsql.Config) error {
	// Java hands the sticky directive to every consumer and lets
	// checkInlineMetadataIfPresent decide; the guard is hoisted here so the
	// three no-result-set shapes take one documented path instead of three.
	if !metadataIsRead(cfg, isRowReturning(query)) {
		meta = nil
	}

	if cfg.Kind == javayamsql.ConfigError {
		return r.checkError(ctx, conn, at, query, cfg)
	}

	if cfg.Kind == javayamsql.ConfigCount {
		// `count:` is the JDBC update count, not a row count.
		n, err := execAny(ctx, conn, query)
		if err != nil {
			return fmt.Errorf("%s: %q: %w", at, truncate(query), err)
		}
		if cfg.Number == nil || *cfg.Number != n {
			return fmt.Errorf("%s: %q: expected count value %v, but got %d", at, truncate(query), derefCount(cfg), n)
		}
		return nil
	}

	if !isRowReturning(query) {
		// A result expectation against a statement that produces no result set
		// is Java's "actual result set is NULL" branch. metadataIsRead has
		// already cleared any sticky directive for exactly this reason.
		if _, err := execAny(ctx, conn, query); err != nil {
			return fmt.Errorf("%s: %q: %w", at, truncate(query), err)
		}
		if err := matchResultSet(cfg, nil, cfg.Kind == javayamsql.ConfigResult); err != nil {
			return fmt.Errorf("%s: %q: %w", at, truncate(query), err)
		}
		return nil
	}

	rs, err := queryRows(ctx, conn, query)
	if err != nil {
		return fmt.Errorf("%s: %q: %w", at, truncate(query), err)
	}
	// Java reads the metadata off the OPEN result set before any row is drawn,
	// then checks it (QueryExecutor.checkInlineMetadataIfPresent). queryRows
	// captures the column identity at the same point, so the descriptors here
	// are the same pre-iteration read.
	if meta != nil {
		if descends, why := metadataDescends(meta.Raw); descends {
			r.skip(SkipResultMetadataNested, at, why)
		} else if err := matchMetadata(meta.Raw, extractDescriptors(rs)); err != nil {
			return fmt.Errorf("%s: %q: %w", at, truncate(query), err)
		}
	}
	if err := matchResultSet(cfg, rs, cfg.Kind == javayamsql.ConfigResult); err != nil {
		return fmt.Errorf("%s: %q: %w", at, truncate(query), err)
	}
	return nil
}

func derefCount(cfg *javayamsql.Config) any {
	if cfg.Number == nil {
		return "<none>"
	}
	return *cfg.Number
}

func (r *runner) checkError(ctx context.Context, conn *sql.Conn, at, query string, cfg *javayamsql.Config) error {
	var err error
	if isRowReturning(query) {
		// A SELECT's error may surface only while rows are drawn — a division
		// by zero in a projection does not fail at plan time.
		_, err = queryRows(ctx, conn, query)
	} else {
		_, err = execAny(ctx, conn, query)
	}
	if err == nil {
		return fmt.Errorf("%s: %q: expecting statement to throw an error %s, however it succeeded",
			at, truncate(query), cfg.ErrorCode)
	}
	got, ok := errorCodeOf(err)
	if !ok {
		return fmt.Errorf("%s: %q: expecting %s, got a non-SQLSTATE error: %v", at, truncate(query), cfg.ErrorCode, err)
	}
	if got != cfg.ErrorCode {
		return fmt.Errorf("%s: %q: expecting '%s' error code, got '%s' instead (%v)",
			at, truncate(query), cfg.ErrorCode, got, err)
	}
	return nil
}

// sqlText mirrors Parameter.getSqlText for the bound forms.
func sqlText(v *javayamsql.Value) (string, error) {
	switch v.TagName() {
	case javayamsql.TagNullArg:
		return "null", nil
	case javayamsql.TagBytes:
		b, err := decodeBytesTag(v)
		if err != nil {
			return "", err
		}
		return "x'" + hexOf(b) + "'", nil
	case javayamsql.TagUUID:
		s, _ := v.AsString()
		return "'" + s + "'", nil
	case javayamsql.TagRandomStr:
		d, _ := v.AsString()
		s, err := expandRandomStr(d)
		if err != nil {
			return "", err
		}
		return "'" + s + "'", nil
	case javayamsql.TagInList:
		// ConstructInList asserts a mapping of exactly one entry, and that
		// entry must bind to a list. InListParameter.getSqlText then renders
		// the list's ELEMENTS parenthesised — `!in {[1, 2]}` becomes `(1, 2)`,
		// not `([1, 2])`.
		inner := v.Untag()
		if len(inner.Map) != 1 {
			return "", fmt.Errorf("!in expects a set of exactly 1 element")
		}
		list := inner.Map[0].Key.Untag()
		if list.Kind != javayamsql.KindSeq {
			return "", fmt.Errorf("!in expects its element to be a list")
		}
		if len(list.Seq) == 0 {
			// Java aborts the test rather than emitting `()`: an empty IN list
			// in a simple statement would evaluate to an empty result set and
			// silently assert nothing.
			return "", fmt.Errorf("empty inLists are not allowed in simple statements")
		}
		parts := make([]string, 0, len(list.Seq))
		for _, e := range list.Seq {
			t, err := sqlText(e)
			if err != nil {
				return "", err
			}
			parts = append(parts, t)
		}
		return "(" + strings.Join(parts, ", ") + ")", nil
	case javayamsql.TagVector16, javayamsql.TagVector32, javayamsql.TagVector64:
		return "", fmt.Errorf("vector parameter injection is not supported")
	}

	u := v.Untag()
	switch u.Kind {
	case javayamsql.KindNull:
		return "null", nil
	case javayamsql.KindBool:
		if u.Bool {
			return "true", nil
		}
		return "false", nil
	case javayamsql.KindString:
		return "'" + u.Str + "'", nil
	case javayamsql.KindSeq:
		parts := make([]string, 0, len(u.Seq))
		for _, e := range u.Seq {
			t, err := sqlText(e)
			if err != nil {
				return "", err
			}
			parts = append(parts, t)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case javayamsql.KindMap:
		return tupleText(u)
	}
	s, ok := v.AsString()
	if !ok {
		return "", fmt.Errorf("cannot render parameter of kind %s", u.Kind)
	}
	return s, nil
}

// tupleText renders a mapping's KEYS as a parenthesised tuple, which is how
// QueryParameterYamlConstructor builds a TupleParameter — it reads the key
// nodes and discards the values, so `{1, 2, 3}` is the tuple (1, 2, 3).
func tupleText(v *javayamsql.Value) (string, error) {
	u := v.Untag()
	if u.Kind != javayamsql.KindMap {
		t, err := sqlText(v)
		if err != nil {
			return "", err
		}
		return "(" + t + ")", nil
	}
	parts := make([]string, 0, len(u.Map))
	for _, e := range u.Map {
		t, err := sqlText(e.Key)
		if err != nil {
			return "", err
		}
		parts = append(parts, t)
	}
	return "(" + strings.Join(parts, ", ") + ")", nil
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0xf])
	}
	return string(out)
}
