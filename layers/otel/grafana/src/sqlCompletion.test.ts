import {
  completeSql,
  completionContext,
  CompletionSources,
  KEYWORDS,
  MACROS,
  normalizeIdentifier,
  quoteIdentifier,
  referencedTables,
} from './sqlCompletion';

describe('identifiers', () => {
  it('quotes anything an unquoted identifier would not name', () => {
    expect(quoteIdentifier('SPANS')).toBe('SPANS');
    expect(quoteIdentifier('START_NS')).toBe('START_NS');
    expect(quoteIdentifier('start_ns')).toBe('"start_ns"');
    expect(quoteIdentifier('a"b')).toBe('"a""b"');
    expect(quoteIdentifier('1X')).toBe('"1X"');
    expect(quoteIdentifier('ORDER')).toBe('"ORDER"');
  });

  it('normalises as the engine does', () => {
    expect(normalizeIdentifier('spans')).toBe('SPANS');
    expect(normalizeIdentifier('"spans"')).toBe('spans');
    expect(normalizeIdentifier('"a""b"')).toBe('a"b');
  });
});

describe('referencedTables', () => {
  it('maps aliases and names to tables', () => {
    const m = referencedTables('SELECT s.x FROM spans s JOIN "Logs" AS l ON s.a = l.a WHERE 1 = 1');
    expect(Object.fromEntries(m)).toEqual({ SPANS: 'SPANS', S: 'SPANS', Logs: 'Logs', L: 'Logs' });
  });

  it('reads comma-joined FROM lists', () => {
    const m = referencedTables('SELECT * FROM a x, b, c AS z WHERE x.k = 1');
    expect(Object.fromEntries(m)).toEqual({ A: 'A', X: 'A', B: 'B', C: 'C', Z: 'C' });
  });

  it('does not take a clause keyword for an alias', () => {
    expect(Object.fromEntries(referencedTables('SELECT * FROM spans WHERE x = 1'))).toEqual({ SPANS: 'SPANS' });
    expect(Object.fromEntries(referencedTables('SELECT * FROM spans JOIN logs ON 1=1'))).toEqual({
      SPANS: 'SPANS',
      LOGS: 'LOGS',
    });
    expect(Object.fromEntries(referencedTables('SELECT * FROM spans ORDER BY 1'))).toEqual({ SPANS: 'SPANS' });
    expect(Object.fromEntries(referencedTables('SELECT * FROM spans AS'))).toEqual({ SPANS: 'SPANS' });
  });

  it('finds nothing without FROM', () => {
    expect(referencedTables('SELECT 1').size).toBe(0);
  });
});

describe('completionContext', () => {
  it.each([
    ['SELECT $__ti', { kind: 'dollar', prefix: '$__ti' }],
    ['WHERE x = $', { kind: 'dollar', prefix: '$' }],
    ['WHERE x = ${sv', { kind: 'dollar', prefix: '${sv' }],
    ['SELECT s.', { kind: 'qualified', qualifier: 'S', prefix: '' }],
    ['SELECT "My".tr', { kind: 'qualified', qualifier: 'My', prefix: 'tr' }],
    ['SELECT * FROM ', { kind: 'table', prefix: '' }],
    ['SELECT * FROM SP', { kind: 'table', prefix: 'SP' }],
    ['SELECT * FROM a JOIN lo', { kind: 'table', prefix: 'lo' }],
    ['SELECT * FROM a, ', { kind: 'table', prefix: '' }],
    ['SELECT * FROM a WHERE b IN (1, ', { kind: 'general', prefix: '' }],
    ['SELECT na', { kind: 'general', prefix: 'na' }],
  ])('%p', (text, want) => {
    expect(completionContext(text)).toEqual(want);
  });
});

describe('completeSql', () => {
  const columnCalls: string[] = [];
  const sources: CompletionSources = {
    tables: async () => ['SPANS', 'logs'],
    columns: async (t) => {
      columnCalls.push(t);
      return t === 'SPANS' ? ['TRACE_ID', 'start_ns'] : t === 'LOGS' ? ['BODY'] : [];
    },
    variables: () => ['svc'],
  };

  beforeEach(() => {
    columnCalls.length = 0;
  });

  it('offers macros as snippets and variables after $', async () => {
    const res = await completeSql('SELECT $__t', 'SELECT $__t', sources);
    expect(res.replaceLength).toBe(4);
    const filter = res.items.find((i) => i.label === '$__timeFilter');
    expect(filter).toMatchObject({ kind: 'macro', snippet: true, insertText: '\\$__timeFilter(${1:start_ns})' });
    expect(res.items.filter((i) => i.kind === 'macro')).toHaveLength(MACROS.length);
    expect(res.items.find((i) => i.kind === 'variable')).toMatchObject({ label: '$svc', insertText: '$svc' });
  });

  it('offers tables, quoted where needed, after FROM', async () => {
    const res = await completeSql('SELECT * FROM s', 'SELECT * FROM s', sources);
    expect(res.replaceLength).toBe(1);
    expect(res.items.map((i) => [i.kind, i.insertText])).toEqual([
      ['table', 'SPANS'],
      ['table', '"logs"'],
    ]);
  });

  it('offers the columns of an aliased table after alias.', async () => {
    const sql = 'SELECT s. FROM SPANS s';
    const res = await completeSql('SELECT s.', sql, sources);
    expect(columnCalls).toEqual(['SPANS']);
    expect(res.items.map((i) => i.insertText)).toEqual(['TRACE_ID', '"start_ns"']);
  });

  it('offers columns of every referenced table plus keywords elsewhere', async () => {
    const sql = 'SELECT  FROM spans JOIN logs ON 1 = 1';
    const res = await completeSql('SELECT ', sql, sources);
    expect(columnCalls.sort()).toEqual(['LOGS', 'SPANS']);
    expect(res.items.filter((i) => i.kind === 'column').map((i) => i.label)).toEqual(['TRACE_ID', 'start_ns', 'BODY']);
    expect(res.items.filter((i) => i.kind === 'keyword')).toHaveLength(KEYWORDS.length);
  });

  it('survives failing resources', async () => {
    const failing: CompletionSources = {
      tables: () => Promise.reject(new Error('down')),
      columns: () => Promise.reject(new Error('down')),
      variables: () => [],
    };
    expect((await completeSql('SELECT * FROM ', 'SELECT * FROM ', failing)).items).toEqual([]);
    expect((await completeSql('SELECT a.', 'SELECT a. FROM t a', failing)).items).toEqual([]);
  });
});
