import React from 'react';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { makeDataSource } from '../testUtils';
import { FDBVariableQuery } from '../variables';
import { VariableQueryEditor } from './VariableQueryEditor';

jest.mock('@grafana/runtime', () => ({
  ...jest.requireActual('@grafana/runtime'),
  getTemplateSrv: () => jest.requireActual('../testUtils').mockTemplateSrv,
}));

jest.mock('@grafana/ui', () => {
  const R = jest.requireActual('react');
  return {
    ...jest.requireActual('@grafana/ui'),
    CodeEditor: (props: { value: string; onBlur?: (v: string) => void }) =>
      R.createElement('textarea', {
        'aria-label': 'SQL editor',
        defaultValue: props.value,
        onBlur: (e: { currentTarget: { value: string } }) => props.onBlur?.(e.currentTarget.value),
      }),
  };
});

function setup(query: FDBVariableQuery | string) {
  const onChange = jest.fn();
  render(
    <VariableQueryEditor
      query={query as FDBVariableQuery}
      datasource={makeDataSource()}
      onChange={onChange}
      onRunQuery={jest.fn()}
    />
  );
  return { onChange };
}

describe('VariableQueryEditor', () => {
  it('lists tenants by default, with no other fields', () => {
    setup({ refId: 'V' } as FDBVariableQuery);
    expect(screen.getByRole('radio', { name: 'Tenants' })).toBeChecked();
    expect(screen.queryByLabelText('Tenant')).not.toBeInTheDocument();
    expect(screen.queryByLabelText('Scope')).not.toBeInTheDocument();
  });

  it('switches kind', async () => {
    const { onChange } = setup({ refId: 'V', kind: 'tenants' });
    await userEvent.click(screen.getByRole('radio', { name: 'Attribute values' }));
    expect(onChange).toHaveBeenCalledWith({ refId: 'V', kind: 'attributeValues' });
  });

  it('edits an attribute-values query', async () => {
    const { onChange } = setup({ refId: 'V', kind: 'attributeValues', scope: 'resource' });
    expect(screen.getByLabelText('Tenant')).toBeInTheDocument();
    await userEvent.type(screen.getByLabelText('Key'), 'service.name');
    await userEvent.tab();
    expect(onChange).toHaveBeenLastCalledWith({
      refId: 'V',
      kind: 'attributeValues',
      scope: 'resource',
      key: 'service.name',
    });
    await userEvent.type(screen.getByLabelText('Limit'), '10');
    await userEvent.tab();
    expect(onChange).toHaveBeenLastCalledWith({ refId: 'V', kind: 'attributeValues', scope: 'resource', limit: 10 });
  });

  it('switching the signal keeps a scope both signals have and drops one they do not', async () => {
    const { onChange } = setup({ refId: 'V', kind: 'attributeKeys', signal: 'span', scope: 'span' });
    await userEvent.click(screen.getByRole('radio', { name: 'Logs' }));
    expect(onChange).toHaveBeenLastCalledWith({ refId: 'V', kind: 'attributeKeys', signal: 'log', scope: undefined });
  });

  it('offers log scopes without event_name', async () => {
    setup({ refId: 'V', kind: 'attributeKeys', signal: 'log' });
    await userEvent.click(screen.getByLabelText('Scope'));
    expect((await screen.findAllByRole('option')).map((o) => o.textContent)).toEqual(['resource', 'scope', 'log']);
  });

  it('attribute keys may be of any type', async () => {
    const { onChange } = setup({ refId: 'V', kind: 'attributeKeys', type: 'int' });
    await userEvent.click(screen.getByLabelText('Type'));
    await userEvent.click(await screen.findByRole('option', { name: 'any' }));
    expect(onChange).toHaveBeenLastCalledWith({ refId: 'V', kind: 'attributeKeys', type: '' });
  });

  it('attribute values need one type', async () => {
    setup({ refId: 'V', kind: 'attributeValues' });
    expect(screen.getByLabelText('Type')).toHaveValue('string');
    await userEvent.click(screen.getByLabelText('Type'));
    const options = (await screen.findAllByRole('option')).map((o) => o.textContent);
    expect(options).toEqual(['string', 'int', 'double', 'bool']);
  });

  it('edits SQL and reads a legacy string query as SQL', async () => {
    const { onChange } = setup('SELECT 1');
    expect(screen.getByRole('radio', { name: 'SQL' })).toBeChecked();
    const sql = screen.getByLabelText('SQL editor');
    await userEvent.clear(sql);
    await userEvent.type(sql, 'SELECT 2');
    sql.blur();
    expect(onChange).toHaveBeenLastCalledWith({ refId: 'variable', kind: 'sql', rawSql: 'SELECT 2' });
  });
});
