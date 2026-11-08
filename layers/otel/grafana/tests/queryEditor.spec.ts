import { expect, test } from '@grafana/plugin-e2e';

// Run against `docker compose up` (docker-compose.yaml) with the backend built.

test('query editor offers the four query types and a tenant', async ({ panelEditPage, readProvisionedDataSource }) => {
  const ds = await readProvisionedDataSource({ fileName: 'datasources.yml' });
  await panelEditPage.datasource.set(ds.name);
  const row = panelEditPage.getQueryEditorRow('A');
  for (const name of ['SQL', 'Trace', 'Search', 'Logs']) {
    await expect(row.getByText(name, { exact: true })).toBeVisible();
  }
  await expect(row.getByRole('combobox', { name: 'Tenant' })).toBeVisible();
});

test('trace search edits predicate rows', async ({ panelEditPage, readProvisionedDataSource }) => {
  const ds = await readProvisionedDataSource({ fileName: 'datasources.yml' });
  await panelEditPage.datasource.set(ds.name);
  const row = panelEditPage.getQueryEditorRow('A');
  await row.getByText('Search', { exact: true }).click();
  await expect(row.getByRole('combobox', { name: 'Key' })).toHaveCount(1);
  await row.getByRole('button', { name: 'Span predicate' }).click();
  await expect(row.getByRole('combobox', { name: 'Key' })).toHaveCount(2);
});

test('a trace query asks for a valid trace ID', async ({ panelEditPage, readProvisionedDataSource }) => {
  const ds = await readProvisionedDataSource({ fileName: 'datasources.yml' });
  await panelEditPage.datasource.set(ds.name);
  const row = panelEditPage.getQueryEditorRow('A');
  await row.getByText('Trace', { exact: true }).click();
  await row.getByRole('textbox', { name: 'Trace ID' }).fill('not-a-trace');
  await row.getByRole('textbox', { name: 'Trace ID' }).blur();
  await expect(row.getByText('A trace ID is 1 to 32 hexadecimal characters')).toBeVisible();
});
