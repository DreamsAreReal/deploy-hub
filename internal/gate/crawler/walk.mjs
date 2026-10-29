import { join } from 'node:path';
import { attached, deepLink, limits, sizes } from './config.mjs';
import { start } from './browser.mjs';
import { alive, call, goto, inPage, tap } from './cdp.mjs';
import { stuck } from './network.mjs';
import { firstLine, nameOf, newCrawl, note, problem, report, serverErrors, tell } from './record.mjs';
import { discover, guessMissing, keyOf, schedule, seen, shape, skipped, widen } from './routes.mjs';
import { markAssistant } from './page.mjs';
import { compareShots, screenshot } from './visual.mjs';

const halves = 2;
const readable = 3;
const titleSpread = 8;
const shotDigits = 3;
const sampled = 3;
const heavyScreen = 3000;
const shift = { problem: 0.25, note: 0.1 };

const sleep = (ms) => new Promise((done) => setTimeout(done, ms));

async function still(crawl) {
  const until = Date.now() + limits.still;
  while (Date.now() < until && (await inPage(crawl, () => window.crawl.still())) === false) await sleep(limits.poll);
}

async function settle(crawl) {
  const started = Date.now();
  let quiet = Date.now();
  while (Date.now() - started < limits.hang) {
    if (crawl.pending.size > 0) quiet = Date.now();
    else if (Date.now() - quiet >= limits.settle) return still(crawl);
    await sleep(limits.poll / halves);
  }
  await stuck(crawl);
}

async function shown(crawl, route) {
  const id = crawl.config.screens.get(shape(route))?.id;
  const until = Date.now() + limits.hang;
  let there = false;
  while (!there && Date.now() < until) {
    there = (await inPage(crawl, () => window.crawl?.screen())) === id;
    if (!there) await sleep(limits.poll);
  }
  return there;
}

async function arrive(crawl, route, passed = new Set()) {
  const via = crawl.found.get(route);
  passed.add(route);
  if (deepLink.test(route) || !via || passed.has(via)) return goto(crawl, `${crawl.config.base}/?open=${encodeURIComponent(route)}`).catch((error) => problem(crawl, 'hang', firstLine(error)));
  await arrive(crawl, via, passed);
  await shown(crawl, via);
  await settle(crawl);
  await tap(crawl, `[data-nav="${route}"]`, route);
}

async function open(crawl, route) {
  crawl.steps = [`open ${route}`];
  crawl.pending.clear();
  crawl.opened = [];
  crawl.actionAt = Date.now();
  const started = Date.now();
  await arrive(crawl, route);
  const ok = await shown(crawl, route);
  await inPage(crawl, () => window.checks.restart());
  await settle(crawl);
  return { ok, ms: Date.now() - started };
}

async function click(crawl, target) {
  crawl.steps.push(`click ${nameOf(target)}`);
  const marked = await inPage(crawl, (wanted) => window.crawl.mark(wanted), target);
  const pressable = Boolean(marked) && !skipped(crawl, marked);
  if (pressable) await pressMarked(crawl, target, marked);
  else crawl.log.write(`  ${marked ? 'unsafe now' : 'gone'}: ${nameOf(target)}\n`);
  return pressable;
}

async function pressMarked(crawl, target, marked) {
  crawl.clicks += 1;
  const before = await inPage(crawl, () => window.checks.signature());
  const sent = crawl.requests;
  const tapped = await tap(crawl, '[data-crawl]', nameOf(target));
  await settle(crawl);
  if (tapped && sent === crawl.requests && !marked.quiet) await deadCheck(crawl, target, before);
  if (marked.menu) await menuKept(crawl, marked);
  const alert = await inPage(crawl, () => window.crawl.alert());
  if (alert) note(crawl, 'alert', alert);
}

async function deadCheck(crawl, target, before) {
  const after = await inPage(crawl, () => window.checks.signature());
  if (before !== null && before === after) problem(crawl, 'dead-button', `${nameOf(target)} changes nothing and sends nothing`);
}

async function menuKept(crawl, target) {
  const kept = await inPage(crawl, (id) => document.querySelector(`[data-testid="${id}"]`)?.checkVisibility() ?? false, target.menu);
  if (kept === false) problem(crawl, 'lost-menu', `${nameOf(target)} leads to a screen without the ${target.menu} menu`);
}

const layoutKinds = [
  ['mixed-alphabet', 'alphabets', problem],
  ['broken-image', 'images', problem],
  ['text-overlap', 'overlap', problem],
  ['silent-empty', 'silent', note],
  ['too-wide', 'wide', problem],
  ['cut-text', 'cut', problem],
  ['off-screen', 'offscreen', problem],
  ['tech-text', 'garbage', problem],
  ['small-target', 'targets', note],
  ['tiny-target', 'tiny', problem],
  ['small-font', 'fonts', note],
  ['near-alignment', 'edges', problem],
  ['uneven-row', 'rows', problem],
  ['cramped', 'cramped', problem],
  ['cut-placeholder', 'placeholders', problem],
  ['truncated', 'truncated', problem],
  ['narrow-field', 'narrow', problem],
];

const senseKinds = [
  ['duplicate-button', 'twins', problem],
  ['ambiguous-label', 'ambiguous', problem],
  ['delete-without-confirm', 'rash', problem],
  ['loud-danger', 'loud', problem],
  ['heavy-list', 'heavy', problem],
  ['long-list', 'long', problem],
  ['listed-twice', 'twice', problem],
  ['jumping-button', 'jumps', problem],
  ['no-microphone', 'voiceless', problem],
];

const checked = (crawl, name) => inPage(crawl, (check) => window.checks[check](), name).then((found) => found ?? []);

async function lookOver(crawl, where) {
  for (const [kind, name, record] of layoutKinds) tell(crawl, kind, where, await checked(crawl, name), record);
  const low = await checked(crawl, 'contrast');
  tell(crawl, 'unreadable', where, low.filter((entry) => entry.ratio < readable).map((entry) => `${entry.what} ${entry.ratio}:1`), problem);
  tell(crawl, 'low-contrast', where, low.filter((entry) => entry.ratio >= readable).map((entry) => `${entry.what} ${entry.ratio}:1`), note);
}

function remember(crawl, entry, where) {
  const words = crawl.words.get(entry.key) ?? new Map();
  if (!words.has(entry.word)) words.set(entry.word, where);
  crawl.words.set(entry.key, words);
}

async function screenSense(crawl, route) {
  const primaries = await checked(crawl, 'primaries');
  if (primaries.length > 1 && crawl.config.screens.get(shape(route))?.id !== 'design') problem(crawl, 'many-primary', `${route}: ${primaries.join('; ')}`);
  const exits = await inPage(crawl, () => window.checks.exits());
  if (exits && !exits.back) problem(crawl, 'dead-end', `${route} has no way back`);
  const gap = await inPage(crawl, () => window.checks.titleGap());
  if (typeof gap === 'number') crawl.gaps.set(route, gap);
}

async function makeSense(crawl, route, overlay) {
  const where = overlay ? `${route} (${overlay})` : route;
  for (const [kind, name, record] of senseKinds) tell(crawl, kind, where, await checked(crawl, name), record);
  for (const entry of await checked(crawl, 'words')) remember(crawl, entry, where);
  if (!overlay) return screenSense(crawl, route);
  const exits = await inPage(crawl, () => window.checks.exits());
  if (overlay.startsWith('sheet') && exits && !exits.close) note(crawl, 'no-close', `${where} has no close button`);
}

function titleGaps(crawl) {
  const gaps = [...crawl.gaps].sort((one, two) => one[1] - two[1]);
  const [low, high] = [gaps[0], gaps.at(-1)];
  if (high && high[1] - low[1] > titleSpread) problem(crawl, 'uneven-title-gap', `${low[1]} px under the title on ${low[0]}, ${high[1]} px on ${high[0]}`);
}

function mixedWords(crawl) {
  crawl.steps = ['after the crawl'];
  for (const words of crawl.words.values()) {
    if (words.size > 1) problem(crawl, 'mixed-words', [...words].map(([word, where]) => `«${word}» on ${where}`).join(', '));
  }
}

async function present(crawl, size) {
  await call(crawl, 'Emulation.setDeviceMetricsOverride', { width: size.width, height: size.height, deviceScaleFactor: 1, mobile: size.mobile });
  await call(crawl, 'Emulation.setEmulatedMedia', { features: [{ name: 'prefers-color-scheme', value: size.dark ? 'dark' : 'light' }] });
}

async function lookAt(crawl, route, size) {
  const opened = await open(crawl, route);
  crawl.steps.push(`at ${size.name}`);
  if (!opened.ok) return;
  const before = crawl.problems.length;
  await lookOver(crawl, `${route} @${size.name}`);
  await screenshot(crawl, join('screens', `${crawl.config.screens.get(shape(route)).id}-${size.name}.png`));
  await shoot(crawl, before);
}

async function sizesPass(crawl) {
  for (const size of sizes) {
    await present(crawl, size);
    for (const route of crawl.visited.values()) if (alive(crawl)) await lookAt(crawl, route, size);
    if (alive(crawl)) await chatAt(crawl, size);
  }
}

async function narrowPass(crawl) {
  await chatAt(crawl, { name: 'default' });
  const narrowest = sizes[0];
  await present(crawl, narrowest);
  for (const route of crawl.visited.values()) {
    if (!alive(crawl)) return;
    const opened = await open(crawl, route);
    crawl.steps.push(`at ${narrowest.name}`);
    const before = crawl.problems.length;
    if (opened.ok) await lookOver(crawl, `${route} @${narrowest.name}`);
    await shoot(crawl, before);
  }
  if (alive(crawl)) await chatAt(crawl, narrowest);
}

async function chatAt(crawl, size) {
  const route = crawl.visited.values().next().value;
  if (!route || !(await open(crawl, route)).ok) return;
  const frog = await inPage(crawl, markAssistant);
  if (!frog) return;
  crawl.steps.push('open the assistant chat');
  await tap(crawl, '[data-crawl]', 'assistant-open');
  await settle(crawl);
  const before = crawl.problems.length;
  if ((await inPage(crawl, () => window.crawl.overlay())) === 'chat') await lookOver(crawl, `the assistant chat on ${route} @${size.name}`);
  await shoot(crawl, before);
}

async function reach(crawl, route, path) {
  const opened = await open(crawl, route);
  await inPage(crawl, (value) => window.crawl.fill(value), crawl.config.typed);
  for (const opener of path) {
    if (!(await click(crawl, opener))) return false;
    await inPage(crawl, (value) => window.crawl.fill(value), crawl.config.typed);
  }
  return opened.ok;
}

async function shoot(crawl, from) {
  const fresh = crawl.problems.slice(from).filter((entry) => !entry.shot);
  if (fresh.length === 0) return;
  const shot = `shots/${String(crawl.problems.length).padStart(shotDigits, '0')}.png`;
  await screenshot(crawl, shot);
  for (const entry of fresh) entry.shot = shot;
}

async function press(crawl, route, path, target) {
  const key = keyOf(route, path, target);
  if (skipped(crawl, target) || crawl.seen.has(key)) return;
  crawl.seen.add(key);
  const before = crawl.problems.length;
  const clicked = (await reach(crawl, route, path)) && (await click(crawl, target));
  await shoot(crawl, before);
  if (!clicked) return;
  discover(crawl, route, (await inPage(crawl, () => window.crawl.routes())) ?? []);
  if (path.length === 0) await deeper(crawl, route, target);
}

async function deeper(crawl, route, opener) {
  const overlay = await inPage(crawl, () => window.crawl.overlay());
  if (!overlay) return;
  const where = `${route} (${overlay} of ${nameOf(opener)})`;
  await lookOver(crawl, where);
  await makeSense(crawl, route, `${overlay} of ${nameOf(opener)}`);
  await upload(crawl).catch((error) => note(crawl, 'upload', firstLine(error)));
  discover(crawl, route, (await inPage(crawl, () => window.crawl.routes())) ?? []);
  await inPage(crawl, (value) => window.crawl.fill(value), crawl.config.typed);
  for (const target of (await inPage(crawl, () => window.crawl.targets())) ?? []) await press(crawl, route, [opener], target);
}

async function upload(crawl) {
  const { root } = await call(crawl, 'DOM.getDocument', { depth: 0 });
  const { nodeIds } = await call(crawl, 'DOM.querySelectorAll', { nodeId: root.nodeId, selector: 'input[type="file"]:not([webkitdirectory])' });
  for (const nodeId of nodeIds) {
    crawl.steps.push(`upload ${attached.name}`);
    await call(crawl, 'DOM.setFileInputFiles', { nodeId, files: [crawl.file] }).catch((error) => note(crawl, 'upload', firstLine(error)));
    await settle(crawl);
  }
}

function repeated(crawl, route) {
  const twice = crawl.opened.filter((url, index) => crawl.opened.indexOf(url) !== index && !url.includes('/assets/'));
  if (twice.length > 0) note(crawl, 'repeated-request', `${route}: ${[...new Set(twice)].slice(0, sampled).join(', ')}`);
}

async function felt(crawl, route) {
  const seen = await inPage(crawl, () => window.checks.measured());
  if (!seen) return;
  if (seen.shift > shift.problem) problem(crawl, 'layout-shift', `${route} moves by ${seen.shift} while it loads`);
  else if (seen.shift > shift.note) note(crawl, 'layout-shift', `${route} moves by ${seen.shift} while it loads`);
  if (seen.slow > 0) note(crawl, 'long-task', `${route}: ${seen.slow} scripts ran longer than 200 ms`);
  if (seen.weight > heavyScreen) note(crawl, 'heavy-screen', `${route}: ${seen.weight} elements`);
}

async function checkScreen(crawl, route, screen) {
  const opened = await open(crawl, route);
  const before = crawl.problems.length;
  repeated(crawl, route);
  await felt(crawl, route);
  if ((await inPage(crawl, () => window.crawl.started())) === false) problem(crawl, 'no-alpine', `${route}: Alpine did not start, buttons do nothing`);
  if (!opened.ok) problem(crawl, 'wrong-screen', `${route} showed «${await inPage(crawl, () => window.crawl.screen())}» instead of «${screen.id}»`);
  else if (opened.ms > limits.slow) problem(crawl, 'slow', `${route} took ${opened.ms} ms to show`);
  if (opened.ok && !(await inPage(crawl, () => window.crawl.filled()))) problem(crawl, 'empty-screen', route);
  await screenshot(crawl, join('screens', `${screen.id}.png`));
  await shoot(crawl, before);
  return opened.ok;
}

async function visit(crawl, route) {
  const screen = crawl.config.screens.get(shape(route));
  crawl.log.write(`screen ${screen.id} ${route}\n`);
  const ok = await checkScreen(crawl, route, screen);
  seen(crawl, route, (await inPage(crawl, () => [...document.querySelectorAll('#screen [data-testid]')].map((node) => node.dataset.testid))) ?? []);
  if (ok) await makeSense(crawl, route, '');
  discover(crawl, route, (await inPage(crawl, () => window.crawl.routes())) ?? [], true);
  if (ok && !crawl.config.quick) await upload(crawl).catch((error) => note(crawl, 'upload', firstLine(error)));
  discover(crawl, route, (await inPage(crawl, () => window.crawl.routes())) ?? []);
  if (!ok) return;
  if (crawl.config.quick) return lookOver(crawl, route);
  await inPage(crawl, (value) => window.crawl.fill(value), crawl.config.typed);
  for (const target of (await inPage(crawl, () => window.crawl.targets())) ?? []) await press(crawl, route, [], target);
}

function unreached(crawl) {
  crawl.steps = ['after the crawl'];
  if (!alive(crawl)) problem(crawl, 'browser-crashed', 'Chrome closed its DevTools connection during the crawl');
  for (const [key, screen] of crawl.config.screens) if (!crawl.visited.has(key)) note(crawl, 'unreached', `${screen.route} (${screen.id})`);
}

async function crawlAll(crawl) {
  for (const screen of crawl.config.screens.values()) if (!screen.route.includes('{')) schedule(crawl, screen.route);
  while (crawl.queue.length > 0 && alive(crawl)) {
    await visit(crawl, crawl.queue.shift());
    if (crawl.queue.length === 0) guessMissing(crawl);
    if (crawl.queue.length === 0) widen(crawl);
  }
  unreached(crawl);
}

export async function run(config, launch, Socket) {
  const crawl = newCrawl(config);
  const chrome = await start(crawl, launch, Socket);
  try {
    await crawlAll(crawl);
    await (config.quick ? narrowPass(crawl) : sizesPass(crawl));
    mixedWords(crawl);
    titleGaps(crawl);
    await compareShots(crawl);
  } finally {
    chrome.kill();
  }
  serverErrors(crawl, config.serverLog);
  const summary = report(crawl);
  crawl.log.end();
  return { crawl, summary };
}
