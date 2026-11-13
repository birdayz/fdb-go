import { CoreApp, DataSourceInstanceSettings, ScopedVars } from '@grafana/data';
import { DataSourceWithBackend, getTemplateSrv } from '@grafana/runtime';

import { interpolateQuery } from './interpolate';
import { defaultQuery, isRunnable, queryDisplayText, toWireQuery } from './query';
import {
  ATTR_TYPES,
  AttrType,
  AttributeKeysResponse,
  AttributeValuesResponse,
  FDBOptions,
  FDBQuery,
  OtelMode,
  Signal,
  TenantsResponse,
} from './types';
import { FDBVariableSupport } from './variables';

export interface AttributeKeysRequest {
  tenant?: string;
  signal: Signal;
  scope: string;
  /** Empty: keys of every type. */
  type?: AttrType | '';
  prefix?: string;
  limit?: number;
}

export interface AttributeValuesRequest {
  tenant?: string;
  signal: Signal;
  scope: string;
  key: string;
  type: AttrType;
  prefix?: string;
  limit?: number;
}

export interface TypedKey {
  key: string;
  types: AttrType[];
}

/** Merges per-type key lists into one sorted list that records each key's types. */
export function mergeKeysByType(results: Array<[AttrType, AttributeKeysResponse]>): {
  keys: TypedKey[];
  truncated: boolean;
} {
  const byKey = new Map<string, AttrType[]>();
  let truncated = false;
  for (const [type, res] of results) {
    truncated = truncated || !!res.truncated;
    for (const key of res.keys ?? []) {
      const types = byKey.get(key) ?? [];
      types.push(type);
      byKey.set(key, types);
    }
  }
  const keys = [...byKey.entries()]
    .sort(([a], [b]) => (a < b ? -1 : a > b ? 1 : 0))
    .map(([key, types]) => ({ key, types }));
  return { keys, truncated };
}

function params(fields: Record<string, string | number | undefined>): Record<string, string | number> {
  const out: Record<string, string | number> = {};
  for (const [k, v] of Object.entries(fields)) {
    if (v !== undefined && v !== '') {
      out[k] = v;
    }
  }
  return out;
}

const COMPLETION_TTL_MS = 5 * 60 * 1000;

// Editors show resource failures in place (the combobox's error row); no global toast per keystroke.
export const QUIET = { showErrorAlert: false };

export class DataSource extends DataSourceWithBackend<FDBQuery, FDBOptions> {
  readonly defaultTenant: string;
  /** off: the datasource serves SQL only, so the editor offers no OTel query types. */
  readonly otelMode: OtelMode;
  private cache = new Map<string, { at: number; value: Promise<unknown> }>();

  constructor(instanceSettings: DataSourceInstanceSettings<FDBOptions>) {
    super(instanceSettings);
    this.defaultTenant = instanceSettings.jsonData.defaultSchema ?? '';
    this.otelMode = instanceSettings.jsonData.otelMode ?? 'auto';
    this.variables = new FDBVariableSupport(this);
  }

  getDefaultQuery(_: CoreApp): Partial<FDBQuery> {
    return defaultQuery();
  }

  filterQuery(query: FDBQuery): boolean {
    return isRunnable(query);
  }

  applyTemplateVariables(query: FDBQuery, scopedVars: ScopedVars): FDBQuery {
    return toWireQuery(interpolateQuery(query, scopedVars, getTemplateSrv()));
  }

  interpolateVariablesInQueries(queries: FDBQuery[], scopedVars: ScopedVars): FDBQuery[] {
    return queries.map((q) => ({ ...interpolateQuery(q, scopedVars, getTemplateSrv()), datasource: this.getRef() }));
  }

  getQueryDisplayText(query: FDBQuery): string {
    return queryDisplayText(query);
  }

  /** Interpolates a tenant that may be a template variable; empty stays empty (the datasource default). */
  resolveTenant(tenant: string | undefined): string {
    return tenant ? getTemplateSrv().replace(tenant) : '';
  }

  private cached<T>(key: string, load: () => Promise<T>): Promise<T> {
    const hit = this.cache.get(key);
    if (hit && Date.now() - hit.at < COMPLETION_TTL_MS) {
      return hit.value as Promise<T>;
    }
    const value = load();
    this.cache.set(key, { at: Date.now(), value });
    value.catch(() => this.cache.delete(key));
    return value;
  }

  async getTenants(): Promise<string[]> {
    const res = await this.getResource<TenantsResponse>('tenants', undefined, QUIET);
    return res?.tenants ?? [];
  }

  async getAttributeKeys(req: AttributeKeysRequest): Promise<{ keys: string[]; truncated: boolean }> {
    const res = await this.getResource<AttributeKeysResponse>(
      'attribute-keys',
      params({
        tenant: this.resolveTenant(req.tenant),
        signal: req.signal,
        scope: req.scope,
        type: req.type,
        prefix: req.prefix,
        limit: req.limit,
      }),
      QUIET
    );
    return { keys: res?.keys ?? [], truncated: !!res?.truncated };
  }

  /** Keys of every type, each tagged with the types it is indexed under. */
  async getTypedAttributeKeys(
    req: Omit<AttributeKeysRequest, 'type'>
  ): Promise<{ keys: TypedKey[]; truncated: boolean }> {
    const settled = await Promise.allSettled(ATTR_TYPES.map((type) => this.getAttributeKeys({ ...req, type })));
    const ok: Array<[AttrType, AttributeKeysResponse]> = [];
    settled.forEach((r, i) => {
      if (r.status === 'fulfilled') {
        ok.push([ATTR_TYPES[i], r.value]);
      }
    });
    if (ok.length === 0 && settled[0].status === 'rejected') {
      throw settled[0].reason;
    }
    return mergeKeysByType(ok);
  }

  async getAttributeValues(req: AttributeValuesRequest): Promise<{ values: string[]; truncated: boolean }> {
    const res = await this.getResource<AttributeValuesResponse>(
      'attribute-values',
      params({
        tenant: this.resolveTenant(req.tenant),
        signal: req.signal,
        scope: req.scope,
        key: getTemplateSrv().replace(req.key),
        type: req.type,
        prefix: req.prefix,
        limit: req.limit,
      }),
      QUIET
    );
    return { values: res?.values ?? [], truncated: !!res?.truncated };
  }

  /** sqlds completion route: tables of the tenant's schema. */
  getTables(tenant: string | undefined): Promise<string[]> {
    const schema = this.resolveTenant(tenant);
    return this.cached(`tables/${schema}`, async () => {
      const res = await this.postResource<string[]>('tables', params({ schema }), QUIET);
      return Array.isArray(res) ? res : [];
    });
  }

  /** sqlds completion route: columns of one table. */
  getColumns(tenant: string | undefined, table: string): Promise<string[]> {
    const schema = this.resolveTenant(tenant);
    return this.cached(`columns/${schema}/${table}`, async () => {
      const res = await this.postResource<string[]>('columns', params({ schema, table }), QUIET);
      return Array.isArray(res) ? res : [];
    });
  }
}
