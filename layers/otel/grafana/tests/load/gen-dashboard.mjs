// Writes provisioning/dashboards/fdb-load.json, the R1 load dashboard: the 20
// panels of panels.mjs on a 4-column grid, refreshing every 5 s over 6 hours.
import { writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import * as prettier from 'prettier';

import { panels } from './panels.mjs';

const ds = { type: 'birdayz-foundationdb-datasource', uid: 'fdb-dev' };
const dashboard = {
  uid: 'fdb-load',
  title: 'FoundationDB load (R1)',
  tags: ['foundationdb', 'load'],
  timezone: 'utc',
  schemaVersion: 39,
  version: 1,
  refresh: '5s',
  time: { from: 'now-6h', to: 'now' },
  panels: panels.map((p, i) => ({
    id: i + 1,
    title: p.title,
    type: p.type,
    datasource: ds,
    gridPos: { x: (i % 4) * 6, y: Math.floor(i / 4) * 7, w: 6, h: 7 },
    targets: [{ refId: 'A', datasource: ds, ...p.target, ...(p.tenantT1 ? { tenant: 'T1' } : {}) }],
  })),
};
const out = join(dirname(fileURLToPath(import.meta.url)), '../../provisioning/dashboards/fdb-load.json');
// Formatted as `prettier --check` (part of lint:fix) expects it.
writeFileSync(out, await prettier.format(JSON.stringify(dashboard), { parser: 'json' }));
console.log(`wrote ${out}: ${dashboard.panels.length} panels`);
