import type { Locator, Page } from '@playwright/test';

// Shared by the e2e specs: the provisioned datasource and the seed's demo data
// (cmd/seed: TraceID(tenant, i) = 5eed<tenant><i+1>, newest trace first).

export const DS_UID = 'fdb-dev';
export const DS_TYPE = 'birdayz-foundationdb-datasource';

/** The newest trace of T0: frontend "POST /chat" with chat, tool and database spans. */
export const TRACE_T0 = '5eed0000000000000000000000000001';
/** Its chat span, which carries the evaluation log. */
export const CHAT_SPAN_T0 = '0000000000000002';
/** The newest checkout trace of T0 (trace 1: services rotate frontend, checkout, llm-gateway). */
export const CHECKOUT_TRACE_T0 = '5eed0000000000000000000000000002';

/** An Explore URL that opens with one query of this datasource over the last 6 hours. */
export function exploreUrl(query: Record<string, unknown>): string {
  const panes = {
    a: {
      datasource: DS_UID,
      queries: [{ refId: 'A', datasource: { type: DS_TYPE, uid: DS_UID }, ...query }],
      range: { from: 'now-6h', to: 'now' },
    },
  };
  return `/explore?schemaVersion=1&orgId=1&panes=${encodeURIComponent(JSON.stringify(panes))}`;
}

/**
 * Selects a RadioButtonGroup option. Grafana 13 lays the radio input over its
 * label, Grafana 12 hides it; forcing the check works on both. The first match
 * is taken: the query type group precedes the SQL format group ("Logs" is in both).
 */
export async function pickRadio(scope: Locator | Page, name: string): Promise<void> {
  await scope.getByRole('radio', { name, exact: true }).first().check({ force: true });
}
