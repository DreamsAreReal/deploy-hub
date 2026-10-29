import { test } from 'node:test';
import assert from 'node:assert/strict';
import { existsSync, mkdtempSync, readFileSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { run } from '../walk.mjs';
import { configFor, fakeApp, fakeLaunch, pumped } from './fake-browser.mjs';

const back = '<button data-testid="back" data-nav="/ui/home" data-box="0 0 48 48">Back</button>';
const found = (list) => list.map((entry) => `${entry.kind}: ${entry.detail}`);
const screenOf = (id, extra = '') => `<div data-screen="${id}"><h1>${id}</h1>${back}${extra}</div>`;
const screensOf = (...entries) => ({ screens: entries.map(([id, route, elements = []]) => ({ id, kind: 'screen', route, elements })) });

function crawlOf(t, app, env, map) {
  const out = mkdtempSync(join(tmpdir(), 'crawl-walk-'));
  const browser = fakeApp(app);
  t.after(() => browser.close());
  const { launch, launched } = fakeLaunch();
  return { out, browser, launched, config: configFor(out, map, env), launch };
}

const quickMap = {
  screens: [
    { id: 'home', kind: 'screen', route: '/ui/home', elements: [{ testid: 'tasks-open' }] },
    { id: 'tasks', kind: 'screen', route: '/ui/tasks', elements: [{ testid: 'task-{id}' }] },
    { id: 'task', kind: 'screen', route: '/ui/tasks/{id}', elements: [] },
    { id: 'settings', kind: 'screen', route: '/ui/settings', elements: [] },
    { id: 'move', kind: 'sheet', route: '/ui/tasks/{id}/move', elements: [] },
    { id: 'note', kind: 'screen', route: '/ui/notes/{id}', elements: [] },
  ],
};
const quickPages = {
  '/ui/home': screenOf('home', `<p>undefined</p><button data-testid="tasks-open" data-nav="/ui/tasks" data-box="0 60 100 48">Tasks</button>
    <button data-testid="note-edit" data-nav="/ui/notes/5/edit" data-box="0 120 100 48">Edit note</button>`),
  '/ui/tasks': screenOf('tasks', '<button data-testid="task-7" data-nav="/ui/tasks/7" data-box="0 60 100 48">Task 7</button>'),
  '/ui/tasks/7': screenOf('task', '<p>Body</p>'),
  '/ui/settings': screenOf('settings', '<input type="file" data-box="0 60 100 48">'),
  '/ui/notes/5': screenOf('note', '<p>Note 5</p>'),
};

test('a quick crawl opens every screen once, guesses the screens behind ids and presses nothing but the assistant', async (t) => {
  const { out, browser, launched, config, launch } = crawlOf(t, { pages: quickPages, shell: () => ({ chat: '<p>NaN items</p>' }) }, { CRAWL_MODE: 'quick' }, quickMap);
  const { crawl, summary } = await pumped(t, () => run(config, launch, browser.Socket));
  assert.deepEqual(found(crawl.problems), [
    'tech-text: /ui/home: 1 — undefined',
    'tech-text: the assistant chat on /ui/home @default: 1 — NaN items',
    'tech-text: /ui/home @360: 1 — undefined',
    'tech-text: the assistant chat on /ui/home @360: 1 — NaN items',
  ]);
  assert.deepEqual(crawl.problems.map((entry) => entry.steps), [['open /ui/home'], ['open /ui/home', 'open the assistant chat'], ['open /ui/home', 'at 360'],
    ['open /ui/home', 'open the assistant chat']]);
  assert.deepEqual(found(crawl.notes), []);
  assert.deepEqual([...crawl.visited.values()], ['/ui/home', '/ui/tasks', '/ui/settings', '/ui/tasks/7', '/ui/notes/5']);
  assert.equal(crawl.clicks, 0);
  assert.deepEqual(browser.seen.clicks, ['assistant-open', 'assistant-open']);
  assert.deepEqual(browser.seen.uploads, []);
  for (const id of ['home', 'tasks', 'settings', 'task', 'note']) assert.ok(existsSync(join(out, 'screens', `${id}.png`)), id);
  assert.equal(readFileSync(join(out, 'crawl.md'), 'utf8'), summary);
  assert.match(summary, /^# Crawl of every screen\n\n5 of 5 screens, 0 clicks, 4 problems, 0 notes\./);
  assert.equal(launched[0].chrome.killed, 1);
  assert.deepEqual(browser.seen.metrics, [420, 360]);
  assert.deepEqual(browser.seen.media, ['light']);
  assert.ok(browser.seen.navigations.includes('https://app/?open=%2Fui%2Ftasks%2F7'));
});

const richMap = screensOf(
  ['home', '/ui/home', [{ testid: 'tasks-open' }]],
  ['tasks', '/ui/tasks', [{ testid: 'task-{id}' }, { testid: 'tasks-add' }]],
  ['task', '/ui/tasks/{id}', [{ testid: 'task-delete', does: 'Destructive: deletes the task.' }, { testid: 'task-extra' }]],
  ['settings', '/ui/settings'],
  ['broken', '/ui/broken'],
  ['blank', '/ui/blank'],
  ['raw', '/ui/raw'],
  ['orphan', '/ui/orphan/{id}'],
  ['design', '/ui/design'],
  ['doc', '/ui/docs/{id}', [{ testid: 'doc-edit' }]],
  ['archive', '/ui/archive/{id}'],
  ['hidden', '/ui/hidden/{id}'],
  ['uploaded', '/ui/uploaded/{id}'],
);
const title = (text, gap) => `<h1 data-box="0 60 300 30">${text}</h1><div data-box="0 ${90 + gap} 300 20">below</div>`;
const richPages = {
  '/ui/home': `<div data-screen="home">${title('Home', 30)}${back}<button data-testid="tasks-open" data-nav="/ui/tasks" data-box="0 200 100 48">Tasks</button>
    <button data-testid="refresh" data-box="0 260 100 48">Refresh</button><button data-testid="notify" data-box="0 320 100 48">Notify</button>
    <button data-testid="store" data-do='{"method":"POST","url":"/api/save"}' data-box="0 380 100 48">Store</button>
    <button data-testid="doc-1" data-nav="/ui/docs/1" data-box="0 440 100 48">Doc 1</button><button data-testid="doc-2" data-nav="/ui/docs/2" data-box="0 500 100 48">Doc 2</button>
    <button data-testid="reveal" data-box="0 560 100 48">Reveal</button></div>`,
  '/ui/tasks': `<div data-screen="tasks">${title('Tasks', 10)}${back}<button data-testid="task-7" data-nav="/ui/tasks/7" data-box="0 200 100 48">Task 7</button>
    <button data-testid="task-8" data-nav="/ui/tasks/8" data-box="0 260 100 48">Task 8</button><button data-testid="tasks-add" class="button" data-sheet="/ui/tasks/new" data-box="0 320 100 48">Add</button>
    <button class="button" data-testid="tasks-sort" data-do='{"method":"POST","url":"/api/sort"}' data-box="0 380 100 48">Sort</button></div>`,
  '/ui/tasks/7': `<div data-screen="task">${title('Task 7', 30)}${back}<button data-testid="task-move" data-sheet="/ui/tasks/7/move" data-box="0 200 100 48">Move</button>
    <button data-testid="task-delete" data-do='{"method":"DELETE","url":"/api/tasks/7","confirm":"Sure?"}' data-box="0 260 100 48">Delete</button>
    <button data-testid="task-note" data-do='{"method":"POST","url":"/api/tasks/7/note"}' data-box="0 320 100 48">Note</button></div>`,
  '/ui/tasks/8': `<div data-screen="task">${title('Task 8', 30)}${back}<button data-testid="task-extra" data-box="0 200 100 48" data-do='{"method":"POST","url":"/api/tasks/8/extra"}'>Extra</button></div>`,
  '/ui/settings': `<div data-screen="settings">${title('Settings', 10)}${back}<input type="file" data-box="0 200 100 48">
    <button data-testid="settings-save" class="button" data-do='{"method":"POST","url":"/api/save"}' data-box="0 260 100 48">Save</button></div>`,
  '/ui/broken': `<div data-screen="home">${title('Broken', 10)}${back}</div>`,
  '/ui/blank': '<div data-screen="blank">   </div>',
  '/ui/raw': `<div data-screen="raw">${title('Raw', 10)}${back}</div>`,
  '/ui/design': `<div data-screen="design">${title('Design', 10)}${back}<button class="button" data-box="0 200 100 48">Main</button><button class="button" data-box="0 260 100 48">Second</button></div>`,
  '/ui/docs/1': `<div data-screen="doc">${title('Doc 1', 10)}${back}<button data-testid="doc-edit" data-sheet="/ui/docs/1/edit" data-box="0 200 100 48">Edit</button></div>`,
  '/ui/docs/2': `<div data-screen="doc">${title('Doc 2', 10)}${back}</div>`,
  '/ui/archive/1': `<div data-screen="archive">${title('Archive', 10)}${back}</div>`,
  '/ui/hidden/1': `<div data-screen="hidden">${title('Hidden', 10)}${back}</div>`,
  '/ui/uploaded/1': `<div data-screen="uploaded">${title('Uploaded', 10)}${back}</div>`,
};
const richSheets = {
  '/ui/tasks/new': `<p>New task</p><input aria-label="title" data-box="0 400 200 48"><input type="file" data-box="0 460 100 48">
    <button data-testid="new-save" data-do='{"method":"POST","url":"/api/tasks"}' data-box="0 520 100 48">Create</button>
    <button data-testid="archive-open" data-nav="/ui/archive/1" data-box="0 580 100 48">Archive</button><button data-testid="sheet-close" data-close data-box="300 400 48 48">Close</button>`,
  '/ui/tasks/7/move': '<p>Move the task</p><button data-testid="move-epic-2" data-do=\'{"method":"PATCH","url":"/api/tasks/7"}\' data-box="0 400 100 48">Epic 2</button>',
};

test('a full crawl presses every safe button, looks into every sheet and reports what it found', async (t) => {
  let uploaded = false;
  const { out, browser, config, launch } = crawlOf(t, {
    pages: richPages, sheets: richSheets, noAlpine: ['/ui/raw'], statuses: { '/api/tasks/7/note': 500 }, widths: { '/ui/settings': 500 },
    onUpload: ({ document }) => {
      if (!uploaded && document.querySelector('[data-screen="settings"]')) {
        uploaded = true;
        document.getElementById('screen').insertAdjacentHTML('beforeend', '<button data-nav="/ui/uploaded/1" data-box="0 320 100 48">Uploaded</button>');
      }
    },
    handlers: {
      notify: ({ document }) => {
        const toast = document.getElementById('toast');
        toast.dataset.kind = 'alert';
        toast.textContent = 'Saved, but late';
        toast.hidden = false;
      },
      reveal: ({ document }) => document.getElementById('screen').insertAdjacentHTML('beforeend', '<button data-nav="/ui/hidden/1" data-box="0 620 100 48">Hidden</button>'),
      'root-home': ({ document, screenTo }) => {
        document.querySelector('[data-testid="roots"]').remove();
        screenTo('/ui/home');
      },
    },
  }, {}, richMap);
  const { crawl } = await pumped(t, () => run(config, launch, browser.Socket));
  assert.deepEqual(found(crawl.problems), [
    'lost-menu: root-home leads to a screen without the roots menu',
    'dead-button: refresh changes nothing and sends nothing',
    'many-primary: /ui/tasks: button[tasks-add] «Add»; button[tasks-sort] «Sort»',
    'wrong-screen: /ui/broken showed «home» instead of «broken»',
    'empty-screen: /ui/blank',
    'dead-end: /ui/blank has no way back',
    'no-alpine: /ui/raw: Alpine did not start, buttons do nothing',
    'http-500: POST https://app/api/tasks/7/note → 500',
    'too-wide: /ui/settings @360: 1 — 500px on a 360px screen',
    'too-wide: /ui/settings @390: 1 — 500px on a 390px screen',
    'too-wide: /ui/settings @430: 1 — 500px on a 430px screen',
    'too-wide: /ui/settings @390-dark: 1 — 500px on a 390px screen',
    'mixed-words: «store» on /ui/home, «save» on /ui/settings',
    'uneven-title-gap: 10 px under the title on /ui/tasks, 30 px on /ui/tasks/8',
  ]);
  assert.deepEqual([crawl.problems[1].steps, crawl.problems[8].steps, crawl.problems[12].steps], [['open /ui/home', 'click refresh'], ['open /ui/settings', 'at 360'], ['after the crawl']]);
  assert.match(crawl.problems[1].shot, /^shots\/\d{3}\.png$/);
  assert.deepEqual(found(crawl.notes), ['alert: Saved, but late', 'no-close: /ui/tasks/7 (sheet of task-move) has no close button', 'unreached: /ui/orphan/{id} (orphan)']);
  assert.deepEqual(crawl.notes[2].steps, ['after the crawl']);
  assert.equal(crawl.clicks, 40);
  assert.deepEqual([...crawl.visited.values()], ['/ui/home', '/ui/tasks', '/ui/settings', '/ui/broken', '/ui/blank', '/ui/raw', '/ui/design', '/ui/docs/1', '/ui/tasks/7',
    '/ui/hidden/1', '/ui/archive/1', '/ui/uploaded/1']);
  assert.deepEqual([...crawl.extra], ['/ui/tasks/8']);
  assert.deepEqual(browser.seen.uploads, [1, 1]);
  assert.deepEqual(browser.seen.viewports.map((size) => [size.width, size.height, size.deviceScaleFactor, size.mobile]), [[420, 860, 1, true], [360, 740, 1, true],
    [390, 844, 1, true], [430, 932, 1, true], [768, 1024, 1, false], [1280, 800, 1, false], [390, 844, 1, true]]);
  assert.deepEqual(browser.seen.media, ['light', 'light', 'light', 'light', 'light', 'dark']);
  assert.deepEqual(['home-360', 'broken-360', 'settings-1280'].map((file) => existsSync(join(out, 'screens', `${file}.png`))), [true, false, true]);
  assert.deepEqual(browser.seen.clicks, ['root-home', 'back', 'tasks-open', 'refresh', 'notify', 'store', 'doc-1', 'reveal', 'assistant-open', 'assistant-open', 'chat-close',
    'back', 'task-7', 'tasks-add', 'tasks-add', 'new-save', 'tasks-add', 'archive-open', 'tasks-add', 'sheet-close', 'tasks-sort', 'back', 'settings-save', 'back', 'back',
    'Main', 'Second', 'back', 'doc-edit', 'doc-edit', 'sheet-close', 'back', 'task-move', 'task-move', 'move-epic-2', 'task-note', 'back', 'back', 'back', 'task-extra',
    'assistant-open', 'assistant-open', 'assistant-open', 'assistant-open', 'assistant-open', 'assistant-open']);
});

const troubleIds = ['broken', 'slow', 'stall', 'busy', 'wobbly', 'repeat', 'moving', 'forever', 'shift25', 'shift10', 'heavy3000'];
const troubleMap = { screens: [...troubleIds.map((id) => ({ id, kind: 'screen', route: `/ui/${id}`, elements: [] })), { id: 'upper', kind: 'screen', route: '/ui/Upper', elements: [] }] };

test('a quick crawl tells slow, stalled, shifting, busy and chatty screens apart', async (t) => {
  const pages = Object.fromEntries(troubleIds.map((id) => [`/ui/${id}`, screenOf(id)]));
  const { browser, config, launch } = crawlOf(t, {
    pages: {
      ...pages, '/ui/broken': screenOf('home'), '/ui/busy': screenOf('busy', '<i></i>'.repeat(3001)), '/ui/heavy3000': screenOf('heavy3000', '<i></i>'.repeat(2985)),
      '/ui/moving': screenOf('moving', '<p>undefined</p>'), '/ui/Upper': screenOf('upper'),
    },
    after: { '/ui/moving': screenOf('moving', '<p>ready</p>') },
    delays: { '/ui/slow': 3500 }, stall: ['/ui/stall'], moving: { '/ui/moving': 1000 }, hanging: { '/ui/forever': ['/api/forever'] },
    perf: { '/ui/busy': { shift: 0.3, longTasks: 2 }, '/ui/wobbly': { shift: 0.15 }, '/ui/shift25': { shift: 0.25 }, '/ui/shift10': { shift: 0.1 } },
    fetches: { '/ui/repeat': ['/api/a', '/api/a', '/assets/app.js', '/assets/app.js'] },
  }, { CRAWL_MODE: 'quick' }, troubleMap);
  const { crawl } = await pumped(t, () => run(config, launch, browser.Socket));
  assert.deepEqual(found(crawl.problems), [
    'wrong-screen: /ui/broken showed «home» instead of «broken»',
    'slow: /ui/slow took 3805 ms to show',
    'hang: load https://app/?open=%2Fui%2Fstall: no answer in 10000 ms',
    'slow: /ui/stall took 10300 ms to show',
    'layout-shift: /ui/busy moves by 0.3 while it loads',
    'hang: no answer in 10000 ms: https://app/api/forever (Fetch by script)',
    'slow: /ui/forever took 10005 ms to show',
  ]);
  assert.deepEqual(found(crawl.notes), ['long-task: /ui/busy: 2 scripts ran longer than 200 ms', 'heavy-screen: /ui/busy: 3016 elements',
    'layout-shift: /ui/wobbly moves by 0.15 while it loads', 'repeated-request: /ui/repeat: https://app/api/a', 'layout-shift: /ui/shift25 moves by 0.25 while it loads']);
  assert.equal(crawl.visited.get('/ui/Upper'), '/ui/Upper');
  assert.deepEqual(browser.seen.clicks, []);
});

test('a full crawl survives buttons that vanish or turn dangerous, requests that never end and a browser that closes', async (t) => {
  const renders = new Map();
  let sheetSeen = false;
  const counted = (route, html) => () => {
    renders.set(route, (renders.get(route) ?? 0) + 1);
    return html(renders.get(route));
  };
  const flakyMap = screensOf(['home', '/ui/home'], ['tasks', '/ui/tasks'], ['task', '/ui/tasks/{id}'], ['last', '/ui/last']);
  const out = mkdtempSync(join(tmpdir(), 'crawl-flaky-'));
  const serverLog = join(out, 'server.log');
  writeFileSync(serverLog, '{"level":"ERROR","msg":"database is locked"}\n');
  const { browser, config, launch } = crawlOf(t, {
    pages: {
      '/ui/home': counted('/ui/home', (count) => screenOf('home', `${sheetSeen ? '' : '<button data-testid="sheet-opener" data-box="0 40 100 48">Options</button>'}
        ${count === 1 ? '<button data-testid="ghost" data-box="0 100 100 48" data-nav="/ui/tasks">Ghost</button>' : ''}
        <button data-testid="arm" class="${count === 1 ? 'text-button' : 'text-button text-button-danger'}" data-box="0 160 100 48" data-do='{"method":"POST","url":"/api/arm"}'>Arm</button>
        <button data-testid="slowpoke" data-box="0 220 100 48">Slow</button><input type="file" data-box="0 280 100 48">`)),
      '/ui/tasks': screenOf('tasks', '<button data-testid="task-9" data-nav="/ui/tasks/9?from=list" data-box="0 100 100 48">Task 9</button>'),
      '/ui/tasks/9?from=list': screenOf('task', '<button data-testid="crash" data-box="0 100 100 48">Crash</button>'),
      '/ui/last': screenOf('last'),
    },
    shell: () => ({ assistant: false }),
    badUpload: 2,
    handlers: {
      'sheet-opener': ({ document, request }) => {
        sheetSeen = true;
        const sheet = document.getElementById('sheet');
        sheet.innerHTML = '<p>Options</p><button data-testid="sheet-pick" data-do=\'{"method":"POST","url":"/api/pick"}\' data-box="0 400 100 48">Pick</button><button data-close data-box="300 400 48 48">Close</button>';
        sheet.hidden = false;
        request('/ui/options');
      },
      slowpoke: ({ request }) => request('/api/never', 'POST', 200, false),
      crash: ({ socket }) => {
        socket.readyState = WebSocket.CLOSED;
      },
    },
  }, { E2E_SERVER_LOG: serverLog }, flakyMap);
  const { crawl } = await pumped(t, () => run(config, launch, browser.Socket));
  await new Promise((done) => (crawl.log.writableFinished ? done() : crawl.log.on('finish', done)));
  assert.deepEqual(found(crawl.problems), [
    'hang: no answer in 10000 ms: https://app/api/never (Fetch by script)',
    'dead-button: crash changes nothing and sends nothing',
    'browser-crashed: Chrome closed its DevTools connection during the crawl',
    'server-error: {"level":"ERROR","msg":"database is locked"}',
  ]);
  assert.deepEqual(crawl.notes.map((entry) => [entry.kind, entry.detail, entry.steps]), [['upload', 'file input is gone', ['open /ui/home', 'upload crawl-sample.md']]]);
  const logged = readFileSync(join(config.out, 'crawl.log'), 'utf8').split('\n').filter((line) => line.startsWith('  ') || line.startsWith('screen'));
  assert.deepEqual(logged, ['screen home /ui/home', '  gone: sheet-opener', '  gone: sheet-opener', '  gone: ghost', '  unsafe now: arm', 'screen tasks /ui/tasks',
    'screen last /ui/last', 'screen task /ui/tasks/9?from=list']);
  assert.deepEqual([...crawl.visited.values()], ['/ui/home', '/ui/tasks', '/ui/last', '/ui/tasks/9?from=list']);
  assert.equal(crawl.clicks, 9);
  assert.deepEqual(browser.seen.uploads, [1, 1]);
  assert.deepEqual(browser.seen.clicks, ['root-home', 'back', 'sheet-opener', 'slowpoke', 'back', 'task-9', 'back', 'task-9', 'task-9', 'back', 'task-9', 'crash']);
  assert.equal(existsSync(join(config.out, 'screens', 'home-360.png')), false);
});

const titled = (id, gap, extra = '') => `<div data-screen="${id}"><h1 data-box="0 60 300 30">${id}</h1><div data-box="0 ${90 + gap} 300 20">below</div>${back}${extra}</div>`;

async function quickCrawl(t, pages, map) {
  const { browser, config, launch } = crawlOf(t, { pages }, { CRAWL_MODE: 'quick' }, map);
  return (await pumped(t, () => run(config, launch, browser.Socket))).crawl;
}

test('the title gap of a later screen can be the smaller one, and text just at a ratio of 3 is only low contrast', async (t) => {
  const crawl = await quickCrawl(t, {
    '/ui/a': titled('a', 30, '<p style="color: rgb(149, 149, 149)">c149</p><p style="color: rgb(150, 150, 150)">c150</p>'),
    '/ui/b': titled('b', 10),
  }, screensOf(['a', '/ui/a'], ['b', '/ui/b']));
  assert.deepEqual(found(crawl.problems), ['unreadable: /ui/a: 1 — p «c150» 2.96:1', 'unreadable: /ui/a @360: 1 — p «c150» 2.96:1',
    'uneven-title-gap: 10 px under the title on /ui/b, 30 px on /ui/a']);
  assert.deepEqual(found(crawl.notes), ['low-contrast: /ui/a: 1 — p «c149» 3:1', 'low-contrast: /ui/a @360: 1 — p «c149» 3:1']);
});

test('title gaps 8 px apart are still even', async (t) => {
  const crawl = await quickCrawl(t, { '/ui/c': titled('c', 18), '/ui/d': titled('d', 10) }, screensOf(['c', '/ui/c'], ['d', '/ui/d']));
  assert.deepEqual(crawl.problems, []);
});

test('screens without a title give no title gaps to compare', async (t) => {
  const crawl = await quickCrawl(t, { '/ui/e': `<div data-screen="e">${back}<p>Plain</p></div>` }, screensOf(['e', '/ui/e']));
  assert.deepEqual([crawl.problems, [...crawl.gaps]], [[], []]);
});
