import { ScopedVars } from '@grafana/data';

import { formatSqlVariable, interpolateQuery, quoteLiteral } from './interpolate';
import { FDBQuery } from './types';

describe('quoteLiteral', () => {
  it('doubles single quotes', () => {
    expect(quoteLiteral("O'Brien")).toBe("'O''Brien'");
    expect(quoteLiteral("'); DROP TABLE SPANS; --")).toBe("'''); DROP TABLE SPANS; --'");
  });
});

describe('formatSqlVariable', () => {
  it('splices a single value as-is', () => {
    expect(formatSqlVariable('checkout', { multi: false, includeAll: false })).toBe('checkout');
  });

  it('quotes a single value of a multi-value or include-all variable', () => {
    expect(formatSqlVariable("a'b", { multi: true })).toBe("'a''b'");
    expect(formatSqlVariable('a', { includeAll: true })).toBe("'a'");
  });

  it('turns several values into a quoted list', () => {
    expect(formatSqlVariable(['a', "b'c", 3], { multi: true })).toBe("'a','b''c','3'");
  });

  it('formats numbers as numbers', () => {
    expect(formatSqlVariable(5, {})).toBe('5');
  });
});

describe('interpolateQuery', () => {
  const vars: Record<string, string> = { $svc: 'checkout', $tenant: 'T7', $trace: 'ABC', $k: 'service.name' };
  const calls: Array<{ text: string; format: unknown }> = [];
  const t = {
    replace: (text?: string, _scoped?: ScopedVars, format?: unknown) => {
      calls.push({ text: text ?? '', format });
      return (text ?? '').replace(/\$\w+/g, (m) => vars[m] ?? m);
    },
  };

  beforeEach(() => {
    calls.length = 0;
  });

  it('interpolates every user-entered text field', () => {
    const q: FDBQuery = {
      refId: 'A',
      queryType: 'traceSearch',
      tenant: '$tenant',
      rawSql: "SELECT * FROM SPANS WHERE name = '$svc'",
      traceId: '$trace',
      spanId: '$trace',
      spanPredicates: [{ scope: 'resource', key: '$k', type: 'string', value: '$svc' }],
      logPredicates: [{ scope: 'log', key: '$k', type: 'string', value: '$svc' }],
      attribute: { scope: 'resource', key: '$k', type: 'string', value: '$svc' },
    };
    const out = interpolateQuery(q, {}, t);
    expect(out.tenant).toBe('T7');
    expect(out.rawSql).toBe("SELECT * FROM SPANS WHERE name = 'checkout'");
    expect(out.traceId).toBe('ABC');
    expect(out.spanId).toBe('ABC');
    const expected = { key: 'service.name', type: 'string', value: 'checkout' };
    expect(out.spanPredicates).toEqual([{ scope: 'resource', ...expected }]);
    expect(out.logPredicates).toEqual([{ scope: 'log', ...expected }]);
    expect(out.attribute).toEqual({ scope: 'resource', ...expected });
    expect(q.tenant).toBe('$tenant');
  });

  it('formats SQL with the SQL formatter and nothing else', () => {
    interpolateQuery({ refId: 'A', rawSql: 'x', tenant: 't' }, {}, t);
    expect(calls.find((c) => c.text === 'x')?.format).toBe(formatSqlVariable);
    expect(calls.find((c) => c.text === 't')?.format).toBeUndefined();
  });

  it('leaves absent fields absent', () => {
    expect(interpolateQuery({ refId: 'A', queryType: 'trace' }, {}, t)).toEqual({ refId: 'A', queryType: 'trace' });
  });
});
