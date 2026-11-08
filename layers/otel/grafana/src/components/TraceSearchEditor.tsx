import React from 'react';
import { Button, InlineField, RadioButtonGroup, Stack, Text } from '@grafana/ui';

import type { DataSource } from '../datasource';
import { emptyLogPredicate, emptySpanPredicate, isRunnable, searchModeOf, withSearchMode } from '../query';
import {
  DEFAULT_SEARCH_LIMIT,
  FDBQuery,
  LOG_SCOPES,
  LogPredicate,
  MAX_SEARCH_LIMIT,
  SearchMode,
  SPAN_SCOPES,
  SpanPredicate,
} from '../types';
import { LimitInput } from './LimitInput';
import { PredicateRow } from './PredicateRow';

export const SEARCH_MODE_OPTIONS: Array<{ label: string; value: SearchMode; description: string }> = [
  { label: 'Same span', value: 'sameSpan', description: 'Spans that hold every span predicate' },
  {
    label: 'Existential',
    value: 'existential',
    description: 'Traces where each span and log predicate matches somewhere in the trace',
  },
  { label: 'Attribute', value: 'attribute', description: 'Newest traces with one span attribute' },
  { label: 'Time range', value: 'time', description: 'Newest traces in the time range' },
];

interface Props {
  query: FDBQuery;
  datasource: DataSource;
  onChange: (q: FDBQuery) => void;
  onRunQuery: () => void;
}

export function TraceSearchEditor({ query, datasource, onChange, onRunQuery }: Props) {
  const mode = searchModeOf(query);
  const spans = query.spanPredicates ?? [];
  const logs = query.logPredicates ?? [];

  // Every edit is committed; the query runs only once it is complete.
  const update = (next: FDBQuery) => {
    onChange(next);
    if (isRunnable(next)) {
      onRunQuery();
    }
  };
  const setSpans = (spanPredicates: SpanPredicate[]) => update({ ...query, spanPredicates });
  const setLogs = (logPredicates: LogPredicate[]) => update({ ...query, logPredicates });

  return (
    <Stack direction="column" gap={1}>
      <Stack gap={1} wrap="wrap" alignItems="center">
        <InlineField
          label="Mode"
          labelWidth={10}
          tooltip={SEARCH_MODE_OPTIONS.find((o) => o.value === mode)?.description}
        >
          <RadioButtonGroup
            options={SEARCH_MODE_OPTIONS}
            value={mode}
            onChange={(m) => update(withSearchMode(query, m))}
          />
        </InlineField>
        <LimitInput
          id={`${query.refId}-search-limit`}
          value={query.limit}
          defaultValue={DEFAULT_SEARCH_LIMIT}
          max={MAX_SEARCH_LIMIT}
          onCommit={(limit) => update({ ...query, limit })}
        />
      </Stack>
      {mode !== 'time' && (
        <Stack direction="column" gap={0.5}>
          <Text variant="bodySmall" color="secondary">
            {mode === 'attribute' ? 'Span attribute' : 'Span predicates (all must match)'}
          </Text>
          {spans.map((p, i) => (
            <PredicateRow
              key={i}
              id={`${query.refId}-span-${i}`}
              predicate={p}
              scopes={SPAN_SCOPES}
              signal="span"
              datasource={datasource}
              tenant={query.tenant}
              onChange={(np) => setSpans(spans.map((x, j) => (j === i ? np : x)))}
              onRemove={
                mode === 'attribute' || (mode === 'sameSpan' && spans.length === 1)
                  ? undefined
                  : () => setSpans(spans.filter((_, j) => j !== i))
              }
            />
          ))}
          {mode !== 'attribute' && (
            <div>
              <Button
                variant="secondary"
                size="sm"
                icon="plus"
                onClick={() => onChange({ ...query, spanPredicates: [...spans, emptySpanPredicate()] })}
              >
                Span predicate
              </Button>
            </div>
          )}
        </Stack>
      )}
      {mode === 'existential' && (
        <Stack direction="column" gap={0.5}>
          <Text variant="bodySmall" color="secondary">
            Log predicates (each matched by a log correlated with the trace)
          </Text>
          {logs.map((p, i) => (
            <PredicateRow
              key={i}
              id={`${query.refId}-log-${i}`}
              predicate={p}
              scopes={LOG_SCOPES}
              signal="log"
              datasource={datasource}
              tenant={query.tenant}
              onChange={(np) => setLogs(logs.map((x, j) => (j === i ? np : x)))}
              onRemove={() => setLogs(logs.filter((_, j) => j !== i))}
            />
          ))}
          <div>
            <Button
              variant="secondary"
              size="sm"
              icon="plus"
              onClick={() => onChange({ ...query, logPredicates: [...logs, emptyLogPredicate()] })}
            >
              Log predicate
            </Button>
          </div>
        </Stack>
      )}
    </Stack>
  );
}
