import { copyFileSync, existsSync, mkdirSync, readdirSync, readFileSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { sizes, visual } from './config.mjs';
import { call, goto, inPage } from './cdp.mjs';
import { holdStill, letGo, pixelDiff } from './page.mjs';
import { note, problem } from './record.mjs';

export async function screenshot(crawl, file) {
  const held = Number.isFinite(crawl.config.frozenAt) && (await inPage(crawl, holdStill));
  const shot = await call(crawl, 'Page.captureScreenshot', { format: 'png' }).catch(() => null);
  if (held) await inPage(crawl, letGo);
  if (shot) writeFileSync(join(crawl.out, file), Buffer.from(shot.data, 'base64'));
}

const stemOf = (file) => file.replace(/\.png$/, '');

function screenOf(stem) {
  const suffix = sizes.map((size) => `-${size.name}`).find((end) => stem.endsWith(end));
  return suffix ? stem.slice(0, -suffix.length) : stem;
}

async function compareShot(crawl, file) {
  const known = join(crawl.config.baseline, file);
  if (!existsSync(known)) return null;
  const pair = { before: readFileSync(known).toString('base64'), after: readFileSync(join(crawl.out, 'screens', file)).toString('base64'), tolerance: visual.tolerance };
  const result = await inPage(crawl, pixelDiff, pair);
  if (!result) {
    problem(crawl, 'compare-failed', `screens/${file} could not be compared with the deployed version`);
    return null;
  }
  if (result.differ <= Math.max(visual.least, result.total * visual.share)) return true;
  const stem = stemOf(file);
  const screen = screenOf(stem);
  copyFileSync(known, join(crawl.out, 'visual', `${stem}-before.png`));
  if (result.diff) writeFileSync(join(crawl.out, 'visual', `${stem}-diff.png`), Buffer.from(result.diff, 'base64'));
  if (!crawl.config.accepted.has(screen)) {
    problem(crawl, 'looks-changed', `screens/${file}: ${result.differ} of ${result.total} pixels differ from the deployed version (visual/${stem}-diff.png); if intended, list «${screen}» with its task in .gate/visual.yaml`);
  }
  return false;
}

export async function compareShots(crawl) {
  if (!crawl.config.baseline) return;
  if (!existsSync(crawl.config.baseline)) return note(crawl, 'no-baseline', 'no shots of the deployed version yet: nothing to compare with');
  mkdirSync(join(crawl.out, 'visual'), { recursive: true });
  crawl.steps = ['compare with the deployed version'];
  await goto(crawl, 'about:blank').catch(() => null);
  const same = new Map();
  for (const file of readdirSync(join(crawl.out, 'screens')).filter((name) => name.endsWith('.png')).sort()) {
    const verdict = await compareShot(crawl, file);
    const screen = screenOf(stemOf(file));
    if (verdict !== null) same.set(screen, (same.get(screen) ?? true) && verdict);
  }
  for (const screen of [...crawl.config.accepted].filter((id) => same.get(id) === true)) {
    problem(crawl, 'visual-accept-stale', `«${screen}» is listed in .gate/visual.yaml, but every shot of it matches the deployed version: remove the line`);
  }
}
