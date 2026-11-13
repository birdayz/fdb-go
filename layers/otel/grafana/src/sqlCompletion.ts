// Context-aware SQL completion over sqlds' tables/columns resources. Pure:
// the Monaco adapter in components/SqlEditor.tsx turns the result into items.

export type SqlCompletionKind = 'macro' | 'variable' | 'table' | 'column' | 'keyword';

export interface SqlCompletionItem {
  label: string;
  insertText: string;
  kind: SqlCompletionKind;
  detail?: string;
  documentation?: string;
  /** insertText is a Monaco snippet ($ escaped, ${n:placeholder} tab stops). */
  snippet?: boolean;
}

export interface SqlCompletionResult {
  /** Characters before the cursor that the chosen item replaces. */
  replaceLength: number;
  items: SqlCompletionItem[];
}

export interface CompletionSources {
  tables(): Promise<string[]>;
  columns(table: string): Promise<string[]>;
  /** Dashboard template variable names, without the leading $. */
  variables(): string[];
}

interface Macro {
  name: string;
  signature: string;
  snippetArgs: string;
  doc: string;
}

// RFC grafana-datasource §6. Times are integer nanoseconds unless a unit is given.
export const MACROS: Macro[] = [
  {
    name: '$__timeFilter',
    signature: '(col[, unit])',
    snippetArgs: '(${1:start_ns})',
    doc: 'col >= <from> AND col < <to>; unit ns (default), us, ms or s.',
  },
  {
    name: '$__timeFrom',
    signature: '([unit])',
    snippetArgs: '()',
    doc: 'Dashboard range start as an integer literal.',
  },
  { name: '$__timeTo', signature: '([unit])', snippetArgs: '()', doc: 'Dashboard range end as an integer literal.' },
  {
    name: '$__timeGroup',
    signature: '(col, interval[, unit])',
    snippetArgs: '(${1:start_ns}, ${2:\\$__interval})',
    doc: '(col / step) * step: buckets col into interval steps.',
  },
  {
    name: '$__timeGroupAlias',
    signature: '(col, interval[, unit])',
    snippetArgs: '(${1:start_ns}, ${2:\\$__interval})',
    doc: '$__timeGroup in nanoseconds, AS "time".',
  },
  {
    name: '$__time',
    signature: '(col[, unit])',
    snippetArgs: '(${1:start_ns})',
    doc: 'col in nanoseconds, AS "time".',
  },
  {
    name: '$__unixEpochNanoFilter',
    signature: '(col)',
    snippetArgs: '(${1:start_ns})',
    doc: 'Alias of $__timeFilter(col).',
  },
  { name: '$__unixEpochNanoFrom', signature: '()', snippetArgs: '()', doc: 'Alias of $__timeFrom().' },
  { name: '$__unixEpochNanoTo', signature: '()', snippetArgs: '()', doc: 'Alias of $__timeTo().' },
  { name: '$__hex', signature: '(v)', snippetArgs: '(${1:hex})', doc: "X'<v>' after checking even-length hex." },
];

export const KEYWORDS = [
  'SELECT',
  'FROM',
  'WHERE',
  'AND',
  'OR',
  'NOT',
  'IN',
  'IS NULL',
  'IS NOT NULL',
  'LIKE',
  'AS',
  'JOIN',
  'LEFT JOIN',
  'ON',
  'GROUP BY',
  'ORDER BY',
  'ASC',
  'DESC',
  'LIMIT',
  'COUNT(*)',
  'DISTINCT',
  'EXPLAIN',
];

const RESERVED = new Set(
  KEYWORDS.flatMap((k) => k.split(' ')).concat(['BY', 'GROUP', 'ORDER', 'TABLE', 'KEY', 'VALUE'])
);

/** Unquoted identifiers are upper-cased by the engine, so anything else is quoted. */
export function quoteIdentifier(name: string): string {
  if (/^[A-Z_][A-Z0-9_]*$/.test(name) && !RESERVED.has(name)) {
    return name;
  }
  return `"${name.replace(/"/g, '""')}"`;
}

/** The engine's name for an identifier as written: quoted verbatim, unquoted upper-cased. */
export function normalizeIdentifier(id: string): string {
  if (id.length >= 2 && id.startsWith('"') && id.endsWith('"')) {
    return id.slice(1, -1).replace(/""/g, '"');
  }
  return id.toUpperCase();
}

const IDENT = String.raw`(?:"(?:[^"]|"")+"|[A-Za-z_][\w]*)`;
const CLAUSE_END = /\b(WHERE|GROUP|ORDER|LIMIT|ON|SELECT|HAVING|UNION)\b/i;
// Unquoted words that end a table reference instead of naming it or its alias.
const NOT_A_NAME =
  /^(WHERE|GROUP|ORDER|LIMIT|OFFSET|ON|USING|SELECT|HAVING|UNION|JOIN|LEFT|RIGHT|INNER|OUTER|CROSS|FULL|AS)$/i;

/** Tables named after FROM/JOIN (and comma-joined in a FROM list), keyed by alias and by name, both normalised. */
export function referencedTables(sql: string): Map<string, string> {
  const out = new Map<string, string>();
  const ref = new RegExp(String.raw`(${IDENT})(?:\s+(?:AS\s+)?(${IDENT}))?`, 'iy');
  const head = new RegExp(String.raw`\b(?:FROM|JOIN)\s+`, 'gi');

  while (head.exec(sql) !== null) {
    let pos = head.lastIndex;
    for (;;) {
      ref.lastIndex = pos;
      const r = ref.exec(sql);
      if (!r) {
        break;
      }
      if (NOT_A_NAME.test(r[1])) {
        break;
      }
      const table = normalizeIdentifier(r[1]);
      out.set(table, table);
      let end = ref.lastIndex;
      const alias = r[2];
      if (alias && !NOT_A_NAME.test(alias)) {
        out.set(normalizeIdentifier(alias), table);
      } else if (alias) {
        end = r.index + r[1].length;
      }
      const comma = /^\s*,\s*/.exec(sql.slice(end));
      if (!comma) {
        break;
      }
      pos = end + comma[0].length;
    }
  }
  return out;
}

export type CompletionContext =
  | { kind: 'dollar'; prefix: string }
  | { kind: 'qualified'; qualifier: string; prefix: string }
  | { kind: 'table'; prefix: string }
  | { kind: 'general'; prefix: string };

export function completionContext(textBeforeCursor: string): CompletionContext {
  const dollar = /\$\{?\w*$/.exec(textBeforeCursor);
  if (dollar) {
    return { kind: 'dollar', prefix: dollar[0] };
  }
  const qualified = new RegExp(String.raw`(${IDENT})\.("?[\w]*)$`).exec(textBeforeCursor);
  if (qualified) {
    return { kind: 'qualified', qualifier: normalizeIdentifier(qualified[1]), prefix: qualified[2] };
  }
  const prefix = /"?[\w]*$/.exec(textBeforeCursor)?.[0] ?? '';
  const before = textBeforeCursor.slice(0, textBeforeCursor.length - prefix.length);
  if (/\b(FROM|JOIN)\s+$/i.test(before)) {
    return { kind: 'table', prefix };
  }
  const fromList = /\bFROM\s+([^;]*),\s*$/i.exec(before);
  if (fromList && !CLAUSE_END.test(fromList[1])) {
    return { kind: 'table', prefix };
  }
  return { kind: 'general', prefix };
}

function macroItems(): SqlCompletionItem[] {
  return MACROS.map((m) => ({
    label: m.name,
    insertText: '\\' + m.name + m.snippetArgs,
    kind: 'macro',
    detail: m.name + m.signature,
    documentation: m.doc,
    snippet: true,
  }));
}

async function columnItems(sources: CompletionSources, table: string): Promise<SqlCompletionItem[]> {
  const cols = await sources.columns(table).catch(() => []);
  return cols.map((c) => ({ label: c, insertText: quoteIdentifier(c), kind: 'column', detail: table }));
}

export async function completeSql(
  textBeforeCursor: string,
  fullText: string,
  sources: CompletionSources
): Promise<SqlCompletionResult> {
  const ctx = completionContext(textBeforeCursor);
  switch (ctx.kind) {
    case 'dollar': {
      const vars: SqlCompletionItem[] = sources
        .variables()
        .map((v) => ({ label: '$' + v, insertText: '$' + v, kind: 'variable', detail: 'template variable' }));
      return { replaceLength: ctx.prefix.length, items: [...macroItems(), ...vars] };
    }
    case 'qualified': {
      const table = referencedTables(fullText).get(ctx.qualifier) ?? ctx.qualifier;
      return { replaceLength: ctx.prefix.length, items: await columnItems(sources, table) };
    }
    case 'table': {
      const tables = await sources.tables().catch(() => []);
      return {
        replaceLength: ctx.prefix.length,
        items: tables.map((t) => ({ label: t, insertText: quoteIdentifier(t), kind: 'table' })),
      };
    }
    case 'general': {
      const tables = [...new Set(referencedTables(fullText).values())];
      const columns = (await Promise.all(tables.map((t) => columnItems(sources, t)))).flat();
      const keywords: SqlCompletionItem[] = KEYWORDS.map((k) => ({ label: k, insertText: k, kind: 'keyword' }));
      return { replaceLength: ctx.prefix.length, items: [...columns, ...keywords] };
    }
  }
}
