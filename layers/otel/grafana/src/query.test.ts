import {
  defaultQuery,
  emptyLogPredicate,
  emptySpanPredicate,
  hasTemplateVariable,
  isRunnable,
  limitError,
  logsByOf,
  normalizeSpanId,
  normalizeTraceId,
  predicateComplete,
  queryDisplayText,
  spanIdError,
  toWireQuery,
  traceIdError,
  valueError,
  withLogsBy,
  withQueryType,
  withScope,
  withSearchMode,
  withTenant,
} from './query';
import { FDBQuery, LogPredicate, SpanPredicate, SqlFormat } from './types';

const span = (key: string, value: string, type: SpanPredicate['type'] = 'string'): SpanPredicate => ({
  scope: 'span',
  key,
  type,
  value,
});
const log = (key: string, value: string): LogPredicate => ({ scope: 'log', key, type: 'string', value });
const TRACE = '0af7651916cd43dd8448eb211c80319c';

describe('defaults', () => {
  it('starts as a SQL table query', () => {
    expect(defaultQuery()).toEqual({ queryType: 'sql', rawSql: '', format: SqlFormat.Table });
  });
});

describe('withQueryType', () => {
  it('fills the defaults of the new type', () => {
    const q: FDBQuery = { refId: 'A' };
    expect(withQueryType(q, 'sql')).toEqual({ refId: 'A', queryType: 'sql', rawSql: '', format: SqlFormat.Table });
    expect(withQueryType(q, 'trace')).toEqual({ refId: 'A', queryType: 'trace', traceId: '' });
    expect(withQueryType(q, 'traceSearch')).toEqual({
      refId: 'A',
      queryType: 'traceSearch',
      mode: 'sameSpan',
      spanPredicates: [emptySpanPredicate()],
    });
    expect(withQueryType(q, 'logs')).toEqual({ refId: 'A', queryType: 'logs', traceId: '' });
  });

  it('keeps other types fields but resets limit, whose default differs per type', () => {
    const q: FDBQuery = { refId: 'A', queryType: 'traceSearch', mode: 'time', limit: 50, rawSql: 'SELECT 1' };
    const logs = withQueryType(q, 'logs');
    expect(logs.limit).toBeUndefined();
    expect(logs.rawSql).toBe('SELECT 1');
    expect(withQueryType(withQueryType(q, 'sql'), 'sql').rawSql).toBe('SELECT 1');
  });

  it('keeps limit when the type does not change', () => {
    expect(withQueryType({ refId: 'A', queryType: 'logs', limit: 7 }, 'logs').limit).toBe(7);
  });

  it('carries a trace ID into logs by trace', () => {
    const q = withQueryType({ refId: 'A', queryType: 'trace', traceId: TRACE }, 'logs');
    expect(logsByOf(q)).toBe('trace');
    expect(q.traceId).toBe(TRACE);
  });

  it('returns to logs by attribute when no trace ID competes', () => {
    const q = withQueryType({ refId: 'A', queryType: 'sql', attribute: log('k', 'v') }, 'logs');
    expect(logsByOf(q)).toBe('attribute');
    expect(q.traceId).toBeUndefined();
  });

  it('prefers the trace when both a trace ID and an attribute are present', () => {
    const q = withQueryType({ refId: 'A', traceId: TRACE, attribute: log('k', 'v') }, 'logs');
    expect(q.attribute).toBeUndefined();
    expect(q.traceId).toBe(TRACE);
  });
});

describe('withTenant', () => {
  it('sets a tenant and removes an empty one', () => {
    expect(withTenant({ refId: 'A' }, 'T1')).toEqual({ refId: 'A', tenant: 'T1' });
    expect(withTenant({ refId: 'A', tenant: 'T1' }, undefined)).toEqual({ refId: 'A' });
    expect(withTenant({ refId: 'A', tenant: 'T1' }, '')).toEqual({ refId: 'A' });
  });
});

describe('withSearchMode', () => {
  const base: FDBQuery = {
    refId: 'A',
    queryType: 'traceSearch',
    spanPredicates: [span('a', '1'), span('b', '2')],
    logPredicates: [log('c', '3')],
  };

  it('time reads no predicates', () => {
    const q = withSearchMode(base, 'time');
    expect(q.mode).toBe('time');
    expect(q.spanPredicates).toBeUndefined();
    expect(q.logPredicates).toBeUndefined();
  });

  it('attribute keeps exactly the first span predicate', () => {
    const q = withSearchMode(base, 'attribute');
    expect(q.spanPredicates).toEqual([span('a', '1')]);
    expect(q.logPredicates).toBeUndefined();
    expect(withSearchMode({ refId: 'A' }, 'attribute').spanPredicates).toEqual([emptySpanPredicate()]);
  });

  it('sameSpan keeps span predicates and drops log predicates', () => {
    const q = withSearchMode(base, 'sameSpan');
    expect(q.spanPredicates).toHaveLength(2);
    expect(q.logPredicates).toBeUndefined();
    expect(withSearchMode({ refId: 'A' }, 'sameSpan').spanPredicates).toEqual([emptySpanPredicate()]);
  });

  it('existential keeps both kinds', () => {
    const q = withSearchMode(base, 'existential');
    expect(q.spanPredicates).toHaveLength(2);
    expect(q.logPredicates).toEqual([log('c', '3')]);
  });

  it('existential with only log predicates adds no empty span predicate', () => {
    const q = withSearchMode({ refId: 'A', logPredicates: [log('c', '3')] }, 'existential');
    expect(q.spanPredicates).toEqual([]);
    expect(withSearchMode({ refId: 'A' }, 'existential').spanPredicates).toEqual([emptySpanPredicate()]);
  });
});

describe('withLogsBy', () => {
  it('attribute removes the trace and span IDs', () => {
    const q = withLogsBy({ refId: 'A', traceId: TRACE, spanId: 'b7ad6b7169203331' }, 'attribute');
    expect(q).toEqual({ refId: 'A', attribute: emptyLogPredicate() });
  });

  it('trace removes the attribute', () => {
    expect(withLogsBy({ refId: 'A', attribute: log('k', 'v') }, 'trace')).toEqual({ refId: 'A', traceId: '' });
  });
});

describe('withScope', () => {
  it('fixes key and type for event_name', () => {
    expect(withScope<LogPredicate>({ scope: 'log', key: 'x', type: 'int', value: '5' }, 'event_name')).toEqual({
      scope: 'event_name',
      key: 'event_name',
      type: 'string',
      value: '5',
    });
  });

  it('clears the fixed key when leaving event_name', () => {
    const p: LogPredicate = { scope: 'event_name', key: 'event_name', type: 'string', value: 'v' };
    expect(withScope(p, 'resource')).toEqual({ scope: 'resource', key: '', type: 'string', value: 'v' });
  });

  it('only changes the scope otherwise', () => {
    expect(withScope(span('k', 'v'), 'resource')).toEqual({ ...span('k', 'v'), scope: 'resource' });
  });
});

describe('validation', () => {
  it.each([
    ['int', '42', undefined],
    ['int', '-9223372036854775808', undefined],
    ['int', '9223372036854775807', undefined],
    ['int', '9223372036854775808', 'Outside the 64-bit integer range'],
    ['int', '1.5', 'Expected an integer'],
    ['double', '1.5e3', undefined],
    ['double', '.5', undefined],
    ['double', 'abc', 'Expected a finite number'],
    ['double', '1e999', 'Expected a finite number'],
    ['bool', 'true', undefined],
    ['bool', 'yes', 'Expected true or false'],
    ['string', 'anything at all', undefined],
    ['int', '$var', undefined],
    ['int', '', undefined],
  ] as const)('valueError(%s, %p) = %p', (type, value, want) => {
    expect(valueError(type, value)).toBe(want);
  });

  it('recognises template variables', () => {
    expect(hasTemplateVariable('$svc')).toBe(true);
    expect(hasTemplateVariable('${svc:raw}')).toBe(true);
    expect(hasTemplateVariable('[[svc]]')).toBe(true);
    expect(hasTemplateVariable('price $')).toBe(false);
  });

  it('checks trace and span IDs', () => {
    expect(traceIdError(TRACE)).toBeUndefined();
    expect(traceIdError(TRACE.toUpperCase())).toBeUndefined();
    expect(traceIdError('')).toBeUndefined();
    expect(traceIdError('$trace')).toBeUndefined();
    expect(traceIdError(TRACE + '0')).toBeDefined();
    expect(traceIdError('xyz')).toBeDefined();
    expect(spanIdError('b7ad6b7169203331')).toBeUndefined();
    expect(spanIdError('b7ad6b71692033310')).toBeDefined();
  });

  it('checks limits', () => {
    expect(limitError(undefined, 1000)).toBeUndefined();
    expect(limitError(1000, 1000)).toBeUndefined();
    expect(limitError(1001, 1000)).toBe('At most 1000');
    expect(limitError(0)).toBeDefined();
    expect(limitError(2.5)).toBeDefined();
    expect(limitError(Number.NaN)).toBeDefined();
  });

  it('a predicate is complete with a key and a valid value', () => {
    expect(predicateComplete(span('k', 'v'))).toBe(true);
    expect(predicateComplete(span('', 'v'))).toBe(false);
    expect(predicateComplete(span('k', ' '))).toBe(false);
    expect(predicateComplete(span('k', 'x', 'int'))).toBe(false);
  });
});

describe('isRunnable', () => {
  it.each<[string, FDBQuery, boolean]>([
    ['empty sql', { refId: 'A', queryType: 'sql', rawSql: '  ' }, false],
    ['sql', { refId: 'A', queryType: 'sql', rawSql: 'SELECT 1' }, true],
    ['missing type means sql', { refId: 'A', rawSql: 'SELECT 1' }, true],
    ['trace', { refId: 'A', queryType: 'trace', traceId: TRACE }, true],
    ['trace variable', { refId: 'A', queryType: 'trace', traceId: '$trace' }, true],
    ['empty trace', { refId: 'A', queryType: 'trace', traceId: '' }, false],
    ['bad trace', { refId: 'A', queryType: 'trace', traceId: 'nothex' }, false],
    ['time search', { refId: 'A', queryType: 'traceSearch', mode: 'time' }, true],
    ['time search bad limit', { refId: 'A', queryType: 'traceSearch', mode: 'time', limit: 5000 }, false],
    ['sameSpan', { refId: 'A', queryType: 'traceSearch', spanPredicates: [span('k', 'v')] }, true],
    [
      'sameSpan with one incomplete',
      { refId: 'A', queryType: 'traceSearch', spanPredicates: [span('k', 'v'), span('', '')] },
      false,
    ],
    ['sameSpan without predicates', { refId: 'A', queryType: 'traceSearch', spanPredicates: [] }, false],
    [
      'attribute reads only the first',
      { refId: 'A', queryType: 'traceSearch', mode: 'attribute', spanPredicates: [span('k', 'v'), span('', '')] },
      true,
    ],
    [
      'existential logs only',
      { refId: 'A', queryType: 'traceSearch', mode: 'existential', spanPredicates: [], logPredicates: [log('k', 'v')] },
      true,
    ],
    [
      'existential with an incomplete log predicate',
      {
        refId: 'A',
        queryType: 'traceSearch',
        mode: 'existential',
        spanPredicates: [span('k', 'v')],
        logPredicates: [log('k', '')],
      },
      false,
    ],
    ['existential empty', { refId: 'A', queryType: 'traceSearch', mode: 'existential' }, false],
    ['logs by trace', { refId: 'A', queryType: 'logs', traceId: TRACE, spanId: 'b7ad6b7169203331' }, true],
    ['logs bad span', { refId: 'A', queryType: 'logs', traceId: TRACE, spanId: 'zz' }, false],
    ['logs by attribute', { refId: 'A', queryType: 'logs', attribute: log('k', 'v') }, true],
    ['logs by attribute bad limit', { refId: 'A', queryType: 'logs', attribute: log('k', 'v'), limit: 0 }, false],
    ['logs by incomplete attribute', { refId: 'A', queryType: 'logs', attribute: log('k', '') }, false],
  ])('%s', (_, q, want) => {
    expect(isRunnable(q)).toBe(want);
  });
});

describe('IDs on the wire', () => {
  it('lowercases and zero-extends 64-bit IDs', () => {
    expect(normalizeTraceId(' 8448EB211C80319C ')).toBe('00000000000000008448eb211c80319c');
    expect(normalizeTraceId(TRACE.toUpperCase())).toBe(TRACE);
    expect(normalizeSpanId('ABC')).toBe('0000000000000abc');
  });

  it('leaves invalid IDs for the backend to refuse', () => {
    expect(normalizeTraceId('not-hex')).toBe('not-hex');
    expect(normalizeTraceId(TRACE + 'ff')).toBe(TRACE + 'ff');
  });
});

describe('toWireQuery', () => {
  const common = { refId: 'A', datasource: { type: 'birdayz-foundationdb-datasource', uid: 'u' }, hide: false };

  it('sql sends rawSql and format only', () => {
    const q: FDBQuery = { ...common, queryType: 'sql', rawSql: 'SELECT 1', traceId: TRACE, limit: 3, tenant: ' T1 ' };
    expect(toWireQuery(q)).toEqual({
      ...common,
      queryType: 'sql',
      tenant: 'T1',
      rawSql: 'SELECT 1',
      format: SqlFormat.Table,
    });
  });

  it('defaults a missing type to sql and omits an empty tenant', () => {
    expect(toWireQuery({ refId: 'A', rawSql: 'x', tenant: '', format: SqlFormat.Logs })).toEqual({
      refId: 'A',
      queryType: 'sql',
      rawSql: 'x',
      format: SqlFormat.Logs,
    });
  });

  it('trace sends the normalised trace ID', () => {
    expect(toWireQuery({ refId: 'A', queryType: 'trace', traceId: TRACE.toUpperCase(), rawSql: 'x' })).toEqual({
      refId: 'A',
      queryType: 'trace',
      traceId: TRACE,
    });
  });

  it('traceSearch sends what each mode reads', () => {
    const q: FDBQuery = {
      refId: 'A',
      queryType: 'traceSearch',
      spanPredicates: [span(' k ', ' 5 ', 'int'), span('s', ' padded ')],
      logPredicates: [log('l', 'v')],
      limit: 30,
    };
    expect(toWireQuery({ ...q, mode: 'sameSpan' })).toEqual({
      refId: 'A',
      queryType: 'traceSearch',
      mode: 'sameSpan',
      spanPredicates: [span('k', '5', 'int'), span('s', ' padded ')],
      limit: 30,
    });
    expect(toWireQuery({ ...q, mode: 'attribute' }).spanPredicates).toEqual([span('k', '5', 'int')]);
    expect(toWireQuery({ ...q, mode: 'existential' }).logPredicates).toEqual([log('l', 'v')]);
    const time = toWireQuery({ ...q, mode: 'time' });
    expect(time.spanPredicates).toBeUndefined();
    expect(time.logPredicates).toBeUndefined();
    expect(toWireQuery({ refId: 'A', queryType: 'traceSearch' }).mode).toBe('sameSpan');
  });

  it('event_name predicates carry the fixed key and type', () => {
    const q: FDBQuery = {
      refId: 'A',
      queryType: 'traceSearch',
      mode: 'existential',
      logPredicates: [{ scope: 'event_name', key: '', type: 'int', value: 'gen_ai.done' }],
    };
    expect(toWireQuery(q).logPredicates).toEqual([
      { scope: 'event_name', key: 'event_name', type: 'string', value: 'gen_ai.done' },
    ]);
  });

  it('logs by trace sends the IDs, never the attribute or limit', () => {
    expect(
      toWireQuery({ refId: 'A', queryType: 'logs', traceId: TRACE, spanId: ' B7AD6B7169203331 ', limit: 5 })
    ).toEqual({ refId: 'A', queryType: 'logs', traceId: TRACE, spanId: 'b7ad6b7169203331' });
    expect(toWireQuery({ refId: 'A', queryType: 'logs', traceId: TRACE, spanId: '' }).spanId).toBeUndefined();
  });

  it('logs by attribute sends the attribute and limit, never the IDs', () => {
    expect(toWireQuery({ refId: 'A', queryType: 'logs', attribute: log('k', 'v'), limit: 10, traceId: TRACE })).toEqual(
      { refId: 'A', queryType: 'logs', attribute: log('k', 'v'), limit: 10 }
    );
  });
});

describe('queryDisplayText', () => {
  it('summarises each type', () => {
    expect(queryDisplayText({ refId: 'A', rawSql: 'SELECT 1' })).toBe('SELECT 1');
    expect(queryDisplayText({ refId: 'A', queryType: 'trace', traceId: TRACE })).toBe(`Trace ${TRACE}`);
    expect(
      queryDisplayText({ refId: 'A', queryType: 'traceSearch', mode: 'attribute', spanPredicates: [span('k', 'v')] })
    ).toBe('Search attribute span.k=v');
    expect(queryDisplayText({ refId: 'A', queryType: 'logs', traceId: TRACE, spanId: 'ab' })).toBe(
      `Logs trace ${TRACE} span ab`
    );
    expect(queryDisplayText({ refId: 'A', queryType: 'logs', attribute: log('k', 'v') })).toBe('Logs log.k=v');
  });
});
