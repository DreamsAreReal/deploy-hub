import { test } from 'node:test';
import assert from 'node:assert/strict';
import fc from 'fast-check';
import { existsSync, mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { cell, firstLine, nameOf, newCrawl, note, problem, report, sentBy, serverErrors, tell } from '../record.mjs';

function memoryLog() {
  const lines = [];
  return { lines, write: (text) => lines.push(text), end: () => lines.push('END') };
}

function crawlIn(screens = new Map()) {
  const out = mkdtempSync(join(tmpdir(), 'crawl-record-'));
  const log = memoryLog();
  const crawl = newCrawl({ out, screens }, () => log);
  return { crawl, log, out };
}

test('a new crawl lays out its folders and the sample file it attaches', () => {
  const { crawl, out } = crawlIn();
  assert.ok(existsSync(join(out, 'screens')) && existsSync(join(out, 'shots')));
  assert.equal(crawl.file, join(out, 'crawl-sample.md'));
  assert.match(readFileSync(crawl.file, 'utf8'), /^# Crawl/);
  assert.equal(crawl.out, out);
  assert.deepEqual([crawl.problems, crawl.notes, crawl.queue], [[], [], []]);
  const real = newCrawl({ out, screens: new Map() });
  real.log.end();
});

test('a finding is kept once with the steps that led to it and is logged', () => {
  const { crawl, log } = crawlIn();
  crawl.steps = ['open /ui', 'click add'];
  problem(crawl, 'js-error', 'boom');
  problem(crawl, 'js-error', 'boom');
  note(crawl, 'alert', 'saved');
  crawl.steps.push('later');
  assert.deepEqual(crawl.problems, [{ kind: 'js-error', detail: 'boom', steps: ['open /ui', 'click add'] }]);
  assert.deepEqual(crawl.notes, [{ kind: 'alert', detail: 'saved', steps: ['open /ui', 'click add'] }]);
  assert.deepEqual(log.lines, ['PROBLEM js-error: boom (open /ui → click add)\n', 'note alert: saved (open /ui → click add)\n']);
});

test('a finding is cut to 300 characters and dedupes on the cut text', () => {
  const { crawl } = crawlIn();
  problem(crawl, 'cut-text', `${'x'.repeat(300)}a`);
  problem(crawl, 'cut-text', `${'x'.repeat(300)}b`);
  problem(crawl, 'other', 7);
  assert.deepEqual(crawl.problems.map((entry) => entry.detail.length), [300, 1]);
});

test('a check with findings is told with the first three of them', () => {
  const { crawl } = crawlIn();
  tell(crawl, 'cut-text', '/ui', [], problem);
  tell(crawl, 'cut-text', '/ui', ['a', 'b', 'c', 'd'], problem);
  assert.deepEqual(crawl.problems.map((entry) => entry.detail), ['/ui: 4 — a; b; c']);
});

test('server errors in the log become problems, other lines do not', () => {
  const { crawl, out } = crawlIn();
  serverErrors(crawl, '');
  serverErrors(crawl, join(out, 'missing.log'));
  const file = join(out, 'server.log');
  writeFileSync(file, ['{"level":"INFO","msg":"ok"}', 'not json', '{"level":"ERROR","msg":"db locked"}', '', 'null'].join('\n'));
  serverErrors(crawl, file);
  assert.deepEqual(crawl.problems, [{ kind: 'server-error', detail: '{"level":"ERROR","msg":"db locked"}', steps: [] }]);
});

test('the report counts the crawl and lists problems and notes as table rows', () => {
  const { crawl, out } = crawlIn(new Map([['/ui', {}], ['/ui/x', {}]]));
  crawl.visited.set('/ui', '/ui');
  crawl.clicks = 4;
  crawl.steps = ['open /ui'];
  problem(crawl, 'cut-text', 'a | b\nc');
  crawl.problems[0].shot = 'shots/001.png';
  note(crawl, 'alert', 'hi');
  const text = report(crawl);
  assert.equal(text, readFileSync(join(out, 'crawl.md'), 'utf8'));
  assert.match(text, /^# Crawl of every screen\n\n1 of 2 screens, 4 clicks, 1 problems, 1 notes\.\n\n## Problems\n/);
  assert.ok(text.includes('| cut-text | open /ui | a \\| b c | shots/001.png |\n'));
  assert.ok(text.endsWith('## Notes\n\n| kind | steps | detail | screenshot |\n| --- | --- | --- | --- |\n| alert | open /ui | hi |  |\n'));
  const json = JSON.parse(readFileSync(join(out, 'crawl.json'), 'utf8'));
  assert.deepEqual([json.screens, json.clicks, json.problems.length, json.notes.length], [['/ui'], 4, 1, 1]);
  assert.ok(readFileSync(join(out, 'crawl.json'), 'utf8').includes('\n  "screens"'));
});

test('a table cell never breaks the table', () => {
  fc.assert(fc.property(fc.string(), (text) => {
    const escaped = cell(text);
    assert.ok(!escaped.includes('\n'));
    assert.equal(escaped.replaceAll(String.raw`\|`, '').includes('|'), false);
  }));
});

test('errors, targets and requests are named for people', () => {
  assert.equal(firstLine(new Error('one\ntwo')), 'one');
  assert.equal(firstLine('plain\nrest'), 'plain');
  assert.equal(firstLine(null), 'null');
  assert.equal(nameOf({ testid: 'board-add', label: 'Add' }), 'board-add');
  assert.equal(nameOf({ testid: '', label: 'Add' }), '«Add»');
  assert.equal(sentBy({ url: '/api', by: 'fetch by script' }), '/api (fetch by script)');
});
