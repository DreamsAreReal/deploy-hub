import { test } from 'node:test';
import assert from 'node:assert/strict';
import { configure } from './config.mjs';
import { run } from './walk.mjs';

const config = configure(process.env);

test('a robot opens every screen and presses every safe button', { skip: !config.ready && 'set E2E_BASE and E2E_APPMAP' }, async () => {
  const { crawl, summary } = await run(config);
  assert.equal(crawl.problems.length, 0, summary);
});
