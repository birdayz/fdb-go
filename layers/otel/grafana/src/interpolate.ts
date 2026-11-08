import { ScopedVars } from '@grafana/data';
import { TemplateSrv } from '@grafana/runtime';

import { FDBQuery, Predicate } from './types';

/** A SQL string literal: single quotes doubled. */
export function quoteLiteral(value: string): string {
  return `'${String(value).replace(/'/g, "''")}'`;
}

/**
 * Formats a variable inside SQL as Grafana's SQL datasources do: a single value
 * is spliced as-is, a multi-value or include-all variable becomes a list of
 * quoted literals for IN (...).
 */
export function formatSqlVariable(
  value: string | number | Array<string | number>,
  variable: { multi?: boolean; includeAll?: boolean }
): string {
  if (typeof value === 'number') {
    return String(value);
  }
  if (typeof value === 'string') {
    return variable.multi || variable.includeAll ? quoteLiteral(value) : value;
  }
  return value.map((v) => quoteLiteral(String(v))).join(',');
}

type Replace = Pick<TemplateSrv, 'replace'>;

function interpolatePredicate<P extends Predicate>(p: P, scopedVars: ScopedVars, t: Replace): P {
  return { ...p, key: t.replace(p.key, scopedVars), value: t.replace(p.value, scopedVars) };
}

/** Interpolates template variables in every user-entered text field of the query. */
export function interpolateQuery(q: FDBQuery, scopedVars: ScopedVars, t: Replace): FDBQuery {
  const out: FDBQuery = { ...q };
  if (q.tenant) {
    out.tenant = t.replace(q.tenant, scopedVars);
  }
  if (q.rawSql) {
    out.rawSql = t.replace(q.rawSql, scopedVars, formatSqlVariable);
  }
  if (q.traceId) {
    out.traceId = t.replace(q.traceId, scopedVars);
  }
  if (q.spanId) {
    out.spanId = t.replace(q.spanId, scopedVars);
  }
  if (q.spanPredicates) {
    out.spanPredicates = q.spanPredicates.map((p) => interpolatePredicate(p, scopedVars, t));
  }
  if (q.logPredicates) {
    out.logPredicates = q.logPredicates.map((p) => interpolatePredicate(p, scopedVars, t));
  }
  if (q.attribute) {
    out.attribute = interpolatePredicate(q.attribute, scopedVars, t);
  }
  return out;
}
