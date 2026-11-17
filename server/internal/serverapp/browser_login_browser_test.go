//go:build browserintegration

package serverapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io.yueyang/tom/internal/scheduledtasks"
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
	seeded := make(chan error, 1)
	go func() {
		timer := time.NewTicker(20 * time.Millisecond)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				seeded <- ctx.Err()
				return
			case <-timer.C:
			}
			var user, bot string
			err := fixture.pool.QueryRowContext(ctx, `SELECT b.user_id,b.id FROM tombot.bots b JOIN tombot.users u ON u.id=b.user_id WHERE u.username=$1 AND b.name='侧栏猫咪'`, fixture.username).Scan(&user, &bot)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				seeded <- err
				return
			}
			tx, err := fixture.pool.BeginTx(ctx, nil)
			if err != nil {
				seeded <- err
				return
			}
			_, err = scheduledtasks.Save(ctx, tx, user, bot, "", 0, scheduledtasks.Definition{Name: "工作日报", Prompt: "总结今天完成的工作，列出明天的重点。", Cron: "0 18 * * *", Timezone: "Asia/Shanghai", IsEnabled: true})
			if err != nil {
				seeded <- errors.Join(err, tx.Rollback())
				return
			}
			seeded <- tx.Commit()
			return
		}
	}()
	defer func() {
		cancel()
		if err := <-seeded; err != nil && !errors.Is(err, context.Canceled) {
			t.Error("准备真实任务卡片失败", err)
		}
	}()
	script := `
import { readFileSync, mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
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
  const verifyComposerCards = async (expectedCount) => {
    const fits = await window.evaluate(expectedCount => {
      const area = document.querySelector('.composer-area');
      const input = area.querySelector('.composer').getBoundingClientRect();
      const cards = [...area.querySelectorAll('.error-banner, .composer-reply')];
      return cards.length === expectedCount && document.documentElement.scrollWidth <= innerWidth && cards.every((element, index) => {
        const card = element.getBoundingClientRect();
        const next = cards[index + 1]?.getBoundingClientRect() ?? input;
        const close = element.querySelector('button')?.getBoundingClientRect();
        return Math.abs(card.left - input.left) <= 1 && Math.abs(card.right - input.right) <= 1 && Math.abs(next.top - card.bottom - 6) <= 1 && card.right <= innerWidth && (!close || close.left >= card.left && close.right <= card.right) && element.scrollWidth <= element.clientWidth;
      });
    }, expectedCount);
    if (!fits) throw new Error('提示或回复卡片未与输入框对齐，或间距与关闭按钮越界');
  };

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
  stage = '客户端读取本人昵称并在网页修改资料后同步';
  await window.getByRole('button', { name: '用户信息', exact: true }).getByText('浏览器登录用户', { exact: true }).waitFor();
  stage = '网页修改用户昵称';
  await application.evaluate(({ BrowserWindow }) => {
    const main = BrowserWindow.getAllWindows()[0];
    main.profileRefreshEvents = { focus: 0, show: 0, hide: 0 };
    for (const name of Object.keys(main.profileRefreshEvents)) main.on(name, () => { main.profileRefreshEvents[name]++; });
  });
  await application.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].hide());
  if (await application.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0].isVisible())) throw new Error('测试窗口没有隐藏');
  await page.bringToFront();
  await page.goto(settings.url + '/account');
  await page.getByRole('button', { name: '编辑昵称', exact: true }).click();
  await page.getByRole('textbox', { name: '昵称', exact: true }).fill('桌面资料联调用户');
  await page.getByRole('textbox', { name: '昵称', exact: true }).press('Enter');
  await page.getByRole('heading', { name: '桌面资料联调用户', exact: true }).waitFor();
  stage = '网页裁剪并更换用户头像';
  const avatarPNG = await page.evaluate(() => {
    const canvas = document.createElement('canvas');
    canvas.width = canvas.height = 64;
    const context = canvas.getContext('2d');
    context.fillStyle = '#257956'; context.fillRect(0, 0, 64, 64);
    return canvas.toDataURL('image/png').split(',')[1];
  });
  await page.getByLabel('选择头像图片', { exact: true }).setInputFiles({ name: 'profile.png', mimeType: 'image/png', buffer: Buffer.from(avatarPNG, 'base64') });
  const crop = page.getByRole('dialog', { name: '裁剪头像', exact: true });
  await crop.getByText('点击“确认更换”立即保存头像。', { exact: true }).waitFor();
  await crop.getByRole('button', { name: '确认更换', exact: true }).click();
  await crop.waitFor({ state: 'hidden' });
  stage = '切回客户端同步用户资料';
  await application.evaluate(({ app, BrowserWindow }) => { const main = BrowserWindow.getAllWindows()[0]; main.show(); app.focus({ steal: true }); main.focus(); });
  await window.getByRole('button', { name: '用户信息', exact: true }).getByText('桌面资料联调用户', { exact: true }).waitFor();
  stage = '客户端头像与服务端资料一致';
  const profileMatches = await window.evaluate(async () => {
    const profile = await window.desktop.backend.loadUserProfile();
    const avatar = document.querySelector('.profile-avatar img');
    return profile.avatar.startsWith('data:image/png;base64,') && avatar.getAttribute('src') === profile.avatar && avatar.complete && avatar.naturalWidth > 0;
  });
  if (!profileMatches) throw new Error('客户端头像与本人资料不一致');
  if (settings.screenshots) await window.screenshot({ path: join(settings.screenshots, 'desktop-user-profile.png') });
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
  stage = '错误提示浅深色与窄窗口布局';
  for (const width of [1073, 860]) {
    await application.evaluate(({ BrowserWindow }, width) => BrowserWindow.getAllWindows()[0].setContentSize(width, 780), width);
    for (const theme of ['light', 'dark']) {
      await window.evaluate(theme => { document.documentElement.dataset.theme = theme; }, theme);
      await verifyComposerCards(1);
      if (settings.screenshots) await window.screenshot({ path: join(settings.screenshots, 'error-banner-' + theme + '-' + width + '.png'), animations: 'disabled' });
    }
  }
  stage = '错误与回复并存及资料栏布局';
  await window.getByRole('log').locator('.message-text').filter({ hasText: /^你好，验证消息入库$/ }).click({ button: 'right' });
  await window.getByRole('menuitem', { name: '回复', exact: true }).click();
  await window.getByRole('button', { name: '查看联调猫咪资料', exact: true }).click();
  for (const width of [1073, 860]) {
    await application.evaluate(({ BrowserWindow }, width) => BrowserWindow.getAllWindows()[0].setContentSize(width, 780), width);
    for (const theme of ['light', 'dark']) {
      await window.evaluate(theme => { document.documentElement.dataset.theme = theme; }, theme);
      await verifyComposerCards(2);
      if (settings.screenshots) await window.screenshot({ path: join(settings.screenshots, 'composer-cards-' + theme + '-' + width + '.png'), animations: 'disabled' });
    }
  }
  await window.getByRole('button', { name: '关闭猫咪面板', exact: true }).click();
  await verifyComposerCards(2);
  await window.evaluate(() => { document.documentElement.dataset.theme = 'light'; });
  if (settings.screenshots) await window.locator('.composer-area').screenshot({ path: join(settings.screenshots, 'composer-cards-preview.png'), animations: 'disabled' });
  await window.getByRole('button', { name: '关闭提示', exact: true }).click();
  await window.getByRole('alert').waitFor({ state: 'hidden' });
  stage = '关闭提示后保留回复，多行输入仍然对齐';
  await window.getByRole('textbox', { name: '给联调猫咪发消息', exact: true }).fill('第一行\n第二行\n第三行');
  await verifyComposerCards(1);
  if (settings.screenshots) await window.locator('.composer-area').screenshot({ path: join(settings.screenshots, 'reply-multiline-preview.png'), animations: 'disabled' });
  await window.getByRole('button', { name: '取消回复', exact: true }).click();
  if (await window.getByRole('textbox', { name: '给联调猫咪发消息', exact: true }).inputValue() !== '第一行\n第二行\n第三行') throw new Error('取消回复丢失了草稿');
  await window.getByRole('textbox', { name: '给联调猫咪发消息', exact: true }).fill('');
  if (settings.screenshots) await window.screenshot({ path: join(settings.screenshots, 'chat-before-reload.png') });
  stage = '刷新后自动加载历史';
  await window.reload();
  await window.getByRole('log').locator('.message-text').filter({ hasText: /^你好，验证消息入库$/ }).waitFor();
  stage = '再次提交并恢复消息';
  await window.getByRole('textbox', { name: '给联调猫咪发消息', exact: true }).fill('补充：请简短回答');
  // 观察真实发送过程：回执、工作事件和历史同步均不能移除原来的消息节点。
  await window.evaluate(() => {
    const log = document.querySelector('.saved-messages');
    const viewport = document.querySelector('.messages-scroll');
    const history = [...log.querySelectorAll('.message-entry')];
    // 提示卡片会占用真实输入区高度；扣除这部分后，消息与工作事件仍不能改变视口。
    const availableHeight = () => {
      const banner = document.querySelector('.error-banner');
      return viewport.getBoundingClientRect().height + (banner ? banner.getBoundingClientRect().height + parseFloat(getComputedStyle(banner).marginBottom) : 0);
    };
    const height = availableHeight();
    const state = { outgoing: null, isStable: true, height };
    const observer = new MutationObserver(() => {
      const outgoing = [...log.querySelectorAll('.message-entry')].find(entry => entry.textContent.includes('补充：请简短回答'));
      if (outgoing && !state.outgoing) state.outgoing = outgoing;
      if (history.some(entry => !entry.isConnected) || (state.outgoing && outgoing !== state.outgoing)) state.isStable = false;
      if (Math.abs(availableHeight() - height) > 1) state.isStable = false;
    });
    observer.observe(document.querySelector('.chat-panel'), { subtree: true, childList: true });
    Reflect.set(window, '__timelineCheck', { state, observer });
  });
  await window.getByRole('button', { name: '发送', exact: true }).click();
  await window.getByRole('log').getByText('补充：请简短回答', { exact: true }).waitFor();
  for (let attempt = 0; attempt < 100; attempt++) {
    const page = await window.evaluate(async (id) => window.desktop.backend.loadAgentConversation(id), snapshot.workspace.activeAgentId);
    if (page.messages.some(message => message.content === '补充：请简短回答')) break;
    if (attempt === 99) throw new Error();
    await new Promise(resolve => setTimeout(resolve, 50));
  }
  await window.getByLabel('发送中', { exact: true }).waitFor({ state: 'hidden' });
  const stableTimeline = await window.evaluate(() => {
    const { state, observer } = Reflect.get(window, '__timelineCheck');
    observer.disconnect();
    return state.isStable && state.outgoing?.isConnected;
  });
  if (!stableTimeline) throw new Error('发送期间消息节点或视口发生跳变');
  await window.reload();
  await window.getByRole('log').getByText('补充：请简短回答', { exact: true }).waitFor();
  stage = '回复消息预览与真实发送';
  await window.getByRole('log').locator('.message-text').filter({ hasText: /^你好，验证消息入库$/ }).click({ button: 'right' });
  await window.getByRole('menuitem', { name: '回复', exact: true }).click();
  await window.getByRole('region', { name: '正在回复自己', exact: true }).waitFor();
  await window.getByRole('textbox', { name: '给联调猫咪发消息', exact: true }).fill('这是对第一条消息的回复');
  for (const theme of ['light', 'dark']) {
    await window.evaluate(theme => { document.documentElement.dataset.theme = theme; }, theme);
    await verifyComposerCards(await window.locator('.error-banner').count() + 1);
    if (settings.screenshots) await window.screenshot({ path: join(settings.screenshots, 'reply-composer-' + theme + '.png'), animations: 'disabled' });
  }
  await window.getByRole('button', { name: '发送', exact: true }).click();
  await window.getByRole('button', { name: '查看原消息：你好，验证消息入库', exact: true }).waitFor();
  for (let attempt = 0; attempt < 100; attempt++) {
    const history = await window.evaluate(async (id) => window.desktop.backend.loadAgentConversation(id), snapshot.workspace.activeAgentId);
    const reply = history.messages.find(message => message.content === '这是对第一条消息的回复');
    if (reply?.replyTo?.excerpt === '你好，验证消息入库') break;
    if (attempt === 99) throw new Error('服务端未保存引用摘要');
    await new Promise(resolve => setTimeout(resolve, 50));
  }
  await window.getByLabel('发送中', { exact: true }).waitFor({ state: 'hidden' });
  await window.reload();
  await window.getByRole('button', { name: '查看原消息：你好，验证消息入库', exact: true }).waitFor();
  const waitForIdle = async () => {
    for (let attempt = 0; attempt < 100; attempt++) {
      const history = await window.evaluate(async id => window.desktop.backend.loadAgentConversation(id), snapshot.workspace.activeAgentId);
      if (history.runs.length === 0) return history;
      await new Promise(resolve => setTimeout(resolve, 50));
    }
    throw new Error('服务端任务未收尾');
  };
  await waitForIdle();
  stage = '真实表情发送、刷新与重启恢复';
  await window.getByRole('log').locator('.message-text').filter({ hasText: /^你好，验证消息入库$/ }).click({ button: 'right' });
  await window.getByRole('button', { name: '回应👍', exact: true }).click();
  await window.getByRole('button', { name: '👍，点击取消', exact: true }).waitFor();
  const marked = await waitForIdle();
  if (marked.messages.length !== 3 || !marked.messages.some(message => message.content === '你好，验证消息入库' && message.reactionRevision === 1 && message.reactions.some(reaction => reaction.emoji === '👍'))) throw new Error('表情未持久化或显示了内部交互');
  await window.reload();
  await window.getByRole('button', { name: '👍，点击取消', exact: true }).waitFor();
  stage = '退出并重启客户端后自动恢复登录、历史、引用和表情';
  await application.close();
  application = undefined;
  application = await _electron.launch({ executablePath: electronPath, args: [process.argv[2], '--user-data-dir=' + settings.profile], env, timeout: 15000 });
  window = await application.firstWindow();
  window.setDefaultTimeout(10000);
  await window.getByRole('log').locator('.message-text').filter({ hasText: /^你好，验证消息入库$/ }).waitFor();
  await window.getByRole('log').getByText('补充：请简短回答', { exact: true }).waitFor();
  await window.getByRole('button', { name: '👍，点击取消', exact: true }).waitFor();
  const quote = window.getByRole('button', { name: '查看原消息：你好，验证消息入库', exact: true });
  await quote.waitFor();
  await quote.click();
  if (!await window.locator('.message-line.is-highlighted').count()) throw new Error('引用无法定位原消息');
  for (const width of [1073, 860]) {
    await application.evaluate(({ BrowserWindow }, width) => BrowserWindow.getAllWindows()[0].setContentSize(width, 780), width);
    for (const theme of ['light', 'dark']) {
      await window.evaluate(theme => { document.documentElement.dataset.theme = theme; }, theme);
      const layout = await quote.evaluate(element => ({ width: element.getBoundingClientRect().width, overflow: document.documentElement.scrollWidth > innerWidth }));
      if (layout.overflow || layout.width > 440) throw new Error('引用气泡超出可读宽度');
      if (settings.screenshots) await window.screenshot({ path: join(settings.screenshots, 'reply-bubble-' + theme + '-' + width + '.png'), animations: 'disabled' });
    }
  }
  if (await window.getByRole('button', { name: '在浏览器中登录', exact: true }).count()) throw new Error();
  await window.getByRole('button', { name: '用户信息', exact: true }).getByText('桌面资料联调用户', { exact: true }).waitFor();
  if (await window.getByText('机器人或群组参数无效', { exact: true }).count()) throw new Error();
  if (settings.screenshots) await window.screenshot({ path: join(settings.screenshots, 'desktop-message.png') });
  stage = '取消表情后刷新仍保持撤销';
  await window.getByRole('button', { name: '👍，点击取消', exact: true }).click();
  await window.getByRole('button', { name: '👍，点击取消', exact: true }).waitFor({ state: 'hidden' });
  const removed = await waitForIdle();
  if (removed.messages.length !== 3 || !removed.messages.some(message => message.content === '你好，验证消息入库' && message.reactionRevision === 2 && message.reactions.length === 0)) throw new Error('取消表情没有落库');
  await window.reload();
  await window.getByRole('log').getByText('补充：请简短回答', { exact: true }).waitFor();
  if (await window.getByRole('button', { name: '👍，点击取消', exact: true }).count()) throw new Error('重载恢复了已取消的表情');
  stage = '侧栏选中留白与分组悬停';
  for (const name of ['侧栏猫咪', '侧栏伙伴']) {
    await window.getByRole('button', { name: '新建', exact: true }).click();
    await window.getByRole('menuitem', { name: '招聘猫咪', exact: true }).click();
    await window.getByRole('textbox', { name: '助手名称', exact: true }).fill(name);
    await window.getByRole('button', { name: '招聘猫咪', exact: true }).click();
    await window.getByRole('textbox', { name: '给' + name + '发消息', exact: true }).waitFor();
  }
  await window.getByRole('button', { name: '侧栏猫咪', exact: true }).click();
  const sidebarInput = window.getByRole('textbox', { name: '给侧栏猫咪发消息', exact: true });
  await sidebarInput.waitFor();
  const groupToggle = window.locator('.group-toggle');
  const selectedCat = window.locator('.agent-button.is-active');
  await selectedCat.waitFor();
  for (const theme of ['light', 'dark']) {
    await window.evaluate(theme => { document.documentElement.dataset.theme = theme; }, theme);
    for (const width of [300, 400]) {
      await application.evaluate(({ BrowserWindow }, width) => BrowserWindow.getAllWindows()[0].setContentSize(width === 300 ? 1073 : 860, width === 300 ? 780 : 640), width);
      if (width === 400) await window.getByRole('separator', { name: '调整侧栏宽度' }).press('End');
      await sidebarInput.focus();
      await window.mouse.move(700, 400);
      await window.waitForFunction(() => getComputedStyle(document.querySelector('.group-toggle > svg')).opacity === '0');
      const aligned = await selectedCat.evaluate(element => {
        const sidebar = element.closest('.sidebar');
        const bounds = sidebar.getBoundingClientRect();
        const row = element.getBoundingClientRect();
        const left = row.left - bounds.left;
        const right = bounds.right - parseFloat(getComputedStyle(sidebar).borderRightWidth) - row.right;
        const group = element.closest('.agent-group');
        const header = group.querySelector('.group-toggle').getBoundingClientRect();
        const label = group.querySelector('.group-name').getBoundingClientRect();
        const rows = [...group.querySelectorAll('.agent-button')].map(row => row.getBoundingClientRect());
        const avatar = group.querySelector('.agent-button .avatar').getBoundingClientRect();
        return left > 0 && Math.abs(left - right) <= 1 && Math.abs(label.left - avatar.left) <= 1 && rows.length === 2 && Math.abs(rows[0].top - header.bottom - 5) <= 1 && Math.abs(rows[1].top - rows[0].bottom - 5) <= 1;
      });
      if (!aligned) throw new Error('侧栏留白、标题头像对齐或分组条目间距不一致');
      const sidebarBounds = await window.locator('.sidebar').boundingBox();
      const groupBounds = await window.locator('.agent-group').boundingBox();
      const sidebarClip = { x: sidebarBounds.x, y: groupBounds.y - 4, width: sidebarBounds.width, height: groupBounds.height + 8 };
      if (settings.screenshots) await window.screenshot({ clip: sidebarClip, path: join(settings.screenshots, 'sidebar-idle-' + theme + '-' + width + '.png'), animations: 'disabled' });
      const restingBackground = await groupToggle.evaluate(element => getComputedStyle(element).backgroundColor);
      await groupToggle.hover();
      await window.waitForFunction(() => getComputedStyle(document.querySelector('.group-toggle > svg')).opacity === '1');
      const hovered = await groupToggle.evaluate(element => ({
        background: getComputedStyle(element).backgroundColor,
        arrowRight: element.querySelector('svg').getBoundingClientRect().left >= element.querySelector('span').getBoundingClientRect().right,
        overflow: element.scrollWidth > element.clientWidth,
      }));
      if (hovered.background === restingBackground || !hovered.arrowRight || hovered.overflow) throw new Error('分组悬停样式或右侧箭头不正确');
      if (settings.screenshots) await window.screenshot({ clip: sidebarClip, path: join(settings.screenshots, 'sidebar-hover-' + theme + '-' + width + '.png'), animations: 'disabled' });
    }
    await window.reload();
    await sidebarInput.waitFor();
  }
  stage = '分组键盘操作与焦点提示';
  await window.mouse.move(700, 400);
  await window.keyboard.press('Tab');
  await groupToggle.focus();
  await window.waitForFunction(() => document.querySelector('.group-toggle').matches(':focus-visible') && getComputedStyle(document.querySelector('.group-toggle > svg')).opacity === '1');
  await window.keyboard.press('Enter');
  if (await groupToggle.getAttribute('aria-expanded') !== 'false' || await selectedCat.isVisible()) throw new Error('键盘无法收起分组');
  if (await groupToggle.locator('.group-count').textContent() !== '2' || await groupToggle.getAttribute('aria-description') !== '2 只猫咪') throw new Error('收起分组的猫咪数量错误');
  await sidebarInput.focus();
  await window.mouse.move(700, 400);
  await window.waitForFunction(() => getComputedStyle(document.querySelector('.group-toggle > svg')).opacity === '0');
  for (const theme of ['light', 'dark']) {
    await window.evaluate(theme => { document.documentElement.dataset.theme = theme; }, theme);
    const sidebarBounds = await window.locator('.sidebar').boundingBox();
    const headerBounds = await groupToggle.boundingBox();
    if (settings.screenshots) await window.screenshot({ clip: { x: sidebarBounds.x, y: headerBounds.y - 4, width: sidebarBounds.width, height: headerBounds.height + 8 }, path: join(settings.screenshots, 'sidebar-collapsed-' + theme + '.png'), animations: 'disabled' });
  }
  await window.keyboard.press('Tab');
  await groupToggle.focus();
  await window.keyboard.press('Space');
  await selectedCat.waitFor();
  if (await groupToggle.getAttribute('aria-expanded') !== 'true' || await groupToggle.locator('.group-count').count()) throw new Error('展开后分组状态或数量显示错误');
  stage = '侧栏消息预览根据真实受理状态显示';
  await sidebarInput.fill('侧栏预览已发送');
  await window.getByRole('button', { name: '发送', exact: true }).click();
  await window.getByRole('log').getByText('侧栏预览已发送', { exact: true }).waitFor();
  await window.getByLabel('发送中', { exact: true }).waitFor({ state: 'hidden' });
  const catPreview = window.getByRole('button', { name: '侧栏猫咪', exact: true }).locator('.agent-preview');
  if (await catPreview.textContent() !== '侧栏预览已发送') throw new Error('受理成功的消息被侧栏误标未发送');
  await window.reload();
  await window.getByRole('log').getByText('侧栏预览已发送', { exact: true }).waitFor();
  if (await catPreview.textContent() !== '侧栏预览已发送') throw new Error('重开后已发送消息预览状态错误');
  await window.getByRole('button', { name: '搜索助手和消息', exact: true }).click();
  await window.getByRole('combobox', { name: '搜索内容', exact: true }).fill('侧栏预览已发送');
  if (await window.getByRole('option', { name: /侧栏猫咪/ }).locator('small').textContent() !== '侧栏预览已发送') throw new Error('搜索与侧栏的发送状态不一致');
  await window.keyboard.press('Escape');
  stage = '重命名输入框浅边框与聚焦提示';
  for (const width of [1073, 860]) {
    await application.evaluate(({ BrowserWindow }, width) => BrowserWindow.getAllWindows()[0].setContentSize(width, 780), width);
    for (const theme of ['light', 'dark']) {
      await window.evaluate(theme => { document.documentElement.dataset.theme = theme; }, theme);
      await window.getByRole('button', { name: '侧栏猫咪', exact: true }).click({ button: 'right' });
      await window.getByRole('menuitem', { name: '重命名猫咪', exact: true }).click();
      const nameInput = window.getByRole('textbox', { name: '猫咪名称', exact: true });
      await nameInput.waitFor();
      const softFocus = await nameInput.evaluate((element, theme) => {
        const style = getComputedStyle(element);
        const border = style.borderColor.match(/[\d.]+/g).slice(0, 3).map(Number);
        const row = element.closest('.agent-button').getBoundingClientRect();
        const bounds = element.getBoundingClientRect();
        return document.activeElement === element && style.borderTopWidth === '1px' && style.boxShadow !== 'none' && (theme === 'light' ? Math.min(...border) >= 180 : Math.max(...border) <= 100) && bounds.left >= row.left && bounds.right <= row.right;
      }, theme);
      if (!softFocus) throw new Error('重命名输入框边框过重、缺少聚焦提示或超出条目');
      if (settings.screenshots) await window.locator('.agent-groups').screenshot({ path: join(settings.screenshots, 'sidebar-rename-' + theme + '-' + width + '.png'), animations: 'disabled' });
      await nameInput.fill('取消的名称');
      await nameInput.press('Escape');
      await window.getByRole('button', { name: '侧栏猫咪', exact: true }).waitFor();
      if (await catPreview.textContent() !== '侧栏预览已发送') throw new Error('取消重命名改变了消息摘要');
    }
  }
  stage = '猫咪详情移除模型策略';
  await window.getByRole('button', { name: '查看侧栏猫咪资料', exact: true }).click();
  const catPanel = window.getByRole('complementary', { name: '侧栏猫咪', exact: true });
  if (await catPanel.getByText('模型策略', { exact: true }).count() || await catPanel.getByText('由服务端管理', { exact: true }).count()) throw new Error('猫咪详情仍显示模型策略');
  stage = '猫咪详情整合电脑和定时卡片';
  if (await catPanel.getByRole('tab').count() !== 2 || await catPanel.getByText('介绍', { exact: true }).count()) throw new Error('旧介绍或独立页签仍存在');
  if (await catPanel.getByText('名称', {exact:true}).count() || await catPanel.getByText('分组', {exact:true}).count()) throw new Error('动态仍显示名称或分组');
  await catPanel.getByRole('switch', {name: '禁用工作日报', exact:true}).waitFor();
  if (await window.evaluate(() => 'loadIntroduction' in window.desktop.backend)) throw new Error('客户端仍暴露介绍请求');
  const computerCard = catPanel.getByRole('button', { name: '侧栏猫咪的电脑', exact: true });
  await computerCard.waitFor();
  if (await computerCard.getAttribute('aria-expanded') !== 'false') throw new Error('电脑配置没有默认收起');
  stage = '展开详情电脑卡片';
  await computerCard.press('Enter');
  await catPanel.getByRole('textbox', { name: '远端电脑地址', exact: true }).waitFor();
  await computerCard.press('Enter');
  if (await catPanel.getByRole('textbox', { name: '远端电脑地址', exact: true }).isVisible()) throw new Error('电脑卡片不能收起');
  stage = '详情页签键盘切换';
  await catPanel.getByRole('tab', { name: '动态', exact: true }).focus();
  await window.keyboard.press('ArrowRight');
  await catPanel.getByRole('tab', { name: '资料库', selected: true }).waitFor();
  await window.keyboard.press('ArrowRight');
  await catPanel.getByRole('tab', { name: '动态', selected: true }).waitFor();
  for (const width of [1073, 860]) {
    await application.evaluate(({ BrowserWindow }, width) => BrowserWindow.getAllWindows()[0].setContentSize(width, 780), width);
    for (const theme of ['light', 'dark']) {
      await window.evaluate(theme => { document.documentElement.dataset.theme = theme; }, theme);
      await catPanel.getByRole('region', { name: '电脑', exact: true }).waitFor();
      await catPanel.getByRole('region', { name: '任务', exact: true }).waitFor();
      stage = '详情卡片布局 ' + theme + ' ' + width;
      const fits = await catPanel.evaluate(panel => {
        const card = panel.querySelector('.computer-card').getBoundingClientRect();
        const preview = panel.querySelector('.computer-preview').getBoundingClientRect();
        const copy = panel.querySelector('.computer-card-copy').getBoundingClientRect();
        const routine = panel.querySelector('.task-pane').getBoundingClientRect();
        const body = panel.querySelector('.cat-panel-body');
        return {height: card.height, previewWidth: preview.width, previewHeight: preview.height, gap: preview.left-copy.right, inset: card.right-preview.right, leftOffset: card.left-routine.left, widthOffset: card.width-routine.width, overflow: body.scrollWidth-body.clientWidth};
      });
      if (!(fits.height>=70 && fits.height<=80 && Math.abs(fits.previewWidth-64)<1 && Math.abs(fits.previewHeight-40)<1 && fits.gap>=0 && fits.inset>0 && Math.abs(fits.leftOffset)<1 && Math.abs(fits.widthOffset)<1 && fits.overflow<=0)) { process.stderr.write('详情卡片布局：' + JSON.stringify(fits) + '\n'); throw new Error('详情卡片尺寸、排列或宽度不符合要求'); }
      const toggle = await catPanel.getByRole('switch', {name:'禁用工作日报',exact:true}).evaluate(element => {
        const track = element.getBoundingClientRect();
        const thumb = element.querySelector('span').getBoundingClientRect();
        return track.width === 34 && track.height === 20 && thumb.width === 16 && thumb.height === 16 && thumb.left > track.left + 10 && thumb.right < track.right;
      });
      if (!toggle) throw new Error('任务开关轨道或滑块尺寸错误');
      await catPanel.getByRole('heading', {name:'侧栏猫咪',exact:true}).click();
      await window.mouse.move(400, 120);
      if (settings.screenshots) await catPanel.screenshot({ path: join(settings.screenshots, 'cat-details-' + theme + '-' + width + '.png'), animations: 'disabled' });
    }
  }

  stage = '任务卡片禁用启用和删除';
  await catPanel.getByRole('switch', {name:'禁用工作日报',exact:true}).click();
  await catPanel.getByRole('switch', {name:'启用工作日报',exact:true}).waitFor();
  await window.getByRole('button', {name:'关闭猫咪面板',exact:true}).click();
  await window.getByRole('button', {name:'查看侧栏猫咪资料',exact:true}).click();
  await catPanel.getByRole('switch', {name:'启用工作日报',exact:true}).waitFor();
  await catPanel.getByRole('switch', {name:'启用工作日报',exact:true}).click();
  await catPanel.getByRole('switch', {name:'禁用工作日报',exact:true}).waitFor();
  await catPanel.getByRole('button', {name:'管理工作日报',exact:true}).click();
  await catPanel.getByRole('menuitem', {name:'删除',exact:true}).click();
  await catPanel.getByText('还没有任务',{exact:true}).waitFor();
  await window.getByRole('button', { name: '关闭猫咪面板', exact: true }).click();
  stage = '通用设置账号与招聘页签布局';
  for (const width of [1073, 860]) {
    await application.evaluate(({ BrowserWindow }, width) => BrowserWindow.getAllWindows()[0].setContentSize(width, 780), width);
    for (const theme of ['light', 'dark']) {
      await window.evaluate(theme => { document.documentElement.dataset.theme = theme; }, theme);
      await window.getByRole('button', { name: '打开设置', exact: true }).click();
      const dialog = window.getByRole('dialog', { name: '设置', exact: true });
      const account = dialog.getByRole('region', { name: '账号', exact: true });
      await account.getByText('桌面资料联调用户', { exact: true }).waitFor();
      if (await dialog.getByRole('button', { name: '模型策略', exact: true }).count() || await account.getByRole('button').count() !== 1) throw new Error('设置分类或账号操作不正确');
      const validAccount = await account.evaluate(async element => {
        const profile = await window.desktop.backend.loadUserProfile();
        const card = element.querySelector('.settings-account');
        const name = element.querySelector('.settings-account-copy strong');
        const username = element.querySelector('.settings-account-copy span');
        const avatar = element.querySelector('img');
        return name.textContent === profile.displayName && username.textContent === profile.username && avatar.getAttribute('src') === profile.avatar && card.scrollWidth <= card.clientWidth && card.getBoundingClientRect().width > 0;
      });
      if (!validAccount) throw new Error('账号资料或卡片布局不正确');
      if (settings.screenshots) await dialog.screenshot({ path: join(settings.screenshots, 'settings-account-' + theme + '-' + width + '.png'), animations: 'disabled' });
      await window.getByRole('button', { name: '关闭设置', exact: true }).click();
      await window.getByRole('button', { name: '新建', exact: true }).click();
      await window.getByRole('menuitem', { name: '招聘猫咪', exact: true }).click();
      const recruit = window.getByRole('dialog', { name: '招聘猫咪', exact: true });
      const kinds = recruit.getByRole('group', { name: '猫咪类型', exact: true });
      const compact = await kinds.evaluate(element => {
        const bounds = element.getBoundingClientRect();
        return bounds.height <= 36 && bounds.height >= 32 && bounds.width <= 240 && bounds.width < element.closest('fieldset').getBoundingClientRect().width && element.scrollWidth <= element.clientWidth;
      });
      if (!compact) throw new Error('招聘页签尺寸过大或溢出');
      if (settings.screenshots) await recruit.screenshot({ path: join(settings.screenshots, 'recruit-tabs-' + theme + '-' + width + '.png'), animations: 'disabled' });
      await kinds.getByRole('button', { name: '自定义猫咪', exact: true }).click();
      if (await kinds.getByRole('button', { name: '自定义猫咪', exact: true }).getAttribute('aria-pressed') !== 'true') throw new Error('自定义猫咪页签无法选中');
      await recruit.getByRole('textbox', { name: '猫咪介绍', exact: true }).waitFor();
      await kinds.getByRole('button', { name: '预设猫咪', exact: true }).focus();
      await window.keyboard.press('Space');
      await recruit.getByRole('group', { name: '预设猫咪头像', exact: true }).waitFor();
      await window.getByRole('button', { name: '关闭弹窗', exact: true }).click();
    }
  }
  stage = '准备长名称成员';
  const longPeerName = '负责复杂项目资料核对与归档整理的协作猫咪';
  await window.getByRole('button', { name: '侧栏伙伴', exact: true }).click({ button: 'right' });
  await window.getByRole('menuitem', { name: '重命名猫咪', exact: true }).click();
  const peerNameInput = window.getByRole('textbox', { name: '猫咪名称', exact: true });
  await peerNameInput.fill(longPeerName);
  await peerNameInput.press('Enter');
  await window.getByRole('button', { name: longPeerName, exact: true }).waitFor();
  stage = '创建用于成员面板验证的群聊';
  await window.getByRole('button', { name: '新建', exact: true }).click();
  await window.getByRole('menuitem', { name: '创建群聊', exact: true }).click();
  const groupDialog = window.getByRole('dialog', { name: '新建群聊', exact: true });
  await groupDialog.getByRole('textbox', { name: '群名称', exact: true }).fill('协作群');
  await groupDialog.getByRole('checkbox', { name: '侧栏猫咪', exact: true }).check();
  await groupDialog.getByRole('button', { name: '创建群聊', exact: true }).click();
  stage = '打开群成员面板';
  await window.getByRole('button', { name: '查看协作群群资料', exact: true }).click();
  const groupPanel = window.getByRole('complementary', { name: '协作群群资料', exact: true });
  await groupPanel.getByRole('img', { name: '你的头像', exact: true }).waitFor();
  for (const width of [1073, 860]) {
    await application.evaluate(({ BrowserWindow }, width) => BrowserWindow.getAllWindows()[0].setContentSize(width, 780), width);
    for (const theme of ['light', 'dark']) {
      stage = '群主头像与成员对齐 ' + theme + ' ' + width;
      await window.evaluate(theme => { document.documentElement.dataset.theme = theme; }, theme);
      const valid = await groupPanel.evaluate(async panel => {
        const profile = await window.desktop.backend.loadUserProfile();
        const rows = panel.querySelectorAll('.group-member-row');
        const owner = rows[0].querySelector('img');
        await owner.decode();
        const ownerAvatar = owner.getBoundingClientRect();
        const memberAvatar = rows[1].querySelector('.avatar').getBoundingClientRect();
        const label = rows[0].querySelector('.group-member-role').getBoundingClientRect();
        const ownerName = rows[0].querySelector('.group-member-name').getBoundingClientRect();
        const memberName = rows[1].querySelector('.group-member-name').getBoundingClientRect();
        const row = rows[0].getBoundingClientRect();
        return owner.getAttribute('src') === profile.avatar && owner.naturalWidth > 0 && Math.abs(ownerAvatar.left - memberAvatar.left) < 1 && Math.abs(ownerAvatar.width - memberAvatar.width) < 1 && Math.abs(ownerName.left - memberName.left) < 1 && Math.abs(row.right - label.right - 8) < 1 && panel.scrollWidth <= panel.clientWidth;
      });
      if (!valid) throw new Error('群主头像、成员对齐或右侧标签布局错误');
      if (settings.screenshots) await groupPanel.screenshot({ path: join(settings.screenshots, 'group-owner-' + theme + '-' + width + '.png'), animations: 'disabled' });
      stage = '添加成员长名称布局 ' + theme + ' ' + width;
      await groupPanel.getByRole('button', { name: '添加成员', exact: true }).click();
      const peerOption = groupPanel.getByRole('option', { name: longPeerName, exact: true });
      const fits = await peerOption.evaluate(option => {
        const name = option.querySelector('.group-member-name');
        const menu = option.closest('.member-picker-results');
        return name !== null && name.scrollWidth > name.clientWidth && getComputedStyle(name).textOverflow === 'ellipsis' && option.scrollWidth <= option.clientWidth && menu.scrollWidth <= menu.clientWidth;
      });
      if (!fits) throw new Error('添加成员的长名称未省略或超出菜单');
      if (settings.screenshots) await groupPanel.screenshot({ path: join(settings.screenshots, 'group-picker-' + theme + '-' + width + '.png'), animations: 'disabled' });
      await groupPanel.getByRole('textbox', { name: '搜索猫咪名称', exact: true }).press('Escape');
    }
  }
  stage = '群资料库上传真实文件';
  await groupPanel.getByRole('tab', { name: '资料库', exact: true }).click();
  await groupPanel.getByText('资料库还是空的。上传文件，或在群聊中分享附件。').waitFor();
  const libraryText = '共享项目资料：上传、预览、下载与持久化。';
  await groupPanel.getByLabel('选择群文件').setInputFiles([
    {name:'群资料.txt',mimeType:'text/plain',buffer:Buffer.from(libraryText)},
    {name:'设计预览.png',mimeType:'image/png',buffer:Buffer.from(avatarPNG,'base64')},
    {name:'用于核对超长中文文件名称省略显示与按钮布局的设计说明文档.txt',mimeType:'text/plain',buffer:Buffer.from('设计说明')}
  ]);
  await groupPanel.getByRole('button', {name:'预览 群资料.txt',exact:true}).waitFor();
  await groupPanel.getByText('已上传 3 个文件', {exact:true}).waitFor();
  for (const width of [1073,860]) {
    await application.evaluate(({BrowserWindow},width)=>BrowserWindow.getAllWindows()[0].setContentSize(width,780),width);
    for (const theme of ['light','dark']) {
      await window.evaluate(theme=>{document.documentElement.dataset.theme=theme;},theme);
      if (!(await groupPanel.evaluate(panel=>panel.scrollWidth<=panel.clientWidth && [...panel.querySelectorAll('.group-library-files li')].every(row=>row.scrollWidth<=row.clientWidth)))) throw new Error('群资料库布局溢出');
      if(settings.screenshots) await groupPanel.screenshot({path:join(settings.screenshots,'group-library-'+theme+'-'+width+'.png'),animations:'disabled'});
    }
  }
  stage = '群资料库图片预览';
  await groupPanel.getByRole('button',{name:'预览 设计预览.png',exact:true}).click();
  const imagePreview=window.getByRole('dialog',{name:'文件预览',exact:true});
  await imagePreview.getByRole('img',{name:'设计预览.png',exact:true}).evaluate(image=>image.decode());
  await imagePreview.getByRole('button',{name:'关闭弹窗',exact:true}).click();
  stage = '群资料库文本预览与真实保存';
  await groupPanel.getByRole('button',{name:'预览 群资料.txt',exact:true}).click();
  const filePreview=window.getByRole('dialog',{name:'文件预览',exact:true});
  await filePreview.getByText(libraryText,{exact:true}).waitFor();
  const downloadDirectory=mkdtempSync(join(tmpdir(),'tombot-library-download-'));
  const downloadPath=join(downloadDirectory,'群资料.txt');
  try {
    await application.evaluate(({dialog},filePath)=>{dialog.showSaveDialog=async()=>({canceled:false,filePath});},downloadPath);
    await filePreview.getByRole('button',{name:'下载文件',exact:true}).click();
    await filePreview.getByText('文件已保存。',{exact:true}).waitFor();
    if(readFileSync(downloadPath,'utf8')!==libraryText) throw new Error('下载文件内容不一致');
  } finally {rmSync(downloadDirectory,{recursive:true,force:true});}
  await filePreview.getByRole('button',{name:'关闭弹窗',exact:true}).click();
  stage = '群文件重命名搜索与重新加载';
  await groupPanel.getByRole('button',{name:'重命名 群资料.txt',exact:true}).click();
  const renameFile=window.getByRole('dialog',{name:'重命名文件',exact:true});
  await renameFile.getByRole('textbox',{name:'文件名称',exact:true}).fill('更新后的群资料.txt');
  await renameFile.getByRole('button',{name:'保存名称',exact:true}).click();
  await groupPanel.getByRole('button',{name:'预览 更新后的群资料.txt',exact:true}).waitFor();
  await groupPanel.getByRole('textbox',{name:'搜索群文件',exact:true}).fill('不存在');
  await groupPanel.getByRole('button',{name:'搜索文件',exact:true}).click();
  await groupPanel.getByText('没有找到匹配的文件。',{exact:true}).waitFor();
  await window.reload();
  await window.getByRole('button',{name:'协作群',exact:true}).click();
  await window.getByRole('button',{name:'查看协作群群资料',exact:true}).click();
  await groupPanel.getByRole('tab',{name:'资料库',exact:true}).click();
  await groupPanel.getByRole('button',{name:'预览 更新后的群资料.txt',exact:true}).waitFor();
  stage = '群聊附件自动收录';
  await groupPanel.getByRole('button',{name:'关闭群资料',exact:true}).click();
  await window.locator('#group-file').setInputFiles({name:'聊天附件.txt',mimeType:'text/plain',buffer:Buffer.from('附件内容')});
  await window.locator('.composer-files').getByText('聊天附件.txt',{exact:true}).waitFor();
  await window.getByRole('textbox',{name:'给协作群发消息',exact:true}).fill('请查看附件');
  await window.getByRole('button',{name:'发送',exact:true}).click();
  await window.getByRole('button',{name:'下载 聊天附件.txt',exact:true}).waitFor();
  await window.getByRole('button',{name:'查看协作群群资料',exact:true}).click();
  await groupPanel.getByRole('tab',{name:'资料库',exact:true}).click();
  await groupPanel.getByRole('button',{name:'预览 聊天附件.txt',exact:true}).waitFor();
  stage = '群资料库确认删除';
  await groupPanel.getByRole('button',{name:'删除 更新后的群资料.txt',exact:true}).click();
  await window.getByRole('dialog',{name:'删除文件',exact:true}).getByRole('button',{name:'确认删除',exact:true}).click();
  await groupPanel.getByRole('button',{name:'预览 更新后的群资料.txt',exact:true}).waitFor({state:'detached'});
  await groupPanel.getByRole('button', { name: '关闭群资料', exact: true }).click();
  stage = '设置退出登录撤销真实会话';
  await window.getByRole('button', { name: '打开设置', exact: true }).click();
  await window.getByRole('button', { name: '退出登录', exact: true }).click();
  // 未登录启动按既有流程先确认已保存地址，不能因地址仍在而恢复已注销的会话。
  await window.getByRole('heading', { name: '连接服务', exact: true }).waitFor();
  if ((await window.evaluate(() => window.desktop.backend.session())).phase !== 'address') throw new Error('退出后仍保留可用会话');
  if (await window.getByRole('textbox', { name: '服务地址', exact: true }).inputValue() !== settings.url) throw new Error('退出丢失了服务地址');
  await window.reload();
  await window.getByRole('heading', { name: '连接服务', exact: true }).waitFor();
  await window.getByRole('button', { name: '连接', exact: true }).click();
  await window.getByRole('heading', { name: '登录 TomBot', exact: true }).waitFor();
  if (await window.getByRole('textbox', { name: '给侧栏猫咪发消息', exact: true }).count()) throw new Error('重新连接错误恢复了已退出的工作空间');
  process.stdout.write('真实登录、招聘和发送操作已完成，继续核对数据库记录\n');
} catch {
  if (application && stage === '切回客户端同步用户资料') {
    const visibility = await application.evaluate(({ BrowserWindow }) => {
      const main = BrowserWindow.getAllWindows()[0];
      return { visible: main.isVisible(), focused: main.isFocused(), events: main.profileRefreshEvents };
    });
    process.stderr.write('账号同步窗口状态：' + JSON.stringify(visibility) + '\n');
  }
  if (application && settings.screenshots) await application.firstWindow().then(window => window.screenshot({ path: join(settings.screenshots, 'chat-failure.png') }));
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
	var bots, messages, runs, reactions, interactions int
	err = fixture.pool.QueryRow(`SELECT
        (SELECT count(*) FROM tombot.bots WHERE user_id=u.id AND name='联调猫咪'),
        (SELECT count(*) FROM tombot.messages m JOIN tombot.conversations c ON c.user_id=m.user_id AND c.id=m.conversation_id JOIN tombot.bots b ON b.user_id=c.user_id AND b.id=c.bot_id WHERE m.user_id=u.id AND b.name='联调猫咪' AND m.sender_kind='user' AND m.blocks @> '[{"kind":"text","text":"你好，验证消息入库"}]'),
        (SELECT count(*) FROM tombot.runs WHERE user_id=u.id),
        (SELECT count(*) FROM tombot.message_reactions WHERE user_id=u.id),
        (SELECT count(*) FROM tombot.messages WHERE user_id=u.id AND blocks @> '[{"kind":"reaction"}]')
        FROM tombot.users u WHERE u.username=$1`, fixture.username).Scan(&bots, &messages, &runs, &reactions, &interactions)
	if err != nil || bots != 1 || messages != 1 || runs != 7 || reactions != 0 || interactions != 2 {
		t.Fatalf("客户端创建或消息入库未通过：猫咪=%d 消息=%d 运行=%d 表情=%d 交互=%d 错误=%v", bots, messages, runs, reactions, interactions, err)
	}
	t.Log(string(output))
}
