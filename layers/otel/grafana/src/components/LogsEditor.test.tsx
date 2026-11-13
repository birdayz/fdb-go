import React from 'react';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { emptyLogPredicate } from '../query';
import { makeDataSource } from '../testUtils';
import { FDBQuery } from '../types';
import { LogsEditor } from './LogsEditor';

jest.mock('@grafana/runtime', () => ({
  ...jest.requireActual('@grafana/runtime'),
  getTemplateSrv: () => jest.requireActual('../testUtils').mockTemplateSrv,
}));

const TRACE = '0af7651916cd43dd8448eb211c80319c';
const logs = (extra: Partial<FDBQuery>): FDBQuery => ({ refId: 'A', queryType: 'logs', ...extra });

function setup(query: FDBQuery) {
  const ds = makeDataSource();
  const onChange = jest.fn();
  const onRunQuery = jest.fn();
  render(<LogsEditor query={query} datasource={ds} onChange={onChange} onRunQuery={onRunQuery} />);
  return { onChange, onRunQuery, ds };
}

describe('LogsEditor', () => {
  it('reads a trace by default and shows no limit', () => {
    setup(logs({ traceId: TRACE }));
    expect(screen.getByRole('radio', { name: 'By trace' })).toBeChecked();
    expect(screen.getByLabelText('Trace ID')).toHaveValue(TRACE);
    expect(screen.queryByLabelText('Limit')).not.toBeInTheDocument();
  });

  it('switching to an attribute drops the IDs and does not run the empty attribute', async () => {
    const { onChange, onRunQuery } = setup(logs({ traceId: TRACE, spanId: 'b7ad6b7169203331' }));
    await userEvent.click(screen.getByRole('radio', { name: 'By attribute' }));
    expect(onChange).toHaveBeenCalledWith(logs({ attribute: emptyLogPredicate() }));
    expect(onRunQuery).not.toHaveBeenCalled();
  });

  it('switching back to a trace drops the attribute', async () => {
    const { onChange } = setup(logs({ attribute: { scope: 'log', key: 'k', type: 'string', value: 'v' } }));
    await userEvent.click(screen.getByRole('radio', { name: 'By trace' }));
    expect(onChange).toHaveBeenCalledWith(logs({ traceId: '' }));
  });

  it('commits a span ID and runs; clearing it removes the field', async () => {
    const { onChange, onRunQuery } = setup(logs({ traceId: TRACE, spanId: 'b7ad6b7169203331' }));
    const span = screen.getByLabelText('Span ID');
    await userEvent.clear(span);
    await userEvent.tab();
    expect(onChange).toHaveBeenLastCalledWith(logs({ traceId: TRACE }));
    await userEvent.type(span, 'ABC{Enter}');
    expect(onChange).toHaveBeenLastCalledWith(logs({ traceId: TRACE, spanId: 'abc' }));
    expect(onRunQuery).toHaveBeenCalledTimes(2);
  });

  it('flags an invalid span ID and does not run', async () => {
    const { onRunQuery } = setup(logs({ traceId: TRACE }));
    await userEvent.type(screen.getByLabelText('Span ID'), 'b7ad6b71692033310{Enter}');
    expect(screen.getByText('A span ID is 1 to 16 hexadecimal characters')).toBeInTheDocument();
    expect(onRunQuery).not.toHaveBeenCalled();
  });

  it('by attribute: log scopes, default limit 1000', async () => {
    const attribute = { scope: 'log' as const, key: 'k', type: 'string' as const, value: 'v' };
    const { onChange, onRunQuery } = setup(logs({ attribute }));
    expect(screen.getByLabelText('Limit')).toHaveAttribute('placeholder', '1000');
    await userEvent.type(screen.getByLabelText('Limit'), '25{Enter}');
    expect(onChange).toHaveBeenCalledWith(logs({ attribute, limit: 25 }));
    expect(onRunQuery).toHaveBeenCalled();
    await userEvent.click(screen.getByLabelText('Scope'));
    const scopes = (await screen.findAllByRole('option')).map((o) => o.textContent);
    expect(scopes).toEqual(['resource', 'scope', 'log', 'event_name']);
  });
});
