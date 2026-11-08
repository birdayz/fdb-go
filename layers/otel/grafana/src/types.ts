import { DataSourceJsonData } from '@grafana/data';
import { DataQuery } from '@grafana/schema';

// Query model shared with the Go backend (grafana_contract.md, "Query model").

export const QUERY_TYPES = ['sql', 'trace', 'traceSearch', 'logs'] as const;
export type QueryType = (typeof QUERY_TYPES)[number];

export const SEARCH_MODES = ['sameSpan', 'existential', 'attribute', 'time'] as const;
export type SearchMode = (typeof SEARCH_MODES)[number];

export const SPAN_SCOPES = ['resource', 'scope', 'span'] as const;
export type SpanScope = (typeof SPAN_SCOPES)[number];

export const LOG_SCOPES = ['resource', 'scope', 'log', 'event_name'] as const;
export type LogScope = (typeof LOG_SCOPES)[number];

export const ATTR_TYPES = ['string', 'int', 'double', 'bool'] as const;
export type AttrType = (typeof ATTR_TYPES)[number];

/** Which attribute indexes an autocomplete request reads: SPANS_*_ATTR or LOGS_*_ATTR. */
export type Signal = 'span' | 'log';

/** The log "scope" that matches the indexed event name; key and type are fixed. */
export const EVENT_NAME_SCOPE = 'event_name';

export interface Predicate<S extends string = string> {
  scope: S;
  key: string;
  type: AttrType;
  value: string;
}
export type SpanPredicate = Predicate<SpanScope>;
export type LogPredicate = Predicate<LogScope>;

/** sqlutil.FormatQueryOption */
export enum SqlFormat {
  TimeSeries = 0,
  Table = 1,
  Logs = 2,
}

export interface FDBQuery extends DataQuery {
  queryType?: QueryType;
  /** Schema name; empty means the datasource's default schema. */
  tenant?: string;

  // sql
  rawSql?: string;
  format?: SqlFormat;

  // trace, logs by trace
  traceId?: string;
  spanId?: string;

  // traceSearch
  mode?: SearchMode;
  spanPredicates?: SpanPredicate[];
  logPredicates?: LogPredicate[];
  /** traceSearch and logs by attribute; absent means the backend default. */
  limit?: number;

  // logs by attribute
  attribute?: LogPredicate;
}

export const DEFAULT_SEARCH_LIMIT = 20;
export const MAX_SEARCH_LIMIT = 1000;
export const DEFAULT_LOGS_LIMIT = 1000;

// Settings (grafana_contract.md, "Settings").

export const TLS_MODES = ['off', 'cluster', 'config'] as const;
export type TlsMode = (typeof TLS_MODES)[number];

export const OTEL_MODES = ['auto', 'on', 'off'] as const;
export type OtelMode = (typeof OTEL_MODES)[number];

export interface FDBOptions extends DataSourceJsonData {
  clusterFilePath?: string;
  clusterString?: string;
  tlsMode?: TlsMode;
  tlsCACert?: string;
  tlsVerifyPeers?: string;
  enableSecureSocksProxy?: boolean;
  database?: string;
  defaultSchema?: string;
  allowedSchemas?: string[];
  otelMode?: OtelMode;
  queryTimeoutSeconds?: number;
  transactionTimeoutMs?: number;
  sqlPageRows?: number;
  sqlPageBytes?: number;
  sqlMaxRows?: number;
  sqlStatementMemoryBytes?: number;
  traceMaxSpans?: number;
  traceMaxBytes?: number;
  searchLimit?: number;
  searchMaxCandidates?: number;
  searchMaxBytes?: number;
  logsLimit?: number;
  transactionTags?: string[];
}

export interface FDBSecureJsonData {
  tlsClientCert?: string;
  tlsClientKey?: string;
}

export type BudgetKey =
  | 'queryTimeoutSeconds'
  | 'transactionTimeoutMs'
  | 'sqlPageRows'
  | 'sqlPageBytes'
  | 'sqlMaxRows'
  | 'sqlStatementMemoryBytes'
  | 'traceMaxSpans'
  | 'traceMaxBytes'
  | 'searchLimit'
  | 'searchMaxCandidates'
  | 'searchMaxBytes'
  | 'logsLimit';

/** Backend defaults; an absent jsonData field means this value. */
export const BUDGET_DEFAULTS: Record<BudgetKey, number> = {
  queryTimeoutSeconds: 30,
  transactionTimeoutMs: 5000,
  sqlPageRows: 10000,
  sqlPageBytes: 16777216,
  sqlMaxRows: 100000,
  sqlStatementMemoryBytes: 268435456,
  traceMaxSpans: 10000,
  traceMaxBytes: 67108864,
  searchLimit: DEFAULT_SEARCH_LIMIT,
  searchMaxCandidates: 20000,
  searchMaxBytes: 4194304,
  logsLimit: DEFAULT_LOGS_LIMIT,
};

// Resources (grafana_contract.md, "Resources").

export interface TenantsResponse {
  tenants?: string[];
}
export interface AttributeKeysResponse {
  keys?: string[];
  truncated?: boolean;
}
export interface AttributeValuesResponse {
  values?: string[];
  truncated?: boolean;
}
