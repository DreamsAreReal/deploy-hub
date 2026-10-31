import { spawn } from 'node:child_process';
import { join } from 'node:path';
import { chromeFlags, limits, viewport } from './config.mjs';
import { call, connect, endpointOf, within } from './cdp.mjs';
import { listen } from './network.mjs';
import { checks, frozenClock, helpers } from './page.mjs';

export async function start(crawl, launch = spawn, Socket = WebSocket) {
  const origin = new URL(crawl.config.base).origin;
  const netLog = crawl.config.netLog ? [`--log-net-log=${crawl.config.netLog}`, '--net-log-capture-mode=Default'] : [];
  const secure = `--unsafely-treat-insecure-origin-as-secure=${origin}`;
  const chrome = launch(crawl.config.chromium, [...chromeFlags, secure, ...netLog, `--user-data-dir=${join(crawl.out, 'profile')}`, 'about:blank'], { stdio: ['ignore', 'ignore', 'pipe'] });
  try {
    crawl.cdp = await connect(await within(endpointOf(chrome), limits.hang, 'chrome start'), Socket);
    await call(crawl, 'Browser.setDownloadBehavior', { behavior: 'deny' });
    for (const name of ['notifications', 'microphone']) await call(crawl, 'Browser.setPermission', { permission: { name }, setting: 'denied', origin });
    const { targetId } = await call(crawl, 'Target.createTarget', { url: 'about:blank' });
    crawl.cdp.session = (await call(crawl, 'Target.attachToTarget', { targetId, flatten: true })).sessionId;
    listen(crawl);
    for (const domain of ['Page', 'Runtime', 'Network', 'Log']) await call(crawl, `${domain}.enable`);
    await call(crawl, 'Emulation.setDeviceMetricsOverride', viewport);
    if (Number.isFinite(crawl.config.frozenAt)) await call(crawl, 'Page.addScriptToEvaluateOnNewDocument', { source: `(${frozenClock})(${crawl.config.frozenAt})` });
    await call(crawl, 'Page.addScriptToEvaluateOnNewDocument', { source: `(${helpers})()` });
    await call(crawl, 'Page.addScriptToEvaluateOnNewDocument', { source: `(${checks})()` });
    await call(crawl, 'Page.addScriptToEvaluateOnNewDocument', { source: `performance.setResourceTimingBufferSize(${limits.timings})` });
    return chrome;
  } catch (error) {
    chrome.kill();
    throw error;
  }
}
