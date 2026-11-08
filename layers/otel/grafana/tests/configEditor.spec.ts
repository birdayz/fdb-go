import { expect, test } from '@grafana/plugin-e2e';

import { FDBOptions, FDBSecureJsonData } from '../src/types';

// Run against `docker compose up` (docker-compose.yaml) with the backend built.

test('config editor renders the cluster, database and TLS settings', async ({
  createDataSourceConfigPage,
  readProvisionedDataSource,
  page,
}) => {
  const ds = await readProvisionedDataSource({ fileName: 'datasources.yml' });
  await createDataSourceConfigPage({ type: ds.type });
  await expect(page.getByRole('textbox', { name: /^Cluster file path/ })).toBeVisible();
  await expect(page.getByRole('textbox', { name: /^Database/ })).toBeVisible();
  await page.getByText('Config', { exact: true }).click();
  await expect(page.getByRole('textbox', { name: /^Client key/ })).toBeVisible();
});

test('"Save & test" reports an unreachable cluster', async ({
  createDataSourceConfigPage,
  readProvisionedDataSource,
  page,
}) => {
  const ds = await readProvisionedDataSource<FDBOptions, FDBSecureJsonData>({ fileName: 'datasources.yml' });
  const configPage = await createDataSourceConfigPage({ type: ds.type });
  await page.getByText('Cluster string', { exact: true }).click();
  await page.getByRole('textbox', { name: /^Cluster string/ }).fill('docker:docker@127.0.0.1:1');
  await page.getByRole('textbox', { name: /^Database/ }).fill(ds.jsonData.database ?? '');
  await expect(configPage.saveAndTest()).not.toBeOK();
});
