import React from 'react';
import { getTemplateSrv } from '@grafana/runtime';
import { Combobox, ComboboxOption } from '@grafana/ui';

import type { DataSource } from '../datasource';

interface Props {
  datasource: DataSource;
  value: string | undefined;
  onChange: (tenant: string | undefined) => void;
  width?: number;
  id?: string;
}

/** Tenant (schema) options: the tenants resource plus dashboard variables, filtered by the typed text. */
export async function tenantOptions(datasource: DataSource, input: string): Promise<Array<ComboboxOption<string>>> {
  let tenants: string[] = [];
  try {
    tenants = await datasource.getTenants();
  } catch {
    // The tenants resource is unavailable (e.g. no cluster yet); variables and custom values still work.
  }
  const variables = getTemplateSrv()
    .getVariables()
    .map((v) => '$' + v.name);
  const needle = input.toLowerCase();
  return [
    ...tenants.map((t) => ({ label: t, value: t })),
    ...variables.map((v) => ({ label: v, value: v, description: 'template variable' })),
  ].filter((o) => o.value.toLowerCase().includes(needle));
}

export function TenantPicker({ datasource, value, onChange, width = 24, id }: Props) {
  const placeholder = datasource.defaultTenant ? `default (${datasource.defaultTenant})` : 'datasource default';
  return (
    <Combobox<string>
      id={id}
      width={width}
      isClearable
      createCustomValue
      placeholder={placeholder}
      value={value ? { label: value, value } : null}
      options={(input) => tenantOptions(datasource, input)}
      onChange={(o) => onChange(o?.value || undefined)}
    />
  );
}
