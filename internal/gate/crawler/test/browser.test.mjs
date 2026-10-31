import { test } from 'node:test';
import assert from 'node:assert/strict';
import { start } from '../browser.mjs';
import { chromeFlags, limits } from '../config.mjs';
import { checks, frozenClock, helpers } from '../page.mjs';
import { fakeCrawl, fakeLaunch, socketClass } from './fake.mjs';

const answers = (message) => ({ 'Target.createTarget': { targetId: 'T1' }, 'Target.attachToTarget': { sessionId: 'S1' } })[message.method] ?? {};

test('chrome starts headless with its own profile, denies what the crawl must not use, hides the fading mobile scrollbar and loads the page helpers', async () => {
  const { crawl, out } = fakeCrawl(undefined, { chromium: '/opt/chrome', netLog: '/out/net.json', frozenAt: 1893490200000, base: 'https://web:8080/app' });
  const { launch, launched } = fakeLaunch();
  const Socket = socketClass(answers);
  const chrome = await start(crawl, launch, Socket);
  assert.equal(chrome, launched[0].chrome);
  assert.equal(launched[0].file, '/opt/chrome');
  assert.deepEqual(launched[0].args, [...chromeFlags, '--unsafely-treat-insecure-origin-as-secure=https://web:8080', '--log-net-log=/out/net.json', '--net-log-capture-mode=Default',
    `--user-data-dir=${out}/profile`, 'about:blank']);
  assert.deepEqual(launched[0].options, { stdio: ['ignore', 'ignore', 'pipe'] });
  assert.equal(crawl.cdp.socket.url, 'ws://127.0.0.1:9222/devtools/browser/x');
  assert.equal(crawl.cdp.session, 'S1');
  const sent = crawl.cdp.socket.sent;
  assert.deepEqual(sent.map((message) => message.method), ['Browser.setDownloadBehavior', 'Browser.setPermission', 'Browser.setPermission', 'Target.createTarget',
    'Target.attachToTarget', 'Page.enable', 'Runtime.enable', 'Network.enable', 'Log.enable', 'Emulation.setDeviceMetricsOverride',
    'Emulation.setScrollbarsHidden', 'Page.addScriptToEvaluateOnNewDocument', 'Page.addScriptToEvaluateOnNewDocument', 'Page.addScriptToEvaluateOnNewDocument',
    'Page.addScriptToEvaluateOnNewDocument']);
  assert.deepEqual(sent[0].params, { behavior: 'deny' });
  assert.deepEqual(sent.slice(1, 3).map((message) => message.params), ['notifications', 'microphone'].map((name) => ({ permission: { name }, setting: 'denied', origin: 'https://web:8080' })));
  assert.deepEqual(sent[4].params, { targetId: 'T1', flatten: true });
  assert.equal(sent[4].sessionId, undefined);
  assert.equal(sent[5].sessionId, 'S1');
  assert.deepEqual(sent[9].params, { width: 420, height: 860, deviceScaleFactor: 1, mobile: true });
  assert.deepEqual(sent[10].params, { hidden: true });
  assert.deepEqual(sent.slice(11).map((message) => message.params.source), [`(${frozenClock})(1893490200000)`, `(${helpers})()`, `(${checks})()`,
    `performance.setResourceTimingBufferSize(${limits.timings})`]);
  assert.ok(crawl.cdp.handlers.has('Network.requestWillBeSent'));
  assert.equal(chrome.killed, 0);
});

test('without a net log or a frozen clock chrome starts plainly', async () => {
  const { crawl } = fakeCrawl(undefined, { chromium: 'chrome' });
  const { launch, launched } = fakeLaunch();
  await start(crawl, launch, socketClass(answers));
  assert.ok(!launched[0].args.some((arg) => arg.startsWith('--log-net-log') || arg.startsWith('--net-log')));
  const sources = crawl.cdp.socket.sent.filter((message) => message.method === 'Page.addScriptToEvaluateOnNewDocument').map((message) => message.params.source);
  assert.equal(sources.length, 3);
  assert.ok(!sources.some((source) => source.includes('Frozen')));
});

test('chrome is killed when it cannot be driven', async () => {
  const { crawl } = fakeCrawl(undefined, { chromium: 'chrome' });
  const { launch, launched } = fakeLaunch();
  const refusing = socketClass((message) => (message.method === 'Target.createTarget' ? Promise.reject(new Error('no targets')) : {}));
  await assert.rejects(start(crawl, launch, refusing), { message: 'Target.createTarget: no targets' });
  assert.equal(launched[0].chrome.killed, 1);
  const silent = fakeLaunch('starting\n');
  const pending = start(fakeCrawl(undefined, { chromium: 'chrome' }).crawl, silent.launch, socketClass(answers));
  await new Promise((done) => setImmediate(done));
  silent.launched[0].chrome.emit('exit', 127);
  await assert.rejects(pending, { message: 'chrome exited with 127: starting\n' });
  assert.equal(silent.launched[0].chrome.killed, 1);
});
