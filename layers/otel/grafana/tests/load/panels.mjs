// The R1 load dashboard's 20 panels (14 SQL, 6 trace searches); gen-dashboard.mjs
// writes provisioning/dashboards/fdb-load.json from them.
const tsGroup = (col) => `$__timeGroupAlias(${col}, $__interval)`;
const grp = (col) => `$__timeGroup(${col}, $__interval)`;
const series = (title, rawSql, tenant) => ({
  title,
  type: 'timeseries',
  target: { queryType: 'sql', format: 0, rawSql, ...(tenant ? { tenant } : {}) },
});
const table = (title, rawSql) => ({ title, type: 'table', target: { queryType: 'sql', format: 1, rawSql } });
const search = (title, q) => ({ title, type: 'table', target: { queryType: 'traceSearch', limit: 20, ...q } });
const span = (scope, key, type, value) => ({ scope, key, type, value });
const spansBy = (where = '') =>
  `SELECT ${tsGroup('start_ns')}, service_name, COUNT(*) AS spans FROM spans WHERE $__timeFilter(start_ns)${where} GROUP BY ${grp('start_ns')}, service_name ORDER BY 1`;
const spanCount = (where) =>
  `SELECT ${tsGroup('start_ns')}, COUNT(*) AS spans FROM spans WHERE $__timeFilter(start_ns) AND ${where} GROUP BY ${grp('start_ns')} ORDER BY 1`;
const logsBy = (where = '') =>
  `SELECT ${tsGroup('effective_ns')}, severity_text, COUNT(*) AS logs FROM logs WHERE $__timeFilter(effective_ns)${where} GROUP BY ${grp('effective_ns')}, severity_text ORDER BY 1`;

export const panels = [
  series('Spans by service (T0)', spansBy()),
  series('Error spans', spanCount('status_code = 2')),
  series(
    'Mean span duration (ms)',
    `SELECT ${tsGroup('start_ns')}, AVG(end_ns - start_ns) / 1000000 AS mean_ms FROM spans WHERE $__timeFilter(start_ns) GROUP BY ${grp('start_ns')} ORDER BY 1`
  ),
  series(
    'Max span duration (ms)',
    `SELECT ${tsGroup('start_ns')}, MAX(end_ns - start_ns) / 1000000 AS max_ms FROM spans WHERE $__timeFilter(start_ns) GROUP BY ${grp('start_ns')} ORDER BY 1`
  ),
  series('Logs by severity (T0)', logsBy()),
  series('Server spans', spanCount('kind = 2')),
  series('Chat calls', spanCount("name = 'chat gpt-4o'")),
  series('Spans by service (T1)', spansBy(), 'T1'),
  series('Logs by severity (T1)', logsBy(), 'T1'),
  series('Error logs', logsBy(' AND severity_number >= 17')),
  series('Database spans', spanCount("name = 'SELECT documents'")),
  series('Checkout spans', spanCount("service_name = 'checkout'")),
  table(
    'Operations',
    'SELECT name, COUNT(*) AS n, AVG(end_ns - start_ns) / 1000000 AS mean_ms FROM spans WHERE $__timeFilter(start_ns) GROUP BY name ORDER BY n DESC'
  ),
  table(
    'Latest errors',
    'SELECT trace_id, service_name, name, status_message FROM spans WHERE $__timeFilter(start_ns) AND status_code = 2 ORDER BY start_ns DESC LIMIT 20'
  ),
  search('Traces: frontend', {
    mode: 'attribute',
    spanPredicates: [span('resource', 'service.name', 'string', 'frontend')],
  }),
  search('Traces: checkout', {
    mode: 'attribute',
    spanPredicates: [span('resource', 'service.name', 'string', 'checkout')],
  }),
  search('Newest traces', { mode: 'time' }),
  search('Chat calls to gpt-4o', {
    mode: 'sameSpan',
    spanPredicates: [
      span('span', 'gen_ai.operation.name', 'string', 'chat'),
      span('span', 'gen_ai.request.model', 'string', 'gpt-4o'),
    ],
  }),
  search('Failed requests with an evaluation', {
    mode: 'existential',
    spanPredicates: [span('span', 'http.status_code', 'int', '500')],
    logPredicates: [{ scope: 'event_name', key: 'event_name', type: 'string', value: 'gen_ai.evaluation.result' }],
  }),
  {
    ...search('Tool calls (T1)', { mode: 'sameSpan', spanPredicates: [span('span', 'tool.name', 'string', 'search')] }),
    tenantT1: true,
  },
];
