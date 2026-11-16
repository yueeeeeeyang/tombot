//go:build browserintegration

package serverapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestUserWebPlatformBrowser 不启动桌面客户端或创建授权请求，验证独立网页的完整用户路径。
func TestUserWebPlatformBrowser(t *testing.T) {
	module := os.Getenv("TOM_TEST_PLAYWRIGHT_MODULE")
	if !filepath.IsAbs(module) {
		t.Fatal("需要项目 Playwright 模块的绝对路径")
	}
	fixture := newBrowserLoginFixture(t)
	certificate, err := os.ReadFile(fixture.certificateFile)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(certificate)
	if block == nil {
		t.Fatal("测试证书格式无效")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	input, err := json.Marshal(map[string]string{
		"url": "https://" + fixture.running.address, "username": fixture.username, "password": fixture.password,
		"fingerprint": base64.StdEncoding.EncodeToString(fingerprint[:]), "screenshots": os.Getenv("TOM_TEST_SCREENSHOT_DIRECTORY"),
	})
	if err != nil {
		t.Fatal("测试输入无法编码")
	}
	defer clear(input)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	script := `
import { readFileSync, mkdirSync } from 'node:fs';
import { join } from 'node:path';
const { chromium } = await import(process.argv[1]);
const settings = JSON.parse(readFileSync(0, 'utf8'));
if (settings.screenshots) mkdirSync(settings.screenshots, { recursive: true });
let browser;
let stage = '直接访问服务根地址';
try {
  browser = await chromium.launch({ args: ['--ignore-certificate-errors-spki-list=' + settings.fingerprint] });
  const context = await browser.newContext();
  const page = await context.newPage();
  page.setDefaultTimeout(10000);
  const problems = [];
  page.on('pageerror', () => problems.push('页面脚本错误'));
  page.on('console', message => { if (message.text().includes('Content Security Policy')) problems.push('内容策略拒绝'); });
  await page.goto(settings.url + '/');
  await page.getByRole('heading', { name: 'TomBot 用户平台', exact: true }).waitFor();
  if (new URL(page.url()).pathname !== '/login') throw new Error();
  if (await page.getByLabel('动态验证码', { exact: true }).count()) throw new Error();
  const loginForm = page.locator('form');
  if (await loginForm.getAttribute('action') !== '/api/v2/web/auth/login') throw new Error();
  for (const theme of ['light', 'dark']) {
    await page.emulateMedia({ colorScheme: theme });
    for (const width of [1280, 375]) {
      await page.setViewportSize({ width, height: 900 });
      if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw new Error();
      if (settings.screenshots) await page.screenshot({ path: join(settings.screenshots, 'web-login-' + theme + '-' + width + '.png') });
    }
  }
  stage = '错误密码后可重新登录';
  await page.getByLabel('用户名', { exact: true }).fill(settings.username);
  await page.getByLabel('密码', { exact: true }).fill(settings.password + '-invalid');
  await page.getByRole('button', { name: '登录', exact: true }).click();
  await page.getByRole('heading', { name: '登录失败', exact: true }).waitFor();
  if (await page.getByLabel('密码', { exact: true }).inputValue() !== '') throw new Error();
  await page.getByLabel('用户名', { exact: true }).fill(settings.username);
  await page.getByLabel('密码', { exact: true }).fill(settings.password);
  await page.getByRole('button', { name: '登录', exact: true }).click();
  await page.getByRole('heading', { name: '用量管理', exact: true }).waitFor();
  if (new URL(page.url()).pathname !== '/') throw new Error();
  stage = '核对侧栏、用户信息与用量占位';
  if (await page.getByRole('navigation', { name: '用户导航' }).getByRole('link').count() !== 1) throw new Error();
  await page.getByRole('heading', { name: '用量管理即将上线', exact: true }).waitFor();
  const account = page.getByLabel('账户菜单：浏览器登录用户（' + settings.username + '）', { exact: true });
  for (const theme of ['light', 'dark']) {
    await page.emulateMedia({ colorScheme: theme });
    for (const width of [1280, 375]) {
      await page.setViewportSize({ width, height: 900 });
      const layout = await page.evaluate(() => {
        const sidebar = document.querySelector('.navigation').getBoundingClientRect();
        const main = document.querySelector('main').getBoundingClientRect();
        const footer = document.querySelector('.navigation-footer').getBoundingClientRect();
        return { sidebarWidth: sidebar.width, mainLeft: main.left, footerBottom: footer.bottom, background: getComputedStyle(document.body).backgroundColor, overflow: document.documentElement.scrollWidth > innerWidth };
      });
      if (layout.overflow || layout.mainLeft < layout.sidebarWidth || layout.footerBottom < 850 || layout.footerBottom > 900 || layout.background !== (theme === 'dark' ? 'rgb(17, 17, 17)' : 'rgb(253, 253, 253)')) throw new Error();
      if (settings.screenshots) await page.screenshot({ path: join(settings.screenshots, 'web-usage-' + theme + '-' + width + '.png') });
    }
  }
  stage = '键盘打开账户菜单并进入个人信息';
  await account.focus();
  await page.keyboard.press('Enter');
  const profile = page.getByRole('link', { name: '个人信息', exact: true });
  await profile.waitFor();
  await profile.click();
  await page.getByRole('heading', { name: '个人信息', exact: true }).waitFor();
  if (new URL(page.url()).pathname !== '/account') throw new Error();
  if (!(await page.getByRole('region', { name: '账户资料' }).innerText()).includes(settings.username)) throw new Error();
  for (const theme of ['light', 'dark']) {
    await page.emulateMedia({ colorScheme: theme });
    for (const width of [1280, 375]) {
      await page.setViewportSize({ width, height: 900 });
      if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw new Error();
      if (settings.screenshots) await page.screenshot({ path: join(settings.screenshots, 'web-profile-' + theme + '-' + width + '.png') });
    }
  }
  stage = '刷新个人信息并通过侧栏返回用量管理';
  await page.reload();
  await page.getByRole('heading', { name: '个人信息', exact: true }).waitFor();
  await page.getByRole('navigation', { name: '用户导航' }).getByRole('link', { name: '用量管理', exact: true }).click();
  await page.getByRole('heading', { name: '用量管理', exact: true }).waitFor();
  await page.reload();
  await page.getByRole('heading', { name: '用量管理', exact: true }).waitFor();
  await page.goto(settings.url + '/login');
  await page.getByRole('heading', { name: '用量管理', exact: true }).waitFor();
  stage = '退出后平台与个人信息均要求登录';
  await account.click();
  await page.getByRole('button', { name: '退出登录', exact: true }).click();
  await page.getByRole('heading', { name: 'TomBot 用户平台', exact: true }).waitFor();
  for (const path of ['/', '/account']) {
    await page.goto(settings.url + path);
    await page.getByRole('heading', { name: 'TomBot 用户平台', exact: true }).waitFor();
    if (new URL(page.url()).pathname !== '/login') throw new Error();
  }
  if (problems.length) throw new Error();
  process.stdout.write('独立网页登录、平台布局、个人信息、刷新恢复和退出均已通过\n');
} catch {
  process.stderr.write('独立用户平台验证失败：' + stage + '\n');
  process.exitCode = 1;
} finally {
  if (browser) await browser.close();
}
`
	command := exec.CommandContext(ctx, "node", "--input-type=module", "-e", script, module)
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("独立用户平台浏览器测试失败：%v\n%s", err, output)
	}
	var authorizations int
	if err := fixture.pool.QueryRow(`SELECT count(*) FROM tombot.desktop_authorizations`).Scan(&authorizations); err != nil || authorizations != 0 {
		t.Fatal("独立网页登录不应创建桌面授权", err)
	}
	t.Log(string(output))
}
