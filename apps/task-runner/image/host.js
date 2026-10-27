'use strict';
// Runs inside the task container: loads the script mounted at /run/task/script.js,
// gives it a fresh headless Chromium page and writes its return value to
// /run/result/result.json. The script is untrusted input from the caller; this
// file only wires it up.
const fs = require('fs');
const { chromium } = require('playwright');

const log = (msg) => process.stderr.write(String(msg) + '\n');

async function main() {
  // Env is read once and never echoed: errors below print messages only.
  const params = JSON.parse(process.env.TASK_PARAMS_JSON || '{}');
  const secrets = JSON.parse(process.env.TASK_SECRETS_JSON || '{}');

  const mod = require('/run/task/script.js');
  const run = typeof mod === 'function' ? mod : mod && mod.run;
  if (typeof run !== 'function') {
    throw new Error('script must export run(ctx) (module.exports = {run} or module.exports = fn)');
  }

  const browser = await chromium.launch({ headless: true });
  try {
    const context = await browser.newContext();
    const page = await context.newPage();
    const result = await run({ page, params, secrets, log });
    const out = JSON.stringify(result === undefined ? null : result);
    fs.writeFileSync('/run/result/result.json', out);
  } finally {
    await browser.close();
  }
}

main().then(
  () => process.exit(0),
  (err) => {
    // One line, message only: the error object could carry request data.
    log('task failed: ' + String((err && err.message) || err).split('\n')[0]);
    process.exit(1);
  }
);
