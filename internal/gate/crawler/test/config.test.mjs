import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { configure } from '../config.mjs';

const appMap = {
  screens: [
    { id: 'board', kind: 'screen', route: '/ui/epics/{id}/tasks', elements: [{ testid: 'task-{id}-delete', does: 'Destructive: deletes the task.' }, { testid: 'board-add', does: 'Opens a form.' }] },
    { id: 'move', kind: 'sheet', route: '/ui/tasks/{id}/move', elements: [{ testid: 'move-cancel', does: 'Closes; never click in a crawl.' }] },
  ],
};

test('a crawl without a base or an app map is not ready and knows no screens', () => {
  const config = configure({ E2E_BASE: 'https://app' });
  assert.equal(config.ready, false);
  assert.equal(config.screens.size, 0);
  assert.deepEqual(config.forbidden, []);
  assert.equal(config.quick, false);
  assert.equal(config.typed, 'Crawl <b>&amp;</b> «quotes» 🐸');
  assert.ok(Number.isNaN(config.frozenAt));
  assert.deepEqual([config.base, config.out, config.chromium, config.netLog, config.serverLog, config.baseline], ['https://app', '', '', '', '', '']);
  assert.equal(configure({}).base, '');
});

test('the environment sets the crawl and the app map gives its screens and forbidden buttons', () => {
  const dir = mkdtempSync(join(tmpdir(), 'crawl-config-'));
  const file = join(dir, 'appmap.json');
  writeFileSync(file, JSON.stringify(appMap));
  const config = configure({
    E2E_BASE: 'https://app:8080', E2E_APPMAP: file, E2E_OUT: '/out', CRAWL_MODE: 'quick', E2E_CHROMIUM: '/chrome', E2E_NETLOG: '/net.json', E2E_SERVER_LOG: '/server.log',
    E2E_TYPED: 'typed', E2E_FROZEN_AT: '2030-01-15T09:30:00Z', E2E_BASELINE: '/baseline', E2E_VISUAL_ACCEPT: ' board, ,memory ',
  });
  assert.equal(config.ready, true);
  assert.deepEqual([config.base, config.out, config.chromium, config.netLog, config.serverLog, config.typed, config.baseline],
    ['https://app:8080', '/out', '/chrome', '/net.json', '/server.log', 'typed', '/baseline']);
  assert.equal(config.quick, true);
  assert.equal(config.frozenAt, Date.UTC(2030, 0, 15, 9, 30));
  assert.deepEqual([...config.accepted], ['board', 'memory']);
  assert.deepEqual([...config.screens.keys()], ['/ui/epics/N/tasks']);
  assert.equal(config.forbidden.length, 2);
  assert.ok(config.forbidden[0].test('task-7-delete') && config.forbidden[1].test('move-cancel'));
  assert.ok(!config.forbidden.some((pattern) => pattern.test('board-add')));
});

test('the app map is read through the given reader and an empty typed text falls back', () => {
  const read = (file) => {
    assert.equal(file, 'map.json');
    return JSON.stringify({ screens: [{ id: 'home', kind: 'screen', route: '/ui/home' }] });
  };
  const config = configure({ E2E_APPMAP: 'map.json', E2E_TYPED: '', CRAWL_MODE: 'full' }, read);
  assert.equal(config.ready, false);
  assert.equal(config.quick, false);
  assert.equal(config.typed, 'Crawl <b>&amp;</b> «quotes» 🐸');
  assert.deepEqual([...config.screens.values()].map((screen) => screen.id), ['home']);
});
