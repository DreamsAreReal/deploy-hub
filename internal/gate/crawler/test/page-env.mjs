import { Window } from 'happy-dom';
import { checks, helpers } from '../page.mjs';

const defaults = {
  display: 'block', overflowX: 'visible', textOverflow: 'clip', webkitLineClamp: 'none', backgroundImage: 'none', backgroundColor: 'rgba(0, 0, 0, 0)',
  color: 'rgb(0, 0, 0)', fontSize: '16px', fontWeight: '400', fontStyle: 'normal', fontFamily: 'sans-serif', lineHeight: 'normal', textAlign: 'start',
  flexDirection: 'row', flexWrap: 'nowrap', paddingLeft: '0px', paddingRight: '0px', paddingTop: '0px', paddingBottom: '0px', borderLeftWidth: '0px',
  borderTopWidth: '0px', borderRightWidth: '0px', borderBottomWidth: '0px', borderTopColor: 'rgba(0, 0, 0, 0)', borderRightColor: 'rgba(0, 0, 0, 0)',
  borderBottomColor: 'rgba(0, 0, 0, 0)', borderLeftColor: 'rgba(0, 0, 0, 0)',
};
const inline = new Set(['A', 'SPAN', 'B', 'I', 'EM', 'STRONG', 'CODE', 'LABEL', 'SMALL']);
const camel = (name) => name.trim().replace(/^-/, '').replace(/-([a-z])/g, (_, letter) => letter.toUpperCase());

function declared(element) {
  const style = {};
  for (const part of (element.getAttribute('style') ?? '').split(';')) {
    const at = part.indexOf(':');
    if (at > 0) style[camel(part.slice(0, at))] = part.slice(at + 1).trim();
  }
  return style;
}

function rect(left, top, width, height) {
  return { x: left, y: top, left, top, width, height, right: left + width, bottom: top + height };
}

function boxOf(element, attribute = 'data-box') {
  const [left, top, width, height] = (element?.getAttribute(attribute) ?? '0 0 0 0').split(' ').map(Number);
  return rect(left, top, width, height);
}

export function pageEnv(html, { url = 'https://app/ui/home', width = 400, height = 800 } = {}) {
  const window = new Window({ url, width, height });
  const { document } = window;
  document.body.innerHTML = html;
  const animations = [];
  const pageColor = (style) => (Object.values(style).includes('var(--bg)') ? { backgroundColor: document.documentElement.dataset.bg ?? defaults.backgroundColor } : {});
  const computed = (element) => {
    const style = declared(element);
    return { ...defaults, display: inline.has(element.tagName) ? 'inline' : 'block', ...style, ...pageColor(style) };
  };
  window.getComputedStyle = computed;
  window.Element.prototype.getBoundingClientRect = function box() {
    return boxOf(this);
  };
  window.Element.prototype.checkVisibility = function visible() {
    return !this.closest('[hidden], [data-invisible]') && this.isConnected;
  };
  window.Element.prototype.scrollIntoView = function scrolled() {
    this.setAttribute('data-scrolled', '');
  };
  window.Range.prototype.getBoundingClientRect = function textBox() {
    return boxOf(this.startContainer?.parentElement ?? this.startContainer, 'data-text-box');
  };
  window.Range.prototype.getClientRects = function textRects() {
    const owner = this.startContainer?.parentElement ?? this.startContainer;
    return owner?.hasAttribute('data-text-box') ? [boxOf(owner, 'data-text-box')] : [];
  };
  window.HTMLCanvasElement.prototype.getContext = () => ({ font: '', measureText: (text) => ({ width: text.length * 8 }) });
  for (const [name, index] of [['scrollWidth', 0], ['clientWidth', 1], ['scrollHeight', 2], ['clientHeight', 3]]) {
    for (const owner of [window.Element.prototype, window.HTMLElement.prototype]) {
      Object.defineProperty(owner, name, { configurable: true, get() { return Number((this.getAttribute('data-size') ?? '0 0 0 0').split(' ')[index]); } });
    }
  }
  document.getAnimations = () => animations;
  document.elementFromPoint = () => null;
  Object.defineProperty(document.documentElement, 'scrollWidth', { configurable: true, get: () => Number(document.documentElement.dataset.scrollWidth ?? width) });
  const names = ['window', 'document', 'location', 'Node', 'Event', 'HTMLElement', 'getComputedStyle', 'requestAnimationFrame', 'PerformanceObserver'];
  const saved = new Map(names.map((name) => [name, Object.getOwnPropertyDescriptor(globalThis, name)]));
  const values = { window, document, location: window.location, Node: window.Node, Event: window.Event, HTMLElement: window.HTMLElement, getComputedStyle: computed,
    requestAnimationFrame: (done) => setTimeout(done, 0), PerformanceObserver: window.PerformanceObserver };
  for (const name of names) Object.defineProperty(globalThis, name, { configurable: true, writable: true, value: values[name] });
  return {
    window, document, animations,
    close() {
      for (const [name, descriptor] of saved) {
        if (descriptor) Object.defineProperty(globalThis, name, descriptor);
        else Reflect.deleteProperty(globalThis, name);
      }
      window.happyDOM.abort();
      window.close();
    },
  };
}

export function checking(t, html, options) {
  const env = pageEnv(html, options);
  t.after(() => env.close());
  helpers();
  checks();
  return env;
}
