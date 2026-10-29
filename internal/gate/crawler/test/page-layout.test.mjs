import { test } from 'node:test';
import assert from 'node:assert/strict';
import { checking } from './page-env.mjs';

const screen = (body) => `<div id="screen" data-box="0 0 400 800">${body}</div><div id="sheet" hidden></div>`;

test('mixed alphabets, technical words, small fonts and broken images are found in visible text only', (t) => {
  const env = checking(t, screen(`
    <h1 data-testid="title">Задачи</h1><p>Мой task list</p><p>deployнуть сейчас и потом ещё много слов, чтобы обрезать</p>
    <p class="md">ТекстWithLatin</p><button disabled>кнопкаX</button><p aria-disabled="true">словоZ</p><p hidden>скрытоеY</p>
    <p>Value: undefined</p><p>NaN items</p><p>[object Object]</p><p>Error HTTP 503 from api</p><p>HTTP 302 moved</p><p>nullable field</p><p>null</p>
    <small style="font-size: 11.5px">tiny note</small><span style="font-size: 12px">ok size</span>
    <img src="/broken.png"><img id="fine" src="/fine.png"><img hidden src="/hidden.png"><img id="loading" src="/later.png">`));
  Object.defineProperty(env.document.getElementById('fine'), 'naturalWidth', { value: 10 });
  Object.defineProperty(env.document.getElementById('loading'), 'complete', { value: false });
  assert.deepEqual(window.checks.alphabets(), ['deployнуть сейчас и потом ещё много слов']);
  assert.deepEqual(window.checks.garbage(), ['Value: undefined', 'NaN items', '[object Object]', 'Error HTTP 503 from api', 'null']);
  assert.deepEqual(window.checks.fonts(), ['small «tiny note» 11.5px']);
  assert.deepEqual(window.checks.images(), ['/broken.png']);
});

test('an empty list without an empty note is silent and a page wider than the screen is too wide', (t) => {
  const env = checking(t, screen(`
    <div><ul data-testid="tasks"></ul></div><div><ol></ol><p class="empty">Nothing yet</p></div><div><ul><li>x</li></ul></div><div><ul hidden></ul></div>
    <div><div class="list" data-testid="docs"></div><p data-testid="docs-hint">Add a doc</p></div><div><ul></ul><p data-testid="list-empty">None</p></div>
    <div><div class="list"></div></div>`));
  assert.deepEqual(window.checks.silent(), ['ul[tasks]', 'div']);
  assert.deepEqual(window.checks.wide(), []);
  env.document.documentElement.dataset.scrollWidth = '401';
  assert.deepEqual(window.checks.wide(), []);
  env.document.documentElement.dataset.scrollWidth = '402';
  assert.deepEqual(window.checks.wide(), ['402px on a 400px screen']);
});

test('text that overlaps other text in view is found, touching or nested text is not', (t) => {
  checking(t, screen(`
    <p data-box="0 0 100 20">one</p><p data-box="95 15 100 20">two</p>
    <p data-box="0 100 100 20">three</p><p data-box="96 100 100 20">four</p>
    <p data-box="0 200 100 20">five</p><p data-box="0 216 100 20">six</p>
    <p data-box="0 300 100 20">seven <b data-box="0 300 50 20">bold</b></p>
    <p data-box="0 799 100 20">edge</p><p data-box="0 805 100 20">below</p><p data-box="0 -30 100 30">above</p><p data-box="0 -25 100 20">gone</p>
    <p data-box="0 400 0 20">empty</p><p data-box="0 400 100 20">full</p>`));
  assert.deepEqual(window.checks.overlap(), ['p «one» over p «two»']);
});

test('text cut by its box, by a clipping parent or growing out of its box is found unless it ends in an ellipsis', (t) => {
  checking(t, screen(`
    <p style="overflow-x: hidden" data-size="102 100 0 0">clipped</p><p style="overflow-x: hidden" data-size="101 100 0 0">fits</p>
    <p style="overflow-x: hidden; text-overflow: ellipsis" data-size="200 100 20 20">ellipsis</p>
    <div style="overflow-x: hidden" data-box="0 400 100 50"><span data-box="0 400 102 20">spill</span><span data-box="0 420 101 20">inside</span><span data-box="-2 440 50 20">left</span></div>
    <div style="overflow-x: hidden" data-box="0 450 100 50"><div style="overflow-x: auto" data-box="0 450 100 50"><span data-box="0 450 300 20">scrolls</span></div></div>
    <div style="overflow-x: hidden; text-overflow: ellipsis" data-box="0 500 100 50"><span data-box="0 500 300 20">dotted</span></div>
    <div data-box="0 600 100 20" data-text-box="0 600 103 20">outgrown</div><div data-box="0 620 100 20" data-text-box="0 620 101 20">snug</div>
    <div data-box="10 640 100 20" data-text-box="8 640 50 20">leftward</div><div data-box="10 660 100 20" data-text-box="9 660 50 20">near</div>
    <span data-box="0 680 10 20" data-text-box="0 680 300 20">inline</span>
    <div style="overflow-x: clip" data-box="0 700 100 20" data-text-box="0 700 300 20" data-size="100 100 0 0">clipped quietly</div>
    <span data-box="0 720 102 20">free</span>`));
  assert.deepEqual(window.checks.cut(), ['p «clipped»', 'span «spill»', 'span «left»', 'div «outgrown»', 'div «leftward»']);
});

test('only ellipsed text that really overflows outside a button counts as truncated', (t) => {
  checking(t, screen(`
    <p style="text-overflow: ellipsis" data-size="200 100 20 20">wide</p><p style="text-overflow: ellipsis" data-size="101 100 20 20">fits</p>
    <p style="-webkit-line-clamp: 2" data-size="100 100 41 39">tall</p><p style="-webkit-line-clamp: 2" data-size="100 100 40 39">short</p>
    <button><span style="text-overflow: ellipsis" data-size="300 100 20 20">in a button</span></button><p data-size="300 100 20 20">plain</p>`));
  assert.deepEqual(window.checks.truncated(), ['p «wide»', 'p «tall»']);
});

test('a button beyond the screen edge is off screen unless it scrolls with its parent', (t) => {
  checking(t, screen(`
    <button data-box="350 0 52 40">right</button><button data-box="350 50 51 40">edge</button><button data-box="-2 100 40 40">left</button>
    <button data-box="-1 150 40 40">flush</button><div style="overflow-x: scroll"><button data-box="500 200 40 40">far</button></div>
    <div style="overflow-x: auto"><div><button data-box="500 220 40 40">deep</button></div></div><button data-box="500 250 0 0">none</button>`));
  assert.deepEqual(window.checks.offscreen(), ['button «right»', 'button «left»']);
});

test('text darker or lighter than its ground needs a ratio of 4.5, or 3 when large', (t) => {
  const env = checking(t, screen(`
    <p style="color: rgb(0, 0, 0)">black</p><p style="color: rgb(170, 170, 170)">light</p><p style="color: rgb(118, 118, 118)">grey 118</p>
    <p style="color: rgb(119, 119, 119)">grey 119</p><p style="color: rgb(148, 148, 148); font-size: 24px">large 148</p>
    <p style="color: rgb(150, 150, 150); font-size: 24px">large 150</p><p style="color: rgb(150, 150, 150); font-size: 23.9px">smaller 150</p>
    <p style="color: rgb(150, 150, 150); font-size: 18.66px; font-weight: 700">bold 150</p><p style="color: rgb(148, 148, 148); font-size: 18.66px; font-weight: 700">bold 148</p>
    <p style="color: rgb(148, 148, 148); font-size: 18.66px; font-weight: 600">semi 148</p><p style="color: rgb(148, 148, 148); font-size: 18.6px; font-weight: 700">small bold 148</p>
    <p style="color: rgb(149, 149, 149); font-size: 24px">large 149</p><p style="color: rgba(0, 0, 0, 0.5)">half</p><p style="color: transparent">ghost</p>
    <div style="background-color: rgba(0, 0, 0, 0.5)"><p style="color: rgb(255, 255, 255)">on shade</p></div>
    <div style="background-color: rgb(0, 0, 0)"><p style="color: rgb(90, 90, 90)">on black</p></div>
    <div style="background-image: url(/bg.png)"><p style="color: rgb(250, 250, 250)">on image</p></div>
    <p style="color: color(srgb 0.6667 0.6667 0.6667)">srgb light</p>`));
  env.document.documentElement.dataset.bg = 'rgb(255, 255, 255)';
  const low = window.checks.contrast();
  assert.deepEqual(low, [
    { what: 'p «light»', ratio: 2.32, need: 4.5 }, { what: 'p «grey 119»', ratio: 4.48, need: 4.5 }, { what: 'p «large 150»', ratio: 2.96, need: 3 },
    { what: 'p «smaller 150»', ratio: 2.96, need: 4.5 }, { what: 'p «bold 150»', ratio: 2.96, need: 3 }, { what: 'p «semi 148»', ratio: 3.03, need: 4.5 },
    { what: 'p «small bold 148»', ratio: 3.03, need: 4.5 }, { what: 'p «half»', ratio: 3.98, need: 4.5 }, { what: 'p «on shade»', ratio: 3.98, need: 4.5 }, { what: 'p «on black»', ratio: 3.04, need: 4.5 },
    { what: 'p «srgb light»', ratio: 2.32, need: 4.5 },
  ]);
  env.document.documentElement.dataset.bg = 'transparent';
  assert.deepEqual(window.checks.contrast().map((entry) => entry.what).slice(0, 2), ['p «light»', 'p «grey 119»']);
  env.document.documentElement.dataset.bg = 'rgb(0, 0, 0)';
  assert.ok(window.checks.contrast().some((entry) => entry.what === 'p «black»' && entry.ratio === 1));
});

test('buttons under 44 px are small targets and under 24 px tiny, inline links aside', (t) => {
  checking(t, screen(`
    <button data-box="0 0 44 44">ok</button><button data-box="0 50 43 60">narrow</button><button data-box="0 120 60 24">flat</button>
    <button data-box="0 150 23.6 40">thin</button><a href="/x" data-box="0 200 10 10">link</a><button data-box="0 220 0 0">gone</button>`));
  assert.deepEqual(window.checks.targets(), ['button «narrow» 43×60', 'button «flat» 60×24']);
  assert.deepEqual(window.checks.tiny(), ['button «thin» 24×40']);
});

test('text and field edges 2 to 6 px apart near the left side are almost aligned', (t) => {
  checking(t, screen(`
    <p data-text-box="16 0 50 20">p1</p><p data-text-box="16 30 50 20">p2</p><p data-text-box="20 60 50 20">p3</p>
    <input aria-label="f1" data-box="12 90 200 40" style="border-left-width: 1px; padding-left: 7px">
    <p data-text-box="40 140 50 20">p5</p><p data-text-box="40 170 50 20">p6</p><p data-text-box="46 200 50 20">p7</p><p data-text-box="46 230 50 20">p8</p>
    <p data-text-box="53 260 50 20">p9</p><p data-text-box="53 290 50 20">p10</p><p data-text-box="54 320 50 20">p14</p><p data-text-box="54 350 50 20">p15</p>
    <p data-text-box="30 380 50 20">alone</p><p data-text-box="70 410 50 20">far1</p><p data-text-box="70 440 50 20">far2</p>
    <p style="text-align: center" data-text-box="18 470 50 20">centred</p><p style="text-align: left" data-text-box="64.4 500 50 20">left1</p>
    <p data-text-box="64 530 50 20">at64</p><span data-text-box="16 560 50 20">inline</span><p>no rect</p>
    <input hidden data-box="16 600 100 40">`));
  assert.deepEqual(window.checks.edges(), ['p «p1» at 16px, p «p3» at 20px', 'p «p5» at 40px, p «p7» at 46px']);
});

test('buttons of one row whose middles differ by more than 4 px are uneven', (t) => {
  checking(t, screen(`
    <div data-testid="r1" style="display: flex"><button data-box="0 0 40 40">a</button><button data-box="50 5 40 40">b</button></div>
    <div data-testid="r2" style="display: flex"><button data-box="0 100 40 40">c</button><button data-box="50 104 40 40">d</button></div>
    <div data-testid="r3" style="display: flex; flex-direction: column"><button data-box="0 200 40 40">e</button><button data-box="0 260 40 40">f</button></div>
    <div data-testid="r4" style="display: flex; flex-wrap: wrap"><button data-box="0 300 40 40">g</button><button data-box="50 320 40 40">h</button></div>
    <div data-testid="r5" style="display: inline-flex"><button data-box="0 400 40 40">i</button><button data-box="50 420 40 40">j</button></div>
    <div data-testid="r6"><button data-box="0 500 40 40">k</button><button data-box="50 520 40 40">l</button></div>
    <div data-testid="r7" style="display: flex"><button data-box="0 600 40 40">m</button><span data-box="50 620 40 40">n</span></div>
    <div data-testid="r8" style="display: flex; flex-direction: row-reverse"><button data-box="0 700 40 40">o</button><button hidden data-box="50 720 40 40">p</button><button data-box="90 706 40 40">q</button></div>`));
  assert.deepEqual(window.checks.rows(), ['div[r1] «ab»', 'div[r5] «ij»', 'div[r8] «opq»']);
});

test('framed controls closer than 4 px are cramped, overlapping or nested ones are not', (t) => {
  const framed = 'background-color: rgb(240, 240, 240)';
  const border = (width) => `border-top-width: ${width}; border-right-width: 1px; border-bottom-width: 1px; border-left-width: 1px; border-top-color: rgb(0, 0, 0); `
    + 'border-right-color: rgb(0, 0, 0); border-bottom-color: rgb(0, 0, 0); border-left-color: rgb(0, 0, 0)';
  checking(t, screen(`
    <button style="${framed}" data-box="0 0 100 40">b1</button><button style="${framed}" data-box="0 43 100 40">b2</button>
    <button style="${framed}" data-box="0 100 100 40">b3</button><button style="${framed}" data-box="0 144 100 40">b4</button>
    <button style="${framed}" data-box="0 200 100 40">b5</button><button style="${border('1px')}" data-box="103 200 100 40">b6</button>
    <button style="${border('0px')}" data-box="0 260 100 40">b7</button><button style="${framed}" data-box="0 302 100 40">b8</button>
    <button style="${framed}" data-box="0 400 100 40">b9</button><button style="${framed}" data-box="0 430 100 40">b10</button>
    <div role="button" style="${framed}" data-box="0 500 200 60">outer<button style="${framed}" data-box="10 510 50 40">inner</button></div>
    <button style="${framed}" data-box="0 600 100 40">b11</button><button style="${framed}" data-box="92 643 100 40">b12</button>
    <input style="${framed}" aria-label="field" data-box="0 700 100 40"><button style="${framed}" data-box="104 700 40 40">b13</button>
    <button style="${framed}" data-box="0 760 100 40">b14</button><button style="${framed}" data-box="100 760 40 40">b15</button>`));
  assert.deepEqual(window.checks.cramped(), ['button «b1» and button «b2»', 'button «b5» and button «b6»', 'button «b14» and button «b15»']);
});

test('a placeholder longer than its field is cut, counting the lines of a text area', (t) => {
  checking(t, screen(`
    <input aria-label="fits" placeholder="Search tasks" data-size="0 100 0 0"><input aria-label="search" placeholder="Search all the tasks" style="padding-left: 5px; padding-right: 5px" data-size="0 150 0 0">
    <input aria-label="exact" placeholder="0123456789" data-size="0 79 0 0"><input aria-label="over" placeholder="0123456789" data-size="0 78 0 0">
    <textarea aria-label="notes" placeholder="${'x'.repeat(40)}" data-size="0 100 0 50"></textarea>
    <textarea aria-label="lines" placeholder="${'x'.repeat(40)}" style="line-height: 10px" data-size="0 100 0 50"></textarea>
    <textarea aria-label="padded" placeholder="${'x'.repeat(30)}" style="padding-top: 5px; padding-bottom: 5px; line-height: 20px" data-size="0 100 0 50"></textarea>
    <input hidden placeholder="${'x'.repeat(40)}"><input aria-label="plain">`));
  assert.deepEqual(window.checks.placeholders(), ['input «search»', 'input «over»', 'textarea «notes»', 'textarea «padded»']);
});

test('a field alone in its row and narrower than 60% of its parent is narrow', (t) => {
  checking(t, screen(`
    <div data-size="0 400 0 0"><label data-box="0 -30 100 20">Name</label><input aria-label="alone" data-box="0 0 200 40"></div>
    <div data-size="0 400 0 0"><input aria-label="beside" data-box="0 100 200 40"><button data-box="210 100 40 40">go</button></div>
    <div data-size="0 400 0 0"><input aria-label="wide" data-box="0 200 240 40"></div>
    <div data-size="0 400 0 0" style="padding-left: 20px; padding-right: 20px"><input aria-label="padded" data-box="0 300 215 40"><input aria-label="padded wide" data-box="0 350 216 40"></div>
    <div data-size="0 400 0 0"><p data-box="0 439 100 20">touch</p><input aria-label="touching" data-box="0 400 100 40"><p data-box="0 380 100 21">above</p></div>
    <div data-size="0 400 0 0"><input aria-label="crowded" data-box="0 600 100 40"><p data-box="0 638 100 20">below</p></div>
    <div data-size="0 400 0 0"><p data-box="0 580 100 22">over</p><input aria-label="pressed" data-box="0 600 100 40"></div>
    <div data-size="0 400 0 0"><input aria-label="hidden" hidden data-box="0 500 10 40"><input aria-label="shadow" data-box="0 500 100 40"><p hidden data-box="0 500 100 40">x</p></div>`));
  assert.deepEqual(window.checks.narrow(), ['input «alone»', 'input «padded»', 'input «touching»', 'input «shadow»']);
});

test('text a person cannot see because of opacity or visibility is not read', (t) => {
  checking(t, screen('<p style="opacity: 0">undefined</p><div style="visibility: hidden"><p>NaN here</p></div><p>[object Object]</p>'));
  assert.deepEqual(window.checks.garbage(), ['[object Object]']);
});

test('contrast counts translucent colours and layers and stops at the first opaque ground', (t) => {
  const env = checking(t, screen(`
    <p style="color: rgb(5, 5, 5)">near black</p><p style="color: rgba(255, 0, 0, 0.5)">half red</p>
    <div style="background-color: rgba(0, 0, 0, 0.5)"><div style="background-color: rgba(255, 255, 255, 0.5)"><p style="color: rgb(80, 80, 80)">stacked</p></div></div>
    <div style="background-image: url(/bg.png)"><div style="background-color: rgb(0, 0, 0)"><p style="color: rgb(90, 90, 90)">dark card</p></div></div>
    <div style="background-color: transparent"><p style="color: rgb(0, 0, 0)">clear ground</p></div>`));
  env.document.documentElement.dataset.bg = 'rgb(255, 255, 255)';
  assert.deepEqual(window.checks.contrast(), [{ what: 'p «half red»', ratio: 2.44, need: 4.5 }, { what: 'p «stacked»', ratio: 4.4, need: 4.5 },
    { what: 'p «dark card»', ratio: 3.04, need: 4.5 }]);
});

test('a translucent layer over a page without a colour keeps its own transparency', (t) => {
  const env = checking(t, screen('<div style="background-color: rgba(255, 255, 255, 0.5)"><p style="color: rgba(255, 0, 0, 0.5)">veiled</p></div>'));
  env.document.documentElement.dataset.bg = 'rgba(0, 0, 0, 0)';
  assert.deepEqual(window.checks.contrast(), [{ what: 'p «veiled»', ratio: 3.14, need: 4.5 }]);
});

test('text one pixel past its clipping edge still fits and only text nodes can outgrow their box', (t) => {
  checking(t, screen(`<div style="overflow-x: hidden" data-box="0 0 100 50"><span data-box="-1 0 50 20">flush</span><span data-box="0 20 101 20">snug</span></div>
    <div data-box="0 100 100 20">ok <b data-text-box="0 100 150 20">wide child</b></div>`));
  assert.deepEqual(window.checks.cut(), []);
});

test('text exactly at the bottom or top edge of the screen is out of view', (t) => {
  checking(t, screen(`<p data-box="200 790 100 20">low</p><p data-box="200 800 100 20">lower</p>
    <p data-box="200 -30 100 30">above</p><p data-box="200 -10 100 20">peek</p>`));
  assert.deepEqual(window.checks.overlap(), []);
});

test('controls framed only by transparent borders do not count, and closeness is measured to the pixel', (t) => {
  const framed = 'background-color: rgb(240, 240, 240)';
  const ghost = 'border-top-width: 1px; border-right-width: 1px; border-bottom-width: 1px; border-left-width: 1px';
  checking(t, screen(`
    <button style="${ghost}" data-box="0 0 100 40">g1</button><button style="${framed}" data-box="0 42 100 40">g2</button>
    <button style="${framed}" data-box="0 200 100 40">v1</button><button style="${framed}" data-box="0 239 100 40">v2</button>
    <button style="${framed}" data-box="0 400 100 40">h1</button><button style="${framed}" data-box="103 432 100 40">h2</button>
    <button style="${framed}" data-box="0 600 100 40">x1</button><button style="${framed}" data-box="99 600 100 40">x2</button>
    <button style="${framed}" data-box="0 800 100 40">t1</button><button style="${framed}" data-box="0 840 100 40">t2</button>
    <div role="button" style="${framed}" data-box="200 0 200 60">outer<button style="${framed}" data-box="200 60 200 0">edge</button></div>`));
  assert.deepEqual(window.checks.cramped(), ['button «t1» and button «t2»']);
});

test('columns are measured from the left edge of the open area and need two members each', (t) => {
  checking(t, `<div id="screen" data-box="10 0 400 800">
    <p data-text-box="56 0 50 20">late1</p><p data-text-box="56 30 50 20">late2</p><p data-text-box="50 60 50 20">early1</p><p data-text-box="50 90 50 20">early2</p>
    <p data-text-box="34 120 50 20">p34</p><span data-text-box="34 150 50 20">s34</span><p style="text-align: center" data-text-box="30 180 50 20">c30</p>
    <p data-text-box="30 210 50 20">p30</p><p data-text-box="80 240 50 20"><b data-text-box="70 240 10 20">B</b> tail</p>
    <p data-text-box="70 270 50 20">r70</p><p data-text-box="70 300 50 20">r70b</p><p data-text-box="72 330 50 20">r72</p><p data-text-box="72 360 50 20">r72b</p>
    <p data-text-box="74 390 50 20">r74</p><p data-text-box="74 420 50 20">r74b</p><p data-text-box="75 450 50 20">r75</p><p data-text-box="75 480 50 20">r75b</p>
  </div><div id="sheet" hidden></div>`);
  assert.deepEqual(window.checks.edges(), ['p «early1» at 50px, p «late1» at 56px', 'p «r70» at 70px, p «r72» at 72px', 'p «r72» at 72px, p «r74» at 74px']);
});

test('buttons of different heights whose middles line up are an even row', (t) => {
  checking(t, screen('<div data-testid="row" style="display: flex"><button data-box="0 0 40 40">tall</button><button data-box="50 5 40 30">short</button></div>'));
  assert.deepEqual(window.checks.rows(), []);
});

test('a placeholder is measured against the room left by the padding and the lines a text area shows', (t) => {
  checking(t, screen(`
    <input aria-label="padded" placeholder="${'x'.repeat(18)}" style="padding-left: 5px; padding-right: 5px" data-size="0 150 0 0">
    <textarea aria-label="roomy" placeholder="${'x'.repeat(13)}" style="padding-top: 20px; padding-bottom: 20px; line-height: 20px" data-size="0 100 0 60"></textarea>
    <textarea aria-label="almost" placeholder="${'x'.repeat(19)}" style="line-height: 20px" data-size="0 100 0 39"></textarea>
    <textarea aria-label="normal" placeholder="${'x'.repeat(30)}" data-size="0 100 0 50"></textarea>`));
  assert.deepEqual(window.checks.placeholders(), ['input «padded»', 'textarea «roomy»', 'textarea «normal»']);
});

test('a word mixing alphabets is found whichever alphabet comes first', (t) => {
  checking(t, screen('<p>Отчётlog</p><p>logОтчёт</p><p>Отчёт log</p>'));
  assert.deepEqual(window.checks.alphabets(), ['Отчётlog', 'logОтчёт']);
});
