const numeric = /^\d+$/;
const digits = /\d+/g;
const maxTries = 2;

export const placeholder = (part) => part.startsWith('{') || numeric.test(part);
export const shape = (route) => route.split('?')[0].split('/').map((part) => (placeholder(part) ? 'N' : part)).join('/');
function piecesOf(testid) {
  const pieces = [''];
  testid.split('-').forEach((part, at) => {
    if (at > 0) pieces[pieces.length - 1] += '-';
    if (part.startsWith('{')) pieces.push('');
    else pieces[pieces.length - 1] += part;
  });
  return pieces;
}

function fits(pieces, text) {
  const [first, ...rest] = pieces;
  if (rest.length === 0) return text === first;
  const last = rest.pop();
  if (!text.startsWith(first) || !text.endsWith(last)) return false;
  let at = first.length;
  for (const piece of rest) {
    const found = text.indexOf(piece, at + 1);
    if (found < 0) return false;
    at = found + piece.length;
  }
  return text.length - last.length > at;
}

export const testidPattern = (testid) => {
  const pieces = piecesOf(testid);
  return { test: (text) => fits(pieces, text) };
};
export const skipped = (crawl, target) => target.unsafe || crawl.config.forbidden.some((pattern) => pattern.test(target.testid));

export function schedule(crawl, route) {
  const key = shape(route);
  if (!crawl.config.screens.has(key) || crawl.visited.has(key)) return;
  crawl.visited.set(key, route);
  crawl.queue.push(route);
}

export function leadsTo(crawl, from, route) {
  const passed = new Set();
  for (let at = from; at && !passed.has(at); at = crawl.found.get(at)) {
    if (at === route) return true;
    passed.add(at);
  }
  return false;
}

export function discover(crawl, from, routes, onScreen = false) {
  for (const route of routes) {
    if ((onScreen || !crawl.found.has(route)) && !leadsTo(crawl, from, route)) crawl.found.set(route, from);
    schedule(crawl, route);
  }
}

export function fillFirst(route, id) {
  const parts = route.split('/');
  parts[parts.findIndex((part) => part.startsWith('{'))] = id;
  return parts.join('/');
}

export function guess(crawl, screen) {
  const [head] = screen.route.split('{');
  const ids = [...crawl.found.keys()].map((route) => (route.startsWith(head) ? route.slice(head.length).split(/[/?]/)[0] : ''));
  const id = ids.find((part) => numeric.test(part));
  return id ? fillFirst(screen.route, id) : '';
}

export function guessMissing(crawl) {
  for (const [key, screen] of crawl.config.screens) {
    const route = crawl.visited.has(key) ? '' : guess(crawl, screen);
    if (route && !route.includes('{')) schedule(crawl, route);
  }
}

export function seen(crawl, route, testids) {
  const known = crawl.testids.get(shape(route)) ?? new Set();
  for (const testid of testids) known.add(testid);
  crawl.testids.set(shape(route), known);
}

export function missing(crawl, key, screen) {
  const known = [...(crawl.testids.get(key) ?? [])];
  return (screen.elements ?? []).filter((element) => !known.some((testid) => testidPattern(element.testid).test(testid)));
}

const another = (crawl, key) => [...crawl.found.keys()].find((route) => shape(route) === key && route !== crawl.visited.get(key) && !crawl.extra.has(route));

export function widen(crawl) {
  for (const [key, screen] of crawl.config.screens) {
    const tries = crawl.tries.get(key) ?? 0;
    const route = tries < maxTries && crawl.visited.has(key) && missing(crawl, key, screen).length > 0 ? another(crawl, key) : '';
    if (!route) continue;
    crawl.tries.set(key, tries + 1);
    crawl.extra.add(route);
    crawl.queue.push(route);
  }
}

export function keyOf(route, path, target) {
  const owner = target.place === 'shell' ? 'shell' : [shape(route), ...path.map((opener) => (opener.sheet ? shape(opener.sheet) : opener.testid))].join(' > ');
  return `${owner} | ${(target.testid || target.label).replace(digits, 'N')}`;
}
