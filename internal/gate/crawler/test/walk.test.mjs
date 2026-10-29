import { test } from 'node:test';
import assert from 'node:assert/strict';
import { existsSync, mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { run } from '../walk.mjs';
import { configFor, fakeApp, fakeLaunch, pumped } from './fake-browser.mjs';

const back = '<button data-testid="back" data-nav="/ui/home" data-box="0 0 48 48">Back</button>';
const appMap = {
  screens: [
    { id: 'home', kind: 'screen', route: '/ui/home', elements: [{ testid: 'tasks-open' }] },
    { id: 'tasks', kind: 'screen', route: '/ui/tasks', elements: [{ testid: 'task-{id}' }] },
    { id: 'task', kind: 'screen', route: '/ui/tasks/{id}', elements: [] },
    { id: 'settings', kind: 'screen', route: '/ui/settings', elements: [] },
    { id: 'move', kind: 'sheet', route: '/ui/tasks/{id}/move', elements: [] },
  ],
};
const pages = {
  '/ui/home': `<div data-screen="home"><h1>Home</h1>${back}<button data-testid="tasks-open" data-nav="/ui/tasks" data-box="0 60 100 48">Tasks</button></div>`,
  '/ui/tasks': `<div data-screen="tasks"><h1>Tasks</h1>${back}<button data-testid="task-7" data-nav="/ui/tasks/7" data-box="0 60 100 48">Task 7</button></div>`,
  '/ui/tasks/7': `<div data-screen="task"><h1>Task 7</h1>${back}<p>Body</p></div>`,
  '/ui/settings': `<div data-screen="settings"><h1>Settings</h1>${back}</div>`,
};

function crawlOf(t, app, env = {}, map = appMap) {
  const out = mkdtempSync(join(tmpdir(), 'crawl-walk-'));
  const browser = fakeApp({ pages, ...app });
  t.after(() => browser.close());
  const { launch, launched } = fakeLaunch();
  return { out, browser, launched, config: configFor(out, map, env), launch };
}

test('a quick crawl opens every screen once, guesses the screens behind ids and presses nothing', async (t) => {
  const { out, browser, launched, config, launch } = crawlOf(t, {}, { CRAWL_MODE: 'quick' });
  const { crawl, summary } = await pumped(t, () => run(config, launch, browser.Socket));
  assert.deepEqual(crawl.problems, []);
  assert.deepEqual([...crawl.visited.values()], ['/ui/home', '/ui/tasks', '/ui/settings', '/ui/tasks/7']);
  assert.equal(crawl.clicks, 0);
  for (const id of ['home', 'tasks', 'settings', 'task']) assert.ok(existsSync(join(out, 'screens', `${id}.png`)), id);
  assert.equal(readFileSync(join(out, 'crawl.md'), 'utf8'), summary);
  assert.match(summary, /^# Crawl of every screen\n\n4 of 4 screens, 0 clicks, 0 problems, \d+ notes\./);
  assert.equal(launched[0].chrome.killed, 1);
  assert.deepEqual(browser.seen.metrics, [420, 360]);
  assert.deepEqual(browser.seen.media, ['light']);
  assert.ok(browser.seen.navigations.includes('https://app/?open=%2Fui%2Ftasks%2F7'));
  assert.deepEqual(crawl.log.lines?.at(-1), undefined);
});

const richMap = {
  screens: [
    { id: 'home', kind: 'screen', route: '/ui/home', elements: [{ testid: 'tasks-open' }] },
    { id: 'tasks', kind: 'screen', route: '/ui/tasks', elements: [{ testid: 'task-{id}' }, { testid: 'tasks-add' }] },
    { id: 'task', kind: 'screen', route: '/ui/tasks/{id}', elements: [{ testid: 'task-delete', does: 'Destructive: deletes the task.' }, { testid: 'task-extra' }] },
    { id: 'settings', kind: 'screen', route: '/ui/settings', elements: [] },
    { id: 'broken', kind: 'screen', route: '/ui/broken', elements: [] },
    { id: 'blank', kind: 'screen', route: '/ui/blank', elements: [] },
    { id: 'raw', kind: 'screen', route: '/ui/raw', elements: [] },
    { id: 'orphan', kind: 'screen', route: '/ui/orphan/{id}', elements: [] },
  ],
};
const title = (text, gap) => `<h1 data-box="0 60 300 30">${text}</h1><div data-box="0 ${90 + gap} 300 20">below</div>`;
const richPages = {
  '/ui/home': `<div data-screen="home">${title('Home', 10)}${back}<button data-testid="tasks-open" data-nav="/ui/tasks" data-box="0 200 100 48">Tasks</button>
    <button data-testid="refresh" data-box="0 260 100 48">Refresh</button><button data-testid="notify" data-box="0 320 100 48">Notify</button>
    <button data-testid="store" data-do='{"method":"POST","url":"/api/save"}' data-box="0 380 100 48">Store</button></div>`,
  '/ui/tasks': `<div data-screen="tasks">${title('Tasks', 10)}${back}<button data-testid="task-7" data-nav="/ui/tasks/7" data-box="0 200 100 48">Task 7</button>
    <button data-testid="task-8" data-nav="/ui/tasks/8" data-box="0 260 100 48">Task 8</button><button data-testid="tasks-add" class="button" data-sheet="/ui/tasks/new" data-box="0 320 100 48">Add</button>
    <button class="button" data-testid="tasks-sort" data-do='{"method":"POST","url":"/api/sort"}' data-box="0 380 100 48">Sort</button></div>`,
  '/ui/tasks/7': `<div data-screen="task">${title('Task 7', 30)}${back}<button data-testid="task-move" data-sheet="/ui/tasks/7/move" data-box="0 200 100 48">Move</button>
    <button data-testid="task-delete" data-do='{"method":"DELETE","url":"/api/tasks/7","confirm":"Sure?"}' data-box="0 260 100 48">Delete</button>
    <button data-testid="task-note" data-do='{"method":"POST","url":"/api/tasks/7/note"}' data-box="0 320 100 48">Note</button></div>`,
  '/ui/tasks/8': `<div data-screen="task">${title('Task 8', 30)}${back}<button data-testid="task-extra" data-box="0 200 100 48" data-do='{"method":"POST","url":"/api/tasks/8/extra"}'>Extra</button></div>`,
  '/ui/settings': `<div data-screen="settings">${title('Settings', 10)}${back}<input type="file" data-box="0 200 100 48">
    <button data-testid="settings-save" data-do='{"method":"POST","url":"/api/save"}' data-box="0 260 100 48">Save</button></div>`,
  '/ui/broken': `<div data-screen="home">${title('Broken', 10)}${back}</div>`,
  '/ui/blank': '<div data-screen="blank">   </div>',
  '/ui/raw': `<div data-screen="raw">${title('Raw', 10)}${back}</div>`,
};
const richSheets = {
  '/ui/tasks/new': '<p>New task</p><input aria-label="title" data-box="0 400 200 48"><input type="file" data-box="0 460 100 48"><button data-testid="new-save" data-do=\'{"method":"POST","url":"/api/tasks"}\' data-box="0 520 100 48">Create</button><button data-testid="sheet-close" data-close data-box="300 400 48 48">Close</button>',
  '/ui/tasks/7/move': '<p>Move the task</p><button data-testid="move-epic-2" data-do=\'{"method":"PATCH","url":"/api/tasks/7"}\' data-box="0 400 100 48">Epic 2</button>',
};

test('a full crawl presses every safe button, looks into every sheet and reports what it found', async (t) => {
  const { browser, config, launch } = crawlOf(t, {
    pages: richPages, sheets: richSheets, noAlpine: ['/ui/raw'], statuses: { '/api/tasks/7/note': 500 },
    handlers: {
      notify: ({ document }) => {
        const toast = document.getElementById('toast');
        toast.dataset.kind = 'alert';
        toast.textContent = 'Saved, but late';
        toast.hidden = false;
      },
      'root-home': ({ document, screenTo }) => {
        document.querySelector('[data-testid="roots"]').hidden = true;
        screenTo('/ui/home');
      },
    },
  }, {}, richMap);
  const { crawl } = await pumped(t, () => run(config, launch, browser.Socket));
  const found = (list) => list.map((entry) => `${entry.kind}: ${entry.detail}`);
  assert.deepEqual(found(crawl.problems), [
    'lost-menu: root-home leads to a screen without the roots menu',
    'dead-button: refresh changes nothing and sends nothing',
    'many-primary: /ui/tasks: button[tasks-add] «Add»; button[tasks-sort] «Sort»',
    'wrong-screen: /ui/broken showed «home» instead of «broken»',
    'empty-screen: /ui/blank',
    'dead-end: /ui/blank has no way back',
    'no-alpine: /ui/raw: Alpine did not start, buttons do nothing',
    'http-500: POST https://app/api/tasks/7/note → 500',
    'mixed-words: «store» on /ui/home, «save» on /ui/settings',
    'uneven-title-gap: 10 px under the title on /ui/home, 30 px on /ui/tasks/8',
  ]);
  assert.deepEqual(found(crawl.notes), ['alert: Saved, but late', 'no-close: /ui/tasks/7 (sheet of task-move) has no close button', 'unreached: /ui/orphan/{id} (orphan)']);
  assert.deepEqual(crawl.problems[1].steps, ['open /ui/home', 'click refresh']);
  assert.match(crawl.problems[1].shot, /^shots\/\d{3}\.png$/);
  assert.equal(crawl.clicks, 26);
  assert.deepEqual([...crawl.visited.values()], ['/ui/home', '/ui/tasks', '/ui/settings', '/ui/broken', '/ui/blank', '/ui/raw', '/ui/tasks/7']);
  assert.deepEqual([...crawl.extra], ['/ui/tasks/8']);
  assert.deepEqual(browser.seen.uploads, [1, 1]);
  assert.deepEqual(browser.seen.metrics, [420, 360, 390, 430, 768, 1280, 390]);
  assert.deepEqual(browser.seen.media, ['light', 'light', 'light', 'light', 'light', 'dark']);
  assert.ok(!browser.seen.clicks.includes('task-delete'));
  assert.ok(browser.seen.clicks.includes('new-save') && browser.seen.clicks.includes('move-epic-2'));
});

const screenOf = (id, extra = '') => `<div data-screen="${id}"><h1>${id}</h1>${back}${extra}</div>`;
const troubleMap = {
  screens: ['broken', 'slow', 'stall', 'busy', 'wobbly', 'repeat', 'moving'].map((id) => ({ id, kind: 'screen', route: `/ui/${id}`, elements: [] })),
};

test('a quick crawl tells slow, stalled, shifting, busy and chatty screens apart', async (t) => {
  const { browser, config, launch } = crawlOf(t, {
    pages: {
      '/ui/broken': screenOf('home'), '/ui/slow': screenOf('slow'), '/ui/stall': screenOf('stall'), '/ui/wobbly': screenOf('wobbly'), '/ui/repeat': screenOf('repeat'),
      '/ui/moving': screenOf('moving'), '/ui/busy': screenOf('busy', '<i></i>'.repeat(3001)),
    },
    delays: { '/ui/slow': 3500 }, stall: ['/ui/stall'], moving: { '/ui/moving': 500 },
    perf: { '/ui/busy': { shift: 0.3, longTasks: 2 }, '/ui/wobbly': { shift: 0.15 } },
    fetches: { '/ui/repeat': ['/api/a', '/api/a', '/assets/app.js', '/assets/app.js'] },
  }, { CRAWL_MODE: 'quick' }, troubleMap);
  const { crawl } = await pumped(t, () => run(config, launch, browser.Socket));
  const found = (list) => list.map((entry) => `${entry.kind}: ${entry.detail}`);
  assert.deepEqual(found(crawl.problems), [
    'wrong-screen: /ui/broken showed «home» instead of «broken»',
    'slow: /ui/slow took 3805 ms to show',
    'hang: load https://app/?open=%2Fui%2Fstall: no answer in 10000 ms',
    'slow: /ui/stall took 10300 ms to show',
    'layout-shift: /ui/busy moves by 0.3 while it loads',
  ]);
  assert.deepEqual(found(crawl.notes), ['long-task: /ui/busy: 2 scripts ran longer than 200 ms', 'heavy-screen: /ui/busy: 3016 elements',
    'layout-shift: /ui/wobbly moves by 0.15 while it loads', 'repeated-request: /ui/repeat: https://app/api/a']);
});

test('a full crawl survives buttons that vanish or turn dangerous, requests that never end and a browser that closes', async (t) => {
  const renders = new Map();
  const counted = (route, html) => () => {
    renders.set(route, (renders.get(route) ?? 0) + 1);
    return html(renders.get(route));
  };
  const flakyMap = {
    screens: [
      { id: 'home', kind: 'screen', route: '/ui/home', elements: [] },
      { id: 'tasks', kind: 'screen', route: '/ui/tasks', elements: [] },
      { id: 'task', kind: 'screen', route: '/ui/tasks/{id}', elements: [] },
      { id: 'last', kind: 'screen', route: '/ui/last', elements: [] },
    ],
  };
  const out = mkdtempSync(join(tmpdir(), 'crawl-flaky-'));
  const serverLog = join(out, 'server.log');
  writeFileSync(serverLog, '{"level":"ERROR","msg":"database is locked"}\n');
  const { browser, config, launch } = crawlOf(t, {
    pages: {
      '/ui/home': counted('/ui/home', (count) => screenOf('home', `${count === 1 ? '<button data-testid="ghost" data-box="0 100 100 48" data-nav="/ui/tasks">Ghost</button>' : ''}
        <button data-testid="arm" class="${count === 1 ? 'text-button' : 'text-button text-button-danger'}" data-box="0 160 100 48" data-do='{"method":"POST","url":"/api/arm"}'>Arm</button>
        <button data-testid="slowpoke" data-box="0 220 100 48">Slow</button><input type="file" data-box="0 280 100 48">`)),
      '/ui/tasks': screenOf('tasks', '<button data-testid="task-9" data-nav="/ui/tasks/9?from=list" data-box="0 100 100 48">Task 9</button>'),
      '/ui/tasks/9?from=list': screenOf('task', '<button data-testid="crash" data-box="0 100 100 48">Crash</button>'),
      '/ui/last': screenOf('last'),
    },
    shell: () => ({ assistant: false }),
    badUpload: 2,
    handlers: {
      slowpoke: ({ request }) => request('/api/never', 'POST', 200, false),
      crash: ({ socket }) => {
        socket.readyState = WebSocket.CLOSED;
      },
    },
  }, { E2E_SERVER_LOG: serverLog }, flakyMap);
  const { crawl } = await pumped(t, () => run(config, launch, browser.Socket));
  await new Promise((done) => (crawl.log.writableFinished ? done() : crawl.log.on('finish', done)));
  const found = (list) => list.map((entry) => `${entry.kind}: ${entry.detail}`);
  assert.deepEqual(found(crawl.problems), [
    'hang: no answer in 10000 ms: https://app/api/never (Fetch by script)',
    'dead-button: crash changes nothing and sends nothing',
    'browser-crashed: Chrome closed its DevTools connection during the crawl',
    'server-error: {"level":"ERROR","msg":"database is locked"}',
  ]);
  assert.deepEqual(found(crawl.notes), ['upload: file input is gone']);
  const skippedLines = readFileSync(join(config.out, 'crawl.log'), 'utf8').split('\n').filter((line) => line.startsWith('  '));
  assert.deepEqual(skippedLines, ['  gone: ghost', '  unsafe now: arm']);
  assert.deepEqual([...crawl.visited.values()], ['/ui/home', '/ui/tasks', '/ui/last', '/ui/tasks/9?from=list']);
  assert.equal(crawl.clicks, 8);
  assert.deepEqual(browser.seen.uploads, [1]);
  assert.equal(browser.seen.clicks.filter((testid) => testid === 'task-9').length, 4);
  assert.equal(browser.seen.clicks.at(-1), 'crash');
  assert.deepEqual(browser.seen.metrics, [420, 360, 390, 430, 768, 1280, 390]);
  assert.equal(existsSync(join(config.out, 'screens', 'home-360.png')), false);
});
