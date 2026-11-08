import { DataFrame, DataQueryError, FieldType, PanelData } from '@grafana/data';

/** The macro-expanded SQL the backend ran for this query, from the last response's frame meta. */
export function executedQuery(data: PanelData | undefined, refId: string): string | undefined {
  for (const frame of data?.series ?? []) {
    if (frame.refId === refId && frame.meta?.executedQueryString) {
      return frame.meta.executedQueryString;
    }
  }
  return undefined;
}

/** EXPLAIN of the statement; the read-only driver admits EXPLAIN of a SELECT only. */
export function explainSql(rawSql: string): string {
  const sql = rawSql.trim().replace(/;+\s*$/, '');
  return /^EXPLAIN\b/i.test(sql) ? sql : `EXPLAIN ${sql}`;
}

/** Plain text of result frames: one line per row, string-like fields tab-separated. */
export function framesToText(frames: DataFrame[]): string {
  const lines: string[] = [];
  for (const frame of frames) {
    const fields = frame.fields.filter((f) => f.type !== FieldType.time);
    for (let row = 0; row < frame.length; row++) {
      lines.push(fields.map((f) => formatCell(f.values[row])).join('\t'));
    }
  }
  return lines.join('\n');
}

function formatCell(v: unknown): string {
  if (v === null || v === undefined) {
    return '';
  }
  return typeof v === 'object' ? JSON.stringify(v) : String(v);
}

export function errorText(errors: DataQueryError[] | undefined): string | undefined {
  const msgs = (errors ?? []).map((e) => e.message ?? e.data?.message ?? 'query failed');
  return msgs.length ? [...new Set(msgs)].join('\n') : undefined;
}
