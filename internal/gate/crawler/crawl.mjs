import { test } from 'node:test';
import assert from 'node:assert/strict';
import { spawn } from 'node:child_process';
import { createWriteStream, existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';

const env = process.env;
const base = env.E2E_BASE;
const quick = env.CRAWL_MODE === 'quick';
const limits = { settle: 300, slow: 3000, hang: 10000, caused: 1500, still: 2000, detail: 300, poll: 100, timings: 100000 };
const viewport = { width: 420, height: 860, deviceScaleFactor: 1, mobile: true };
const sizes = [
  { name: '360', width: 360, height: 740, mobile: true },
  { name: '390', width: 390, height: 844, mobile: true },
  { name: '430', width: 430, height: 932, mobile: true },
  { name: '768', width: 768, height: 1024, mobile: false },
  { name: '1280', width: 1280, height: 800, mobile: false },
  { name: '390-dark', width: 390, height: 844, mobile: true, dark: true },
];
const typed = env.E2E_TYPED || 'Crawl <b>&amp;</b> «quotes» 🐸';
const attached = { name: 'crawl-sample.md', body: '# Crawl\n\nA file the crawler attaches wherever a file input is.\n' };
const deepLink = /^\/ui\/[a-z0-9-]+(\/[a-z0-9-]+)*$/;
const longLived = new Set(['EventSource', 'WebSocket']);
const chromeFlags = ['--headless=new', '--no-sandbox', '--disable-gpu', '--remote-debugging-port=0', '--block-new-web-contents', '--no-first-run',
  '--no-default-browser-check', '--mute-audio', '--disable-background-networking', '--disable-component-update'];
const appMap = env.E2E_APPMAP ? JSON.parse(readFileSync(env.E2E_APPMAP, 'utf8')) : { screens: [] };
const sleep = (ms) => new Promise((done) => setTimeout(done, ms));

const placeholder = (part) => part.startsWith('{') || /^\d+$/.test(part);
const shape = (route) => route.split('?')[0].split('/').map((part) => (placeholder(part) ? 'N' : part)).join('/');
const escapeRe = (text) => text.replace(/[.*+?^${}()|[\]\\]/g, String.raw`\$&`);
const testidRe = (testid) => new RegExp(`^${testid.split('-').map((part) => (part.startsWith('{') ? '.+' : escapeRe(part))).join('-')}$`);
const screens = new Map(appMap.screens.filter((screen) => screen.kind === 'screen').map((screen) => [shape(screen.route), screen]));
const forbidden = appMap.screens
  .flatMap((screen) => screen.elements ?? [])
  .filter((element) => /Destructive|never click/.test(element.does))
  .map((element) => testidRe(element.testid));
const skipped = (target) => target.unsafe || forbidden.some((pattern) => pattern.test(target.testid));
const firstLine = (error) => String(error?.message ?? error).split('\n')[0];
const nameOf = (target) => target.testid || `«${target.label}»`;
const cell = (text) => text.replaceAll('|', String.raw`\|`).replaceAll('\n', ' ');

function helpers() {
  const opened = (id) => {
    const node = document.getElementById(id);
    return node && !node.hidden ? node : null;
  };
  const scope = () => opened('sheet') ?? opened('chat') ?? document.body;
  const place = (node) => ['sheet', 'chat', 'screen'].find((id) => node.closest(`#${id}`)) ?? 'shell';
  const label = (node) => (node.getAttribute('aria-label') || node.textContent || '').replace(/\s+/g, ' ').trim().slice(0, 60);
  const foreign = (node) => node.tagName === 'A' && (node.target === '_blank' || node.hasAttribute('download') || new URL(node.href, location.href).origin !== location.origin);
  const danger = (node) => [...node.classList].some((name) => name.endsWith('-danger')) || (node.dataset.do ?? '').includes('"DELETE"');
  const clickable = 'button, a[href], [data-nav], [data-sheet], [data-do], [role="button"], summary';
  const fields = 'input:not([type]), input[type="text"], input[type="search"], input[type="url"], textarea';
  const nodes = () => [...scope().querySelectorAll(clickable)].filter((node) => node.checkVisibility() && !node.disabled);
  const marked = (node) => ['aria-pressed', 'aria-checked', 'aria-selected'].some((name) => node.getAttribute(name) === 'true') || node.hasAttribute('aria-current');
  const selected = (node) => marked(node) || [...node.classList].some((name) => name.endsWith('-current') || name.endsWith('-picked'));
  const sample = (node) => node.closest('[data-screen="design"]') !== null;
  const invalid = (node) => node.type === 'submit' && node.form !== null && !node.form.checkValidity();
  const describe = (node, index) => ({ index, testid: node.dataset.testid ?? '', nav: node.dataset.nav ?? '', sheet: node.dataset.sheet ?? '', act: node.dataset.do ?? '', label: label(node), place: place(node), unsafe: danger(node) || foreign(node), quiet: selected(node) || invalid(node) || sample(node), menu: node.closest('nav')?.dataset.testid ?? '' });
  const loose = (text) => text.replace(/\d+/g, '#');
  const same = (wanted, node) => {
    if (wanted.testid) return node.dataset.testid === wanted.testid;
    if (wanted.nav) return node.dataset.nav === wanted.nav;
    if (wanted.sheet) return node.dataset.sheet === wanted.sheet;
    if (wanted.act) return node.dataset.do === wanted.act;
    return loose(label(node)) === loose(wanted.label);
  };
  const writable = (field) => field.checkVisibility() && !field.disabled && !field.readOnly && !field.value;
  const current = () => document.querySelector('#screen [data-screen]');
  window.crawl = {
    targets: () => nodes().map(describe),
    mark(wanted) {
      for (const node of document.querySelectorAll('[data-crawl]')) node.removeAttribute('data-crawl');
      const all = nodes();
      const at = all.findIndex((node) => same(wanted, node));
      if (at < 0) return null;
      all[at].setAttribute('data-crawl', '');
      return describe(all[at], at);
    },
    fill(value) {
      for (const field of [...scope().querySelectorAll(fields)].filter(writable)) {
        field.value = value;
        field.dispatchEvent(new Event('input', { bubbles: true }));
      }
    },
    screen: () => current()?.dataset.screen ?? '',
    started: () => typeof window.Alpine === 'object',
    filled: () => (current()?.innerText.trim().length ?? 0) > 0,
    routes: () => [...document.querySelectorAll('[data-nav]')].map((node) => node.dataset.nav),
    overlay: () => (opened('sheet') && 'sheet') || (opened('chat') && 'chat') || '',
    still: () => document.getAnimations().every((animation) => animation.playState !== 'running' || animation.effect?.getComputedTiming().endTime === Infinity),
    alert: () => (opened('toast')?.dataset.kind === 'alert' ? opened('toast').textContent.trim() : ''),
    area: () => opened('sheet') ?? opened('chat') ?? document.getElementById('screen'),
    nodes,
    label,
    loose,
    clickable,
  };
}

function checks() {
  const perf = { shift: 0, slow: 0, watching: true };
  try {
    new PerformanceObserver((list) => {
      for (const entry of list.getEntries()) if (!entry.hadRecentInput) perf.shift += entry.value;
    }).observe({ type: 'layout-shift', buffered: true });
    new PerformanceObserver((list) => {
      perf.slow += list.getEntries().filter((entry) => entry.duration > 200).length;
    }).observe({ type: 'longtask', buffered: true });
  } catch {
    perf.watching = false;
  }
  const tools = () => window.crawl;
  const shown = (node) => node.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true });
  const own = (node) => [...node.childNodes].filter((child) => child.nodeType === Node.TEXT_NODE).map((child) => child.textContent).join('').trim();
  const named = (node) => [node.tagName.toLowerCase(), node.dataset.testid ? `[${node.dataset.testid}]` : '', tools().label(node) ? ` «${tools().label(node).slice(0, 30)}»` : ''].join('');
  const act = (node) => node.dataset.nav || node.dataset.sheet || node.dataset.do || node.getAttribute('@click') || '';
  const doShape = (value) => {
    try {
      const action = JSON.parse(value);
      return tools().loose(`${action.method} ${action.url} ${JSON.stringify(action.body ?? {})}`);
    } catch {
      return tools().loose(value);
    }
  };
  const shapeOf = (value) => {
    if (value.startsWith('{')) return doShape(value);
    const text = tools().loose(value);
    const called = /^([\w$.]+)\(\s*('[^']*')?/.exec(text);
    return called ? `${called[1]}(${called[2] ?? ''})` : text;
  };
  const lettered = () => [...tools().area().querySelectorAll('*')].filter((node) => own(node) !== '' && shown(node) && !node.closest('.md, :disabled, [aria-disabled="true"]'));
  const channels = (value) => {
    const numbers = (value.match(/[\d.]+/g) ?? []).map(Number);
    if (numbers.length < 3) return null;
    const scale = value.startsWith('color(') ? 255 : 1;
    return [numbers[0] * scale, numbers[1] * scale, numbers[2] * scale, numbers[3] ?? 1];
  };
  const over = (top, under) => {
    const alpha = top[3] + under[3] * (1 - top[3]);
    const mix = (index) => (alpha === 0 ? 0 : (top[index] * top[3] + under[index] * under[3] * (1 - top[3])) / alpha);
    return [mix(0), mix(1), mix(2), alpha];
  };
  const page = () => {
    const probe = document.createElement('div');
    probe.style.background = 'var(--bg)';
    document.body.append(probe);
    const color = channels(getComputedStyle(probe).backgroundColor) ?? [255, 255, 255, 1];
    probe.remove();
    return color;
  };
  const backdrop = (node) => {
    const layers = [];
    for (let up = node; up; up = up.parentElement) {
      if (getComputedStyle(up).backgroundImage !== 'none') return null;
      const color = channels(getComputedStyle(up).backgroundColor);
      if (color && color[3] > 0) layers.push(color);
      if (color && color[3] >= 1) return layers.reverse().reduce((under, top) => over(top, under));
    }
    return layers.reverse().reduce((under, top) => over(top, under), page());
  };
  const linear = (value) => {
    const share = value / 255;
    return share <= 0.03928 ? share / 12.92 : ((share + 0.055) / 1.055) ** 2.4;
  };
  const luminance = (color) => 0.2126 * linear(color[0]) + 0.7152 * linear(color[1]) + 0.0722 * linear(color[2]);
  const ratio = (one, two) => {
    const [light, dark] = [luminance(one), luminance(two)].sort((a, b) => b - a);
    return (light + 0.05) / (dark + 0.05);
  };
  const needed = (style) => (parseFloat(style.fontSize) >= 24 || (parseFloat(style.fontSize) >= 18.66 && Number(style.fontWeight) >= 700) ? 3 : 4.5);
  const legible = (node) => {
    const style = getComputedStyle(node);
    const ground = backdrop(node);
    if (!ground) return { what: named(node), ratio: Infinity, need: 0 };
    const value = ratio(over(channels(style.color) ?? [0, 0, 0, 1], ground), ground);
    return { what: named(node), ratio: Math.round(value * 100) / 100, need: needed(style) };
  };
  const ellipsed = (node) => getComputedStyle(node).textOverflow === 'ellipsis' || getComputedStyle(node).webkitLineClamp !== 'none';
  const clipper = (node) => {
    for (let up = node.parentElement; up; up = up.parentElement) {
      const overflow = getComputedStyle(up).overflowX;
      if (overflow === 'auto' || overflow === 'scroll') return null;
      if (overflow !== 'visible') return up;
    }
    return null;
  };
  const spills = (node) => {
    const edge = clipper(node);
    if (!edge || ellipsed(edge)) return false;
    const box = node.getBoundingClientRect();
    const bounds = edge.getBoundingClientRect();
    return box.right > bounds.right + 1 || box.left < bounds.left - 1;
  };
  const outgrows = (node) => {
    const style = getComputedStyle(node);
    if (style.display === 'inline' || style.overflowX !== 'visible') return false;
    const box = node.getBoundingClientRect();
    const range = document.createRange();
    return [...node.childNodes].filter((child) => child.nodeType === Node.TEXT_NODE && child.textContent.trim()).some((child) => {
      range.selectNodeContents(child);
      const text = range.getBoundingClientRect();
      return text.right > box.right + 1 || text.left < box.left - 1;
    });
  };
  const cut = (node) => !ellipsed(node) && ((getComputedStyle(node).overflowX !== 'visible' && node.scrollWidth > node.clientWidth + 1) || spills(node) || outgrows(node));
  const scrolled = (node) => {
    for (let up = node.parentElement; up; up = up.parentElement) {
      const overflow = getComputedStyle(up).overflowX;
      if (overflow === 'auto' || overflow === 'scroll') return true;
    }
    return false;
  };
  const inView = (box) => box.width > 0 && box.height > 0 && box.top < window.innerHeight && box.bottom > 0;
  const crosses = (one, two) => {
    if (one.node.contains(two.node) || two.node.contains(one.node)) return false;
    const width = Math.min(one.box.right, two.box.right) - Math.max(one.box.left, two.box.left);
    const height = Math.min(one.box.bottom, two.box.bottom) - Math.max(one.box.top, two.box.top);
    return width > 4 && height > 4;
  };
  const overlapsOf = (boxes, index) => boxes.slice(index + 1).filter((other) => crosses(boxes[index], other)).map((other) => `${named(boxes[index].node)} over ${named(other.node)}`);
  const pressable = (node) => node.matches(tools().clickable);
  const reddish = (node) => {
    const [red, green, blue, alpha] = channels(getComputedStyle(node).backgroundColor) ?? [0, 0, 0, 0];
    return alpha > 0.5 && red > 150 && red > green * 1.6 && red > blue * 1.6;
  };
  const boxed = (style) => ['Top', 'Right', 'Bottom', 'Left'].every((edge) => parseFloat(style[`border${edge}Width`]) > 0 && (channels(style[`border${edge}Color`])?.[3] ?? 0) > 0);
  const framed = (node) => {
    const style = getComputedStyle(node);
    return boxed(style) || (channels(style.backgroundColor)?.[3] ?? 0) > 0;
  };
  const textLeft = (node) => {
    const text = [...node.childNodes].find((child) => child.nodeType === Node.TEXT_NODE && child.textContent.trim() !== '');
    const range = document.createRange();
    range.selectNodeContents(text);
    return range.getClientRects()[0]?.left ?? null;
  };
  const fieldLeft = (field) => {
    const style = getComputedStyle(field);
    return field.getBoundingClientRect().left + parseFloat(style.borderLeftWidth) + parseFloat(style.paddingLeft);
  };
  const leading = (node) => getComputedStyle(node).display !== 'inline' && ['left', 'start'].includes(getComputedStyle(node).textAlign);
  const typeable = 'input:not([type="checkbox"]):not([type="radio"]):not([type="file"]):not([type="range"]), textarea, select';
  const columnsOf = () => {
    const edge = tools().area().getBoundingClientRect().left;
    const lefts = new Map();
    const add = (x, node) => {
      if (x === null || x - edge > 64) return;
      lefts.set(Math.round(x), [...(lefts.get(Math.round(x)) ?? []), named(node)]);
    };
    for (const node of lettered().filter(leading)) add(textLeft(node), node);
    for (const field of [...tools().area().querySelectorAll(typeable)].filter(shown)) add(fieldLeft(field), field);
    return [...lefts].filter(([, names]) => names.length > 1).sort((one, two) => one[0] - two[0]);
  };
  const centerOf = (node) => {
    const box = node.getBoundingClientRect();
    return box.top + box.height / 2;
  };
  const inRow = (row) => {
    const style = getComputedStyle(row);
    return style.display.endsWith('flex') && style.flexDirection.startsWith('row') && style.flexWrap === 'nowrap';
  };
  const uneven = (row) => {
    const centers = [...row.children].filter((child) => shown(child) && pressable(child)).map(centerOf);
    return centers.length > 1 && Math.max(...centers) - Math.min(...centers) > 4;
  };
  const sized = () => tools().nodes().filter((node) => getComputedStyle(node).display !== 'inline').map((node) => ({ node, box: node.getBoundingClientRect() })).filter(({ box }) => box.width > 0);
  const sizeOf = ({ node, box }) => `${named(node)} ${Math.round(box.width)}×${Math.round(box.height)}`;
  const side = ({ box }) => Math.min(box.width, box.height);
  const near = (one, two) => {
    const across = Math.min(one.right, two.right) - Math.max(one.left, two.left);
    const down = Math.min(one.bottom, two.bottom) - Math.max(one.top, two.top);
    const apartY = Math.max(two.top - one.bottom, one.top - two.bottom);
    const apartX = Math.max(two.left - one.right, one.left - two.right);
    return (across > 8 && apartY > -1 && apartY < 4) || (down > 8 && apartX > -1 && apartX < 4);
  };
  const nested = (one, two) => one.node.contains(two.node) || two.node.contains(one.node);
  const apart = (one, two) => one.bottom <= two.top + 1 || one.top >= two.bottom - 1;
  const lonely = (field) => [...field.parentElement.children].filter((child) => child !== field && shown(child)).every((child) => apart(child.getBoundingClientRect(), field.getBoundingClientRect()));
  const inner = (node) => node.clientWidth - parseFloat(getComputedStyle(node).paddingLeft) - parseFloat(getComputedStyle(node).paddingRight);
  const roomOf = (field) => {
    const style = getComputedStyle(field);
    return field.clientWidth - parseFloat(style.paddingLeft) - parseFloat(style.paddingRight);
  };
  const linesOf = (field) => {
    if (field.tagName !== 'TEXTAREA') return 1;
    const style = getComputedStyle(field);
    const height = field.clientHeight - parseFloat(style.paddingTop) - parseFloat(style.paddingBottom);
    return Math.max(1, Math.floor((height + 1) / (parseFloat(style.lineHeight) || parseFloat(style.fontSize) * 1.2)));
  };
  const words = (text) => text.split(/\s+/).filter(Boolean).length;
  const extraWords = (item) => {
    const first = [...item.querySelectorAll('*')].find((node) => own(node) !== '');
    return words(item.innerText) - words(first ? own(first) : '');
  };
  const median = (values) => [...values].sort((one, two) => one - two)[Math.floor(values.length / 2)] ?? 0;
  const lists = () => [...tools().area().querySelectorAll('ul, ol')].filter(shown);
  const described = (list) => median([...list.children].map(extraWords));
  const plain = (text) => tools().loose(text).replace(/^\+\s*/, '').trim().toLowerCase();
  const frame = () => new Promise((done) => requestAnimationFrame(() => setTimeout(done, 0)));
  const spot = (node) => {
    const box = node.getBoundingClientRect();
    return `${Math.round(box.left)},${Math.round(box.top)}`;
  };
  const editable = (field) => shown(field) && !field.disabled && !field.readOnly;
  const neighbours = (field) => [...(field.form ?? field.parentElement).querySelectorAll('button')].filter(shown);
  const speakable = 'textarea, input:not([type]), input[type="text"], input[type="search"]';
  const dictation = () => document.getElementById('dictate') !== null && typeof window.MediaRecorder === 'function';
  const silent = (field) => shown(field) && !field.readOnly && !field.disabled && field.autocomplete !== 'one-time-code' && !field.matches('[data-no-dictation]') && field.dataset.dictation !== 'ready';
  const following = (node) => {
    for (let up = node; up && up.id !== 'screen'; up = up.parentElement) {
      let next = up.nextElementSibling;
      while (next && !shown(next)) next = next.nextElementSibling;
      if (next) return next;
    }
    return null;
  };
  window.checks = {
    measured: () => ({ shift: Math.round(perf.shift * 1000) / 1000, slow: perf.slow, watching: perf.watching, weight: document.querySelectorAll('*').length }),
    restart() {
      perf.shift = 0;
      perf.slow = 0;
    },
    alphabets: () => lettered().map(own).filter((text) => /[\u0430-\u044f\u0451][a-z]|[a-z][\u0430-\u044f\u0451]/i.test(text)).map((text) => text.slice(0, 40)),
    images: () => [...tools().area().querySelectorAll('img')].filter((image) => shown(image) && image.complete && image.naturalWidth === 0).map((image) => image.getAttribute('src') ?? ''),
    overlap() {
      const boxes = lettered().map((node) => ({ node, box: node.getBoundingClientRect() })).filter((entry) => inView(entry.box));
      return boxes.flatMap((_, index) => overlapsOf(boxes, index));
    },
    silent: () => [...tools().area().querySelectorAll('.list, ul, ol')]
      .filter((list) => shown(list) && list.children.length === 0 && !list.parentElement.querySelector('.empty, [data-testid*="empty"], [data-testid*="hint"]')).map(named),
    wide: () => (document.documentElement.scrollWidth > window.innerWidth + 1 ? [`${document.documentElement.scrollWidth}px on a ${window.innerWidth}px screen`] : []),
    cut: () => lettered().filter(cut).map(named),
    offscreen: () => tools().nodes().filter((node) => !scrolled(node)).filter((node) => {
      const box = node.getBoundingClientRect();
      return box.width > 0 && (box.right > window.innerWidth + 1 || box.left < -1);
    }).map(named),
    contrast: () => lettered().map(legible).filter((entry) => entry.ratio < entry.need),
    targets: () => sized().filter((entry) => side(entry) >= 24 && side(entry) < 44).map(sizeOf),
    tiny: () => sized().filter((entry) => side(entry) < 24).map(sizeOf),
    edges() {
      const columns = columnsOf();
      return columns.slice(1).map((column, index) => [columns[index], column]).filter(([one, two]) => two[0] - one[0] >= 2 && two[0] - one[0] <= 6)
        .map(([one, two]) => `${one[1][0]} at ${one[0]}px, ${two[1][0]} at ${two[0]}px`);
    },
    rows: () => [...new Set(tools().nodes().map((node) => node.parentElement))].filter((row) => row && inRow(row) && uneven(row)).map(named),
    cramped() {
      const boxes = [...tools().area().querySelectorAll(`${typeable}, button, [role="button"]`)].filter((node) => shown(node) && framed(node)).map((node) => ({ node, box: node.getBoundingClientRect() }));
      return boxes.flatMap((one, index) => boxes.slice(index + 1).filter((two) => !nested(one, two) && near(one.box, two.box)).map((two) => `${named(one.node)} and ${named(two.node)}`));
    },
    placeholders() {
      const pen = document.createElement('canvas').getContext('2d');
      return [...tools().area().querySelectorAll('input[placeholder], textarea[placeholder]')].filter(shown).filter((field) => {
        const style = getComputedStyle(field);
        pen.font = `${style.fontStyle} ${style.fontWeight} ${style.fontSize} ${style.fontFamily}`;
        return pen.measureText(field.placeholder).width > roomOf(field) * linesOf(field) + 1;
      }).map(named);
    },
    narrow: () => [...tools().area().querySelectorAll(typeable)].filter((field) => shown(field) && lonely(field) && field.getBoundingClientRect().width < inner(field.parentElement) * 0.6).map(named),
    truncated: () => lettered().filter((node) => ellipsed(node) && !node.closest(tools().clickable) && (node.scrollWidth > node.clientWidth + 1 || node.scrollHeight > node.clientHeight + 1)).map(named),
    loud: () => (tools().area().id === 'screen' ? [...tools().area().querySelectorAll('[class*="danger"]')] : []).filter((node) => shown(node) && pressable(node) && reddish(node) && !node.closest('[data-screen="design"]')).map(named),
    heavy: () => lists().filter((list) => list.children.length >= 4 && described(list) > 20).map((list) => `${named(list)}: ${list.children.length} rows with ${described(list)} words of description each`),
    long: () => (tools().area().querySelector('input[type="search"]') ? [] : lists().filter((list) => list.children.length >= 15).map((list) => `${named(list)}: ${list.children.length} rows and no search`)),
    twice() {
      const pressed = new Set(tools().nodes().map((node) => plain(tools().label(node))).filter((text) => text.length > 2));
      const both = new Set(lettered().filter((node) => !node.closest(tools().clickable)).map((node) => plain(own(node))).filter((text) => pressed.has(text)));
      return both.size >= 3 ? [`${both.size} names are both text and buttons: ${[...both].slice(0, 3).join(', ')}`] : [];
    },
    voiceless: () => (dictation() ? [...tools().area().querySelectorAll(speakable)].filter(silent).map(named) : []),
    async jumps() {
      const found = [];
      for (const field of [...tools().area().querySelectorAll(typeable)].filter(editable)) {
        field.focus();
        await frame();
        const buttons = neighbours(field);
        const before = buttons.map(spot);
        field.blur();
        await frame();
        found.push(...buttons.filter((button, index) => button.isConnected && spot(button) !== before[index]).map(named));
      }
      return found;
    },
    titleGap() {
      const title = document.querySelector('#screen h1');
      if (!title || !shown(title)) return null;
      let head = title;
      let next = following(head);
      while (next?.tagName === 'P' && next.getBoundingClientRect().top - head.getBoundingClientRect().bottom < 8) {
        head = next;
        next = following(head);
      }
      return next ? Math.round(next.getBoundingClientRect().top - head.getBoundingClientRect().bottom) : null;
    },
    fonts: () => lettered().filter((node) => parseFloat(getComputedStyle(node).fontSize) < 12).map((node) => `${named(node)} ${getComputedStyle(node).fontSize}`),
    garbage: () => lettered().map(own).filter((text) => /\b(undefined|NaN|null)\b|\[object Object\]|\bHTTP [45]\d\d\b/.test(text)),
    twins() {
      const seen = new Set();
      const twins = [];
      for (const node of tools().nodes().filter(act)) {
        const key = `${act(node)}|${tools().loose(tools().label(node))}`;
        if (seen.has(key)) twins.push(named(node));
        seen.add(key);
      }
      return twins;
    },
    ambiguous() {
      const acts = new Map();
      for (const node of tools().nodes().filter(act)) {
        const name = tools().loose(tools().label(node)).toLowerCase();
        if (name) acts.set(name, (acts.get(name) ?? new Set()).add(shapeOf(act(node))));
      }
      return [...acts].filter(([, set]) => set.size > 1).map(([name, set]) => `«${name}» does ${set.size} things: ${[...set].map((shape) => shape.slice(0, 60)).join(' | ')}`);
    },
    rash: () => [...document.querySelectorAll('#screen [data-do]')].filter((node) => shown(node) && node.dataset.do.includes('"DELETE"'))
      .filter((node) => !['"undo"', '"confirm"'].some((guard) => node.dataset.do.includes(guard))).map(named),
    primaries: () => [...document.querySelectorAll('#screen .button:not(.button-quiet):not(.button-danger)')].filter(shown).map(named),
    exits: () => ({ back: document.querySelector('#screen [data-testid="back"], #screen [data-testid="roots"]') !== null, close: document.querySelector('#sheet:not([hidden]) [data-close]') !== null }),
    words() {
      const byKey = new Map();
      for (const node of tools().nodes().filter((each) => (each.dataset.do || each.dataset.sheet) && !each.closest('.sheet-list'))) {
        const key = shapeOf(node.dataset.do || node.dataset.sheet);
        byKey.set(key, (byKey.get(key) ?? new Set()).add(tools().label(node).split(' ')[0].toLowerCase()));
      }
      return [...byKey].filter(([, words]) => words.size === 1).map(([key, words]) => ({ key, word: [...words][0] })).filter((entry) => entry.word);
    },
    signature() {
      const html = document.documentElement.outerHTML;
      let hash = 0;
      for (let index = 0; index < html.length; index += 1) hash = (hash * 31 + html.charCodeAt(index)) | 0;
      return `${location.href} ${html.length} ${hash}`;
    },
  };
}

function aim(selector) {
  const node = document.querySelector(selector);
  if (!node) return { missing: true };
  node.scrollIntoView({ block: 'center', inline: 'center' });
  const box = node.getBoundingClientRect();
  const x = box.left + box.width / 2;
  const y = box.top + box.height / 2;
  const hit = document.elementFromPoint(x, y);
  if (hit && (hit === node || node.contains(hit))) return { x, y };
  return { covered: hit ? [hit.tagName.toLowerCase(), hit.id && `#${hit.id}`, ...[...hit.classList].map((name) => `.${name}`)].join('') : 'nothing' };
}

function within(promise, ms, what) {
  let timer;
  const late = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error(`${what}: no answer in ${ms} ms`)), ms);
  });
  return Promise.race([promise, late]).finally(() => clearTimeout(timer));
}

function deliver(cdp, message) {
  const waiting = cdp.calls.get(message.id);
  if (!waiting) return cdp.handlers.get(message.method)?.(message.params);
  cdp.calls.delete(message.id);
  if (message.error) waiting.reject(new Error(message.error.message));
  else waiting.resolve(message.result);
}

async function connect(url) {
  const socket = new WebSocket(url);
  await new Promise((resolve, reject) => {
    socket.addEventListener('open', resolve, { once: true });
    socket.addEventListener('error', reject, { once: true });
  });
  const cdp = { socket, next: 0, calls: new Map(), handlers: new Map(), session: undefined };
  socket.addEventListener('message', (event) => deliver(cdp, JSON.parse(event.data)));
  return cdp;
}

function send(cdp, method, params = {}) {
  cdp.next += 1;
  const id = cdp.next;
  return new Promise((resolve, reject) => {
    cdp.calls.set(id, { resolve, reject });
    cdp.socket.send(JSON.stringify({ id, method, params, sessionId: cdp.session }));
  });
}

const call = (crawl, method, params) => within(send(crawl.cdp, method, params), limits.hang, method);

function endpointOf(chrome) {
  return new Promise((resolve, reject) => {
    let said = '';
    chrome.stderr.on('data', (chunk) => {
      said += chunk;
      const found = /DevTools listening on (ws:\S+)/.exec(said);
      if (found) resolve(found[1]);
    });
    chrome.on('exit', (code) => reject(new Error(`chrome exited with ${code}: ${said.slice(-limits.detail)}`)));
  });
}

function newCrawl(out) {
  mkdirSync(join(out, 'screens'), { recursive: true });
  mkdirSync(join(out, 'shots'), { recursive: true });
  writeFileSync(join(out, attached.name), attached.body);
  return {
    out, file: join(out, attached.name), cdp: null, loaded: null, actionAt: 0, log: createWriteStream(join(out, 'crawl.log')), problems: [], notes: [], keys: new Set(), steps: [],
    seen: new Set(), visited: new Map(), found: new Map(), queue: [], pending: new Map(), clicks: 0, requests: 0, words: new Map(), opened: [], gaps: new Map(),
    testids: new Map(), extra: new Set(), tries: new Map(), hung: new Map(),
  };
}

function record(crawl, list, kind, detail) {
  const text = String(detail).slice(0, limits.detail);
  if (crawl.keys.has(`${kind} ${text}`)) return;
  crawl.keys.add(`${kind} ${text}`);
  crawl[list].push({ kind, detail: text, steps: [...crawl.steps] });
  crawl.log.write(`${list === 'problems' ? 'PROBLEM' : 'note'} ${kind}: ${text} (${crawl.steps.join(' → ')})\n`);
}

const problem = (crawl, kind, detail) => record(crawl, 'problems', kind, detail);
const note = (crawl, kind, detail) => record(crawl, 'notes', kind, detail);

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

const sentBy = (sent) => `${sent.url} (${sent.by})`;

function late(crawl, requestId, what) {
  const sent = crawl.hung.get(requestId);
  if (!sent) return;
  crawl.hung.delete(requestId);
  note(crawl, 'late-answer', `${sent.method} ${sent.url} ${what} after ${Date.now() - sent.at} ms`);
}

function responded(crawl, event) {
  const status = event.response.status;
  const what = `${crawl.pending.get(event.requestId)?.method ?? 'GET'} ${event.response.url} → ${status}`;
  if (status >= 500) problem(crawl, `http-${status}`, what);
  else if (status >= 400) note(crawl, `http-${status}`, what);
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

function listen(crawl) {
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

async function start(crawl) {
  const netLog = env.E2E_NETLOG ? [`--log-net-log=${env.E2E_NETLOG}`, '--net-log-capture-mode=Default'] : [];
  const chrome = spawn(env.E2E_CHROMIUM, [...chromeFlags, ...netLog, `--user-data-dir=${join(crawl.out, 'profile')}`, 'about:blank'], { stdio: ['ignore', 'ignore', 'pipe'] });
  try {
    crawl.cdp = await connect(await within(endpointOf(chrome), limits.hang, 'chrome start'));
    await call(crawl, 'Browser.setDownloadBehavior', { behavior: 'deny' });
    for (const name of ['notifications', 'microphone']) await call(crawl, 'Browser.setPermission', { permission: { name }, setting: 'denied', origin: new URL(base).origin });
    const { targetId } = await call(crawl, 'Target.createTarget', { url: 'about:blank' });
    crawl.cdp.session = (await call(crawl, 'Target.attachToTarget', { targetId, flatten: true })).sessionId;
    listen(crawl);
    for (const domain of ['Page', 'Runtime', 'Network', 'Log']) await call(crawl, `${domain}.enable`);
    await call(crawl, 'Emulation.setDeviceMetricsOverride', viewport);
    await call(crawl, 'Page.addScriptToEvaluateOnNewDocument', { source: `(${helpers})()` });
    await call(crawl, 'Page.addScriptToEvaluateOnNewDocument', { source: `(${checks})()` });
    await call(crawl, 'Page.addScriptToEvaluateOnNewDocument', { source: `performance.setResourceTimingBufferSize(${limits.timings})` });
    return chrome;
  } catch (error) {
    chrome.kill();
    throw error;
  }
}

async function evaluate(crawl, fn, arg) {
  const result = await call(crawl, 'Runtime.evaluate', { expression: `(${fn})(${JSON.stringify(arg ?? null)})`, awaitPromise: true, returnByValue: true });
  if (result.exceptionDetails) throw new Error(result.exceptionDetails.text);
  return result.result.value;
}

const inPage = (crawl, fn, arg) => evaluate(crawl, fn, arg).catch(() => null);

async function goto(crawl, url) {
  const loaded = new Promise((resolve) => {
    crawl.loaded = resolve;
  });
  const navigated = await call(crawl, 'Page.navigate', { url });
  if (navigated.errorText) throw new Error(`${url}: ${navigated.errorText}`);
  await within(loaded, limits.hang, `load ${url}`);
}

async function tap(crawl, selector, name) {
  crawl.actionAt = Date.now();
  const spot = await inPage(crawl, aim, selector);
  if (!spot || spot.missing) return note(crawl, 'blocked', `${name} is not on the page`);
  if (spot.covered) return (spot.covered === 'html' ? note : problem)(crawl, 'covered', `${name} is covered by ${spot.covered}`);
  for (const type of ['mouseMoved', 'mousePressed', 'mouseReleased']) {
    await call(crawl, 'Input.dispatchMouseEvent', { type, x: spot.x, y: spot.y, button: 'left', clickCount: 1 }).catch(() => null);
  }
  return true;
}

async function screenshot(crawl, file) {
  const shot = await call(crawl, 'Page.captureScreenshot', { format: 'png' }).catch(() => null);
  if (shot) writeFileSync(join(crawl.out, file), Buffer.from(shot.data, 'base64'));
}

const finishedInPage = (crawl, sent) =>
  inPage(crawl, ({ url, at, near }) => performance.getEntriesByName(url).some((entry) => entry.responseEnd > 0 && Math.abs(performance.timeOrigin + entry.startTime - at) < near),
    { url: sent.url, at: sent.at, near: limits.caused });

async function stuck(crawl) {
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
    await sleep(limits.poll / 2);
  }
  await stuck(crawl);
}

async function shown(crawl, route) {
  const id = screens.get(shape(route))?.id;
  const until = Date.now() + limits.hang;
  while (Date.now() < until) {
    if ((await inPage(crawl, () => window.crawl?.screen())) === id) return true;
    await sleep(limits.poll);
  }
  return false;
}

async function arrive(crawl, route, passed = new Set()) {
  const via = crawl.found.get(route);
  passed.add(route);
  if (deepLink.test(route) || !via || passed.has(via)) return goto(crawl, `${base}/?open=${encodeURIComponent(route)}`).catch((error) => problem(crawl, 'hang', firstLine(error)));
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
  if (!marked || skipped(marked)) {
    crawl.log.write(`  ${marked ? 'unsafe now' : 'gone'}: ${nameOf(target)}\n`);
    return false;
  }
  crawl.clicks += 1;
  const before = await inPage(crawl, () => window.checks.signature());
  const sent = crawl.requests;
  const tapped = await tap(crawl, '[data-crawl]', nameOf(target));
  await settle(crawl);
  if (tapped && sent === crawl.requests && !marked.quiet) await deadCheck(crawl, target, before);
  if (marked.menu) await menuKept(crawl, marked);
  const alert = await inPage(crawl, () => window.crawl.alert());
  if (alert) note(crawl, 'alert', alert);
  return true;
}

async function deadCheck(crawl, target, before) {
  const after = await inPage(crawl, () => window.checks.signature());
  if (before !== null && before === after) problem(crawl, 'dead-button', `${nameOf(target)} changes nothing and sends nothing`);
}

async function menuKept(crawl, target) {
  const kept = await inPage(crawl, (id) => document.querySelector(`[data-testid="${id}"]`)?.checkVisibility() ?? false, target.menu);
  if (kept === false) problem(crawl, 'lost-menu', `${nameOf(target)} leads to a screen without the ${target.menu} menu`);
}

function tell(crawl, kind, where, items, record) {
  if (items.length > 0) record(crawl, kind, `${where}: ${items.length} — ${items.slice(0, 3).join('; ')}`);
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
  tell(crawl, 'unreadable', where, low.filter((entry) => entry.ratio < 3).map((entry) => `${entry.what} ${entry.ratio}:1`), problem);
  tell(crawl, 'low-contrast', where, low.filter((entry) => entry.ratio >= 3).map((entry) => `${entry.what} ${entry.ratio}:1`), note);
}

function remember(crawl, entry, where) {
  const words = crawl.words.get(entry.key) ?? new Map();
  if (!words.has(entry.word)) words.set(entry.word, where);
  crawl.words.set(entry.key, words);
}

async function screenSense(crawl, route) {
  const primaries = await checked(crawl, 'primaries');
  if (primaries.length > 1 && screens.get(shape(route))?.id !== 'design') problem(crawl, 'many-primary', `${route}: ${primaries.join('; ')}`);
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
  if (overlay === 'sheet' && exits && !exits.close) note(crawl, 'no-close', `${where} has no close button`);
}

function titleGaps(crawl) {
  const gaps = [...crawl.gaps].sort((one, two) => one[1] - two[1]);
  if (gaps.length > 1 && gaps.at(-1)[1] - gaps[0][1] > 8) problem(crawl, 'uneven-title-gap', `${gaps[0][1]} px under the title on ${gaps[0][0]}, ${gaps.at(-1)[1]} px on ${gaps.at(-1)[0]}`);
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
  await screenshot(crawl, join('screens', `${screens.get(shape(route)).id}-${size.name}.png`));
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
  const frog = await inPage(crawl, () => {
    for (const node of document.querySelectorAll('[data-crawl]')) node.removeAttribute('data-crawl');
    const node = document.querySelector('[data-testid="assistant-open"]');
    node?.setAttribute('data-crawl', '');
    return node?.checkVisibility() ?? false;
  });
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
  await inPage(crawl, (value) => window.crawl.fill(value), typed);
  for (const opener of path) {
    if (!(await click(crawl, opener))) return false;
    await inPage(crawl, (value) => window.crawl.fill(value), typed);
  }
  return opened.ok;
}

async function shoot(crawl, from) {
  const fresh = crawl.problems.slice(from).filter((entry) => !entry.shot);
  if (fresh.length === 0) return;
  const shot = `shots/${String(crawl.problems.length).padStart(3, '0')}.png`;
  await screenshot(crawl, shot);
  for (const entry of fresh) entry.shot = shot;
}

function keyOf(route, path, target) {
  const owner = target.place === 'shell' ? 'shell' : [shape(route), ...path.map((opener) => (opener.sheet ? shape(opener.sheet) : opener.testid))].join(' > ');
  return `${owner} | ${(target.testid || target.label).replace(/\d+/g, 'N')}`;
}

async function press(crawl, route, path, target) {
  const key = keyOf(route, path, target);
  if (skipped(target) || crawl.seen.has(key)) return;
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
  await inPage(crawl, (value) => window.crawl.fill(value), typed);
  for (const target of (await inPage(crawl, () => window.crawl.targets())) ?? []) await press(crawl, route, [opener], target);
}

function schedule(crawl, route) {
  const key = shape(route);
  if (!screens.has(key) || crawl.visited.has(key)) return;
  crawl.visited.set(key, route);
  crawl.queue.push(route);
}

function leadsTo(crawl, from, route) {
  const passed = new Set();
  for (let at = from; at && !passed.has(at); at = crawl.found.get(at)) {
    if (at === route) return true;
    passed.add(at);
  }
  return false;
}

function discover(crawl, from, routes, onScreen = false) {
  for (const route of routes) {
    if ((onScreen || !crawl.found.has(route)) && !leadsTo(crawl, from, route)) crawl.found.set(route, from);
    schedule(crawl, route);
  }
}

function fillFirst(route, id) {
  const parts = route.split('/');
  parts[parts.findIndex((part) => part.startsWith('{'))] = id;
  return parts.join('/');
}

function guess(crawl, screen) {
  const [head] = screen.route.split('{');
  const ids = [...crawl.found.keys()].map((route) => (route.startsWith(head) ? route.slice(head.length).split(/[/?]/)[0] : ''));
  const id = ids.find((part) => /^\d+$/.test(part));
  return id ? fillFirst(screen.route, id) : '';
}

function guessMissing(crawl) {
  for (const [key, screen] of screens) {
    const route = crawl.visited.has(key) ? '' : guess(crawl, screen);
    if (route && !route.includes('{')) schedule(crawl, route);
  }
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
  if (twice.length > 0) note(crawl, 'repeated-request', `${route}: ${[...new Set(twice)].slice(0, 3).join(', ')}`);
}

async function felt(crawl, route) {
  const seen = await inPage(crawl, () => window.checks.measured());
  if (!seen) return;
  if (seen.shift > 0.25) problem(crawl, 'layout-shift', `${route} moves by ${seen.shift} while it loads`);
  else if (seen.shift > 0.1) note(crawl, 'layout-shift', `${route} moves by ${seen.shift} while it loads`);
  if (seen.slow > 0) note(crawl, 'long-task', `${route}: ${seen.slow} scripts ran longer than 200 ms`);
  if (seen.weight > 3000) note(crawl, 'heavy-screen', `${route}: ${seen.weight} elements`);
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

function seen(crawl, route, testids) {
  const known = crawl.testids.get(shape(route)) ?? new Set();
  for (const testid of testids) known.add(testid);
  crawl.testids.set(shape(route), known);
}

function missing(crawl, key, screen) {
  const known = [...(crawl.testids.get(key) ?? [])];
  return (screen.elements ?? []).filter((element) => !known.some((testid) => testidRe(element.testid).test(testid)));
}

const another = (crawl, key) => [...crawl.found.keys()].find((route) => shape(route) === key && route !== crawl.visited.get(key) && !crawl.extra.has(route));

function widen(crawl) {
  for (const [key, screen] of screens) {
    const tries = crawl.tries.get(key) ?? 0;
    const route = tries < 2 && crawl.visited.has(key) && missing(crawl, key, screen).length > 0 ? another(crawl, key) : '';
    if (!route) continue;
    crawl.tries.set(key, tries + 1);
    crawl.extra.add(route);
    crawl.queue.push(route);
  }
}

async function visit(crawl, route) {
  const screen = screens.get(shape(route));
  crawl.log.write(`screen ${screen.id} ${route}\n`);
  const ok = await checkScreen(crawl, route, screen);
  seen(crawl, route, (await inPage(crawl, () => [...document.querySelectorAll('#screen [data-testid]')].map((node) => node.dataset.testid))) ?? []);
  if (ok) await makeSense(crawl, route, '');
  discover(crawl, route, (await inPage(crawl, () => window.crawl.routes())) ?? [], true);
  if (ok && !quick) await upload(crawl).catch((error) => note(crawl, 'upload', firstLine(error)));
  discover(crawl, route, (await inPage(crawl, () => window.crawl.routes())) ?? []);
  if (!ok) return;
  if (quick) return lookOver(crawl, route);
  await inPage(crawl, (value) => window.crawl.fill(value), typed);
  for (const target of (await inPage(crawl, () => window.crawl.targets())) ?? []) await press(crawl, route, [], target);
}

const alive = (crawl) => crawl.cdp.socket.readyState === WebSocket.OPEN;

function unreached(crawl) {
  crawl.steps = ['after the crawl'];
  if (!alive(crawl)) problem(crawl, 'browser-crashed', 'Chrome closed its DevTools connection during the crawl');
  for (const [key, screen] of screens) if (!crawl.visited.has(key)) note(crawl, 'unreached', `${screen.route} (${screen.id})`);
}

async function crawlAll(crawl) {
  for (const screen of screens.values()) if (!screen.route.includes('{')) schedule(crawl, screen.route);
  while (crawl.queue.length > 0 && alive(crawl)) {
    await visit(crawl, crawl.queue.shift());
    if (crawl.queue.length === 0) guessMissing(crawl);
    if (crawl.queue.length === 0) widen(crawl);
  }
  unreached(crawl);
}

const parse = (line) => {
  try {
    return JSON.parse(line);
  } catch {
    return null;
  }
};

function serverErrors(crawl, file) {
  if (!file || !existsSync(file)) return;
  for (const entry of readFileSync(file, 'utf8').split('\n').map(parse)) {
    if (entry?.level === 'ERROR') problem(crawl, 'server-error', JSON.stringify(entry));
  }
}

function report(crawl) {
  const rows = (entries) => entries.map((entry) => `| ${entry.kind} | ${entry.steps.join(' → ')} | ${cell(entry.detail)} | ${entry.shot ?? ''} |`);
  const head = ['| kind | steps | detail | screenshot |', '| --- | --- | --- | --- |'];
  const counts = `${crawl.visited.size} of ${screens.size} screens, ${crawl.clicks} clicks, ${crawl.problems.length} problems, ${crawl.notes.length} notes`;
  const text = ['# Crawl of every screen', '', `${counts}.`, '', '## Problems', '', ...head, ...rows(crawl.problems), '', '## Notes', '', ...head, ...rows(crawl.notes), ''].join('\n');
  writeFileSync(join(crawl.out, 'crawl.md'), text);
  writeFileSync(join(crawl.out, 'crawl.json'), JSON.stringify({ screens: [...crawl.visited.values()], clicks: crawl.clicks, problems: crawl.problems, notes: crawl.notes }, null, 2));
  return text;
}

test('a robot opens every screen and presses every safe button', { skip: !(base && env.E2E_APPMAP) && 'set E2E_BASE and E2E_APPMAP' }, async () => {
  const crawl = newCrawl(env.E2E_OUT);
  const chrome = await start(crawl);
  try {
    await crawlAll(crawl);
    await (quick ? narrowPass(crawl) : sizesPass(crawl));
    mixedWords(crawl);
    titleGaps(crawl);
  } finally {
    chrome.kill();
  }
  serverErrors(crawl, env.E2E_SERVER_LOG);
  const summary = report(crawl);
  crawl.log.end();
  assert.equal(crawl.problems.length, 0, summary);
});
