import { test } from 'node:test';
import assert from 'node:assert/strict';
import { aim, frozenClock, helpers, holdStill, letGo, pixelDiff } from '../page.mjs';
import { pageEnv } from './page-env.mjs';

function withPage(t, html, options) {
  const env = pageEnv(html, options);
  t.after(() => env.close());
  helpers();
  return env;
}

const shell = `
<nav data-testid="roots"><button data-testid="root-home" aria-current="page">Home</button><a href="/ui/docs" data-nav="/ui/docs">Docs</a></nav>
<div id="screen"><div data-screen="board">
  <button data-testid="board-add" data-sheet="/ui/board/new">  Add
    task  </button>
  <button data-testid="board-delete" data-do='{"method":"DELETE","url":"/api/x"}'>Delete</button>
  <button class="text-button text-button-danger">Revoke 12</button>
  <button class="chip chip-picked">Picked</button>
  <button aria-pressed="true">Pressed</button>
  <button aria-label="Close the panel">x</button>
  <button disabled>Off</button>
  <button hidden>Hidden</button>
  <a href="https://elsewhere.example/help">Help</a>
  <a href="/files/1" download>File</a>
  <a href="/ui/new" target="_blank">New tab</a>
  <a href="/ui/same">Same</a>
  <div role="button" data-testid="role">Role</div>
  <summary>More</summary>
  <form><input required><button type="submit" data-testid="send">Send</button></form>
  <form><input><button type="submit" data-testid="send-ok">Send ok</button></form>
  <input data-testid="free">
  <input type="search" value="kept">
  <input type="url" readonly>
  <textarea></textarea>
  <input type="checkbox">
  <input disabled>
</div></div>
<div id="sheet" hidden><button data-testid="sheet-close" data-close>Close</button><input></div>
<div id="chat" hidden><button data-testid="chat-send">Send</button></div>
<div id="toast" data-kind="status">Saved</div>`;

test('the page lists what a person can press, where it is and whether pressing it is safe', (t) => {
  withPage(t, shell);
  const targets = window.crawl.targets();
  const byLabel = new Map(targets.map((target) => [target.label, target]));
  assert.deepEqual(targets.map((target) => target.label), ['Home', 'Docs', 'Add task', 'Delete', 'Revoke 12', 'Picked', 'Pressed', 'Close the panel', 'Help', 'File',
    'New tab', 'Same', 'Role', 'More', 'Send', 'Send ok']);
  assert.deepEqual(byLabel.get('Add task'), { index: 2, testid: 'board-add', nav: '', sheet: '/ui/board/new', act: '', label: 'Add task', place: 'screen', unsafe: false,
    quiet: false, menu: '' });
  assert.deepEqual(byLabel.get('Docs'), { index: 1, testid: '', nav: '/ui/docs', sheet: '', act: '', label: 'Docs', place: 'shell', unsafe: false, quiet: false, menu: 'roots' });
  assert.deepEqual(['Delete', 'Revoke 12', 'Help', 'File', 'New tab', 'Same'].map((name) => byLabel.get(name).unsafe), [true, true, true, true, true, false]);
  assert.deepEqual(['Home', 'Picked', 'Pressed', 'Send', 'Send ok'].map((name) => byLabel.get(name).quiet), [true, true, true, true, false]);
  assert.equal(byLabel.get('Delete').act, '{"method":"DELETE","url":"/api/x"}');
});

test('an open sheet or chat narrows the page to itself', (t) => {
  const env = withPage(t, shell);
  assert.equal(window.crawl.overlay(), '');
  assert.equal(window.crawl.area().id, 'screen');
  env.document.getElementById('chat').hidden = false;
  assert.deepEqual([window.crawl.overlay(), window.crawl.area().id, window.crawl.targets().map((target) => `${target.testid} ${target.place}`)], ['chat', 'chat', ['chat-send chat']]);
  env.document.getElementById('sheet').hidden = false;
  assert.deepEqual([window.crawl.overlay(), window.crawl.area().id, window.crawl.targets().map((target) => `${target.testid} ${target.place}`)], ['sheet', 'sheet', ['sheet-close sheet']]);
});

test('a wanted target is found again by testid, route, sheet, action or label, and only it is marked', (t) => {
  const env = withPage(t, shell);
  const marked = () => [...env.document.querySelectorAll('[data-crawl]')].map((node) => node.textContent.trim());
  assert.equal(window.crawl.mark({ testid: 'board-add', nav: '/ui/docs' }).testid, 'board-add');
  assert.equal(window.crawl.mark({ nav: '/ui/docs', sheet: '/ui/board/new' }).label, 'Docs');
  assert.equal(window.crawl.mark({ sheet: '/ui/board/new', act: 'x' }).testid, 'board-add');
  assert.equal(window.crawl.mark({ act: '{"method":"DELETE","url":"/api/x"}', label: 'Help' }).testid, 'board-delete');
  assert.deepEqual(marked(), ['Delete']);
  const revoked = window.crawl.mark({ label: 'Revoke 7' });
  assert.deepEqual([revoked.label, revoked.index], ['Revoke 12', 4]);
  assert.equal(window.crawl.mark({ testid: 'gone' }), null);
  assert.deepEqual(marked(), []);
  assert.equal(window.crawl.loose('task 12 of 3'), 'task # of #');
});

test('typing goes into every empty, visible, editable text field of the open part of the page', (t) => {
  const env = withPage(t, shell);
  const typed = [];
  env.document.addEventListener('input', (event) => typed.push(event.target.tagName));
  window.crawl.fill('hello');
  const values = [...env.document.querySelectorAll('#screen input, #screen textarea')].map((field) => field.value);
  assert.deepEqual(values, ['hello', 'hello', 'hello', 'kept', '', 'hello', 'on', '']);
  assert.deepEqual(typed, ['INPUT', 'INPUT', 'INPUT', 'TEXTAREA']);
  assert.equal(env.document.querySelector('#sheet input').value, '');
});

test('the page tells its screen, whether it started and has content, its routes, animations and alerts', (t) => {
  const env = withPage(t, shell);
  assert.equal(window.crawl.screen(), 'board');
  assert.equal(window.crawl.started(), false);
  window.Alpine = {};
  assert.equal(window.crawl.started(), true);
  assert.equal(window.crawl.filled(), true);
  assert.deepEqual(window.crawl.routes(), ['/ui/docs']);
  assert.equal(window.crawl.alert(), '');
  env.document.getElementById('toast').dataset.kind = 'alert';
  assert.equal(window.crawl.alert(), 'Saved');
  env.document.getElementById('toast').hidden = true;
  assert.equal(window.crawl.alert(), '');
  assert.equal(window.crawl.still(), true);
  const timing = (endTime) => ({ getComputedTiming: () => ({ endTime }) });
  env.animations.push({ playState: 'finished', effect: timing(10) }, { playState: 'running', effect: timing(Infinity) });
  assert.equal(window.crawl.still(), true);
  env.animations.push({ playState: 'running', effect: timing(300) });
  assert.equal(window.crawl.still(), false);
  env.animations.splice(0, env.animations.length, { playState: 'running' });
  assert.equal(window.crawl.still(), false);
  assert.equal(window.crawl.clickable, 'button, a[href], [data-nav], [data-sheet], [data-do], [role="button"], summary');
  assert.equal(window.crawl.nodes().length, 16);
  env.document.querySelector('#screen [data-screen]').remove();
  assert.deepEqual([window.crawl.screen(), window.crawl.filled()], ['', false]);
});

test('an empty screen is not filled and a long label is cut to 60 characters', (t) => {
  const env = withPage(t, `<div id="screen"><div data-screen="empty">   </div></div><button>${'word '.repeat(20)}</button>`);
  assert.equal(window.crawl.filled(), false);
  assert.equal(window.crawl.label(env.document.querySelector('button')).length, 60);
  assert.equal(window.crawl.label(env.document.createElement('button')), '');
});

test('the aim is the middle of a target nothing covers', (t) => {
  const env = withPage(t, '<button id="go" class="a b" data-box="10 20 100 40"><span>go</span></button><div id="toast" class="toast">hi</div>');
  const button = env.document.getElementById('go');
  assert.deepEqual(aim('#missing'), { missing: true });
  env.document.elementFromPoint = (x, y) => (x === 60 && y === 40 ? button.firstChild : null);
  assert.deepEqual(aim('#go'), { x: 60, y: 40 });
  assert.ok(button.hasAttribute('data-scrolled'));
  env.document.elementFromPoint = () => button;
  assert.deepEqual(aim('#go'), { x: 60, y: 40 });
  env.document.elementFromPoint = () => env.document.getElementById('toast');
  assert.deepEqual(aim('#go'), { covered: 'div#toast.toast' });
  env.document.elementFromPoint = () => env.document.body;
  assert.deepEqual(aim('#go'), { covered: 'body' });
  env.document.elementFromPoint = () => null;
  assert.deepEqual(aim('#go'), { covered: 'nothing' });
});

test('a shot is taken with animations finished, volatile parts hidden and the caret clear, and all comes back after', (t) => {
  const env = withPage(t, '<p data-volatile style="visibility: visible">12:01</p><p data-volatile>now</p><input id="field">');
  const done = [];
  env.animations.push({ finish: () => done.push('finished') }, { finish: () => { throw new Error('infinite'); }, cancel: () => done.push('cancelled') });
  const field = env.document.getElementById('field');
  field.style.caretColor = 'red';
  field.focus();
  assert.equal(holdStill(), true);
  assert.deepEqual(done, ['finished', 'cancelled']);
  const volatile = [...env.document.querySelectorAll('[data-volatile]')];
  assert.deepEqual(volatile.map((node) => node.style.visibility), ['hidden', 'hidden']);
  assert.equal(field.style.caretColor, 'transparent');
  letGo();
  assert.deepEqual(volatile.map((node) => node.style.visibility), ['visible', '']);
  assert.equal(field.style.caretColor, 'red');
  assert.ok(window.crawlStill === undefined, 'nothing is held after letting go');
  letGo();
  field.blur();
  holdStill();
  assert.ok(window.crawlStill.field === env.document.body);
  letGo();
  assert.equal(env.document.body.style.caretColor, '');
});

test('a frozen clock gives the same time to every reader but keeps real dates', (t) => {
  const Real = Date;
  t.after(() => {
    globalThis.Date = Real;
  });
  const at = Real.UTC(2030, 0, 15, 9, 30);
  frozenClock(at);
  assert.equal(Date.now(), at);
  assert.equal(new Date().getTime(), at);
  assert.equal(new Date(0).getTime(), 0);
  assert.equal(new Date(2001, 1).getFullYear(), 2001);
  assert.equal(Date(), new Real(at).toString());
  assert.ok(new Date() instanceof Real);
  assert.equal(Date.parse('2030-01-15T09:30:00Z'), at);
  assert.equal(Date.UTC(2030, 0, 15, 9, 30), at);
});

function fakeImages(t) {
  const decoded = new Map();
  const drawn = [];
  globalThis.createImageBitmap = async (blob) => {
    const text = Buffer.from(await blob.arrayBuffer()).toString();
    return decoded.get(text);
  };
  globalThis.OffscreenCanvas = class {
    constructor(width, height) {
      this.size = [width, height];
    }

    getContext(kind, options) {
      assert.deepEqual([kind, options], ['2d', { willReadFrequently: true }]);
      let current = null;
      return {
        clearRect: (...args) => drawn.push(['clear', ...args]),
        drawImage: (image, x, y) => {
          current = image;
          drawn.push(['draw', x, y]);
        },
        getImageData: () => ({ data: Uint8ClampedArray.from(current.pixels) }),
        createImageData: (width, height) => ({ data: new Uint8ClampedArray(width * height * 4) }),
        putImageData: (image) => {
          this.put = image;
        },
      };
    }

    async convertToBlob(options) {
      assert.deepEqual(options, { type: 'image/png' });
      return new Blob([Uint8Array.from(this.put.data)]);
    }
  };
  t.after(() => {
    delete globalThis.createImageBitmap;
    delete globalThis.OffscreenCanvas;
  });
  return {
    drawn,
    image(name, width, height, pixels) {
      decoded.set(name, { width, height, pixels });
      return Buffer.from(name).toString('base64');
    },
  };
}

test('two shots are compared pixel by pixel within the tolerance and the differing pixels are painted red', async (t) => {
  const images = fakeImages(t);
  const before = images.image('before', 2, 1, [10, 10, 10, 255, 100, 100, 100, 255]);
  const after = images.image('after', 2, 1, [30, 10, 10, 255, 100, 100, 135, 255]);
  const result = await pixelDiff({ before, after, tolerance: 20 });
  assert.deepEqual([result.differ, result.total], [1, 2]);
  const painted = [...Buffer.from(result.diff, 'base64')];
  assert.deepEqual(painted, [166, 166, 166, 255, 255, 0, 0, 255]);
  assert.deepEqual(images.drawn, [['clear', 0, 0, 2, 1], ['draw', 0, 0], ['clear', 0, 0, 2, 1], ['draw', 0, 0]]);
  const exact = await pixelDiff({ before, after: images.image('after2', 2, 1, [10, 30, 10, 255, 100, 100, 120, 255]), tolerance: 20 });
  assert.deepEqual(exact, { differ: 0, total: 2, diff: '' });
  const blueOff = await pixelDiff({ before, after: images.image('after3', 2, 1, [10, 10, 31, 255, 100, 121, 100, 255]), tolerance: 20 });
  assert.equal(blueOff.differ, 2);
  const resized = await pixelDiff({ before, after: images.image('wide', 3, 1, [0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0]), tolerance: 20 });
  assert.deepEqual(resized, { differ: 3, total: 3, diff: '' });
  const taller = await pixelDiff({ before, after: images.image('tall', 2, 2, new Array(16).fill(0)), tolerance: 20 });
  assert.deepEqual(taller, { differ: 4, total: 4, diff: '' });
});

test('a large difference is encoded in chunks without losing a byte', async (t) => {
  const images = fakeImages(t);
  const width = 12000;
  const black = new Array(width * 4).fill(0);
  const white = new Array(width * 4).fill(255);
  const result = await pixelDiff({ before: images.image('black', width, 1, black), after: images.image('white', width, 1, white), tolerance: 0 });
  assert.equal(result.differ, width);
  const bytes = Buffer.from(result.diff, 'base64');
  assert.equal(bytes.length, width * 4);
  assert.ok(bytes.every((value, index) => value === [255, 0, 0, 255][index % 4]));
});
