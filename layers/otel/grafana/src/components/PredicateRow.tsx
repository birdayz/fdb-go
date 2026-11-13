import React, { useRef } from 'react';
import { getTemplateSrv } from '@grafana/runtime';
import { Combobox, ComboboxOption, IconButton, InlineField, Stack } from '@grafana/ui';

import type { DataSource } from '../datasource';
import { hasTemplateVariable, valueError, withScope } from '../query';
import { ATTR_TYPES, AttrType, EVENT_NAME_SCOPE, Predicate, Signal } from '../types';

export const AUTOCOMPLETE_LIMIT = 100;

interface Props<S extends string> {
  /** Unique within the editor; prefixes the control IDs. */
  id: string;
  predicate: Predicate<S>;
  scopes: readonly S[];
  signal: Signal;
  datasource: DataSource;
  tenant?: string;
  onChange: (p: Predicate<S>) => void;
  onRemove?: () => void;
}

/** Key options of every type for the typed prefix; each option's description lists the key's types. */
export async function keyOptions(
  datasource: DataSource,
  req: { tenant?: string; signal: Signal; scope: string; prefix: string },
  typesByKey: Map<string, AttrType[]>
): Promise<Array<ComboboxOption<string>>> {
  if (req.scope === EVENT_NAME_SCOPE) {
    return [];
  }
  const { keys } = await datasource.getTypedAttributeKeys({ ...req, limit: AUTOCOMPLETE_LIMIT });
  for (const k of keys) {
    typesByKey.set(k.key, k.types);
  }
  return keys.map((k) => ({ label: k.key, value: k.key, description: k.types.join(', ') }));
}

/** Value options: true/false for bool, else the attribute-values resource, plus dashboard variables. */
export async function valueOptions(
  datasource: DataSource,
  req: { tenant?: string; signal: Signal; scope: string; key: string; type: AttrType; prefix: string }
): Promise<Array<ComboboxOption<string>>> {
  const variables = getTemplateSrv()
    .getVariables()
    .map((v) => ({ label: '$' + v.name, value: '$' + v.name, description: 'template variable' }));
  let values: string[] = [];
  if (req.type === 'bool') {
    values = ['true', 'false'];
  } else if (req.key && req.scope !== EVENT_NAME_SCOPE && !hasTemplateVariable(req.key)) {
    values = (await datasource.getAttributeValues({ ...req, limit: AUTOCOMPLETE_LIMIT })).values;
  }
  const needle = req.prefix.toLowerCase();
  return [...values.map((v) => ({ label: v, value: v })), ...variables].filter((o) =>
    o.value.toLowerCase().startsWith(needle)
  );
}

export function PredicateRow<S extends string>({
  id,
  predicate,
  scopes,
  signal,
  datasource,
  tenant,
  onChange,
  onRemove,
}: Props<S>) {
  const typesByKey = useRef(new Map<string, AttrType[]>());
  const isEventName = predicate.scope === EVENT_NAME_SCOPE;
  const error = valueError(predicate.type, predicate.value);

  const setKey = (key: string) => {
    const types = typesByKey.current.get(key);
    const type = types && types.length && !types.includes(predicate.type) ? types[0] : predicate.type;
    onChange({ ...predicate, key, type, value: type === predicate.type ? predicate.value : '' });
  };

  const setType = (type: AttrType) => {
    onChange({
      ...predicate,
      type,
      value: type === 'bool' && !/^(true|false)$/.test(predicate.value) ? '' : predicate.value,
    });
  };

  return (
    <Stack gap={0.5} alignItems="flex-start" wrap="wrap">
      <InlineField label="Scope" htmlFor={`${id}-scope`}>
        <Combobox<string>
          id={`${id}-scope`}
          width={16}
          value={predicate.scope}
          options={scopes.map((s) => ({ label: s, value: s }))}
          onChange={(o) => onChange(withScope(predicate, o.value as S))}
        />
      </InlineField>
      {/* Combobox's own disabled prop is overridden by its input props; the field context reaches the input. */}
      <InlineField label="Key" htmlFor={`${id}-key`} disabled={isEventName}>
        <Combobox<string>
          id={`${id}-key`}
          width={32}
          createCustomValue
          placeholder="attribute key"
          value={predicate.key ? { label: predicate.key, value: predicate.key } : null}
          options={(prefix) =>
            keyOptions(datasource, { tenant, signal, scope: predicate.scope, prefix }, typesByKey.current)
          }
          onChange={(o) => setKey(o.value)}
        />
      </InlineField>
      <InlineField label="Type" htmlFor={`${id}-type`} disabled={isEventName}>
        <Combobox<string>
          id={`${id}-type`}
          width={12}
          value={predicate.type}
          options={ATTR_TYPES.map((t) => ({ label: t, value: t }))}
          onChange={(o) => setType(o.value as AttrType)}
        />
      </InlineField>
      <InlineField label="Value" htmlFor={`${id}-value`} invalid={!!error} error={error}>
        <Combobox<string>
          id={`${id}-value`}
          width={32}
          createCustomValue
          invalid={!!error}
          placeholder={isEventName ? 'event name' : 'value'}
          value={predicate.value ? { label: predicate.value, value: predicate.value } : null}
          options={(prefix) =>
            valueOptions(datasource, {
              tenant,
              signal,
              scope: predicate.scope,
              key: predicate.key,
              type: predicate.type,
              prefix,
            })
          }
          onChange={(o) => onChange({ ...predicate, value: o.value })}
        />
      </InlineField>
      {onRemove && <IconButton name="trash-alt" aria-label="Remove predicate" onClick={onRemove} />}
    </Stack>
  );
}
