import { limits } from './config.mjs';
import { aim } from './page.mjs';
import { note, problem } from './record.mjs';

export function within(promise, ms, what) {
  let timer;
  const late = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error(`${what}: no answer in ${ms} ms`)), ms);
  });
  return Promise.race([promise, late]).finally(() => clearTimeout(timer));
}

export function deliver(cdp, message) {
  const waiting = cdp.calls.get(message.id);
  if (!waiting) return cdp.handlers.get(message.method)?.(message.params);
  cdp.calls.delete(message.id);
  if (message.error) waiting.reject(new Error(message.error.message));
  else waiting.resolve(message.result);
}

export async function connect(url, Socket = WebSocket) {
  const socket = new Socket(url);
  await new Promise((resolve, reject) => {
    socket.addEventListener('open', resolve);
    socket.addEventListener('error', reject);
  });
  const cdp = { socket, next: 0, calls: new Map(), handlers: new Map(), session: undefined };
  socket.addEventListener('message', (event) => deliver(cdp, JSON.parse(event.data)));
  return cdp;
}

export function send(cdp, method, params = {}) {
  cdp.next += 1;
  const id = cdp.next;
  return new Promise((resolve, reject) => {
    cdp.calls.set(id, { resolve, reject });
    cdp.socket.send(JSON.stringify({ id, method, params, sessionId: cdp.session }));
  });
}

export const call = (crawl, method, params) => within(send(crawl.cdp, method, params), limits.hang, method);

export function endpointOf(chrome) {
  return new Promise((resolve, reject) => {
    let said = '';
    chrome.stderr.on('data', (chunk) => {
      said += chunk;
      const found = /DevTools listening on (ws:\S+)\s/.exec(said);
      if (found) resolve(found[1]);
    });
    chrome.on('exit', (code) => reject(new Error(`chrome exited with ${code}: ${said.slice(-limits.detail)}`)));
  });
}

async function evaluate(crawl, fn, arg) {
  const result = await call(crawl, 'Runtime.evaluate', { expression: `(${fn})(${JSON.stringify(arg ?? null)})`, awaitPromise: true, returnByValue: true });
  if (result.exceptionDetails) throw new Error(result.exceptionDetails.text);
  return result.result.value;
}

export const inPage = (crawl, fn, arg) => evaluate(crawl, fn, arg).catch(() => null);

export async function goto(crawl, url) {
  const loaded = new Promise((resolve) => {
    crawl.loaded = resolve;
  });
  const navigated = await call(crawl, 'Page.navigate', { url });
  if (navigated.errorText) throw new Error(`${url}: ${navigated.errorText}`);
  await within(loaded, limits.hang, `load ${url}`);
}

export async function tap(crawl, selector, name) {
  crawl.actionAt = Date.now();
  const spot = await inPage(crawl, aim, selector);
  if (!spot || spot.missing) return note(crawl, 'blocked', `${name} is not on the page`);
  if (spot.covered) return (spot.covered === 'html' ? note : problem)(crawl, 'covered', `${name} is covered by ${spot.covered}`);
  for (const type of ['mouseMoved', 'mousePressed', 'mouseReleased']) {
    await call(crawl, 'Input.dispatchMouseEvent', { type, x: spot.x, y: spot.y, button: 'left', clickCount: 1 }).catch(() => null);
  }
  return true;
}

export const alive = (crawl) => crawl.cdp.socket.readyState === WebSocket.OPEN;
