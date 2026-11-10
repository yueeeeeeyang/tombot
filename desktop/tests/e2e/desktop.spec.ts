import {
  _electron as electron,
  expect,
  test,
  type ElectronApplication,
  type Page,
} from '@playwright/test'
import { randomBytes } from 'node:crypto'
import { createServer, type ServerResponse } from 'node:http'
import { mkdir, mkdtemp, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { buildCompanion, startCompanion, type Companion } from './companionServer'

let application: ElectronApplication
let window: Page
let profilePath: string
let browserErrors: string[]
let companionBinary = ''
let companion: Companion
let password = ''
let setupShot = false

test.beforeAll(async () => {
  test.setTimeout(180_000)
  companionBinary = await buildCompanion()
})

/**
 * 把页面设成指定尺寸。Linux 标题栏在窗口外侧，setSize 会让页面少掉这一截，
 * 最小布局仍按页面 640 像素验收。
 */
async function setPageSize(width: number, height: number): Promise<void> {
  await application.evaluate(
    ({ BrowserWindow }, size) => {
      BrowserWindow.getAllWindows()[0]!.setContentSize(size.width, size.height)
    },
    { width, height },
  )
}

/** 通过自定义下拉菜单选择对应文本。 */
async function chooseOption(label: string, option: string): Promise<void> {
  await window.getByRole('combobox', { name: label, exact: true }).click()
  await window.getByRole('option', { name: option, exact: true }).click()
}

/** 从设置页填写本次测试的模型，不注入内置选项。 */
async function configureModel(): Promise<void> {
  await window.getByRole('button', { name: '打开设置' }).click()
  await window.getByRole('button', { name: '模型设置', exact: true }).click()
  await window.getByRole('textbox', { name: '模型名称', exact: true }).fill('项目模型')
  await window.keyboard.press('Escape')
}

test.beforeEach(async () => {
  browserErrors = []
  password = randomBytes(18).toString('base64url')
  companion = await startCompanion(companionBinary)
  profilePath = await mkdtemp(join(tmpdir(), 'tombot-desktop-test-'))
  // 虚拟显示器的设备像素比会略偏离 1，窗口和布局是按 CSS 像素断言的。
  application = await launchApp(profilePath)
  window = await application.firstWindow()
  // 等待原生窗口就绪，避免首次拖动被窗口显示过程打断。
  await expect
    .poll(() =>
      application.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0]!.isVisible()),
    )
    .toBe(true)
  await application.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0]!.focus())
  // Playwright 默认模拟浅色；取消覆盖后才能验证 Electron 的原生主题变化。
  await window.emulateMedia({ colorScheme: null })
  window.on('pageerror', (error) => browserErrors.push(error.message))
  await unlock()
  await expect(window.getByRole('textbox', { name: '给小黑发消息' })).toBeVisible()
  await expect
    .poll(() =>
      window.locator('.avatar img').evaluateAll(
        (images) =>
          images.length > 0 &&
          images.every((image) => {
            const portrait = image as HTMLImageElement
            return portrait.complete && portrait.naturalWidth > 0
          }),
      ),
    )
    .toBe(true)
  await expect(window.locator('.avatar').first()).toHaveAttribute('data-frames', 'ready')
})

test.afterEach(async () => {
  await application?.close()
  await companion?.stop()
  await rm(profilePath, { recursive: true, force: true })
  expect(browserErrors).toEqual([])
})

function launchApp(profile: string): Promise<ElectronApplication> {
  return electron.launch({
    args: ['.', `--user-data-dir=${profile}`, '--force-device-scale-factor=1'],
    env: { ...process.env, TOMBOT_SERVER_URL: companion.url },
  })
}

/** 每个测试都有自己的新服务，所以这里走初始化。初始化码和密码只填进表单。 */
async function unlock(): Promise<void> {
  const setupHeading = window.getByRole('heading', { name: '初始化 TomBot' })
  await expect(setupHeading).toBeVisible()
  if (!setupShot) {
    await mkdir('/opt/cursor/artifacts', { recursive: true })
    await window.screenshot({ path: '/opt/cursor/artifacts/session-setup.png', scale: 'css' })
    setupShot = true
  }
  await window.getByLabel('初始化码').fill(companion.setupCode)
  await window.getByLabel('密码', { exact: true }).fill(password)
  await window.getByLabel('确认密码', { exact: true }).fill(password)
  await window.getByRole('button', { name: '完成初始化' }).click()
}

/** 等服务确认保存后再重载，避免把还在传输的草稿看成丢失。 */
async function reloadPersisted(): Promise<void> {
  await expect
    .poll(() => window.evaluate(() => document.documentElement.dataset.workspaceSave ?? 'saved'))
    .toBe('saved')
  await window.reload()
  await expect(window.getByRole('textbox', { name: /发消息/ })).toBeVisible()
}

async function updateServerWorkspace(
  mutate: (workspace: {
    activeAgentId: string
    agents: Array<{
      id: string
      draft: string
      messages: Array<{
        id?: string
        role?: string
        content?: string
        createdAt: string
        status?: string
        requestId?: string
        reactions?: unknown
      }>
      artifacts: unknown[]
    }>
  }) => void,
): Promise<void> {
  await expect
    .poll(() => window.evaluate(() => document.documentElement.dataset.workspaceSave ?? 'saved'))
    .toBe('saved')
  const login = await fetch(`${companion.url}/api/v1/auth/login`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ password, deviceName: '端到端', platform: 'linux' }),
  })
  if (!login.ok) throw new Error(`登录服务失败：${login.status}`)
  const token = ((await login.json()) as { device_token: string }).device_token
  const current = await fetch(`${companion.url}/api/v1/workspace`, {
    headers: { authorization: `Bearer ${token}` },
  })
  if (!current.ok) throw new Error(`读取工作空间失败：${current.status}`)
  const snapshot = (await current.json()) as {
    workspace: {
      activeAgentId: string
      agents: Array<{
        id: string
        draft: string
        messages: Array<{
          id?: string
          role?: string
          content?: string
          createdAt: string
          status?: string
          requestId?: string
          reactions?: unknown
        }>
        artifacts: unknown[]
      }>
    }
  }
  mutate(snapshot.workspace)
  const saved = await fetch(`${companion.url}/api/v1/workspace`, {
    method: 'PUT',
    headers: { authorization: `Bearer ${token}`, 'content-type': 'application/json' },
    body: JSON.stringify(snapshot.workspace),
  })
  if (!saved.ok) throw new Error(`写入工作空间失败：${saved.status}`)
  await window.reload()
  await expect(window.getByRole('textbox', { name: /发消息/ })).toBeVisible()
}

test('已初始化后要验证密码才能进入工作空间', async () => {
  await application.close()
  await rm(profilePath, { recursive: true, force: true })
  profilePath = await mkdtemp(join(tmpdir(), 'tombot-desktop-login-'))
  application = await launchApp(profilePath)
  window = await application.firstWindow()
  window.on('pageerror', (error) => browserErrors.push(error.message))
  await expect(window.getByRole('heading', { name: '验证密码' })).toBeVisible()
  await window.screenshot({ path: '/opt/cursor/artifacts/session-login.png', scale: 'css' })
  await window.getByLabel('密码', { exact: true }).fill(password)
  await window.getByRole('button', { name: '进入工作空间' }).click()
  await expect(window.getByRole('textbox', { name: '给小黑发消息' })).toBeVisible()
})

test('桌面布局、沙盒、招聘猫咪和草稿恢复', async () => {
  const testInfo = test.info()
  const windowSize = await application.evaluate(({ BrowserWindow }) =>
    BrowserWindow.getAllWindows()[0]!.getSize(),
  )
  expect(windowSize).toEqual([1073, 780])
  // 标准尺寸下预设助手应完整展示，不能靠滚动隐藏被放大的列表项。
  expect(
    await window
      .locator('.agent-groups')
      .evaluate((element) => element.scrollHeight <= element.clientHeight),
  ).toBe(true)
  await window.screenshot({ path: testInfo.outputPath('desktop-home.png'), scale: 'css' })
  await window.screenshot({ path: '/opt/cursor/artifacts/desktop-home.png', scale: 'css' })
  const rendererProcesses = await application.evaluate(({ app }) =>
    app.getAppMetrics().filter((metric) => metric.type === 'Tab'),
  )
  expect(rendererProcesses.length).toBeGreaterThan(0)
  // Electron 只在 macOS 和 Windows 的进程指标里提供操作系统级沙箱标记。
  if (process.platform === 'darwin' || process.platform === 'win32') {
    expect(rendererProcesses.every((metric) => metric.sandboxed)).toBe(true)
  }
  expect(await window.evaluate(() => globalThis.window.desktop?.platform)).toBe(process.platform)
  expect(await window.evaluate(() => typeof (globalThis as Record<string, unknown>).require)).toBe(
    'undefined',
  )
  expect(await window.evaluate(() => typeof (globalThis as Record<string, unknown>).process)).toBe(
    'undefined',
  )

  await configureModel()
  await window.getByRole('button', { name: '新建', exact: true }).click()
  await window.getByRole('menuitem', { name: '招聘猫咪', exact: true }).click()
  const createDialog = window.getByRole('dialog')
  const dialogBox = await createDialog.boundingBox()
  expect(dialogBox!.width).toBe(480)
  await expect(createDialog.getByText('预设头像', { exact: true })).toHaveCount(0)
  await expect(createDialog.getByRole('combobox', { name: '模型', exact: true })).toHaveText(
    '项目模型',
  )
  const viewport = await window.evaluate(() => ({ width: innerWidth, height: innerHeight }))
  expect(dialogBox!.x).toBeGreaterThanOrEqual(0)
  expect(dialogBox!.y).toBeGreaterThanOrEqual(0)
  expect(dialogBox!.x + dialogBox!.width).toBeLessThanOrEqual(viewport.width)
  expect(dialogBox!.y + dialogBox!.height).toBeLessThanOrEqual(viewport.height)
  expect(await createDialog.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(
    true,
  )
  await window.screenshot({ path: testInfo.outputPath('desktop-create-agent.png'), scale: 'css' })
  await window.getByRole('textbox', { name: '助手名称' }).fill('写作搭档')
  await window.getByRole('button', { name: '布偶猫', exact: true }).click()
  await chooseOption('模型', '项目模型')
  await createDialog.getByRole('button', { name: '招聘猫咪', exact: true }).click()
  const input = window.getByRole('textbox', { name: '给写作搭档发消息' })
  await window.getByRole('button', { name: '查看写作搭档资料' }).click()
  const catPanel = window.getByRole('complementary', { name: '写作搭档' })
  await expect(catPanel.getByRole('combobox', { name: '模型', exact: true })).toHaveText('项目模型')
  await window.keyboard.press('Escape')
  await expect(catPanel).toHaveCount(0)
  await input.fill('把想法变成一篇好故事。')
  await input.press('Enter')
  await expect(window.getByRole('log')).toContainText('把想法变成一篇好故事。')
  await expect(window.getByRole('button', { name: '未发送，点击重试' })).toBeVisible()
  await input.fill('第二个尚未完成的想法')
  await reloadPersisted()
  await expect(input).toHaveValue('第二个尚未完成的想法')
  await expect(window.locator('.chat-header .avatar')).toHaveAttribute('data-avatar', 'ragdoll')
  await expect(
    window.getByRole('button', { name: /^写作搭档/ }).locator('.avatar'),
  ).toHaveAttribute('data-avatar', 'ragdoll')
  await expect(window.getByRole('log')).toContainText('把想法变成一篇好故事。')

  await window.screenshot({ path: testInfo.outputPath('desktop-conversation.png'), scale: 'css' })
})

test('八款猫咪头像加载、搜索一致性及深浅主题选择器', async () => {
  await configureModel()
  const sidebarAvatars = window.locator('.pinned-area .avatar, .agent-groups .avatar')
  await expect(sidebarAvatars).toHaveCount(8)
  const presets = await sidebarAvatars.evaluateAll((elements) =>
    elements.map((element) => element.getAttribute('data-avatar')),
  )
  expect(new Set(presets).size).toBe(8)
  const sources = await sidebarAvatars
    .locator('img')
    .evaluateAll((images) => images.map((image) => (image as HTMLImageElement).src))
  expect(sources.every((source, index) => source.includes(presets[index] ?? 'missing'))).toBe(true)
  expect(new Set(sources).size).toBe(8)
  await window.getByRole('button', { name: '搜索助手和消息' }).click()
  expect(
    await window
      .locator('.search-result .avatar')
      .evaluateAll((elements) => elements.map((element) => element.getAttribute('data-avatar'))),
  ).toEqual(presets)
  await window.keyboard.press('Escape')

  for (const theme of ['light', 'dark'] as const) {
    await window.getByRole('button', { name: '打开设置' }).click()
    await chooseOption('主题', theme === 'light' ? '浅色' : '深色')
    await window.keyboard.press('Escape')
    await window.getByRole('button', { name: '新建', exact: true }).click()
    await window.getByRole('menuitem', { name: '招聘猫咪', exact: true }).click()
    const choices = window.getByRole('group', { name: '预设猫咪头像' })
    await expect(choices.getByRole('button')).toHaveCount(8)
    for (const choice of await choices.getByRole('button').all()) {
      await choice.click()
      await expect(choice).toHaveAttribute('aria-pressed', 'true')
      await expect(choices.locator('[aria-pressed="true"]')).toHaveCount(1)
      const preset = await choice.locator('.avatar').getAttribute('data-avatar')
      const image = choice.locator('img')
      expect(await image.evaluate((node: HTMLImageElement) => node.src)).toContain(preset ?? '')
      expect(
        await image.evaluate((node: HTMLImageElement) => node.complete && node.naturalWidth > 0),
      ).toBe(true)
    }
    await window.screenshot({
      path: test.info().outputPath(`cat-presets-${theme}.png`),
      scale: 'css',
    })
    await window.keyboard.press('Escape')
  }
  await setPageSize(860, 640)
  await expect.poll(() => window.evaluate(() => innerHeight)).toBe(640)
  await window.getByRole('button', { name: '新建', exact: true }).click()
  await window.getByRole('menuitem', { name: '招聘猫咪', exact: true }).click()
  const dialog = window.getByRole('dialog', { name: '招聘猫咪' })
  expect(await dialog.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true)
  const bounds = await dialog.boundingBox()
  expect(bounds!.y).toBeGreaterThanOrEqual(0)
  expect(bounds!.y + bounds!.height).toBeLessThanOrEqual(640)
  await window.getByRole('button', { name: '暹罗猫', exact: true }).click()
  await window.getByRole('textbox', { name: '助手名称' }).fill('猫咪搭档')
  await dialog.getByRole('button', { name: '招聘猫咪', exact: true }).click()
  await expect(window.locator('.chat-header .avatar')).toHaveAttribute('data-avatar', 'siamese')
})

test('自定义选择器的顶层菜单、键盘操作、关闭与最小窗口边界', async () => {
  for (const theme of ['浅色', '深色']) {
    await window.getByRole('button', { name: '打开设置' }).click()
    await expect(window.locator('select')).toHaveCount(0)
    await chooseOption('主题', theme)
    const language = window.getByRole('combobox', { name: '语言' })
    await language.click()
    await window.keyboard.press('End')
    await window.keyboard.press('Escape')
    await expect(language).toHaveText('跟随系统')
    await expect(window.getByRole('dialog', { name: '设置' })).toBeVisible()
    await expect(language).toBeFocused()
    await language.press('Enter')
    await language.press('End')
    await language.press('Enter')
    await expect(language).toHaveText('英语')
    await chooseOption('语言', '跟随系统')
    await window.getByRole('button', { name: '模型设置', exact: true }).click()
    await expect(window.locator('select')).toHaveCount(0)
    await window.getByRole('combobox', { name: '模型供应商' }).click()
    const providerMenu = window.getByRole('listbox', { name: '模型供应商' })
    expect(await providerMenu.evaluate((element) => element.matches(':popover-open'))).toBe(true)
    await window.screenshot({
      path: test.info().outputPath(`provider-menu-${theme}.png`),
      scale: 'css',
    })
    await window.getByLabel('API 地址').click()
    await expect(providerMenu).toBeHidden()
    await window.getByRole('textbox', { name: '模型名称', exact: true }).fill('项目模型')
    await expect(window.getByRole('dialog', { name: '设置' })).toBeVisible()
    await window.keyboard.press('Escape')

    await window.getByRole('button', { name: '新建', exact: true }).click()
    await window.getByRole('menuitem', { name: '招聘猫咪', exact: true }).click()
    await expect(window.locator('select')).toHaveCount(0)
    const model = window.getByRole('combobox', { name: '模型', exact: true })
    await model.click()
    const modelMenu = window.getByRole('listbox', { name: '模型', exact: true })
    await expect(modelMenu).toBeVisible()
    expect(await modelMenu.evaluate((element) => element.matches(':popover-open'))).toBe(true)
    await window.screenshot({
      path: test.info().outputPath(`recruit-model-${theme}.png`),
      scale: 'css',
    })
    await model.press('End')
    await model.press('Enter')
    await expect(model).toHaveText('项目模型')
    await expect(window.getByRole('dialog', { name: '招聘猫咪' })).toBeVisible()
    await model.click()
    await window.keyboard.press('Tab')
    await expect(modelMenu).toBeHidden()
    await expect(
      window.getByRole('dialog').getByRole('button', { name: '招聘猫咪', exact: true }),
    ).toBeFocused()
    await window.keyboard.press('Escape')
  }

  await setPageSize(860, 640)
  await expect.poll(() => window.evaluate(() => innerHeight)).toBe(640)
  await window.getByRole('button', { name: '新建', exact: true }).click()
  await window.getByRole('menuitem', { name: '招聘猫咪', exact: true }).click()
  await window.getByRole('combobox', { name: '模型', exact: true }).click()
  const menu = window.getByRole('listbox', { name: '模型', exact: true })
  const menuBounds = await menu.boundingBox()
  expect(menuBounds!.x).toBeGreaterThanOrEqual(12)
  expect(menuBounds!.y).toBeGreaterThanOrEqual(12)
  expect(menuBounds!.x + menuBounds!.width).toBeLessThanOrEqual(848)
  expect(menuBounds!.y + menuBounds!.height).toBeLessThanOrEqual(628)
  await window.getByRole('option', { name: '项目模型', exact: true }).click()
  await expect(window.getByRole('combobox', { name: '模型', exact: true })).toHaveText('项目模型')
  await window.screenshot({ path: test.info().outputPath('recruit-compact.png'), scale: 'css' })
})

test('招聘仅列出已配置模型，空状态配置后返回并保留表单', async () => {
  await window.getByRole('button', { name: '新建', exact: true }).click()
  await window.getByRole('menuitem', { name: '招聘猫咪', exact: true }).click()
  const recruit = window.getByRole('dialog', { name: '招聘猫咪' })
  const model = recruit.getByRole('combobox', { name: '模型', exact: true })
  await expect(model).toBeDisabled()
  await expect(model).toHaveText('暂无已配置模型')
  await expect(recruit.getByRole('button', { name: '招聘猫咪', exact: true })).toBeDisabled()
  await window.getByRole('textbox', { name: '助手名称' }).fill('我的猫咪')
  await window.getByRole('button', { name: '三花猫', exact: true }).click()
  await chooseOption('分组', '游戏开发')
  await window.screenshot({ path: test.info().outputPath('recruit-empty-model.png'), scale: 'css' })
  await window.getByRole('button', { name: '前往模型设置' }).click()
  await expect(window.getByRole('dialog', { name: '设置' })).toBeVisible()
  await window.getByRole('textbox', { name: '模型名称', exact: true }).fill('团队模型')
  await window.keyboard.press('Escape')
  await expect(recruit).toBeVisible()
  await expect(window.getByRole('textbox', { name: '助手名称' })).toHaveValue('我的猫咪')
  await expect(window.getByRole('button', { name: '三花猫', exact: true })).toHaveAttribute(
    'aria-pressed',
    'true',
  )
  await expect(window.getByRole('combobox', { name: '分组' })).toHaveText('游戏开发')
  await model.click()
  await expect(window.getByRole('option')).toHaveCount(1)
  await expect(window.getByRole('option')).toHaveText('团队模型openai')
  await window.getByRole('option', { name: '团队模型', exact: true }).click()
  await recruit.getByRole('button', { name: '招聘猫咪', exact: true }).click()
  await expect(window.getByRole('textbox', { name: '给我的猫咪发消息' })).toBeVisible()
  await window.getByRole('button', { name: '查看我的猫咪资料', exact: true }).click()
  const hired = window.getByRole('complementary', { name: '我的猫咪' })
  await expect(hired.getByRole('combobox', { name: '模型' })).toHaveText('团队模型')
  await window.screenshot({ path: '/opt/cursor/artifacts/recruit-model.png', scale: 'css' })
})

test('搜索键盘操作与最小窗口布局', async () => {
  const testInfo = test.info()
  await window.getByRole('button', { name: '搜索助手和消息' }).click()
  await window.getByRole('combobox', { name: '搜索内容' }).fill('游戏美术')
  await window
    .getByRole('dialog')
    .getByRole('option', { name: /游戏美术/ })
    .click()
  await expect(window.getByRole('textbox', { name: '给游戏美术发消息' })).toBeVisible()
  await window.keyboard.press(process.platform === 'darwin' ? 'Meta+k' : 'Control+k')
  await expect(window.getByRole('dialog')).toBeVisible()
  await window.keyboard.press('Escape')
  await expect(window.getByRole('dialog')).toHaveCount(0)

  await setPageSize(860, 640)
  await expect.poll(() => window.evaluate(() => globalThis.innerHeight)).toBe(640)
  const input = window.getByRole('textbox', { name: '给游戏美术发消息' })
  await expect(input).toBeVisible()
  expect(
    await window.evaluate(() => document.documentElement.scrollWidth <= globalThis.innerWidth),
  ).toBe(true)
  const box = await input.boundingBox()
  expect(box!.y + box!.height).toBeLessThanOrEqual(640)
  await window.screenshot({ path: testInfo.outputPath('desktop-compact.png'), scale: 'css' })
  const separator = window.getByRole('separator', { name: '调整侧栏宽度' })
  await separator.focus()
  await separator.press('Home')
  await expect(separator).toHaveAttribute('aria-valuenow', '88')
  await expect(window.getByRole('complementary')).toBeVisible()
  await separator.press('ArrowRight')
  await expect(separator).toHaveAttribute('aria-valuenow', '240')

  // 缩小窗口且输入多行时，输入框应在上限处内部滚动，保存按钮仍处于可视区。
  await input.fill(Array.from({ length: 20 }, (_, index) => `第 ${index + 1} 行想法`).join('\n'))
  await expect
    .poll(() => input.evaluate((element) => element.scrollHeight > element.clientHeight))
    .toBe(true)
  const saveButton = window.getByRole('button', { name: '保存为本地草稿' })
  const saveButtonBox = await saveButton.boundingBox()
  expect(saveButtonBox!.y + saveButtonBox!.height).toBeLessThanOrEqual(640)
  await saveButton.click()
  await expect(window.getByRole('log')).toContainText('第 20 行想法')
})

test('参考图设置页和搜索框的布局与交互', async () => {
  const testInfo = test.info()
  await expect(window.getByText('Enter 保存 · Shift + Enter 换行')).toHaveCount(0)
  await window.getByRole('button', { name: '打开设置' }).click()
  const settingsDialog = window.getByRole('dialog', { name: '设置' })
  await expect(settingsDialog).toBeVisible()
  await expect(
    window.getByRole('navigation', { name: '设置分类' }).getByRole('button'),
  ).toHaveCount(2)
  await window.screenshot({
    path: testInfo.outputPath('desktop-settings-general.png'),
    scale: 'css',
  })
  await chooseOption('主题', '深色')
  await chooseOption('语言', '简体中文')
  await window.getByRole('button', { name: '模型设置', exact: true }).click()
  await chooseOption('模型供应商', 'Anthropic')
  const apiKey = window.getByLabel('API 密钥')
  await expect(apiKey).toBeEnabled()
  await expect(apiKey).toHaveAttribute('type', 'password')
  await apiKey.fill('layout-check-key')
  const outputLength = window.getByRole('spinbutton', { name: /最大输出长度/ })
  await outputLength.fill('0')
  await expect(window.getByRole('alert')).toContainText('之间的整数')
  await outputLength.fill('8192')
  await expect(window.getByRole('alert')).toHaveCount(0)
  await window.locator('.settings-pane').evaluate((element) => {
    element.scrollTop = 0
  })
  await window.screenshot({
    path: testInfo.outputPath('desktop-settings-models.png'),
    scale: 'css',
  })
  await window.keyboard.press('Escape')
  await expect(settingsDialog).toHaveCount(0)
  await expect(window.getByRole('button', { name: '打开设置' })).toBeFocused()

  await window.getByRole('button', { name: '搜索助手和消息' }).click()
  const searchInput = window.getByRole('combobox', { name: '搜索内容' })
  await expect(searchInput).toBeFocused()
  await searchInput.press('ArrowDown')
  await expect(window.getByRole('option', { selected: true })).toContainText('游戏程序')
  await window.screenshot({ path: testInfo.outputPath('desktop-search.png'), scale: 'css' })
  await searchInput.press('Enter')
  await expect(window.getByRole('textbox', { name: '给游戏程序发消息' })).toBeVisible()
  await window.getByRole('button', { name: '搜索助手和消息' }).click()
  await searchInput.fill('主题')
  await searchInput.press('Enter')
  await expect(window.getByRole('combobox', { name: '主题' })).toHaveText('深色')

  await setPageSize(860, 640)
  await expect.poll(() => window.evaluate(() => innerHeight)).toBe(640)
  await window.getByRole('button', { name: '模型设置', exact: true }).click()
  expect(
    await settingsDialog.evaluate((element) => element.scrollWidth <= element.clientWidth),
  ).toBe(true)
  const bounds = await settingsDialog.boundingBox()
  expect(bounds!.y + bounds!.height).toBeLessThanOrEqual(640)
  await expect(window.getByRole('button', { name: '关闭设置' })).toBeVisible()
  await window.getByRole('switch', { name: /流式响应/ }).click()
  await expect(window.getByRole('switch', { name: /流式响应/ })).not.toBeChecked()
  await window.screenshot({
    path: testInfo.outputPath('desktop-settings-compact.png'),
    scale: 'css',
  })
})

/** 底栏齿轮必须和头像外框同尺寸，图标居中且保持一半比例，悬停和键盘焦点样式仍在。 */
async function expectFooterControlsAligned(): Promise<void> {
  const settings = window.getByRole('button', { name: '打开设置' })
  const metrics = await window.locator('.sidebar-footer').evaluate((footer) => {
    const avatar = footer.querySelector('.profile-avatar')!.getBoundingClientRect()
    const portrait = footer.querySelector('.profile-avatar img')!.getBoundingClientRect()
    const button = footer.querySelector('.settings-button')!.getBoundingClientRect()
    const icon = footer.querySelector('.settings-button svg')!.getBoundingClientRect()
    const frame = getComputedStyle(footer.querySelector('.profile-avatar')!)
    const image = getComputedStyle(footer.querySelector('.profile-avatar img')!)
    return {
      avatar: { x: avatar.x, y: avatar.y, width: avatar.width, height: avatar.height },
      portrait: { x: portrait.x, y: portrait.y, width: portrait.width, height: portrait.height },
      button: { width: button.width, height: button.height },
      icon: { width: icon.width, height: icon.height, x: icon.x, y: icon.y },
      buttonOrigin: { x: button.x, y: button.y },
      clip: {
        overflow: frame.overflow,
        radius: frame.borderTopLeftRadius,
        border: frame.borderTopWidth,
        fit: image.objectFit,
        animation: image.animationName,
      },
    }
  })
  expect(metrics.button.width).toBe(metrics.avatar.width)
  expect(metrics.button.height).toBe(metrics.avatar.height)
  expect(metrics.clip.overflow).toBe('hidden')
  expect(metrics.clip.fit).toBe('cover')
  expect(metrics.clip.border).toBe('1px')
  expect(metrics.clip.animation).toBe('none')
  expect(
    metrics.clip.radius === '50%' ||
      Math.abs(parseFloat(metrics.clip.radius) - metrics.avatar.width / 2) < 0.5,
  ).toBe(true)
  expect(metrics.portrait.x).toBeGreaterThanOrEqual(metrics.avatar.x)
  expect(metrics.portrait.y).toBeGreaterThanOrEqual(metrics.avatar.y)
  expect(metrics.portrait.x + metrics.portrait.width).toBeLessThanOrEqual(
    metrics.avatar.x + metrics.avatar.width,
  )
  expect(metrics.portrait.y + metrics.portrait.height).toBeLessThanOrEqual(
    metrics.avatar.y + metrics.avatar.height,
  )
  expect(metrics.icon.width).toBeCloseTo(metrics.button.width / 2, 1)
  expect(metrics.icon.height).toBeCloseTo(metrics.button.height / 2, 1)
  expect(metrics.icon.x + metrics.icon.width / 2).toBeCloseTo(
    metrics.buttonOrigin.x + metrics.button.width / 2,
    1,
  )
  expect(metrics.icon.y + metrics.icon.height / 2).toBeCloseTo(
    metrics.buttonOrigin.y + metrics.button.height / 2,
    1,
  )
  const resting = await settings.evaluate((element) => getComputedStyle(element).backgroundColor)
  await settings.hover()
  // 背景色有 150ms 过渡，刚悬停时计算值仍是静止色。
  await expect
    .poll(() => settings.evaluate((element) => getComputedStyle(element).backgroundColor))
    .not.toBe(resting)
  await settings.evaluate((element) => {
    if (!(element instanceof HTMLElement)) throw new Error('设置按钮不可聚焦')
    element.focus({ focusVisible: true })
  })
  expect(await settings.evaluate((element) => getComputedStyle(element).outlineStyle)).toBe('solid')
  await window.mouse.move(1, 1)
}

/** 在真实窗口中拖动分隔线，验证指针捕获在自动收缩后仍能连续工作。 */
async function dragSidebarTo(width: number): Promise<void> {
  const separator = window.getByRole('separator', { name: '调整侧栏宽度' })
  const bounds = await separator.boundingBox()
  await window.mouse.move(bounds!.x + bounds!.width / 2, 350)
  await window.mouse.down()
  await window.mouse.move(width, 350, { steps: 12 })
  await window.mouse.up()
}

test('侧栏底栏设置按钮与工作空间头像同尺寸', async () => {
  await expectFooterControlsAligned()
  await dragSidebarTo(239)
  await expect(window.getByRole('separator', { name: '调整侧栏宽度' })).toHaveAttribute(
    'aria-valuenow',
    '88',
  )
  await expect(window.locator('.profile-copy')).toBeHidden()
  await expectFooterControlsAligned()
})

test('侧栏拖动上限、收缩阈值、头像栏操作及重新展开', async () => {
  const separator = window.getByRole('separator', { name: '调整侧栏宽度' })
  const sidebar = window.getByRole('complementary')
  await expect(window.getByRole('button', { name: '切换侧边栏' })).toHaveCount(0)
  await expect(window.getByRole('button', { name: '分享', exact: true })).toHaveCount(0)
  await dragSidebarTo(550)
  await expect(separator).toHaveAttribute('aria-valuenow', '400')
  expect((await sidebar.boundingBox())!.width).toBe(400)
  await dragSidebarTo(240)
  await expect(separator).toHaveAttribute('aria-valuenow', '240')
  await expect(window.locator('.profile-copy')).toBeVisible()
  await dragSidebarTo(239)
  await expect(separator).toHaveAttribute('aria-valuenow', '88')
  expect((await sidebar.boundingBox())!.width).toBe(88)
  const collapsedSidebarBox = await sidebar.boundingBox()
  const collapsedPinnedBox = await window
    .locator('.pinned-area .pinned-agent')
    .first()
    .boundingBox()
  expect(
    Math.abs(
      collapsedSidebarBox!.x +
        collapsedSidebarBox!.width / 2 -
        (collapsedPinnedBox!.x + collapsedPinnedBox!.width / 2),
    ),
  ).toBeLessThan(1)
  await expect(window.locator('.profile-copy')).toBeHidden()
  await window.getByRole('button', { name: '游戏程序', exact: true }).click()
  await expect(window.getByRole('textbox', { name: '给游戏程序发消息' })).toBeVisible()
  await window.screenshot({ path: test.info().outputPath('desktop-collapsed.png'), scale: 'css' })
  await window.getByRole('button', { name: '游戏程序', exact: true }).click({ button: 'right' })
  await window.getByRole('menuitem', { name: '重命名猫咪', exact: true }).click()
  const collapsedNameInput = window.getByRole('textbox', { name: '猫咪名称', exact: true })
  await expect(collapsedNameInput).toBeFocused()
  await expect(collapsedNameInput).toBeVisible()
  await window.screenshot({
    path: test.info().outputPath('desktop-collapsed-rename.png'),
    scale: 'css',
  })
  await collapsedNameInput.press('Escape')
  await expect(window.getByRole('button', { name: '游戏程序', exact: true })).toBeVisible()
  await window.getByRole('button', { name: '我的工作空间' }).click()
  const workspace = window.getByRole('dialog', { name: '我的工作空间' })
  await expect(workspace).toContainText('8')
  await expect(workspace).toContainText('服务端连接')
  await expect(window.getByRole('combobox', { name: '主题' })).toHaveCount(0)
  await window.keyboard.press('Escape')
  await expect(window.getByRole('button', { name: '我的工作空间' })).toBeFocused()
  await window.getByRole('button', { name: '打开设置' }).click()
  await expect(window.getByRole('dialog', { name: '设置' })).toBeVisible()
  await window.keyboard.press('Escape')
  await window.getByRole('button', { name: '新建', exact: true }).click()
  await window.getByRole('menuitem', { name: '招聘猫咪', exact: true }).click()
  await expect(window.getByRole('dialog', { name: '招聘猫咪' })).toBeVisible()
  await window.keyboard.press('Escape')
  await window.getByRole('button', { name: '搜索助手和消息' }).click()
  await expect(window.getByRole('combobox', { name: '搜索内容' })).toBeFocused()
  await window.keyboard.press('Escape')
  await dragSidebarTo(320)
  await expect(separator).toHaveAttribute('aria-valuenow', '320')
  await expect(window.locator('.profile-copy')).toBeVisible()
  expect(
    await window
      .locator('.app-shell')
      .evaluate((element) => element.scrollWidth <= element.clientWidth),
  ).toBe(true)
})

/** 滚动条占位不变，只在该滚动区域被悬浮时显示滑块。 */
async function expectHoverScrollbar(selector: string): Promise<void> {
  const region = window.locator(selector)
  const thumb = () =>
    region.evaluate(
      (element) => getComputedStyle(element, '::-webkit-scrollbar-thumb').backgroundColor,
    )
  await window.mouse.move(1, 1)
  await expect.poll(thumb).toBe('rgba(0, 0, 0, 0)')
  await region.hover()
  await expect.poll(thumb).not.toBe('rgba(0, 0, 0, 0)')
  await window.mouse.move(1, 1)
  await expect.poll(thumb).toBe('rgba(0, 0, 0, 0)')
}

test('真实主题、系统变化、表单焦点、悬浮滚动条与顶部雾化', async () => {
  await application.evaluate(({ nativeTheme }) => {
    nativeTheme.themeSource = 'light'
  })
  await expect(window.locator('html')).toHaveAttribute('data-theme', 'light')
  await application.evaluate(({ nativeTheme }) => {
    nativeTheme.themeSource = 'dark'
  })
  await expect(window.locator('html')).toHaveAttribute('data-theme', 'dark')
  await window.getByRole('button', { name: '打开设置' }).click()
  const theme = window.getByRole('combobox', { name: '主题' })
  await chooseOption('主题', '浅色')
  await expect(window.locator('html')).toHaveAttribute('data-theme', 'light')
  await application.evaluate(({ nativeTheme }) => {
    nativeTheme.themeSource = 'light'
  })
  await application.evaluate(({ nativeTheme }) => {
    nativeTheme.themeSource = 'dark'
  })
  await expect(window.locator('html')).toHaveAttribute('data-theme', 'light')
  await chooseOption('主题', '深色')
  await window.reload()
  await expect(window.locator('html')).toHaveAttribute('data-theme', 'dark')
  await window.getByRole('button', { name: '打开设置' }).click()
  await expect(theme).toHaveText('深色')
  await theme.focus()
  expect(await theme.evaluate((element) => getComputedStyle(element).outlineStyle)).toBe('none')
  await window.getByRole('button', { name: '模型设置', exact: true }).click()
  const address = window.getByLabel('API 地址')
  await address.click()
  expect(await address.evaluate((element) => getComputedStyle(element).outlineStyle)).toBe('none')
  expect(await address.evaluate((element) => getComputedStyle(element).boxShadow)).toBe('none')
  await expectHoverScrollbar('.settings-pane')
  await window.keyboard.press('Escape')

  expect(
    await window
      .locator('.chat-panel')
      .evaluate((element) => getComputedStyle(element).backgroundColor),
  ).toBe('rgb(17, 17, 17)')
  expect(
    await window
      .locator('.avatar[data-avatar="bombay"] img')
      .first()
      .evaluate((element) => getComputedStyle(element).filter),
  ).toBe('none')
  await expectHoverScrollbar('.agent-groups')
  await expectHoverScrollbar('.messages-scroll')
  const input = window.getByRole('textbox', { name: '给小黑发消息' })
  await input.fill(
    Array.from(
      { length: 24 },
      (_, index) => `第 ${index + 1} 行：验证消息在顶部渐变下自然淡出`,
    ).join('\n'),
  )
  await expectHoverScrollbar('.composer textarea')
  await input.press('Enter')
  expect(
    await window
      .locator('.messages-scroll')
      .evaluate((element) => element.scrollHeight > element.clientHeight),
  ).toBe(true)
  const fog = await window.locator('.chat-header').evaluate((element) => ({
    position: getComputedStyle(element).position,
    blur: getComputedStyle(element, '::before').backdropFilter,
    mask: getComputedStyle(element, '::before').maskImage,
  }))
  expect(fog.position).toBe('absolute')
  expect(fog.blur).toBe('blur(10px)')
  expect(fog.mask).toContain('linear-gradient')
  expect(
    await window
      .locator('.sidebar')
      .evaluate((element) => getComputedStyle(element).borderRightColor),
  ).toBe('rgb(25, 25, 25)')
  const separator = window.getByRole('separator', { name: '调整侧栏宽度' })
  await separator.focus()
  expect(
    await separator.evaluate((element) => getComputedStyle(element, '::after').backgroundColor),
  ).toBe('rgb(37, 37, 37)')
  await input.focus()

  await window.screenshot({ path: test.info().outputPath('desktop-dark.png'), scale: 'css' })
  await window.getByRole('button', { name: '搜索助手和消息' }).click()
  await expectHoverScrollbar('.search-results')
  await window.keyboard.press('Escape')
  await window.getByRole('button', { name: '打开设置' }).click()
  await chooseOption('主题', '跟随系统')
  await application.evaluate(({ nativeTheme }) => {
    nativeTheme.themeSource = 'light'
  })
  await expect(window.locator('html')).toHaveAttribute('data-theme', 'light')
  await window.keyboard.press('Escape')
  await window.screenshot({ path: test.info().outputPath('desktop-fog-light.png'), scale: 'css' })
})

test('侧栏右键菜单、置顶、未读与分组子菜单', async () => {
  const testInfo = test.info()
  const developer = window.getByRole('button', { name: '游戏程序', exact: true })
  await developer.click({ button: 'right' })
  const menu = window.getByRole('menu', { name: '游戏程序的菜单', exact: true })
  await expect(menu).toBeVisible()
  expect(await menu.evaluate((element) => element.matches(':popover-open'))).toBe(true)
  expect(
    await menu
      .getByRole('menuitem', { name: '置顶', exact: true })
      .evaluate((element) => getComputedStyle(element).fontWeight),
  ).toBe('600')
  const viewport = await window.evaluate(() => ({ width: innerWidth, height: innerHeight }))
  const menuBounds = await menu.boundingBox()
  expect(menuBounds!.x).toBeGreaterThanOrEqual(8)
  expect(menuBounds!.y).toBeGreaterThanOrEqual(8)
  expect(menuBounds!.x + menuBounds!.width).toBeLessThanOrEqual(viewport.width - 8)
  expect(menuBounds!.y + menuBounds!.height).toBeLessThanOrEqual(viewport.height - 8)
  await window.screenshot({
    path: testInfo.outputPath('desktop-context-menu.png'),
    scale: 'css',
  })

  await menu.getByRole('menuitem', { name: '标为未读', exact: true }).click()
  await expect(menu).toHaveCount(0)
  const unread = window.getByRole('button', { name: '游戏程序，有未读消息', exact: true })
  await expect(unread).toBeVisible()
  await expect(unread.locator('.agent-unread-dot')).toHaveCount(1)
  await unread.click()
  await expect(window.getByRole('textbox', { name: '给游戏程序发消息', exact: true })).toBeVisible()
  await expect(
    window.locator('.agent-button[aria-label="游戏程序"] .agent-unread-dot'),
  ).toHaveCount(0)

  await window.getByRole('button', { name: '游戏程序', exact: true }).click({ button: 'right' })
  await window.getByRole('menuitem', { name: '置顶', exact: true }).click()
  await expect(window.locator('.pinned-area .pinned-agent')).toHaveCount(2)
  const pinnedDeveloper = window
    .locator('.pinned-area')
    .getByRole('button', { name: '游戏程序', exact: true })
  await expect(pinnedDeveloper).toBeVisible()
  await pinnedDeveloper.click({ button: 'right' })
  await window.getByRole('menuitem', { name: '取消置顶', exact: true }).click()
  await expect(window.locator('.pinned-area .pinned-agent')).toHaveCount(1)
  await expect(window.getByRole('button', { name: '游戏程序', exact: true })).toBeVisible()

  await window.getByRole('button', { name: '游戏程序', exact: true }).click({ button: 'right' })
  const moveTrigger = window.getByRole('menuitem', { name: '移至', exact: true })
  await moveTrigger.click()
  const moveMenu = window.getByRole('menu', { name: '移至分组', exact: true })
  await expect(moveMenu).toBeVisible()
  await expect(moveTrigger).toHaveAttribute('aria-expanded', 'true')
  await expect(moveMenu.getByRole('menuitem', { name: '创建分组', exact: true })).toBeVisible()
  await expect(
    moveMenu.getByRole('menuitemradio', { name: '游戏开发', exact: true }).locator('svg'),
  ).toHaveCount(2)
  await expect(
    moveMenu.getByRole('menuitemradio', { name: '未分组', exact: true }).locator('svg'),
  ).toHaveCount(1)
  const moveBounds = await moveMenu.boundingBox()
  expect(moveBounds!.x).toBeGreaterThanOrEqual(8)
  expect(moveBounds!.y).toBeGreaterThanOrEqual(8)
  expect(moveBounds!.x + moveBounds!.width).toBeLessThanOrEqual(viewport.width - 8)
  expect(moveBounds!.y + moveBounds!.height).toBeLessThanOrEqual(viewport.height - 8)
  await window.screenshot({
    path: testInfo.outputPath('desktop-context-submenu.png'),
    scale: 'css',
  })
  await window.keyboard.press('Escape')
  await expect(moveMenu).toHaveCount(0)
  await expect(moveTrigger).toBeFocused()
  await window.keyboard.press('Escape')
  await expect(menu).toHaveCount(0)
  await expect(developer).toBeFocused()

  await window.getByRole('button', { name: '小黑', exact: true }).click({ button: 'right' })
  await expect(window.getByRole('menuitem', { name: '删除', exact: true })).toBeDisabled()
  await expect(window.getByRole('menuitem', { name: '从侧边栏隐藏', exact: true })).toBeDisabled()
  await window.keyboard.press('Escape')
})

test('侧栏内联重命名与创建分组', async () => {
  const testInfo = test.info()
  await window.getByRole('button', { name: '游戏程序', exact: true }).click({ button: 'right' })
  await window.getByRole('menuitem', { name: '重命名猫咪', exact: true }).click()
  const nameInput = window.getByRole('textbox', { name: '猫咪名称', exact: true })
  await expect(nameInput).toBeFocused()
  await expect(nameInput).toHaveValue('游戏程序')
  await window.screenshot({
    path: testInfo.outputPath('desktop-inline-agent-name.png'),
    scale: 'css',
  })
  await nameInput.fill('程序猫')
  await nameInput.press('Enter')
  await expect(nameInput).toHaveCount(0)
  await expect(window.getByRole('button', { name: '程序猫', exact: true })).toBeVisible()
  await expect(window.getByRole('alert')).toHaveCount(0)

  await window.getByRole('button', { name: '程序猫', exact: true }).click({ button: 'right' })
  await window.getByRole('menuitem', { name: '移至', exact: true }).click()
  await window.getByRole('menuitem', { name: '创建分组', exact: true }).click()
  const groupInput = window.getByRole('textbox', { name: '分组名称', exact: true })
  await expect(groupInput).toBeFocused()
  await expect(groupInput).toHaveValue('新分组')
  await window.screenshot({
    path: testInfo.outputPath('desktop-inline-group-name.png'),
    scale: 'css',
  })
  await groupInput.fill('程序分组')
  await groupInput.press('Enter')
  await expect(groupInput).toHaveCount(0)
  await expect(window.getByRole('heading', { name: '程序分组', exact: true })).toBeVisible()
  await expect(window.getByRole('alert')).toHaveCount(0)
  const group = window.locator('.agent-group', {
    has: window.getByRole('heading', { name: '程序分组', exact: true }),
  })
  await expect(group.getByRole('button', { name: '程序猫', exact: true })).toBeVisible()
  await window.screenshot({
    path: testInfo.outputPath('desktop-inline-group.png'),
    scale: 'css',
  })
})

for (const modal of [
  {
    trigger: '搜索助手和消息',
    title: '搜索',
    inputRole: 'combobox' as const,
    inputName: '搜索内容',
  },
  {
    trigger: '新建',
    menuItem: '招聘猫咪',
    title: '招聘猫咪',
    inputRole: 'textbox' as const,
    inputName: '助手名称',
  },
]) {
  test(`${modal.title}弹窗遮罩覆盖完整窗口并关闭背景拖拽区域`, async () => {
    const trigger = window.getByRole('button', { name: modal.trigger, exact: true })
    const dialog = window.getByRole('dialog', { name: modal.title, exact: true })
    const backgroundRegions = window.locator('.sidebar-toolbar, .chat-header')
    const viewport = await window.evaluate(() => ({ width: innerWidth, height: innerHeight }))
    for (const point of [
      { x: viewport.width - 43, y: 400 },
      { x: 900, y: 20 },
      { x: 700, y: viewport.height - 20 },
      { x: 60, y: 400 },
    ]) {
      await trigger.click()
      if ('menuItem' in modal && modal.menuItem) {
        await window.getByRole('menuitem', { name: modal.menuItem, exact: true }).click()
      }
      await expect(dialog).toBeVisible()
      // 页面合成点击会绕过 Electron 的原生拖拽命中测试，必须同时验证原生区域已关闭。
      expect(
        await backgroundRegions.evaluateAll((elements) =>
          elements.map((element) =>
            getComputedStyle(element).getPropertyValue('-webkit-app-region'),
          ),
        ),
      ).toEqual(['no-drag', 'no-drag'])
      const input = window.getByRole(modal.inputRole, { name: modal.inputName })
      await input.click()
      await input.fill('验证输入')
      await expect(dialog).toBeVisible()
      await expect(input).toHaveValue('验证输入')
      // 弹窗内部的空白区域仍然属于内容，不能被当作遮罩关闭。
      await dialog.locator('.dialog-content').click({ position: { x: 5, y: 5 } })
      await expect(dialog).toBeVisible()
      await window.mouse.click(point.x, point.y)
      await expect(dialog).toHaveCount(0)
      await expect(trigger).toBeFocused()
      expect(
        await backgroundRegions.evaluateAll((elements) =>
          elements.map((element) =>
            getComputedStyle(element).getPropertyValue('-webkit-app-region'),
          ),
        ),
      ).toEqual(['drag', 'drag'])
    }
    await trigger.click()
    if ('menuItem' in modal && modal.menuItem) {
      await window.getByRole('menuitem', { name: modal.menuItem, exact: true }).click()
    }
    await expect(dialog).toBeVisible()
    await window.keyboard.press('Escape')
    await expect(dialog).toHaveCount(0)
    await expect(trigger).toBeFocused()
  })
}

test('工作空间头像是静止图片，悬停不播放猫咪动画', async () => {
  const button = window.getByRole('button', { name: '我的工作空间' })
  const image = button.locator('.profile-avatar img')
  await expect(button.locator('.avatar')).toHaveCount(0)
  await expect(image).toHaveAttribute('src', /workspace-cat/)
  expect(
    await image.evaluate((element) => {
      const portrait = element as HTMLImageElement
      return portrait.complete && portrait.naturalWidth === 256 && portrait.naturalHeight === 256
    }),
  ).toBe(true)
  expect(await image.evaluate((element) => getComputedStyle(element).animationName)).toBe('none')
  const box = await image.boundingBox()
  await window.mouse.move(box!.x + box!.width / 2, box!.y + box!.height / 2)
  await expect(button.locator('.avatar')).toHaveCount(0)
  expect(await image.evaluate((element) => getComputedStyle(element).transform)).toBe('none')
  expect(await image.evaluate((element) => getComputedStyle(element).animationName)).toBe('none')
})

test('猫咪静息、悬停与工作中姿态', async () => {
  await window.emulateMedia({ reducedMotion: 'no-preference' })
  const pinned = window.getByRole('button', { name: '小黑', exact: true }).locator('.avatar')
  const listed = window.getByRole('button', { name: '游戏程序', exact: true }).locator('.avatar')
  const header = window.locator('.chat-header .avatar')
  await expect(pinned).toHaveAttribute('data-pose', 'idle')
  expect((await listed.boundingBox())!.width).toBe(34)
  expect((await listed.boundingBox())!.height).toBe(34)
  expect((await pinned.boundingBox())!.width).toBe(56)
  expect((await pinned.boundingBox())!.height).toBe(56)
  expect((await header.boundingBox())!.width).toBe(22)
  await expect
    .poll(() =>
      pinned.locator('.cat-motion').evaluate((element) => getComputedStyle(element).animationName),
    )
    .toBe('cat-idle')
  await window.screenshot({ path: test.info().outputPath('cat-idle.png'), scale: 'css' })

  const input = window.getByRole('textbox', { name: '给小黑发消息' })
  await input.fill('只是本地草稿')
  await input.press('Enter')
  await expect(window.getByRole('button', { name: '未发送，点击重试' })).toBeVisible()
  await expect(pinned).toHaveAttribute('data-pose', 'idle')

  await window.evaluate(() => {
    globalThis.dispatchEvent(new Event('tombot:agent-activity'))
    globalThis.dispatchEvent(
      new CustomEvent('tombot:agent-activity', { detail: { agentId: '', activity: 'working' } }),
    )
  })
  await expect(pinned).toHaveAttribute('data-pose', 'idle')

  const agentIds = [
    'assistant',
    'developer',
    'producer',
    'artist',
    'audio',
    'coding',
    'news',
    'organizer',
  ]
  const repliesBefore = await window.locator('.assistant-bubble').count()
  await window.evaluate((ids) => {
    for (const agentId of ids) {
      globalThis.dispatchEvent(
        new CustomEvent('tombot:agent-activity', { detail: { agentId, activity: 'working' } }),
      )
    }
  }, agentIds)
  await expect(window.locator('.sidebar .avatar[data-pose="working"]')).toHaveCount(8)
  await expect(window.locator('.sidebar .avatar[data-ear="perk"]')).toHaveCount(8)
  await expect(window.locator('.sidebar .cat-work-mark')).toHaveCount(8)
  await expect(window.getByRole('button', { name: '我的工作空间' }).locator('.avatar')).toHaveCount(
    0,
  )
  await expect(header).toHaveAttribute('data-pose', 'working')
  await expect(window.getByRole('button', { name: '未发送，点击重试' })).toBeVisible()
  expect(await window.locator('.assistant-bubble').count()).toBe(repliesBefore)
  await window.mouse.move(700, 420)
  const pinnedBox = await pinned.boundingBox()
  await window.mouse.move(pinnedBox!.x + 4, pinnedBox!.y + pinnedBox!.height / 2)
  await expect(pinned).toHaveAttribute('data-pose', 'working')
  expect(
    await pinned.evaluate((element) =>
      getComputedStyle(element).getPropertyValue('--look-x').trim(),
    ),
  ).toBe('0')
  await expect
    .poll(() =>
      pinned.locator('.cat-motion').evaluate((element) => getComputedStyle(element).animationName),
    )
    .toBe('cat-work')
  await window.screenshot({ path: test.info().outputPath('cat-working.png'), scale: 'css' })

  await window.evaluate((ids) => {
    for (const agentId of ids) {
      globalThis.dispatchEvent(
        new CustomEvent('tombot:agent-activity', { detail: { agentId, activity: 'idle' } }),
      )
    }
  }, agentIds)
  const listedBox = await listed.boundingBox()
  await window.mouse.move(listedBox!.x + 2, listedBox!.y + listedBox!.height / 2)
  await expect(listed).toHaveAttribute('data-pose', 'hover')
  const lookLeft = Number(
    await listed.evaluate((element) => getComputedStyle(element).getPropertyValue('--look-x')),
  )
  await window.mouse.move(listedBox!.x + listedBox!.width - 2, listedBox!.y + listedBox!.height / 2)
  const lookRight = Number(
    await listed.evaluate((element) => getComputedStyle(element).getPropertyValue('--look-x')),
  )
  expect(lookLeft).toBeLessThan(0)
  expect(lookRight).toBeGreaterThan(0)
  await expect
    .poll(() =>
      listed.locator('.cat-tilt').evaluate((element) => getComputedStyle(element).transform),
    )
    .not.toBe('none')
  await expect(listed).toHaveAttribute('data-ear', 'perk')
  await window.mouse.down()
  await expect(listed).toHaveAttribute('data-pressed', 'true')
  await window.mouse.up()
  await expect(listed).toHaveAttribute('data-pressed', 'false')
  await expect(listed).toHaveAttribute('data-pose', 'hover')
  await expect(window.locator('.sidebar .cat-work-mark')).toHaveCount(0)
  await window.screenshot({ path: test.info().outputPath('cat-hover.png'), scale: 'css' })

  const separator = window.getByRole('separator', { name: '调整侧栏宽度' })
  await separator.focus()
  await separator.press('Home')
  await expect(separator).toHaveAttribute('aria-valuenow', '88')
  const rail = window.getByRole('button', { name: '游戏程序', exact: true }).locator('.avatar')
  const railBox = await rail.boundingBox()
  expect(railBox!.width).toBe(34)
  await window.mouse.move(railBox!.x + 2, railBox!.y + railBox!.height / 2)
  await expect(rail).toHaveAttribute('data-pose', 'hover')
  const railLeft = Number(
    await rail.evaluate((element) => getComputedStyle(element).getPropertyValue('--look-x')),
  )
  await window.mouse.move(railBox!.x + railBox!.width - 2, railBox!.y + railBox!.height / 2)
  const railRight = Number(
    await rail.evaluate((element) => getComputedStyle(element).getPropertyValue('--look-x')),
  )
  expect(railLeft).toBeLessThan(0)
  expect(railRight).toBeGreaterThan(0)
  const pinnedRail = window.getByRole('button', { name: '小黑', exact: true }).locator('.avatar')
  const pinnedRailBox = await pinnedRail.boundingBox()
  expect(pinnedRailBox!.width).toBe(34)
  await window.mouse.move(pinnedRailBox!.x + pinnedRailBox!.width - 2, pinnedRailBox!.y + 8)
  await expect(pinnedRail).toHaveAttribute('data-pose', 'hover')

  await window.emulateMedia({ reducedMotion: 'reduce' })
  await window.evaluate(() => {
    globalThis.dispatchEvent(
      new CustomEvent('tombot:agent-activity', {
        detail: { agentId: 'assistant', activity: 'working' },
      }),
    )
  })
  await window.mouse.move(pinnedRailBox!.x + 4, pinnedRailBox!.y + pinnedRailBox!.height / 2)
  await expect(pinnedRail).toHaveAttribute('data-pose', 'idle')
  await expect
    .poll(() =>
      pinnedRail
        .locator('.cat-motion')
        .evaluate((element) => getComputedStyle(element).animationName),
    )
    .toBe('none')
  await expect(pinnedRail.locator('.cat-work-mark')).toHaveCount(0)
  await expect(pinnedRail).toHaveAttribute('data-eye', 'open')
  await expect(pinnedRail).toHaveAttribute('data-ear', 'rest')
  expect(
    await pinnedRail.evaluate((element) =>
      getComputedStyle(element).getPropertyValue('--look-x').trim(),
    ),
  ).toBe('0')
})

function boxesOverlap(
  first: { x: number; y: number; width: number; height: number },
  second: { x: number; y: number; width: number; height: number },
): boolean {
  return (
    first.x < second.x + second.width &&
    first.x + first.width > second.x &&
    first.y < second.y + second.height &&
    first.y + first.height > second.y
  )
}

test('消息菜单、悬停按钮、回复、表情和时间分隔', async () => {
  const artifacts = '/opt/cursor/artifacts'
  await mkdir(artifacts, { recursive: true })
  const input = window.getByRole('textbox', { name: '给小黑发消息' })
  await input.fill('原来的想法')
  await input.press('Enter')
  await expect(window.getByRole('log')).toContainText('原来的想法')
  await updateServerWorkspace((workspace) => {
    const agent = workspace.agents.find((item) => item.id === workspace.activeAgentId)
    const saved = agent?.messages.at(-1)
    if (!agent || !saved?.createdAt) throw new Error('服务端没有刚保存的消息')
    const sentAt = new Date(saved.createdAt).getTime()
    saved.createdAt = new Date(sentAt - 20 * 60 * 1000).toISOString()
    agent.messages.unshift({
      id: 'assistant-message',
      role: 'assistant',
      content: '助手已经看到这句。',
      createdAt: new Date(sentAt - 30 * 60 * 1000).toISOString(),
      status: 'sent',
      requestId: 'req-e2e',
      reactions: [{ emoji: '👍', count: 2 }],
    })
  })
  await expect(window.getByText('助手已经看到这句。')).toBeVisible()
  await expect(window.locator('.time-separator')).toHaveCount(2)
  await expect(window.getByRole('log')).not.toContainText('· 未发送')
  await window.locator('.messages-scroll').evaluate((element) => {
    element.scrollTop = element.scrollHeight
  })
  const viewport = await window.evaluate(() => ({ width: innerWidth, height: innerHeight }))
  await window.screenshot({
    path: `${artifacts}/message-time-separators.png`,
    scale: 'css',
  })

  const botRow = window.locator('.message-row.assistant-row', { hasText: '助手已经看到这句。' })
  const botBubble = botRow.locator('.message-bubble')
  await botBubble.hover()
  const botActions = botRow.locator('.message-actions')
  await expect(botActions).toBeVisible()
  const botBubbleBox = (await botBubble.boundingBox())!
  const botActionBox = (await botActions.boundingBox())!
  expect(botActionBox.x).toBeGreaterThanOrEqual(botBubbleBox.x + botBubbleBox.width - 1)
  expect(botActionBox.x + botActionBox.width).toBeLessThanOrEqual(viewport.width - 8)
  expect(boxesOverlap(botActionBox, botBubbleBox)).toBe(false)
  const idleAction = botActions.getByRole('button', { name: '添加表情' })
  const replyAction = botActions.getByRole('button', { name: '回复' })
  await expect(idleAction).toHaveCSS('background-color', 'rgba(0, 0, 0, 0)')
  await expect(idleAction).toHaveCSS('border-top-width', '0px')
  await expect(idleAction).toHaveCSS('width', '28px')
  await expect(idleAction).toHaveCSS('height', '28px')
  await window.screenshot({ path: `${artifacts}/message-hover-buttons-idle.png`, scale: 'css' })
  await replyAction.hover()
  await expect(replyAction).not.toHaveCSS('background-color', 'rgba(0, 0, 0, 0)')
  await window.screenshot({ path: `${artifacts}/message-hover-button-hovered.png`, scale: 'css' })
  await replyAction.evaluate((element: HTMLButtonElement) => element.focus({ focusVisible: true }))
  await expect(replyAction).toHaveCSS('outline-width', '2px')

  const userRow = window.locator('.message-row.user-row', { hasText: '原来的想法' })
  const userBubble = userRow.locator('.message-bubble')
  await userBubble.hover()
  const userActionBox = (await userRow.locator('.message-actions').boundingBox())!
  const userBubbleBox = (await userBubble.boundingBox())!
  expect(userActionBox.x + userActionBox.width).toBeLessThanOrEqual(userBubbleBox.x + 1)
  expect(userActionBox.x).toBeGreaterThanOrEqual(8)
  expect(boxesOverlap(userActionBox, userBubbleBox)).toBe(false)

  await botBubble.click({ button: 'right' })
  const menu = window.getByRole('menu', { name: '消息操作' })
  await expect(menu).toBeVisible()
  await expect(menu.getByRole('menuitem', { name: '回复', exact: true })).toBeVisible()
  await expect(menu.getByRole('menuitem', { name: '复制', exact: true })).toBeVisible()
  await expect(menu.getByRole('menuitem', { name: '复制请求 ID', exact: true })).toBeVisible()
  await expect(menu.getByRole('menuitem', { name: '重试' })).toHaveCount(0)
  const menuBounds = (await menu.boundingBox())!
  expect(menuBounds.x).toBeGreaterThanOrEqual(8)
  expect(menuBounds.y).toBeGreaterThanOrEqual(8)
  expect(menuBounds.x + menuBounds.width).toBeLessThanOrEqual(viewport.width - 8)
  expect(menuBounds.y + menuBounds.height).toBeLessThanOrEqual(viewport.height - 8)
  await window.screenshot({ path: `${artifacts}/message-context-menu.png`, scale: 'css' })
  await menu.getByRole('button', { name: '更多表情' }).click()
  const expanded = (await menu.boundingBox())!
  expect(expanded.x).toBeGreaterThanOrEqual(8)
  expect(expanded.y).toBeGreaterThanOrEqual(8)
  expect(expanded.x + expanded.width).toBeLessThanOrEqual(viewport.width - 8)
  expect(expanded.y + expanded.height).toBeLessThanOrEqual(viewport.height - 8)
  await window.keyboard.press('Escape')
  await expect(menu).toHaveCount(0)

  await botBubble.click({ button: 'right' })
  await window.getByRole('button', { name: '回应❤️' }).click()
  await expect(window.getByRole('button', { name: '❤️，点击取消' })).toBeVisible()
  await expect(window.getByRole('button', { name: '👍 2，点击取消' })).toBeVisible()
  await userBubble.click({ button: 'right' })
  await window.getByRole('button', { name: '回应🎉' }).click()
  await expect(userBubble.getByRole('button', { name: '🎉，点击取消' })).toBeVisible()
  await expect(botBubble.locator('.reaction-pill')).toHaveCount(2)
  await expect(window.locator('.message-line > .reaction-pills')).toHaveCount(0)
  await window.screenshot({ path: `${artifacts}/message-reactions-inside.png`, scale: 'css' })

  await userBubble.hover()
  await userRow.getByRole('button', { name: '回复' }).click()
  await expect(window.locator('.composer-reply')).toContainText('原来的想法')
  await window.screenshot({ path: `${artifacts}/message-reply-preview.png`, scale: 'css' })
  await input.fill('接着说')
  await input.press('Enter')
  const quote = window.getByRole('button', { name: '查看原消息：原来的想法' })
  await expect(quote).toBeVisible()
  await window.screenshot({ path: `${artifacts}/message-reply-quote.png`, scale: 'css' })
  await quote.click()
  const original = (await window.locator('.message-text', { hasText: '原来的想法' }).boundingBox())!
  expect(original.y).toBeGreaterThanOrEqual(0)
  expect(original.y).toBeLessThan(viewport.height)

  await window.getByRole('button', { name: '未发送，点击重试' }).first().click()
  await expect(window.getByRole('alert')).toContainText('尚未配置可用模型')
  await reloadPersisted()
  await expect(window.getByRole('button', { name: '❤️，点击取消' })).toBeVisible()
  await expect(quote).toBeVisible()
})

test('点击顶部猫咪打开右侧面板，页签可切换并记住模型', async () => {
  const artifacts = '/opt/cursor/artifacts'
  await mkdir(artifacts, { recursive: true })
  const trigger = window.getByRole('button', { name: '查看小黑资料', exact: true })
  const chevron = trigger.locator('.pill-chevron')
  await expect(chevron).toHaveClass(/lucide-chevron-right/)
  await expect(chevron).toHaveCSS('opacity', '0')
  const idleWidth = (await trigger.boundingBox())!.width
  await trigger.screenshot({ path: `${artifacts}/chat-chip-idle.png` })
  await trigger.hover()
  await expect(chevron).toHaveCSS('opacity', '1')
  expect((await trigger.boundingBox())!.width).toBe(idleWidth)
  await trigger.screenshot({ path: `${artifacts}/chat-chip-hover.png` })
  await window.getByRole('textbox', { name: '给小黑发消息' }).hover()
  await expect(chevron).toHaveCSS('opacity', '0')
  await trigger.evaluate((element: HTMLElement) => {
    element.focus({ focusVisible: true } as FocusOptions)
  })
  await expect(chevron).toHaveCSS('opacity', '1')
  expect((await trigger.boundingBox())!.width).toBe(idleWidth)
  await trigger.evaluate((element: HTMLElement) => element.blur())
  await window.screenshot({ path: `${artifacts}/cat-panel-closed.png`, scale: 'css' })
  await trigger.click()
  const panel = window.getByRole('complementary', { name: '小黑' })
  await expect(panel).toBeVisible()
  const close = panel.getByRole('button', { name: '关闭猫咪面板' })
  const search = window.getByRole('button', { name: '搜索助手和消息' })
  const create = window.getByRole('button', { name: '新建' })
  await expect(close).toHaveClass(/round-button/)
  const closeBox = (await close.boundingBox())!
  const searchBox = (await search.boundingBox())!
  const createBox = (await create.boundingBox())!
  expect(closeBox.width).toBe(searchBox.width)
  expect(closeBox.height).toBe(searchBox.height)
  expect(closeBox.width).toBe(createBox.width)
  expect(closeBox.height).toBe(createBox.height)
  await window.screenshot({ path: `${artifacts}/panel-close-compare.png`, scale: 'css' })
  await expect(trigger).toHaveAttribute('aria-expanded', 'true')
  await expect(panel.getByRole('tab', { name: '详情', selected: true })).toBeVisible()
  await expect(panel.getByRole('combobox', { name: '模型', exact: true })).toHaveText('GPT-4o')
  await window.screenshot({ path: `${artifacts}/cat-panel-details.png`, scale: 'css' })
  await panel.getByRole('tab', { name: '资料库', exact: true }).click()
  await expect(panel.getByRole('status')).toContainText('资料库还是空的')
  await window.screenshot({ path: `${artifacts}/cat-panel-library.png`, scale: 'css' })
  await panel.getByRole('tab', { name: '电脑', exact: true }).click()
  await expect(panel.getByRole('status')).toContainText('未连接')
  await expect(panel.getByRole('button', { name: '开始准备', exact: true })).toBeVisible()
  await window.screenshot({ path: `${artifacts}/cat-panel-computer.png`, scale: 'css' })
  await panel.getByRole('tab', { name: '记忆', exact: true }).click()
  await expect(panel.getByRole('status')).toHaveText('还没有记忆')
  await expect(panel.getByRole('button', { name: '记下' })).toHaveCount(0)
  await window.screenshot({ path: `${artifacts}/cat-memory.png`, scale: 'css' })
  await panel.getByRole('tab', { name: '定时', exact: true }).click()
  await expect(panel.getByRole('status')).toHaveText('还没有定时任务')
  await expect(panel.getByRole('button', { name: '添加定时任务' })).toHaveCount(0)
  await window.screenshot({ path: `${artifacts}/cat-routines.png`, scale: 'css' })
  await trigger.click()
  await expect(panel).toHaveCount(0)
  await trigger.click()
  await panel.getByRole('combobox', { name: '模型', exact: true }).click()
  await window.getByRole('option', { name: 'Claude Sonnet', exact: true }).click()
  await reloadPersisted()
  await trigger.click()
  await expect(panel.getByRole('combobox', { name: '模型', exact: true })).toHaveText(
    'Claude Sonnet',
  )
  await window.keyboard.press('Escape')
  await expect(panel).toHaveCount(0)
})

test('资料库按时间分组展示产物，电脑在没有地址时保持未连接', async () => {
  const artifacts = '/opt/cursor/artifacts'
  await mkdir(artifacts, { recursive: true })
  const preview =
    'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAoAAAAKCAYAAACNMs+9AAAAFUlEQVR42mP8z8BQz0AEYBxVSF+FABJADveWkH6oAAAAAElFTkSuQmCC'
  await updateServerWorkspace((workspace) => {
    const agent = workspace.agents.find((item) => item.id === 'assistant')
    if (!agent) throw new Error('没有找到小黑')
    const now = new Date()
    const day = now.getDay()
    const mondayOffset = day === 0 ? 6 : day - 1
    const weekStart = new Date(now.getFullYear(), now.getMonth(), now.getDate() - mondayOffset, 12)
    const older = new Date(now.getFullYear(), now.getMonth(), now.getDate() - 10, 11)
    agent.artifacts = [
      ...Array.from({ length: 6 }, (_, index) => ({
        id: `today-${index}`,
        title: `今天的文件 ${index + 1}`,
        kind: index === 5 ? 'image' : 'file',
        createdAt: new Date(now.getTime() - (6 - index) * 60_000).toISOString(),
        ...(index === 5 ? { preview } : {}),
      })),
      { id: 'week', title: '本周录像', kind: 'video', createdAt: weekStart.toISOString() },
      { id: 'older', title: '更早的草稿', kind: 'file', createdAt: older.toISOString() },
    ]
    agent.draft = ''
  })
  await window.getByRole('button', { name: '查看小黑资料', exact: true }).click()
  const panel = window.getByRole('complementary', { name: '小黑' })
  await panel.getByRole('tab', { name: '资料库', exact: true }).click()
  await expect(panel.getByRole('region', { name: '今天' })).toBeVisible()
  await expect(panel.getByRole('region', { name: '更早' })).toBeVisible()
  await expect(panel.getByText('今天的文件 6')).toBeVisible()
  await expect(panel.getByText('今天的文件 1')).toHaveCount(0)
  await expect(panel.locator('li img')).toBeVisible()
  await window.screenshot({ path: `${artifacts}/cat-panel-library-grouped.png`, scale: 'css' })
  await panel.getByRole('button', { name: '展开', exact: true }).click()
  await expect(panel.getByText('今天的文件 1')).toBeVisible()
  await panel.getByRole('tab', { name: '电脑', exact: true }).click()
  await expect(panel.getByRole('status')).toHaveText('未连接')
  await window.screenshot({ path: `${artifacts}/cat-panel-computer.png`, scale: 'css' })
  await panel.getByLabel('远端电脑地址').fill('javascript:alert(1)')
  await panel.getByRole('button', { name: '保存地址', exact: true }).click()
  await expect(panel.getByRole('alert')).toContainText('HTTP 或 HTTPS')
})

test('连接电脑使用这只猫咪的工作区，断开后回到未连接', async () => {
  const artifacts = '/opt/cursor/artifacts'
  await mkdir(artifacts, { recursive: true })
  await window.getByRole('button', { name: '查看小黑资料', exact: true }).click()
  const panel = window.getByRole('complementary', { name: '小黑' })
  await panel.getByRole('tab', { name: '电脑', exact: true }).click()
  await expect(panel.getByRole('status')).toHaveText('未连接')
  await panel.getByRole('button', { name: '连接电脑', exact: true }).click()
  await expect(panel.getByRole('status')).toHaveText('运行中')
  await expect(panel.getByTitle('小黑的远端电脑')).toBeVisible()
  const view = window.frameLocator('[title="小黑的远端电脑"]')
  await expect(view.getByRole('heading', { name: '引导' })).toBeVisible()
  await expect(view.getByText('尚未开始')).toBeVisible()
  await expect(view.getByRole('heading', { name: '文件' })).toBeVisible()
  await expect(view.getByRole('heading', { name: '终端' })).toBeVisible()
  await view.locator('body').screenshot({ path: `${artifacts}/computer-view.png` })
  await window.screenshot({ path: `${artifacts}/computer-connected.png`, scale: 'css' })
  await panel.getByRole('button', { name: '全屏查看', exact: true }).click()
  await expect(panel.getByRole('button', { name: '还原', exact: true })).toBeVisible()
  await panel.getByRole('button', { name: '断开', exact: true }).click()
  await expect(panel.getByRole('status')).toHaveText('未连接')
  await window.screenshot({ path: `${artifacts}/computer-disconnected.png`, scale: 'css' })
})

test('对话里记住一件事并创建定时任务后，界面能看到、能暂停、能右键删除', async () => {
  test.setTimeout(120_000)
  const artifacts = '/opt/cursor/artifacts'
  await mkdir(artifacts, { recursive: true })
  const model = await startToolModel()
  try {
    await window.getByRole('button', { name: '打开设置' }).click()
    await window.getByRole('button', { name: '模型设置', exact: true }).click()
    await window.getByRole('textbox', { name: 'API 地址', exact: true }).fill(model.url)
    await window.getByRole('textbox', { name: '模型名称', exact: true }).fill('fixture-model')
    await window.getByLabel('API 密钥').fill('fixture-key')
    await window.keyboard.press('Escape')
    await waitForModel('fixture-model', true)
    await window
      .getByRole('textbox', { name: '给小黑发消息' })
      .fill('请记住我每周三晨跑，并在当天早上9点提醒我')
    await window.getByRole('button', { name: '保存为本地草稿' }).click()
    await expect
      .poll(async () => window.locator('.message-text').allTextContents(), { timeout: 45_000 })
      .toContain('已经记下，并设好周三早上的提醒。')
    await expect(window.getByRole('alert')).toHaveCount(0)
    await window.getByRole('button', { name: '查看小黑资料', exact: true }).click()
    const panel = window.getByRole('complementary', { name: '小黑' })
    await panel.getByRole('tab', { name: '记忆', exact: true }).click()
    await expect(panel.getByText('晨跑', { exact: true })).toBeVisible()
    await expect(panel.getByRole('button', { name: '记下' })).toHaveCount(0)
    await expect(panel.getByRole('button', { name: '删除' })).toHaveCount(0)
    await window.screenshot({ path: `${artifacts}/cat-memory.png`, scale: 'css' })
    await panel.getByRole('tab', { name: '定时', exact: true }).click()
    await expect(panel.getByText('晨跑提醒', { exact: true })).toBeVisible()
    await expect(panel.getByRole('button', { name: '添加定时任务' })).toHaveCount(0)
    await window.screenshot({ path: `${artifacts}/cat-routines.png`, scale: 'css' })
    await panel.getByRole('switch', { name: '暂停晨跑提醒' }).click()
    await expect(panel.getByRole('switch', { name: '启用晨跑提醒' })).toBeVisible()
    await panel.getByText('晨跑提醒', { exact: true }).click({ button: 'right' })
    await expect(window.getByRole('menuitem', { name: '删除' })).toBeVisible()
    await window.screenshot({ path: `${artifacts}/cat-routine-menu.png`, scale: 'css' })
    await window.getByRole('menuitem', { name: '删除' }).click()
    await expect(panel.getByRole('status')).toHaveText('还没有定时任务')
  } finally {
    await model.close()
  }
})

test('群聊、私信和附件沿用原来的气泡', async () => {
  test.setTimeout(120_000)
  const artifacts = '/opt/cursor/artifacts'
  await mkdir(artifacts, { recursive: true })
  const model = await startFixtureModel([['群', '里收到了']])
  try {
    await window.getByRole('button', { name: '打开设置' }).click()
    await window.getByRole('button', { name: '模型设置', exact: true }).click()
    await window.getByRole('textbox', { name: 'API 地址', exact: true }).fill(model.url)
    await window.getByRole('textbox', { name: '模型名称', exact: true }).fill('fixture-model')
    await window.getByLabel('API 密钥').fill('fixture-key')
    await window.keyboard.press('Escape')
    await waitForModel('fixture-model', true)
    await window.getByRole('button', { name: '新建', exact: true }).click()
    await window.screenshot({ path: `${artifacts}/create-menu.png`, scale: 'css' })
    await window.getByRole('menuitem', { name: '创建群聊', exact: true }).click()
    await window.getByRole('textbox', { name: '群名称', exact: true }).fill('晨会')
    await window.getByRole('checkbox', { name: '小黑' }).check()
    await window.getByRole('button', { name: '创建群聊', exact: true }).click()
    await window.getByRole('textbox', { name: '给晨会发消息' }).fill('大家看一下今天的安排')
    await window.getByRole('button', { name: '发送', exact: true }).click()
    await expect(window.getByRole('log', { name: '晨会' })).toContainText('群里收到了', {
      timeout: 30_000,
    })
    await window.screenshot({ path: `${artifacts}/group-chat.png`, scale: 'css' })
    await window.getByRole('button', { name: '小黑', exact: true }).click()
    await window
      .locator('input[type="file"]')
      .setInputFiles('/agent/repos/tombot/desktop/src/renderer/src/assets/cats/frames/bombay.png')
    await expect(window.getByText('bombay.png')).toBeVisible()
    await window.getByRole('textbox', { name: '给小黑发消息' }).fill('看看这张图')
    await window.getByRole('button', { name: '保存为本地草稿' }).click()
    await expect(window.getByRole('log').locator('img[alt="bombay.png"]')).toBeVisible()
    await expect(window.getByRole('log')).toContainText('群里收到了', { timeout: 20_000 })
    await window.screenshot({ path: `${artifacts}/attachment-message.png`, scale: 'css' })
  } finally {
    await model.close()
  }
})

test('猫咪主动发的私信出现在侧栏', async () => {
  test.setTimeout(120_000)
  const artifacts = '/opt/cursor/artifacts'
  await mkdir(artifacts, { recursive: true })
  const model = await startDmModel()
  try {
    await window.getByRole('button', { name: '打开设置' }).click()
    await window.getByRole('button', { name: '模型设置', exact: true }).click()
    await window.getByRole('textbox', { name: 'API 地址', exact: true }).fill(model.url)
    await window.getByRole('textbox', { name: '模型名称', exact: true }).fill('fixture-model')
    await window.getByLabel('API 密钥').fill('fixture-key')
    await window.keyboard.press('Escape')
    await waitForModel('fixture-model', true)
    const login = await fetch(`${companion.url}/api/v1/auth/login`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ password, deviceName: '端到端', platform: 'linux' }),
    })
    const token = ((await login.json()) as { device_token: string }).device_token
    await fetch(`${companion.url}/api/v1/workspace/agents/developer/ready`, {
      method: 'POST',
      headers: { authorization: `Bearer ${token}` },
    })
    await window.getByRole('textbox', { name: '给小黑发消息' }).fill('请私下告诉游戏程序一声')
    await window.getByRole('button', { name: '保存为本地草稿' }).click()
    const notice = window.getByRole('button', { name: /已发消息给/ })
    await expect(notice).toBeVisible({ timeout: 30_000 })
    await window.screenshot({ path: `${artifacts}/cat-dm.png`, scale: 'css' })
    await notice.click()
    const dialog = window.getByRole('dialog', { name: '小黑 ↔ 游戏程序' })
    await expect(dialog).toBeVisible()
    await expect(dialog.getByText('晨会我先看了')).toBeVisible()
    await expect(dialog.getByText('好的')).toBeVisible()
    await expect(dialog.locator('.dm-capsule')).toContainText('小黑')
    await expect(dialog.locator('.dm-capsule')).toContainText('游戏程序')
    await expect(dialog.locator('.dm-bubble')).toHaveCount(2)
    await expect(dialog.locator('.user-bubble, .dialog-heading')).toHaveCount(0)
    await expect(dialog.getByRole('textbox')).toHaveCount(0)
    const viewportHeight = await window.evaluate(() => innerHeight)
    const dialogBox = await dialog.boundingBox()
    expect(dialogBox!.height).toBeGreaterThan(viewportHeight * 0.7)
    const capsuleBox = await dialog.locator('.dm-capsule').boundingBox()
    const firstName = await dialog.locator('.dm-name').first().boundingBox()
    expect(firstName!.y).toBeGreaterThan(capsuleBox!.y + capsuleBox!.height + 16)
    const thread = dialog.locator('.dm-thread')
    expect(await thread.evaluate((element) => element.scrollTop)).toBe(0)
    await window.screenshot({ path: `${artifacts}/cat-dm-open.png`, scale: 'css' })
  } finally {
    await model.close()
  }
})

test('流式开关打开时回复逐段出现，关闭时完成后一次出现', async () => {
  test.setTimeout(120_000)
  const artifacts = '/opt/cursor/artifacts'
  await mkdir(artifacts, { recursive: true })
  const model = await startFixtureModel([
    ['你', '好'],
    ['甲', '乙'],
  ])
  try {
    await window.getByRole('button', { name: '打开设置' }).click()
    await window.getByRole('button', { name: '模型设置', exact: true }).click()
    await window.getByRole('textbox', { name: 'API 地址', exact: true }).fill(model.url)
    await window.getByRole('textbox', { name: '模型名称', exact: true }).fill('fixture-model')
    await window.getByLabel('API 密钥').fill('fixture-key')
    await expect(window.getByRole('switch', { name: /流式响应/ })).toBeChecked()
    await window.keyboard.press('Escape')
    await waitForModel('fixture-model', true)
    await window.getByRole('textbox', { name: '给小黑发消息' }).fill('打个招呼')
    await window.getByRole('button', { name: '保存为本地草稿' }).click()
    await expect
      .poll(async () => window.locator('.message-text').allTextContents(), { timeout: 20_000 })
      .toContain('你')
    await window.screenshot({ path: `${artifacts}/stream-in-progress.png`, scale: 'css' })
    await expect
      .poll(async () => window.locator('.message-text').allTextContents(), { timeout: 20_000 })
      .toContain('你好')

    await window.getByRole('button', { name: '打开设置' }).click()
    await window.getByRole('button', { name: '模型设置', exact: true }).click()
    await window.getByRole('switch', { name: /流式响应/ }).click()
    await expect(window.getByRole('switch', { name: /流式响应/ })).not.toBeChecked()
    await window.keyboard.press('Escape')
    await waitForModel('fixture-model', false)
    await window.getByRole('textbox', { name: '给小黑发消息' }).fill('再来一次')
    await window.getByRole('button', { name: '保存为本地草稿' }).click()
    const started = Date.now()
    let sawPartial = false
    while (Date.now() - started < 1_200) {
      const texts = await window.locator('.message-text').allTextContents()
      if (texts.some((text) => text.includes('甲') && !text.includes('甲乙'))) sawPartial = true
      await window.waitForTimeout(100)
    }
    expect(sawPartial).toBe(false)
    await expect
      .poll(async () => window.locator('.message-text').allTextContents(), { timeout: 20_000 })
      .toContain('甲乙')
    await expect(window.getByRole('alert')).toHaveCount(0)
  } finally {
    await model.close()
  }
})

/** 等设置写进服务。主进程的请求不会出现在页面网络记录里。 */
async function waitForModel(modelName: string, isStreaming: boolean): Promise<void> {
  const login = await fetch(`${companion.url}/api/v1/auth/login`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ password, deviceName: '端到端', platform: 'linux' }),
  })
  if (!login.ok) throw new Error(`登录服务失败：${login.status}`)
  const token = ((await login.json()) as { device_token: string }).device_token
  await expect
    .poll(
      async () => {
        const response = await fetch(`${companion.url}/api/v1/model-settings`, {
          headers: { authorization: `Bearer ${token}` },
        })
        if (!response.ok) return ''
        const body = (await response.json()) as { modelName?: string; isStreaming?: boolean }
        return body.modelName === modelName && body.isStreaming === isStreaming ? 'ready' : ''
      },
      { timeout: 15_000 },
    )
    .toBe('ready')
}

/** 前两轮分别调用记忆和例程工具，第三轮返回最终回复。 */
function startToolModel(): Promise<{ url: string; close: () => Promise<void> }> {
  let step = 0
  const server = createServer((request, response) => {
    void (async () => {
      try {
        await new Promise<void>((resolve, reject) => {
          request.on('end', () => resolve())
          request.on('error', reject)
          request.resume()
        })
        if (request.method !== 'POST' || !request.url?.endsWith('/chat/completions')) {
          response.writeHead(404).end()
          return
        }
        const current = step
        step += 1
        response.writeHead(200, {
          'content-type': 'text/event-stream',
          'cache-control': 'no-cache',
        })
        if (current === 0) {
          writeToolCall(
            response,
            'call-memory',
            'memory_write',
            '{"title":"晨跑","content":"用户每周三晨跑"}',
          )
        } else if (current === 1) {
          writeToolCall(
            response,
            'call-routine',
            'routine_upsert',
            '{"name":"晨跑提醒","cron":"0 9 * * 3","prompt":"提醒用户周三晨跑"}',
          )
        } else {
          response.write(
            `data: ${JSON.stringify({ choices: [{ index: 0, delta: { content: '已经记下，并设好周三早上的提醒。' }, finish_reason: 'stop' }] })}\n\n`,
          )
          response.write('data: [DONE]\n\n')
          response.end()
        }
      } catch {
        response.destroy()
      }
    })()
  })
  return new Promise((resolve, reject) => {
    server.listen(0, '127.0.0.1', () => {
      const address = server.address()
      if (!address || typeof address === 'string') {
        reject(new Error('模型端口没有打开'))
        return
      }
      resolve({
        url: `http://127.0.0.1:${address.port}/v1`,
        close: () =>
          new Promise((done, fail) => {
            server.close((error) => (error ? fail(error) : done()))
          }),
      })
    })
    server.on('error', reject)
  })
}

function startDmModel(): Promise<{ url: string; close: () => Promise<void> }> {
  let step = 0
  const server = createServer((request, response) => {
    void (async () => {
      try {
        await new Promise<void>((resolve, reject) => {
          request.on('end', () => resolve())
          request.on('error', reject)
          request.resume()
        })
        if (request.method !== 'POST' || !request.url?.endsWith('/chat/completions')) {
          response.writeHead(404).end()
          return
        }
        const current = step
        step += 1
        response.writeHead(200, {
          'content-type': 'text/event-stream',
          'cache-control': 'no-cache',
        })
        if (current === 0) {
          writeToolCall(
            response,
            'call-dm',
            'message_bot',
            '{"bot_id":"developer","message":"晨会我先看了。倒地保持最后一帧，移动改成爬行，倒地开火不要再把整段动作锁死。副武器走自己的一套键，按侧向速度选择左右并加上角度滞回，避免来回跳切。缺片先标清楚，不要静默。等用户说合再合，资源先留在当前版本上，不要提前开合。静止时末帧停住，移动只播爬行，开火从瞄准接射击，不要整段锁死。沙鹰只用手枪那一组动作，步枪的射击和战斗待机都不要混进来。侧向速度过了阈值再换左右，并留一点滞回，免得准星附近来回跳。缺的片段先写明，等用户确认后再接。"}',
          )
          return
        }
        response.write(
          `data: ${JSON.stringify({ choices: [{ index: 0, delta: { content: '好的' }, finish_reason: 'stop' }] })}\n\n`,
        )
        response.write('data: [DONE]\n\n')
        response.end()
      } catch {
        response.destroy()
      }
    })()
  })
  return new Promise((resolve, reject) => {
    server.listen(0, '127.0.0.1', () => {
      const address = server.address()
      if (!address || typeof address === 'string') {
        reject(new Error('模型端口没有打开'))
        return
      }
      resolve({
        url: `http://127.0.0.1:${address.port}/v1`,
        close: () =>
          new Promise((done, fail) => {
            server.close((error) => (error ? fail(error) : done()))
          }),
      })
    })
    server.on('error', reject)
  })
}

function writeToolCall(response: ServerResponse, id: string, name: string, args: string): void {
  response.write(
    `data: ${JSON.stringify({ choices: [{ index: 0, delta: { tool_calls: [{ index: 0, id, function: { name, arguments: args } }] } }] })}\n\n`,
  )
  response.write(
    `data: ${JSON.stringify({ choices: [{ index: 0, delta: {}, finish_reason: 'tool_calls' }] })}\n\n`,
  )
  response.write('data: [DONE]\n\n')
  response.end()
}

/** 按 OpenAI 兼容流式分两段返回。两段之间留出时间，方便截到进行中的回复。 */
function startFixtureModel(
  pairs: Array<[string, string]>,
): Promise<{ url: string; close: () => Promise<void> }> {
  let index = 0
  const server = createServer((request, response) => {
    void (async () => {
      try {
        await new Promise<void>((resolve, reject) => {
          request.on('end', () => resolve())
          request.on('error', reject)
          request.resume()
        })
        if (request.method !== 'POST' || !request.url?.endsWith('/chat/completions')) {
          response.writeHead(404).end()
          return
        }
        const pair = pairs[Math.min(index, pairs.length - 1)] ?? ['', '']
        index += 1
        response.writeHead(200, {
          'content-type': 'text/event-stream',
          'cache-control': 'no-cache',
        })
        response.write(
          `data: ${JSON.stringify({ choices: [{ index: 0, delta: { content: pair[0] } }] })}\n\n`,
        )
        await new Promise((resolve) => setTimeout(resolve, 1_600))
        response.write(
          `data: ${JSON.stringify({ choices: [{ index: 0, delta: { content: pair[1] }, finish_reason: 'stop' }] })}\n\n`,
        )
        response.write('data: [DONE]\n\n')
        response.end()
      } catch {
        response.destroy()
      }
    })()
  })
  return new Promise((resolve, reject) => {
    server.listen(0, '127.0.0.1', () => {
      const address = server.address()
      if (!address || typeof address === 'string') {
        reject(new Error('模型端口没有打开'))
        return
      }
      resolve({
        url: `http://127.0.0.1:${address.port}/v1`,
        close: () =>
          new Promise((done, fail) => {
            server.close((error) => (error ? fail(error) : done()))
          }),
      })
    })
    server.on('error', reject)
  })
}
