import { test } from 'node:test';
import assert from 'node:assert/strict';
import fc from 'fast-check';
import {
  discover, fillFirst, guess, guessMissing, keyOf, leadsTo, missing, placeholder, schedule, seen, shape, skipped, testidPattern, widen,
} from '../routes.mjs';

const screen = (id, route, elements = []) => ({ id, route, kind: 'screen', elements });
const board = screen('board', '/ui/epics/{id}/tasks', [{ testid: 'task-{id}' }, { testid: 'board-add' }]);
const home = screen('home', '/ui/home');

function crawlOf(screens = [home, board], forbidden = []) {
  return {
    config: { screens: new Map(screens.map((each) => [shape(each.route), each])), forbidden },
    visited: new Map(), found: new Map(), queue: [], testids: new Map(), extra: new Set(), tries: new Map(),
  };
}

test('a route keeps its fixed parts and loses ids, placeholders and the query', () => {
  assert.equal(shape('/ui/epics/7/tasks?today=2030-01-15'), '/ui/epics/N/tasks');
  assert.equal(shape('/ui/epics/{id}/tasks'), '/ui/epics/N/tasks');
  assert.equal(shape('/ui/v2/home'), '/ui/v2/home');
  assert.ok(placeholder('{id}') && placeholder('42') && !placeholder('v2') && !placeholder('4a'));
});

test('a shaped route is a fixed point and has no digits-only part', () => {
  fc.assert(fc.property(fc.array(fc.oneof(fc.nat().map(String), fc.stringMatching(/^[a-z][a-z0-9-]{0,8}$/), fc.constant('{id}'))), (parts) => {
    const once = shape(`/${parts.join('/')}`);
    assert.equal(shape(once), once);
    assert.ok(once.split('/').every((part) => !/^\d+$/.test(part)));
  }));
});

test('a testid without placeholders matches only itself', () => {
  fc.assert(fc.property(fc.string(), fc.string(), (text, other) => {
    fc.pre(text.split('-').every((part) => !part.startsWith('{')));
    const exact = testidPattern(text);
    assert.ok(exact.test(text));
    assert.equal(exact.test(other), other === text);
  }));
});

test('a placeholder takes any value of at least one character between its neighbours', () => {
  fc.assert(fc.property(fc.stringMatching(/^[a-z]{1,6}$/), fc.string({ minLength: 1 }), fc.stringMatching(/^[a-z]{1,6}$/), (head, value, tail) => {
    const pattern = testidPattern(`${head}-{id}-${tail}`);
    assert.ok(pattern.test(`${head}-${value}-${tail}`));
    assert.ok(!pattern.test(`${head}--${tail}`) && !pattern.test(`${head}-${value}-${tail}x`) && !pattern.test(`x${head}-${value}-${tail}`));
  }));
});

test('a testid pattern takes any value for a placeholder and nothing else', () => {
  const pattern = testidPattern('task-{id}-open');
  assert.ok(pattern.test('task-12-open') && pattern.test('task-a-b-open'));
  assert.ok(!pattern.test('task--open') && !pattern.test('xtask-1-open') && !pattern.test('task-1-open-now'));
  assert.ok(testidPattern('board.add').test('board.add') && !testidPattern('board.add').test('boardxadd'));
});

test('placeholders may lead, follow each other and end a testid', () => {
  const alone = testidPattern('{id}');
  assert.ok(alone.test('x') && !alone.test(''));
  const leading = testidPattern('{id}-close');
  assert.ok(leading.test('a-close') && leading.test('a-b-close') && !leading.test('-close') && !leading.test('a-closed'));
  const pair = testidPattern('{a}-{b}');
  assert.ok(pair.test('x-y') && pair.test('x-y-z') && !pair.test('-y') && !pair.test('x-') && !pair.test('xy'));
  const trailing = testidPattern('token-revoke-{id}');
  assert.ok(trailing.test('token-revoke-3') && !trailing.test('token-revoke-') && !trailing.test('token-revok-3'));
});

test('the pieces between placeholders keep their order and leave room for each value', () => {
  const pattern = testidPattern('a-{x}-b-{y}-c');
  assert.ok(pattern.test('a-1-b-2-c') && pattern.test('a-b-b-b-c') && pattern.test('a-1-b-2-b-3-c'));
  assert.ok(!pattern.test('a-1-c') && !pattern.test('a-1-b-c') && !pattern.test('a-b-c') && !pattern.test('a--b-2-c'));
  assert.ok(!testidPattern('a-{x}-a').test('a-a') && testidPattern('a-{x}-a').test('a-x-a'));
});

test('a target is skipped when the page marks it unsafe or the app map forbids it', () => {
  const crawl = crawlOf([], [testidPattern('token-revoke-{id}')]);
  assert.ok(skipped(crawl, { unsafe: true, testid: 'board-add' }));
  assert.ok(skipped(crawl, { unsafe: false, testid: 'token-revoke-3' }));
  assert.ok(!skipped(crawl, { unsafe: false, testid: 'token-issue' }));
});

test('a known screen is scheduled once and an unknown route never', () => {
  const crawl = crawlOf();
  schedule(crawl, '/ui/epics/7/tasks');
  schedule(crawl, '/ui/epics/8/tasks');
  schedule(crawl, '/ui/unknown');
  assert.deepEqual(crawl.queue, ['/ui/epics/7/tasks']);
  assert.deepEqual([...crawl.visited], [['/ui/epics/N/tasks', '/ui/epics/7/tasks']]);
});

test('a route remembers the screen it was found on unless that would loop back', () => {
  const crawl = crawlOf();
  discover(crawl, '/ui/home', ['/ui/epics/7/tasks']);
  discover(crawl, '/ui/epics/7/tasks', ['/ui/home']);
  discover(crawl, '/ui/elsewhere', ['/ui/epics/7/tasks']);
  assert.equal(crawl.found.get('/ui/epics/7/tasks'), '/ui/home');
  assert.equal(crawl.found.get('/ui/home'), undefined);
  discover(crawl, '/ui/elsewhere', ['/ui/epics/7/tasks'], true);
  assert.equal(crawl.found.get('/ui/epics/7/tasks'), '/ui/elsewhere');
  assert.deepEqual(crawl.queue, ['/ui/epics/7/tasks', '/ui/home']);
  assert.ok(leadsTo(crawl, '/ui/epics/7/tasks', '/ui/elsewhere'));
  crawl.found.set('/ui/elsewhere', '/ui/epics/7/tasks');
  assert.equal(leadsTo(crawl, '/ui/epics/7/tasks', '/ui/nowhere'), false);
});

test('a screen behind an id is guessed from an id seen in another route', () => {
  assert.equal(fillFirst('/ui/epics/{id}/tasks/{task}', '9'), '/ui/epics/9/tasks/{task}');
  const crawl = crawlOf();
  assert.equal(guess(crawl, board), '');
  crawl.found.set('/ui/epics/settings', '/ui/home');
  crawl.found.set('/ui/epics/12?tab=x', '/ui/home');
  assert.equal(guess(crawl, board), '/ui/epics/12/tasks');
  guessMissing(crawl);
  assert.deepEqual(crawl.queue, ['/ui/epics/12/tasks']);
  const visited = crawlOf([screen('task', '/ui/epics/{id}/tasks/{task}')]);
  visited.found.set('/ui/epics/3', '/ui/home');
  guessMissing(visited);
  assert.deepEqual(visited.queue, []);
});

test('a screen with testids the map promises but the crawl never saw is opened again elsewhere, twice at most', () => {
  const crawl = crawlOf();
  schedule(crawl, '/ui/epics/7/tasks');
  crawl.queue.length = 0;
  seen(crawl, '/ui/epics/7/tasks', ['board-add']);
  seen(crawl, '/ui/epics/7/tasks?x=1', ['board-add', 'other']);
  assert.deepEqual([...crawl.testids.get('/ui/epics/N/tasks')], ['board-add', 'other']);
  assert.deepEqual(missing(crawl, '/ui/epics/N/tasks', board), [{ testid: 'task-{id}' }]);
  assert.deepEqual(missing(crawl, '/ui/home', home), []);
  for (const route of ['/ui/epics/7/tasks', '/ui/epics/8/tasks', '/ui/epics/9/tasks', '/ui/epics/10/tasks']) crawl.found.set(route, '/ui/home');
  for (let round = 0; round < 3; round += 1) widen(crawl);
  assert.deepEqual(crawl.queue, ['/ui/epics/8/tasks', '/ui/epics/9/tasks']);
  seen(crawl, '/ui/epics/8/tasks', ['task-5']);
  const done = crawlOf();
  schedule(done, '/ui/epics/7/tasks');
  done.queue.length = 0;
  seen(done, '/ui/epics/7/tasks', ['task-5', 'board-add']);
  done.found.set('/ui/epics/8/tasks', '/ui/home');
  widen(done);
  assert.deepEqual(done.queue, []);
});

test('a pressed target is keyed by where it lives and what it is, ids aside', () => {
  const target = { place: 'screen', testid: 'task-12-open', label: '' };
  assert.equal(keyOf('/ui/epics/7/tasks', [], target), '/ui/epics/N/tasks | task-N-open');
  assert.equal(keyOf('/ui/epics/7/tasks', [{ sheet: '/ui/tasks/3/move' }, { testid: 'move-9' }], target),
    '/ui/epics/N/tasks > /ui/tasks/N/move > move-9 | task-N-open');
  assert.equal(keyOf('/ui/epics/7/tasks', [], { place: 'shell', testid: '', label: 'Step 2' }), 'shell | Step N');
});
