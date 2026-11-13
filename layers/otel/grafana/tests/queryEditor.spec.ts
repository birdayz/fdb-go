import { expect, test } from '@grafana/plugin-e2e';

import { DS_UID, pickRadio } from './fdb';

// The query editor in a dashboard panel, against `docker compose up`.

test('query editor offers the four query types and a tenant', async ({ panelEditPage }) => {
  await panelEditPage.datasource.set('FoundationDB');
  const row = panelEditPage.getQueryEditorRow('A');
  for (const name of ['SQL', 'Trace', 'Search', 'Logs']) {
    await expect(row.getByRole('radio', { name, exact: true }).first()).toBeAttached();
  }
  await expect(row.getByRole('radio', { name: 'SQL', exact: true })).toBeChecked();
  await expect(row.locator('#A-tenant')).toBeVisible();
});

test('trace search edits predicate rows with autocomplete from the tenant', async ({ panelEditPage, page }) => {
  await panelEditPage.datasource.set('FoundationDB');
  const row = panelEditPage.getQueryEditorRow('A');
  await pickRadio(row, 'Search');
  await expect(row.locator('[id^="A-span-"][id$="-key"]')).toHaveCount(1);
  await row.getByRole('button', { name: 'Span predicate' }).click();
  await expect(row.locator('[id^="A-span-"][id$="-key"]')).toHaveCount(2);
  // Key autocomplete reads the attribute-keys resource of T0.
  const keys = page.waitForResponse((r) => r.url().includes(`/api/datasources/uid/${DS_UID}/resources/attribute-keys`));
  await row.locator('#A-span-0-key').click();
  await row.locator('#A-span-0-key').fill('gen_ai.re');
  expect((await keys).ok()).toBeTruthy();
  await expect(page.getByRole('option', { name: /gen_ai\.request\.model/ })).toBeVisible();
});

test('a trace query asks for a valid trace ID', async ({ panelEditPage }) => {
  await panelEditPage.datasource.set('FoundationDB');
  const row = panelEditPage.getQueryEditorRow('A');
  await pickRadio(row, 'Trace');
  await row.getByRole('textbox', { name: 'Trace ID' }).fill('not-a-trace');
  await row.getByRole('textbox', { name: 'Trace ID' }).blur();
  await expect(row.getByText('A trace ID is 1 to 32 hexadecimal characters')).toBeVisible();
});
