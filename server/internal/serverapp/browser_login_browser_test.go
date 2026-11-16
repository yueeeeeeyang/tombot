//go:build browserintegration

package serverapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestDesktopBrowserLogin 验证真实登录、招聘、消息受理与持久化，不修改日常用户目录。
func TestDesktopBrowserLogin(t *testing.T) {
	module := os.Getenv("TOM_TEST_PLAYWRIGHT_MODULE")
	desktop := os.Getenv("TOM_TEST_DESKTOP_DIRECTORY")
	if !filepath.IsAbs(module) || !filepath.IsAbs(desktop) {
		t.Fatal("需要项目 Playwright 模块及已构建桌面目录的绝对路径")
	}
	fixture := newBrowserLoginFixture(t)
	// 消息受理需要开户时分配的配额；此测试不配置外部模型和电脑节点。
	if _, err := fixture.pool.Exec(`INSERT INTO tombot.user_limits(user_id,concurrent_model_calls,concurrent_desktops,queued_runs) SELECT id,1,1,10 FROM tombot.users WHERE username=$1`, fixture.username); err != nil {
		t.Fatal("准备用户配额失败", err)
	}
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
	certificateFingerprint := sha256.Sum256(leaf.Raw)
	input, err := json.Marshal(map[string]string{"screenshots": os.Getenv("TOM_TEST_SCREENSHOT_DIRECTORY"), "url": "https://" + fixture.running.address, "username": fixture.username, "password": fixture.password, "profile": t.TempDir(), "certificateFingerprint": hex.EncodeToString(certificateFingerprint[:]), "fingerprint": base64.StdEncoding.EncodeToString(fingerprint[:])})
	if err != nil {
		t.Fatal("测试输入无法编码")
	}
	defer clear(input)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	script := `
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { pathToFileURL } from 'node:url';
const { chromium, _electron } = await import(process.argv[1]);
const { default: electronPath } = await import(pathToFileURL(process.argv[2] + '/node_modules/electron/index.js').href);
const settings = JSON.parse(readFileSync(0, 'utf8'));
const env = { ...process.env };
delete env.NODE_EXTRA_CA_CERTS;
delete env.NODE_TLS_REJECT_UNAUTHORIZED;
delete env.ELECTRON_RUN_AS_NODE;
delete env.TOMBOT_SERVER_URL;
let application, browser, page;
const violations = [];
let approvalStatus = 0;
let hasInvalidOrigin = false, hasUnsafeReferrer = false;
let stage = '启动隔离客户端';
try {
  application = await _electron.launch({ executablePath: electronPath, args: [process.argv[2], '--user-data-dir=' + settings.profile], env, timeout: 15000 });
  let window = await application.firstWindow();
  window.setDefaultTimeout(10000);
  await application.evaluate(({ shell }) => { shell.openExternal = async address => { Reflect.set(globalThis, '__loginTestURL', address); }; });
  stage = '填写服务地址';
  await window.getByLabel('服务地址', { exact: true }).fill(settings.url);
  await window.getByRole('button', { name: '连接', exact: true }).click();
  stage = '点击浏览器登录';
  await window.getByRole('button', { name: '在浏览器中登录', exact: true }).click();
  stage = '核对自签名证书指纹';
  await window.getByLabel('证书 SHA-256 指纹', { exact: true }).waitFor();
  if (settings.screenshots) {
    await application.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].setContentSize(860, 640));
    for (const theme of ['light', 'dark']) {
      await window.evaluate(theme => { document.documentElement.dataset.theme = theme; }, theme);
      await window.screenshot({ path: join(settings.screenshots, 'certificate-' + theme + '.png'), animations: 'disabled' });
    }
  }
  await window.getByLabel('证书 SHA-256 指纹', { exact: true }).fill(settings.certificateFingerprint);
  await window.getByRole('button', { name: '验证并信任证书', exact: true }).click();
  await window.getByText('证书已验证并保存，可以继续登录。').waitFor();
  await window.getByRole('button', { name: '在浏览器中登录', exact: true }).click();
  let address;
  for (let attempt = 0; attempt < 100; attempt++) {
    address = await application.evaluate(() => Reflect.get(globalThis, '__loginTestURL'));
    if (address) break;
    await new Promise(resolve => setTimeout(resolve, 50));
  }
  if (!address) {
    const diagnostic = await application.evaluate(async (_, url) => {
      try { return String((await fetch(url + '/health/live')).status); }
      catch (error) { return error.cause?.code || '网络请求失败'; }
    }, settings.url);
    stage += '，HTTPS 检查：' + diagnostic;
    throw new Error();
  }
  if (new URL(address).pathname !== '/authorize') throw new Error();
  stage = '打开真实登录页';
  browser = await chromium.launch({ args: ['--ignore-certificate-errors-spki-list=' + settings.fingerprint] });
  page = await browser.newPage();
  page.setDefaultTimeout(10000);
  page.on('console', message => { if (message.text().includes('Content Security Policy')) violations.push('CSP'); });
  page.on('response', response => { if (['/api/v2/auth/browser/approve','/api/v2/web/auth/login'].includes(new URL(response.url()).pathname)) approvalStatus = response.status(); });
  page.on('request', request => {
    if (new URL(request.url()).pathname !== '/api/v2/auth/browser/approve') return;
    const headers = request.headers();
    hasInvalidOrigin ||= headers.origin !== settings.url;
    hasUnsafeReferrer ||= headers.referer !== settings.url + '/';
  });
  await page.goto(settings.url + '/login');
  stage = '核对登录页主题、品牌和窄屏布局';
  for (const theme of ['light', 'dark']) {
    await page.emulateMedia({ colorScheme: theme });
    for (const width of [960, 375]) {
      await page.setViewportSize({ width, height: 760 });
      await page.getByRole('heading', { name: 'TomBot 用户平台', exact: true }).waitFor();
      const appearance = await page.evaluate(() => ({
        background: getComputedStyle(document.body).backgroundColor,
        radius: getComputedStyle(document.querySelector('.login-card')).borderRadius,
        hasLogo: document.querySelector('.login-brand').naturalWidth > 0,
        hasOverflow: document.documentElement.scrollWidth > innerWidth,
      }));
      stage = '核对登录页外观 ' + theme + '/' + width + ' ' + JSON.stringify(appearance);
      if (!appearance.hasLogo || appearance.hasOverflow || appearance.radius !== '18px' ||
          appearance.background !== (theme === 'dark' ? 'rgb(17, 17, 17)' : 'rgb(253, 253, 253)')) throw new Error();
      if (settings.screenshots) await page.screenshot({ path: join(settings.screenshots, 'login-' + theme + '-' + width + '.png') });
    }
  }
  stage = '错误口令后重新填写登录表单';
  await page.getByLabel('用户名', { exact: true }).fill(settings.username);
  await page.getByLabel('密码', { exact: true }).fill(settings.password + '-invalid');
  await page.getByRole('button', { name: '登录', exact: true }).click();
  await page.getByRole('heading', { name: '登录失败', exact: true }).waitFor();
  if (approvalStatus !== 401 || await page.getByLabel('密码', { exact: true }).inputValue() !== '') throw new Error();
  stage = '登录普通用户网页并查看个人信息';
  await page.getByLabel('用户名', { exact: true }).fill(settings.username);
  await page.getByLabel('密码', { exact: true }).fill(settings.password);
  await page.getByRole('button', { name: '登录', exact: true }).click();
  await page.getByRole('heading', { name: '用量管理', exact: true }).waitFor();
  await page.getByLabel('账户菜单：浏览器登录用户（' + settings.username + '）', { exact: true }).click();
  await page.getByRole('link', { name: '个人信息', exact: true }).click();
  await page.getByRole('heading', { name: '个人信息', exact: true }).waitFor();
  for (const theme of ['light', 'dark']) {
    await page.emulateMedia({ colorScheme: theme });
    for (const width of [960, 375]) {
      await page.setViewportSize({ width, height: 850 });
      if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw new Error();
      if (settings.screenshots) await page.screenshot({ path: join(settings.screenshots, 'account-' + theme + '-' + width + '.png') });
    }
  }
  stage = '网页已登录时直接确认客户端授权';
  await page.goto(address);
  await page.getByRole('heading', { name: '授权 TomBot 客户端', exact: true }).waitFor();
  if (await page.getByLabel('密码', { exact: true }).count()) throw new Error();
  if (settings.screenshots) await page.screenshot({ path: join(settings.screenshots, 'authorization.png') });
  await page.getByRole('button', { name: '授权客户端', exact: true }).click();
  await page.getByRole('heading', { name: '授权成功', exact: true }).waitFor();
  if (new URL(page.url()).search !== '') throw new Error();
  stage = '验证自包含授权结果页主题与窄屏布局';
  for (const theme of ['light', 'dark']) {
    await page.emulateMedia({ colorScheme: theme });
    for (const width of [960, 375]) {
      await page.setViewportSize({ width, height: 760 });
      const appearance = await page.evaluate(() => ({
        background: getComputedStyle(document.body).backgroundColor,
        hasLogo: document.querySelector('.login-brand').naturalWidth > 0,
        hasOverflow: document.documentElement.scrollWidth > innerWidth,
      }));
      if (!appearance.hasLogo || appearance.hasOverflow || appearance.background !== (theme === 'dark' ? 'rgb(17, 17, 17)' : 'rgb(253, 253, 253)')) throw new Error();
      if (settings.screenshots) await page.screenshot({ path: join(settings.screenshots, 'result-' + theme + '-' + width + '.png') });
    }
  }
  if (violations.length || hasInvalidOrigin || hasUnsafeReferrer) throw new Error();
  stage = '确认客户端会话';
  await window.getByRole('heading', { name: '登录 TomBot' }).waitFor({ state: 'hidden' });
  const session = await window.evaluate(() => window.desktop.backend.session());
  if (session.phase !== 'ready' || session.username !== settings.username) throw new Error();
  stage = '确认登录后不再要求客户端配置模型';
  await window.getByRole('heading', { name: '招聘第一只猫咪', exact: true }).waitFor();
  if (await window.getByText('配置默认模型', { exact: true }).count() || await window.getByLabel('API 密钥', { exact: true }).count()) throw new Error();
  const hasModelConfiguration = await window.evaluate(() => ['loadSettings', 'saveSettings', 'listBuiltinProviders', 'listProviderModels'].some(key => key in window.desktop.backend));
  if (hasModelConfiguration) throw new Error();
  await window.reload();
  await window.getByRole('heading', { name: '招聘第一只猫咪', exact: true }).waitFor();
  if (settings.screenshots) await window.screenshot({ path: join(settings.screenshots, 'desktop-after-login.png') });
  stage = '真实招聘创建服务端猫咪和单聊';
  await window.getByRole('textbox', { name: '助手名称', exact: true }).fill('联调猫咪');
  await window.getByRole('button', { name: '招聘猫咪', exact: true }).click();
  await window.getByRole('textbox', { name: '给联调猫咪发消息', exact: true }).waitFor();
  stage = '刷新后恢复服务端猫咪';
  await window.reload();
  await window.getByRole('textbox', { name: '给联调猫咪发消息', exact: true }).waitFor();
  const snapshot = await window.evaluate(() => window.desktop.backend.loadWorkspace());
  if (snapshot.workspace.agents.length !== 1 || !/^[0-7][0-9A-HJKMNP-TV-Z]{25}$/.test(snapshot.workspace.activeAgentId)) throw new Error();
  stage = '从客户端发送消息并确认服务端受理';
  // 只观察服务端提交事件，不替换网络请求或制造回复。
  await application.evaluate(({ BrowserWindow }) => {
    const contents = BrowserWindow.getAllWindows()[0].webContents;
    const send = contents.send.bind(contents);
    contents.send = (channel, ...args) => {
      if (channel === 'desktop:user-event' && args[0]?.kind === 'message.created') Reflect.set(globalThis, '__acceptedRun', true);
      return send(channel, ...args);
    };
  });
  await window.getByRole('textbox', { name: '给联调猫咪发消息', exact: true }).fill('你好，验证消息入库');
  await window.getByRole('button', { name: '发送', exact: true }).click();
  let hasRun = false;
  for (let attempt = 0; attempt < 100; attempt++) {
    hasRun = await application.evaluate(() => Reflect.get(globalThis, '__acceptedRun') === true);
    if (hasRun) break;
    await new Promise(resolve => setTimeout(resolve, 50));
  }
  if (!hasRun) throw new Error();
  // 此夹具没有外部模型，运行会明确失败；在原订阅中确认终态，避免刷新丢过时通知。
  stage = '任务失败保留已发送消息且不阻止重开';
  await window.getByRole('alert').filter({ hasText: '消息已接收，但本次任务未完成' }).waitFor();
  if (await window.getByRole('button', { name: '重试保存', exact: true }).count()) throw new Error();
  if (await window.getByRole('button', { name: '未发送，点击重试', exact: true }).count()) throw new Error();
  const preventsClosing = await window.evaluate(() => {
    const closing = new Event('beforeunload', { cancelable: true });
    window.dispatchEvent(closing);
    return closing.defaultPrevented;
  });
  if (preventsClosing) throw new Error();
  await window.getByRole('button', { name: '关闭提示', exact: true }).click();
  await window.getByRole('alert').waitFor({ state: 'hidden' });
  if (settings.screenshots) await window.screenshot({ path: join(settings.screenshots, 'chat-before-reload.png') });
  stage = '刷新后自动加载历史';
  await window.reload();
  await window.getByRole('log').getByText('你好，验证消息入库', { exact: true }).waitFor();
  stage = '再次提交并恢复消息';
  await window.getByRole('textbox', { name: '给联调猫咪发消息', exact: true }).fill('补充：请简短回答');
  await window.getByRole('button', { name: '发送', exact: true }).click();
  await window.getByRole('log').getByText('补充：请简短回答', { exact: true }).waitFor();
  for (let attempt = 0; attempt < 100; attempt++) {
    const page = await window.evaluate(async (id) => window.desktop.backend.loadAgentConversation(id), snapshot.workspace.activeAgentId);
    if (page.messages.some(message => message.content === '补充：请简短回答')) break;
    if (attempt === 99) throw new Error();
    await new Promise(resolve => setTimeout(resolve, 50));
  }
  await window.reload();
  await window.getByRole('log').getByText('补充：请简短回答', { exact: true }).waitFor();
  stage = '退出并重启客户端后自动恢复登录及两条历史消息';
  await application.close();
  application = undefined;
  application = await _electron.launch({ executablePath: electronPath, args: [process.argv[2], '--user-data-dir=' + settings.profile], env, timeout: 15000 });
  window = await application.firstWindow();
  window.setDefaultTimeout(10000);
  await window.getByRole('log').getByText('你好，验证消息入库', { exact: true }).waitFor();
  await window.getByRole('log').getByText('补充：请简短回答', { exact: true }).waitFor();
  if (await window.getByRole('button', { name: '在浏览器中登录', exact: true }).count()) throw new Error();
  if (await window.getByText('机器人或群组参数无效', { exact: true }).count()) throw new Error();
  if (settings.screenshots) await window.screenshot({ path: join(settings.screenshots, 'desktop-message.png') });
  process.stdout.write('真实登录、招聘和发送操作已完成，继续核对数据库记录\n');
} catch {
  if (application && stage.includes('刷新后')) await application.firstWindow().then(window => window.screenshot({ path: join(settings.screenshots, 'chat-failure.png') }));
  process.stderr.write('浏览器联合验证失败：' + stage + '，授权响应=' + approvalStatus + '，CSP 拒绝数=' + violations.length + '\n');
  process.exitCode = 1;
} finally {
  if (browser) await browser.close();
  if (application) await application.close();
}
`
	command := exec.CommandContext(ctx, "node", "--input-type=module", "-e", script, module, desktop)
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("真实浏览器登录未通过：%v\n%s", err, output)
	}
	var bots, messages, runs int
	err = fixture.pool.QueryRow(`SELECT
        (SELECT count(*) FROM tombot.bots WHERE user_id=u.id AND name='联调猫咪'),
        (SELECT count(*) FROM tombot.messages m JOIN tombot.conversations c ON c.user_id=m.user_id AND c.id=m.conversation_id JOIN tombot.bots b ON b.user_id=c.user_id AND b.id=c.bot_id WHERE m.user_id=u.id AND b.name='联调猫咪' AND m.sender_kind='user' AND m.blocks @> '[{"kind":"text","text":"你好，验证消息入库"}]'),
        (SELECT count(*) FROM tombot.runs WHERE user_id=u.id)
        FROM tombot.users u WHERE u.username=$1`, fixture.username).Scan(&bots, &messages, &runs)
	if err != nil || bots != 1 || messages != 1 || runs != 2 {
		t.Fatalf("客户端创建或消息入库未通过：猫咪=%d 消息=%d 运行=%d 错误=%v", bots, messages, runs, err)
	}
	t.Log(string(output))
}
