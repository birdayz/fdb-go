import { DataSourcePlugin } from '@grafana/data';

import { ConfigEditor } from './components/ConfigEditor';
import { QueryEditor } from './components/QueryEditor';
import { DataSource } from './datasource';
import { FDBOptions, FDBQuery, FDBSecureJsonData } from './types';

export const plugin = new DataSourcePlugin<DataSource, FDBQuery, FDBOptions, FDBSecureJsonData>(DataSource)
  .setConfigEditor(ConfigEditor)
  .setQueryEditor(QueryEditor);
