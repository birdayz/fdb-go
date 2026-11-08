import React from 'react';
import { InlineField, Input } from '@grafana/ui';

import { limitError } from '../query';
import { useDraft } from './useDraft';

interface Props {
  id: string;
  value: number | undefined;
  defaultValue: number;
  max?: number;
  /** undefined: use the backend default. */
  onCommit: (limit: number | undefined) => void;
}

export function parseLimit(text: string): number | undefined {
  const t = text.trim();
  return t === '' ? undefined : Number(t);
}

export function LimitInput({ id, value, defaultValue, max, onCommit }: Props) {
  const [text, setText] = useDraft(value === undefined ? '' : String(value));
  const error = limitError(parseLimit(text), max);
  const commit = () => {
    const limit = parseLimit(text);
    if (!error && limit !== value) {
      onCommit(limit);
    }
  };
  return (
    <InlineField
      label="Limit"
      labelWidth={10}
      htmlFor={id}
      invalid={!!error}
      error={error}
      tooltip={`Default ${defaultValue}${max ? `, at most ${max}` : ''}`}
    >
      <Input
        id={id}
        width={12}
        type="number"
        min={1}
        max={max}
        value={text}
        placeholder={String(defaultValue)}
        onChange={(e) => setText(e.currentTarget.value)}
        onBlur={commit}
        onKeyDown={(e) => {
          if (e.key === 'Enter') {
            commit();
          }
        }}
      />
    </InlineField>
  );
}
