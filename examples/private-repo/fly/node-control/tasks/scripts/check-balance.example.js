'use strict';
// Example task script for node-control's tasks module (see apps/fly/node-control/README.md, "Tasks module").
//
// Contract: a CommonJS module that exports `run`. The runner calls it as
//   run({ page, params, secrets, log })
// where
//   page     a Playwright Page in a fresh headless Chromium context (nothing is shared between runs),
//   params   the `params` object of the task in tasks.json,
//   secrets  the values of the secrets the task lists in `secrets`, by name (here LOGIN and PASSWORD),
//   log      log(msg) writes a line to the run's log tail. Never log a secret; the runner scrubs known
//            secret values, but do not rely on it.
// It must return a small JSON object (at most 4096 bytes). Throw to fail the run; the message becomes
// the run's reason, so keep it free of secret values and of page content you do not want stored.
//
// Everything about the page below is a placeholder: example.com is not a real account page and every
// selector is site-specific. Adapt them to the page you check.
module.exports = {
  async run({ page, params, secrets, log }) {
    const url = params.account_url || 'https://example.com/account';

    await page.goto(url, { waitUntil: 'domcontentloaded', timeout: 60000 });

    // Log in. The selectors are placeholders.
    await page.fill('input[name="login"]', secrets.LOGIN);
    await page.fill('input[name="password"]', secrets.PASSWORD);
    await Promise.all([
      page.waitForLoadState('domcontentloaded'),
      page.click('button[type="submit"]'),
    ]);

    // Read the balance text, for example "Balance: 1 234,56". Placeholder selector.
    const text = await page.locator('#balance').innerText({ timeout: 30000 });
    log('balance element found');

    // Keep digits, one decimal separator and a sign; treat a comma as the decimal separator.
    const cleaned = text.replace(/\s+/g, '').replace(',', '.').replace(/[^0-9.-]/g, '');
    const balance = Number.parseFloat(cleaned);
    if (!Number.isFinite(balance)) {
      // Do not put the page text into the message: it may hold account data.
      throw new Error('balance text is not a number');
    }

    // The task's alert condition reads this field: { "when": { "field": "balance", "op": "<", "value": 10 } }
    return { balance };
  },
};
