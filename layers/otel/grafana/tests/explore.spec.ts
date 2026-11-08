import { expect, test } from '@grafana/plugin-e2e';

import { exploreUrl } from './fdb';

// SQL in Explore against the seeded run (cmd/seed): a table, a time series and
// the read-only refusal.

test('SQL table', async ({ page }) => {
  const done = page.waitForResponse((r) => r.url().includes('/api/ds/query'));
  await page.goto(
    exploreUrl({
      queryType: 'sql',
      format: 1,
      rawSql:
        'SELECT service_name, name, start_ns FROM spans WHERE $__timeFilter(start_ns) ORDER BY start_ns DESC LIMIT 20',
    })
  );
  expect((await done).ok()).toBeTruthy();
  await expect(page.getByText('SERVICE_NAME', { exact: true })).toBeVisible();
  await expect(page.getByText('SELECT documents').first()).toBeVisible();
  await expect(page.getByText('frontend', { exact: true }).first()).toBeVisible();
});

test('SQL time series with macros', async ({ page, explorePage }) => {
  const done = page.waitForResponse((r) => r.url().includes('/api/ds/query'));
  await page.goto(
    exploreUrl({
      queryType: 'sql',
      format: 0,
      rawSql:
        'SELECT $__timeGroupAlias(start_ns, 10m), COUNT(*) AS spans FROM spans WHERE $__timeFilter(start_ns) GROUP BY $__timeGroup(start_ns, 10m) ORDER BY 1',
    })
  );
  const res = await done;
  expect(res.ok()).toBeTruthy();
  const body = await res.json();
  const frame = body.results.A.frames[0];
  expect(frame.schema.fields.map((f: { type: string }) => f.type)).toEqual(['time', 'number']);
  expect(frame.data.values[0].length).toBeGreaterThan(10);
  await expect(explorePage.timeSeriesPanel.locator).toBeVisible();
});

test('SQL typed into the editor runs', async ({ page, explorePage }) => {
  await page.goto(exploreUrl({ queryType: 'sql', format: 1, rawSql: '' }));
  const row = explorePage.getQueryEditorRow('A');
  const editor = row.locator('.monaco-editor[role="code"]');
  await editor.locator('.view-lines').click();
  await page.keyboard.type("SELECT COUNT(*) AS n FROM spans WHERE service_name = 'checkout'");
  await expect(editor).toContainText("service_name = 'checkout'");
  // plugin-e2e's runQuery() selector finds no run button in Grafana 12.0; the role does in both.
  const done = page.waitForResponse((r) => r.url().includes('/api/ds/query'));
  await page.getByRole('button', { name: 'Run query' }).first().click();
  const res = await done;
  expect(res.ok()).toBeTruthy();
  const body = await res.json();
  // 200 traces per tenant, services rotating over three, four spans per trace.
  expect(body.results.A.frames[0].data.values[0][0]).toBe(67 * 4);
});

test('a write is refused as read-only', async ({ page }) => {
  const done = page.waitForResponse((r) => r.url().includes('/api/ds/query'));
  await page.goto(exploreUrl({ queryType: 'sql', format: 1, rawSql: 'DELETE FROM spans' }));
  expect((await done).ok()).toBeFalsy();
  await expect(page.getByText(/0A000: read-only connection/).first()).toBeVisible();
});
