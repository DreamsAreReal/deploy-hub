import { test } from 'node:test';
import assert from 'node:assert/strict';
import { existsSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { holdStill, letGo, pixelDiff } from '../page.mjs';
import { compareShots, screenshot } from '../visual.mjs';
import { evaluated, fakeCrawl, methods } from './fake.mjs';

const png = Buffer.from('fake png');

test('a screenshot is written as taken, holding animations and carets still when the clock is frozen', async () => {
  const shots = [{ data: png.toString('base64') }];
  const { crawl, out } = fakeCrawl((message) => (message.method === 'Page.captureScreenshot' ? shots.shift() ?? Promise.reject(new Error('gone')) : evaluated(true)));
  await screenshot(crawl, 'screens/home.png');
  assert.deepEqual(readFileSync(join(out, 'screens/home.png')), png);
  assert.deepEqual(methods(crawl), ['Page.captureScreenshot']);
  await screenshot(crawl, 'screens/lost.png');
  assert.equal(existsSync(join(out, 'screens/lost.png')), false);
  const frozen = fakeCrawl((message) => (message.method === 'Page.captureScreenshot' ? { data: png.toString('base64') } : evaluated(true)), { frozenAt: 1 });
  await screenshot(frozen.crawl, 'screens/still.png');
  const sent = frozen.crawl.cdp.socket.sent;
  assert.deepEqual(sent.map((message) => message.method), ['Runtime.evaluate', 'Page.captureScreenshot', 'Runtime.evaluate']);
  assert.ok(sent[0].params.expression.startsWith(`(${holdStill})`) && sent[2].params.expression.startsWith(`(${letGo})`));
  const unheld = fakeCrawl((message) => (message.method === 'Page.captureScreenshot' ? { data: '' } : evaluated(false)), { frozenAt: 1 });
  await screenshot(unheld.crawl, 'screens/moving.png');
  assert.deepEqual(methods(unheld.crawl), ['Runtime.evaluate', 'Page.captureScreenshot']);
});

function comparing(results, accepted = [], files = ['board-360.png', 'board-390-dark.png', 'card.png', 'memory-1280.png', 'memory.png', 'new.png', 'notes.txt']) {
  const baseline = mkdtempSync(join(tmpdir(), 'crawl-baseline-'));
  for (const file of files.filter((name) => name !== 'new.png')) writeFileSync(join(baseline, file), `before ${file}`);
  const made = fakeCrawl((message) => {
    if (message.method === 'Page.navigate') {
      setImmediate(() => made.crawl.loaded());
      return { frameId: 'F' };
    }
    const file = Object.keys(results).find((name) => message.params.expression.includes(Buffer.from(`before ${name}`).toString('base64')));
    return results[file] === undefined ? Promise.reject(new Error('no canvas')) : evaluated(results[file]);
  }, { baseline, accepted: new Set(accepted) });
  for (const file of files) writeFileSync(join(made.out, 'screens', file), `after ${file}`);
  return { ...made, baseline };
}

const total = 100_000;

test('a shot within the tolerance matches, a changed one is kept with its diff and fails unless accepted', async () => {
  const diff = Buffer.from('red pixels').toString('base64');
  const { crawl, out } = comparing({
    'board-360.png': { differ: 200, total, diff: '' },
    'board-390-dark.png': { differ: 201, total, diff },
    'card.png': { differ: 40, total: 100, diff: '' },
    'memory-1280.png': { differ: 41, total: 100, diff: '' },
    'memory.png': { differ: 0, total, diff: '' },
  });
  await compareShots(crawl);
  assert.deepEqual(crawl.problems.map((entry) => entry.detail), [
    `screens/board-390-dark.png: 201 of ${total} pixels differ from the deployed version (visual/board-390-dark-diff.png); if intended, list «board» with its task in .gate/visual.yaml`,
    'screens/memory-1280.png: 41 of 100 pixels differ from the deployed version (visual/memory-1280-diff.png); if intended, list «memory» with its task in .gate/visual.yaml',
  ]);
  assert.deepEqual(crawl.problems[0].steps, ['compare with the deployed version']);
  assert.equal(readFileSync(join(out, 'visual', 'board-390-dark-before.png'), 'utf8'), 'before board-390-dark.png');
  assert.equal(readFileSync(join(out, 'visual', 'board-390-dark-diff.png'), 'utf8'), 'red pixels');
  assert.equal(existsSync(join(out, 'visual', 'memory-1280-diff.png')), false);
  assert.equal(existsSync(join(out, 'visual', 'board-360-before.png')), false);
  const pairs = crawl.cdp.socket.sent.filter((message) => message.method === 'Runtime.evaluate');
  assert.equal(pairs.length, 5);
  assert.ok(pairs[0].params.expression.startsWith(`(${pixelDiff})({"before":"${Buffer.from('before board-360.png').toString('base64')}","after":"${Buffer.from('after board-360.png').toString('base64')}","tolerance":24})`));
  assert.equal(crawl.cdp.socket.sent[0].method, 'Page.navigate');
});

test('an accepted screen may change but must change, and a broken comparison is a problem', async () => {
  const { crawl } = comparing({
    'board-360.png': { differ: 500, total, diff: '' },
    'memory.png': { differ: 0, total, diff: '' },
    'memory-1280.png': { differ: 0, total, diff: '' },
  }, ['board', 'memory', 'card', 'unknown']);
  await compareShots(crawl);
  assert.deepEqual(crawl.problems.map((entry) => entry.kind), ['compare-failed', 'compare-failed', 'visual-accept-stale']);
  assert.deepEqual(crawl.problems.map((entry) => entry.detail), [
    'screens/board-390-dark.png could not be compared with the deployed version',
    'screens/card.png could not be compared with the deployed version',
    '«memory» is listed in .gate/visual.yaml, but every shot of it matches the deployed version: remove the line',
  ]);
});

test('without a baseline nothing is compared, and a baseline that never came is a note', async () => {
  const none = fakeCrawl();
  await compareShots(none.crawl);
  assert.deepEqual(methods(none.crawl), []);
  const missing = fakeCrawl(undefined, { baseline: '/nowhere/baseline' });
  await compareShots(missing.crawl);
  assert.deepEqual(missing.crawl.notes.map((entry) => `${entry.kind}: ${entry.detail}`), ['no-baseline: no shots of the deployed version yet: nothing to compare with']);
  assert.deepEqual(methods(missing.crawl), []);
  const dir = mkdtempSync(join(tmpdir(), 'crawl-empty-'));
  mkdirSync(join(dir, 'x'));
  const empty = fakeCrawl(() => Promise.reject(new Error('blank failed')), { baseline: dir });
  await compareShots(empty.crawl);
  assert.deepEqual([empty.crawl.problems, empty.crawl.notes], [[], []]);
});

test('an accepted screen with one changed shot is not stale, and comparing twice reuses the visual folder', async () => {
  const { crawl } = comparing({
    'board-360.png': { differ: 500, total, diff: '' },
    'board-390-dark.png': { differ: 0, total, diff: '' },
  }, ['board'], ['board-360.png', 'board-390-dark.png']);
  await compareShots(crawl);
  await compareShots(crawl);
  assert.deepEqual(crawl.problems, []);
});
