import { readFileSync } from 'node:fs';
import { shape, testidRe } from './routes.mjs';

export const limits = { settle: 300, slow: 3000, hang: 10000, caused: 1500, still: 2000, detail: 300, poll: 100, timings: 100000 };
export const viewport = { width: 420, height: 860, deviceScaleFactor: 1, mobile: true };
export const sizes = [
  { name: '360', width: 360, height: 740, mobile: true },
  { name: '390', width: 390, height: 844, mobile: true },
  { name: '430', width: 430, height: 932, mobile: true },
  { name: '768', width: 768, height: 1024, mobile: false },
  { name: '1280', width: 1280, height: 800, mobile: false },
  { name: '390-dark', width: 390, height: 844, mobile: true, dark: true },
];
export const attached = { name: 'crawl-sample.md', body: '# Crawl\n\nA file the crawler attaches wherever a file input is.\n' };
export const deepLink = /^\/ui\/[a-z0-9-]+(\/[a-z0-9-]+)*$/;
export const longLived = new Set(['EventSource', 'WebSocket']);
export const chromeFlags = ['--headless=new', '--no-sandbox', '--disable-gpu', '--remote-debugging-port=0', '--block-new-web-contents', '--no-first-run',
  '--no-default-browser-check', '--mute-audio', '--disable-background-networking', '--disable-component-update'];
export const visual = { tolerance: 24, share: 0.002, least: 40 };

const defaultTyped = 'Crawl <b>&amp;</b> «quotes» 🐸';
const destructive = /Destructive|never click/;

const readText = (file) => readFileSync(file, 'utf8');

export function configure(env, read = readText) {
  const appMap = env.E2E_APPMAP ? JSON.parse(read(env.E2E_APPMAP)) : { screens: [] };
  return {
    ready: Boolean(env.E2E_BASE && env.E2E_APPMAP),
    base: env.E2E_BASE ?? '',
    out: env.E2E_OUT ?? '',
    quick: env.CRAWL_MODE === 'quick',
    chromium: env.E2E_CHROMIUM ?? '',
    netLog: env.E2E_NETLOG ?? '',
    serverLog: env.E2E_SERVER_LOG ?? '',
    typed: env.E2E_TYPED || defaultTyped,
    frozenAt: Date.parse(env.E2E_FROZEN_AT ?? ''),
    baseline: env.E2E_BASELINE ?? '',
    accepted: new Set((env.E2E_VISUAL_ACCEPT ?? '').split(',').map((id) => id.trim()).filter(Boolean)),
    screens: new Map(appMap.screens.filter((screen) => screen.kind === 'screen').map((screen) => [shape(screen.route), screen])),
    forbidden: appMap.screens
      .flatMap((screen) => screen.elements ?? [])
      .filter((element) => destructive.test(element.does))
      .map((element) => testidRe(element.testid)),
  };
}
