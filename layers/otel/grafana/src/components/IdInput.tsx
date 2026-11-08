import React from 'react';
import { InlineField, Input } from '@grafana/ui';

import { hasTemplateVariable } from '../query';
import { useDraft } from './useDraft';

interface Props {
  id: string;
  label: string;
  value: string | undefined;
  placeholder: string;
  validate: (v: string) => string | undefined;
  /** Called on blur and Enter with the trimmed text, lowercased unless it holds a template variable. */
  onCommit: (v: string) => void;
  width?: number;
}

/** A hex ID field: edits locally, normalises and commits on blur or Enter. */
export function IdInput({ id, label, value, placeholder, validate, onCommit, width = 48 }: Props) {
  const [text, setText] = useDraft(value ?? '');
  const error = validate(text);
  const commit = () => {
    const t = text.trim();
    const v = hasTemplateVariable(t) ? t : t.toLowerCase();
    setText(v);
    if (v !== (value ?? '')) {
      onCommit(v);
    }
  };
  return (
    <InlineField label={label} labelWidth={14} htmlFor={id} invalid={!!error} error={error}>
      <Input
        id={id}
        width={width}
        value={text}
        placeholder={placeholder}
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
