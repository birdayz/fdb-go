import React from 'react';
import { createDataFrame, FieldType, getDefaultTimeRange, LoadingState, PanelData } from '@grafana/data';
import { act, render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { of, throwError } from 'rxjs';

import { makeDataSource } from '../testUtils';
import { FDBQuery, SqlFormat } from '../types';
import { SqlEditor } from './SqlEditor';

jest.mock('@grafana/runtime', () => ({
  ...jest.requireActual('@grafana/runtime'),
  getTemplateSrv: () => jest.requireActual('../testUtils').mockTemplateSrv,
}));

// Monaco does not run in jsdom: a textarea stands in, and the test drives onEditorDidMount itself.
const mockEditorProps: { current?: Record<string, any> } = {};
jest.mock('@grafana/ui', () => {
  const R = jest.requireActual('react');
  return {
    ...jest.requireActual('@grafana/ui'),
    CodeEditor: (props: Record<string, any>) => {
      mockEditorProps.current = props;
      return R.createElement('textarea', {
        'aria-label': 'SQL editor',
        defaultValue: props.value,
        onBlur: (e: { currentTarget: { value: string } }) => props.onBlur?.(e.currentTarget.value),
      });
    },
  };
});

const base: FDBQuery = { refId: 'A', queryType: 'sql', rawSql: 'SELECT 1', format: SqlFormat.Table, tenant: 'T1' };

function setup(query: FDBQuery = base, data?: PanelData) {
  const ds = makeDataSource();
  const onChange = jest.fn();
  const onRunQuery = jest.fn();
  render(<SqlEditor query={query} datasource={ds} onChange={onChange} onRunQuery={onRunQuery} data={data} />);
  return { ds, onChange, onRunQuery };
}

describe('SqlEditor', () => {
  it('commits the SQL on blur without running it', async () => {
    const { onChange, onRunQuery } = setup();
    const sql = screen.getByLabelText('SQL editor');
    await userEvent.clear(sql);
    await userEvent.type(sql, 'SELECT 2');
    sql.blur();
    expect(onChange).toHaveBeenCalledWith({ ...base, rawSql: 'SELECT 2' });
    expect(onRunQuery).not.toHaveBeenCalled();
  });

  it('commits nothing when the text is unchanged', async () => {
    const { onChange, onRunQuery } = setup();
    screen.getByLabelText('SQL editor').focus();
    screen.getByLabelText('SQL editor').blur();
    expect(onChange).not.toHaveBeenCalled();
    expect(onRunQuery).not.toHaveBeenCalled();
  });

  it('commits and runs on save (Ctrl/Cmd+S)', () => {
    const { onChange, onRunQuery } = setup();
    act(() => mockEditorProps.current!.onSave('SELECT 4'));
    expect(onChange).toHaveBeenCalledWith({ ...base, rawSql: 'SELECT 4' });
    expect(onRunQuery).toHaveBeenCalledTimes(1);
  });

  it('changes the format and runs', async () => {
    const { onChange, onRunQuery } = setup();
    await userEvent.click(screen.getByRole('radio', { name: 'Logs' }));
    expect(onChange).toHaveBeenCalledWith({ ...base, format: SqlFormat.Logs });
    expect(onRunQuery).toHaveBeenCalled();
  });

  it('shows the executed SQL of this query', async () => {
    const data: PanelData = {
      state: LoadingState.Done,
      timeRange: getDefaultTimeRange(),
      series: [createDataFrame({ refId: 'A', fields: [], meta: { executedQueryString: 'SELECT 1 /* expanded */' } })],
    };
    setup(base, data);
    expect(screen.queryByTestId('fdb-executed-query')).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole('switch', { name: 'Show query' }));
    expect(screen.getByTestId('fdb-executed-query')).toHaveTextContent('SELECT 1 /* expanded */');
  });

  it('says when nothing ran yet', async () => {
    setup();
    await userEvent.click(screen.getByRole('switch', { name: 'Show query' }));
    expect(screen.getByTestId('fdb-executed-query')).toHaveTextContent('Run the query');
  });

  it('runs EXPLAIN of the statement through the backend and shows the plan', async () => {
    const ds = makeDataSource();
    const plan = createDataFrame({ fields: [{ name: 'PLAN', type: FieldType.string, values: ['Scan(SPANS)'] }] });
    const query = jest.spyOn(ds, 'query').mockReturnValue(of({ data: [plan] }));
    render(
      <SqlEditor query={{ ...base, rawSql: 'SELECT 1;' }} datasource={ds} onChange={jest.fn()} onRunQuery={jest.fn()} />
    );
    await userEvent.click(screen.getByRole('button', { name: /Explain/ }));
    expect(await screen.findByTestId('fdb-explain')).toHaveTextContent('Scan(SPANS)');
    const req = query.mock.calls[0][0];
    expect(req.targets).toEqual([
      { refId: 'A', queryType: 'sql', tenant: 'T1', rawSql: 'EXPLAIN SELECT 1', format: SqlFormat.Table },
    ]);
    await userEvent.click(screen.getByRole('button', { name: 'Hide plan' }));
    expect(screen.queryByTestId('fdb-explain')).not.toBeInTheDocument();
  });

  it('shows EXPLAIN errors', async () => {
    const ds = makeDataSource();
    jest
      .spyOn(ds, 'query')
      .mockReturnValueOnce(of({ data: [], errors: [{ message: '0A000: read-only connection' }] }))
      .mockReturnValueOnce(throwError(() => new Error('network down')));
    render(<SqlEditor query={base} datasource={ds} onChange={jest.fn()} onRunQuery={jest.fn()} />);
    await userEvent.click(screen.getByRole('button', { name: /Explain/ }));
    expect(await screen.findByTestId('fdb-explain-error')).toHaveTextContent('0A000: read-only connection');
    await userEvent.click(screen.getByRole('button', { name: /Explain/ }));
    await waitFor(() => expect(screen.getByTestId('fdb-explain-error')).toHaveTextContent('network down'));
  });

  it('disables EXPLAIN without SQL', () => {
    setup({ ...base, rawSql: ' ' });
    expect(screen.getByRole('button', { name: /Explain/ })).toBeDisabled();
  });

  it('enables EXPLAIN as the user types in a new query and explains the live buffer', async () => {
    const ds = makeDataSource();
    const plan = createDataFrame({ fields: [{ name: 'PLAN', type: FieldType.string, values: ['Scan(T)'] }] });
    const query = jest.spyOn(ds, 'query').mockReturnValue(of({ data: [plan] }));
    const onChange = jest.fn();
    render(<SqlEditor query={{ ...base, rawSql: '' }} datasource={ds} onChange={onChange} onRunQuery={jest.fn()} />);
    expect(screen.getByRole('button', { name: /Explain/ })).toBeDisabled();

    let text = '';
    let onContentChange: (() => void) | undefined;
    const editor = {
      getModel: () => ({ id: 'm1' }),
      getValue: () => text,
      onDidChangeModelContent: (f: () => void) => {
        onContentChange = f;
        return { dispose: jest.fn() };
      },
      onDidDispose: jest.fn(),
      addCommand: jest.fn(),
    };
    const monaco = {
      KeyMod: { CtrlCmd: 2048 },
      KeyCode: { Enter: 3 },
      languages: {
        CompletionItemKind: {},
        CompletionItemInsertTextRule: {},
        registerCompletionItemProvider: () => ({ dispose: jest.fn() }),
      },
    };
    act(() => mockEditorProps.current!.onEditorDidMount(editor, monaco));
    expect(screen.getByRole('button', { name: /Explain/ })).toBeDisabled();

    act(() => {
      text = 'SELECT 9;';
      onContentChange!();
    });
    expect(screen.getByRole('button', { name: /Explain/ })).not.toBeDisabled();

    await userEvent.click(screen.getByRole('button', { name: /Explain/ }));
    expect(onChange).toHaveBeenCalledWith({ ...base, rawSql: 'SELECT 9;' });
    expect(query.mock.calls[0][0].targets[0].rawSql).toBe('EXPLAIN SELECT 9');
  });
});

describe('SqlEditor completion', () => {
  type Provider = {
    triggerCharacters: string[];
    provideCompletionItems: (model: unknown, position: unknown) => Promise<{ suggestions: Array<Record<string, any>> }>;
  };

  function mount() {
    const ds = makeDataSource();
    jest.spyOn(ds, 'postResource').mockImplementation(async (path, body) => {
      if (path === 'tables') {
        return (body as { schema?: string }).schema === 'T1' ? ['SPANS'] : [];
      }
      return ['TRACE_ID'];
    });
    const onRunQuery = jest.fn();
    const onChange = jest.fn();
    render(<SqlEditor query={base} datasource={ds} onChange={onChange} onRunQuery={onRunQuery} />);
    let provider: Provider | undefined;
    const dispose = jest.fn();
    const commands: Array<() => void> = [];
    let onDispose: (() => void) | undefined;
    const model = {
      id: 'm1',
      value: '',
      getValueInRange: () => model.value,
      getValue: () => model.value,
    };
    const editor = {
      getModel: () => model,
      getValue: () => 'SELECT 3',
      onDidDispose: (f: () => void) => (onDispose = f),
      addCommand: (_k: number, f: () => void) => commands.push(f),
    };
    class Range {
      constructor(
        public startLineNumber: number,
        public startColumn: number,
        public endLineNumber: number,
        public endColumn: number
      ) {}
    }
    const monaco = {
      Range,
      KeyMod: { CtrlCmd: 2048 },
      KeyCode: { Enter: 3 },
      languages: {
        CompletionItemKind: { Function: 1, Variable: 4, Struct: 6, Field: 3, Keyword: 17 },
        CompletionItemInsertTextRule: { InsertAsSnippet: 4 },
        registerCompletionItemProvider: (lang: string, p: Provider) => {
          expect(lang).toBe('sql');
          provider = p;
          return { dispose };
        },
      },
    };
    act(() => mockEditorProps.current!.onEditorDidMount(editor, monaco));
    return { model, provider: provider!, dispose, commands, onDispose: () => onDispose!(), onRunQuery, onChange };
  }

  it('completes tables of the query tenant after FROM, replacing the typed prefix', async () => {
    const { model, provider } = mount();
    expect(provider.triggerCharacters).toEqual(['.', '$']);
    model.value = 'SELECT * FROM SP';
    const res = await provider.provideCompletionItems(model, { lineNumber: 1, column: 17 });
    expect(res.suggestions).toEqual([
      expect.objectContaining({
        label: 'SPANS',
        insertText: 'SPANS',
        kind: 6,
        range: { startLineNumber: 1, startColumn: 15, endLineNumber: 1, endColumn: 17 },
      }),
    ]);
  });

  it('inserts macros as snippets', async () => {
    const { model, provider } = mount();
    model.value = 'WHERE $__ti';
    const res = await provider.provideCompletionItems(model, { lineNumber: 1, column: 12 });
    const macro = res.suggestions.find((s) => s.label === '$__timeFilter')!;
    expect(macro.insertTextRules).toBe(4);
    expect(macro.kind).toBe(1);
    expect(macro.range.startColumn).toBe(7);
    expect(res.suggestions.find((s) => s.label === '$svc')?.insertTextRules).toBeUndefined();
  });

  it('answers nothing for another editor model', async () => {
    const { provider } = mount();
    const other = { id: 'm2', getValueInRange: () => 'FROM ', getValue: () => 'FROM ' };
    expect(await provider.provideCompletionItems(other, { lineNumber: 1, column: 6 })).toEqual({ suggestions: [] });
  });

  it('disposes the provider with the editor and runs on Ctrl+Enter', () => {
    const { dispose, commands, onDispose, onRunQuery, onChange } = mount();
    act(() => commands[0]());
    expect(onChange).toHaveBeenCalledWith({ ...base, rawSql: 'SELECT 3' });
    expect(onRunQuery).toHaveBeenCalled();
    onDispose();
    expect(dispose).toHaveBeenCalled();
  });
});
