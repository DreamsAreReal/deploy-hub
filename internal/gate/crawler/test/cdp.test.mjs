import { test } from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { limits } from '../config.mjs';
import { alive, call, connect, deliver, endpointOf, goto, inPage, send, tap, within } from '../cdp.mjs';
import { aim } from '../page.mjs';
import { evaluated, fakeCrawl, methods } from './fake.mjs';

const settled = () => new Promise((done) => setImmediate(done));

const made = [];

class FakeSocket extends EventTarget {
  constructor(url) {
    super();
    this.url = url;
    this.sent = [];
    made.push(this);
  }

  send(text) {
    this.sent.push(JSON.parse(text));
  }

  hear(message) {
    this.dispatchEvent(Object.assign(new Event('message'), { data: JSON.stringify(message) }));
  }
}

test('a promise that answers in time is passed through, a late one fails by name', async (t) => {
  assert.equal(await within(Promise.resolve('ok'), limits.hang, 'quick'), 'ok');
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const late = within(new Promise(() => null), 50, 'Page.navigate');
  t.mock.timers.tick(50);
  await assert.rejects(late, { message: 'Page.navigate: no answer in 50 ms' });
});

test('a message answers its call or reaches the handler of its event', async () => {
  const seen = [];
  const cdp = { calls: new Map(), handlers: new Map([['Page.loadEventFired', (params) => seen.push(params)]]) };
  const answered = new Promise((resolve, reject) => cdp.calls.set(1, { resolve, reject }));
  const refused = new Promise((resolve, reject) => cdp.calls.set(2, { resolve, reject }));
  deliver(cdp, { id: 1, result: { frameId: 'F' } });
  deliver(cdp, { id: 2, error: { message: 'No node' } });
  deliver(cdp, { method: 'Page.loadEventFired', params: { timestamp: 1 } });
  assert.equal(deliver(cdp, { method: 'Unknown.event', params: {} }), undefined);
  assert.deepEqual(await answered, { frameId: 'F' });
  await assert.rejects(refused, { message: 'No node' });
  assert.deepEqual(seen, [{ timestamp: 1 }]);
  assert.equal(cdp.calls.size, 0);
});

test('a connection opens on its socket, numbers its calls and routes the answers', async () => {
  const opening = connect('wss://chrome/devtools', FakeSocket);
  const socket = made.at(-1);
  assert.equal(socket.url, 'wss://chrome/devtools');
  socket.dispatchEvent(new Event('open'));
  const cdp = await opening;
  cdp.session = 'S1';
  const first = send(cdp, 'Page.enable');
  const second = send(cdp, 'Page.navigate', { url: 'https://app' });
  assert.deepEqual(socket.sent, [{ id: 1, method: 'Page.enable', params: {}, sessionId: 'S1' }, { id: 2, method: 'Page.navigate', params: { url: 'https://app' }, sessionId: 'S1' }]);
  socket.hear({ id: 2, result: { frameId: 'F' } });
  socket.hear({ id: 1, result: {} });
  assert.deepEqual(await Promise.all([first, second]), [{}, { frameId: 'F' }]);
  const failing = connect('wss://chrome/broken', FakeSocket);
  made.at(-1).dispatchEvent(new Event('error'));
  await assert.rejects(failing);
});

test('a call without an answer fails after the hang limit', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const { crawl } = fakeCrawl(() => new Promise(() => null));
  const waiting = call(crawl, 'DOM.getDocument', {});
  await settled();
  t.mock.timers.tick(limits.hang);
  await assert.rejects(waiting, { message: `DOM.getDocument: no answer in ${limits.hang} ms` });
});

test('the DevTools address is read from what chrome says, even split in pieces', async () => {
  const chrome = Object.assign(new EventEmitter(), { stderr: new EventEmitter() });
  const found = endpointOf(chrome);
  chrome.stderr.emit('data', 'starting\nDevTools listening on ws://127.0.0.1:9');
  chrome.stderr.emit('data', '222/devtools/browser/abc\n');
  assert.equal(await found, 'ws://127.0.0.1:9222/devtools/browser/abc');
  const dead = Object.assign(new EventEmitter(), { stderr: new EventEmitter() });
  const failed = endpointOf(dead);
  dead.stderr.emit('data', `${'x'.repeat(limits.detail)}cannot open display`);
  dead.emit('exit', 1);
  const tail = 'cannot open display';
  await assert.rejects(failed, { message: `chrome exited with 1: ${'x'.repeat(limits.detail - tail.length)}${tail}` });
});

test('code runs in the page with its argument and gives back the value, or null when it throws', async () => {
  const answers = [evaluated({ ok: true }), { exceptionDetails: { text: 'ReferenceError' } }];
  const { crawl } = fakeCrawl(() => answers.shift() ?? Promise.reject(new Error('closed')));
  assert.deepEqual(await inPage(crawl, (value) => value.length, 'abc'), { ok: true });
  assert.equal(await inPage(crawl, () => globalThis.missing.call()), null);
  assert.equal(await inPage(crawl, () => 1), null);
  const [first, second] = crawl.cdp.socket.sent;
  assert.deepEqual(first.params, { expression: '((value) => value.length)("abc")', awaitPromise: true, returnByValue: true });
  assert.equal(second.params.expression, '(() => globalThis.missing.call())(null)');
});

test('a navigation waits for the load, fails on a navigation error and on a load that never comes', async (t) => {
  const made = fakeCrawl((message) => {
    if (message.params.url.endsWith('/bad')) return { errorText: 'net::ERR_CONNECTION_REFUSED' };
    if (message.params.url.endsWith('/ok')) setImmediate(() => made.crawl.loaded());
    return { frameId: 'F' };
  });
  const { crawl } = made;
  await goto(crawl, 'https://app/ok');
  await assert.rejects(goto(crawl, 'https://app/bad'), { message: 'https://app/bad: net::ERR_CONNECTION_REFUSED' });
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const stalled = goto(crawl, 'https://app/stall');
  await settled();
  t.mock.timers.tick(limits.hang);
  await assert.rejects(stalled, { message: `load https://app/stall: no answer in ${limits.hang} ms` });
  assert.deepEqual(crawl.cdp.socket.sent.map((message) => message.params.url), ['https://app/ok', 'https://app/bad', 'https://app/stall']);
});

test('a tap presses the middle of a visible target, or tells why it could not', async (t) => {
  t.mock.timers.enable({ apis: ['Date'], now: 5000 });
  const spots = [evaluated({ x: 10, y: 20 }), evaluated({ missing: true }), evaluated(null), evaluated({ covered: 'html' }), evaluated({ covered: 'div#toast.toast' })];
  const { crawl } = fakeCrawl((message) => (message.method === 'Runtime.evaluate' ? spots.shift() : Promise.reject(new Error('detached'))));
  assert.equal(await tap(crawl, '[data-crawl]', 'board-add'), true);
  assert.equal(crawl.actionAt, 5000);
  const presses = crawl.cdp.socket.sent.filter((message) => message.method === 'Input.dispatchMouseEvent').map((message) => message.params);
  assert.deepEqual(presses, ['mouseMoved', 'mousePressed', 'mouseReleased'].map((type) => ({ type, x: 10, y: 20, button: 'left', clickCount: 1 })));
  assert.ok(crawl.cdp.socket.sent[0].params.expression.startsWith(`(${aim})("[data-crawl]")`));
  for (let left = 4; left > 0; left -= 1) await tap(crawl, '[data-crawl]', 'board-add');
  assert.deepEqual(crawl.notes.map((entry) => entry.detail), ['board-add is not on the page', 'board-add is covered by html']);
  assert.deepEqual(crawl.problems.map((entry) => entry.detail), ['board-add is covered by div#toast.toast']);
  assert.equal(methods(crawl).length, 8);
});

test('the crawl is alive while its socket is open', () => {
  const { crawl } = fakeCrawl();
  assert.equal(alive(crawl), true);
  crawl.cdp.socket.readyState = WebSocket.CLOSED;
  assert.equal(alive(crawl), false);
});
