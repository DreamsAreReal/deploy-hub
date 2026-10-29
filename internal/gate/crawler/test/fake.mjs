import { EventEmitter } from 'node:events';
import { deliver } from '../cdp.mjs';
import { newCrawl } from '../record.mjs';
import { mkdtempSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

export const evaluated = (value) => ({ result: { value } });

export function fakeCdp(answer = () => ({})) {
  const cdp = { next: 0, calls: new Map(), handlers: new Map(), session: undefined };
  cdp.socket = {
    readyState: WebSocket.OPEN,
    sent: [],
    send(text) {
      const message = JSON.parse(text);
      cdp.socket.sent.push(message);
      Promise.resolve()
        .then(() => answer(message))
        .then(
          (result) => deliver(cdp, { id: message.id, result }),
          (error) => deliver(cdp, { id: message.id, error: { message: error.message } }),
        );
    },
  };
  return cdp;
}

export function memoryLog() {
  const lines = [];
  return { lines, write: (text) => lines.push(text), end: () => lines.push('END') };
}

export function fakeCrawl(answer, config = {}) {
  const out = mkdtempSync(join(tmpdir(), 'crawl-test-'));
  const log = memoryLog();
  const crawl = newCrawl({ out, screens: new Map(), forbidden: [], accepted: new Set(), baseline: '', frozenAt: Number.NaN, typed: 'typed', base: 'https://app', ...config }, () => log);
  crawl.cdp = fakeCdp(answer);
  return { crawl, log, out };
}

export const methods = (crawl) => crawl.cdp.socket.sent.map((message) => message.method);

export function socketClass(answer = () => ({})) {
  return class ScriptedSocket extends EventTarget {
    constructor(url) {
      super();
      this.url = url;
      this.readyState = WebSocket.OPEN;
      this.sent = [];
      queueMicrotask(() => this.dispatchEvent(new Event('open')));
    }

    send(text) {
      const message = JSON.parse(text);
      this.sent.push(message);
      Promise.resolve()
        .then(() => answer(message, this))
        .then(
          (result) => this.hear({ id: message.id, result }),
          (error) => this.hear({ id: message.id, error: { message: error.message } }),
        );
    }

    hear(message) {
      this.dispatchEvent(Object.assign(new Event('message'), { data: JSON.stringify(message) }));
    }
  };
}

export function fakeLaunch(said = 'DevTools listening on ws://127.0.0.1:9222/devtools/browser/x\n') {
  const launched = [];
  const launch = (file, args, options) => {
    const chrome = Object.assign(new EventEmitter(), { stderr: new EventEmitter(), killed: 0 });
    chrome.kill = () => {
      chrome.killed += 1;
    };
    launched.push({ file, args, options, chrome });
    setImmediate(() => chrome.stderr.emit('data', said));
    return chrome;
  };
  return { launch, launched };
}
