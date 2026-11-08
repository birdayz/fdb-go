import { CustomVariableSupport, DataQueryRequest, DataQueryResponse, MetricFindValue } from '@grafana/data';
import { DataQuery } from '@grafana/schema';
import { from, Observable } from 'rxjs';

import { VariableQueryEditor } from './components/VariableQueryEditor';
import type { DataSource } from './datasource';
import { AttrType, FDBQuery, Signal, SqlFormat } from './types';

export const VARIABLE_KINDS = ['tenants', 'attributeKeys', 'attributeValues', 'sql'] as const;
export type VariableKind = (typeof VARIABLE_KINDS)[number];

export interface FDBVariableQuery extends DataQuery {
  kind: VariableKind;
  tenant?: string;
  signal?: Signal;
  scope?: string;
  key?: string;
  /** attributeKeys: empty means every type. */
  type?: AttrType | '';
  prefix?: string;
  limit?: number;
  rawSql?: string;
}

export const DEFAULT_VARIABLE_LIMIT = 100;

/** Accepts the stored variable query: this model, or a bare SQL string. */
export function migrateVariableQuery(raw: unknown): FDBVariableQuery {
  if (typeof raw === 'string') {
    return raw.trim() === '' ? { refId: 'variable', kind: 'tenants' } : { refId: 'variable', kind: 'sql', rawSql: raw };
  }
  const q = (raw ?? {}) as Partial<FDBVariableQuery>;
  return { ...q, refId: q.refId ?? 'variable', kind: q.kind ?? 'tenants' };
}

/** Values for the resource-backed kinds; sql runs as a normal query instead. */
export async function fetchVariableValues(ds: DataSource, q: FDBVariableQuery): Promise<MetricFindValue[]> {
  const toValues = (xs: string[]) => xs.map((text) => ({ text, value: text }));
  switch (q.kind) {
    case 'tenants':
      return toValues(await ds.getTenants());
    case 'attributeKeys': {
      if (!q.scope) {
        return [];
      }
      const res = await ds.getAttributeKeys({
        tenant: q.tenant,
        signal: q.signal ?? 'span',
        scope: q.scope,
        type: q.type ?? '',
        prefix: q.prefix,
        limit: q.limit ?? DEFAULT_VARIABLE_LIMIT,
      });
      return toValues(res.keys);
    }
    case 'attributeValues': {
      if (!q.scope || !q.key) {
        return [];
      }
      const res = await ds.getAttributeValues({
        tenant: q.tenant,
        signal: q.signal ?? 'span',
        scope: q.scope,
        key: q.key,
        type: q.type || 'string',
        prefix: q.prefix,
        limit: q.limit ?? DEFAULT_VARIABLE_LIMIT,
      });
      return toValues(res.values);
    }
    case 'sql':
      return [];
  }
}

export function variableSqlQuery(q: FDBVariableQuery): FDBQuery {
  return { refId: q.refId, queryType: 'sql', tenant: q.tenant, rawSql: q.rawSql ?? '', format: SqlFormat.Table };
}

export class FDBVariableSupport extends CustomVariableSupport<DataSource, FDBVariableQuery, FDBQuery> {
  editor = VariableQueryEditor;

  constructor(private readonly datasource: DataSource) {
    super();
  }

  query(request: DataQueryRequest<FDBVariableQuery>): Observable<DataQueryResponse> {
    const q = migrateVariableQuery(request.targets[0]);
    if (q.kind === 'sql') {
      // The first string field of the result frame becomes the variable's values.
      return this.datasource.query({ ...request, targets: [variableSqlQuery(q)] });
    }
    return from(fetchVariableValues(this.datasource, q).then((data) => ({ data })));
  }
}
