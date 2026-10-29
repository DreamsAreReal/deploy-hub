import { createWriteStream, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { attached, limits } from './config.mjs';

export const firstLine = (error) => String(error?.message ?? error).split('\n')[0];
export const nameOf = (target) => target.testid || `«${target.label}»`;
export const cell = (text) => text.replaceAll('|', String.raw`\|`).replaceAll('\n', ' ');
export const sentBy = (sent) => `${sent.url} (${sent.by})`;

export function newCrawl(config, open = createWriteStream) {
  mkdirSync(join(config.out, 'screens'), { recursive: true });
  mkdirSync(join(config.out, 'shots'), { recursive: true });
  writeFileSync(join(config.out, attached.name), attached.body);
  return {
    config, out: config.out, file: join(config.out, attached.name), cdp: null, loaded: null, actionAt: 0, log: open(join(config.out, 'crawl.log')), problems: [], notes: [],
    keys: new Set(), steps: [], seen: new Set(), visited: new Map(), found: new Map(), queue: [], pending: new Map(), clicks: 0, requests: 0, words: new Map(), opened: [],
    gaps: new Map(), testids: new Map(), extra: new Set(), tries: new Map(), hung: new Map(),
  };
}

function record(crawl, list, kind, detail) {
  const text = String(detail).slice(0, limits.detail);
  if (crawl.keys.has(`${kind} ${text}`)) return;
  crawl.keys.add(`${kind} ${text}`);
  crawl[list].push({ kind, detail: text, steps: [...crawl.steps] });
  crawl.log.write(`${list === 'problems' ? 'PROBLEM' : 'note'} ${kind}: ${text} (${crawl.steps.join(' → ')})\n`);
}

export const problem = (crawl, kind, detail) => record(crawl, 'problems', kind, detail);
export const note = (crawl, kind, detail) => record(crawl, 'notes', kind, detail);

export function tell(crawl, kind, where, items, write) {
  const shown = 3;
  if (items.length > 0) write(crawl, kind, `${where}: ${items.length} — ${items.slice(0, shown).join('; ')}`);
}

const parse = (line) => {
  try {
    return JSON.parse(line);
  } catch {
    return null;
  }
};

export function serverErrors(crawl, file) {
  if (!file || !existsSync(file)) return;
  for (const entry of readFileSync(file, 'utf8').split('\n').map(parse)) {
    if (entry?.level === 'ERROR') problem(crawl, 'server-error', JSON.stringify(entry));
  }
}

export function report(crawl) {
  const rows = (entries) => entries.map((entry) => `| ${entry.kind} | ${entry.steps.join(' → ')} | ${cell(entry.detail)} | ${entry.shot ?? ''} |`);
  const head = ['| kind | steps | detail | screenshot |', '| --- | --- | --- | --- |'];
  const counts = `${crawl.visited.size} of ${crawl.config.screens.size} screens, ${crawl.clicks} clicks, ${crawl.problems.length} problems, ${crawl.notes.length} notes`;
  const text = ['# Crawl of every screen', '', `${counts}.`, '', '## Problems', '', ...head, ...rows(crawl.problems), '', '## Notes', '', ...head, ...rows(crawl.notes), ''].join('\n');
  const indent = 2;
  writeFileSync(join(crawl.out, 'crawl.md'), text);
  writeFileSync(join(crawl.out, 'crawl.json'), JSON.stringify({ screens: [...crawl.visited.values()], clicks: crawl.clicks, problems: crawl.problems, notes: crawl.notes }, null, indent));
  return text;
}
