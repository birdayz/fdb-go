import { expect, test } from '@grafana/plugin-e2e';

import { DS_TYPE, DS_UID, pickRadio } from './fdb';

// Against `docker compose up` (docker-compose.yaml): FDB seeded by cmd/seed.

test('"Save & test" is green on the provisioned datasource', async ({ gotoDataSourceConfigPage }) => {
  const configPage = await gotoDataSourceConfigPage(DS_UID);
  await expect(configPage.saveAndTest()).toBeOK();
  await expect(configPage).toHaveAlert('success', { hasText: /SELECT 1 ok.*OTEL_V1 store, messages=deltas/ });
});

test('config editor renders the cluster, database and TLS settings', async ({ createDataSourceConfigPage, page }) => {
  await createDataSourceConfigPage({ type: DS_TYPE });
  await expect(page.getByRole('textbox', { name: /^Cluster file path/ })).toBeVisible();
  await expect(page.getByRole('textbox', { name: /^Database/ })).toBeVisible();
  await pickRadio(page, 'Config');
  await expect(page.getByRole('textbox', { name: /^Client key/ })).toBeVisible();
});

test('"Save & test" names the failing step for an unreachable cluster', async ({
  createDataSourceConfigPage,
  page,
}) => {
  test.setTimeout(90_000);
  const configPage = await createDataSourceConfigPage({ type: DS_TYPE });
  await pickRadio(page, 'Cluster string');
  await page.getByRole('textbox', { name: /^Cluster string/ }).fill('docker:docker@127.0.0.1:1');
  await page.getByRole('textbox', { name: /^Database/ }).fill('/OTEL/R_GRAFANA_DEV');
  // A short query timeout bounds the health check's coordinator bootstrap.
  await page.getByText('Limits and transaction tags').click();
  await page.getByRole('textbox', { name: /^Query timeout/ }).fill('5');
  await expect(configPage.saveAndTest()).not.toBeOK();
  await expect(configPage).toHaveAlert('error', { hasText: /failed/ });
});
