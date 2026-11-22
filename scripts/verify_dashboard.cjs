// Run against a loopback gateway in mock mode. OAuth responses are local fixtures.
const assert = require('node:assert/strict');
const { chromium } = require('playwright-core');

async function main() {
  const port = Number(process.env.DASHBOARD_TEST_PORT || 18080);
  assert(Number.isInteger(port) && port > 0 && port < 65536);
  const origin = 'http://127.0.0.1:' + port;
  const browser = await chromium.launch({
    executablePath: process.env.CHROME_BIN,
    args: ['--disable-background-networking'],
  });
  try {
    const context = await browser.newContext({
      permissions: ['clipboard-read', 'clipboard-write'],
      viewport: { width: 1080, height: 900 },
    });
    const errors = [];
    await context.route('**/*', route => {
      if (new URL(route.request().url()).origin === origin) return route.continue();
      errors.push('unexpected external request');
      return route.abort();
    });
    const page = await context.newPage();
    page.on('pageerror', error => errors.push(error.message));
    const el = id => page.locator('#' + id);
    const columns = () => el('usage').evaluate(node => getComputedStyle(node).gridTemplateColumns.split(' ').length);
    const waitText = (id, text) => page.locator('#' + id).filter({ hasText: text }).waitFor();

    await page.goto(origin);
    await el('key').waitFor();
    await page.waitForFunction(() => document.querySelector('#key') === document.activeElement);
    assert.equal(await el('notice').isVisible(), false);
    await el('key').fill('wrong');
    await el('unlock').locator('button').click();
    await waitText('notice', 'API Key 无效');
    await el('key').fill('dashboard-fixture');
    await el('unlock').locator('button').click();
    await waitText('badge', 'Mock 模式');
    assert.equal(await columns(), 3);
    await page.getByRole('button', { name: 'Python', exact: true }).click();
    await el('copy').click();
    await waitText('feedback', '已复制 接入示例');
    assert.match(await page.evaluate(() => navigator.clipboard.readText()), /client.responses.create/);
    await el('models').locator('button').first().click();
    await waitText('feedback', '已复制 mock');
    assert.equal(await page.evaluate(() => navigator.clipboard.readText()), 'mock');
    await page.setViewportSize({ width: 375, height: 800 });
    assert.equal(await columns(), 2);
    assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));

    const status = await page.evaluate(async key => (await fetch('/admin/api/status', {
      headers: { Authorization: 'Bearer ' + key },
    })).json(), 'dashboard-fixture');
    status.mode = 'live';
    Object.assign(status.auth, { status: 'unauthenticated', email: '', auth_required: true });
    status.acp.alive = false;
    await page.route('**/admin/api/**', async route => {
      const path = new URL(route.request().url()).pathname;
      let result = status;
      if (path.endsWith('/start')) {
        status.auth.login_in_progress = true;
        result = { login_id: 'fixture-id', auth_url: 'https://accounts.google.com/o/oauth2/auth?state=fixture', expires_in_s: 2 };
      } else if (path.endsWith('/cancel')) {
        status.auth.login_in_progress = false;
        result = { ok: true };
      } else if (path.endsWith('/complete')) {
        assert.deepEqual(route.request().postDataJSON(), { login_id: 'fixture-id', code: 'fixture-code' });
        Object.assign(status.auth, { status: 'authenticated', email: 'test@example.invalid', auth_required: false, login_in_progress: false });
        status.acp.alive = true;
        result = { ok: true };
      }
      await route.fulfill({ json: result });
    });
    await el('refresh').click();
    await el('start').click();
    await waitText('countdown', '剩余有效时间');
    assert.match(await el('authlink').getAttribute('href'), /^https:\/\/accounts\.google\.com\//);
    await waitText('countdown', '已过期');
    await page.reload();
    await waitText('accountNote', '检测到进行中的登录流程');
    assert.equal(await el('flow').isVisible(), false);
    await el('cancel').click();
    await el('start').click();
    await el('code').fill('fixture-code');
    await el('complete').click();
    await waitText('account', 'test@example.invalid');
    await waitText('feedback', '登录成功');
    assert.equal(await el('code').inputValue(), '');
    await el('lock').click();
    assert.equal(await page.evaluate(() => sessionStorage.length), 0);
    assert.deepEqual(errors, []);
    console.log('Dashboard desktop, mobile, clipboard and OAuth fixture checks passed');
  } finally {
    await browser.close();
  }
}
main().catch(error => { console.error(error); process.exitCode = 1; });
