import React from 'react';
import { render, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { DataSource } from '../datasource';
import { emptyLogPredicate, emptySpanPredicate } from '../query';
import { makeDataSource } from '../testUtils';
import { FDBQuery, SpanPredicate } from '../types';
import { TraceSearchEditor } from './TraceSearchEditor';

jest.mock('@grafana/runtime', () => ({
  ...jest.requireActual('@grafana/runtime'),
  getTemplateSrv: () => jest.requireActual('../testUtils').mockTemplateSrv,
}));

const p = (key: string, value: string, type: SpanPredicate['type'] = 'string'): SpanPredicate => ({
  scope: 'span',
  key,
  type,
  value,
});

function setup(query: FDBQuery, ds: DataSource = makeDataSource()) {
  const onChange = jest.fn();
  const onRunQuery = jest.fn();
  render(<TraceSearchEditor query={query} datasource={ds} onChange={onChange} onRunQuery={onRunQuery} />);
  return { onChange, onRunQuery, ds };
}

const search = (extra: Partial<FDBQuery>): FDBQuery => ({ refId: 'A', queryType: 'traceSearch', ...extra });

describe('TraceSearchEditor', () => {
  it('switching to time drops the predicates and runs', async () => {
    const { onChange, onRunQuery } = setup(search({ mode: 'sameSpan', spanPredicates: [p('k', 'v')] }));
    await userEvent.click(screen.getByRole('radio', { name: 'Time range' }));
    expect(onChange).toHaveBeenCalledWith(search({ mode: 'time' }));
    expect(onRunQuery).toHaveBeenCalled();
  });

  it('time mode shows no predicates', () => {
    setup(search({ mode: 'time' }));
    expect(screen.queryByLabelText('Key')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Span predicate/ })).not.toBeInTheDocument();
  });

  it('adds an empty span predicate without running', async () => {
    const { onChange, onRunQuery } = setup(search({ mode: 'sameSpan', spanPredicates: [p('k', 'v')] }));
    await userEvent.click(screen.getByRole('button', { name: /Span predicate/ }));
    expect(onChange).toHaveBeenCalledWith(
      search({ mode: 'sameSpan', spanPredicates: [p('k', 'v'), emptySpanPredicate()] })
    );
    expect(onRunQuery).not.toHaveBeenCalled();
  });

  it('removes a span predicate and runs what remains', async () => {
    const { onChange, onRunQuery } = setup(search({ mode: 'sameSpan', spanPredicates: [p('a', '1'), p('b', '')] }));
    const remove = screen.getAllByRole('button', { name: 'Remove predicate' });
    expect(remove).toHaveLength(2);
    await userEvent.click(remove[1]);
    expect(onChange).toHaveBeenCalledWith(search({ mode: 'sameSpan', spanPredicates: [p('a', '1')] }));
    expect(onRunQuery).toHaveBeenCalled();
  });

  it('keeps the last same-span predicate', () => {
    setup(search({ mode: 'sameSpan', spanPredicates: [p('a', '1')] }));
    expect(screen.queryByRole('button', { name: 'Remove predicate' })).not.toBeInTheDocument();
  });

  it('attribute mode has exactly one predicate', () => {
    setup(search({ mode: 'attribute', spanPredicates: [p('a', '1')] }));
    expect(screen.getAllByLabelText('Key')).toHaveLength(1);
    expect(screen.queryByRole('button', { name: /Span predicate/ })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Remove predicate' })).not.toBeInTheDocument();
  });

  it('existential mode edits log predicates too', async () => {
    const { onChange } = setup(search({ mode: 'existential', spanPredicates: [p('a', '1')], logPredicates: [] }));
    expect(screen.getByText(/Log predicates/)).toBeInTheDocument();
    await userEvent.click(screen.getByRole('button', { name: /Log predicate/ }));
    expect(onChange).toHaveBeenCalledWith(
      search({ mode: 'existential', spanPredicates: [p('a', '1')], logPredicates: [emptyLogPredicate()] })
    );
  });

  it('only existential mode shows log predicates', () => {
    setup(search({ mode: 'sameSpan', spanPredicates: [p('a', '1')], logPredicates: [emptyLogPredicate()] }));
    expect(screen.queryByText(/Log predicates/)).not.toBeInTheDocument();
  });

  it('commits a limit and rejects one over the cap', async () => {
    const { onChange } = setup(search({ mode: 'time' }));
    const limit = screen.getByLabelText('Limit');
    expect(limit).toHaveAttribute('placeholder', '20');
    await userEvent.type(limit, '1001');
    await userEvent.tab();
    expect(screen.getByText('At most 1000')).toBeInTheDocument();
    expect(onChange).not.toHaveBeenCalled();
    await userEvent.clear(limit);
    await userEvent.type(limit, '50{Enter}');
    expect(onChange).toHaveBeenCalledWith(search({ mode: 'time', limit: 50 }));
  });
});

describe('PredicateRow autocomplete', () => {
  it('offers typed keys of the tenant and adopts the chosen key type', async () => {
    const ds = makeDataSource();
    const keys = jest.spyOn(ds, 'getTypedAttributeKeys').mockResolvedValue({
      keys: [
        { key: 'http.method', types: ['string'] },
        { key: 'http.status_code', types: ['int'] },
      ],
      truncated: false,
    });
    const { onChange } = setup(search({ tenant: 'T1', mode: 'sameSpan', spanPredicates: [p('', 'x')] }), ds);
    const key = screen.getByLabelText('Key');
    await userEvent.click(key);
    await userEvent.type(key, 'http');
    const option = await screen.findByRole('option', { name: /http.status_code/ });
    expect(option).toHaveTextContent('int');
    expect(keys).toHaveBeenLastCalledWith({ tenant: 'T1', signal: 'span', scope: 'span', prefix: 'http', limit: 100 });
    await userEvent.click(option);
    expect(onChange).toHaveBeenLastCalledWith(
      search({ tenant: 'T1', mode: 'sameSpan', spanPredicates: [p('http.status_code', '', 'int')] })
    );
  });

  it('keeps the type and value when the chosen key has that type', async () => {
    const ds = makeDataSource();
    jest.spyOn(ds, 'getTypedAttributeKeys').mockResolvedValue({
      keys: [{ key: 'http.method', types: ['int', 'string'] }],
      truncated: false,
    });
    const { onChange } = setup(search({ mode: 'sameSpan', spanPredicates: [p('', 'GET')] }), ds);
    await userEvent.click(screen.getByLabelText('Key'));
    await userEvent.click(await screen.findByRole('option', { name: /http.method/ }));
    expect(onChange).toHaveBeenLastCalledWith(search({ mode: 'sameSpan', spanPredicates: [p('http.method', 'GET')] }));
  });

  it('offers values of the key, plus variables, and runs once complete', async () => {
    const ds = makeDataSource();
    const values = jest
      .spyOn(ds, 'getAttributeValues')
      .mockResolvedValue({ values: ['checkout', 'cart'], truncated: false });
    const pred: SpanPredicate = { scope: 'resource', key: 'service.name', type: 'string', value: '' };
    const { onChange, onRunQuery } = setup(search({ mode: 'sameSpan', spanPredicates: [pred] }), ds);
    await userEvent.click(screen.getByLabelText('Value'));
    expect(await screen.findByRole('option', { name: 'checkout' })).toBeInTheDocument();
    expect(screen.getByRole('option', { name: /\$svc/ })).toBeInTheDocument();
    expect(values).toHaveBeenCalledWith({
      tenant: undefined,
      signal: 'span',
      scope: 'resource',
      key: 'service.name',
      type: 'string',
      prefix: '',
      limit: 100,
    });
    await userEvent.click(screen.getByRole('option', { name: 'cart' }));
    expect(onChange).toHaveBeenLastCalledWith(
      search({ mode: 'sameSpan', spanPredicates: [{ ...pred, value: 'cart' }] })
    );
    expect(onRunQuery).toHaveBeenCalled();
  });

  it('offers true and false for bool without asking the backend', async () => {
    const ds = makeDataSource();
    const values = jest.spyOn(ds, 'getAttributeValues');
    setup(search({ mode: 'sameSpan', spanPredicates: [p('error', '', 'bool')] }), ds);
    await userEvent.click(screen.getByLabelText('Value'));
    expect(await screen.findByRole('option', { name: 'true' })).toBeInTheDocument();
    expect(screen.getByRole('option', { name: 'false' })).toBeInTheDocument();
    expect(values).not.toHaveBeenCalled();
  });

  it('asks no values before a key is chosen', async () => {
    const ds = makeDataSource();
    const values = jest.spyOn(ds, 'getAttributeValues');
    setup(search({ mode: 'sameSpan', spanPredicates: [p('', '')] }), ds);
    await userEvent.click(screen.getByLabelText('Value'));
    await screen.findByRole('option', { name: /\$svc/ });
    expect(values).not.toHaveBeenCalled();
  });

  it('flags a value that does not parse as its type', () => {
    setup(search({ mode: 'sameSpan', spanPredicates: [p('http.status_code', 'abc', 'int')] }));
    expect(screen.getByText('Expected an integer')).toBeInTheDocument();
  });

  it('changing the type to bool clears a non-bool value', async () => {
    const { onChange } = setup(search({ mode: 'sameSpan', spanPredicates: [p('k', 'abc')] }));
    await userEvent.click(screen.getByLabelText('Type'));
    await userEvent.click(await screen.findByRole('option', { name: 'bool' }));
    expect(onChange).toHaveBeenLastCalledWith(search({ mode: 'sameSpan', spanPredicates: [p('k', '', 'bool')] }));
  });

  it('log predicates use the log signal and fix event_name', async () => {
    const ds = makeDataSource();
    const keys = jest.spyOn(ds, 'getTypedAttributeKeys').mockResolvedValue({ keys: [], truncated: false });
    const { onChange } = setup(
      search({
        mode: 'existential',
        spanPredicates: [],
        logPredicates: [{ scope: 'log', key: 'k', type: 'int', value: '1' }],
      }),
      ds
    );
    const row = screen.getByText(/Log predicates/).parentElement!;
    await userEvent.click(within(row).getByLabelText('Key'));
    await userEvent.type(within(row).getByLabelText('Key'), 'x');
    await waitFor(() => expect(keys).toHaveBeenCalledWith(expect.objectContaining({ signal: 'log', scope: 'log' })));
    await userEvent.keyboard('{Escape}');
    await userEvent.click(within(row).getByLabelText('Scope'));
    await userEvent.click(await screen.findByRole('option', { name: 'event_name' }));
    expect(onChange).toHaveBeenLastCalledWith(
      search({
        mode: 'existential',
        spanPredicates: [],
        logPredicates: [{ scope: 'event_name', key: 'event_name', type: 'string', value: '1' }],
      })
    );
  });

  it('event_name rows have a fixed key and type', () => {
    setup(
      search({
        mode: 'existential',
        spanPredicates: [],
        logPredicates: [{ scope: 'event_name', key: 'event_name', type: 'string', value: '' }],
      })
    );
    expect(screen.getByLabelText('Key')).toBeDisabled();
    expect(screen.getByLabelText('Type')).toBeDisabled();
  });
});
