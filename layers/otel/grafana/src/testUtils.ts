import { DataSourceInstanceSettings, DataSourcePluginMeta } from '@grafana/data';

import { DataSource } from './datasource';
import { FDBOptions } from './types';

// Shared by the jest suites; not imported by the plugin.

/** Dashboard variables the mocked template service knows, name -> value. */
export const templateVariables: Record<string, string> = { svc: 'checkout', tenant: 'T7', trace: 'ABCDEF' };

export const mockTemplateSrv = {
  replace: (text?: string, _scopedVars?: unknown, format?: unknown) =>
    (text ?? '').replace(/\$(\w+)/g, (m, name: string) => {
      const v = templateVariables[name];
      if (v === undefined) {
        return m;
      }
      return typeof format === 'function' ? format(v, { multi: false, includeAll: false }) : v;
    }),
  getVariables: () => Object.keys(templateVariables).map((name) => ({ name })),
};

export function instanceSettings(jsonData: Partial<FDBOptions> = {}): DataSourceInstanceSettings<FDBOptions> {
  return {
    uid: 'fdb-uid',
    type: 'birdayz-foundationdb-datasource',
    name: 'FoundationDB',
    access: 'proxy',
    readOnly: false,
    jsonData: { database: '/OTEL/R_TEST', ...jsonData },
    meta: { id: 'birdayz-foundationdb-datasource' } as DataSourcePluginMeta,
  } as DataSourceInstanceSettings<FDBOptions>;
}

export function makeDataSource(jsonData: Partial<FDBOptions> = { defaultSchema: 'T0' }): DataSource {
  return new DataSource(instanceSettings(jsonData));
}
