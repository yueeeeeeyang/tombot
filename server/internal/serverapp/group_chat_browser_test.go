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

// TestDesktopGroupChat 验证真实登录、建群、提及、引用、成员管理与聊天菜单，不修改日常用户目录。
func TestDesktopGroupChat(t *testing.T) {
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
  await window.getByLabel('证书 SHA-256 指纹', { exact: true }).press('Tab');
  if(await window.getByLabel('证书 SHA-256 指纹', { exact: true }).inputValue()!==settings.certificateFingerprint)throw new Error('指纹输入未完整写入');
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
  await window.getByRole('textbox', { name: '助手名称', exact: true }).fill('侧栏猫咪');
  await window.getByRole('button', { name: '招聘猫咪', exact: true }).click();
  await window.getByRole('textbox', { name: '给侧栏猫咪发消息', exact: true }).waitFor();

  stage = '单聊附件与资料库';
  await window.locator('.composer input[type=file]').setInputFiles({name:'单聊需求.txt',mimeType:'text/plain',buffer:Buffer.from('单聊文件内容')});
  await window.locator('.composer-files').getByText('单聊需求.txt',{exact:true}).waitFor();
  await window.getByRole('button',{name:'发送',exact:true}).click();
  await window.locator('.saved-messages').getByText('单聊需求.txt',{exact:true}).first().waitFor();
  await window.getByRole('button',{name:'查看侧栏猫咪资料',exact:true}).click();
  let libraryPanel=window.getByRole('complementary',{name:'侧栏猫咪',exact:true});
  await libraryPanel.getByRole('tab',{name:'资料库',exact:true}).click();
  await libraryPanel.getByRole('button',{name:'预览 单聊需求.txt',exact:true}).waitFor();
  if(await libraryPanel.getByRole('button',{name:'上传文件',exact:true}).count())throw new Error('资料库仍包含上传入口');
  await libraryPanel.getByRole('textbox',{name:'搜索聊天文件',exact:true}).fill('不存在');
  await libraryPanel.getByRole('button',{name:'搜索文件',exact:true}).click();
  await libraryPanel.getByText('没有找到匹配的文件。',{exact:true}).waitFor();
  await libraryPanel.getByRole('textbox',{name:'搜索聊天文件',exact:true}).fill('需求');
  await libraryPanel.getByRole('button',{name:'搜索文件',exact:true}).click();
  await libraryPanel.getByRole('button',{name:'预览 单聊需求.txt',exact:true}).waitFor();
  if(await window.locator('.work-status-slot').count())throw new Error('输入框仍有空白占位');
  if(settings.screenshots)await window.screenshot({path:join(settings.screenshots,'direct-library.png')});
  await libraryPanel.getByRole('button',{name:'关闭猫咪面板',exact:true}).click();

  stage = '招聘第二位群成员';
  await window.getByRole('button',{name:'新建',exact:true}).click();
  await window.getByRole('menuitem',{name:'招聘猫咪',exact:true}).click();
  await window.getByRole('textbox',{name:'助手名称',exact:true}).fill('协作猫咪');
  await window.getByRole('button',{name:'招聘猫咪',exact:true}).click();
  await window.getByRole('textbox',{name:'给协作猫咪发消息',exact:true}).waitFor();
  stage = '真实创建群聊并定向发送';
  await window.getByRole('button',{name:'新建',exact:true}).click();
  await window.getByRole('menuitem',{name:'创建群聊',exact:true}).click();
  await window.getByRole('textbox',{name:'群名称',exact:true}).fill('协作讨论');
  await window.getByRole('checkbox',{name:/侧栏猫咪/}).check();
  await window.getByRole('checkbox',{name:/协作猫咪/}).check();
  await window.getByRole('button',{name:'创建群聊',exact:true}).click();
  const composer=window.getByRole('textbox',{name:'给协作讨论发消息',exact:true});
  await composer.waitFor();
  if(await window.locator('.saved-messages .local-note').count())throw new Error('空群聊仍显示顶部提示');
  if(settings.screenshots)await window.screenshot({path:join(settings.screenshots,'empty-group.png')});
  await composer.fill('@侧');
  await window.getByRole('option',{name:'侧栏猫咪',exact:true}).click();
  await composer.fill('@侧栏猫咪 请检查这个方案');
  await window.getByRole('button',{name:'发送',exact:true}).click();
  await window.getByText('@侧栏猫咪 请检查这个方案',{exact:true}).waitFor();
  stage = '引用消息及再次发送';
  await window.getByRole('button',{name:'回复',exact:true}).first().click();
  await composer.fill('补充这一点');
  await window.getByRole('button',{name:'发送',exact:true}).click();
  await window.locator('.saved-messages').getByText('补充这一点',{exact:true}).waitFor();
  stage = '读取群消息持久记录';
  const persisted=await window.evaluate(async()=>{
    const backend=window.desktop.backend;
    const groups=await backend.listGroups();
    const group=groups.find(group=>group.name==='协作讨论');
    return {group,page:await backend.listConversationMessages(group.conversation_id)};
  });
  const userMessages=persisted.page.messages.filter(message=>message.role==='user');
  stage = '核对群消息元数据：消息数=' + userMessages.length + '，提及数=' + userMessages[0]?.mentions?.length + '，引用匹配=' + (userMessages[1]?.replyTo?.messageId === userMessages[0]?.id);
  if(userMessages.length!==2||userMessages[0].mentions.length!==1||userMessages[1].replyTo.messageId!==userMessages[0].id)throw new Error('群消息元数据未持久化');
  stage = '成员页签和真实增删成员';
  await window.getByRole('button',{name:'查看协作讨论群资料',exact:true}).click();
  let panel=window.getByRole('complementary',{name:'协作讨论群资料',exact:true});
  if(JSON.stringify(await panel.getByRole('tab').allTextContents())!==JSON.stringify(['成员','资料库']))throw new Error('群页签未调整');
  await panel.getByRole('button',{name:'移出协作猫咪',exact:true}).click();
  await panel.getByRole('button',{name:'移出协作猫咪',exact:true}).waitFor({state:'hidden'});
  await panel.getByRole('button',{name:'添加成员',exact:true}).click();
  await window.getByRole('option',{name:'协作猫咪',exact:true}).click();
  await panel.getByRole('button',{name:'移出协作猫咪',exact:true}).waitFor();
  for(const width of [1073,860]){
    await application.evaluate(({BrowserWindow},width)=>BrowserWindow.getAllWindows()[0].setContentSize(width,780),width);
    for(const theme of ['light','dark']){
      await window.evaluate(theme=>document.documentElement.dataset.theme=theme,theme);
      if(await window.evaluate(()=>document.documentElement.scrollWidth>innerWidth || document.querySelector('.chat-panel').getBoundingClientRect().width<300))throw new Error('群聊界面溢出');
      if(settings.screenshots)await window.screenshot({path:join(settings.screenshots,'group-'+theme+'-'+width+'.png'),animations:'disabled'});
    }
  }
  await panel.getByRole('button',{name:'关闭群资料',exact:true}).click();
  stage = '群附件收录与回到底部';
  await window.locator('.composer input[type=file]').setInputFiles({name:'群聊资料.txt',mimeType:'text/plain',buffer:Buffer.from('群聊资料正文')});
  await window.locator('.composer-files').getByText('群聊资料.txt',{exact:true}).waitFor();
  await composer.fill(Array.from({length:40},(_,i)=>'滚动验证第'+(i+1)+'行').join('\n'));
  await window.getByRole('button',{name:'发送',exact:true}).click();
  await window.locator('.saved-messages').getByText('群聊资料.txt',{exact:true}).waitFor();
  await window.locator('.messages-scroll').evaluate(element=>{element.scrollTop=0;element.dispatchEvent(new Event('scroll',{bubbles:true}));});
  await window.getByRole('button',{name:'回到最新消息',exact:true}).click();
  await window.getByRole('button',{name:'回到最新消息',exact:true}).waitFor({state:'hidden'});
  if(await window.locator('.messages-scroll').evaluate(element=>element.scrollHeight-element.scrollTop-element.clientHeight>48))throw new Error('没有返回消息底部');
  await window.getByRole('button',{name:'查看协作讨论群资料',exact:true}).click();
  await panel.getByRole('tab',{name:'资料库',exact:true}).click();
  await panel.getByRole('button',{name:'预览 群聊资料.txt',exact:true}).waitFor();
  if(await panel.getByText('单聊需求.txt',{exact:true}).count())throw new Error('群资料库混入单聊文件');
  if(await panel.locator('.chat-library-search').evaluate(form=>{
    const input=form.querySelector('input').getBoundingClientRect(),button=form.querySelector('button').getBoundingClientRect();
    return button.left<input.left || button.right>input.right || button.top<input.top || button.bottom>input.bottom;
  }))throw new Error('搜索图标没有置于输入框内');
  if(settings.screenshots)await window.screenshot({path:join(settings.screenshots,'group-library.png')});
  await panel.getByRole('button',{name:'关闭群资料',exact:true}).click();
  stage = '侧栏重命名、置顶和重新打开';
  let item=window.getByRole('button',{name:'协作讨论',exact:true});
  await item.click({button:'right'});
  await window.getByRole('menuitem',{name:'重命名聊天',exact:true}).click();
  await window.getByRole('textbox',{name:'聊天名称',exact:true}).fill('方案讨论');
  await window.getByRole('textbox',{name:'聊天名称',exact:true}).press('Enter');
  item=window.getByRole('button',{name:'方案讨论',exact:true});
  await item.click({button:'right'});
  await window.getByRole('menuitem',{name:'置顶',exact:true}).click();
  await window.reload();
  await window.getByRole('button',{name:'方案讨论',exact:true}).click();
  await window.locator('.saved-messages').getByText('补充这一点',{exact:true}).waitFor();
  await window.getByRole('button',{name:/查看原消息：/}).waitFor();
  if(await window.getByRole('button',{name:'方案讨论',exact:true}).evaluate(element=>!element.classList.contains('pinned-agent')))throw new Error('群置顶未恢复');
  stage = '群聊删除';
  await window.getByRole('button',{name:'方案讨论',exact:true}).click({button:'right'});
  await window.getByRole('menuitem',{name:'删除',exact:true}).click();
  await window.getByRole('button',{name:'确认删除',exact:true}).click();
  await window.getByRole('button',{name:'方案讨论',exact:true}).waitFor({state:'hidden'});
  if((await window.evaluate(()=>window.desktop.backend.listGroups())).length)throw new Error('群聊删除未保存');

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
		t.Fatalf("群聊联合验收失败：%v\n%s", err, output)
	}
}
