import {
  AttrType,
  DEFAULT_LOGS_LIMIT,
  DEFAULT_SEARCH_LIMIT,
  EVENT_NAME_SCOPE,
  FDBQuery,
  LogPredicate,
  MAX_SEARCH_LIMIT,
  Predicate,
  QueryType,
  SearchMode,
  SpanPredicate,
  SqlFormat,
} from './types';

// Pure functions over the query model. The editors only change a query through
// these, so a stored query (run by the backend alone for alerting and public
// dashboards) always holds exactly the fields its query type and mode read.

export function defaultQuery(): Partial<FDBQuery> {
  return { queryType: 'sql', rawSql: '', format: SqlFormat.Table };
}

export function emptySpanPredicate(): SpanPredicate {
  return { scope: 'span', key: '', type: 'string', value: '' };
}

export function emptyLogPredicate(): LogPredicate {
  return { scope: 'log', key: '', type: 'string', value: '' };
}

export function queryTypeOf(q: FDBQuery): QueryType {
  return q.queryType ?? 'sql';
}

export function searchModeOf(q: FDBQuery): SearchMode {
  return q.mode ?? 'sameSpan';
}

/** Switches the query type. Fields of other types stay (the backend dispatches on queryType); limit resets because its default differs per type. */
export function withQueryType(q: FDBQuery, queryType: QueryType): FDBQuery {
  const next: FDBQuery = { ...q, queryType };
  if (q.queryType !== queryType) {
    delete next.limit;
  }
  switch (queryType) {
    case 'sql':
      return { ...next, rawSql: next.rawSql ?? '', format: next.format ?? SqlFormat.Table };
    case 'trace':
      return { ...next, traceId: next.traceId ?? '' };
    case 'traceSearch':
      return withSearchMode(next, searchModeOf(next));
    case 'logs':
      return withLogsBy(next, next.attribute && !next.traceId ? 'attribute' : 'trace');
  }
}

/** Sets the tenant; empty removes it, so the datasource default applies. */
export function withTenant(q: FDBQuery, tenant: string | undefined): FDBQuery {
  const next: FDBQuery = { ...q, tenant };
  if (!tenant) {
    delete next.tenant;
  }
  return next;
}

/** Switches the search mode and keeps only the predicates that mode reads. */
export function withSearchMode(q: FDBQuery, mode: SearchMode): FDBQuery {
  const next: FDBQuery = { ...q, mode };
  const spans = q.spanPredicates ?? [];
  switch (mode) {
    case 'time':
      delete next.spanPredicates;
      delete next.logPredicates;
      break;
    case 'attribute':
      next.spanPredicates = [spans[0] ?? emptySpanPredicate()];
      delete next.logPredicates;
      break;
    case 'sameSpan':
      next.spanPredicates = spans.length ? spans : [emptySpanPredicate()];
      delete next.logPredicates;
      break;
    case 'existential': {
      const logs = q.logPredicates ?? [];
      next.spanPredicates = spans.length || logs.length ? spans : [emptySpanPredicate()];
      next.logPredicates = logs;
      break;
    }
  }
  return next;
}

export type LogsBy = 'trace' | 'attribute';

export function logsByOf(q: FDBQuery): LogsBy {
  return q.attribute ? 'attribute' : 'trace';
}

/** Logs read either a trace (and optionally one span) or one log attribute, never both. */
export function withLogsBy(q: FDBQuery, by: LogsBy): FDBQuery {
  const next: FDBQuery = { ...q };
  if (by === 'attribute') {
    next.attribute = q.attribute ?? emptyLogPredicate();
    delete next.traceId;
    delete next.spanId;
  } else {
    next.traceId = q.traceId ?? '';
    delete next.attribute;
  }
  return next;
}

/** Changes a predicate's scope. Log scope event_name matches the indexed event name: its key and type are fixed. */
export function withScope<P extends Predicate>(p: P, scope: P['scope']): P {
  if (scope === EVENT_NAME_SCOPE) {
    return { ...p, scope, key: EVENT_NAME_SCOPE, type: 'string' };
  }
  if (p.scope === EVENT_NAME_SCOPE) {
    return { ...p, scope, key: '' };
  }
  return { ...p, scope };
}

const templateVariable = /\$\w+|\$\{[^}]+\}|\[\[[^\]]+\]\]/;

export function hasTemplateVariable(s: string | undefined): boolean {
  return !!s && templateVariable.test(s);
}

const INT64_MIN = BigInt('-9223372036854775808');
const INT64_MAX = BigInt('9223372036854775807');

/** Validates a predicate value against its type; template variables are checked by the backend after interpolation. */
export function valueError(type: AttrType, value: string): string | undefined {
  const v = value.trim();
  if (v === '' || type === 'string' || hasTemplateVariable(v)) {
    return undefined;
  }
  switch (type) {
    case 'int': {
      if (!/^[+-]?\d+$/.test(v)) {
        return 'Expected an integer';
      }
      const n = BigInt(v);
      return n < INT64_MIN || n > INT64_MAX ? 'Outside the 64-bit integer range' : undefined;
    }
    case 'double':
      if (v === 'NaN' || v === '+Inf' || v === '-Inf') {
        return undefined;
      }
      return /^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$/.test(v) && Number.isFinite(Number(v))
        ? undefined
        : 'Expected a finite number';
    case 'bool':
      return v === 'true' || v === 'false' ? undefined : 'Expected true or false';
  }
}

export function predicateComplete(p: Predicate): boolean {
  return p.key.trim() !== '' && p.value.trim() !== '' && valueError(p.type, p.value) === undefined;
}

export function traceIdError(id: string | undefined): string | undefined {
  const v = (id ?? '').trim();
  if (v === '' || hasTemplateVariable(v) || /^[0-9a-fA-F]{1,32}$/.test(v)) {
    return undefined;
  }
  return 'A trace ID is 1 to 32 hexadecimal characters';
}

export function spanIdError(id: string | undefined): string | undefined {
  const v = (id ?? '').trim();
  if (v === '' || hasTemplateVariable(v) || /^[0-9a-fA-F]{1,16}$/.test(v)) {
    return undefined;
  }
  return 'A span ID is 1 to 16 hexadecimal characters';
}

/** Lowercase hex, left-padded to the full width (64-bit trace IDs are zero-extended in OTLP). */
function normalizeHexId(id: string, width: number): string {
  const v = id.trim().toLowerCase();
  return new RegExp(`^[0-9a-f]{1,${width}}$`).test(v) ? v.padStart(width, '0') : v;
}

export function normalizeTraceId(id: string): string {
  return normalizeHexId(id, 32);
}

export function normalizeSpanId(id: string): string {
  return normalizeHexId(id, 16);
}

export function limitError(limit: number | undefined, max?: number): string | undefined {
  if (limit === undefined) {
    return undefined;
  }
  if (!Number.isInteger(limit) || limit < 1) {
    return 'Expected a positive integer';
  }
  return max !== undefined && limit > max ? `At most ${max}` : undefined;
}

export function defaultLimit(q: FDBQuery): number | undefined {
  switch (queryTypeOf(q)) {
    case 'traceSearch':
      return DEFAULT_SEARCH_LIMIT;
    case 'logs':
      return DEFAULT_LOGS_LIMIT;
    default:
      return undefined;
  }
}

export function maxLimit(q: FDBQuery): number | undefined {
  return queryTypeOf(q) === 'traceSearch' ? MAX_SEARCH_LIMIT : undefined;
}

/** Whether the query is complete enough to send; incomplete editor states are not run. */
export function isRunnable(q: FDBQuery): boolean {
  switch (queryTypeOf(q)) {
    case 'sql':
      return (q.rawSql ?? '').trim() !== '';
    case 'trace':
      return (q.traceId ?? '').trim() !== '' && traceIdError(q.traceId) === undefined;
    case 'traceSearch': {
      if (limitError(q.limit, MAX_SEARCH_LIMIT) !== undefined) {
        return false;
      }
      const spans = q.spanPredicates ?? [];
      switch (searchModeOf(q)) {
        case 'time':
          return true;
        case 'attribute':
          return spans.length > 0 && predicateComplete(spans[0]);
        case 'sameSpan':
          return spans.length > 0 && spans.every(predicateComplete);
        case 'existential': {
          const logs = q.logPredicates ?? [];
          return spans.length + logs.length > 0 && spans.every(predicateComplete) && logs.every(predicateComplete);
        }
      }
      return false;
    }
    case 'logs':
      if (q.attribute) {
        return predicateComplete(q.attribute) && limitError(q.limit) === undefined;
      }
      return (
        (q.traceId ?? '').trim() !== '' && traceIdError(q.traceId) === undefined && spanIdError(q.spanId) === undefined
      );
  }
}

function wirePredicate<P extends Predicate>(p: P): P {
  if (p.scope === EVENT_NAME_SCOPE) {
    return { ...p, key: EVENT_NAME_SCOPE, type: 'string' };
  }
  return { ...p, key: p.key.trim(), value: p.type === 'string' ? p.value : p.value.trim() };
}

/** The query as sent to the backend: only the fields its type and mode read, IDs normalised. */
export function toWireQuery(q: FDBQuery): FDBQuery {
  const queryType = queryTypeOf(q);
  const out: FDBQuery = { refId: q.refId, queryType };
  if (q.hide !== undefined) {
    out.hide = q.hide;
  }
  if (q.key !== undefined) {
    out.key = q.key;
  }
  if (q.datasource !== undefined) {
    out.datasource = q.datasource;
  }
  const tenant = (q.tenant ?? '').trim();
  if (tenant !== '') {
    out.tenant = tenant;
  }
  switch (queryType) {
    case 'sql':
      out.rawSql = q.rawSql ?? '';
      out.format = q.format ?? SqlFormat.Table;
      break;
    case 'trace':
      out.traceId = normalizeTraceId(q.traceId ?? '');
      break;
    case 'traceSearch': {
      const mode = searchModeOf(q);
      out.mode = mode;
      const spans = (q.spanPredicates ?? []).map(wirePredicate);
      if (mode === 'attribute') {
        out.spanPredicates = spans.slice(0, 1);
      } else if (mode !== 'time') {
        out.spanPredicates = spans;
      }
      if (mode === 'existential') {
        out.logPredicates = (q.logPredicates ?? []).map(wirePredicate);
      }
      if (q.limit !== undefined) {
        out.limit = q.limit;
      }
      break;
    }
    case 'logs':
      if (q.attribute) {
        out.attribute = wirePredicate(q.attribute);
        if (q.limit !== undefined) {
          out.limit = q.limit;
        }
      } else {
        out.traceId = normalizeTraceId(q.traceId ?? '');
        const spanId = (q.spanId ?? '').trim();
        if (spanId !== '') {
          out.spanId = normalizeSpanId(spanId);
        }
      }
      break;
  }
  return out;
}

/** Short text for Explore's query history. */
export function queryDisplayText(q: FDBQuery): string {
  switch (queryTypeOf(q)) {
    case 'sql':
      return q.rawSql ?? '';
    case 'trace':
      return `Trace ${q.traceId ?? ''}`;
    case 'traceSearch': {
      const preds = [...(q.spanPredicates ?? []), ...(q.logPredicates ?? [])]
        .map((p) => `${p.scope}.${p.key}=${p.value}`)
        .join(' ');
      return `Search ${searchModeOf(q)}${preds ? ' ' + preds : ''}`;
    }
    case 'logs':
      if (q.attribute) {
        return `Logs ${q.attribute.scope}.${q.attribute.key}=${q.attribute.value}`;
      }
      return `Logs trace ${q.traceId ?? ''}${q.spanId ? ' span ' + q.spanId : ''}`;
  }
}
