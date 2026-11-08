import { createDataFrame, FieldType, getDefaultTimeRange, LoadingState, PanelData } from '@grafana/data';

import { errorText, executedQuery, explainSql, framesToText } from './sqlUtil';

describe('executedQuery', () => {
  const data = (frames: Array<{ refId: string; sql?: string }>): PanelData => ({
    state: LoadingState.Done,
    timeRange: getDefaultTimeRange(),
    series: frames.map((f) =>
      createDataFrame({ refId: f.refId, fields: [], meta: f.sql ? { executedQueryString: f.sql } : undefined })
    ),
  });

  it('returns the SQL the backend ran for this query', () => {
    expect(
      executedQuery(
        data([
          { refId: 'A', sql: 'a' },
          { refId: 'B', sql: 'b' },
        ]),
        'B'
      )
    ).toBe('b');
  });

  it('skips frames without it', () => {
    expect(executedQuery(data([{ refId: 'B' }, { refId: 'B', sql: 'b2' }]), 'B')).toBe('b2');
    expect(executedQuery(data([{ refId: 'A', sql: 'a' }]), 'B')).toBeUndefined();
    expect(executedQuery(undefined, 'A')).toBeUndefined();
  });
});

describe('explainSql', () => {
  it('prefixes EXPLAIN once and drops trailing semicolons', () => {
    expect(explainSql(' SELECT 1;; ')).toBe('EXPLAIN SELECT 1');
    expect(explainSql('explain SELECT 1')).toBe('explain SELECT 1');
    expect(explainSql('SELECT explained FROM t')).toBe('EXPLAIN SELECT explained FROM t');
  });
});

describe('framesToText', () => {
  it('prints one line per row, skipping time fields', () => {
    const frame = createDataFrame({
      fields: [
        { name: 't', type: FieldType.time, values: [1, 2] },
        { name: 'plan', type: FieldType.string, values: ['Scan(<,>)', 'Filter'] },
        { name: 'n', type: FieldType.number, values: [1, null] },
        { name: 'o', type: FieldType.other, values: [{ a: 1 }, undefined] },
      ],
    });
    expect(framesToText([frame])).toBe('Scan(<,>)\t1\t{"a":1}\nFilter\t\t');
    expect(framesToText([])).toBe('');
  });
});

describe('errorText', () => {
  it('joins distinct messages', () => {
    expect(errorText(undefined)).toBeUndefined();
    expect(errorText([{ message: 'a' }, { message: 'a' }, { data: { message: 'b' } }, {}])).toBe('a\nb\nquery failed');
  });
});
