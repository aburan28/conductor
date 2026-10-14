#!/usr/bin/env node
// Browser smoke test for the dashboard's simplified navigation and the admin area.
//
//   node scripts/ui-smoke.mjs http://127.0.0.1:8080 <org_admin token> [project]
//
// Needs Node and Playwright with Chromium (NODE_PATH=$(npm root -g) for a global install).
// It is not part of CI, which has no browser; internal/web's TestDashboardSmoke runs it
// against a throwaway server when both are present. It changes the organization's policy
// (a feature flag and the display name) and puts them back. Exit status 0 means every check
// passed; failures are listed.
import { createRequire } from 'module';

const require = createRequire(import.meta.url);
const { chromium } = require('playwright');

const [base, token, project = 'app'] = process.argv.slice(2);
if (!base || !token) {
  console.error('usage: node scripts/ui-smoke.mjs BASE_URL ORG_ADMIN_TOKEN [PROJECT]');
  process.exit(2);
}

const failures = [];
const check = (ok, what) => { console.log((ok ? 'ok   ' : 'FAIL ') + what); if (!ok) failures.push(what); };
const api = async (method, path, body) => {
  const res = await fetch(base + path, { method, headers: { Authorization: 'Bearer ' + token, 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body) });
  const text = await res.text();
  return { status: res.status, body: text ? JSON.parse(text) : null };
};

const browser = await chromium.launch();
const page = await (await browser.newContext({ viewport: { width: 1280, height: 900 } })).newPage();
const errors = [];
page.on('pageerror', e => errors.push(String(e)));
// A violation of the Content-Security-Policy is reported on the console.
page.on('console', m => { if (m.type() === 'error' && /Content Security Policy|Refused/.test(m.text())) errors.push(m.text()); });
const navLabels = () => page.$$eval('.nav a .label, .nav-toggle .label', els => els.map(e => e.textContent.trim()));

try {
  const before = (await api('GET', '/v1/admin/policy')).body;
  check(before && before.policy, 'the token administers its organization (GET /v1/admin/policy)');
  const swarmWas = before.policy.features && before.policy.features.swarm;
  const nameWas = (before.policy.branding && before.policy.branding.display_name) || '';

  await page.goto(`${base}/#token=${token}&project=${project}`);
  await page.waitForSelector('.nav a[data-name="home"]');
  await page.waitForTimeout(800);
  const labels = await navLabels();
  for (const want of ['Home', 'Tasks', 'People', 'More', 'Settings', 'Admin']) check(labels.includes(want), `the menu has ${want}`);
  for (const gone of ['Fleet', 'Queue', 'Usage', 'Events']) check(!(await page.isVisible(`.nav a[data-name="${gone.toLowerCase()}"]`)), `${gone} is folded under More`);

  await page.click('.nav-toggle');
  check(await page.isVisible('.nav-more a[data-name="fleet"]'), 'More opens and lists Fleet');

  // Home: the check-before-edit form answers.
  await page.fill('#check-summary', 'ui smoke check');
  await page.fill('#check-paths', 'scripts/ui-smoke-nobody-holds-this.txt');
  await page.click('.check-form button[type=submit]');
  await page.waitForSelector('.check-result .notice', { timeout: 5000 });
  check(/Clear|already/.test(await page.textContent('.check-result')), 'Home\'s "Can I start?" check answers');

  // Old addresses keep working.
  await page.goto(`${base}/sessions`);
  await page.waitForTimeout(800);
  check(new URL(page.url()).pathname === '/people', '/sessions redirects to /people');
  await page.goto(`${base}/#/tasks`);
  await page.waitForTimeout(800);
  check(new URL(page.url()).pathname === '/tasks', '#/tasks lands on /tasks');
  await page.goto(`${base}/queue`);
  await page.waitForTimeout(800);
  check((await page.textContent('#page-title')) === 'Queue', 'a feature-flagged area keeps its address');

  // Admin → Features: turning Swarm on puts it in the menu.
  await page.goto(`${base}/admin/features`);
  await page.waitForSelector('.features input[type=checkbox]');
  const swarmBox = page.locator('.features li', { hasText: 'Swarm' }).locator('input[type=checkbox]');
  if (!(await swarmBox.isChecked())) {
    await swarmBox.check();
    await page.waitForTimeout(1200);
  }
  await page.click('.nav-toggle').catch(() => {});
  await page.waitForTimeout(300);
  check(await page.locator('.nav-more a[data-name="swarm"]').count() === 1, 'Admin → Features turns Swarm on in the menu');

  // Admin → Organization: the display name brands the sidebar.
  await page.goto(`${base}/admin/organization`);
  const nameInput = page.getByLabel('Display name');
  const nameEditable = await nameInput.isEnabled();
  if (nameEditable) {
    await nameInput.fill('Smoke Test Co');
    await page.click('text=Save branding');
    await page.waitForTimeout(1200);
    check((await page.textContent('.brand .name')) === 'Smoke Test Co', 'the display name brands the sidebar');
  } else {
    check(await page.isVisible('text=Managed by your administrator\'s config file'), 'a locked display name says the config file manages it');
  }

  for (const section of ['authentication', 'provisioning', 'members', 'audit', 'configuration']) {
    await page.goto(`${base}/admin/${section}`);
    await page.waitForTimeout(900);
    check(!(await page.isVisible('.error-box')), `Admin → ${section} renders`);
    if (section === 'audit') check(await page.locator('table').first().isVisible(), 'the audit log lists records');
  }

  // Restore what this script changed.
  const restore = { features: { swarm: !!swarmWas } };
  if (nameEditable) restore.branding = { display_name: nameWas };
  await api('PATCH', '/v1/admin/policy', restore);

  // Demo mode still works without a server.
  await page.goto(`${base}/?demo=1`);
  await page.waitForTimeout(1200);
  check(await page.isVisible('.demo-badge'), 'demo mode loads');
} catch (err) {
  failures.push(String(err));
  console.log('FAIL ' + err);
}
check(errors.length === 0, 'no script errors or CSP violations' + (errors.length ? ': ' + errors.join(' | ') : ''));
await browser.close();
if (failures.length) {
  console.log(`\n${failures.length} check(s) failed`);
  process.exit(1);
}
console.log('\nall checks passed');
