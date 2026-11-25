//go:build bazelrunfiles

package conformance_test

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/internal/relational/relapi"
	"fdb.dev/pkg/relational/conformance/plandiff"
)

// Live-JVM measurements for the RFC-257 WS-F design (rfcs/257-java-upgrade-audit/
// ws-f-design.md): null-safe equality scans, IS DISTINCT FROM residuals, a
// multi-column index and a sparse index (W9); the relational planner
// configuration's index preference against every plan class it can meet, IN-list
// plans with the target's planner rule trace, and IN lists longer than the
// in-union size (W8); enum comparisons over a table column (W10); array subscripts
// with literal and bound indexes (W13); and user identifiers in explain over a
// quoted dotted table name (W13). Both engines' answers are pinned: wsfPins is the
// target's, wsfGoPins Go's current one, which the WS-F port moves on purpose.
var _ = Describe("WS-F target oracle", func() {
	// The acceptance ratchet's verdict arms, driven without the JVM so every arm is
	// exercised whatever the probes currently answer.
	It("measures each acceptance verdict as it is declared", func() {
		ok := `OK [ID:BIGINT] [[1]]`
		jErr := `ERROR XXXXX "class java.lang.Long cannot be cast" [ClassCastException]`
		gErr := `ERROR XXXXX "subscript index must be INT"`
		gOther := `ERROR 0AF00 "projection slot 0 has no resolved Value"`
		for _, c := range []struct{ want, java, goLine, got string }{
			{"SAME", ok, ok, "SAME"},
			{"SAME", ok, `OK [ID:BIGINT] [[2]]`, "DIFF"},
			{"SAME", jErr, `ERROR XXXXX "class java.lang.Long cannot be cast"`, "SAME"}, // the exception class is ignored
			{"SAME-STATE", jErr, gErr, "SAME-STATE"},
			{"SAME-STATE", jErr, gOther, "DIFF"},
			{"SAME-STATE", ok, ok, "SAME-STATE"},
			{"SAME-STATE", ok, gErr, "DIFF"},
			{"DIFF", `OK EXPLAIN "SCAN([IS T1])"`, `OK EXPLAIN "Scan(T1)"`, "DIFF"},
			{"DIFF", ok, ok, "SAME"},
		} {
			Expect(wsfVerdict(c.want, c.java, c.goLine)).To(Equal(c.got), "%s over %q and %q", c.want, c.java, c.goLine)
		}
		Expect(wsfState(jErr)).To(Equal("ERROR XXXXX"))
		Expect(wsfState(ok)).To(Equal(ok))

		// The access-path arms, over one plan in each engine's syntax.
		jCov := `OK EXPLAIN "[IN arrayDistinct(promote(@c7 AS ARRAY(LONG)))] INUNION q0 -> { COVERING(I1 [EQUALS q0] -> [COL1: KEY:[0], ID: KEY:[2]]) } COMPARE BY (_.COL1, _.ID) | FETCH"`
		gSame := `OK EXPLAIN "Fetch(InUnion(IndexScan(I1, [=] COVERING), bindings=1, ASC))"`
		gUnfetched := `OK EXPLAIN "InUnion(IndexScan(I1, [=]), bindings=1, ASC)"`
		gOtherIndex := `OK EXPLAIN "Fetch(InUnion(IndexScan(I9, [=] COVERING), bindings=1, ASC))"`
		for _, c := range []struct {
			entry, java, goLine string
			at                  bool
		}{
			{"SAME-PATH", jCov, gSame, true},
			{"SAME-PATH", jCov, gUnfetched, false},
			{"SAME-PATH", jCov, gErr, false},
			{"SAME-CLASS", jCov, gOtherIndex, true},
			{"SAME-CLASS", jCov, gUnfetched, false},
			{"DIFF-PATH covering-rank -> INUNION(ISCAN(I1 [=]))", jCov, gUnfetched, true},
			{"DIFF-PATH covering-rank -> INUNION(ISCAN(I1 [=]))", jCov, gSame, false},
			{"DIFF-PATH covering-rank -> FETCH(INUNION(COVERING(I1 [=])))", jCov, gSame, false}, // Go on the target's path is not a difference
			{"DIFF-PATH covering-rank -> INUNION(ISCAN(I2 [=]))", jCov, gUnfetched, false},      // Go on a path the entry does not declare
			{
				"SAME-RL", `RL EXPLAIN "[10, 20 SORTED] | INJOIN q# -> { COVERING(p [EQUALS q#] -> [o: KEY:[1]]) } | FETCH" size=0 ids=[1 2]`,
				`RL EXPLAIN "Fetch(InJoin(IndexScan(p, [=] COVERING), binding ASC))" size=0 ids=[1 2]`, true,
			},
			{
				"SAME-RL", `RL EXPLAIN "ISCAN(p [EQUALS 1])" size=0 EXECUTE-ERROR RecordCoreException: too many IN values`,
				`RL EXPLAIN "IndexScan(p, [=])" size=0 EXECUTE-ERROR executor: too many IN values (size 3)`, true,
			},
			{"SAME-RL", `RL EXPLAIN "ISCAN(p [EQUALS 1])" size=0 ids=[1]`, `RL EXPLAIN "IndexScan(p, [=])" size=24 ids=[1]`, false},
			{"SAME", `OK [ID:BIGINT] [[3]] ; OK [ID:BIGINT] [[3]]`, `OK [ID:BIGINT] [[3]] cache=miss ; OK [ID:BIGINT] [[3]] cache=hit`, true},
		} {
			Expect(wsfAtAcceptance(c.entry, c.java, c.goLine)).To(Equal(c.at), "%s over %q and %q", c.entry, c.java, c.goLine)
		}
		// The ratchet's arms, over explicit tables: at and not listed, and away and
		// listed, are clean; at and listed, away and not listed, and a listed name that
		// is no probe are each one gap.
		acc := map[string]string{"at": "SAME", "away": "SAME", "at_listed": "SAME", "away_unlisted": "SAME"}
		java := map[string]string{"at": ok, "away": ok, "at_listed": ok, "away_unlisted": ok}
		goAns := map[string]string{"at": ok, "away": gErr, "at_listed": ok, "away_unlisted": gErr}
		Expect(wsfRatchetGaps(acc, map[string]string{"away": "F-1", "at_listed": "F-2", "ghost": "F-3"}, java, goAns)).To(Equal([]string{
			"at_listed is at its acceptance verdict SAME; remove it from wsfOpenUntil (F-2)",
			"away_unlisted is not at its acceptance verdict SAME, and no phase owns it",
			"wsfOpenUntil names ghost, which is not a probe",
		}))
		Expect(wsfRatchetGaps(acc, map[string]string{"away": "F-1", "away_unlisted": "F-2"}, java, goAns)).To(BeEmpty())

		// The ROOT-PAIRS reader: a whole trace, and each way a trace is refused rather than
		// read as agreement.
		canon := strings.NewReplacer("ZZKEYA", "I1", "ZZROWS", "T1")
		trace := []string{
			"ROOT memo order: 1 0 sequential winner: 0",
			"ROOT members: 2 preference=PREFER_INDEX",
			"ROOT-MEMBER 0 ISCAN(ZZKEYA <,>) | FILTER q#.COL1 NOT_EQUALS @c8 conjuncts=1 typeFilters=0 unmatchedFields=3 hash=7",
			"ROOT-MEMBER 1 SCAN([IS ZZROWS]) | FILTER q#.COL1 NOT_EQUALS @c8 conjuncts=1 typeFilters=0 unmatchedFields=1 hash=-",
			"ROOT-COMPARE 0 vs 1 = -1 hashOrder=1",
		}
		rt, err := wsfRootPairs(trace, canon)
		Expect(err).NotTo(HaveOccurred())
		Expect(rt.header).To(Equal("members=2 preference=PREFER_INDEX"))
		Expect(rt.members).To(Equal([]string{
			"ISCAN(I1 <,>) | FILTER q#.COL1 NOT_EQUALS @c8 {conjuncts=1 typeFilters=0 unmatchedFields=3}",
			"SCAN([IS T1]) | FILTER q#.COL1 NOT_EQUALS @c8 {conjuncts=1 typeFilters=0 unmatchedFields=1}",
		}))
		Expect(rt.order).To(Equal("memo order [SCAN([IS T1]) | FILTER q#.COL1 NOT_EQUALS @c8 ; ISCAN(I1 <,>) | FILTER q#.COL1 NOT_EQUALS @c8] winner ISCAN(I1 <,>) | FILTER q#.COL1 NOT_EQUALS @c8"))
		Expect(rt.winner).To(Equal("ISCAN(I1 <,>) | FILTER q#.COL1 NOT_EQUALS @c8"))
		// The pair is keyed by its names in name order, its verdicts turned with it.
		Expect(rt.pairs).To(Equal(map[string]string{
			"ISCAN(I1 <,>) | FILTER q#.COL1 NOT_EQUALS @c8 ~ SCAN([IS T1]) | FILTER q#.COL1 NOT_EQUALS @c8": "-1 hash1",
		}))
		swapped := append([]string{}, trace...)
		swapped[4] = "ROOT-COMPARE 1 vs 0 = 1 hashOrder=-1"
		rt2, err := wsfRootPairs(swapped, canon)
		Expect(err).NotTo(HaveOccurred())
		Expect(rt2.pairs).To(Equal(rt.pairs))
		for _, c := range []struct {
			name    string
			replace int
			line    string
			refusal string
		}{
			{"an unreadable line", 4, "ROOT-COMPARE 0 vs 1 = maybe", "unreadable ROOT-PAIRS line"},
			{"two members rendered alike", 3, "ROOT-MEMBER 1 ISCAN(ZZKEYA <,>) | FILTER q#.COL1 NOT_EQUALS @c8 conjuncts=0 typeFilters=0 unmatchedFields=0 hash=1", "two root members render as"},
			{"an order naming no member", 0, "ROOT memo order: 1 2 sequential winner: 0", "memo order names member 2"},
			{"a winner that is no member", 0, "ROOT memo order: 1 0 sequential winner: 5", "sequential winner 5 is not a listed member"},
			{"a member count the trace does not list", 1, "ROOT members: 3 preference=PREFER_INDEX", "ROOT-PAIRS trace incomplete"},
			{"an empty line", 0, "", "unreadable ROOT-PAIRS line"},
		} {
			bad := append([]string{}, trace...)
			bad[c.replace] = c.line
			_, err := wsfRootPairs(bad, canon)
			Expect(err).To(MatchError(ContainSubstring(c.refusal)), c.name)
		}
		_, err = wsfRootPairs(trace[1:], canon)
		Expect(err).To(MatchError(ContainSubstring("ROOT-PAIRS trace incomplete")), "a trace without its memo order")
		// A member and the EXPLAIN of one plan agree once quantifier names are gone, and
		// only then.
		Expect(wsfPlanShape("[IN @c7] | INJOIN q# -> { ISCAN(I1 [EQUALS q#]) } | FILTER q#.COL1 NOT_EQUALS @c8")).
			To(Equal(wsfPlanShape("[IN @c7] | INJOIN q0 -> { ISCAN(I1 [EQUALS q0]) } | FILTER _.COL1 NOT_EQUALS @c8")))
		Expect(wsfPlanShape("ISCAN(I1 [EQUALS q#])")).NotTo(Equal(wsfPlanShape("ISCAN(I2 [EQUALS q0])")))
		Expect(wsfPlanShape("FILTER _.COL1 NOT_EQUALS @c8")).To(Equal("FILTER Q.COL1 NOT_EQUALS @c8"))

		// Every pinned plan of either engine reduces to operators the reducer knows,
		// so a pinned difference is never an unparsed one.
		for _, side := range []struct {
			engine string
			pins   map[string]string
		}{{"java", wsfPins}, {"go", wsfGoPins}} {
			for name, line := range side.pins {
				if e, ok := wsfExplainOf(line); ok {
					Expect(wsfAccessPath(side.engine, e)).NotTo(ContainSubstring("?"), "%s's %s plan %q", name, side.engine, e)
				}
			}
		}
	})

	It("records null-safe scans, planner configuration, IN plans, enums, subscripts and display names", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "ws_f_"+uuid.New().String())
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(context.Background()) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFilePath := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFilePath)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFilePath)
		goPrepared := goRunner.(plandiff.PreparedSetupRunner)
		goDML := goRunner.(plandiff.PreparedDMLRunner)

		const t1Schema = "CREATE TABLE T1 (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id)) " +
			"CREATE INDEX I1 AS SELECT col1 FROM T1 ORDER BY col1 " +
			"CREATE INDEX I2 AS SELECT col2 FROM T1 ORDER BY col2"
		t1Setup := []string{"INSERT INTO T1 VALUES (1, 10, 1), (2, 10, NULL), (3, 20, 3), (4, NULL, NULL)"}
		const t5Schema = "CREATE TABLE T5 (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id)) " +
			"CREATE INDEX I5 AS SELECT col1 FROM T5 ORDER BY col1"
		t5Setup := []string{"INSERT INTO T5 VALUES (1, 20, 1), (2, 10, NULL), (3, 20, 3), (4, NULL, NULL), (5, 10, 5)"}
		const t6Schema = "CREATE TABLE T6 (id BIGINT, fk BIGINT, PRIMARY KEY (id)) " +
			"CREATE INDEX I6 AS SELECT fk FROM T6 ORDER BY fk"
		t6Setup := []string{"INSERT INTO T6 VALUES (1, 10), (2, 20), (3, 99)"}
		const t7Schema = "CREATE TABLE T7 (id BIGINT, v BIGINT, PRIMARY KEY (id))"
		t7Rows := make([]string, 30)
		for i := range t7Rows {
			t7Rows[i] = fmt.Sprintf("(%d, %d)", i, i*10)
		}
		t7Setup := []string{"INSERT INTO T7 VALUES " + strings.Join(t7Rows, ", ")}
		in25 := make([]string, 25)
		in25Values := make([]int, 25)
		for i := range in25 {
			in25[i] = fmt.Sprint(i * 10)
			in25Values[i] = i * 10
		}
		inList25 := strings.Join(in25, ", ")

		type probe struct{ name, sql string }
		type group struct {
			schema string
			setup  []string
			probes []probe
		}
		groups := []group{
			{
				schema: t1Schema, setup: t1Setup,
				probes: []probe{
					{"w9_not_distinct_null_explain", "EXPLAIN SELECT * FROM T1 WHERE col2 IS NOT DISTINCT FROM NULL"},
					{"w9_not_distinct_null_rows", "SELECT id FROM T1 WHERE col2 IS NOT DISTINCT FROM NULL ORDER BY id"},
					{"w9_not_distinct_literal_explain", "EXPLAIN SELECT * FROM T1 WHERE col1 IS NOT DISTINCT FROM 20"},
					{"w9_not_distinct_literal_rows", "SELECT id FROM T1 WHERE col1 IS NOT DISTINCT FROM 20 ORDER BY id"},
					{"w9_distinct_explain", "EXPLAIN SELECT * FROM T1 WHERE col1 IS DISTINCT FROM 10"},
					{"w9_distinct_rows", "SELECT id FROM T1 WHERE col1 IS DISTINCT FROM 10 ORDER BY id"},
					{"w9_not_distinct_range_explain", "EXPLAIN SELECT * FROM T1 WHERE col1 IS NOT DISTINCT FROM 10 AND col1 > 5"},
					{"w8_prefer_index_neq_explain", "EXPLAIN SELECT * FROM T1 WHERE col1 <> 10"},
					{"w8_covering_all_explain", "EXPLAIN SELECT col1 FROM T1"},
					{"w8_covering_neq_explain", "EXPLAIN SELECT col1 FROM T1 WHERE col1 <> 10"},
					{"w8_covering_id_neq_explain", "EXPLAIN SELECT id FROM T1 WHERE col1 <> 10"},
					{"w8_no_predicate_explain", "EXPLAIN SELECT * FROM T1"},
					{"w8_or_two_indexes_explain", "EXPLAIN SELECT * FROM T1 WHERE col1 = 10 OR col2 = 3"},
					{"w8_or_two_indexes_rows", "SELECT id FROM T1 WHERE col1 = 10 OR col2 = 3 ORDER BY id"},
					{"w8_in_union_explain", "EXPLAIN SELECT * FROM T1 WHERE col1 IN (10, 20) ORDER BY id"},
					{"w8_in_union_rows", "SELECT id FROM T1 WHERE col1 IN (10, 20) ORDER BY id"},
					{"w8_in_order_by_col1_explain", "EXPLAIN SELECT * FROM T1 WHERE col1 IN (10, 20) ORDER BY col1"},
					{"w8_in_no_order_explain", "EXPLAIN SELECT * FROM T1 WHERE col1 IN (10, 20)"},
					{"w8_in25_order_by_id_explain", "EXPLAIN SELECT * FROM T1 WHERE col1 IN (" + inList25 + ") ORDER BY id"},
					{"w8_in25_order_by_id_rows", "SELECT id FROM T1 WHERE col1 IN (" + inList25 + ") ORDER BY id"},
					{"w8_in25_order_by_col1_explain", "EXPLAIN SELECT * FROM T1 WHERE col1 IN (" + inList25 + ") ORDER BY col1"},
					{"w8_in25_order_by_col1_rows", "SELECT * FROM T1 WHERE col1 IN (" + inList25 + ") ORDER BY col1"},
					{"w8_in24_order_by_col1_rows", "SELECT * FROM T1 WHERE col1 IN (" + strings.Join(in25[:24], ", ") + ") ORDER BY col1"},
					{"w8_eq_order_by_id_explain", "EXPLAIN SELECT * FROM T1 WHERE col1 = 10 ORDER BY id"},
					{"w8_eq_order_by_id_rows", "SELECT id FROM T1 WHERE col1 = 10 ORDER BY id"},
					// An IN list beside an equality on another index: the target's
					// intersection ordering treats the record-type part as bound
					// (4.3 item 2's hypothesis), so it may order an intersection leg by id.
					{"w8_intersection_in_order_by_id_explain", "EXPLAIN SELECT * FROM T1 WHERE col1 IN (10, 20) AND col2 = 3 ORDER BY id"},
					{"w8_intersection_in_order_by_id_rows", "SELECT id FROM T1 WHERE col1 IN (10, 20) AND col2 = 3 ORDER BY id"},
				},
			},
			{
				schema: "CREATE TABLE T2 (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id)) " +
					"CREATE INDEX I3 AS SELECT col1, col2 FROM T2 ORDER BY col1, col2",
				setup: []string{"INSERT INTO T2 VALUES (1, 10, 1), (2, 10, NULL), (3, 20, 3), (4, NULL, NULL)"},
				probes: []probe{
					{"w9_multi_not_distinct_explain", "EXPLAIN SELECT * FROM T2 WHERE col1 = 10 AND col2 IS NOT DISTINCT FROM NULL"},
					{"w9_multi_not_distinct_rows", "SELECT id FROM T2 WHERE col1 = 10 AND col2 IS NOT DISTINCT FROM NULL ORDER BY id"},
					{"w9_multi_distinct_explain", "EXPLAIN SELECT * FROM T2 WHERE col1 = 10 AND col2 IS DISTINCT FROM 1"},
					{"w9_multi_distinct_rows", "SELECT id FROM T2 WHERE col1 = 10 AND col2 IS DISTINCT FROM 1 ORDER BY id"},
					// Two IN lists over one index: the in-union's size counts their
					// PRODUCT (RecordQueryInUnionPlan.getValuesSize), 25 over the limit
					// of 24, 24 at it.
					{"w8_in5x5_explain", "EXPLAIN SELECT * FROM T2 WHERE col1 IN (10, 20, 30, 40, 50) AND col2 IN (1, 2, 3, 4, 5) ORDER BY col1, col2"},
					{"w8_in5x5_rows", "SELECT id FROM T2 WHERE col1 IN (10, 20, 30, 40, 50) AND col2 IN (1, 2, 3, 4, 5) ORDER BY col1, col2"},
					{"w8_in4x6_explain", "EXPLAIN SELECT * FROM T2 WHERE col1 IN (10, 20, 30, 40) AND col2 IN (1, 2, 3, 4, 5, 6) ORDER BY col1, col2"},
					{"w8_in4x6_rows", "SELECT id FROM T2 WHERE col1 IN (10, 20, 30, 40) AND col2 IN (1, 2, 3, 4, 5, 6) ORDER BY col1, col2"},
				},
			},
			{
				schema: "CREATE TABLE T3 (id BIGINT, col2 BIGINT, PRIMARY KEY (id)) " +
					"CREATE INDEX I4 AS SELECT col2, id FROM T3 WHERE col2 IS NOT NULL ORDER BY col2, id",
				setup: []string{"INSERT INTO T3 VALUES (1, 1), (2, NULL), (3, 3), (4, NULL)"},
				probes: []probe{
					{"w9_sparse_not_distinct_null_explain", "EXPLAIN SELECT * FROM T3 WHERE col2 IS NOT DISTINCT FROM NULL"},
					{"w9_sparse_not_distinct_null_rows", "SELECT id FROM T3 WHERE col2 IS NOT DISTINCT FROM NULL ORDER BY id"},
					{"w9_sparse_not_distinct_value_explain", "EXPLAIN SELECT * FROM T3 WHERE col2 IS NOT DISTINCT FROM 3"},
					{"w9_sparse_not_distinct_value_rows", "SELECT id FROM T3 WHERE col2 IS NOT DISTINCT FROM 3 ORDER BY id"},
					// A literal through CAST: a NULL one implies nothing, a non-null one
					// implies the sparse predicate (section 5, "literal" defined).
					{"w9_sparse_not_distinct_cast_null_explain", "EXPLAIN SELECT * FROM T3 WHERE col2 IS NOT DISTINCT FROM CAST(NULL AS BIGINT)"},
					{"w9_sparse_not_distinct_cast_null_rows", "SELECT id FROM T3 WHERE col2 IS NOT DISTINCT FROM CAST(NULL AS BIGINT) ORDER BY id"},
					{"w9_sparse_not_distinct_cast_value_explain", "EXPLAIN SELECT * FROM T3 WHERE col2 IS NOT DISTINCT FROM CAST(3 AS BIGINT)"},
					{"w9_sparse_not_distinct_cast_value_rows", "SELECT id FROM T3 WHERE col2 IS NOT DISTINCT FROM CAST(3 AS BIGINT) ORDER BY id"},
				},
			},
			{
				// An index that names id in its own key: ORDER BY id reaches id through an
				// index key part, not past the record-type position (4.3 item 2).
				schema: "CREATE TABLE T8 (id BIGINT, col1 BIGINT, PRIMARY KEY (id)) " +
					"CREATE INDEX I8 AS SELECT col1, id FROM T8 ORDER BY col1, id",
				setup: []string{"INSERT INTO T8 VALUES (1, 20), (2, 10), (3, 20), (4, NULL), (5, 10)"},
				probes: []probe{
					{"w8_explicit_id_in_order_by_id_explain", "EXPLAIN SELECT * FROM T8 WHERE col1 IN (10, 20) ORDER BY id"},
					{"w8_explicit_id_in_order_by_id_rows", "SELECT id FROM T8 WHERE col1 IN (10, 20) ORDER BY id"},
				},
			},
			{
				// A quoted primary-key column without a dot: separates the quoting from the
				// dotted table name of the w13_display rows.
				schema: `CREATE TABLE "footab" ("id" BIGINT, "x" BIGINT, PRIMARY KEY ("id"))`,
				setup:  []string{`INSERT INTO "footab" VALUES (1, 2)`},
				probes: []probe{
					{"w13_quoted_pk_eq_explain", `EXPLAIN SELECT * FROM "footab" WHERE "id" = 1`},
					{"w13_quoted_pk_eq_rows", `SELECT * FROM "footab" WHERE "id" = 1`},
				},
			},
			{
				schema: "CREATE TYPE AS ENUM MOOD ('JOYFUL', 'HAPPY', 'SAD') " +
					"CREATE TABLE T (ID BIGINT, M MOOD, PRIMARY KEY(ID)) " +
					"CREATE INDEX T_M_IDX AS SELECT M, ID FROM T ORDER BY M, ID",
				setup: []string{"INSERT INTO T VALUES (1, 'JOYFUL'), (2, 'HAPPY'), (3, 'SAD'), (4, 'HAPPY'), (5, NULL)"},
				probes: []probe{
					{"w10_enum_distinct_rows", "SELECT ID FROM T WHERE M IS DISTINCT FROM 'HAPPY' ORDER BY ID"},
					{"w10_enum_distinct_explain", "EXPLAIN SELECT * FROM T WHERE M IS DISTINCT FROM 'HAPPY'"},
					{"w10_enum_not_distinct_rows", "SELECT ID FROM T WHERE M IS NOT DISTINCT FROM 'HAPPY' ORDER BY ID"},
					{"w10_enum_not_distinct_explain", "EXPLAIN SELECT * FROM T WHERE M IS NOT DISTINCT FROM 'HAPPY'"},
					{"w10_enum_eq_rows", "SELECT ID FROM T WHERE M = 'HAPPY' ORDER BY ID"},
					{"w10_enum_eq_explain", "EXPLAIN SELECT * FROM T WHERE M = 'HAPPY'"},
					{"w10_enum_neq_rows", "SELECT ID FROM T WHERE M <> 'HAPPY' ORDER BY ID"},
					{"w10_enum_or_rows", "SELECT ID FROM T WHERE ID < 2 OR M IS DISTINCT FROM 'HAPPY' ORDER BY ID"},
					{"w10_enum_not_distinct_null_rows", "SELECT ID FROM T WHERE M IS NOT DISTINCT FROM NULL ORDER BY ID"},
					{"w10_enum_unknown_literal", "SELECT ID FROM T WHERE M = 'ANGRY'"},
				},
			},
			{
				schema: "CREATE TABLE A (id BIGINT, arr BIGINT ARRAY, PRIMARY KEY (id))",
				setup:  []string{"INSERT INTO A VALUES (1, [7, 8]), (2, NULL)"},
				probes: []probe{
					{"w13_subscript_rows", "SELECT arr[1] FROM A WHERE id = 1"},
					{"w13_subscript_explain", "EXPLAIN SELECT arr[1] FROM A WHERE id = 1"},
					{"w13_subscript_out_of_range", "SELECT arr[3] FROM A WHERE id = 1"},
					{"w13_subscript_zero", "SELECT arr[0] FROM A WHERE id = 1"},
					{"w13_subscript_null_array", "SELECT arr[1] FROM A WHERE id = 2"},
					{"w13_subscript_null_index", "SELECT arr[NULL] FROM A WHERE id = 1"},
					{"w13_subscript_where", "SELECT id FROM A WHERE arr[2] = 8"},
					{"w13_subscript_sum", "SELECT arr[1] + arr[2] FROM A WHERE id = 1"},
					{"w13_subscript_alias", "SELECT arr[1] AS x FROM A WHERE id = 1"},
					{"w13_subscript_long_index", "SELECT arr[id] FROM A WHERE id = 1"},
					{"w13_subscript_long_index_empty", "SELECT arr[id] FROM A WHERE id = 99"},
					{"w13_subscript_string_index", "SELECT arr['a'] FROM A WHERE id = 1"},
					{"w13_subscript_non_array", "SELECT id[1] FROM A WHERE id = 1"},
				},
			},
			{
				// No setup: the SELECT and EXPLAIN probes then show how each engine
				// resolves the quoted dotted name itself, not how an INSERT fared.
				schema: `CREATE TABLE "foo.table$nested" ("id" BIGINT, "x$y" BIGINT, PRIMARY KEY ("id"))`,
				probes: []probe{
					{"w13_display_scan_explain", `EXPLAIN SELECT * FROM "foo.table$nested" WHERE "id" = 1`},
					{"w13_display_filter_explain", `EXPLAIN SELECT * FROM "foo.table$nested" WHERE "x$y" = 2`},
					{"w13_display_insert_explain", `EXPLAIN INSERT INTO "foo.table$nested" VALUES (5, 6)`},
					{"w13_display_select_empty", `SELECT * FROM "foo.table$nested"`},
				},
			},
			{
				schema: `CREATE TABLE "foo.table$nested" ("id" BIGINT, "x$y" BIGINT, PRIMARY KEY ("id"))`,
				setup:  []string{`INSERT INTO "foo.table$nested" VALUES (1, 2)`},
				probes: []probe{
					{"w13_display_rows", `SELECT * FROM "foo.table$nested"`},
				},
			},
			{
				// T5 breaks T1's tie: its IN-matching rows are ordered differently by
				// id and by col1, so a union merged by (COL1, ID) and one merged by ID
				// return different row orders.
				schema: t5Schema, setup: t5Setup,
				probes: []probe{
					{"w8_tie_in_order_by_col1_rows", "SELECT id, col1 FROM T5 WHERE col1 IN (20, 10) ORDER BY col1"},
					{"w8_tie_in_order_by_col1_explain", "EXPLAIN SELECT id, col1 FROM T5 WHERE col1 IN (20, 10) ORDER BY col1"},
					{"w8_tie_in_order_by_col1_desc_rows", "SELECT id, col1 FROM T5 WHERE col1 IN (20, 10) ORDER BY col1 DESC"},
					{"w8_tie_in_order_by_col1_desc_explain", "EXPLAIN SELECT id, col1 FROM T5 WHERE col1 IN (20, 10) ORDER BY col1 DESC"},
					{"w8_tie_in_order_by_id_rows", "SELECT id, col1 FROM T5 WHERE col1 IN (20, 10) ORDER BY id"},
					{"w8_tie_in_order_by_id_explain", "EXPLAIN SELECT id, col1 FROM T5 WHERE col1 IN (20, 10) ORDER BY id"},
					{"w8_tie_in_one_value_order_by_col1_rows", "SELECT id, col1 FROM T5 WHERE col1 IN (10) ORDER BY col1"},
				},
			},
			{
				// A LEFT JOIN whose ON column is indexed on the null-supplying side (the
				// target probes it through the index) and one whose ON predicate names only
				// the preserved side (RFC-152's materialized outer join in Go).
				schema: t1Schema + " " + t6Schema, setup: append(append([]string{}, t1Setup...), t6Setup...),
				probes: []probe{
					{"w6_left_join_indexed_explain", "EXPLAIN SELECT T1.id AS a, T6.id AS b FROM T1 LEFT JOIN T6 ON T6.fk = T1.col1"},
					{"w6_left_join_indexed_rows_unordered", "SELECT T1.id AS a, T6.id AS b FROM T1 LEFT JOIN T6 ON T6.fk = T1.col1"},
					{"w6_left_join_preserved_only_explain", "EXPLAIN SELECT T1.id AS a, T6.id AS b FROM T1 LEFT JOIN T6 ON T1.col2 = 1"},
					{"w6_left_join_preserved_only_rows_unordered", "SELECT T1.id AS a, T6.id AS b FROM T1 LEFT JOIN T6 ON T1.col2 = 1"},
				},
			},
			{
				// The IN-subquery read extension against the in-union rule: a projecting
				// subquery source, ordered by the IN column, over more than 24 values.
				schema: t1Schema + " " + t7Schema, setup: append(append([]string{}, t1Setup...), t7Setup...),
				probes: []probe{
					{"w8_in_subquery_order_by_col1_explain", "EXPLAIN SELECT * FROM T1 WHERE col1 IN (SELECT v FROM T7) ORDER BY col1"},
					{"w8_in_subquery_order_by_col1_rows", "SELECT * FROM T1 WHERE col1 IN (SELECT v FROM T7) ORDER BY col1"},
					{"w8_in_subquery_rows_unordered", "SELECT id FROM T1 WHERE col1 IN (SELECT v FROM T7)"},
				},
			},
		}

		javaGot, goGot := map[string]string{}, map[string]string{}
		record := func(name, sqlText, java, goLine string) {
			Expect(javaGot).NotTo(HaveKey(name), "probe names are unique")
			javaGot[name], goGot[name] = java, goLine
			same := "DIFF"
			if wsfComparable(java) == wsfComparable(goLine) {
				same = "SAME"
			}
			fmt.Fprintf(GinkgoWriter, "WSF %s %s\n  JAVA %s\n  GO   %s\n  SQL  %s\n", name, same, java, goLine, sqlText)
		}
		for _, g := range groups {
			for _, p := range g.probes {
				jr := javaRunner.RunWithSetup(ctx, g.schema, g.setup, p.sql)
				gr := goRunner.RunWithSetup(ctx, g.schema, g.setup, p.sql)
				if strings.HasSuffix(p.name, "_unordered") {
					wsfSortRows(&jr)
					wsfSortRows(&gr)
				}
				record(p.name, p.sql, wsfRender(p.sql, jr), wsfRender(p.sql, gr))
			}
		}

		// Prepared probes: the target binds through the JDBC setters
		// (runPreparedExtended), Go through database/sql.
		type param struct {
			kind, sqlType string
			value         any
		}
		goArg := func(p param) any {
			switch p.kind {
			case "long":
				return int64(p.value.(int))
			case "int":
				return int32(p.value.(int))
			case "null":
				return nil
			case "longArray":
				var out []int64
				for _, x := range p.value.([]int) {
					out = append(out, int64(x))
				}
				return out
			}
			return p.value
		}
		prepared := []struct {
			name, schema string
			setup        []string
			sql          string
			params       []param
		}{
			{
				"w13_subscript_int_param", "CREATE TABLE A (id BIGINT, arr BIGINT ARRAY, PRIMARY KEY (id))",
				[]string{"INSERT INTO A VALUES (1, [7, 8])"},
				"SELECT arr[?] FROM A WHERE id = 1",
				[]param{{"int", "", 2}},
			},
			{
				"w13_subscript_long_param", "CREATE TABLE A (id BIGINT, arr BIGINT ARRAY, PRIMARY KEY (id))",
				[]string{"INSERT INTO A VALUES (1, [7, 8])"},
				"SELECT arr[?] FROM A WHERE id = 1",
				[]param{{"long", "", 2}},
			},
			{
				"w9_not_distinct_null_param_rows", t1Schema, t1Setup,
				"SELECT id FROM T1 WHERE col2 IS NOT DISTINCT FROM ? ORDER BY id",
				[]param{{"null", "BIGINT", nil}},
			},
			{
				"w9_not_distinct_value_param_rows", t1Schema, t1Setup,
				"SELECT id FROM T1 WHERE col2 IS NOT DISTINCT FROM ? ORDER BY id",
				[]param{{"long", "", 3}},
			},
			{
				"w9_sparse_not_distinct_null_param_rows", "CREATE TABLE T3 (id BIGINT, col2 BIGINT, PRIMARY KEY (id)) " +
					"CREATE INDEX I4 AS SELECT col2, id FROM T3 WHERE col2 IS NOT NULL ORDER BY col2, id",
				[]string{"INSERT INTO T3 VALUES (1, 1), (2, NULL), (3, 3), (4, NULL)"},
				"SELECT id FROM T3 WHERE col2 IS NOT DISTINCT FROM ? ORDER BY id",
				[]param{{"null", "BIGINT", nil}},
			},
			{
				"w9_sparse_not_distinct_value_param_explain", "CREATE TABLE T3 (id BIGINT, col2 BIGINT, PRIMARY KEY (id)) " +
					"CREATE INDEX I4 AS SELECT col2, id FROM T3 WHERE col2 IS NOT NULL ORDER BY col2, id",
				[]string{"INSERT INTO T3 VALUES (1, 1), (2, NULL), (3, 3), (4, NULL)"},
				"EXPLAIN SELECT id FROM T3 WHERE col2 IS NOT DISTINCT FROM ?",
				[]param{{"long", "", 3}},
			},
			{
				"w8_in25_param_order_by_id_rows", t1Schema, t1Setup,
				"SELECT id FROM T1 WHERE col1 IN ? ORDER BY id",
				[]param{{"longArray", "", in25Values}},
			},
			{
				"w8_in25_param_order_by_col1_rows", t1Schema, t1Setup,
				"SELECT * FROM T1 WHERE col1 IN ? ORDER BY col1",
				[]param{{"longArray", "", in25Values}},
			},
		}
		for _, c := range prepared {
			javaParams := make([]map[string]any, len(c.params))
			goArgs := make([]any, len(c.params))
			for i, p := range c.params {
				javaParams[i] = map[string]any{"name": "", "kind": p.kind, "sqlType": p.sqlType, "value": p.value}
				goArgs[i] = goArg(p)
			}
			var jr plandiff.RunResult
			var rows plandiff.RowSet
			if jerr := srv.InvokeAs(ctx, "runPreparedExtended", map[string]any{
				"clusterFile": env.ClusterFile, "schemaTemplate": c.schema, "setupSqls": c.setup,
				"querySql": c.sql, "params": javaParams, "connectionOptions": map[string]any{},
				"update": false, "followUpSql": "",
			}, &rows); jerr != nil {
				var je *JavaError
				if errors.As(jerr, &je) {
					jr.Err = &plandiff.JavaError{Message: je.Message, ExceptionClass: je.ExceptionClass, SQLState: je.SQLState}
				} else {
					jr.Err = jerr
				}
			} else {
				jr.Rows = rows
			}
			gr := goPrepared.RunPreparedWithSetup(ctx, c.schema, c.setup, c.sql, goArgs)
			record(c.name, c.sql, wsfRender(c.sql, jr), wsfRender(c.sql, gr))
		}

		// DML through each engine's update path, read back by a follow-up SELECT.
		for _, c := range []struct{ name, schema, sql, followUp string }{
			{
				"w13_display_insert", `CREATE TABLE "foo.table$nested" ("id" BIGINT, "x$y" BIGINT, PRIMARY KEY ("id"))`,
				`INSERT INTO "foo.table$nested" VALUES (5, 6)`, `SELECT * FROM "foo.table$nested"`,
			},
		} {
			var jr plandiff.RunResult
			var rows plandiff.RowSet
			if jerr := srv.InvokeAs(ctx, "runPreparedExtended", map[string]any{
				"clusterFile": env.ClusterFile, "schemaTemplate": c.schema, "setupSqls": []string{},
				"querySql": c.sql, "params": []map[string]any{}, "connectionOptions": map[string]any{},
				"update": true, "followUpSql": c.followUp,
			}, &rows); jerr != nil {
				var je *JavaError
				if errors.As(jerr, &je) {
					jr.Err = &plandiff.JavaError{Message: je.Message, ExceptionClass: je.ExceptionClass, SQLState: je.SQLState}
				} else {
					jr.Err = jerr
				}
			} else {
				jr.Rows = rows
			}
			gr := goDML.RunPreparedDMLWithSetup(ctx, c.schema, nil, c.sql, nil, c.followUp)
			record(c.name, c.sql, wsfRender(c.sql, jr), wsfRender(c.sql, gr))
		}

		// The target's planner rule trace: which IN-list plans its rules ever
		// produced, not only the winner EXPLAIN shows. Go has no counterpart
		// instrument yet (the WS-F rule-call observer), so its line is empty.
		type ruleTally struct {
			Calls       int      `json:"calls"`
			Finals      int      `json:"finals"`
			Exploratory int      `json:"exploratory"`
			FinalKinds  []string `json:"finalKinds"`
		}
		traceRules := []string{"InComparisonToExplodeRule", "ImplementInJoinRule", "ImplementInUnionRule"}
		for _, c := range []probe{
			{"w8_trace_in_order_by_id", "SELECT * FROM T1 WHERE col1 IN (10, 20) ORDER BY id"},
			{"w8_trace_in_order_by_col1", "SELECT * FROM T1 WHERE col1 IN (10, 20) ORDER BY col1"},
			{"w8_trace_in_no_order", "SELECT * FROM T1 WHERE col1 IN (10, 20)"},
		} {
			var trace struct {
				Explain     string               `json:"explain"`
				Rules       map[string]ruleTally `json:"rules"`
				Comparisons []string             `json:"inUnionComparisons"`
			}
			line := ""
			if jerr := srv.InvokeAs(ctx, "planRuleTrace", map[string]any{
				"clusterFile": env.ClusterFile, "schemaTemplate": t1Schema, "setupSqls": t1Setup,
				"querySql": c.sql, "rules": traceRules,
			}, &trace); jerr != nil {
				line = "ERROR " + wsfMsg(jerr.Error())
			} else {
				parts := []string{fmt.Sprintf("EXPLAIN %q", trace.Explain)}
				for _, r := range traceRules {
					t := trace.Rules[r]
					parts = append(parts, fmt.Sprintf("%s calls=%d finals=%d exploratory=%d kinds=%v", r, t.Calls, t.Finals, t.Exploratory, t.FinalKinds))
				}
				// Where the IN-union lost: the target's own PlanningCostModel over the
				// in-union and every other final member of the reference it went into.
				// Quantifier aliases are random per planning (q<uuid>) and the members
				// come from a set, so aliases are masked and the lines sorted.
				var cmps []string
				for _, c := range trace.Comparisons {
					cmps = append(cmps, wsfAliasRE.ReplaceAllString(c, "q#"))
				}
				sort.Strings(cmps)
				parts = append(parts, "inUnion vs members: "+strings.Join(cmps, " | "))
				line = "TRACE " + strings.Join(parts, "; ")
			}
			record(c.name, c.sql, line, "NO-GO-INSTRUMENT")
		}

		// One statement text executed with several bindings on one connection: a plan
		// cached under the value-free text by the first binding must not be reused where
		// it is wrong for the next (the sparse index I4 answers NOT DISTINCT FROM 3 but
		// not NOT DISTINCT FROM NULL).
		goSeq := goRunner.(plandiff.PreparedSequenceRunner)
		const t3Schema = "CREATE TABLE T3 (id BIGINT, col2 BIGINT, PRIMARY KEY (id)) " +
			"CREATE INDEX I4 AS SELECT col2, id FROM T3 WHERE col2 IS NOT NULL ORDER BY col2, id"
		t3Setup := []string{"INSERT INTO T3 VALUES (1, 1), (2, NULL), (3, 3), (4, NULL)"}
		for _, c := range []struct {
			name  string
			sets  [][]param
			reuse bool
		}{
			{"w9_sparse_param_value_then_null_reused", [][]param{{{"long", "", 3}}, {{"null", "BIGINT", nil}}}, true},
			{"w9_sparse_param_value_then_null_reprepared", [][]param{{{"long", "", 3}}, {{"null", "BIGINT", nil}}}, false},
			{"w9_sparse_param_null_then_value_reused", [][]param{{{"null", "BIGINT", nil}}, {{"long", "", 3}}}, true},
			{"w9_sparse_param_null_then_value_reprepared", [][]param{{{"null", "BIGINT", nil}}, {{"long", "", 3}}}, false},
			// One binding twice: whether Go's plan cache keys a ? statement on its text
			// or on its substituted values.
			{"w9_sparse_param_value_then_value_reused", [][]param{{{"long", "", 3}}, {{"long", "", 3}}}, true},
		} {
			const q = "SELECT id FROM T3 WHERE col2 IS NOT DISTINCT FROM ? ORDER BY id"
			javaSets := make([][]map[string]any, len(c.sets))
			goSets := make([][]any, len(c.sets))
			for i, set := range c.sets {
				for _, p := range set {
					javaSets[i] = append(javaSets[i], map[string]any{"name": "", "kind": p.kind, "sqlType": p.sqlType, "value": p.value})
					goSets[i] = append(goSets[i], goArg(p))
				}
			}
			var out struct {
				Results []map[string]any `json:"results"`
			}
			var javaLines []string
			if jerr := srv.InvokeAs(ctx, "runPreparedSequence", map[string]any{
				"clusterFile": env.ClusterFile, "schemaTemplate": t3Schema, "setupSqls": t3Setup,
				"querySql": q, "paramSets": javaSets, "reuseStatement": c.reuse,
			}, &out); jerr != nil {
				javaLines = []string{"ERROR " + wsfMsg(jerr.Error())}
			} else {
				for _, r := range out.Results {
					javaLines = append(javaLines, wsfRenderSequenceElement(q, r))
				}
			}
			// The target's step plans every execution fresh: its engine is built with
			// no plan cache (sql_plan_steps.java, planCache null), so its rows measure
			// the plan each binding gets, not a cached plan's reuse. Go's line carries
			// each execution's plan-cache event, pinned, so whether a later binding
			// reused the first binding's plan is measured, not assumed.
			var goLines []string
			for _, r := range goSeq.RunPreparedSequenceWithSetup(ctx, t3Schema, t3Setup, q, goSets, c.reuse) {
				goLines = append(goLines, wsfRender(q, r)+" cache="+r.PlanCache)
			}
			record(c.name, q, strings.Join(javaLines, " ; "), strings.Join(goLines, " ; "))
		}

		// The in-union rule's view of its inner reference on the target: per call, the
		// ordering partitions it iterates, and per requested ordering the union ordering
		// it builds and the comparison keys it enumerates.
		iupRules := []string{"ImplementInUnionRule", "IN-UNION-PARTITIONS"}
		for _, c := range []struct{ name, schema, sql string }{
			{"w8_iup_order_by_id", t1Schema, "SELECT * FROM T1 WHERE col1 IN (10, 20) ORDER BY id"},
			{"w8_iup_order_by_col1", t1Schema, "SELECT * FROM T1 WHERE col1 IN (10, 20) ORDER BY col1"},
			{"w8_iup_no_order", t1Schema, "SELECT * FROM T1 WHERE col1 IN (10, 20)"},
			{"w8_iup_tie_order_by_col1", t5Schema, "SELECT id, col1 FROM T5 WHERE col1 IN (20, 10) ORDER BY col1"},
			{"w8_iup_intersection_order_by_id", t1Schema, "SELECT * FROM T1 WHERE col1 IN (10, 20) AND col2 = 3 ORDER BY id"},
		} {
			var trace struct {
				Explain    string   `json:"explain"`
				Partitions []string `json:"inUnionPartitions"`
			}
			line := ""
			if jerr := srv.InvokeAs(ctx, "planRuleTrace", map[string]any{
				"clusterFile": env.ClusterFile, "schemaTemplate": c.schema, "setupSqls": []string{},
				"querySql": c.sql, "rules": iupRules,
			}, &trace); jerr != nil {
				line = "ERROR " + wsfMsg(jerr.Error())
			} else {
				var parts []string
				for _, l := range trace.Partitions {
					parts = append(parts, wsfAliasRE.ReplaceAllString(l, "q#"))
				}
				line = fmt.Sprintf("IUP EXPLAIN %q; %s", trace.Explain, strings.Join(parts, " | "))
			}
			record(c.name, c.sql, line, "NO-GO-INSTRUMENT")
		}

		// The query graph the target's REWRITING hands to PLANNING (its physical prune's
		// survivors): for the two LEFT JOINs, whether the un-rewritten outer join or the
		// rewritten null-on-empty form survives, which outerJoinCount decides.
		for _, c := range []struct{ name, sql string }{
			{"w6_rewriting_left_join_indexed", "SELECT T1.id AS a, T6.id AS b FROM T1 LEFT JOIN T6 ON T6.fk = T1.col1"},
			{"w6_rewriting_left_join_preserved_only", "SELECT T1.id AS a, T6.id AS b FROM T1 LEFT JOIN T6 ON T1.col2 = 1"},
			{"w6_rewriting_inner_join_control", "SELECT T1.id AS a, T6.id AS b FROM T1, T6 WHERE T6.fk = T1.col1"},
		} {
			var trace struct {
				Explain     string   `json:"explain"`
				Comparisons []string `json:"inUnionComparisons"`
			}
			line := ""
			if jerr := srv.InvokeAs(ctx, "planRuleTrace", map[string]any{
				"clusterFile": env.ClusterFile, "schemaTemplate": t1Schema + " " + t6Schema, "setupSqls": []string{},
				"querySql": c.sql, "rules": []string{"REWRITING-RESULT"},
			}, &trace); jerr != nil {
				line = "ERROR " + wsfMsg(jerr.Error())
			} else {
				line = wsfAliasRE.ReplaceAllString(fmt.Sprintf("REWRITING EXPLAIN %q; %s", trace.Explain, strings.Join(trace.Comparisons, " | ")), "q#")
			}
			record(c.name, c.sql, line, "NO-GO-INSTRUMENT")
		}

		// The target's PlanningCostModel over every pair of root members, under the
		// relational configuration, for each W8 plan class, with the table and index names
		// varied: a pair whose verdict moves with the names is decided by the plan hash,
		// not by a criterion.
		type rootVariant struct{ table, i1, i2 string }
		variants := []rootVariant{{"T1", "I1", "I2"}, {"WSFQTAB", "WSFQIDX1", "WSFQIDX2"}, {"ZZROWS", "ZZKEYA", "ZZKEYB"}}
		for _, c := range []struct{ name, sql string }{
			{"w8_root_no_predicate", "SELECT * FROM {T}"},
			{"w8_root_covering_all", "SELECT col1 FROM {T}"},
			{"w8_root_covering_neq", "SELECT col1 FROM {T} WHERE col1 <> 10"},
			{"w8_root_covering_id_neq", "SELECT id FROM {T} WHERE col1 <> 10"},
			{"w8_root_neq", "SELECT * FROM {T} WHERE col1 <> 10"},
			{"w8_root_or", "SELECT * FROM {T} WHERE col1 = 10 OR col2 = 3"},
			{"w8_root_eq_order_by_id", "SELECT * FROM {T} WHERE col1 = 10 ORDER BY id"},
			// The two in-join rows whose cause v3 left unmeasured: the fetch placement
			// with no ORDER BY, and the DESC order the target reaches with a sorted
			// in-join where Go sorts in memory.
			{"w8_root_in_no_order", "SELECT * FROM {T} WHERE col1 IN (10, 20)"},
			{"w8_root_in_order_by_col1_desc", "SELECT id, col1 FROM {T} WHERE col1 IN (20, 10) ORDER BY col1 DESC"},
		} {
			verdicts := map[string][]string{}
			for vi, v := range variants {
				schema := strings.NewReplacer("T1", v.table, "I1", v.i1, "I2", v.i2).Replace(
					"CREATE TABLE T1 (id BIGINT, col1 BIGINT, col2 BIGINT, PRIMARY KEY (id)) " +
						"CREATE INDEX I1 AS SELECT col1 FROM T1 ORDER BY col1 " +
						"CREATE INDEX I2 AS SELECT col2 FROM T1 ORDER BY col2",
				)
				sqlText := strings.ReplaceAll(c.sql, "{T}", v.table)
				canon := strings.NewReplacer(v.i1, "I1", v.i2, "I2", v.table, "T1")
				var trace struct {
					Explain     string   `json:"explain"`
					Comparisons []string `json:"inUnionComparisons"`
				}
				line := ""
				if jerr := srv.InvokeAs(ctx, "planRuleTrace", map[string]any{
					"clusterFile": env.ClusterFile, "schemaTemplate": schema, "setupSqls": []string{},
					"querySql": sqlText, "rules": []string{"ROOT-PAIRS"},
				}, &trace); jerr != nil {
					line = "ERROR " + wsfMsg(jerr.Error())
				} else {
					rt, perr := wsfRootPairs(trace.Comparisons, canon)
					Expect(perr).NotTo(HaveOccurred(), "the ROOT-PAIRS trace of %s", sqlText)
					// The target's final plan is the member its sequential pass keeps
					// (ws-f-design.md 4.2 item 3), so the design reads each class's final
					// plan from the memo order only while the two agree.
					Expect(wsfPlanShape(rt.winner)).To(Equal(wsfPlanShape(canon.Replace(trace.Explain))),
						"the sequential winner of %s is the plan the target explains", sqlText)
					for k, verdict := range rt.pairs {
						verdicts[k] = append(verdicts[k], verdict)
					}
					line = fmt.Sprintf("ROOT EXPLAIN %q; %s; members %s; %s; pairs %s",
						canon.Replace(trace.Explain), rt.header, strings.Join(rt.members, " | "), rt.order, wsfJoinPairs(rt.pairs))
				}
				record(fmt.Sprintf("%s_v%d", c.name, vi), sqlText, line, "NO-GO-INSTRUMENT")
			}
			// Across the variants: which pairs keep one verdict and which move with the
			// names (a pair missing from a variant is reported as such).
			var summary []string
			for _, k := range sortedSliceKeys(verdicts) {
				vs := verdicts[k]
				state := "STABLE"
				for _, v := range vs {
					if strings.Fields(v)[0] != strings.Fields(vs[0])[0] {
						state = "MOVES"
					}
				}
				if len(vs) != len(variants) {
					state = fmt.Sprintf("IN %d OF %d VARIANTS", len(vs), len(variants))
				}
				summary = append(summary, fmt.Sprintf("%s: %s %v", k, state, vs))
			}
			record(c.name+"_summary", c.sql, "ROOT-SUMMARY "+strings.Join(summary, " | "), "NO-GO-INSTRUMENT")
		}

		// The target's RECORD layer (a RecordQuery through its CascadesPlanner, not SQL)
		// planning and executing an IN predicate under its own default configuration
		// (PREFER_SCAN, in-union size 0) and under the relational one. Go has no RecordQuery
		// planner; its record-layer callers build the query graph and plan it with the
		// exported Cascades planner, which wsfGoRecordLayerIn does for the same query.
		for _, c := range []struct {
			name       string
			prices     []int
			sort       string
			relational bool
		}{
			{"w8_rl_default_in2_order_by_price", []int{10, 20}, "price", false},
			{"w8_rl_default_in1_order_by_price", []int{10}, "price", false},
			{"w8_rl_default_in2_order_by_pk", []int{10, 20}, "order_id", false},
			{"w8_rl_default_in2_no_sort", []int{10, 20}, "", false},
			{"w8_rl_relational_in2_order_by_price", []int{10, 20}, "price", true},
		} {
			keyspace := []byte(fmt.Sprintf("wsf_rl_%s_%s", c.name, uuid.NewString()))
			var out struct {
				Explain      string  `json:"explain"`
				InUnionMax   int     `json:"inUnionMaxSize"`
				IDs          []int64 `json:"ids"`
				PlanError    string  `json:"planError"`
				ExecuteError string  `json:"executeError"`
			}
			line := ""
			if jerr := srv.InvokeAs(ctx, "wsfRecordLayerIn", map[string]any{
				"clusterFile": env.ClusterFile, "subspace": BytesToIntArray(keyspace), "tenantName": env.TenantName,
				"prices": c.prices, "sort": c.sort, "relationalConfiguration": c.relational,
			}, &out); jerr != nil {
				line = "ERROR " + wsfMsg(jerr.Error())
			} else if out.PlanError != "" {
				line = "PLAN-ERROR " + wsfMsg(out.PlanError)
			} else {
				line = fmt.Sprintf("RL EXPLAIN %q size=%d", wsfAliasRE.ReplaceAllString(out.Explain, "q#"), out.InUnionMax)
				if out.ExecuteError != "" {
					line += " EXECUTE-ERROR " + wsfMsg(out.ExecuteError)
				} else {
					line += fmt.Sprintf(" ids=%v", out.IDs)
				}
			}
			record(c.name, fmt.Sprintf("RecordQuery price IN %v sort %q relational=%v", c.prices, c.sort, c.relational),
				line, wsfGoRecordLayerIn(ctx, c.prices, c.sort, c.relational))
		}

		for _, name := range sortedStringKeys(javaGot) {
			fmt.Fprintf(GinkgoWriter, "WSF-JAVA %q: %q,\n", name, javaGot[name])
		}
		for _, name := range sortedStringKeys(goGot) {
			fmt.Fprintf(GinkgoWriter, "WSF-GO %q: %q,\n", name, goGot[name])
		}
		Expect(sortedStringKeys(javaGot)).To(Equal(sortedStringKeys(wsfPins)), "every probe has a target pin and every target pin is probed")
		Expect(sortedStringKeys(goGot)).To(Equal(sortedStringKeys(wsfGoPins)), "every probe has a Go pin and every Go pin is probed")
		var moved []string
		for name, v := range javaGot {
			if v != wsfPins[name] {
				moved = append(moved, fmt.Sprintf("target answer for %s moved: %s", name, v))
			}
			if goGot[name] != wsfGoPins[name] {
				moved = append(moved, fmt.Sprintf("Go answer for %s moved: %s", name, goGot[name]))
			}
		}
		sort.Strings(moved)
		Expect(strings.Join(moved, "\n")).To(BeEmpty())

		// The design's acceptance (ws-f-design.md section 13) as code: every probe's
		// verdict at acceptance, and, while a probe is not there yet, the phase that
		// owns moving it. A probe at its acceptance verdict must not be listed as open
		// (the ratchet), and one away from it must be.
		Expect(sortedStringKeys(wsfAcceptance)).To(Equal(sortedStringKeys(wsfPins)), "every probe has an acceptance verdict")
		for _, name := range sortedStringKeys(wsfAcceptance) {
			entry := wsfAcceptance[name]
			want := strings.Fields(entry)[0]
			Expect(want).To(BeElementOf("SAME", "SAME-STATE", "DIFF", "SAME-PATH", "SAME-CLASS", "DIFF-PATH", "SAME-RL"), "acceptance verdict of %s", name)
			if want != "SAME" && want != "SAME-PATH" && want != "SAME-CLASS" && want != "SAME-RL" {
				Expect(len(strings.Fields(entry))).To(BeNumerically(">", 1), "a declared difference names its reason: %s", name)
			}
			if want == "DIFF-PATH" {
				Expect(wsfDeclaredPathReasons).To(HaveKey(strings.Fields(entry)[1]), "a DIFF-PATH names a declared access-path divergence: %s", name)
				Expect(entry).To(ContainSubstring(" -> "), "a DIFF-PATH names Go's path at acceptance: %s", name)
			}
			// An EXPLAIN row both engines answer with a plan is accepted by access
			// path, never by "DIFF": explain syntax alone may not be a reason.
			_, jExplain := wsfExplainOf(wsfPins[name])
			_, gExplain := wsfExplainOf(wsfGoPins[name])
			if jExplain && gExplain {
				Expect(want).To(BeElementOf("SAME-PATH", "SAME-CLASS", "DIFF-PATH"), "an EXPLAIN row is accepted by access path: %s", name)
			}
		}
		Expect(strings.Join(wsfRatchetGaps(wsfAcceptance, wsfOpenUntil, javaGot, goGot), "\n")).To(BeEmpty())
	})
})

// wsfRatchetGaps is the acceptance ratchet over explicit state: a probe at its
// acceptance entry must not be listed as open, one away from it must be, and every
// open entry must name a probe.
func wsfRatchetGaps(acceptance, openUntil, javaGot, goGot map[string]string) []string {
	var gaps []string
	for _, name := range sortedStringKeys(acceptance) {
		entry := acceptance[name]
		at := wsfAtAcceptance(entry, javaGot[name], goGot[name])
		owner, open := openUntil[name]
		switch {
		case at && open:
			gaps = append(gaps, fmt.Sprintf("%s is at its acceptance verdict %s; remove it from wsfOpenUntil (%s)", name, strings.Fields(entry)[0], owner))
		case !at && !open:
			gaps = append(gaps, fmt.Sprintf("%s is not at its acceptance verdict %s, and no phase owns it", name, entry))
		}
	}
	for _, name := range sortedStringKeys(openUntil) {
		if _, ok := acceptance[name]; !ok {
			gaps = append(gaps, fmt.Sprintf("wsfOpenUntil names %s, which is not a probe", name))
		}
	}
	return gaps
}

// wsfRender renders one engine's answer. A Go failure in the SETUP statements is
// rendered as such, with its phase, so a probe whose schema or data could not be
// built is never read as that probe's own answer.
func wsfRender(sqlText string, r plandiff.RunResult) string {
	if r.Err != nil {
		var fe *plandiff.FixtureError
		if errors.As(r.Err, &fe) {
			return "SETUP-ERROR " + fe.Phase + ": " + wsfErr(fe.Err)
		}
		return wsfErr(r.Err)
	}
	if strings.HasPrefix(sqlText, "EXPLAIN") && len(r.Rows.Rows) > 0 && len(r.Rows.Rows[0]) > 0 {
		return fmt.Sprintf("OK EXPLAIN %q", fmt.Sprint(r.Rows.Rows[0][0]))
	}
	var cols []string
	for _, c := range r.Rows.Columns {
		cols = append(cols, c.Name+":"+c.Type)
	}
	return fmt.Sprintf("OK %v %s", cols, wsjRows(r.Rows.Rows))
}

func wsfErr(err error) string {
	var je *plandiff.JavaError
	if errors.As(err, &je) {
		return fmt.Sprintf("ERROR %s %q [%s]", je.SQLState, wsfMsg(je.Message), je.ExceptionClass)
	}
	var ge *api.Error
	if errors.As(err, &ge) {
		return fmt.Sprintf("ERROR %s %q", string(ge.Code), wsfMsg(ge.Message))
	}
	return fmt.Sprintf("ERROR %q", wsfMsg(err.Error()))
}

// wsfAliasRE matches a planner-generated quantifier alias (q + a UUID with
// underscores).
var wsfAliasRE = regexp.MustCompile(`q[0-9a-f]{8}_[0-9a-f]{4}_[0-9a-f]{4}_[0-9a-f]{4}_[0-9a-f]{12}`)

// wsfMsg collapses whitespace and bounds a message at 400 runes, cutting on a rune
// boundary.
func wsfMsg(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= 400 {
		return s
	}
	r := []rune(s)
	return string(r[:400]) + "…"
}

// wsfComparable drops the target's exception class, the one part of a rendered
// answer Go has no counterpart for, so the SAME/DIFF marker compares SQLSTATE,
// message, columns and rows.
func wsfComparable(line string) string {
	// Go's sequence elements carry their plan-cache event, which the target (no plan
	// cache) cannot; the event is pinned, and not part of the answer compared.
	line = wsfCacheMarkerRE.ReplaceAllString(line, "")
	if i := strings.LastIndex(line, " ["); i >= 0 && strings.HasPrefix(line, "ERROR ") && strings.HasSuffix(line, "]") {
		return line[:i]
	}
	return line
}

var wsfCacheMarkerRE = regexp.MustCompile(` cache=(hit|miss|skip|inconclusive)?`)

// wsfVerdict is the verdict of one probe measured the way its acceptance verdict wants
// it measured: SAME-STATE compares an error's SQLSTATE (or a non-error answer whole),
// SAME and DIFF compare through wsfComparable. It returns want when the answers agree
// under that measure, and the opposite verdict otherwise. Access-path verdicts are
// measured by wsfAtAcceptance, which needs the whole acceptance entry.
func wsfVerdict(want, java, goLine string) string {
	agree := wsfComparable(java) == wsfComparable(goLine)
	if want == "SAME-STATE" {
		agree = wsfState(java) == wsfState(goLine)
	}
	switch {
	case want == "DIFF" && !agree, want != "DIFF" && agree:
		return want
	case want == "DIFF":
		return "SAME"
	}
	return "DIFF"
}

// wsfDeclaredPathReasons are the declared access-path divergences a DIFF-PATH entry
// may name (ws-f-design.md section 13), each with the section that decides it.
var wsfDeclaredPathReasons = map[string]string{
	"record-type-horizon":            "4.3 item 1: Go's match ordering past a relational table's record-type position (read extension)",
	"rfc152-materialized-outer-join": "2.3: the preserved RFC-152 materialized outer join",
	"sparse-param-plan-constraint":   "5, item 3: a real (not substituted) ? parameter implies no sparse index until WS-H ports the plan-constraint identity; WS-E section 4 moves the prepared row here",
	"covering-rank":                  "4.2 item 3: Go's primary-versus-index rank against the target's cyclic relation, final plan",
	"rfc-191":                        "DIVERGENCES.md, ordered IN over a non-covering index: Go pushes a comparand in-join through its fetch, Fetch(InJoin) where Java plans Fetch(InUnion)",
	"in-memory-sort":                 "DIVERGENCES.md, an IN ordered by a key no probe provides: Go's in-memory sort lets the IN probes compete with the ordered full scan, Java's only plan",
	"single-element-in":              "DIVERGENCES.md, a one-value IN is an equality: Go plans `x IN (v)` as `x = v`, Java as a one-value IN-join over the same probe",
}

// wsfAtAcceptance reports whether a probe is at its acceptance entry. The entry's first
// field is the verdict: SAME, SAME-STATE and DIFF as wsfVerdict measures them;
// SAME-RL, a record-layer row whose access path, in-union size and ids (or "too many IN
// values" failure) agree (wsfRLComparable); SAME-PATH, both engines explain one access
// path (wsfAccessPath); SAME-CLASS, one
// access path up to index names (a choice only a plan hash decides); DIFF-PATH
// REASON -> PATH, Go explains PATH, the target another path, and REASON is one of
// wsfDeclaredPathReasons.
func wsfAtAcceptance(entry, java, goLine string) bool {
	want := strings.Fields(entry)[0]
	switch want {
	case "SAME", "SAME-STATE", "DIFF":
		return wsfVerdict(want, java, goLine) == want
	case "SAME-RL":
		return strings.HasPrefix(java, "RL EXPLAIN ") && wsfRLComparable("java", java) == wsfRLComparable("go", goLine)
	case "DIFF-PATH":
		// A record-layer row: the declared Go path, a different target path,
		// and the same size and ids.
		if jp, jt, ok := wsfRLParts("java", java); ok {
			gp, gt, gok := wsfRLParts("go", goLine)
			i := strings.Index(entry, " -> ")
			return gok && i >= 0 && gp == strings.TrimSpace(entry[i+len(" -> "):]) && jp != gp && jt == gt
		}
	}
	je, jok := wsfExplainOf(java)
	ge, gok := wsfExplainOf(goLine)
	if !jok || !gok {
		return false
	}
	jp, gp := wsfAccessPath("java", je), wsfAccessPath("go", ge)
	switch want {
	case "SAME-PATH":
		return jp == gp
	case "SAME-CLASS":
		return wsfPathClass(jp) == wsfPathClass(gp)
	case "DIFF-PATH":
		i := strings.Index(entry, " -> ")
		return i >= 0 && gp == strings.TrimSpace(entry[i+len(" -> "):]) && jp != gp
	}
	return false
}

// wsfState is an error answer's SQLSTATE ("ERROR 0AF00"), or a non-error answer whole.
func wsfState(line string) string {
	if f := strings.Fields(line); len(f) > 1 && f[0] == "ERROR" {
		return f[0] + " " + f[1]
	}
	return wsfComparable(line)
}

// wsfSortRows orders a result's rows by their rendering, for probes whose SQL leaves
// the order unspecified, so both engines' answers compare as sets.
func wsfSortRows(r *plandiff.RunResult) {
	if r.Err != nil {
		return
	}
	sort.SliceStable(r.Rows.Rows, func(i, j int) bool {
		return fmt.Sprint(r.Rows.Rows[i]) < fmt.Sprint(r.Rows.Rows[j])
	})
}

// wsfRenderSequenceElement renders one execution of the target's runPreparedSequence:
// a result set, or the error it recorded, in wsfRender's form.
func wsfRenderSequenceElement(sqlText string, r map[string]any) string {
	if msg, isErr := r["message"].(string); isErr {
		state, _ := r["sqlState"].(string)
		class, _ := r["exceptionClass"].(string)
		return wsfErr(&plandiff.JavaError{Message: msg, SQLState: state, ExceptionClass: class})
	}
	var rs plandiff.RowSet
	for _, c := range r["columns"].([]any) {
		col := c.(map[string]any)
		rs.Columns = append(rs.Columns, plandiff.Column{Name: col["name"].(string), Type: col["type"].(string)})
	}
	for _, row := range r["rows"].([]any) {
		rs.Rows = append(rs.Rows, row.([]any))
	}
	return wsfRender(sqlText, plandiff.RunResult{Rows: rs})
}

// wsfRootPairs reads the ROOT-PAIRS lines of a target rule trace: the header (the member
// count and the index-scan preference in force), the members, named through canon with
// their residual, type-filter and unmatched-field counts, the memo order OptimizeGroup
// iterates with the winner its sequential pass picks, and the pairwise verdicts keyed by
// the two names in name order. A line it cannot read, or two members that render the
// same, is an error: a silently dropped line or a collapsed pair would read as agreement.
func wsfRootPairs(lines []string, canon *strings.Replacer) (wsfRootTrace, error) {
	var (
		header, order string
		members       []string
	)
	headerRE := regexp.MustCompile(`^ROOT members: (\d+) preference=(\S+)$`)
	memberRE := regexp.MustCompile(`^ROOT-MEMBER (\d+) (.*) (conjuncts=\d+ typeFilters=\d+ unmatchedFields=\d+) hash=(-?\d+|-)$`)
	orderRE := regexp.MustCompile(`^ROOT memo order: ([\d ]*) sequential winner: (\d+|-)$`)
	pairRE := regexp.MustCompile(`^ROOT-COMPARE (\d+) vs (\d+) = (-?\d) hashOrder=(-?\d|-)$`)
	names := map[string]string{}
	seen := map[string]bool{}
	pairs := map[string]string{}
	count := -1
	var orderIdx []string
	winner, winnerName := "", ""
	for _, l := range lines {
		if m := headerRE.FindStringSubmatch(l); m != nil {
			header = "members=" + m[1] + " preference=" + m[2]
			fmt.Sscan(m[1], &count)
			continue
		}
		if m := memberRE.FindStringSubmatch(l); m != nil {
			n := canon.Replace(wsfAliasRE.ReplaceAllString(m[2], "q#"))
			if seen[n] {
				return wsfRootTrace{}, fmt.Errorf("two root members render as %q", n)
			}
			seen[n] = true
			names[m[1]] = n
			members = append(members, n+" {"+m[3]+"}")
			continue
		}
		if m := orderRE.FindStringSubmatch(l); m != nil {
			// The target prints the order before the members, so it is resolved
			// to names once every member has been read.
			orderIdx, winner = strings.Fields(m[1]), m[2]
			continue
		}
		if m := pairRE.FindStringSubmatch(l); m != nil {
			a, b, v, h := names[m[1]], names[m[2]], m[3], m[4]
			if b < a {
				a, b = b, a
				v, h = wsfNegate(v), wsfNegate(h)
			}
			pairs[a+" ~ "+b] = v + " hash" + h
			continue
		}
		return wsfRootTrace{}, fmt.Errorf("unreadable ROOT-PAIRS line %q", l)
	}
	if orderIdx != nil || winner != "" {
		var named []string
		for _, i := range orderIdx {
			n, ok := names[i]
			if !ok {
				return wsfRootTrace{}, fmt.Errorf("memo order names member %s, which the trace does not list", i)
			}
			named = append(named, n)
		}
		w, ok := names[winner]
		if !ok {
			return wsfRootTrace{}, fmt.Errorf("sequential winner %s is not a listed member", winner)
		}
		order = "memo order [" + strings.Join(named, " ; ") + "] winner " + w
		winnerName = w
	}
	if header == "" || order == "" || count != len(members) {
		return wsfRootTrace{}, fmt.Errorf("ROOT-PAIRS trace incomplete: header %q, order %q, %d members of %d", header, order, len(members), count)
	}
	sort.Strings(members)
	return wsfRootTrace{header: header, members: members, order: order, winner: winnerName, pairs: pairs}, nil
}

// wsfRootTrace is one ROOT-PAIRS trace as wsfRootPairs reads it; winner is the member
// the target's sequential OptimizeGroup pass keeps, by name.
type wsfRootTrace struct {
	header, order, winner string
	members               []string
	pairs                 map[string]string
}

// wsfPlanShape renders a target plan with its quantifier names erased: the trace names a
// member's quantifiers q# and the EXPLAIN names the same ones q0, q1 and _, so the two
// renderings of one plan compare equal only once the names are gone.
func wsfPlanShape(plan string) string {
	return wsfQuantifierRE.ReplaceAllString(plan, "Q")
}

var wsfQuantifierRE = regexp.MustCompile(`\bq(#|\d+)|\b_\b`)

func wsfNegate(v string) string {
	switch v {
	case "1":
		return "-1"
	case "-1":
		return "1"
	}
	return v
}

func wsfJoinPairs(pairs map[string]string) string {
	var out []string
	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+" = "+pairs[k])
	}
	return strings.Join(out, " | ")
}

func sortedSliceKeys(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

//go:embed testdata/probes/ws_f_acceptance.json
var wsfAcceptanceJSON []byte

//go:embed testdata/probes/ws_f_java_pins.json
var wsfPinsJSON []byte

//go:embed testdata/probes/ws_f_go_pins.json
var wsfGoPinsJSON []byte

func mustLoadWSFPinMap(name string, raw []byte) map[string]string {
	var out map[string]string
	if err := json.Unmarshal(raw, &out); err != nil {
		panic(fmt.Sprintf("decode %s: %v", name, err))
	}
	return out
}

// wsfAcceptance is every probe's verdict at WS-F acceptance (ws-f-design.md section
// 13): SAME (the comparator of section 0.1), SAME-STATE (the SQLSTATE agrees and the
// message difference is the declared reason), or DIFF followed by the declared reason.
var wsfAcceptance = mustLoadWSFPinMap("ws_f_acceptance.json", wsfAcceptanceJSON)

// wsfOpenUntil names, for each probe not yet at its acceptance verdict, the phase or
// dependency that moves it there (ws-f-design.md section 12).
var wsfOpenUntil = map[string]string{}

// wsfPins is the measured target answer of every WS-F probe (4.14.2.0).
var wsfPins = mustLoadWSFPinMap("ws_f_java_pins.json", wsfPinsJSON)

// wsfGoPins is Go's measured answer of every WS-F probe at this tree.
var wsfGoPins = mustLoadWSFPinMap("ws_f_go_pins.json", wsfGoPinsJSON)
