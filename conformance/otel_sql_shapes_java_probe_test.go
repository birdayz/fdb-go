//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"
	"os"
	"strings"

	"fdb.dev/pkg/relational/conformance/plandiff"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const otelShapesSchema = "CREATE TYPE AS STRUCT attr (scope STRING, k STRING, sv STRING, iv BIGINT) " +
	"CREATE TABLE spans (trace_id STRING, span_id STRING, start_ns BIGINT, name STRING, attrs attr ARRAY, PRIMARY KEY (trace_id, span_id)) " +
	"CREATE INDEX spans_attr_str AS SELECT a.scope, a.k, a.sv, s.start_ns, s.trace_id, s.span_id FROM spans AS s, (SELECT scope, k, sv FROM s.attrs) AS a ORDER BY a.scope, a.k, a.sv, s.start_ns, s.trace_id, s.span_id " +
	"CREATE INDEX spans_time AS SELECT start_ns, trace_id, span_id FROM spans ORDER BY start_ns, trace_id, span_id " +
	"CREATE TABLE logs (event_id STRING, trace_id STRING, span_id STRING, time_ns BIGINT, body STRING, PRIMARY KEY (event_id)) " +
	"CREATE INDEX logs_trace AS SELECT trace_id, span_id, time_ns FROM logs ORDER BY trace_id, span_id, time_ns"

var otelShapesSetup = []string{
	"INSERT INTO spans VALUES " +
		"('t1', 's1', 100, 'a', [('span', 'model', 'gpt', NULL), ('span', 'tag', 'x', NULL), ('span', 'tag', 'y', NULL)]), " +
		"('t1', 's2', 200, 'b', [('span', 'model', 'gpt', NULL), ('resource', 'model', 'gpt', NULL)]), " +
		"('t2', 's3', 300, 'c', [('span', 'model', 'claude', NULL), ('span', 'turn', NULL, 3)]), " +
		"('t3', 's4', 400, 'd', [('span', 'model', 'gpt', NULL), ('span', 'tag', 'x', NULL)]), " +
		"('t3', 's5', 500, 'e', [])",
	"INSERT INTO logs VALUES ('e1', 't1', 's1', 110, 'one'), ('e2', 't1', 's1', 120, 'two'), ('e3', 't3', 's4', 410, 'three'), ('e4', 't2', NULL, 310, 'four')",
}

// The OTel span/log SQL shapes in both engines: fan-out EXISTS over a
// repeated attribute struct, its time order, trace_id join probes, cardinality
// and keyset pages. pkg/relational/sqltest/otel pins Go end to end; this pins
// Java 4.14.2's plans and rows for the same schema, including two Java wrong
// answers Go does not reproduce (DIVERGENCES.md "fan-out EXISTS cardinality").
var _ = Describe("OtelSQLShapesJavaProbe", func() {
	It("plans and answers the OTel SQL shapes in both engines", func() {
		ctx := context.Background()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, fmt.Sprintf("otelshapes_%s", uuid.New().String()))
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = env.Cleanup(ctx) }()
		srv, err := NewIsolatedJavaInvoker()
		Expect(err).NotTo(HaveOccurred())
		defer func() { _ = srv.Close() }()
		javaRunner := plandiff.NewJavaRunnerHTTP(javaBaseURL(srv), env.ClusterFile).(plandiff.SetupRunner)
		clusterFilePath := writeClusterFileToTemp(env.ClusterFile)
		defer os.Remove(clusterFilePath)
		goRunner := plandiff.NewGoSQLSetupRunner(clusterFilePath)

		explain := func(r plandiff.RunResult) string {
			if r.Err != nil || len(r.Rows.Rows) == 0 {
				return fmt.Sprintf("ERR(%v)", r.Err)
			}
			return fmt.Sprint(r.Rows.Rows[0][0])
		}
		rows := func(r plandiff.RunResult) string {
			if r.Err != nil {
				return "ERR(" + r.Err.Error() + ")"
			}
			parts := make([]string, 0, len(r.Rows.Rows))
			for _, row := range r.Rows.Rows {
				parts = append(parts, fmt.Sprint(row))
			}
			return strings.Join(parts, " ")
		}
		const (
			gpt   = "EXISTS (SELECT 1 FROM s.attrs AS a WHERE a.scope = 'span' AND a.k = 'model' AND a.sv = 'gpt')"
			order = " ORDER BY s.start_ns DESC, s.trace_id DESC, s.span_id DESC"
			cols  = "SELECT s.trace_id, s.span_id, s.start_ns FROM spans AS s WHERE "
		)
		// javaRows is empty where Java answers goRows; only errors match a prefix.
		arms := []struct {
			sql, goRows, javaRows string
			goPlan, javaPlan      []string
			goLacks, javaLacks    []string
		}{
			// Several element conditions of one EXISTS bind the element columns.
			{
				sql: "SELECT s.trace_id, s.span_id FROM spans AS s WHERE " + gpt, goRows: "[t1 s1] [t1 s2] [t3 s4]",
				goPlan: []string{"IndexScan(SPANS_ATTR_STR, [=, =, =,"}, javaPlan: []string{"COVERING(SPANS_ATTR_STR [EQUALS", "EQUALS", "EQUALS"},
			},
			// ORDER BY the base-record suffix is the scan direction.
			{
				sql: cols + gpt + " ORDER BY s.start_ns DESC", goRows: "[t3 s4 400] [t1 s2 200] [t1 s1 100]",
				goPlan: []string{"IndexScan(SPANS_ATTR_STR, [=, =, =,", "REVERSE"}, goLacks: []string{"InMemorySort"},
				javaPlan: []string{"COVERING(SPANS_ATTR_STR", "REVERSE"},
			},
			{
				sql: cols + gpt + order, goRows: "[t3 s4 400] [t1 s2 200] [t1 s1 100]",
				goPlan: []string{"IndexScan(SPANS_ATTR_STR, [=, =, =,", "REVERSE"}, goLacks: []string{"InMemorySort"},
				javaPlan: []string{"COVERING(SPANS_ATTR_STR", "REVERSE"},
			},
			{
				sql: cols + gpt + " AND s.start_ns >= 150 AND s.start_ns < 450 ORDER BY s.start_ns DESC", goRows: "[t3 s4 400] [t1 s2 200]",
				goPlan: []string{"IndexScan(SPANS_ATTR_STR, [=, =, =, <>", "REVERSE"}, goLacks: []string{"InMemorySort"},
				javaPlan: []string{"COVERING(SPANS_ATTR_STR", "[GREATER_THAN_OR_EQUALS", "REVERSE"},
			},
			// A base-record predicate inside the EXISTS leaves the element scan to
			// a time-index driver in both engines.
			{
				sql:    cols + "EXISTS (SELECT 1 FROM s.attrs AS a WHERE a.scope = 'span' AND a.k = 'model' AND a.sv = 'gpt' AND s.start_ns >= 150 AND s.start_ns < 450) ORDER BY s.start_ns DESC",
				goRows: "[t3 s4 400] [t1 s2 200]",
				goPlan: []string{"IndexScan(SPANS_TIME", "Explode"}, javaPlan: []string{"ISCAN(SPANS_TIME", "EXPLODE"},
			},
			// The leading element column unbound: no fan-out index.
			{
				sql:    "SELECT s.trace_id, s.span_id FROM spans AS s WHERE EXISTS (SELECT 1 FROM s.attrs AS a WHERE a.k = 'model' AND a.sv = 'gpt')",
				goRows: "[t1 s1] [t1 s2] [t3 s4]", goPlan: []string{"Explode"}, goLacks: []string{"SPANS_ATTR_STR"},
				javaPlan: []string{"EXPLODE"}, javaLacks: []string{"SPANS_ATTR_STR"},
			},
			// The join form of the unnest uses no value fan-out index (Java issue #3896).
			{
				sql:    "SELECT s.trace_id, s.span_id FROM spans AS s, s.attrs AS a WHERE a.scope = 'span' AND a.k = 'model' AND a.sv = 'gpt'",
				goRows: "[t1 s1] [t1 s2] [t3 s4]", goPlan: []string{"Explode"}, goLacks: []string{"SPANS_ATTR_STR"},
				javaPlan: []string{"EXPLODE"}, javaLacks: []string{"SPANS_ATTR_STR"},
			},
			// Same-span conjunction: one EXISTS drives the index, the other filters elements.
			{
				sql:    "SELECT s.trace_id, s.span_id FROM spans AS s WHERE " + gpt + " AND EXISTS (SELECT 1 FROM s.attrs AS b WHERE b.scope = 'span' AND b.k = 'tag' AND b.sv = 'x')",
				goRows: "[t1 s1] [t3 s4]", goPlan: []string{"IndexScan(SPANS_ATTR_STR", "Explode"}, javaPlan: []string{"ISCAN(SPANS_ATTR_STR", "EXPLODE"},
			},
			// Cardinality: Java returns a span once per matching element when an
			// element column is unbound, and a DISTINCT trace twice.
			{
				sql:    "SELECT s.trace_id, s.span_id FROM spans AS s WHERE EXISTS (SELECT 1 FROM s.attrs AS a WHERE a.scope = 'span' AND a.k = 'tag')",
				goRows: "[t1 s1] [t3 s4]", javaRows: "[t1 s1] [t3 s4] [t1 s1]",
				goPlan: []string{"UnorderedPrimaryKeyDistinct(IndexScan(SPANS_ATTR_STR, [=, =,"}, javaPlan: []string{"COVERING(SPANS_ATTR_STR"},
			},
			{
				sql: "SELECT DISTINCT s1.trace_id FROM spans AS s1, spans AS s2 WHERE s1.trace_id = s2.trace_id AND " +
					"EXISTS (SELECT 1 FROM s1.attrs AS a WHERE a.scope = 'span' AND a.k = 'model' AND a.sv = 'gpt') AND " +
					"EXISTS (SELECT 1 FROM s2.attrs AS b WHERE b.scope = 'span' AND b.k = 'tag' AND b.sv = 'y')",
				goRows: "[t1]", javaRows: "[t1] [t1]",
				goPlan: []string{"Distinct(", "Scan(SPANS, [=])"}, javaPlan: []string{"FLATMAP", "FILTER q0.TRACE_ID EQUALS _.TRACE_ID"},
			},
			// Two-column trace joins between the tables of one store.
			{
				sql:    "SELECT s.span_id, l.event_id FROM spans AS s, logs AS l WHERE s.trace_id = 't1' AND l.trace_id = s.trace_id AND l.span_id = s.span_id",
				goRows: "[s1 e1] [s1 e2]", goPlan: []string{"IndexScan(LOGS_TRACE, [=, =,"},
				javaPlan: []string{"ISCAN(LOGS_TRACE [EQUALS q0.TRACE_ID, EQUALS q0.SPAN_ID])"},
			},
			{
				sql:    "SELECT l.event_id, s.start_ns FROM logs AS l, spans AS s WHERE l.event_id = 'e3' AND s.trace_id = l.trace_id AND s.span_id = l.span_id",
				goRows: "[e3 400]", goPlan: []string{"Scan(SPANS, [=, =])"},
				javaPlan: []string{"SCAN([IS SPANS, EQUALS q0.TRACE_ID, EQUALS q0.SPAN_ID])"},
			},
			{
				sql:    "SELECT s.span_id, l.event_id FROM spans AS s, logs AS l WHERE " + gpt + " AND l.trace_id = s.trace_id AND l.span_id = s.span_id",
				goRows: "[s1 e1] [s1 e2] [s4 e3]", goPlan: []string{"IndexScan(SPANS_ATTR_STR, [=, =, =,", "IndexScan(LOGS_TRACE, [=, =,"},
				javaPlan: []string{"ISCAN(SPANS_ATTR_STR", "ISCAN(LOGS_TRACE [EQUALS q0.TRACE_ID, EQUALS q0.SPAN_ID])"},
			},
			// Keyset pages: one range scan under a filter, or an ordered union.
			{
				sql:    cols + gpt + " AND s.start_ns <= 200 AND (s.start_ns < 200 OR s.trace_id < 't1' OR (s.trace_id = 't1' AND s.span_id < 's2'))" + order,
				goRows: "[t1 s1 100]", goPlan: []string{"PredicatesFilter(IndexScan(SPANS_ATTR_STR, [=, =, =, <>", "REVERSE"},
				goLacks: []string{"InMemorySort"}, javaPlan: []string{"COVERING(SPANS_ATTR_STR", "[LESS_THAN_OR_EQUALS", "REVERSE", "FILTER"},
			},
			{
				sql:    cols + gpt + " AND (s.start_ns < 200 OR (s.start_ns = 200 AND (s.trace_id < 't1' OR (s.trace_id = 't1' AND s.span_id < 's2'))))" + order,
				goRows: "[t1 s1 100]", goPlan: []string{"MergeSortUnion(", "DESC DISTINCT"}, goLacks: []string{"InMemorySort"},
				javaPlan: []string{"∪", "COMPARE BY (_.START_NS, _.TRACE_ID, _.SPAN_ID)"},
			},
			{
				sql:    cols + "s.start_ns < 400 OR (s.start_ns = 400 AND (s.trace_id < 't3' OR (s.trace_id = 't3' AND s.span_id < 's4')))" + order,
				goRows: "[t2 s3 300] [t1 s2 200] [t1 s1 100]", goPlan: []string{"MergeSortUnion(", "IndexScan(SPANS_TIME"},
				goLacks: []string{"InMemorySort"}, javaPlan: []string{"∪", "COVERING(SPANS_TIME"},
			},
			// Row-value comparison: neither engine accepts it.
			{
				sql:    "SELECT trace_id FROM spans WHERE (start_ns, trace_id, span_id) < (400, 't3', 's4')",
				goRows: "ERR(plandiff/go: query: 0AF00", javaRows: "ERR(plandiff: java SemanticException: The comparand to a comparison expecting an argument of a primitive type",
			},
			// LIMIT is Go's: Java rejects it and pages with maxRows.
			{
				sql:    "SELECT trace_id, span_id, start_ns FROM spans ORDER BY start_ns DESC, trace_id DESC, span_id DESC LIMIT 2",
				goRows: "[t3 s5 500] [t3 s4 400]", javaRows: "ERR(plandiff: java RelationalException: LIMIT clause is not supported.",
				goPlan: []string{"Limit(2,", "IndexScan(SPANS_TIME", "REVERSE"},
			},
		}
		for _, a := range arms {
			javaPlan := explain(javaRunner.RunWithSetup(ctx, otelShapesSchema, otelShapesSetup, "EXPLAIN "+a.sql))
			goPlan := explain(goRunner.RunWithSetup(ctx, otelShapesSchema, otelShapesSetup, "EXPLAIN "+a.sql))
			javaRows := rows(javaRunner.RunWithSetup(ctx, otelShapesSchema, otelShapesSetup, a.sql))
			goRows := rows(goRunner.RunWithSetup(ctx, otelShapesSchema, otelShapesSetup, a.sql))
			fmt.Fprintf(GinkgoWriter, "%s\n  java %s -> %s\n  go   %s -> %s\n", a.sql, javaPlan, javaRows, goPlan, goRows)
			wantJava := a.javaRows
			if wantJava == "" {
				wantJava = a.goRows
			}
			if strings.HasPrefix(wantJava, "ERR(") {
				Expect(javaRows).To(HavePrefix(wantJava), "%s: Java error", a.sql)
			} else {
				Expect(javaRows).To(Equal(wantJava), "%s: Java rows", a.sql)
			}
			if strings.HasPrefix(a.goRows, "ERR(") {
				Expect(goRows).To(HavePrefix(a.goRows), "%s: Go error", a.sql)
			} else {
				Expect(goRows).To(Equal(a.goRows), "%s: Go rows", a.sql)
			}
			for _, want := range a.javaPlan {
				Expect(javaPlan).To(ContainSubstring(want), "%s: Java plan", a.sql)
			}
			for _, unwanted := range a.javaLacks {
				Expect(javaPlan).NotTo(ContainSubstring(unwanted), "%s: Java plan", a.sql)
			}
			for _, want := range a.goPlan {
				Expect(goPlan).To(ContainSubstring(want), "%s: Go plan", a.sql)
			}
			for _, unwanted := range a.goLacks {
				Expect(goPlan).NotTo(ContainSubstring(unwanted), "%s: Go plan", a.sql)
			}
		}
	})
})
