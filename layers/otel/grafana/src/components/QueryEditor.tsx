import React from 'react';
import { QueryEditorProps } from '@grafana/data';
import { InlineField, RadioButtonGroup, Stack } from '@grafana/ui';

import { DataSource } from '../datasource';
import { isRunnable, queryTypeOf, traceIdError, withQueryType, withTenant } from '../query';
import { FDBOptions, FDBQuery, QueryType } from '../types';
import { IdInput } from './IdInput';
import { LogsEditor } from './LogsEditor';
import { SqlEditor } from './SqlEditor';
import { TenantPicker } from './TenantPicker';
import { TraceSearchEditor } from './TraceSearchEditor';

type Props = QueryEditorProps<DataSource, FDBQuery, FDBOptions>;

export const QUERY_TYPE_OPTIONS: Array<{ label: string; value: QueryType; description: string }> = [
  { label: 'SQL', value: 'sql', description: 'SQL over the tenant schema' },
  { label: 'Trace', value: 'trace', description: 'One trace by ID' },
  { label: 'Search', value: 'traceSearch', description: 'Traces by typed attribute predicates' },
  { label: 'Logs', value: 'logs', description: 'Logs by trace or by attribute' },
];

/** The query types offered: SQL only when OTel is off, keeping the current type so it can be switched away from. */
export function queryTypeOptions(otelMode: string, current: QueryType) {
  return otelMode === 'off'
    ? QUERY_TYPE_OPTIONS.filter((o) => o.value === 'sql' || o.value === current)
    : QUERY_TYPE_OPTIONS;
}

export function QueryEditor({ query, datasource, onChange, onRunQuery, data, range }: Props) {
  const queryType = queryTypeOf(query);

  const update = (next: FDBQuery) => {
    onChange(next);
    if (isRunnable(next)) {
      onRunQuery();
    }
  };

  return (
    <Stack direction="column" gap={1}>
      <Stack gap={1} wrap="wrap" alignItems="center">
        <InlineField label="Query type" labelWidth={14}>
          <RadioButtonGroup
            options={queryTypeOptions(datasource.otelMode, queryType)}
            value={queryType}
            onChange={(t) => update(withQueryType(query, t))}
          />
        </InlineField>
        <InlineField label="Tenant" labelWidth={10} htmlFor={`${query.refId}-tenant`} tooltip="Schema the query reads">
          <TenantPicker
            id={`${query.refId}-tenant`}
            datasource={datasource}
            value={query.tenant}
            onChange={(tenant) => update(withTenant(query, tenant))}
          />
        </InlineField>
      </Stack>
      {queryType === 'sql' && (
        <SqlEditor
          query={query}
          datasource={datasource}
          onChange={onChange}
          onRunQuery={onRunQuery}
          data={data}
          range={range}
        />
      )}
      {queryType === 'trace' && (
        <IdInput
          id={`${query.refId}-trace-id`}
          label="Trace ID"
          value={query.traceId}
          placeholder="32 hex characters"
          validate={traceIdError}
          onCommit={(traceId) => update({ ...query, traceId })}
        />
      )}
      {queryType === 'traceSearch' && (
        <TraceSearchEditor query={query} datasource={datasource} onChange={onChange} onRunQuery={onRunQuery} />
      )}
      {queryType === 'logs' && (
        <LogsEditor query={query} datasource={datasource} onChange={onChange} onRunQuery={onRunQuery} />
      )}
    </Stack>
  );
}
