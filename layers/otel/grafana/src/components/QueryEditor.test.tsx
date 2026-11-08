import React from 'react';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { DataSource } from '../datasource';
import { emptySpanPredicate } from '../query';
import { makeDataSource } from '../testUtils';
import { FDBQuery, SqlFormat } from '../types';
import { QueryEditor, queryTypeOptions } from './QueryEditor';

jest.mock('@grafana/runtime', () => ({
  ...jest.requireActual('@grafana/runtime'),
  getTemplateSrv: () => jest.requireActual('../testUtils').mockTemplateSrv,
}));

jest.mock('@grafana/ui', () => {
  const R = jest.requireActual('react');
  return {
    ...jest.requireActual('@grafana/ui'),
    CodeEditor: (props: { value: string }) =>
      R.createElement('textarea', { 'aria-label': 'SQL editor', defaultValue: props.value }),
  };
});

const TRACE = '0af7651916cd43dd8448eb211c80319c';

function setup(query: FDBQuery, ds: DataSource = makeDataSource()) {
  const onChange = jest.fn();
  const onRunQuery = jest.fn();
  const view = render(<QueryEditor query={query} datasource={ds} onChange={onChange} onRunQuery={onRunQuery} />);
  return { onChange, onRunQuery, ds, view };
}

describe('QueryEditor', () => {
  it('shows the SQL editor for SQL and missing query types', () => {
    setup({ refId: 'A' });
    expect(screen.getByRole('radio', { name: 'SQL' })).toBeChecked();
    expect(screen.getByLabelText('SQL editor')).toBeInTheDocument();
  });

  it('offers only SQL when the datasource has OTel off', () => {
    setup({ refId: 'A', queryType: 'sql' }, makeDataSource({ otelMode: 'off' }));
    expect(screen.getByRole('radio', { name: 'SQL' })).toBeChecked();
    expect(screen.queryByRole('radio', { name: 'Trace' })).not.toBeInTheDocument();
    expect(screen.queryByRole('radio', { name: 'Search' })).not.toBeInTheDocument();
  });

  it('with OTel off still shows the current OTel type so it can be left', () => {
    expect(queryTypeOptions('off', 'trace').map((o) => o.value)).toEqual(['sql', 'trace']);
    expect(queryTypeOptions('auto', 'sql').map((o) => o.value)).toEqual(['sql', 'trace', 'traceSearch', 'logs']);
    expect(queryTypeOptions('on', 'sql')).toHaveLength(4);
  });

  it('switches to search with one empty span predicate, without running', async () => {
    const { onChange, onRunQuery } = setup({ refId: 'A', queryType: 'sql', rawSql: 'SELECT 1' });
    await userEvent.click(screen.getByRole('radio', { name: 'Search' }));
    expect(onChange).toHaveBeenCalledWith({
      refId: 'A',
      queryType: 'traceSearch',
      rawSql: 'SELECT 1',
      mode: 'sameSpan',
      spanPredicates: [emptySpanPredicate()],
    });
    expect(onRunQuery).not.toHaveBeenCalled();
  });

  it('switches back to SQL and runs the kept statement', async () => {
    const { onChange, onRunQuery } = setup({ refId: 'A', queryType: 'trace', traceId: TRACE, rawSql: 'SELECT 1' });
    await userEvent.click(screen.getByRole('radio', { name: 'SQL' }));
    expect(onChange).toHaveBeenCalledWith(
      expect.objectContaining({ queryType: 'sql', rawSql: 'SELECT 1', format: SqlFormat.Table })
    );
    expect(onRunQuery).toHaveBeenCalled();
  });

  it('renders the editor of each type', () => {
    const { view } = setup({ refId: 'A', queryType: 'trace', traceId: TRACE });
    expect(screen.getByLabelText('Trace ID')).toHaveValue(TRACE);
    view.rerender(
      <QueryEditor
        query={{ refId: 'A', queryType: 'logs', traceId: TRACE }}
        datasource={makeDataSource()}
        onChange={jest.fn()}
        onRunQuery={jest.fn()}
      />
    );
    expect(screen.getByRole('radio', { name: 'By trace' })).toBeChecked();
    expect(screen.getByLabelText('Span ID')).toBeInTheDocument();
  });

  it('commits a trace ID lowercased on blur and runs it', async () => {
    const { onChange, onRunQuery } = setup({ refId: 'A', queryType: 'trace', traceId: '' });
    await userEvent.type(screen.getByLabelText('Trace ID'), ` ${TRACE.toUpperCase()} `);
    await userEvent.tab();
    expect(onChange).toHaveBeenCalledWith({ refId: 'A', queryType: 'trace', traceId: TRACE });
    expect(onRunQuery).toHaveBeenCalledTimes(1);
  });

  it('keeps the case of a template variable', async () => {
    const { onChange } = setup({ refId: 'A', queryType: 'trace', traceId: '' });
    await userEvent.type(screen.getByLabelText('Trace ID'), '$TraceVar{Enter}');
    expect(onChange).toHaveBeenCalledWith({ refId: 'A', queryType: 'trace', traceId: '$TraceVar' });
  });

  it('flags an invalid trace ID and does not run it', async () => {
    const { onRunQuery } = setup({ refId: 'A', queryType: 'trace', traceId: '' });
    await userEvent.type(screen.getByLabelText('Trace ID'), 'not-a-trace{Enter}');
    expect(screen.getByText('A trace ID is 1 to 32 hexadecimal characters')).toBeInTheDocument();
    expect(onRunQuery).not.toHaveBeenCalled();
  });

  it('picks a tenant from the tenants resource and runs', async () => {
    const ds = makeDataSource({ defaultSchema: 'T0' });
    jest.spyOn(ds, 'getTenants').mockResolvedValue(['T0', 'T1']);
    const { onChange, onRunQuery } = setup({ refId: 'A', queryType: 'sql', rawSql: 'SELECT 1' }, ds);
    const tenant = screen.getByLabelText('Tenant');
    expect(tenant).toHaveAttribute('placeholder', 'default (T0)');
    await userEvent.click(tenant);
    const options = await screen.findAllByRole('option');
    expect(options.map((o) => o.textContent)).toEqual(
      expect.arrayContaining(['T0', 'T1', expect.stringContaining('$tenant')])
    );
    await userEvent.click(screen.getByRole('option', { name: 'T1' }));
    expect(onChange).toHaveBeenCalledWith({ refId: 'A', queryType: 'sql', rawSql: 'SELECT 1', tenant: 'T1' });
    expect(onRunQuery).toHaveBeenCalled();
  });

  it('still offers variables when the tenants resource fails', async () => {
    const ds = makeDataSource();
    jest.spyOn(ds, 'getTenants').mockRejectedValue(new Error('no cluster'));
    setup({ refId: 'A', queryType: 'sql' }, ds);
    await userEvent.click(screen.getByLabelText('Tenant'));
    await userEvent.type(screen.getByLabelText('Tenant'), 'ten');
    expect(await screen.findByRole('option', { name: /\$tenant/ })).toBeInTheDocument();
  });
});
