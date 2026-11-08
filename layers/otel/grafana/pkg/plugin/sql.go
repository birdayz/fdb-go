package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	"github.com/grafana/grafana-plugin-sdk-go/data/sqlutil"
	"github.com/grafana/sqlds/v4"
)

// dsn is the read-only connection string of one tenant schema; schema ""
// opens a schema-less connection for catalog reads.
func (s *Settings) dsn(schema string) string {
	q := url.Values{}
	if schema != "" {
		q.Set("schema", schema)
	}
	q.Set("read_only", "true")
	q.Set("execution_scanned_rows_limit", strconv.FormatInt(s.SQLPageRows, 10))
	q.Set("execution_scanned_bytes_limit", strconv.FormatInt(s.SQLPageBytes, 10))
	q.Set("max_rows", strconv.FormatInt(s.SQLMaxRows, 10))
	q.Set("max_statement_memory_bytes", strconv.FormatInt(s.SQLStatementMemoryBytes, 10))
	q.Set("transaction_timeout", strconv.FormatInt(s.TransactionTimeout.Milliseconds(), 10))
	if len(s.TransactionTags) > 0 {
		q.Set("transaction_tags", strings.Join(s.TransactionTags, ","))
	}
	u := url.URL{Scheme: "fdbsql", Path: s.Database, RawQuery: q.Encode()}
	return u.String()
}

// sqlTenant is one tenant's sqlds datasource and its *sql.DB pool. The LRU
// over these is what bounds the pools sqlds would otherwise keep forever.
type sqlTenant struct {
	ds *sqlds.SQLDatasource
	db *sql.DB
}

func (t *sqlTenant) close() { t.ds.Dispose() }

func (d *Datasource) openSQLTenant(ctx context.Context, schema string) (*sqlTenant, error) {
	h, err := d.database(ctx)
	if err != nil {
		return nil, err
	}
	drv := &sqlDriver{d: d, schema: schema, handle: h}
	ds := sqlds.NewDatasource(drv)
	ds.EnableRowLimit = true
	ds.Completable = &completer{schema: schema, db: func() *sql.DB { return drv.db }}
	if _, err := ds.NewDatasource(ctx, d.inst); err != nil {
		return nil, err
	}
	return &sqlTenant{ds: ds, db: drv.db}, nil
}

// sqlDriver adapts the fdbsql driver to sqlds for one tenant schema.
type sqlDriver struct {
	d      *Datasource
	schema string
	handle *fdbHandle
	db     *sql.DB
}

var (
	_ sqlds.Driver          = (*sqlDriver)(nil)
	_ sqlds.ResponseMutator = (*sqlDriver)(nil)
)

// Connect opens the tenant's pool over the instance's FDB database. sqlds
// calls it once per tenant datasource; per-query connection args are off.
func (s *sqlDriver) Connect(_ context.Context, _ backend.DataSourceInstanceSettings, args json.RawMessage) (*sql.DB, error) {
	if len(args) > 0 {
		return nil, errors.New("connection arguments are not supported; set the query's tenant")
	}
	conn, err := s.d.deps.newConnector(s.handle.db, s.d.cfg.dsn(s.schema))
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(conn)
	db.SetMaxOpenConns(sqlMaxOpenConns)
	db.SetMaxIdleConns(sqlMaxIdleConns)
	s.db = db
	return db, nil
}

// Settings are the sqlds budgets: query deadline and returned-row cap.
// Retries stay off: a read that failed is not retried behind the user's back.
func (s *sqlDriver) Settings(context.Context, backend.DataSourceInstanceSettings) sqlds.DriverSettings {
	return sqlds.DriverSettings{Timeout: s.d.cfg.QueryTimeout, RowLimit: s.d.cfg.SQLMaxRows}
}

func (s *sqlDriver) Macros() sqlutil.Macros { return macros }

func (s *sqlDriver) Converters() []sqlutil.Converter { return converters() }

// MutateResponse links trace_id columns to the trace query (RFC §7).
func (s *sqlDriver) MutateResponse(_ context.Context, frames data.Frames) (data.Frames, error) {
	for _, f := range frames {
		for _, field := range f.Fields {
			if strings.EqualFold(field.Name, "trace_id") {
				addLinks(field, traceLink(s.d.inst, s.schema))
			}
		}
	}
	return frames, nil
}

// querySQL groups the request's queries by tenant and runs each group
// through that tenant's sqlds datasource.
func (d *Datasource) querySQL(ctx context.Context, req *backend.QueryDataRequest) (*backend.QueryDataResponse, error) {
	resp := backend.NewQueryDataResponse()
	groups := map[string][]backend.DataQuery{}
	var order []string
	for _, q := range req.Queries {
		m, err := parseQuery(q)
		if err == nil {
			m.Tenant, err = d.tenant(m.Tenant)
		}
		if err != nil {
			resp.Responses[q.RefID] = errorResponse(err)
			continue
		}
		if _, ok := groups[m.Tenant]; !ok {
			order = append(order, m.Tenant)
		}
		groups[m.Tenant] = append(groups[m.Tenant], q)
	}
	for _, tenant := range order {
		qs := groups[tenant]
		t, release, err := d.sqlPools.acquire(ctx, tenant)
		if err != nil {
			for _, q := range qs {
				resp.Responses[q.RefID] = errorResponse(err)
			}
			continue
		}
		r, err := t.ds.QueryData(ctx, &backend.QueryDataRequest{PluginContext: req.PluginContext, Headers: req.Headers, Queries: qs})
		release()
		if err != nil {
			for _, q := range qs {
				resp.Responses[q.RefID] = errorResponse(err)
			}
			continue
		}
		for refID, dr := range r.Responses {
			if dr.Error != nil {
				dr.Error = errors.New(describeError(dr.Error))
			}
			resp.Responses[refID] = dr
		}
	}
	return resp, nil
}

// completer answers sqlds' tables/columns routes for one tenant from
// INFORMATION_SCHEMA, filtering rows in Go so no option is spliced into SQL.
// The schemas route is the resource handler's (allowedSchemas applies).
type completer struct {
	schema string
	db     func() *sql.DB
}

func (c *completer) Schemas(context.Context, sqlds.Options) ([]string, error) {
	return nil, sqlds.ErrorNotImplemented
}

func (c *completer) Tables(ctx context.Context, _ sqlds.Options) ([]string, error) {
	rows, err := c.db().QueryContext(ctx, `SELECT TABLE_SCHEMA, TABLE_NAME FROM "INFORMATION_SCHEMA"."TABLES"`)
	if err != nil {
		return nil, err
	}
	return collectMatching(rows, func(cols []string) (string, bool) {
		return cols[1], cols[0] == c.schema
	})
}

func (c *completer) Columns(ctx context.Context, o sqlds.Options) ([]string, error) {
	rows, err := c.db().QueryContext(ctx, `SELECT TABLE_SCHEMA, TABLE_NAME, COLUMN_NAME FROM "INFORMATION_SCHEMA"."COLUMNS"`)
	if err != nil {
		return nil, err
	}
	return collectMatching(rows, func(cols []string) (string, bool) {
		return cols[2], cols[0] == c.schema && strings.EqualFold(cols[1], o["table"])
	})
}

// collectMatching scans string rows and keeps keep's value where it matches,
// in result order, without duplicates.
func collectMatching(rows *sql.Rows, keep func([]string) (string, bool)) ([]string, error) {
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	vals := make([]sql.NullString, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	out := []string{}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		strs := make([]string, len(vals))
		for i, v := range vals {
			strs[i] = v.String
		}
		if v, ok := keep(strs); ok && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out, rows.Err()
}

// listSchemas returns the database's schemas that allowedSchemas admits.
func listSchemas(ctx context.Context, db *sql.DB, policy schemaPolicy) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT SCHEMA_NAME FROM "INFORMATION_SCHEMA"."SCHEMATA"`)
	if err != nil {
		return nil, err
	}
	out, err := collectMatching(rows, func(cols []string) (string, bool) {
		return cols[0], policy.allows(cols[0])
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(out)
	return out, nil
}
