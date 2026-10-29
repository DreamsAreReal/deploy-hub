

export function frozenClock(at) {
  const Real = Date;
  function Frozen(...args) {
    if (!new.target) return new Real(at).toString();
    return args.length === 0 ? new Real(at) : new Real(...args);
  }
  Frozen.prototype = Real.prototype;
  Frozen.now = () => at;
  Frozen.parse = Real.parse;
  Frozen.UTC = Real.UTC;
  globalThis.Date = Frozen;
}

export async function pixelDiff({ before, after, tolerance }) {
  const channels = 4;
  const green = 1;
  const blue = 2;
  const opaque = 255;
  const lift = 160;
  const fade = 9;
  const chunk = 32768;
  const decode = (data) => createImageBitmap(new Blob([Uint8Array.from(atob(data), (char) => char.codePointAt(0))], { type: 'image/png' }));
  const [old, now] = await Promise.all([decode(before), decode(after)]);
  const { width, height } = now;
  const total = width * height;
  if (old.width !== width || old.height !== height) return { differ: total, total, diff: '' };
  const canvas = new OffscreenCanvas(width, height);
  const context = canvas.getContext('2d', { willReadFrequently: true });
  const read = (image) => {
    context.clearRect(0, 0, width, height);
    context.drawImage(image, 0, 0);
    return context.getImageData(0, 0, width, height).data;
  };
  const was = read(old);
  const is = read(now);
  const marked = context.createImageData(width, height);
  const off = (at) => Math.max(Math.abs(is[at] - was[at]), Math.abs(is[at + green] - was[at + green]), Math.abs(is[at + blue] - was[at + blue]));
  let differ = 0;
  for (let at = 0; at < is.length; at += channels) {
    const hit = off(at) > tolerance;
    const grey = lift + (is[at] + is[at + green] + is[at + blue]) / fade;
    marked.data.set(hit ? [opaque, 0, 0, opaque] : [grey, grey, grey, opaque], at);
    differ += Number(hit);
  }
  if (differ === 0) return { differ, total, diff: '' };
  context.putImageData(marked, 0, 0);
  const bytes = new Uint8Array(await (await canvas.convertToBlob({ type: 'image/png' })).arrayBuffer());
  const chunks = [];
  for (let at = 0; at < bytes.length; at += chunk) chunks.push(String.fromCodePoint(...bytes.subarray(at, at + chunk)));
  return { differ, total, diff: btoa(chunks.join('')) };
}

export function helpers() {
  const labelLength = 60;
  const opened = (id) => {
    const node = document.getElementById(id);
    return node && !node.hidden ? node : null;
  };
  const scope = () => opened('sheet') ?? opened('chat') ?? document.body;
  const place = (node) => ['sheet', 'chat', 'screen'].find((id) => node.closest(`#${id}`)) ?? 'shell';
  const label = (node) => (node.getAttribute('aria-label') || node.textContent || '').replace(/\s+/g, ' ').trim().slice(0, labelLength);
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

export function checks() {
  const marks = {
    longTask: 200, nameLength: 30, channels: 3, channelMax: 255, srgbKnee: 0.03928, srgbSlope: 12.92, srgbOffset: 0.055, srgbScale: 1.055, srgbGamma: 2.4,
    redWeight: 0.2126, greenWeight: 0.7152, blueWeight: 0.0722, flare: 0.05, largeText: 24, boldText: 18.66, bold: 700, largeRatio: 3, textRatio: 4.5,
    hundredths: 100, overlap: 4, solid: 0.5, redFloor: 150, redLead: 1.6, columnReach: 64, half: 2, rowSkew: 4, touching: 8, gap: 4, lineHeight: 1.2,
    thousandths: 1000, alphabetSample: 40, small: 44, tiny: 24, nearMin: 2, nearMax: 6, narrowShare: 0.6, heavyRows: 4, heavyWords: 20, longRows: 15,
    twiceLength: 2, twiceCount: 3, shown: 3, titleGap: 8, smallFont: 12, actionSample: 60, hashBase: 31,
  };
  const blue = 2;
  const white = [marks.channelMax, marks.channelMax, marks.channelMax, 1];
  const perf = { shift: 0, slow: 0, watching: true };
  try {
    new PerformanceObserver((list) => {
      for (const entry of list.getEntries()) if (!entry.hadRecentInput) perf.shift += entry.value;
    }).observe({ type: 'layout-shift', buffered: true });
    new PerformanceObserver((list) => {
      perf.slow += list.getEntries().filter((entry) => entry.duration > marks.longTask).length;
    }).observe({ type: 'longtask', buffered: true });
  } catch {
    perf.watching = false;
  }
  const tools = () => window.crawl;
  const shown = (node) => node.checkVisibility({ checkOpacity: true, checkVisibilityCSS: true });
  const own = (node) => [...node.childNodes].filter((child) => child.nodeType === Node.TEXT_NODE).map((child) => child.textContent).join('').trim();
  const named = (node) => [node.tagName.toLowerCase(), node.dataset.testid ? `[${node.dataset.testid}]` : '', tools().label(node) ? ` «${tools().label(node).slice(0, marks.nameLength)}»` : ''].join('');
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
    if (numbers.length < marks.channels) return null;
    const scale = value.startsWith('color(') ? marks.channelMax : 1;
    return [numbers[0] * scale, numbers[1] * scale, numbers[blue] * scale, numbers[marks.channels] ?? 1];
  };
  const over = (top, under) => {
    const alpha = top[3] + under[3] * (1 - top[3]);
    const mix = (index) => (alpha === 0 ? 0 : (top[index] * top[3] + under[index] * under[3] * (1 - top[3])) / alpha);
    return [mix(0), mix(1), mix(blue), alpha];
  };
  const page = () => {
    const probe = document.createElement('div');
    probe.style.background = 'var(--bg)';
    document.body.append(probe);
    const color = channels(getComputedStyle(probe).backgroundColor) ?? white;
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
    const share = value / marks.channelMax;
    return share <= marks.srgbKnee ? share / marks.srgbSlope : ((share + marks.srgbOffset) / marks.srgbScale) ** marks.srgbGamma;
  };
  const luminance = (color) => marks.redWeight * linear(color[0]) + marks.greenWeight * linear(color[1]) + marks.blueWeight * linear(color[blue]);
  const ratio = (one, two) => {
    const [light, dark] = [luminance(one), luminance(two)].sort((a, b) => b - a);
    return (light + marks.flare) / (dark + marks.flare);
  };
  const needed = (style) => (parseFloat(style.fontSize) >= marks.largeText || (parseFloat(style.fontSize) >= marks.boldText && Number(style.fontWeight) >= marks.bold) ? marks.largeRatio : marks.textRatio);
  const legible = (node) => {
    const style = getComputedStyle(node);
    const ground = backdrop(node);
    if (!ground) return { what: named(node), ratio: Infinity, need: 0 };
    const value = ratio(over(channels(style.color) ?? [0, 0, 0, 1], ground), ground);
    return { what: named(node), ratio: Math.round(value * marks.hundredths) / marks.hundredths, need: needed(style) };
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
    return width > marks.overlap && height > marks.overlap;
  };
  const overlapsOf = (boxes, index) => boxes.slice(index + 1).filter((other) => crosses(boxes[index], other)).map((other) => `${named(boxes[index].node)} over ${named(other.node)}`);
  const pressable = (node) => node.matches(tools().clickable);
  const reddish = (node) => {
    const [red, green, blue, alpha] = channels(getComputedStyle(node).backgroundColor) ?? [0, 0, 0, 0];
    return alpha > marks.solid && red > marks.redFloor && red > green * marks.redLead && red > blue * marks.redLead;
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
      if (x === null || x - edge > marks.columnReach) return;
      lefts.set(Math.round(x), [...(lefts.get(Math.round(x)) ?? []), named(node)]);
    };
    for (const node of lettered().filter(leading)) add(textLeft(node), node);
    for (const field of [...tools().area().querySelectorAll(typeable)].filter(shown)) add(fieldLeft(field), field);
    return [...lefts].filter(([, names]) => names.length > 1).sort((one, two) => one[0] - two[0]);
  };
  const centerOf = (node) => {
    const box = node.getBoundingClientRect();
    return box.top + box.height / marks.half;
  };
  const inRow = (row) => {
    const style = getComputedStyle(row);
    return style.display.endsWith('flex') && style.flexDirection.startsWith('row') && style.flexWrap === 'nowrap';
  };
  const uneven = (row) => {
    const centers = [...row.children].filter((child) => shown(child) && pressable(child)).map(centerOf);
    return centers.length > 1 && Math.max(...centers) - Math.min(...centers) > marks.rowSkew;
  };
  const sized = () => tools().nodes().filter((node) => getComputedStyle(node).display !== 'inline').map((node) => ({ node, box: node.getBoundingClientRect() })).filter(({ box }) => box.width > 0);
  const sizeOf = ({ node, box }) => `${named(node)} ${Math.round(box.width)}×${Math.round(box.height)}`;
  const side = ({ box }) => Math.min(box.width, box.height);
  const near = (one, two) => {
    const across = Math.min(one.right, two.right) - Math.max(one.left, two.left);
    const down = Math.min(one.bottom, two.bottom) - Math.max(one.top, two.top);
    const apartY = Math.max(two.top - one.bottom, one.top - two.bottom);
    const apartX = Math.max(two.left - one.right, one.left - two.right);
    return (across > marks.touching && apartY > -1 && apartY < marks.gap) || (down > marks.touching && apartX > -1 && apartX < marks.gap);
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
    return Math.max(1, Math.floor((height + 1) / (parseFloat(style.lineHeight) || parseFloat(style.fontSize) * marks.lineHeight)));
  };
  const words = (text) => text.split(/\s+/).filter(Boolean).length;
  const extraWords = (item) => {
    const first = [...item.querySelectorAll('*')].find((node) => own(node) !== '');
    return words(item.innerText) - words(first ? own(first) : '');
  };
  const median = (values) => [...values].sort((one, two) => one - two)[Math.floor(values.length / marks.half)] ?? 0;
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
    measured: () => ({ shift: Math.round(perf.shift * marks.thousandths) / marks.thousandths, slow: perf.slow, watching: perf.watching, weight: document.querySelectorAll('*').length }),
    restart() {
      perf.shift = 0;
      perf.slow = 0;
    },
    alphabets: () => lettered().map(own).filter((text) => /[\u0430-\u044f\u0451][a-z]|[a-z][\u0430-\u044f\u0451]/i.test(text)).map((text) => text.slice(0, marks.alphabetSample)),
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
    targets: () => sized().filter((entry) => side(entry) >= marks.tiny && side(entry) < marks.small).map(sizeOf),
    tiny: () => sized().filter((entry) => side(entry) < marks.tiny).map(sizeOf),
    edges() {
      const columns = columnsOf();
      return columns.slice(1).map((column, index) => [columns[index], column]).filter(([one, two]) => two[0] - one[0] >= marks.nearMin && two[0] - one[0] <= marks.nearMax)
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
    narrow: () => [...tools().area().querySelectorAll(typeable)].filter((field) => shown(field) && lonely(field) && field.getBoundingClientRect().width < inner(field.parentElement) * marks.narrowShare).map(named),
    truncated: () => lettered().filter((node) => ellipsed(node) && !node.closest(tools().clickable) && (node.scrollWidth > node.clientWidth + 1 || node.scrollHeight > node.clientHeight + 1)).map(named),
    loud: () => (tools().area().id === 'screen' ? [...tools().area().querySelectorAll('[class*="danger"]')] : []).filter((node) => shown(node) && pressable(node) && reddish(node) && !node.closest('[data-screen="design"]')).map(named),
    heavy: () => lists().filter((list) => list.children.length >= marks.heavyRows && described(list) > marks.heavyWords).map((list) => `${named(list)}: ${list.children.length} rows with ${described(list)} words of description each`),
    long: () => (tools().area().querySelector('input[type="search"]') ? [] : lists().filter((list) => list.children.length >= marks.longRows).map((list) => `${named(list)}: ${list.children.length} rows and no search`)),
    twice() {
      const pressed = new Set(tools().nodes().map((node) => plain(tools().label(node))).filter((text) => text.length > marks.twiceLength));
      const both = new Set(lettered().filter((node) => !node.closest(tools().clickable)).map((node) => plain(own(node))).filter((text) => pressed.has(text)));
      return both.size >= marks.twiceCount ? [`${both.size} names are both text and buttons: ${[...both].slice(0, marks.shown).join(', ')}`] : [];
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
      while (next?.tagName === 'P' && next.getBoundingClientRect().top - head.getBoundingClientRect().bottom < marks.titleGap) {
        head = next;
        next = following(head);
      }
      return next ? Math.round(next.getBoundingClientRect().top - head.getBoundingClientRect().bottom) : null;
    },
    fonts: () => lettered().filter((node) => parseFloat(getComputedStyle(node).fontSize) < marks.smallFont).map((node) => `${named(node)} ${getComputedStyle(node).fontSize}`),
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
      return [...acts].filter(([, set]) => set.size > 1).map(([name, set]) => `«${name}» does ${set.size} things: ${[...set].map((shape) => shape.slice(0, marks.actionSample)).join(' | ')}`);
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
      for (let index = 0; index < html.length; index += 1) hash = (hash * marks.hashBase + html.charCodeAt(index)) | 0;
      return `${location.href} ${html.length} ${hash}`;
    },
  };
}

export function aim(selector) {
  const node = document.querySelector(selector);
  if (!node) return { missing: true };
  node.scrollIntoView({ block: 'center', inline: 'center' });
  const box = node.getBoundingClientRect();
  const half = 2;
  const x = box.left + box.width / half;
  const y = box.top + box.height / half;
  const hit = document.elementFromPoint(x, y);
  if (hit && (hit === node || node.contains(hit))) return { x, y };
  return { covered: hit ? [hit.tagName.toLowerCase(), hit.id && `#${hit.id}`, ...[...hit.classList].map((name) => `.${name}`)].join('') : 'nothing' };
}

export function holdStill() {
  for (const animation of document.getAnimations()) {
    try {
      animation.finish();
    } catch {
      animation.cancel();
    }
  }
  const hidden = [...document.querySelectorAll('[data-volatile]')].map((node) => [node, node.style.visibility]);
  for (const [node] of hidden) node.style.visibility = 'hidden';
  const field = document.activeElement instanceof HTMLElement ? document.activeElement : null;
  window.crawlStill = { hidden, field, caret: field?.style.caretColor ?? '' };
  if (field) field.style.caretColor = 'transparent';
  return true;
}

export function letGo() {
  const held = window.crawlStill;
  if (!held) return;
  for (const [node, visibility] of held.hidden) node.style.visibility = visibility;
  if (held.field) held.field.style.caretColor = held.caret;
  delete window.crawlStill;
}
