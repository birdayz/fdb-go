import React, { useState } from 'react';
import { DataSourceSettings } from '@grafana/data';
import { config } from '@grafana/runtime';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { FDBOptions, FDBSecureJsonData } from '../types';
import { BUDGETS, ConfigEditor } from './ConfigEditor';

type Settings = DataSourceSettings<FDBOptions, FDBSecureJsonData>;

// A Field's label also holds its description, so a control's name starts with the label.
const fieldName = (label: string) => new RegExp('^' + label.replace(/[.*+?^${}()|[\]\\]/g, '\\$&'));
const field = (label: string) => screen.getByRole('textbox', { name: fieldName(label) });
const queryField = (label: string) => screen.queryByRole('textbox', { name: fieldName(label) });

function settings(jsonData: FDBOptions = {}, secureJsonFields: Record<string, boolean> = {}): Settings {
  // The deprecated top-level database field is left out; the editor reads jsonData.database.
  return {
    id: 1,
    uid: 'u',
    orgId: 1,
    name: 'FoundationDB',
    type: 'birdayz-foundationdb-datasource',
    typeName: 'FoundationDB',
    typeLogoUrl: '',
    access: 'proxy',
    url: '',
    user: '',
    basicAuth: false,
    basicAuthUser: '',
    isDefault: false,
    jsonData,
    secureJsonFields,
    readOnly: false,
    withCredentials: false,
  } as Settings;
}

/** Holds the options like Grafana's settings page and records every change. */
function setup(initial: Settings) {
  const changes: Settings[] = [];
  function Harness() {
    const [options, setOptions] = useState(initial);
    return (
      <ConfigEditor
        options={options}
        onOptionsChange={(o) => {
          changes.push(o);
          setOptions(o);
        }}
      />
    );
  }
  render(<Harness />);
  return { last: () => changes[changes.length - 1], changes };
}

describe('ConfigEditor cluster', () => {
  it('defaults to a cluster file path', async () => {
    const { last } = setup(settings());
    expect(screen.getByRole('radio', { name: 'Cluster file path' })).toBeChecked();
    await userEvent.type(field('Cluster file path'), '/etc/fdb.cluster');
    expect(last().jsonData).toEqual({ clusterFilePath: '/etc/fdb.cluster' });
  });

  it('switching to a cluster string clears the path', async () => {
    const { last } = setup(settings({ clusterFilePath: '/etc/fdb.cluster', database: 'd' }));
    await userEvent.click(screen.getByRole('radio', { name: 'Cluster string' }));
    expect(last().jsonData).toEqual({ database: 'd' });
    expect(queryField('Cluster file path')).not.toBeInTheDocument();
    await userEvent.type(field('Cluster string'), 'docker:docker@fdb:4500');
    expect(last().jsonData).toEqual({ database: 'd', clusterString: 'docker:docker@fdb:4500' });
  });

  it('opens on the cluster string when only that is set, and flags a malformed one', async () => {
    setup(settings({ clusterString: 'docker@fdb' }));
    expect(screen.getByRole('radio', { name: 'Cluster string' })).toBeChecked();
    expect(screen.getByText('Expected description:id@host:port[,host:port...]')).toBeInTheDocument();
  });

  it('removes an emptied field', async () => {
    const { last } = setup(settings({ clusterFilePath: '/x', database: 'd' }));
    await userEvent.clear(field('Cluster file path'));
    expect(last().jsonData).toEqual({ database: 'd' });
  });
});

describe('ConfigEditor TLS', () => {
  it('shows certificate fields only in config mode', async () => {
    const { last } = setup(settings());
    expect(screen.getAllByRole('radio', { name: 'Off' })[0]).toBeChecked();
    expect(queryField('Client key')).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole('radio', { name: 'Cluster' }));
    expect(last().jsonData.tlsMode).toBe('cluster');
    expect(queryField('Client key')).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole('radio', { name: 'Config' }));
    expect(last().jsonData.tlsMode).toBe('config');
    expect(field('CA certificate')).toBeInTheDocument();
    expect(field('Verify peers')).toBeInTheDocument();
  });

  it('writes the client certificate and key to secureJsonData only', async () => {
    const { last } = setup(settings({ tlsMode: 'config' }));
    await userEvent.type(field('Client certificate'), 'CERT');
    await userEvent.type(field('Client key'), 'KEY');
    expect(last().secureJsonData).toEqual({ tlsClientCert: 'CERT', tlsClientKey: 'KEY' });
    expect(JSON.stringify(last().jsonData)).not.toContain('KEY');
  });

  it('resets a configured secret', async () => {
    const { last } = setup(settings({ tlsMode: 'config' }, { tlsClientKey: true, tlsClientCert: true }));
    expect(field('Client key')).toBeDisabled();
    const resets = screen.getAllByRole('button', { name: 'Reset' });
    expect(resets).toHaveLength(2);
    await userEvent.click(resets[1]);
    expect(last().secureJsonFields).toEqual({ tlsClientCert: true, tlsClientKey: false });
    expect(last().secureJsonData).toEqual({ tlsClientKey: '' });
    expect(field('Client key')).toBeEnabled();
    expect(field('Client certificate')).toBeDisabled();
  });

  it('stores CA and verify-peers in jsonData, and requires the CA as the backend does', async () => {
    const { last } = setup(settings({ tlsMode: 'config' }));
    expect(screen.getByText('TLS from the settings needs a CA certificate')).toBeInTheDocument();
    await userEvent.type(field('CA certificate'), 'CA');
    expect(screen.queryByText('TLS from the settings needs a CA certificate')).not.toBeInTheDocument();
    await userEvent.type(field('Verify peers'), 'Check.Valid=0');
    expect(last().jsonData).toEqual({ tlsMode: 'config', tlsCACert: 'CA', tlsVerifyPeers: 'Check.Valid=0' });
  });
});

describe('ConfigEditor proxy', () => {
  afterEach(() => {
    config.secureSocksDSProxyEnabled = false;
  });

  it('offers the secure socks proxy only when Grafana enables it', () => {
    config.secureSocksDSProxyEnabled = false;
    const view = render(<ConfigEditor options={settings()} onOptionsChange={jest.fn()} />);
    expect(screen.queryByText(/Secure Socks Proxy/i)).not.toBeInTheDocument();
    view.unmount();
    config.secureSocksDSProxyEnabled = true;
    render(<ConfigEditor options={settings()} onOptionsChange={jest.fn()} />);
    expect(screen.getAllByText(/Secure Socks Proxy/i).length).toBeGreaterThan(0);
  });
});

describe('ConfigEditor database and tenants', () => {
  it('writes database, default tenant, allowed tenants and OTel mode', async () => {
    const { last } = setup(settings());
    await userEvent.type(field('Database'), '/OTEL/R_1');
    await userEvent.type(field('Default tenant'), 'T0');
    await userEvent.type(field('Allowed tenants'), 'T*{Enter}');
    expect(last().jsonData).toEqual({ database: '/OTEL/R_1', defaultSchema: 'T0', allowedSchemas: ['T*'] });
    await userEvent.click(screen.getAllByRole('radio', { name: 'Off' })[1]);
    expect(last().jsonData.otelMode).toBe('off');
  });

  it('flags two prefix patterns', () => {
    setup(settings({ allowedSchemas: ['T*', 'X*'] }));
    expect(screen.getByText('At most one prefix pattern')).toBeInTheDocument();
  });

  it('flags a default tenant outside the allowed ones', () => {
    setup(settings({ allowedSchemas: ['T1'], defaultSchema: 'T0' }));
    expect(screen.getByText('Not in the allowed tenants')).toBeInTheDocument();
  });
});

describe('ConfigEditor limits', () => {
  async function openLimits() {
    await userEvent.click(screen.getByText('Limits and transaction tags'));
  }

  it('shows every contract budget with its default', async () => {
    setup(settings());
    await openLimits();
    for (const b of BUDGETS) {
      expect(field(b.label)).toBeInTheDocument();
    }
    expect(field('SQL page bytes')).toHaveAttribute('placeholder', '16777216');
    expect(screen.getByText(/Default 16777216 \(16 MiB\)/)).toBeInTheDocument();
    expect(BUDGETS.map((b) => b.key).sort()).toEqual(
      [
        'logsLimit',
        'queryTimeoutSeconds',
        'searchLimit',
        'searchMaxBytes',
        'searchMaxCandidates',
        'sqlMaxRows',
        'sqlPageBytes',
        'sqlPageRows',
        'sqlStatementMemoryBytes',
        'traceMaxBytes',
        'traceMaxSpans',
        'transactionTimeoutMs',
      ].sort()
    );
  });

  it('stores a number, removes an emptied one and ignores invalid text', async () => {
    const { last, changes } = setup(settings({ sqlMaxRows: 10 }));
    await openLimits();
    const rows = field('SQL max rows');
    await userEvent.clear(rows);
    expect(last().jsonData).toEqual({});
    await userEvent.type(rows, '5000');
    expect(last().jsonData).toEqual({ sqlMaxRows: 5000 });
    const n = changes.length;
    await userEvent.type(rows, 'x');
    expect(screen.getByText('Expected a positive integer')).toBeInTheDocument();
    expect(changes.slice(n).every((c) => c.jsonData.sqlMaxRows === 5000)).toBe(true);
  });

  it('caps the search limit at 1000', async () => {
    setup(settings());
    await openLimits();
    await userEvent.type(field('Search limit'), '1001');
    expect(screen.getByText('At most 1000')).toBeInTheDocument();
  });

  it('writes transaction tags', async () => {
    const { last } = setup(settings());
    await openLimits();
    await userEvent.type(field('Transaction tags'), 'grafana{Enter}');
    expect(last().jsonData).toEqual({ transactionTags: ['grafana'] });
  });
});
