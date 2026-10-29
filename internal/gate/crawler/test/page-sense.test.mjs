import { test } from 'node:test';
import assert from 'node:assert/strict';
import { checks, helpers } from '../page.mjs';
import { checking, pageEnv } from './page-env.mjs';

const screen = (body, sheet = '') => `<div id="screen" data-box="0 0 400 800">${body}</div><div id="sheet" hidden>${sheet}</div>`;
const words = (count, word = 'word') => Array.from({ length: count }, () => word).join(' ');

function scenario(html, look) {
  const env = pageEnv(html);
  try {
    helpers();
    checks();
    return look(env);
  } finally {
    env.close();
  }
}

test('a red filled button on a screen is loud, quiet or sampled ones and sheets are not', (t) => {
  const red = 'background-color: rgb(220, 50, 50)';
  const env = checking(t, screen(`
    <button class="button button-danger" style="${red}">Delete</button><button class="text-button text-button-danger">Remove</button>
    <span class="danger-note" style="${red}">careful</span><button class="danger" style="background-color: rgb(150, 50, 50)">Edge</button>
    <button class="danger" style="background-color: rgb(151, 50, 50)">Over</button><button class="danger" style="background-color: rgb(200, 125, 50)">Orange</button>
    <button class="danger" style="background-color: rgb(200, 124, 50)">Reddish</button><button class="danger" style="background-color: rgb(200, 50, 125)">Pink</button>
    <button class="danger" style="background-color: rgb(200, 50, 124)">Rose</button><button class="danger" style="background-color: rgba(220, 50, 50, 0.5)">Faint</button>
    <button class="danger" style="background-color: rgba(220, 50, 50, 0.51)">Solid</button><button class="danger" hidden style="${red}">Hidden</button>
    <button class="plain" style="${red}">Unmarked</button><button class="danger" style="background-color: transparent">Clear</button>
    <div data-screen="design"><button class="button-danger" style="${red}">Sample</button></div>`, `<button class="button-danger" style="${red}">Confirm</button>`));
  assert.deepEqual(window.checks.loud(), ['button «Delete»', 'button «Over»', 'button «Reddish»', 'button «Rose»', 'button «Solid»']);
  env.document.getElementById('sheet').hidden = false;
  assert.deepEqual(window.checks.loud(), []);
});

test('a list of four rows or more with a long description per row is heavy, fifteen rows without search are long', (t) => {
  const row = (extra) => `<li><span>Title</span> ${words(extra)}</li>`;
  const env = checking(t, screen(`
    <ul data-testid="a" aria-label="tasks">${[21, 21, 21, 21].map(row).join('')}</ul>
    <ul data-testid="b" aria-label="notes">${[20, 20, 20, 20].map(row).join('')}</ul>
    <ul data-testid="c" aria-label="three">${[30, 30, 30].map(row).join('')}</ul>
    <ol data-testid="d" aria-label="mixed">${[26, 1, 25, 2].map(row).join('')}</ol>
    <ul data-testid="e" aria-label="plain">${Array.from({ length: 4 }, () => `<li>${words(21)}</li>`).join('')}</ul>
    <ul data-testid="f" aria-label="long">${Array.from({ length: 15 }, () => '<li>x</li>').join('')}</ul>
    <ul data-testid="g" aria-label="almost">${Array.from({ length: 14 }, () => '<li>x</li>').join('')}</ul>
    <ul hidden aria-label="hidden">${[30, 30, 30, 30].map(row).join('')}${Array.from({ length: 15 }, () => '<li>x</li>').join('')}</ul>
    <ul data-testid="h" aria-label="empty"></ul>`));
  assert.deepEqual(window.checks.heavy(), ['ul[a] «tasks»: 4 rows with 21 words of description each', 'ol[d] «mixed»: 4 rows with 25 words of description each',
    'ul[e] «plain»: 4 rows with 21 words of description each']);
  assert.deepEqual(window.checks.long(), ['ul[f] «long»: 15 rows and no search']);
  env.document.getElementById('screen').insertAdjacentHTML('afterbegin', '<input type="search" aria-label="find">');
  assert.deepEqual(window.checks.long(), []);
});

test('three or more names that are both plain text and buttons are listed twice', (t) => {
  const env = checking(t, screen(`
    <button>+  Add task</button><button>Delete</button><button>Open 12</button><button>OK</button><a href="/ui/docs">Docs</a>
    <p>Add task</p><p>delete</p><p>Open 7</p><p>ok</p><p>Elsewhere</p>`));
  assert.deepEqual(window.checks.twice(), ['3 names are both text and buttons: add task, delete, open #']);
  env.document.querySelector('p').remove();
  assert.deepEqual(window.checks.twice(), []);
  env.document.getElementById('screen').insertAdjacentHTML('beforeend', '<p>Docs</p><p>more docs</p>');
  assert.deepEqual(window.checks.twice(), ['3 names are both text and buttons: delete, open #, docs']);
});

test('text fields without dictation are voiceless only where the app offers dictation', (t) => {
  const env = checking(t, screen(`
    <textarea aria-label="notes"></textarea><input aria-label="name" data-dictation="ready"><input type="text" readonly aria-label="ro">
    <input type="search" aria-label="find"><input autocomplete="one-time-code" aria-label="code"><input data-no-dictation aria-label="secret">
    <input type="email" aria-label="mail"><input disabled aria-label="off"><textarea hidden aria-label="gone"></textarea>`));
  assert.deepEqual(window.checks.voiceless(), []);
  env.document.body.insertAdjacentHTML('beforeend', '<button id="dictate">mic</button>');
  assert.deepEqual(window.checks.voiceless(), []);
  window.MediaRecorder = function MediaRecorder() {
    return this;
  };
  assert.deepEqual(window.checks.voiceless(), ['textarea «notes»', 'input «find»']);
});

test('a button that moves when its field loses focus jumps, a still or removed one does not', async (t) => {
  const env = checking(t, screen(`
    <form><input aria-label="title" id="title"><button id="save" data-box="0 0 40 40">Save</button><button data-box="50 0 40 40">Cancel</button>
      <button id="gone" data-box="100 0 40 40">Gone</button><button hidden data-box="150 0 40 40">Hidden</button></form>
    <div><textarea aria-label="body" id="body"></textarea><button id="send" data-box="0 100 40 40">Send</button></div>
    <div><input aria-label="off" disabled><button id="near-off" data-box="0 200 40 40">Near</button></div>
    <div><input aria-label="fixed" readonly><button data-box="0 300 40 40">Fixed</button></div>`));
  const title = env.document.getElementById('title');
  title.addEventListener('focus', () => env.document.getElementById('send').setAttribute('data-box', '9 9 40 40'));
  title.addEventListener('blur', () => {
    env.document.getElementById('save').setAttribute('data-box', '0 50 40 40');
    env.document.getElementById('gone').remove();
  });
  env.document.getElementById('body').addEventListener('blur', () => env.document.getElementById('near-off').setAttribute('data-box', '9 9 40 40'));
  assert.deepEqual(await window.checks.jumps(), ['button «Save»']);
});

test('the gap under a title is measured past its subtitles to the next visible block', () => {
  const gap = (body) => scenario(screen(body), () => window.checks.titleGap());
  assert.equal(gap('<h1 data-box="0 0 300 30">Title</h1><p data-box="0 33 300 20">Subtitle</p><p hidden>x</p><div data-box="0 60 300 100">content</div>'), 7);
  assert.equal(gap('<h1 data-box="0 0 300 30">Title</h1><p data-box="0 38 300 20">Far subtitle</p>'), 8);
  assert.equal(gap('<h1 data-box="0 0 300 30">Title</h1><p data-box="0 37 300 20">Near</p><p data-box="0 64 300 20">Second</p>'), null);
  assert.equal(gap('<h1 data-box="0 0 300 30">Title</h1><p data-box="0 37 300 20">Near</p><p data-box="0 64 300 20">Second</p><div data-box="0 90 300 20">x</div>'), 6);
  assert.equal(gap('<div><h1 data-box="0 0 100 30">T</h1></div><section data-box="0 45 100 10">s</section>'), 15);
  assert.equal(gap('<div><h1 data-box="0 0 100 30">T</h1></div>'), null);
  assert.equal(gap('<p>No title</p>'), null);
  assert.equal(gap('<h1 hidden data-box="0 0 100 30">T</h1><p data-box="0 40 10 10">x</p>'), null);
  assert.equal(gap('<h1 data-box="0 0.4 100 30">T</h1><div data-box="0 40 10 10">x</div>'), 10);
  assert.equal(scenario(`<h1 data-box="0 0 100 30">Outside</h1>${screen('<p>x</p>')}`, () => window.checks.titleGap()), null);
});

test('buttons doing the same thing under the same name are twins and one name doing different things is ambiguous', () => {
  const body = `
    <button data-nav="/ui/a">Open</button><button data-nav="/ui/a">Open</button><button data-nav="/ui/b">Item 2</button><button data-nav="/ui/b">Item 3</button>
    <button @click="save()">Save</button><button @click="save()">Save</button><button data-nav="/ui/c">Go</button><button data-nav="/ui/d">Go</button>
    <button>Plain</button><button>Plain</button>
    <button data-do='{"method":"DELETE","url":"/api/tasks/7"}'>Remove</button><button data-do='{"method":"DELETE","url":"/api/tasks/8","body":{"x":1}}'>Remove</button>
    <button @click="edit('title', 5)">Edit</button><button @click="edit('body')">Edit</button><button data-do="{broken">Bad</button><button data-nav="/ui/bad/12">Bad</button>
    <button data-sheet="/ui/${'x'.repeat(70)}">Long</button><button data-nav="/ui/y">Long</button><button aria-label="" data-nav="/ui/z"></button><button data-nav="/ui/w"></button>
    <button @click="toggle()">Mode</button><button @click="open = !open">Mode</button>`;
  scenario(screen(body), () => {
    assert.deepEqual(window.checks.twins(), ['button «Open»', 'button «Item 3»', 'button «Save»']);
    assert.deepEqual(window.checks.ambiguous(), [
      '«go» does 2 things: /ui/c | /ui/d',
      '«remove» does 2 things: DELETE /api/tasks/# {} | DELETE /api/tasks/# {"x":#}',
      "«edit» does 2 things: edit('title') | edit('body')",
      '«bad» does 2 things: {broken | /ui/bad/#',
      `«long» does 2 things: /ui/${'x'.repeat(56)} | /ui/y`,
      '«mode» does 2 things: toggle() | open = !open',
    ]);
  });
});

test('a delete on the screen without undo or confirm is rash, and only plain main buttons are primary', () => {
  const body = `
    <button data-do='{"method":"DELETE","url":"/api/x"}'>Drop</button><button data-do='{"method":"DELETE","url":"/api/y","undo":{"method":"POST"}}'>Undo</button>
    <button data-do='{"method":"DELETE","url":"/api/z","confirm":"Sure?"}'>Ask</button><button data-do='{"method":"POST","url":"/api/w"}'>Post</button>
    <button hidden data-do='{"method":"DELETE","url":"/api/h"}'>Hidden</button>
    <button class="button">Save</button><button class="button button-quiet">Later</button><button class="button button-danger">Erase</button>
    <button class="button" hidden>Gone</button><button class="buttons">Other</button>`;
  scenario(screen(body, `<button class="button">Sheet main</button><button data-do='{"method":"DELETE","url":"/api/s"}'>Sheet drop</button>`), () => {
    assert.deepEqual(window.checks.rash(), ['button «Drop»']);
    assert.deepEqual(window.checks.primaries(), ['button «Save»']);
  });
});

test('a screen has a way back and an open sheet a way to close it', () => {
  const exits = (html, open = false) => scenario(html, (env) => {
    env.document.getElementById('sheet').hidden = !open;
    return window.checks.exits();
  });
  assert.deepEqual(exits(screen('<button data-testid="back">Back</button>', '<button data-close>x</button>'), true), { back: true, close: true });
  assert.deepEqual(exits(screen('<nav data-testid="roots"></nav>', '<button data-close>x</button>')), { back: true, close: false });
  assert.deepEqual(exits(`<button data-testid="back">Back</button>${screen('<p>x</p>', '<button>x</button>')}`, true), { back: false, close: false });
});

test('the first word of every button for one action is collected, unless the buttons disagree', () => {
  const body = `
    <button data-sheet="/ui/tasks/1/move">Move task</button><button data-sheet="/ui/tasks/2/move">Move</button>
    <button data-do='{"method":"POST","url":"/api/a"}'>Save</button><button data-do='{"method":"POST","url":"/api/a"}'>Store now</button>
    <div class="sheet-list"><button data-sheet="/ui/pick">Pick</button></div><button data-sheet="/ui/icon" aria-label=""></button>
    <button data-nav="/ui/nav">Navigate</button><button data-do='{"method":"PATCH","url":"/api/b"}'>Rename It</button>`;
  scenario(screen(body), () => {
    assert.deepEqual(window.checks.words(), [{ key: '/ui/tasks/#/move', word: 'move' }, { key: 'PATCH /api/b {}', word: 'rename' }]);
  });
});

test('the page signature changes with the page and only with it', (t) => {
  const env = checking(t, screen('<p>one</p>'));
  const first = window.checks.signature();
  const html = env.document.documentElement.outerHTML;
  let hash = 0;
  for (let index = 0; index < html.length; index += 1) hash = (hash * 31 + html.charCodeAt(index)) | 0;
  assert.equal(first, `https://app/ui/home ${html.length} ${hash}`);
  assert.equal(window.checks.signature(), first);
  env.document.querySelector('p').textContent = 'two';
  assert.notEqual(window.checks.signature(), first);
});

function observing(t, Observer) {
  const env = pageEnv(screen('<p>x</p><p>y</p>'));
  t.after(() => env.close());
  globalThis.PerformanceObserver = Observer;
  helpers();
  checks();
  return env;
}

test('layout shifts not caused by input and long tasks are measured until restarted', (t) => {
  const seen = new Map();
  const env = observing(t, class {
    constructor(callback) {
      this.callback = callback;
    }

    observe(options) {
      seen.set(options.type, { callback: this.callback, buffered: options.buffered });
    }
  });
  assert.deepEqual([...seen.keys()], ['layout-shift', 'longtask']);
  assert.ok([...seen.values()].every((entry) => entry.buffered === true));
  const deliver = (type, entries) => seen.get(type).callback({ getEntries: () => entries });
  deliver('layout-shift', [{ value: 0.1234, hadRecentInput: false }, { value: 0.5, hadRecentInput: true }]);
  deliver('layout-shift', [{ value: 0.0007, hadRecentInput: false }]);
  deliver('longtask', [{ duration: 201 }, { duration: 200 }, { duration: 500 }]);
  const weight = env.document.querySelectorAll('*').length;
  assert.deepEqual(window.checks.measured(), { shift: 0.124, slow: 2, watching: true, weight });
  window.checks.restart();
  assert.deepEqual(window.checks.measured(), { shift: 0, slow: 0, watching: true, weight });
});

test('a browser without the performance observer still measures the weight', (t) => {
  observing(t, class {
    observe() {
      throw new TypeError('unsupported entry type');
    }
  });
  assert.equal(window.checks.measured().watching, false);
});
