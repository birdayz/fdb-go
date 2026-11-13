import { BUDGET_DEFAULTS, BudgetKey, FDBOptions, MAX_SEARCH_LIMIT } from './types';

export type ClusterSource = 'file' | 'string';

export function clusterSourceOf(o: FDBOptions): ClusterSource {
  return o.clusterString && !o.clusterFilePath ? 'string' : 'file';
}

/** Switching the source clears the other field, so the backend never sees both. */
export function withClusterSource(o: FDBOptions, source: ClusterSource): FDBOptions {
  const next = { ...o };
  if (source === 'file') {
    delete next.clusterString;
  } else {
    delete next.clusterFilePath;
  }
  return next;
}

const ADDRESS = /^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9.-]+):\d{1,5}(:tls)?$/;

/** Checks `description:id@host:port[:tls],...` (FDB cluster file syntax; whitespace is ignored as FDB does). */
export function clusterStringError(s: string | undefined): string | undefined {
  const v = (s ?? '').replace(/\s+/g, '');
  if (v === '') {
    return undefined;
  }
  const m = /^([A-Za-z0-9_]+):([A-Za-z0-9]+)@(.+)$/.exec(v);
  if (!m) {
    return 'Expected description:id@host:port[,host:port...]';
  }
  const bad = m[3].split(',').find((a) => !ADDRESS.test(a));
  return bad === undefined ? undefined : `Invalid coordinator address "${bad}"`;
}

/** Names, plus at most one prefix pattern ending in `*`. */
export function allowedSchemasError(list: string[] | undefined): string | undefined {
  const entries = list ?? [];
  if (entries.some((e) => e.indexOf('*') !== -1 && e.indexOf('*') !== e.length - 1)) {
    return '`*` may only end an entry';
  }
  if (entries.filter((e) => e.endsWith('*')).length > 1) {
    return 'At most one prefix pattern';
  }
  return undefined;
}

export function schemaAllowed(name: string, allowed: string[] | undefined): boolean {
  if (!allowed || allowed.length === 0) {
    return true;
  }
  return allowed.some((a) => (a.endsWith('*') ? name.startsWith(a.slice(0, -1)) : a === name));
}

export const BUDGET_MAX: Partial<Record<BudgetKey, number>> = { searchLimit: MAX_SEARCH_LIMIT };

export function budgetError(key: BudgetKey, text: string): string | undefined {
  const t = text.trim();
  if (t === '') {
    return undefined;
  }
  const n = Number(t);
  if (!/^\d+$/.test(t) || !Number.isSafeInteger(n) || n < 1) {
    return 'Expected a positive integer';
  }
  const max = BUDGET_MAX[key];
  return max !== undefined && n > max ? `At most ${max}` : undefined;
}

/** Sets a budget from its input text; empty removes it (the backend default applies). Invalid text changes nothing. */
export function withBudget(o: FDBOptions, key: BudgetKey, text: string): FDBOptions {
  if (budgetError(key, text) !== undefined) {
    return o;
  }
  const next = { ...o };
  if (text.trim() === '') {
    delete next[key];
  } else {
    next[key] = Number(text.trim());
  }
  return next;
}

export function effectiveBudget(o: FDBOptions, key: BudgetKey): number {
  return o[key] ?? BUDGET_DEFAULTS[key];
}

export function formatBytes(n: number): string {
  const units = ['bytes', 'KiB', 'MiB', 'GiB', 'TiB'];
  let v = n;
  let i = 0;
  while (v >= 1024 && i < units.length - 1 && v % 1024 === 0) {
    v /= 1024;
    i++;
  }
  return `${v} ${units[i]}`;
}
