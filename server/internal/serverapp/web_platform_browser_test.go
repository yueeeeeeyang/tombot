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
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
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
	picture := image.NewNRGBA(image.Rect(0, 0, 800, 400))
	for y := 0; y < 400; y++ {
		for x := 0; x < 800; x++ {
			shade := color.NRGBA{R: 210, G: 70, B: 70, A: 255}
			if x >= 400 {
				shade = color.NRGBA{R: 60, G: 110, B: 200, A: 255}
			}
			picture.SetNRGBA(x, y, shade)
		}
	}
	var avatar bytes.Buffer
	if err := png.Encode(&avatar, picture); err != nil {
		t.Fatal(err)
	}
	var jpegAvatar bytes.Buffer
	if err := jpeg.Encode(&jpegAvatar, picture, nil); err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]string{
		"jpegAvatar": base64.StdEncoding.EncodeToString(jpegAvatar.Bytes()),
		"avatar":     base64.StdEncoding.EncodeToString(avatar.Bytes()), "nextPassword": loginTestToken(t),
		"url": "https://" + fixture.running.address, "username": fixture.username, "password": fixture.password,
		"fingerprint": base64.StdEncoding.EncodeToString(fingerprint[:]), "screenshots": os.Getenv("TOM_TEST_SCREENSHOT_DIRECTORY"),
	})
	if err != nil {
		t.Fatal("测试输入无法编码")
	}
	defer clear(input)
	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
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
    if (await page.locator('.user-shell').count()) await page.waitForFunction(theme => document.documentElement.dataset.theme === theme, theme);
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
  const account = page.locator('.account-trigger');
  if (await account.getAttribute('aria-label') !== '账户菜单：浏览器登录用户（' + settings.username + '）') throw new Error();
  for (const theme of ['light', 'dark']) {
    await page.emulateMedia({ colorScheme: theme });
    if (await page.locator('.user-shell').count()) await page.waitForFunction(theme => document.documentElement.dataset.theme === theme, theme);
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
    if (await page.locator('.user-shell').count()) await page.waitForFunction(theme => document.documentElement.dataset.theme === theme, theme);
    for (const width of [1280, 375]) {
      await page.setViewportSize({ width, height: 900 });
      if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw new Error();
      if (settings.screenshots) await page.screenshot({ path: join(settings.screenshots, 'web-profile-' + theme + '-' + width + '.png') });
    }
  }
  stage = '昵称自动保存、取消与空值校验';
  const nickname = page.getByRole('textbox', { name: '昵称', exact: true });
  const currentAvatar = page.getByAltText('当前头像', { exact: true });
  const initialAvatarURL = await currentAvatar.getAttribute('src');
  let nicknameRequests = 0;
  let avatarRequests = 0;
  let saveBehavior = 'normal';
  let releaseSave;
  await page.route('**/api/v2/web/auth/profile', async route => {
    const body = route.request().postDataBuffer();
    const changesNickname = body?.includes(Buffer.from('name="display_name"'));
    const changesAvatar = body?.includes(Buffer.from('name="avatar"'));
    if (changesNickname === changesAvatar) problems.push('资料字段未独立提交');
    if (changesNickname) nicknameRequests++;
    if (changesAvatar) avatarRequests++;
    if ((saveBehavior === 'nickname-fail' && changesNickname) || (saveBehavior === 'avatar-fail' && changesAvatar)) {
      await route.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify({ message: '测试保存失败，请重试。' }) });
    } else {
      if ((saveBehavior === 'nickname-hold' && changesNickname) || (saveBehavior === 'avatar-hold' && changesAvatar)) await new Promise(resolve => { releaseSave = resolve; });
      await route.continue();
    }
  });
  if (await nickname.isVisible() || await page.locator('.avatar-picker').count() || await page.getByRole('button', { name: '保存个人信息', exact: true }).count()) throw new Error();
  await page.getByRole('button', { name: '编辑昵称', exact: true }).click();
  if (!(await nickname.evaluate(element => element === document.activeElement))) throw new Error();
  await nickname.fill('取消的昵称');
  await nickname.press('Escape');
  if (await nickname.isVisible() || await page.locator('#nickname-display').textContent() !== '浏览器登录用户' || nicknameRequests !== 0) throw new Error();
  await page.getByRole('button', { name: '编辑昵称', exact: true }).click();
  await nickname.fill('   ');
  await page.getByRole('heading', { name: '个人信息', exact: true }).click();
  await page.getByRole('alert').filter({ hasText: '昵称须为' }).waitFor();
  if (await nickname.evaluate(element => element.checkValidity()) || nicknameRequests !== 0) throw new Error();
  await page.getByRole('button', { name: '取消', exact: true }).click();
  await page.getByRole('button', { name: '编辑昵称', exact: true }).click();
  await page.getByRole('heading', { name: '个人信息', exact: true }).click();
  if (await nickname.isVisible() || nicknameRequests !== 0) throw new Error();
  stage = '昵称保存失败保留编辑，回车重试不需要保存按钮';
  saveBehavior = 'nickname-fail';
  await page.getByRole('button', { name: '编辑昵称', exact: true }).click();
  await nickname.fill('新的昵称');
  await nickname.press('Enter');
  await page.getByRole('alert').filter({ hasText: '测试保存失败' }).waitFor();
  if (await nickname.inputValue() !== '新的昵称' || !(await account.getAttribute('aria-label')).includes('浏览器登录用户')) throw new Error();
  saveBehavior = 'nickname-hold';
  await nickname.fill('  新的昵称  ');
  await nickname.press('Enter');
  await page.getByRole('status').filter({ hasText: '正在保存昵称' }).waitFor();
  await page.getByRole('heading', { name: '个人信息', exact: true }).click();
  if (!(await nickname.getAttribute('readonly')) && !(await nickname.evaluate(element => element.readOnly))) throw new Error();
  if (nicknameRequests !== 2 || typeof releaseSave !== 'function') throw new Error();
  releaseSave(); releaseSave = undefined; saveBehavior = 'normal';
  await page.waitForFunction(() => document.querySelector('#nickname-display').textContent === '新的昵称' && !document.querySelector('#nickname-display').hidden);
  await page.getByRole('status').filter({ hasText: '昵称已保存' }).waitFor();
  if (await account.getAttribute('aria-label') !== '账户菜单：新的昵称（' + settings.username + '）') throw new Error();
  stage = '点击原头像选择图片并拒绝损坏文件';
  const selectAvatar = async (name, buffer) => {
    const chooser = page.waitForEvent('filechooser');
    await page.getByRole('button', { name: '更换头像', exact: true }).click();
    await (await chooser).setFiles({ name, mimeType: name.endsWith('.jpg') ? 'image/jpeg' : 'image/png', buffer });
  };
  await selectAvatar('fake.png', Buffer.from('<svg/>'));
  const cropDialog = page.getByRole('dialog', { name: '裁剪头像', exact: true });
  await cropDialog.getByRole('alert').filter({ hasText: '图片格式无效' }).waitFor();
  if (!(await cropDialog.getByRole('button', { name: '确认更换', exact: true }).isDisabled())) throw new Error();
  await cropDialog.getByRole('button', { name: '取消裁剪', exact: true }).click();
  stage = '读取期间取消会释放图片且不重新打开弹窗';
  const original = Buffer.from(settings.avatar, 'base64');
  await page.evaluate(() => {
    const decode = window.createImageBitmap;
    window.createImageBitmap = (...arguments_) => {
      window.createImageBitmap = decode;
      return decode(...arguments_).then(picture => new Promise(resolve => {
        const close = picture.close.bind(picture);
        picture.close = () => { close(); window.avatarDecodeReleased = true; };
        window.finishAvatarDecode = () => resolve(picture);
      }));
    };
  });
  await selectAvatar('avatar.png', original);
  await page.waitForFunction(() => typeof window.finishAvatarDecode === 'function');
  await cropDialog.getByRole('button', { name: '取消裁剪', exact: true }).click();
  await page.evaluate(() => { window.finishAvatarDecode(); delete window.finishAvatarDecode; });
  await page.waitForFunction(() => window.avatarDecodeReleased === true);
  if (await cropDialog.isVisible() || avatarRequests !== 0 || await currentAvatar.getAttribute('src') !== initialAvatarURL) throw new Error();
  stage = 'JPEG 图片可以裁剪且取消不改变头像';
  await selectAvatar('avatar.jpg', Buffer.from(settings.jpegAvatar, 'base64'));
  await cropDialog.getByText('点击“确认更换”立即保存头像。', { exact: true }).waitFor();
  await page.keyboard.press('Escape');
  await cropDialog.waitFor({ state: 'hidden' });
  if (avatarRequests !== 0 || await currentAvatar.getAttribute('src') !== initialAvatarURL || !(await page.getByRole('button', { name: '更换头像', exact: true }).evaluate(element => element === document.activeElement))) throw new Error();
  await selectAvatar('avatar.png', original);
  await cropDialog.getByText('点击“确认更换”立即保存头像。', { exact: true }).waitFor();
  stage = '缩放、拖动和键盘裁剪不产生空白边';
  await cropDialog.getByLabel('缩放', { exact: true }).fill('2');
  const cropCanvas = page.locator('#crop-canvas');
  const pixel = () => cropCanvas.evaluate(element => [...element.getContext('2d').getImageData(256, 256, 1, 1).data]);
  const bounds = await cropCanvas.boundingBox();
  if (!bounds) throw new Error();
  await page.mouse.move(bounds.x + bounds.width / 2, bounds.y + bounds.height / 2);
  await page.mouse.down();
  await page.mouse.move(bounds.x + bounds.width / 2 + 80, bounds.y + bounds.height / 2);
  await page.mouse.up();
  if ((await pixel())[0] !== 210) throw new Error();
  await cropCanvas.focus();
  for (let i = 0; i < 40; i++) await page.keyboard.press('ArrowLeft');
  if ((await pixel())[2] !== 200) throw new Error();
  if (!(await cropCanvas.evaluate(element => [0, 511].every(x => [0, 511].every(y => element.getContext('2d').getImageData(x, y, 1, 1).data[3] === 255))))) throw new Error();
  for (const theme of ['light', 'dark']) {
    await page.emulateMedia({ colorScheme: theme });
    await page.waitForFunction(theme => document.documentElement.dataset.theme === theme, theme);
    for (const width of [1280, 375]) {
      await page.setViewportSize({ width, height: 900 });
      if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw new Error();
      if (settings.screenshots) await page.screenshot({ path: join(settings.screenshots, 'web-avatar-crop-' + theme + '-' + width + '.png') });
    }
  }
  stage = '头像编码和保存失败保留裁剪，确认更换直接生效';
  await page.evaluate(() => {
    const canvas = document.querySelector('#crop-canvas');
    const encode = canvas.toBlob.bind(canvas);
    canvas.toBlob = callback => { canvas.toBlob = encode; callback(null); };
  });
  await cropDialog.getByRole('button', { name: '确认更换', exact: true }).click();
  await cropDialog.getByRole('alert').filter({ hasText: '头像生成失败' }).waitFor();
  if (avatarRequests !== 0) throw new Error();
  saveBehavior = 'avatar-fail';
  await cropDialog.getByRole('button', { name: '确认更换', exact: true }).click();
  await cropDialog.getByRole('alert').filter({ hasText: '测试保存失败' }).waitFor();
  if (await currentAvatar.getAttribute('src') !== initialAvatarURL || avatarRequests !== 1) throw new Error();
  stage = '取消旧图片编码不会解除新上传的交互锁';
  await page.evaluate(() => {
    const canvas = document.querySelector('#crop-canvas');
    const encode = canvas.toBlob.bind(canvas);
    canvas.toBlob = callback => {
      canvas.toBlob = encode;
      encode(blob => { window.finishAvatarEncode = () => callback(blob); }, 'image/png');
    };
  });
  await cropDialog.getByRole('button', { name: '确认更换', exact: true }).click();
  await page.waitForFunction(() => typeof window.finishAvatarEncode === 'function');
  await cropDialog.getByRole('button', { name: '取消裁剪', exact: true }).click();
  await selectAvatar('avatar.png', original);
  await cropDialog.getByText('点击“确认更换”立即保存头像。', { exact: true }).waitFor();
  await cropDialog.getByLabel('缩放', { exact: true }).fill('2');
  await cropCanvas.focus();
  for (let i = 0; i < 40; i++) await page.keyboard.press('ArrowLeft');
  saveBehavior = 'avatar-hold';
  await cropDialog.getByRole('button', { name: '确认更换', exact: true }).click();
  await cropDialog.getByRole('status').filter({ hasText: '正在保存头像' }).waitFor();
  await page.evaluate(async () => {
    window.finishAvatarEncode(); delete window.finishAvatarEncode;
    await new Promise(resolve => requestAnimationFrame(resolve));
  });
  await page.keyboard.press('Escape');
  if (!(await cropDialog.isVisible()) || !(await cropDialog.getByRole('button', { name: '正在更换…', exact: true }).isDisabled()) || !(await cropDialog.getByRole('button', { name: '取消裁剪', exact: true }).isDisabled())) throw new Error();
  if (avatarRequests !== 2 || typeof releaseSave !== 'function') throw new Error();
  releaseSave(); releaseSave = undefined; saveBehavior = 'normal';
  await cropDialog.waitFor({ state: 'hidden' });
  await page.getByRole('status').filter({ hasText: '头像已更换' }).waitFor();
  await page.waitForFunction(() => document.querySelector('img[alt="当前头像"]').naturalWidth === 512 && document.querySelector('.account-trigger img').naturalWidth === 512);
  if (await page.locator('#nickname-display').textContent() !== '新的昵称') throw new Error();
  const savedAvatarURL = await currentAvatar.getAttribute('src');
  await selectAvatar('avatar.png', original);
  await cropDialog.getByText('点击“确认更换”立即保存头像。', { exact: true }).waitFor();
  await cropDialog.getByRole('button', { name: '取消裁剪', exact: true }).click();
  if (await currentAvatar.getAttribute('src') !== savedAvatarURL || avatarRequests !== 2) throw new Error();
  await page.reload();
  if (await page.locator('#nickname-display').textContent() !== '新的昵称' || await nickname.isVisible()) throw new Error();
  await page.waitForFunction(() => document.querySelector('img[alt="当前头像"]').naturalWidth === 512);
  const savedPixel = await currentAvatar.evaluate(image => {
    const canvas = document.createElement('canvas'); canvas.width = canvas.height = 512;
    const context = canvas.getContext('2d'); context.drawImage(image, 0, 0);
    return [...context.getImageData(256, 256, 1, 1).data];
  });
  if (savedPixel[2] !== 200 || savedPixel[0] !== 60) throw new Error();
  stage = '昵称失焦直接保存并保留已更换头像';
  await page.getByRole('button', { name: '编辑昵称', exact: true }).click();
  await nickname.fill('失焦保存昵称');
  await page.getByRole('heading', { name: '个人信息', exact: true }).click();
  await page.waitForFunction(() => document.querySelector('#nickname-display').textContent === '失焦保存昵称' && !document.querySelector('#nickname-display').hidden);
  if (nicknameRequests !== 3) throw new Error();
  stage = '中文输入完成前不提交，结束后补全失焦保存';
  await page.getByRole('button', { name: '编辑昵称', exact: true }).click();
  await nickname.dispatchEvent('compositionstart');
  await nickname.fill('再次修改昵称');
  await page.getByRole('heading', { name: '个人信息', exact: true }).click();
  if (nicknameRequests !== 3) throw new Error();
  await nickname.dispatchEvent('compositionend');
  await page.waitForFunction(() => document.querySelector('#nickname-display').textContent === '再次修改昵称' && !document.querySelector('#nickname-display').hidden);
  await page.getByRole('status').filter({ hasText: '昵称已保存' }).waitFor();
  if (avatarRequests !== 2 || nicknameRequests !== 4) throw new Error();
  await page.waitForFunction(() => document.querySelector('img[alt="当前头像"]').naturalWidth === 512);
  stage = '账户菜单主题切换、持久保存和关闭';
  for (const theme of ['light', 'dark']) {
    await account.click();
    await page.getByLabel('主题切换', { exact: true }).selectOption(theme);
    await page.waitForFunction(theme => document.documentElement.dataset.theme === theme, theme);
    for (const width of [1280, 375]) {
      await page.setViewportSize({ width, height: 900 });
      if (await page.evaluate(() => document.documentElement.scrollWidth > innerWidth)) throw new Error();
      if (settings.screenshots) await page.screenshot({ path: join(settings.screenshots, 'web-account-menu-' + theme + '-' + width + '.png') });
    }
    await page.keyboard.press('Escape');
    if (await page.locator('.account-control').getAttribute('open') !== null) throw new Error();
    await page.reload();
    if (await page.evaluate(() => document.documentElement.dataset.theme) !== theme) throw new Error();
  }
  await account.click();
  await page.getByLabel('主题切换', { exact: true }).selectOption('system');
  await page.emulateMedia({ colorScheme: 'light' });
  await page.waitForFunction(() => document.documentElement.dataset.theme === 'light');
  await page.emulateMedia({ colorScheme: 'dark' });
  await page.waitForFunction(() => document.documentElement.dataset.theme === 'dark');
  await page.getByRole('heading', { name: '个人信息', exact: true }).click();
  if (await page.locator('.account-control').getAttribute('open') !== null) throw new Error();
  stage = '修改密码校验与全会话退出';
  await account.click();
  await page.getByRole('link', { name: '修改密码', exact: true }).click();
  await page.getByRole('heading', { name: '修改密码', exact: true }).waitFor();
  const changePassword = async (current, confirmation) => {
    await page.getByLabel('当前密码', { exact: true }).fill(current);
    await page.getByLabel('新密码', { exact: true }).fill(settings.nextPassword);
    await page.getByLabel('确认新密码', { exact: true }).fill(confirmation);
    await page.getByRole('button', { name: '修改密码', exact: true }).click();
  };
  await changePassword(settings.password, settings.nextPassword + '-mismatch');
  await page.getByRole('alert').filter({ hasText: '两次输入的新密码不一致' }).waitFor();
  await changePassword(settings.password + '-invalid', settings.nextPassword);
  await page.getByRole('alert').filter({ hasText: '当前密码不正确' }).waitFor();
  if (await page.getByLabel('当前密码', { exact: true }).inputValue() !== '') throw new Error();
  for (const width of [1280, 375]) {
    await page.setViewportSize({ width, height: 900 });
    await page.evaluate(() => scrollTo(0, 0));
    if (settings.screenshots) await page.screenshot({ path: join(settings.screenshots, 'web-password-' + width + '.png') });
  }
  await changePassword(settings.password, settings.nextPassword);
  await page.getByRole('heading', { name: 'TomBot 用户平台', exact: true }).waitFor();
  await page.getByText('密码修改成功，所有设备已退出登录。请使用新密码重新登录。', { exact: true }).waitFor();
  await page.getByLabel('用户名', { exact: true }).fill(settings.username);
  await page.getByLabel('密码', { exact: true }).fill(settings.password);
  await page.getByRole('button', { name: '登录', exact: true }).click();
  await page.getByRole('heading', { name: '登录失败', exact: true }).waitFor();
  await page.getByLabel('用户名', { exact: true }).fill(settings.username);
  await page.getByLabel('密码', { exact: true }).fill(settings.nextPassword);
  await page.getByRole('button', { name: '登录', exact: true }).click();
  await page.getByRole('heading', { name: '用量管理', exact: true }).waitFor();
  await page.goto(settings.url + '/account');
  if (await page.locator('#nickname-display').textContent() !== '再次修改昵称') throw new Error();
  await page.waitForFunction(() => document.querySelector('img[alt="当前头像"]').naturalWidth === 512);
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
  for (const path of ['/', '/account', '/account/password']) {
    await page.goto(settings.url + path);
    await page.getByRole('heading', { name: 'TomBot 用户平台', exact: true }).waitFor();
    if (new URL(page.url()).pathname !== '/login') throw new Error();
  }
  stage = '浏览器禁止存储时明确提示且仍可切换主题';
  await page.getByLabel('用户名', { exact: true }).fill(settings.username);
  await page.getByLabel('密码', { exact: true }).fill(settings.nextPassword);
  await page.getByRole('button', { name: '登录', exact: true }).click();
  await page.getByRole('heading', { name: '用量管理', exact: true }).waitFor();
  await page.addInitScript(() => Object.defineProperty(window, 'localStorage', { get() { throw new Error('浏览器存储受限'); } }));
  await page.reload();
  await account.click();
  await page.getByText('浏览器无法保存主题偏好，本次切换仍然有效。', { exact: true }).waitFor();
  await page.getByLabel('主题切换', { exact: true }).selectOption('light');
  await page.waitForFunction(() => document.documentElement.dataset.theme === 'light');
  if (problems.length) throw new Error();
  process.stdout.write('独立网页登录、平台布局、昵称自动保存、头像确认即更换、主题切换、修改密码、刷新恢复和退出均已通过\n');
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
