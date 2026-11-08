import { DataQueryRequest, DataQueryResponse, getDefaultTimeRange } from '@grafana/data';
import { lastValueFrom, of } from 'rxjs';

import { makeDataSource } from './testUtils';
import { FDBQuery, SqlFormat } from './types';
import {
  FDBVariableQuery,
  FDBVariableSupport,
  fetchVariableValues,
  migrateVariableQuery,
  variableSqlQuery,
} from './variables';

jest.mock('@grafana/runtime', () => ({
  ...jest.requireActual('@grafana/runtime'),
  getTemplateSrv: () => jest.requireActual('./testUtils').mockTemplateSrv,
}));

function request(target: FDBVariableQuery): DataQueryRequest<FDBVariableQuery> {
  return {
    app: 'dashboard',
    requestId: 'r',
    interval: '1m',
    intervalMs: 60000,
    range: getDefaultTimeRange(),
    scopedVars: {},
    targets: [target],
    timezone: 'utc',
    startTime: 0,
  };
}

describe('migrateVariableQuery', () => {
  it('reads a bare string as SQL and an empty one as tenants', () => {
    expect(migrateVariableQuery('SELECT 1')).toEqual({ refId: 'variable', kind: 'sql', rawSql: 'SELECT 1' });
    expect(migrateVariableQuery(' ')).toEqual({ refId: 'variable', kind: 'tenants' });
  });

  it('fills refId and kind', () => {
    expect(migrateVariableQuery(undefined)).toEqual({ refId: 'variable', kind: 'tenants' });
    expect(migrateVariableQuery({ refId: 'V', kind: 'attributeValues', key: 'k' })).toEqual({
      refId: 'V',
      kind: 'attributeValues',
      key: 'k',
    });
  });
});

describe('fetchVariableValues', () => {
  it('lists tenants', async () => {
    const ds = makeDataSource();
    jest.spyOn(ds, 'getTenants').mockResolvedValue(['T0', 'T1']);
    expect(await fetchVariableValues(ds, { refId: 'V', kind: 'tenants' })).toEqual([
      { text: 'T0', value: 'T0' },
      { text: 'T1', value: 'T1' },
    ]);
  });

  it('lists attribute values of one key, defaulting signal, type and limit', async () => {
    const ds = makeDataSource();
    const values = jest.spyOn(ds, 'getAttributeValues').mockResolvedValue({ values: ['checkout'], truncated: false });
    const q: FDBVariableQuery = {
      refId: 'V',
      kind: 'attributeValues',
      tenant: 'T1',
      scope: 'resource',
      key: 'service.name',
    };
    expect(await fetchVariableValues(ds, q)).toEqual([{ text: 'checkout', value: 'checkout' }]);
    expect(values).toHaveBeenCalledWith({
      tenant: 'T1',
      signal: 'span',
      scope: 'resource',
      key: 'service.name',
      type: 'string',
      prefix: undefined,
      limit: 100,
    });
  });

  it('lists attribute keys of every type unless one is chosen', async () => {
    const ds = makeDataSource();
    const keys = jest.spyOn(ds, 'getAttributeKeys').mockResolvedValue({ keys: ['a', 'b'], truncated: true });
    const q: FDBVariableQuery = {
      refId: 'V',
      kind: 'attributeKeys',
      signal: 'log',
      scope: 'log',
      prefix: 'a',
      limit: 7,
    };
    expect(await fetchVariableValues(ds, q)).toEqual([
      { text: 'a', value: 'a' },
      { text: 'b', value: 'b' },
    ]);
    expect(keys).toHaveBeenCalledWith({
      tenant: undefined,
      signal: 'log',
      scope: 'log',
      type: '',
      prefix: 'a',
      limit: 7,
    });
  });

  it('asks nothing until scope and key are set', async () => {
    const ds = makeDataSource();
    const get = jest.spyOn(ds, 'getResource');
    expect(await fetchVariableValues(ds, { refId: 'V', kind: 'attributeKeys' })).toEqual([]);
    expect(await fetchVariableValues(ds, { refId: 'V', kind: 'attributeValues', scope: 'span' })).toEqual([]);
    expect(get).not.toHaveBeenCalled();
  });
});

describe('FDBVariableSupport', () => {
  it('answers resource kinds with metric find values', async () => {
    const ds = makeDataSource();
    jest.spyOn(ds, 'getTenants').mockResolvedValue(['T0']);
    const res = await lastValueFrom(new FDBVariableSupport(ds).query(request({ refId: 'V', kind: 'tenants' })));
    expect(res).toEqual({ data: [{ text: 'T0', value: 'T0' }] });
  });

  it('runs SQL kinds as a table query through the backend', async () => {
    const ds = makeDataSource();
    const answer: DataQueryResponse = { data: [] };
    const query = jest.spyOn(ds, 'query').mockReturnValue(of(answer));
    const q: FDBVariableQuery = { refId: 'V', kind: 'sql', tenant: 'T2', rawSql: 'SELECT DISTINCT x FROM SPANS' };
    expect(await lastValueFrom(new FDBVariableSupport(ds).query(request(q)))).toBe(answer);
    const sent = query.mock.calls[0][0] as DataQueryRequest<FDBQuery>;
    expect(sent.targets).toEqual([variableSqlQuery(q)]);
    expect(variableSqlQuery(q)).toEqual({
      refId: 'V',
      queryType: 'sql',
      tenant: 'T2',
      rawSql: 'SELECT DISTINCT x FROM SPANS',
      format: SqlFormat.Table,
    });
  });
});
