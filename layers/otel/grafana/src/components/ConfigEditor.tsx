import React, { useState } from 'react';
import { DataSourcePluginOptionsEditorProps } from '@grafana/data';
import { config } from '@grafana/runtime';
import {
  CollapsableSection,
  Field,
  FieldSet,
  Input,
  RadioButtonGroup,
  SecretTextArea,
  SecureSocksProxySettings,
  Stack,
  TagsInput,
  TextArea,
} from '@grafana/ui';

import {
  allowedSchemasError,
  budgetError,
  ClusterSource,
  clusterSourceOf,
  clusterStringError,
  formatBytes,
  schemaAllowed,
  withBudget,
  withClusterSource,
} from '../settings';
import { BUDGET_DEFAULTS, BudgetKey, FDBOptions, FDBSecureJsonData, OtelMode, TlsMode } from '../types';
import { useDraft } from './useDraft';

type Props = DataSourcePluginOptionsEditorProps<FDBOptions, FDBSecureJsonData>;

interface Budget {
  key: BudgetKey;
  label: string;
  description: string;
  bytes?: boolean;
}

export const BUDGETS: Budget[] = [
  { key: 'queryTimeoutSeconds', label: 'Query timeout (s)', description: 'Deadline of every SQL, trace and log query' },
  { key: 'transactionTimeoutMs', label: 'Transaction timeout (ms)', description: 'Per FoundationDB transaction' },
  { key: 'sqlPageRows', label: 'SQL page rows', description: 'Rows scanned per transaction page' },
  { key: 'sqlPageBytes', label: 'SQL page bytes', description: 'Bytes scanned per transaction page', bytes: true },
  { key: 'sqlMaxRows', label: 'SQL max rows', description: 'Rows returned by one SQL query' },
  {
    key: 'sqlStatementMemoryBytes',
    label: 'SQL statement memory',
    description: 'Planner memory of one statement (sorts, hash aggregation)',
    bytes: true,
  },
  { key: 'traceMaxSpans', label: 'Trace max spans', description: 'Spans read for one trace; past it, a partial trace' },
  { key: 'traceMaxBytes', label: 'Trace max bytes', description: 'Bytes read for one trace', bytes: true },
  { key: 'searchLimit', label: 'Search limit', description: 'Default traces per search (at most 1000)' },
  { key: 'searchMaxCandidates', label: 'Search max candidates', description: 'Index entries one search may examine' },
  { key: 'searchMaxBytes', label: 'Search max bytes', description: 'Bytes one search may read', bytes: true },
  { key: 'logsLimit', label: 'Logs limit', description: 'Default rows per logs query' },
];

const CLUSTER_SOURCE_OPTIONS: Array<{ label: string; value: ClusterSource }> = [
  { label: 'Cluster file path', value: 'file' },
  { label: 'Cluster string', value: 'string' },
];

const TLS_OPTIONS: Array<{ label: string; value: TlsMode; description: string }> = [
  { label: 'Off', value: 'off', description: 'Plain TCP' },
  {
    label: 'Cluster',
    value: 'cluster',
    description: 'As the client resolves it: coordinators marked :tls, with FDB_TLS_* from the Grafana host',
  },
  { label: 'Config', value: 'config', description: 'TLS with the certificates below' },
];

const OTEL_OPTIONS: Array<{ label: string; value: OtelMode; description: string }> = [
  { label: 'Auto', value: 'auto', description: 'Trace, search and logs queries when the schema has OTEL_V1 tables' },
  { label: 'On', value: 'on', description: 'Always offer trace, search and logs queries' },
  { label: 'Off', value: 'off', description: 'SQL only' },
];

function BudgetField({
  budget,
  value,
  onChange,
}: {
  budget: Budget;
  value?: number;
  onChange: (text: string) => void;
}) {
  const [text, setText] = useDraft(value === undefined ? '' : String(value));
  const error = budgetError(budget.key, text);
  const def = BUDGET_DEFAULTS[budget.key];
  const id = `fdb-budget-${budget.key}`;
  return (
    <Field
      label={budget.label}
      description={`${budget.description}. Default ${def}${budget.bytes ? ` (${formatBytes(def)})` : ''}.`}
      invalid={!!error}
      error={error}
      htmlFor={id}
    >
      <Input
        id={id}
        width={24}
        inputMode="numeric"
        value={text}
        placeholder={String(def)}
        onChange={(e) => {
          setText(e.currentTarget.value);
          onChange(e.currentTarget.value);
        }}
      />
    </Field>
  );
}

export function ConfigEditor({ options, onOptionsChange }: Props) {
  const { jsonData, secureJsonFields, secureJsonData } = options;
  const [clusterSource, setClusterSource] = useState<ClusterSource>(clusterSourceOf(jsonData));

  const setJsonData = (next: FDBOptions) => onOptionsChange({ ...options, jsonData: next });
  const setField = <K extends keyof FDBOptions>(key: K, value: FDBOptions[K]) => {
    const next = { ...jsonData, [key]: value };
    if (value === '' || value === undefined || (Array.isArray(value) && value.length === 0)) {
      delete next[key];
    }
    setJsonData(next);
  };
  const setSecret = (key: keyof FDBSecureJsonData, value: string) =>
    onOptionsChange({ ...options, secureJsonData: { ...secureJsonData, [key]: value } });
  const resetSecret = (key: keyof FDBSecureJsonData) =>
    onOptionsChange({
      ...options,
      secureJsonFields: { ...secureJsonFields, [key]: false },
      secureJsonData: { ...secureJsonData, [key]: '' },
    });

  const tlsMode = jsonData.tlsMode ?? 'off';
  const clusterError = clusterStringError(jsonData.clusterString);
  const allowedError = allowedSchemasError(jsonData.allowedSchemas);
  const defaultOutside =
    !!jsonData.defaultSchema && !allowedError && !schemaAllowed(jsonData.defaultSchema, jsonData.allowedSchemas);

  return (
    <Stack direction="column" gap={2}>
      <FieldSet label="Cluster">
        <Field label="Connect with">
          <RadioButtonGroup
            options={CLUSTER_SOURCE_OPTIONS}
            value={clusterSource}
            onChange={(s) => {
              setClusterSource(s);
              setJsonData(withClusterSource(jsonData, s));
            }}
          />
        </Field>
        {clusterSource === 'file' ? (
          <Field
            label="Cluster file path"
            description="Path on the Grafana host. The client writes coordinator changes back to it."
            htmlFor="fdb-cluster-file"
          >
            <Input
              id="fdb-cluster-file"
              width={60}
              value={jsonData.clusterFilePath ?? ''}
              placeholder="/etc/foundationdb/fdb.cluster"
              onChange={(e) => setField('clusterFilePath', e.currentTarget.value)}
            />
          </Field>
        ) : (
          <Field
            label="Cluster string"
            description="For containers and Cloud. Coordinator changes stay in memory, so it goes stale once every coordinator it names has moved."
            htmlFor="fdb-cluster-string"
            invalid={!!clusterError}
            error={clusterError}
          >
            <Input
              id="fdb-cluster-string"
              width={60}
              value={jsonData.clusterString ?? ''}
              placeholder="docker:docker@127.0.0.1:4500"
              onChange={(e) => setField('clusterString', e.currentTarget.value)}
            />
          </Field>
        )}
        <Field label="TLS" description={TLS_OPTIONS.find((o) => o.value === tlsMode)?.description}>
          <RadioButtonGroup options={TLS_OPTIONS} value={tlsMode} onChange={(m) => setField('tlsMode', m)} />
        </Field>
        {tlsMode === 'config' && (
          <>
            <Field
              label="CA certificate"
              description="PEM. Required: peers are verified against this CA alone."
              htmlFor="fdb-tls-ca"
              required
              invalid={!jsonData.tlsCACert?.trim()}
              error={!jsonData.tlsCACert?.trim() ? 'TLS from the settings needs a CA certificate' : undefined}
            >
              <TextArea
                id="fdb-tls-ca"
                rows={4}
                value={jsonData.tlsCACert ?? ''}
                placeholder="-----BEGIN CERTIFICATE-----"
                onChange={(e) => setField('tlsCACert', e.currentTarget.value)}
              />
            </Field>
            <Field
              label="Verify peers"
              description="FDB_TLS_VERIFY_PEERS syntax. Empty: Check.Valid=1."
              htmlFor="fdb-tls-verify-peers"
            >
              <Input
                id="fdb-tls-verify-peers"
                width={60}
                value={jsonData.tlsVerifyPeers ?? ''}
                placeholder="Check.Valid=1"
                onChange={(e) => setField('tlsVerifyPeers', e.currentTarget.value)}
              />
            </Field>
            <Field label="Client certificate" description="PEM" htmlFor="fdb-tls-client-cert">
              <SecretTextArea
                id="fdb-tls-client-cert"
                rows={4}
                isConfigured={!!secureJsonFields?.tlsClientCert}
                value={secureJsonData?.tlsClientCert ?? ''}
                placeholder="-----BEGIN CERTIFICATE-----"
                onReset={() => resetSecret('tlsClientCert')}
                onChange={(e) => setSecret('tlsClientCert', e.currentTarget.value)}
              />
            </Field>
            <Field label="Client key" description="PEM" htmlFor="fdb-tls-client-key">
              <SecretTextArea
                id="fdb-tls-client-key"
                rows={4}
                isConfigured={!!secureJsonFields?.tlsClientKey}
                value={secureJsonData?.tlsClientKey ?? ''}
                placeholder="PEM private key"
                onReset={() => resetSecret('tlsClientKey')}
                onChange={(e) => setSecret('tlsClientKey', e.currentTarget.value)}
              />
            </Field>
          </>
        )}
        {config.secureSocksDSProxyEnabled && (
          <SecureSocksProxySettings options={options} onOptionsChange={onOptionsChange} />
        )}
      </FieldSet>

      <FieldSet label="Database">
        <Field
          label="Database"
          description="One relational database per datasource; OTel stores are /OTEL/R_<RUN>."
          htmlFor="fdb-database"
          required
        >
          <Input
            id="fdb-database"
            width={60}
            value={jsonData.database ?? ''}
            placeholder="/OTEL/R_RUN"
            onChange={(e) => setField('database', e.currentTarget.value)}
          />
        </Field>
        <Field
          label="Default tenant"
          description="Schema used when a query names no tenant."
          htmlFor="fdb-default-schema"
          invalid={defaultOutside}
          error={defaultOutside ? 'Not in the allowed tenants' : undefined}
        >
          <Input
            id="fdb-default-schema"
            width={30}
            value={jsonData.defaultSchema ?? ''}
            placeholder="T0"
            onChange={(e) => setField('defaultSchema', e.currentTarget.value)}
          />
        </Field>
        <Field
          label="Allowed tenants"
          description="Schema names, plus at most one prefix ending in * (e.g. T*). Empty: every schema. Others are refused before any read."
          htmlFor="fdb-allowed-schemas"
          invalid={!!allowedError}
          error={allowedError}
        >
          <TagsInput
            id="fdb-allowed-schemas"
            width={60}
            tags={jsonData.allowedSchemas ?? []}
            placeholder="T0, T1 or T*"
            onChange={(tags) => setField('allowedSchemas', tags)}
          />
        </Field>
        <Field
          label="OTel queries"
          description={OTEL_OPTIONS.find((o) => o.value === (jsonData.otelMode ?? 'auto'))?.description}
        >
          <RadioButtonGroup
            options={OTEL_OPTIONS}
            value={jsonData.otelMode ?? 'auto'}
            onChange={(m) => setField('otelMode', m)}
          />
        </Field>
      </FieldSet>

      <CollapsableSection label="Limits and transaction tags" isOpen={false}>
        <Field
          label="Transaction tags"
          description="Tags on every FDB transaction, so an operator can throttle Grafana's reads."
          htmlFor="fdb-transaction-tags"
        >
          <TagsInput
            id="fdb-transaction-tags"
            width={60}
            tags={jsonData.transactionTags ?? []}
            placeholder="grafana"
            onChange={(tags) => setField('transactionTags', tags)}
          />
        </Field>
        {BUDGETS.map((b) => (
          <BudgetField
            key={b.key}
            budget={b}
            value={jsonData[b.key]}
            onChange={(text) => setJsonData(withBudget(jsonData, b.key, text))}
          />
        ))}
      </CollapsableSection>
    </Stack>
  );
}
