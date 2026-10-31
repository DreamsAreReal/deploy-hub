import { runInThisContext } from 'node:vm';
import { configure } from '../config.mjs';
import { pageEnv } from './page-env.mjs';
import { fakeLaunch, socketClass } from './fake.mjs';

export const shotBytes = Buffer.from('shot');
const clickable = 'button, a[href], [data-nav], [data-sheet], [data-do], [role="button"], summary';
const later = () => new Promise((done) => setImmediate(done));

function contains(node, x, y) {
  const [left, top, width, height] = node.getAttribute('data-box').split(' ').map(Number);
  return x >= left && x <= left + width && y >= top && y <= top + height;
}

export function shellOf(screen, { menu = true, assistant = true, chat = '<p>Chat</p>' } = {}) {
  const roots = menu ? '<nav data-testid="roots" data-box="0 760 400 40"><button data-testid="root-home" data-nav="/ui/home" data-box="0 752 100 48">Home</button></nav>' : '';
  const frog = assistant ? '<button data-testid="assistant-open" data-box="300 680 48 48">Assistant</button>' : '';
  return `${roots}<div id="screen">${screen}</div><div id="sheet" hidden></div><div id="chat" hidden>${chat}<button data-testid="chat-close" data-close data-box="300 0 48 48">Close chat</button></div><div id="toast" hidden></div>${frog}`;
}

export function fakeApp(app) {
  const env = pageEnv('', { url: 'https://app/' });
  env.document.documentElement.dataset.bg = 'rgb(255, 255, 255)';
  const seen = { navigations: [], clicks: [], uploads: [], metrics: [], viewports: [], scrollbars: [], media: [], scripts: [], evaluations: 0, requests: 0 };
  const scripts = [];
  let socket = null;
  const emit = (method, params) => socket.hear({ method, params });
  const document = env.document;
  document.elementFromPoint = (x, y) => [...document.querySelectorAll('[data-box]')].reverse().find((node) => node.checkVisibility() && contains(node, x, y)) ?? null;

  function request(url, method = 'GET', status = 200, settle = true) {
    seen.requests += 1;
    const requestId = `R${seen.requests}`;
    emit('Network.requestWillBeSent', { requestId, type: 'Fetch', request: { url: `https://app${url}`, method }, loaderId: 'L1', initiator: { type: 'script' } });
    if (!settle) return;
    setImmediate(() => {
      emit('Network.responseReceived', { requestId, response: { status, url: `https://app${url}` } });
      emit('Network.loadingFinished', { requestId });
    });
  }

  function draw(route) {
    const page = app.pages[route];
    document.getElementById('screen').innerHTML = typeof page === 'function' ? page() : page ?? '<div data-screen="missing"><p>Not found</p></div>';
    const wide = app.widths?.[route];
    if (wide) document.documentElement.dataset.scrollWidth = String(wide);
    else delete document.documentElement.dataset.scrollWidth;
  }

  function screenTo(route) {
    const delay = app.delays?.[route];
    if (delay) {
      document.getElementById('screen').innerHTML = '<div data-screen="loading"><p>Loading</p></div>';
      setTimeout(() => draw(route), delay);
    } else draw(route);
    for (const fetched of app.fetches?.[route] ?? [route]) request(fetched);
    for (const fetched of app.hanging?.[route] ?? []) request(fetched, 'GET', 200, false);
    const moving = app.moving?.[route];
    if (moving) {
      env.animations.push({ playState: 'running', effect: { getComputedTiming: () => ({ endTime: moving }) } });
      setTimeout(() => {
        env.animations.splice(0);
        if (app.after?.[route]) document.getElementById('screen').innerHTML = app.after[route];
      }, moving);
    }
  }

  function load(url) {
    seen.navigations.push(url);
    const route = url.startsWith('https://app/?open=') ? decodeURIComponent(url.slice('https://app/?open='.length)) : '';
    document.body.innerHTML = route ? shellOf('', app.shell?.(route)) : '';
    if (route) screenTo(route);
    window.Alpine = app.noAlpine?.includes(route) ? undefined : {};
    globalThis.PerformanceObserver = observerFor(app.perf?.[route]);
    for (const source of scripts) runInThisContext(source);
    if (!app.stall?.includes(route)) setImmediate(() => emit('Page.loadEventFired', {}));
  }

  function press(node) {
    const target = node.closest(clickable) ?? node;
    seen.clicks.push(target.dataset.testid || target.textContent.trim());
    const handler = app.handlers?.[target.dataset.testid];
    if (handler) return handler({ document, request, screenTo, emit, socket });
    if (target.dataset.nav) return screenTo(target.dataset.nav);
    if (target.dataset.sheet) {
      const sheet = document.getElementById('sheet');
      sheet.innerHTML = app.sheets?.[target.dataset.sheet] ?? '<p>Sheet</p><button data-testid="sheet-close" data-close data-box="300 0 48 48">Close</button>';
      sheet.hidden = false;
      return request(target.dataset.sheet);
    }
    if (target.hasAttribute('data-close')) {
      target.closest('#sheet, #chat').hidden = true;
      return null;
    }
    if (target.dataset.testid === 'assistant-open') {
      document.getElementById('chat').hidden = false;
      return null;
    }
    if (target.dataset.do) {
      const action = JSON.parse(target.dataset.do);
      return request(action.url, action.method, app.statuses?.[action.url] ?? 200);
    }
    return null;
  }

  async function evaluate(expression) {
    seen.evaluations += 1;
    try {
      const value = await runInThisContext(expression);
      return { result: { value: value === undefined ? undefined : JSON.parse(JSON.stringify(value)) } };
    } catch (error) {
      return { exceptionDetails: { text: error.message } };
    }
  }

  const answers = {
    'Target.createTarget': () => ({ targetId: 'T1' }),
    'Target.attachToTarget': () => ({ sessionId: 'S1' }),
    'Page.addScriptToEvaluateOnNewDocument': ({ source }) => {
      seen.scripts.push(source);
      if (!source.includes('frozenClock') && !source.includes('setResourceTimingBufferSize')) scripts.push(source);
      return {};
    },
    'Page.navigate': ({ url }) => {
      load(url);
      return { frameId: 'F1' };
    },
    'Runtime.evaluate': ({ expression }) => evaluate(expression),
    'Input.dispatchMouseEvent': ({ type, x, y }) => {
      const node = document.elementFromPoint(x, y);
      if (type === 'mouseReleased' && node) press(node);
      return {};
    },
    'Page.captureScreenshot': () => ({ data: shotBytes.toString('base64') }),
    'Emulation.setDeviceMetricsOverride': (params) => {
      seen.metrics.push(params.width);
      seen.viewports.push(params);
      env.window.happyDOM.setViewport({ width: params.width, height: params.height });
      return {};
    },
    'Emulation.setScrollbarsHidden': ({ hidden }) => {
      seen.scrollbars.push(hidden);
      return {};
    },
    'Emulation.setEmulatedMedia': (params) => {
      seen.media.push(params.features[0].value);
      return {};
    },
    'DOM.getDocument': () => ({ root: { nodeId: 1 } }),
    'DOM.querySelectorAll': ({ selector }) => ({ nodeIds: [...document.querySelectorAll(selector)].map((_, index) => index + 2) }),
    'DOM.setFileInputFiles': ({ nodeId, files }) => {
      seen.uploads.push(files.length);
      if (app.badUpload === nodeId) throw new Error('file input is gone');
      app.onUpload?.({ document });
      return {};
    },
  };

  const Socket = socketClass((message, current) => {
    socket = current;
    if (app.refuse?.includes(message.method)) throw new Error('refused by the test');
    const answer = answers[message.method];
    return answer ? answer(message.params) : {};
  });
  return { env, seen, Socket, document, request, close: () => env.close() };
}

function observerFor(perf = {}) {
  return class {
    constructor(callback) {
      this.callback = callback;
    }

    observe({ type }) {
      const entries = type === 'layout-shift' ? [{ value: perf.shift ?? 0, hadRecentInput: false }] : Array.from({ length: perf.longTasks ?? 0 }, () => ({ duration: 250 }));
      setTimeout(() => this.callback({ getEntries: () => entries }), perf.after ?? 50);
    }
  };
}

export function configFor(out, appMap, env = {}) {
  return configure({ E2E_BASE: 'https://app', E2E_APPMAP: 'map.json', E2E_OUT: out, E2E_CHROMIUM: 'chrome', E2E_TYPED: 'typed text', ...env }, () => JSON.stringify(appMap));
}

export async function pumped(t, work, turns = 100_000) {
  t.mock.timers.enable({ apis: ['setTimeout', 'Date'], now: 1_000_000 });
  let done = false;
  const running = work().finally(() => {
    done = true;
  });
  let turn = 0;
  while (!done) {
    if (turn === turns) throw new Error(`the crawl did not finish in ${turns} turns`);
    turn += 1;
    await later();
    t.mock.timers.tick(5);
  }
  return running;
}

export { fakeLaunch };
