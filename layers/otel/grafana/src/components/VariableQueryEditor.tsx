import React from 'react';
import { QueryEditorProps } from '@grafana/data';
import { CodeEditor, Combobox, InlineField, Input, RadioButtonGroup, Stack } from '@grafana/ui';

import type { DataSource } from '../datasource';
import { ATTR_TYPES, AttrType, FDBOptions, FDBQuery, Signal, SPAN_SCOPES } from '../types';
import { DEFAULT_VARIABLE_LIMIT, FDBVariableQuery, migrateVariableQuery, VariableKind } from '../variables';
import { TenantPicker } from './TenantPicker';
import { useDraft } from './useDraft';

type Props = QueryEditorProps<DataSource, FDBQuery, FDBOptions, FDBVariableQuery>;

export const VARIABLE_KIND_OPTIONS: Array<{ label: string; value: VariableKind }> = [
  { label: 'Tenants', value: 'tenants' },
  { label: 'Attribute keys', value: 'attributeKeys' },
  { label: 'Attribute values', value: 'attributeValues' },
  { label: 'SQL', value: 'sql' },
];

// The attribute indexes autocomplete reads: event_name is not one of them.
const SCOPES: Record<Signal, readonly string[]> = { span: SPAN_SCOPES, log: ['resource', 'scope', 'log'] };
const ANY_TYPE = '*';

/** Text input that commits on blur, so a variable is not re-run per keystroke. */
function BlurInput(props: {
  id: string;
  value?: string;
  placeholder?: string;
  width?: number;
  onCommit: (v: string) => void;
}) {
  const [text, setText] = useDraft(props.value ?? '');
  return (
    <Input
      id={props.id}
      width={props.width ?? 32}
      value={text}
      placeholder={props.placeholder}
      onChange={(e) => setText(e.currentTarget.value)}
      onBlur={() => text !== (props.value ?? '') && props.onCommit(text)}
    />
  );
}

export function VariableQueryEditor({ query: raw, datasource, onChange }: Props) {
  const query = migrateVariableQuery(raw);
  const set = (patch: Partial<FDBVariableQuery>) => onChange({ ...query, ...patch });
  const signal = query.signal ?? 'span';
  const isAttr = query.kind === 'attributeKeys' || query.kind === 'attributeValues';

  return (
    <Stack direction="column" gap={1}>
      <InlineField label="Values of" labelWidth={14}>
        <RadioButtonGroup options={VARIABLE_KIND_OPTIONS} value={query.kind} onChange={(kind) => set({ kind })} />
      </InlineField>
      {query.kind !== 'tenants' && (
        <InlineField label="Tenant" labelWidth={14} htmlFor="fdb-var-tenant">
          <TenantPicker
            id="fdb-var-tenant"
            datasource={datasource}
            value={query.tenant}
            onChange={(tenant) => set({ tenant })}
          />
        </InlineField>
      )}
      {isAttr && (
        <>
          <InlineField label="Signal" labelWidth={14}>
            <RadioButtonGroup<Signal>
              options={[
                { label: 'Spans', value: 'span' },
                { label: 'Logs', value: 'log' },
              ]}
              value={signal}
              onChange={(s) =>
                set({ signal: s, scope: SCOPES[s].includes(query.scope ?? '') ? query.scope : undefined })
              }
            />
          </InlineField>
          <InlineField label="Scope" labelWidth={14} htmlFor="fdb-var-scope">
            <Combobox<string>
              id="fdb-var-scope"
              width={20}
              value={query.scope ?? null}
              options={SCOPES[signal].map((s) => ({ label: s, value: s }))}
              onChange={(o) => set({ scope: o.value })}
            />
          </InlineField>
          {query.kind === 'attributeValues' && (
            <InlineField label="Key" labelWidth={14} htmlFor="fdb-var-key">
              <BlurInput
                id="fdb-var-key"
                value={query.key}
                placeholder="service.name"
                onCommit={(key) => set({ key })}
              />
            </InlineField>
          )}
          <InlineField label="Type" labelWidth={14} htmlFor="fdb-var-type">
            <Combobox<string>
              id="fdb-var-type"
              width={20}
              value={query.kind === 'attributeKeys' ? query.type || ANY_TYPE : query.type || 'string'}
              options={[
                ...(query.kind === 'attributeKeys' ? [{ label: 'any', value: ANY_TYPE }] : []),
                ...ATTR_TYPES.map((t) => ({ label: t, value: t })),
              ]}
              onChange={(o) => set({ type: o.value === ANY_TYPE ? '' : (o.value as AttrType) })}
            />
          </InlineField>
          <InlineField label="Prefix" labelWidth={14} htmlFor="fdb-var-prefix">
            <BlurInput
              id="fdb-var-prefix"
              value={query.prefix}
              placeholder="optional"
              onCommit={(prefix) => set({ prefix })}
            />
          </InlineField>
          <InlineField label="Limit" labelWidth={14} htmlFor="fdb-var-limit">
            <BlurInput
              id="fdb-var-limit"
              width={12}
              value={query.limit === undefined ? '' : String(query.limit)}
              placeholder={String(DEFAULT_VARIABLE_LIMIT)}
              onCommit={(v) => {
                const n = Number(v);
                set({ limit: v.trim() === '' || !Number.isInteger(n) || n < 1 ? undefined : n });
              }}
            />
          </InlineField>
        </>
      )}
      {query.kind === 'sql' && (
        <CodeEditor
          language="sql"
          value={query.rawSql ?? ''}
          height={120}
          showMiniMap={false}
          onBlur={(rawSql) => rawSql !== (query.rawSql ?? '') && set({ rawSql })}
        />
      )}
    </Stack>
  );
}
