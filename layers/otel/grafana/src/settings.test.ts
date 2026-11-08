import {
  allowedSchemasError,
  budgetError,
  clusterSourceOf,
  clusterStringError,
  effectiveBudget,
  formatBytes,
  schemaAllowed,
  withBudget,
  withClusterSource,
} from './settings';
import { BUDGET_DEFAULTS, FDBOptions } from './types';

describe('cluster source', () => {
  it('defaults to the cluster file', () => {
    expect(clusterSourceOf({})).toBe('file');
    expect(clusterSourceOf({ clusterFilePath: '/x' })).toBe('file');
    expect(clusterSourceOf({ clusterString: 'a:b@1.2.3.4:4500' })).toBe('string');
  });

  it('clears the other field on switch', () => {
    const o: FDBOptions = { clusterFilePath: '/x', clusterString: 'a:b@h:1', database: 'd' };
    expect(withClusterSource(o, 'file')).toEqual({ clusterFilePath: '/x', database: 'd' });
    expect(withClusterSource(o, 'string')).toEqual({ clusterString: 'a:b@h:1', database: 'd' });
  });

  it.each([
    ['docker:docker@127.0.0.1:4500', undefined],
    ['docker:docker@fdb:4500', undefined],
    ['d_1:Ab9@10.0.0.1:4500:tls,10.0.0.2:4500:tls', undefined],
    ['d:i@[::1]:4500', undefined],
    [' d : i @ h:1 ', undefined],
    ['', undefined],
    ['docker@127.0.0.1:4500', 'Expected description:id@host:port[,host:port...]'],
    ['d:i@127.0.0.1', 'Invalid coordinator address "127.0.0.1"'],
    ['d:i@h:1,h:x', 'Invalid coordinator address "h:x"'],
    ['d:i-x@h:1', 'Expected description:id@host:port[,host:port...]'],
  ])('clusterStringError(%p)', (s, want) => {
    expect(clusterStringError(s)).toBe(want);
  });
});

describe('allowed schemas', () => {
  it('accepts names and one trailing prefix', () => {
    expect(allowedSchemasError(undefined)).toBeUndefined();
    expect(allowedSchemasError(['T0', 'T1', 'X*'])).toBeUndefined();
    expect(allowedSchemasError(['T*', 'X*'])).toBe('At most one prefix pattern');
    expect(allowedSchemasError(['T*X'])).toBe('`*` may only end an entry');
    expect(allowedSchemasError(['**'])).toBe('`*` may only end an entry');
  });

  it('matches names exactly and prefixes by prefix', () => {
    expect(schemaAllowed('T5', undefined)).toBe(true);
    expect(schemaAllowed('T5', [])).toBe(true);
    expect(schemaAllowed('T5', ['T*'])).toBe(true);
    expect(schemaAllowed('T5', ['T0', 'T1'])).toBe(false);
    expect(schemaAllowed('T1', ['T0', 'T1'])).toBe(true);
    expect(schemaAllowed('T10', ['T1'])).toBe(false);
  });
});

describe('budgets', () => {
  it('validates positive integers and the search cap', () => {
    expect(budgetError('sqlMaxRows', '')).toBeUndefined();
    expect(budgetError('sqlMaxRows', '5000')).toBeUndefined();
    expect(budgetError('sqlMaxRows', '0')).toBe('Expected a positive integer');
    expect(budgetError('sqlMaxRows', '-1')).toBe('Expected a positive integer');
    expect(budgetError('sqlMaxRows', '1.5')).toBe('Expected a positive integer');
    expect(budgetError('sqlMaxRows', '1e3')).toBe('Expected a positive integer');
    expect(budgetError('sqlMaxRows', '99999999999999999999')).toBe('Expected a positive integer');
    expect(budgetError('searchLimit', '1000')).toBeUndefined();
    expect(budgetError('searchLimit', '1001')).toBe('At most 1000');
  });

  it('sets, removes and ignores invalid text', () => {
    const o: FDBOptions = { database: 'd', sqlMaxRows: 10 };
    expect(withBudget(o, 'sqlMaxRows', ' 20 ')).toEqual({ database: 'd', sqlMaxRows: 20 });
    expect(withBudget(o, 'sqlMaxRows', '')).toEqual({ database: 'd' });
    expect(withBudget(o, 'sqlMaxRows', 'abc')).toBe(o);
  });

  it('falls back to the contract defaults', () => {
    expect(effectiveBudget({}, 'traceMaxBytes')).toBe(67108864);
    expect(effectiveBudget({ traceMaxBytes: 5 }, 'traceMaxBytes')).toBe(5);
    expect(BUDGET_DEFAULTS).toEqual({
      queryTimeoutSeconds: 30,
      transactionTimeoutMs: 5000,
      sqlPageRows: 10000,
      sqlPageBytes: 16777216,
      sqlMaxRows: 100000,
      sqlStatementMemoryBytes: 268435456,
      traceMaxSpans: 10000,
      traceMaxBytes: 67108864,
      searchLimit: 20,
      searchMaxCandidates: 20000,
      searchMaxBytes: 4194304,
      logsLimit: 1000,
    });
  });

  it('formats byte sizes in binary units', () => {
    expect(formatBytes(16777216)).toBe('16 MiB');
    expect(formatBytes(268435456)).toBe('256 MiB');
    expect(formatBytes(4194304)).toBe('4 MiB');
    expect(formatBytes(1536)).toBe('1536 bytes');
    expect(formatBytes(3 * 1024 * 1024 * 1024)).toBe('3 GiB');
    expect(formatBytes(1000)).toBe('1000 bytes');
  });
});
