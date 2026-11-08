import React, { useEffect, useRef, useState } from 'react';
import { lastValueFrom } from 'rxjs';
import { CoreApp, DataQueryRequest, getDefaultTimeRange, PanelData, TimeRange } from '@grafana/data';
import { getTemplateSrv } from '@grafana/runtime';
import {
  Button,
  CodeEditor,
  InlineField,
  InlineSwitch,
  type Monaco,
  type MonacoEditor,
  type monacoTypes,
  RadioButtonGroup,
  Stack,
} from '@grafana/ui';

import type { DataSource } from '../datasource';
import { completeSql, SqlCompletionKind } from '../sqlCompletion';
import { errorText, executedQuery, explainSql, framesToText } from '../sqlUtil';
import { FDBQuery, SqlFormat } from '../types';

export const FORMAT_OPTIONS = [
  { label: 'Time series', value: SqlFormat.TimeSeries },
  { label: 'Table', value: SqlFormat.Table },
  { label: 'Logs', value: SqlFormat.Logs },
];

interface Props {
  query: FDBQuery;
  datasource: DataSource;
  onChange: (q: FDBQuery) => void;
  onRunQuery: () => void;
  data?: PanelData;
  range?: TimeRange;
}

const SORT: Record<SqlCompletionKind, string> = { column: '1', table: '1', macro: '2', variable: '3', keyword: '4' };

function itemKind(monaco: Monaco, kind: SqlCompletionKind): monacoTypes.languages.CompletionItemKind {
  const k = monaco.languages.CompletionItemKind;
  switch (kind) {
    case 'macro':
      return k.Function;
    case 'variable':
      return k.Variable;
    case 'table':
      return k.Struct;
    case 'column':
      return k.Field;
    case 'keyword':
      return k.Keyword;
  }
}

interface ExplainState {
  loading: boolean;
  text?: string;
  error?: string;
}

export function SqlEditor({ query, datasource, onChange, onRunQuery, data, range }: Props) {
  const [showQuery, setShowQuery] = useState(false);
  const [explain, setExplain] = useState<ExplainState | undefined>();
  // Monaco callbacks are registered once; they read the current props through refs.
  const latest = useRef({ query, onChange, onRunQuery });
  useEffect(() => {
    latest.current = { query, onChange, onRunQuery };
  }, [query, onChange, onRunQuery]);

  const commit = (rawSql: string, run: boolean) => {
    const { query: q, onChange: change, onRunQuery: runQuery } = latest.current;
    if (rawSql !== (q.rawSql ?? '')) {
      change({ ...q, rawSql });
    }
    if (run) {
      runQuery();
    }
  };

  const onEditorDidMount = (editor: MonacoEditor, monaco: Monaco) => {
    const provider = monaco.languages.registerCompletionItemProvider('sql', {
      triggerCharacters: ['.', '$'],
      provideCompletionItems: async (model, position) => {
        // The provider is global to the language: answer only for this editor's model.
        if (model.id !== editor.getModel()?.id) {
          return { suggestions: [] };
        }
        const before = model.getValueInRange({
          startLineNumber: 1,
          startColumn: 1,
          endLineNumber: position.lineNumber,
          endColumn: position.column,
        });
        const tenant = latest.current.query.tenant;
        const res = await completeSql(before, model.getValue(), {
          tables: () => datasource.getTables(tenant),
          columns: (table) => datasource.getColumns(tenant, table),
          variables: () =>
            getTemplateSrv()
              .getVariables()
              .map((v) => v.name),
        });
        const range = new monaco.Range(
          position.lineNumber,
          Math.max(1, position.column - res.replaceLength),
          position.lineNumber,
          position.column
        );
        return {
          suggestions: res.items.map((i) => ({
            label: i.label,
            kind: itemKind(monaco, i.kind),
            insertText: i.insertText,
            insertTextRules: i.snippet ? monaco.languages.CompletionItemInsertTextRule.InsertAsSnippet : undefined,
            detail: i.detail,
            documentation: i.documentation,
            sortText: SORT[i.kind] + i.label,
            range,
          })),
        };
      },
    });
    editor.onDidDispose(() => provider.dispose());
    editor.addCommand(monaco.KeyMod.CtrlCmd | monaco.KeyCode.Enter, () => commit(editor.getValue(), true));
  };

  const runExplain = async () => {
    setExplain({ loading: true });
    const request: DataQueryRequest<FDBQuery> = {
      app: CoreApp.Unknown,
      requestId: `fdb-explain-${query.refId}`,
      interval: '1m',
      intervalMs: 60000,
      range: range ?? getDefaultTimeRange(),
      scopedVars: {},
      targets: [
        {
          refId: query.refId,
          queryType: 'sql',
          tenant: query.tenant,
          rawSql: explainSql(query.rawSql ?? ''),
          format: SqlFormat.Table,
        },
      ],
      timezone: 'utc',
      startTime: Date.now(),
    };
    try {
      const res = await lastValueFrom(datasource.query(request));
      const error = errorText(res.errors);
      setExplain(error ? { loading: false, error } : { loading: false, text: framesToText(res.data) });
    } catch (e) {
      setExplain({ loading: false, error: e instanceof Error ? e.message : String(e) });
    }
  };

  const executed = executedQuery(data, query.refId);

  return (
    <Stack direction="column" gap={1}>
      <CodeEditor
        language="sql"
        value={query.rawSql ?? ''}
        height={200}
        showLineNumbers
        showMiniMap={false}
        // Blur only commits: running here races Explore's Run button, and Grafana 12.0
        // aborts the blur's run without starting the button's (as Grafana's SQL editors do).
        onBlur={(v) => commit(v, false)}
        onSave={(v) => commit(v, true)}
        onEditorDidMount={onEditorDidMount}
      />
      <Stack gap={1} alignItems="center" wrap="wrap">
        <InlineField label="Format" labelWidth={10}>
          <RadioButtonGroup
            options={FORMAT_OPTIONS}
            value={query.format ?? SqlFormat.Table}
            onChange={(format) => {
              onChange({ ...query, format });
              onRunQuery();
            }}
          />
        </InlineField>
        <InlineField label="Show query" labelWidth={12}>
          <InlineSwitch
            aria-label="Show query"
            value={showQuery}
            onChange={(e) => setShowQuery(e.currentTarget.checked)}
          />
        </InlineField>
        <Button
          variant="secondary"
          size="sm"
          icon={explain?.loading ? 'spinner' : 'list-ul'}
          disabled={!query.rawSql?.trim() || explain?.loading}
          onClick={runExplain}
        >
          Explain
        </Button>
        {explain && !explain.loading && (
          <Button variant="secondary" fill="text" size="sm" onClick={() => setExplain(undefined)}>
            Hide plan
          </Button>
        )}
      </Stack>
      {showQuery && (
        <pre data-testid="fdb-executed-query">{executed ?? 'Run the query to see the SQL the backend executed.'}</pre>
      )}
      {explain?.error && <pre data-testid="fdb-explain-error">{explain.error}</pre>}
      {explain?.text !== undefined && <pre data-testid="fdb-explain">{explain.text}</pre>}
    </Stack>
  );
}
