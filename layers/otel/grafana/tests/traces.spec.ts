import { expect, test } from '@grafana/plugin-e2e';

import { CHAT_SPAN_T0, CHECKOUT_TRACE_T0, exploreUrl, TRACE_T0 } from './fdb';

// Traces and logs against the seeded run (cmd/seed): TraceView, search, and the
// links between spans and logs.

const spanNames = ['POST /chat', 'chat gpt-4o', 'tool search', 'SELECT documents'];

test('a trace by ID renders its spans in TraceView', async ({ page }) => {
  await page.goto(exploreUrl({ queryType: 'trace', traceId: TRACE_T0 }));
  for (const name of spanNames) {
    await expect(page.getByText(name, { exact: true }).first()).toBeVisible();
  }
  await expect(page.locator(`[data-item-key^="${TRACE_T0}--"][data-item-key$="--bar"]`)).toHaveCount(4);
});

test('a search lists traces and opens one', async ({ page }) => {
  await page.goto(
    exploreUrl({
      queryType: 'traceSearch',
      mode: 'attribute',
      spanPredicates: [{ scope: 'resource', key: 'service.name', type: 'string', value: 'checkout' }],
      limit: 5,
    })
  );
  const first = page.getByRole('link', { name: CHECKOUT_TRACE_T0, exact: true });
  await expect(first).toBeVisible();
  await expect(page.getByRole('link', { name: /^5eed0{24}[0-9a-f]{4}$/ })).toHaveCount(5);
  // The ID overflows its narrow column under the next cell: click its start.
  await first.click({ position: { x: 8, y: 6 } });
  // The link opens the trace in a second pane.
  await expect(page.locator(`[data-item-key^="${CHECKOUT_TRACE_T0}--"][data-item-key$="--bar"]`)).toHaveCount(4);
  await expect(page.getByText('checkout: POST /chat')).toBeVisible();
});

test('a span links to its logs', async ({ page }) => {
  await page.goto(exploreUrl({ queryType: 'trace', traceId: TRACE_T0 }));
  const row = page.locator(`[data-item-key="${TRACE_T0}--${CHAT_SPAN_T0}--bar"]`);
  await row.getByTestId('SpanLinksMenu').getByRole('button').click();
  await page.getByText('Logs for this span', { exact: true }).click();
  // The chat span has one log, the evaluation; the request log belongs to the root.
  await expect(page.getByText('evaluation of answer 0').first()).toBeVisible();
  await expect(page.getByText('request 0 received')).toHaveCount(0);
});

test('a log links to its trace', async ({ page }) => {
  await page.goto(exploreUrl({ queryType: 'logs', traceId: TRACE_T0 }));
  await page.getByText('request 0 received').first().click();
  await page.getByText(`Trace: ${TRACE_T0}`).first().click();
  await expect(page.locator(`[data-item-key^="${TRACE_T0}--"][data-item-key$="--bar"]`)).toHaveCount(4);
});
