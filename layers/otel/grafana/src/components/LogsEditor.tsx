import React from 'react';
import { InlineField, RadioButtonGroup, Stack } from '@grafana/ui';

import type { DataSource } from '../datasource';
import { emptyLogPredicate, isRunnable, LogsBy, logsByOf, spanIdError, traceIdError, withLogsBy } from '../query';
import { DEFAULT_LOGS_LIMIT, FDBQuery, LOG_SCOPES } from '../types';
import { IdInput } from './IdInput';
import { LimitInput } from './LimitInput';
import { PredicateRow } from './PredicateRow';

export const LOGS_BY_OPTIONS: Array<{ label: string; value: LogsBy; description: string }> = [
  { label: 'By trace', value: 'trace', description: 'Logs correlated with a trace, optionally one span' },
  { label: 'By attribute', value: 'attribute', description: 'Newest logs with one attribute' },
];

interface Props {
  query: FDBQuery;
  datasource: DataSource;
  onChange: (q: FDBQuery) => void;
  onRunQuery: () => void;
}

export function LogsEditor({ query, datasource, onChange, onRunQuery }: Props) {
  const by = logsByOf(query);
  const update = (next: FDBQuery) => {
    onChange(next);
    if (isRunnable(next)) {
      onRunQuery();
    }
  };

  return (
    <Stack direction="column" gap={1}>
      <Stack gap={1} wrap="wrap" alignItems="center">
        <InlineField label="Logs by" labelWidth={14}>
          <RadioButtonGroup options={LOGS_BY_OPTIONS} value={by} onChange={(b) => update(withLogsBy(query, b))} />
        </InlineField>
        {by === 'attribute' && (
          <LimitInput
            id={`${query.refId}-logs-limit`}
            value={query.limit}
            defaultValue={DEFAULT_LOGS_LIMIT}
            onCommit={(limit) => update({ ...query, limit })}
          />
        )}
      </Stack>
      {by === 'trace' ? (
        <Stack gap={1} wrap="wrap">
          <IdInput
            id={`${query.refId}-logs-trace-id`}
            label="Trace ID"
            value={query.traceId}
            placeholder="32 hex characters"
            validate={traceIdError}
            onCommit={(traceId) => update({ ...query, traceId })}
          />
          <IdInput
            id={`${query.refId}-logs-span-id`}
            label="Span ID"
            width={24}
            value={query.spanId}
            placeholder="optional, 16 hex"
            validate={spanIdError}
            onCommit={(spanId) => {
              const next: FDBQuery = { ...query, spanId };
              if (spanId === '') {
                delete next.spanId;
              }
              update(next);
            }}
          />
        </Stack>
      ) : (
        <PredicateRow
          id={`${query.refId}-logs-attr`}
          predicate={query.attribute ?? emptyLogPredicate()}
          scopes={LOG_SCOPES}
          signal="log"
          datasource={datasource}
          tenant={query.tenant}
          onChange={(attribute) => update({ ...query, attribute })}
        />
      )}
    </Stack>
  );
}
