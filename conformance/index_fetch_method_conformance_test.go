//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen/demopb"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"fdb.dev/pkg/recordlayer/indexfetch"
	"fdb.dev/pkg/recordlayer/query/executor"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"fdb.dev/pkg/relational/api"
	"fdb.dev/pkg/relational/core/embedded"
)

// RFC-258 PR3: INDEX_FETCH_METHOD through the planner and the SQL layer
// against Java on one cluster. Java's plan is read from its SQL layer
// (RelationalPlanConformanceAccess), since its EXPLAIN does not render the
// method either.

const indexFetchSQLSchema = "CREATE TABLE T (id BIGINT, a BIGINT, b BIGINT, c STRING, PRIMARY KEY (id))" +
	" CREATE INDEX idx_a ON T(a)" +
	" CREATE INDEX idx_ab ON T(a, b)" +
	" CREATE TABLE U (id BIGINT, t_id BIGINT, v BIGINT, PRIMARY KEY (id))" +
	" CREATE INDEX idx_t ON U(t_id)" +
	" CREATE INDEX sum_v AS SELECT SUM(v) FROM U GROUP BY t_id"

var indexFetchSQLSetup = []string{
	"INSERT INTO T VALUES (1, 1, 1, 'one'), (2, 2, 2, 'two'), (3, 1, 3, 'three'), (4, 3, 4, 'four'), (5, 2, 5, 'five')",
	"INSERT INTO U VALUES (1, 1, 5), (2, 1, 6), (3, 2, 7), (4, 4, 8)",
}

var indexFetchSQLStatements = []string{
	"SELECT * FROM T WHERE a = 1",
	"SELECT * FROM T WHERE a > 1 ORDER BY a DESC",
	"SELECT * FROM T WHERE a IN (1, 2)",
	"SELECT a, b FROM T WHERE a = 1",
	"SELECT a, COUNT(*) FROM T GROUP BY a",
	"SELECT t_id, SUM(v) FROM U GROUP BY t_id",
	"SELECT T.c, U.v FROM T, U WHERE T.a = 1 AND U.t_id = T.id",
	"SELECT T.id FROM T WHERE EXISTS (SELECT U.id FROM U WHERE U.t_id = T.id)",
	"UPDATE T SET c = 'x' WHERE a = 1",
	"DELETE FROM T WHERE a = 1",
}

// The order of Java's Options.IndexFetchMethod.values().
var indexFetchSQLMethods = []api.IndexFetchMethod{
	api.IndexFetchScanAndFetch, api.IndexFetchUseRemoteFetch, api.IndexFetchUseRemoteFetchWithFallback,
}

type indexFetchSQLResult struct {
	Method         string   `json:"method"`
	SQL            string   `json:"sql"`
	Explain        string   `json:"explain"`
	Scans          []string `json:"scans"`
	Rows           [][]any  `json:"rows"`
	UpdateCount    *int64   `json:"updateCount"`
	PlanThrew      bool     `json:"plan_threw"`
	PlanMessage    string   `json:"plan_message"`
	ExecuteThrew   bool     `json:"execute_threw"`
	ExecuteMessage string   `json:"execute_message"`
}

// indexFetchScans is the Java step's indexScans over a Go plan.
func indexFetchScans(plan plans.RecordQueryPlan) []string {
	out := []string{}
	plans.Walk(plan, func(node plans.RecordQueryPlan) bool {
		var kind string
		var idx *plans.RecordQueryIndexPlan
		switch p := node.(type) {
		case *plans.RecordQueryIndexPlan:
			kind, idx = "scan", p
		case *plans.RecordQueryCoveringIndexPlan:
			kind, idx = "covering", p.GetIndexPlan()
		case *plans.RecordQueryAggregateIndexPlan:
			kind, idx = "aggregate", p.GetIndexPlan()
		default:
			return true
		}
		out = append(out, fmt.Sprintf("%s %s %s", kind, idx.GetIndexName(), idx.GetIndexFetchMethod()))
		return true
	})
	slices.Sort(out)
	return out
}

// indexFetchRows renders rows as a sorted multiset with numbers as Java's JSON
// carries them.
func indexFetchRows(rows [][]any) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		cells := make([]string, len(row))
		for i, v := range row {
			switch n := v.(type) {
			case int64:
				v = float64(n)
			case int32:
				v = float64(n)
			}
			cells[i] = fmt.Sprintf("%v", v)
		}
		out = append(out, strings.Join(cells, "|"))
	}
	slices.Sort(out)
	return out
}

// indexFetchGoSchema creates a schema from schemaTemplate with the Go driver,
// runs setup, and returns a pool on it.
func indexFetchGoSchema(ctx context.Context, clusterFilePath, schemaTemplate string, setup []string) *sql.DB {
	suffix := strings.ReplaceAll(uuid.NewString(), "-", "")
	sys, err := sql.Open("fdbsql", "fdbsql:///__SYS?cluster_file="+clusterFilePath)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(sys.Close)
	template, dbPath, schema := "IFM_T_"+suffix, "/FRL/IFM_"+suffix, "S_"+suffix
	for _, stmt := range []string{
		fmt.Sprintf("CREATE SCHEMA TEMPLATE %s %s", template, schemaTemplate),
		"CREATE DATABASE " + dbPath,
		fmt.Sprintf("CREATE SCHEMA %s/%s WITH TEMPLATE %s", dbPath, schema, template),
	} {
		_, err := sys.ExecContext(ctx, stmt)
		Expect(err).NotTo(HaveOccurred(), stmt)
	}
	DeferCleanup(func() {
		_, _ = sys.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+dbPath)
		_, _ = sys.ExecContext(context.Background(), "DROP SCHEMA TEMPLATE IF EXISTS "+template)
	})
	db, err := sql.Open("fdbsql", fmt.Sprintf("fdbsql://%s?cluster_file=%s&schema=%s",
		strings.ToUpper(dbPath), clusterFilePath, strings.ToUpper(schema)))
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(db.Close)
	for _, stmt := range setup {
		_, err := db.ExecContext(ctx, stmt)
		Expect(err).NotTo(HaveOccurred(), stmt)
	}
	return db
}

// indexFetchGoConn pins a connection carrying the given options.
func indexFetchGoConn(ctx context.Context, db *sql.DB, options map[api.OptionName]any) *sql.Conn {
	conn, err := db.Conn(ctx)
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(conn.Close)
	Expect(conn.Raw(func(dc any) error {
		ec := dc.(*embedded.EmbeddedConnection)
		for name, value := range options {
			if err := ec.SetOption(name, value); err != nil {
				return err
			}
		}
		return nil
	})).To(Succeed())
	return conn
}

func indexFetchQueryRows(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, query string,
) ([][]any, error) {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]any
	for rows.Next() {
		row := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range row {
			ptrs[i] = &row[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

type indexFetchPlanRequest struct {
	Shape        string `json:"shape"`
	Method       string `json:"method"`
	Reverse      bool   `json:"reverse"`
	RowLimit     int    `json:"rowLimit"`
	Skip         int    `json:"skip"`
	LocalWriteID int    `json:"localWriteID"`
}

type indexFetchPlanPage struct {
	IDs            []int64 `json:"ids"`
	ExceptionClass string  `json:"exceptionClass"`
	ErrorCode      int     `json:"errorCode"`
	RemoteFetches  int64   `json:"remoteFetches"`
	RemoteEntries  int64   `json:"remoteEntries"`
}

var _ = Describe("Index fetch method planner and SQL Java conformance (RFC-258)", func() {
	var (
		ctx  context.Context
		env  *TenantEnvironment
		java *JavaInvoker
	)
	BeforeEach(func() {
		ctx = context.Background()
		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, "index_fetch_"+uuid.NewString())
		Expect(err).NotTo(HaveOccurred())
		java = remoteFetchJavaInvoker()
	})
	AfterEach(func() {
		if env != nil {
			Expect(env.Cleanup(ctx)).To(Succeed())
		}
	})

	It("stamps each option on Java's index scans, never changes the plan, and returns Java's rows", func() {
		var oracle struct {
			Results []indexFetchSQLResult `json:"results"`
		}
		Expect(java.InvokeAs(ctx, "indexFetchSqlPlans", map[string]any{
			"clusterFile": env.ClusterFile, "schemaTemplate": indexFetchSQLSchema,
			"setupSqls": indexFetchSQLSetup, "statements": indexFetchSQLStatements,
		}, &oracle)).To(Succeed())
		Expect(oracle.Results).To(HaveLen(len(indexFetchSQLMethods) * len(indexFetchSQLStatements)))

		clusterFilePath := writeClusterFileToTemp(env.ClusterFile)
		DeferCleanup(os.Remove, clusterFilePath)
		db := indexFetchGoSchema(ctx, clusterFilePath, indexFetchSQLSchema, indexFetchSQLSetup)
		var mismatches []string
		javaExplain, goExplain := map[string]string{}, map[string]string{}
		conns := map[api.IndexFetchMethod]*sql.Conn{}
		for i, result := range oracle.Results {
			method := indexFetchSQLMethods[i/len(indexFetchSQLStatements)]
			sqlText := indexFetchSQLStatements[i%len(indexFetchSQLStatements)]
			Expect(result.Method).To(Equal(method.String()))
			Expect(result.SQL).To(Equal(sqlText))
			mismatch := func(format string, args ...any) {
				mismatches = append(mismatches, fmt.Sprintf("%s %q: ", method, sqlText)+fmt.Sprintf(format, args...))
			}
			if result.PlanThrew || result.ExecuteThrew {
				mismatch("Java failed: %s %s", result.PlanMessage, result.ExecuteMessage)
				continue
			}

			// Java stamps every BY_VALUE scan, covering inners included, and
			// downgrades an aggregate index's BY_GROUP scan.
			for _, scan := range result.Scans {
				want := method.String()
				if strings.HasPrefix(scan, "aggregate ") {
					want = indexfetch.ScanAndFetch.String()
				}
				if !strings.HasSuffix(scan, " "+want) {
					mismatch("Java scan %q does not carry %s", scan, want)
				}
			}
			options := api.NewOptionsBuilder().Set(api.OptIndexFetchMethod, method).Build()
			isDML := !strings.HasPrefix(sqlText, "SELECT")
			var plan plans.RecordQueryPlan
			var err error
			if isDML {
				plan, err = embedded.PlanPhysicalDMLForTestWithOptions(sqlText, indexFetchSQLSchema, nil, options)
			} else {
				plan, err = embedded.PlanPhysicalForTestWithOptions(sqlText, indexFetchSQLSchema, nil, options)
			}
			Expect(err).NotTo(HaveOccurred(), sqlText)
			if scans := indexFetchScans(plan); !slices.Equal(scans, result.Scans) {
				mismatch("Go scans %v, Java scans %v (Go plan %s, Java plan %s)", scans, result.Scans, plan.Explain(), result.Explain)
			}
			// The option never changes the plan: EXPLAIN text is the same under
			// every method, in each engine.
			if prev, ok := javaExplain[sqlText]; ok && prev != result.Explain {
				mismatch("Java plan %s differs from %s", result.Explain, prev)
			}
			javaExplain[sqlText] = result.Explain
			if prev, ok := goExplain[sqlText]; ok && prev != plan.Explain() {
				mismatch("Go plan %s differs from %s", plan.Explain(), prev)
			}
			goExplain[sqlText] = plan.Explain()

			conn, ok := conns[method]
			if !ok {
				conn = indexFetchGoConn(ctx, db, map[api.OptionName]any{api.OptIndexFetchMethod: method})
				conns[method] = conn
			}
			if isDML {
				tx, err := conn.BeginTx(ctx, nil)
				Expect(err).NotTo(HaveOccurred())
				res, err := tx.ExecContext(ctx, sqlText)
				Expect(err).NotTo(HaveOccurred(), sqlText)
				count, err := res.RowsAffected()
				Expect(err).NotTo(HaveOccurred())
				Expect(tx.Rollback()).To(Succeed())
				if result.UpdateCount == nil || *result.UpdateCount != count {
					mismatch("Go updated %d rows, Java %v", count, result.UpdateCount)
				}
				continue
			}
			rows, err := indexFetchQueryRows(ctx, conn, sqlText)
			Expect(err).NotTo(HaveOccurred(), sqlText)
			if got, want := indexFetchRows(rows), indexFetchRows(result.Rows); !slices.Equal(got, want) {
				mismatch("Go rows %v, Java rows %v", got, want)
			}
		}
		Expect(mismatches).To(BeEmpty(), strings.Join(mismatches, "\n"))
	})

	// RemoteFetchTest.failAfterRecordsReturnedTest at plan level, with the
	// ExecuteProperties limits Java's fallBackContinueFrom passes again.
	It("matches Java's plan-level fallback after returned rows, row limit and skip included", func() {
		var written int
		ks := subspace.FromBytes(tuple.Tuple{"remote", "plan"}.Pack())
		Expect(java.InvokeAs(ctx, "seedIndexRemoteFetch", map[string]any{
			"clusterFile": env.ClusterFile, "tenantName": env.TenantName, "subspace": BytesToIntArray(ks.Bytes()),
			"shape": "many", "count": 500, "orphan": false,
		}, &written)).To(Succeed())
		Expect(written).To(Equal(500))
		orderType := executor.PositionalTypeForRecordLayout((&demopb.Order{}).ProtoReflect().Descriptor(), true)
		goPage := func(req indexFetchPlanRequest) indexFetchPlanPage {
			transaction, err := env.RecordDB.CreateTransaction()
			Expect(err).NotTo(HaveOccurred())
			defer transaction.Cancel()
			rtx := env.RecordDB.NewRecordContext(transaction)
			timer := recordlayer.NewStoreTimer()
			rtx.SetTimer(timer)
			store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetSubspace(ks).
				SetMetaDataProvider(indexRemoteMetadata("many")).SetFormatVersion(indexRemoteFormat("many")).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())
			if req.LocalWriteID >= 0 {
				previous, err := store.LoadRecord(indexRemotePrimaryKey("many", req.LocalWriteID))
				Expect(err).NotTo(HaveOccurred())
				order := proto.Clone(previous.Record).(*demopb.Order)
				order.Tags = []string{"local-write"}
				_, err = store.SaveRecord(order)
				Expect(err).NotTo(HaveOccurred())
			}
			plan, err := plans.NewRecordQueryIndexPlan("price", []*predicates.ComparisonRange{predicates.EmptyComparisonRange()},
				[]string{"Order"}, orderType, req.Reverse)
			Expect(err).NotTo(HaveOccurred())
			var method indexfetch.Method
			switch req.Method {
			case "USE_REMOTE_FETCH":
				method = indexfetch.UseRemoteFetch
			case "USE_REMOTE_FETCH_WITH_FALLBACK":
				method = indexfetch.UseRemoteFetchWithFallback
			}
			stamped := plan.WithKeyComponentTypes([]values.Type{values.NullableInt}).WithIndexFetchMethod(method)
			props := recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(req.RowLimit)
			props.Skip = req.Skip
			timer.Reset()
			page := indexFetchPlanPage{IDs: []int64{}}
			cursor, err := executor.ExecutePlan(ctx, stamped, store, executor.EmptyEvaluationContext(), nil, props)
			Expect(err).NotTo(HaveOccurred())
			defer cursor.Close()
			for {
				next, err := cursor.OnNext(ctx)
				if err != nil {
					var native fdb.Error
					if errors.As(err, &native) {
						page.ExceptionClass, page.ErrorCode = "FDBException", native.Code
					} else {
						page.ExceptionClass = fmt.Sprintf("%T", err)
					}
					break
				}
				if !next.HasNext() {
					break
				}
				page.IDs = append(page.IDs, next.GetValue().Record.Record.(*demopb.Order).GetOrderId())
			}
			page.RemoteFetches = timer.GetCount(recordlayer.CountRemoteFetch)
			page.RemoteEntries = timer.GetCount(recordlayer.EventScanRemoteFetchEntry)
			return page
		}
		var mismatches []string
		for _, reverse := range []bool{false, true} {
			for _, method := range []string{"SCAN_AND_FETCH", "USE_REMOTE_FETCH", "USE_REMOTE_FETCH_WITH_FALLBACK"} {
				for _, limits := range [][2]int{{0, 0}, {150, 0}, {0, 3}, {150, 3}} {
					for _, write := range []bool{false, true} {
						req := indexFetchPlanRequest{
							Shape: "many", Method: method, Reverse: reverse,
							RowLimit: limits[0], Skip: limits[1], LocalWriteID: -1,
						}
						if write {
							// Position 100 in scan order: past the first mapped batch.
							req.LocalWriteID = 100
							if reverse {
								req.LocalWriteID = 399
							}
						}
						var oracle indexFetchPlanPage
						Expect(java.InvokeAs(ctx, "indexFetchPlanExecute", map[string]any{
							"clusterFile": env.ClusterFile, "tenantName": env.TenantName,
							"subspace": BytesToIntArray(ks.Bytes()), "request": req,
						}, &oracle)).To(Succeed())
						actual := goPage(req)
						if oracle.IDs == nil {
							oracle.IDs = []int64{}
						}
						// Go applies a plan's skip and row limit above its scan's
						// ranges (DIVERGENCES.md), which changes the entry count only.
						entriesDiffer := req.RowLimit == 0 && req.Skip == 0 && actual.RemoteEntries != oracle.RemoteEntries
						if write && method == "USE_REMOTE_FETCH" && len(oracle.IDs) == 0 {
							mismatches = append(mismatches, fmt.Sprintf("%+v: Java failed before returning a row", req))
						}
						if !slices.Equal(actual.IDs, oracle.IDs) || actual.ExceptionClass != oracle.ExceptionClass ||
							actual.ErrorCode != oracle.ErrorCode || actual.RemoteFetches != oracle.RemoteFetches || entriesDiffer {
							mismatches = append(mismatches, fmt.Sprintf(
								"%+v: Go %d rows %v.. err %s/%d fetches %d entries %d; Java %d rows %v.. err %s/%d fetches %d entries %d",
								req, len(actual.IDs), indexFetchHead(actual.IDs), actual.ExceptionClass, actual.ErrorCode, actual.RemoteFetches, actual.RemoteEntries,
								len(oracle.IDs), indexFetchHead(oracle.IDs), oracle.ExceptionClass, oracle.ErrorCode, oracle.RemoteFetches, oracle.RemoteEntries))
						}
					}
				}
			}
		}
		Expect(mismatches).To(BeEmpty(), strings.Join(mismatches, "\n"))
	})

	// The same fallback through the SQL layer: a write in the transaction, then
	// an index scan reaching it past its first batch, under MAX_ROWS.
	It("matches Java's SQL results when remote fetch fails after returned rows", func() {
		const schema = "CREATE TABLE T (id BIGINT, a BIGINT, c STRING, PRIMARY KEY (id)) CREATE INDEX idx_a ON T(a)"
		var setup []string
		for first := 0; first < 500; first += 100 {
			var tuples []string
			for id := first; id < first+100; id++ {
				tuples = append(tuples, fmt.Sprintf("(%d, %d, 'r')", id, id))
			}
			setup = append(setup, "INSERT INTO T VALUES "+strings.Join(tuples, ", "))
		}
		const write, query = "UPDATE T SET c = 'w' WHERE id = 100", "SELECT * FROM T WHERE a >= 0"
		clusterFilePath := writeClusterFileToTemp(env.ClusterFile)
		DeferCleanup(os.Remove, clusterFilePath)
		db := indexFetchGoSchema(ctx, clusterFilePath, schema, setup)
		var mismatches []string
		for _, method := range indexFetchSQLMethods {
			for _, maxRows := range []int{0, 150} {
				var oracle struct {
					Rows          [][]any `json:"rows"`
					QueryThrew    bool    `json:"query_threw"`
					QuerySQLState string  `json:"query_sqlState"`
					QueryMessage  string  `json:"query_message"`
				}
				Expect(java.InvokeAs(ctx, "indexFetchSqlFallback", map[string]any{
					"clusterFile": env.ClusterFile, "schemaTemplate": schema, "setupSqls": setup,
					"writeSql": write, "querySql": query, "method": method.String(), "maxRows": maxRows,
				}, &oracle)).To(Succeed())
				options := map[api.OptionName]any{api.OptIndexFetchMethod: method}
				if maxRows > 0 {
					options[api.OptMaxRows] = maxRows
				}
				conn := indexFetchGoConn(ctx, db, options)
				tx, err := conn.BeginTx(ctx, nil)
				Expect(err).NotTo(HaveOccurred())
				_, err = tx.ExecContext(ctx, write)
				Expect(err).NotTo(HaveOccurred())
				rows, queryErr := indexFetchQueryRows(ctx, tx, query)
				Expect(tx.Rollback()).To(Succeed())
				if (queryErr != nil) != oracle.QueryThrew || len(rows) != len(oracle.Rows) ||
					!slices.Equal(indexFetchRows(rows), indexFetchRows(oracle.Rows)) {
					mismatches = append(mismatches, fmt.Sprintf("%s MAX_ROWS %d: Go %d rows err %v; Java %d rows err %v %s %s",
						method, maxRows, len(rows), queryErr, len(oracle.Rows), oracle.QueryThrew, oracle.QuerySQLState, oracle.QueryMessage))
				}
			}
		}
		Expect(mismatches).To(BeEmpty(), strings.Join(mismatches, "\n"))
	})
})

func indexFetchHead(ids []int64) []int64 {
	if len(ids) > 5 {
		return ids[:5]
	}
	return ids
}
