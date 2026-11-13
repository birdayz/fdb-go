import { CoreApp } from '@grafana/data';

import { mergeKeysByType, QUIET } from './datasource';
import { makeDataSource } from './testUtils';
import { FDBQuery, SqlFormat } from './types';
import { FDBVariableSupport } from './variables';

jest.mock('@grafana/runtime', () => ({
  ...jest.requireActual('@grafana/runtime'),
  getTemplateSrv: () => jest.requireActual('./testUtils').mockTemplateSrv,
}));

describe('mergeKeysByType', () => {
  it('records each key with every type it is indexed under, sorted by key', () => {
    expect(
      mergeKeysByType([
        ['string', { keys: ['service.name', 'http.method'] }],
        ['int', { keys: ['http.status_code', 'http.method'], truncated: true }],
        ['bool', {}],
      ])
    ).toEqual({
      keys: [
        { key: 'http.method', types: ['string', 'int'] },
        { key: 'http.status_code', types: ['int'] },
        { key: 'service.name', types: ['string'] },
      ],
      truncated: true,
    });
    expect(mergeKeysByType([])).toEqual({ keys: [], truncated: false });
  });
});

describe('DataSource', () => {
  it('reads the default tenant and installs variable support', () => {
    const ds = makeDataSource({ defaultSchema: 'T3' });
    expect(ds.defaultTenant).toBe('T3');
    expect(ds.variables).toBeInstanceOf(FDBVariableSupport);
    expect(makeDataSource({}).defaultTenant).toBe('');
  });

  it('defaults new queries to a SQL table query', () => {
    expect(makeDataSource().getDefaultQuery(CoreApp.PanelEditor)).toEqual({
      queryType: 'sql',
      rawSql: '',
      format: SqlFormat.Table,
    });
  });

  it('filters out incomplete queries', () => {
    const ds = makeDataSource();
    expect(ds.filterQuery({ refId: 'A', queryType: 'sql', rawSql: '' })).toBe(false);
    expect(ds.filterQuery({ refId: 'A', queryType: 'sql', rawSql: 'SELECT 1' })).toBe(true);
  });

  it('interpolates, then sends only the fields of the query type', () => {
    const q: FDBQuery = { refId: 'A', queryType: 'trace', traceId: '$trace', tenant: '$tenant', rawSql: 'stale' };
    expect(makeDataSource().applyTemplateVariables(q, {})).toEqual({
      refId: 'A',
      queryType: 'trace',
      tenant: 'T7',
      traceId: '00000000000000000000000000abcdef',
    });
  });

  it('quotes SQL variables with the SQL formatter', () => {
    const q: FDBQuery = { refId: 'A', queryType: 'sql', rawSql: "SELECT * FROM SPANS WHERE s = '$svc'" };
    expect(makeDataSource().applyTemplateVariables(q, {}).rawSql).toBe("SELECT * FROM SPANS WHERE s = 'checkout'");
  });

  it('interpolates for Explore and tags the datasource', () => {
    const [q] = makeDataSource().interpolateVariablesInQueries([{ refId: 'A', tenant: '$tenant', rawSql: 'x' }], {});
    expect(q.tenant).toBe('T7');
    expect(q.datasource).toEqual({ type: 'birdayz-foundationdb-datasource', uid: 'fdb-uid' });
  });

  it('describes queries for history', () => {
    expect(makeDataSource().getQueryDisplayText({ refId: 'A', rawSql: 'SELECT 2' })).toBe('SELECT 2');
  });
});

describe('resources', () => {
  it('lists tenants', async () => {
    const ds = makeDataSource();
    const get = jest.spyOn(ds, 'getResource').mockResolvedValue({ tenants: ['T0', 'T1'] });
    expect(await ds.getTenants()).toEqual(['T0', 'T1']);
    expect(get).toHaveBeenCalledWith('tenants', undefined, QUIET);
    expect(QUIET).toEqual({ showErrorAlert: false });
    get.mockResolvedValue({});
    expect(await ds.getTenants()).toEqual([]);
  });

  it('asks for attribute keys with the contract parameters, omitting empty ones', async () => {
    const ds = makeDataSource();
    const get = jest.spyOn(ds, 'getResource').mockResolvedValue({ keys: ['a'], truncated: true });
    expect(
      await ds.getAttributeKeys({
        tenant: '$tenant',
        signal: 'log',
        scope: 'resource',
        type: '',
        prefix: 'se',
        limit: 5,
      })
    ).toEqual({ keys: ['a'], truncated: true });
    expect(get).toHaveBeenCalledWith(
      'attribute-keys',
      {
        tenant: 'T7',
        signal: 'log',
        scope: 'resource',
        prefix: 'se',
        limit: 5,
      },
      QUIET
    );
    get.mockResolvedValue({});
    expect(await ds.getAttributeKeys({ signal: 'span', scope: 'span', type: 'int' })).toEqual({
      keys: [],
      truncated: false,
    });
    expect(get).toHaveBeenLastCalledWith('attribute-keys', { signal: 'span', scope: 'span', type: 'int' }, QUIET);
  });

  it('merges typed key lists from one request per type', async () => {
    const ds = makeDataSource();
    const get = jest.spyOn(ds, 'getResource').mockImplementation(async (_path, params) => {
      const type = (params as { type: string }).type;
      if (type === 'double') {
        throw new Error('down');
      }
      return { keys: type === 'string' || type === 'int' ? ['k'] : [] };
    });
    const res = await ds.getTypedAttributeKeys({ signal: 'span', scope: 'span', prefix: 'k' });
    expect(get.mock.calls.map((c) => (c[1] as { type: string }).type).sort()).toEqual([
      'bool',
      'double',
      'int',
      'string',
    ]);
    expect(res).toEqual({ keys: [{ key: 'k', types: ['string', 'int'] }], truncated: false });
  });

  it('fails typed keys only when every request fails', async () => {
    const ds = makeDataSource();
    jest.spyOn(ds, 'getResource').mockRejectedValue(new Error('no cluster'));
    await expect(ds.getTypedAttributeKeys({ signal: 'span', scope: 'span' })).rejects.toThrow('no cluster');
  });

  it('asks for attribute values with the contract parameters', async () => {
    const ds = makeDataSource();
    const get = jest.spyOn(ds, 'getResource').mockResolvedValue({ values: ['checkout'] });
    expect(
      await ds.getAttributeValues({
        signal: 'span',
        scope: 'resource',
        key: 'service.name',
        type: 'string',
        limit: 100,
      })
    ).toEqual({ values: ['checkout'], truncated: false });
    expect(get).toHaveBeenCalledWith(
      'attribute-values',
      { signal: 'span', scope: 'resource', key: 'service.name', type: 'string', limit: 100 },
      QUIET
    );
  });

  it('reads tables and columns from sqlds routes with the tenant as schema, cached', async () => {
    const ds = makeDataSource();
    const post = jest
      .spyOn(ds, 'postResource')
      .mockImplementation(async (path) => (path === 'tables' ? ['SPANS'] : ['X']));
    expect(await ds.getTables('$tenant')).toEqual(['SPANS']);
    expect(await ds.getTables('$tenant')).toEqual(['SPANS']);
    expect(await ds.getColumns(undefined, 'SPANS')).toEqual(['X']);
    expect(post.mock.calls).toEqual([
      ['tables', { schema: 'T7' }, QUIET],
      ['columns', { table: 'SPANS' }, QUIET],
    ]);
  });

  it('does not cache a failed completion request', async () => {
    const ds = makeDataSource();
    const post = jest.spyOn(ds, 'postResource').mockRejectedValueOnce(new Error('down')).mockResolvedValue(['SPANS']);
    await expect(ds.getTables('T0')).rejects.toThrow('down');
    expect(await ds.getTables('T0')).toEqual(['SPANS']);
    expect(post).toHaveBeenCalledTimes(2);
  });

  it('treats a non-array completion answer as empty', async () => {
    const ds = makeDataSource();
    jest.spyOn(ds, 'postResource').mockResolvedValue({ error: 'x' });
    expect(await ds.getColumns('T0', 'SPANS')).toEqual([]);
  });
});
