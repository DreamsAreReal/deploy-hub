import { test } from 'node:test';
import assert from 'node:assert/strict';
import { limits } from '../config.mjs';
import { listen, stuck } from '../network.mjs';
import { evaluated, fakeCrawl, methods } from './fake.mjs';

const start = 1_000_000;

function listening(t, answer) {
  t.mock.timers.enable({ apis: ['Date'], now: start });
  const made = fakeCrawl(answer);
  listen(made.crawl);
  const fire = (method, params) => made.crawl.cdp.handlers.get(method)(params);
  return { ...made, fire };
}

const kinds = (entries) => entries.map((entry) => `${entry.kind}: ${entry.detail}`);
const request = (requestId, url, extra = {}) => ({ requestId, type: 'Fetch', request: { url, method: 'POST' }, loaderId: 'L1', ...extra });

test('page errors, console errors and log entries become findings by their kind', async (t) => {
  const { crawl, fire } = listening(t);
  fire('Runtime.exceptionThrown', { exceptionDetails: { text: 'Uncaught', exception: { description: 'TypeError: x is null\n    at app.js:1' } } });
  fire('Runtime.exceptionThrown', { exceptionDetails: { text: 'Uncaught (in promise)' } });
  fire('Runtime.consoleAPICalled', { type: 'log', args: [{ value: 'fine' }] });
  fire('Runtime.consoleAPICalled', { type: 'error', args: [{ value: 'failed' }, { description: 'Error: deep' }, {}] });
  for (const entry of [
    { level: 'warning', source: 'other', text: 'just a warning' },
    { level: 'error', source: 'network', text: 'net' },
    { level: 'error', source: 'javascript', text: 'js' },
    { level: 'error', source: 'intervention', text: 'blocked vibrate', url: 'https://app/a.js' },
    { level: 'error', source: 'violation', text: 'slow handler' },
    { level: 'error', source: 'other', text: 'CSP refused', url: 'https://app' },
  ]) fire('Log.entryAdded', { entry });
  assert.deepEqual(kinds(crawl.problems), ['js-error: TypeError: x is null', 'js-error: Uncaught (in promise)', 'console-error: failed Error: deep ',
    'console-error: CSP refused https://app']);
  assert.deepEqual(kinds(crawl.notes), ['intervention: blocked vibrate https://app/a.js', 'violation: slow handler']);
});

test('a dialog is dismissed, a load resolves the wait and only the main frame sets the loader', async (t) => {
  const { crawl, fire } = listening(t, (message) => (message.method === 'Page.handleJavaScriptDialog' ? Promise.reject(new Error('gone')) : {}));
  fire('Page.javascriptDialogOpening', {});
  await new Promise((done) => setImmediate(done));
  assert.deepEqual(crawl.cdp.socket.sent.map((message) => [message.method, message.params]), [['Page.handleJavaScriptDialog', { accept: false }]]);
  fire('Page.loadEventFired', {});
  let loaded = 0;
  crawl.loaded = () => {
    loaded += 1;
  };
  fire('Page.loadEventFired', {});
  assert.equal(loaded, 1);
  fire('Page.frameNavigated', { frame: { loaderId: 'L2' } });
  fire('Page.frameNavigated', { frame: { parentId: 'F1', loaderId: 'L3' } });
  assert.equal(crawl.loader, 'L2');
});

test('only page requests are counted and each knows what sent it and whether a click caused it', (t) => {
  const { crawl, fire } = listening(t);
  crawl.actionAt = start - limits.caused;
  fire('Network.requestWillBeSent', request('1', 'https://app/api', { initiator: { type: 'script', stack: { callFrames: [{ functionName: '', url: 'https://app/static/app.js', lineNumber: 9 }] } } }));
  crawl.actionAt = start - limits.caused - 1;
  fire('Network.requestWillBeSent', { requestId: '2', request: { url: 'https://app/x', method: 'GET' } });
  fire('Network.requestWillBeSent', request('3', 'data:image/png;base64,AA'));
  fire('Network.requestWillBeSent', request('4', 'https://app/live', { type: 'EventSource' }));
  fire('Network.requestWillBeSent', request('5', 'https://app/save', { initiator: { type: 'script', stack: { callFrames: [{ functionName: 'save', url: 'https://app/card.js', lineNumber: 0 }] } } }));
  assert.equal(crawl.requests, 3);
  assert.deepEqual(crawl.opened, ['https://app/api', 'https://app/x', 'https://app/save']);
  assert.deepEqual(crawl.pending.get('1'), { url: 'https://app/api', method: 'POST', at: start, caused: true, by: 'Fetch by script anonymous app.js:10', loader: 'L1' });
  assert.deepEqual([crawl.pending.get('2').caused, crawl.pending.get('2').by], [false, 'Other by other']);
  assert.equal(crawl.pending.get('5').by, 'Fetch by script save card.js:1');
});

test('answers of 5xx are problems, 4xx are notes and slow answers are told by who caused them', (t) => {
  const { crawl, fire } = listening(t);
  crawl.actionAt = start;
  fire('Network.requestWillBeSent', request('1', 'https://app/a'));
  fire('Network.requestWillBeSent', request('2', 'https://app/b'));
  crawl.actionAt = 0;
  fire('Network.requestWillBeSent', request('3', 'https://app/c'));
  fire('Network.requestWillBeSent', request('4', 'https://app/d'));
  fire('Network.responseReceived', { requestId: '1', response: { status: 503, url: 'https://app/a' } });
  fire('Network.responseReceived', { requestId: '9', response: { status: 404, url: 'https://app/gone' } });
  fire('Network.responseReceived', { requestId: '9', response: { status: 399, url: 'https://app/redirect' } });
  fire('Network.responseReceived', { requestId: '9', response: { status: 499, url: 'https://app/closed' } });
  fire('Network.responseReceived', { requestId: '9', response: { status: 500, url: 'https://app/boom' } });
  fire('Network.responseReceived', { requestId: '9', response: { status: 400, url: 'https://app/bad' } });
  t.mock.timers.tick(limits.slow);
  fire('Network.loadingFinished', { requestId: '4' });
  t.mock.timers.tick(1);
  fire('Network.loadingFinished', { requestId: '2' });
  fire('Network.loadingFinished', { requestId: '3' });
  fire('Network.loadingFinished', { requestId: '8' });
  assert.deepEqual(kinds(crawl.problems), ['http-503: POST https://app/a → 503', 'http-500: GET https://app/boom → 500', 'slow: POST https://app/b took 3001 ms']);
  assert.deepEqual(kinds(crawl.notes), ['http-404: GET https://app/gone → 404', 'http-499: GET https://app/closed → 499', 'http-400: GET https://app/bad → 400',
    'slow-background: POST https://app/c took 3001 ms']);
  assert.equal(crawl.pending.size, 0);
});

test('a failed request is a problem unless it was cancelled', (t) => {
  const { crawl, fire } = listening(t);
  fire('Network.requestWillBeSent', request('1', 'https://app/a'));
  fire('Network.requestWillBeSent', request('2', 'https://app/b'));
  fire('Network.requestWillBeSent', request('3', 'https://app/c'));
  fire('Network.loadingFailed', { requestId: '1', errorText: 'net::ERR_BLOCKED_BY_CLIENT', blockedReason: 'inspector' });
  fire('Network.loadingFailed', { requestId: '2', canceled: true, errorText: 'net::ERR_ABORTED' });
  fire('Network.loadingFailed', { requestId: '3', errorText: 'net::ERR_FAILED' });
  fire('Network.loadingFailed', { requestId: '4', errorText: 'net::ERR_FAILED' });
  assert.deepEqual(kinds(crawl.problems), ['request-failed: POST https://app/a net::ERR_BLOCKED_BY_CLIENT inspector', 'request-failed: POST https://app/c net::ERR_FAILED']);
  assert.equal(crawl.pending.size, 0);
});

test('a hung request is told once and its late end becomes a note', async (t) => {
  const answered = new Set(['https://app/answered']);
  const { crawl, fire } = listening(t, (message) => evaluated([...answered].some((url) => message.params.expression.includes(JSON.stringify(url)))));
  crawl.loader = 'L1';
  crawl.actionAt = start;
  fire('Network.requestWillBeSent', request('1', 'https://app/click'));
  fire('Network.requestWillBeSent', request('2', 'https://app/answered'));
  fire('Network.requestWillBeSent', request('3', 'https://app/old', { loaderId: 'L0' }));
  crawl.actionAt = 0;
  fire('Network.requestWillBeSent', request('4', 'https://app/poll'));
  fire('Network.requestWillBeSent', request('5', 'https://app/fresh', { loaderId: undefined }));
  await stuck(crawl);
  assert.deepEqual(kinds(crawl.problems), [`hang: no answer in ${limits.hang} ms: https://app/click (Fetch by other)`]);
  assert.deepEqual(kinds(crawl.notes), [
    'lost-network-event: POST https://app/answered was answered in the page, Chrome sent no end of it',
    'left-with-its-page: POST https://app/old was sent by a page that has since been replaced',
    `background-hang: no answer in ${limits.hang} ms: https://app/poll (Fetch by other), https://app/fresh (Fetch by other)`,
  ]);
  assert.equal(crawl.pending.size, 0);
  assert.deepEqual([...crawl.hung.keys()], ['1', '4', '5']);
  assert.ok(methods(crawl).every((method) => method === 'Runtime.evaluate'));
  t.mock.timers.tick(250);
  fire('Network.loadingFinished', { requestId: '1' });
  fire('Network.loadingFailed', { requestId: '4', canceled: true });
  fire('Network.loadingFailed', { requestId: '5', errorText: 'net::ERR_TIMED_OUT' });
  assert.deepEqual(kinds(crawl.notes).slice(3), ['late-answer: POST https://app/click answered after 250 ms', 'late-answer: POST https://app/poll was cancelled after 250 ms',
    'late-answer: POST https://app/fresh failed: net::ERR_TIMED_OUT after 250 ms']);
  assert.equal(crawl.hung.size, 0);
  crawl.loader = undefined;
  fire('Network.requestWillBeSent', request('6', 'https://app/next', { loaderId: 'L9' }));
  await stuck(crawl);
  assert.equal(crawl.hung.size, 1);
  assert.equal(crawl.problems.length, 1);
  assert.equal(kinds(crawl.notes).at(-1), `background-hang: no answer in ${limits.hang} ms: https://app/next (Fetch by other)`);
});
