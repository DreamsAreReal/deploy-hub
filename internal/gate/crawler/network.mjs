import { limits, longLived } from './config.mjs';
import { call, inPage } from './cdp.mjs';
import { answered } from './page.mjs';
import { firstLine, note, problem, sentBy } from './record.mjs';

const serverError = 500;
const clientError = 400;

function consoleCall(crawl, event) {
  if (event.type === 'error') problem(crawl, 'console-error', event.args.map((arg) => arg.value ?? arg.description ?? '').join(' '));
}

function logEntry(crawl, entry) {
  if (entry.level !== 'error' || ['network', 'javascript'].includes(entry.source)) return;
  const told = `${entry.text} ${entry.url ?? ''}`.trim();
  if (['intervention', 'violation'].includes(entry.source)) note(crawl, entry.source, told);
  else problem(crawl, 'console-error', told);
}

function initiatorOf(event) {
  const frame = event.initiator?.stack?.callFrames?.[0];
  const where = frame ? ` ${frame.functionName || 'anonymous'} ${frame.url.split('/').pop()}:${frame.lineNumber + 1}` : '';
  return `${event.type ?? 'Other'} by ${event.initiator?.type ?? 'other'}${where}`;
}

function requested(crawl, event) {
  if (!/^https?:/.test(event.request.url) || longLived.has(event.type)) return;
  crawl.requests += 1;
  crawl.opened.push(event.request.url);
  crawl.pending.set(event.requestId, {
    url: event.request.url, method: event.request.method, at: Date.now(), caused: Date.now() - crawl.actionAt <= limits.caused, by: initiatorOf(event),
    loader: event.loaderId,
  });
}

function late(crawl, requestId, what) {
  const sent = crawl.hung.get(requestId);
  if (!sent) return;
  crawl.hung.delete(requestId);
  note(crawl, 'late-answer', `${sent.method} ${sent.url} ${what} after ${Date.now() - sent.at} ms`);
}

function responded(crawl, event) {
  const status = event.response.status;
  const what = `${crawl.pending.get(event.requestId)?.method ?? 'GET'} ${event.response.url} → ${status}`;
  if (status >= serverError) problem(crawl, `http-${status}`, what);
  else if (status >= clientError) note(crawl, `http-${status}`, what);
  finished(crawl, event.requestId);
}

function finished(crawl, requestId) {
  late(crawl, requestId, 'answered');
  const sent = crawl.pending.get(requestId);
  crawl.pending.delete(requestId);
  if (!sent || Date.now() - sent.at <= limits.slow) return;
  (sent.caused ? problem : note)(crawl, sent.caused ? 'slow' : 'slow-background', `${sent.method} ${sent.url} took ${Date.now() - sent.at} ms`);
}

function failed(crawl, event) {
  late(crawl, event.requestId, event.canceled ? 'was cancelled' : `failed: ${event.errorText}`);
  const sent = crawl.pending.get(event.requestId);
  crawl.pending.delete(event.requestId);
  if (sent && !event.canceled) problem(crawl, 'request-failed', `${sent.method} ${sent.url} ${event.errorText} ${event.blockedReason ?? ''}`.trim());
}

export function listen(crawl) {
  const on = (method, handle) => crawl.cdp.handlers.set(method, handle);
  on('Runtime.exceptionThrown', ({ exceptionDetails }) => problem(crawl, 'js-error', firstLine(exceptionDetails.exception?.description ?? exceptionDetails.text)));
  on('Runtime.consoleAPICalled', (event) => consoleCall(crawl, event));
  on('Log.entryAdded', ({ entry }) => logEntry(crawl, entry));
  on('Page.javascriptDialogOpening', () => call(crawl, 'Page.handleJavaScriptDialog', { accept: false }).catch(() => null));
  on('Page.loadEventFired', () => crawl.loaded?.());
  on('Page.frameNavigated', ({ frame }) => {
    if (!frame.parentId) crawl.loader = frame.loaderId;
  });
  on('Network.requestWillBeSent', (event) => requested(crawl, event));
  on('Network.responseReceived', (event) => responded(crawl, event));
  on('Network.loadingFinished', ({ requestId }) => finished(crawl, requestId));
  on('Network.loadingFailed', (event) => failed(crawl, event));
}

const finishedInPage = (crawl, sent) => inPage(crawl, answered, { url: sent.url, at: sent.at, near: limits.caused });

export async function stuck(crawl) {
  const waiting = [];
  for (const [requestId, sent] of crawl.pending) {
    if (sent.loader && crawl.loader && sent.loader !== crawl.loader) {
      note(crawl, 'left-with-its-page', `${sent.method} ${sent.url} was sent by a page that has since been replaced`);
      continue;
    }
    if (await finishedInPage(crawl, sent)) {
      note(crawl, 'lost-network-event', `${sent.method} ${sent.url} was answered in the page, Chrome sent no end of it`);
      continue;
    }
    crawl.hung.set(requestId, sent);
    waiting.push(sent);
  }
  const told = (list) => `no answer in ${limits.hang} ms: ${list.map(sentBy).join(', ')}`;
  const caused = waiting.filter((sent) => sent.caused);
  const background = waiting.filter((sent) => !sent.caused);
  if (caused.length > 0) problem(crawl, 'hang', told(caused));
  if (background.length > 0) note(crawl, 'background-hang', told(background));
  crawl.pending.clear();
}
