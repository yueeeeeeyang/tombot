import {
  _electron as electron,
  expect,
  test,
  type ElectronApplication,
  type Page,
} from '@playwright/test'
import { mkdir, mkdtemp, readFile, rm, writeFile } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { pathToFileURL } from 'node:url'
import { Companion } from './companionServer'
import type { AppWorkspace } from '../../src/renderer/src/features/workspace/model'

const artifactsRoot = join(import.meta.dirname, '../../test-results/screenshots')

let application: ElectronApplication
let window: Page
let profilePath = ''
let browserErrors: string[]
let companion: Companion

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

const sampleCats = [
  ['assistant', '小黑', '你的日常搭档，陪你把想法变成现实。', '我的助手', 'bombay'],
  ['developer', '游戏程序', '代码、逻辑和实现，交给我一起想。', '游戏开发', 'orange-tabby'],
  [
    'producer',
    '游戏负责人 + 策划',
    '从一个好点子，到一份清晰的计划。',
    '游戏开发',
    'british-shorthair',
  ],
  ['artist', '游戏美术', '一起寻找属于这个世界的视觉语言。', '游戏开发', 'calico'],
  ['audio', '游戏音效', '让每一个场景，都有自己的声音。', '游戏开发', 'siamese'],
  ['coding', '编程搭档', '聊聊代码，也聊聊下一步怎么做。', '未分组', 'american-shorthair'],
  ['news', '资讯助手', '为你整理值得关注的新鲜事。', '未分组', 'ragdoll'],
  ['organizer', '整理助手', '少一点杂乱，多一点井井有条。', '未分组', 'persian'],
] as const

function sampleWorkspace(): AppWorkspace {
  return {
    version: 5,
    activeAgentId: 'assistant',
    leadAgentId: 'assistant',
    agents: sampleCats.map(([id, name, description, group, avatar]) => ({
      id,
      name,
      description,
      group,
      avatar,
      modelId: 'test-model',
      conversationId: `chat-${id}`,
      messages: [],
      draft: '',
      artifacts: [],
    })),
  }
}

// 原生窗口和预加载保持真实，业务响应固定在测试夹具内，以便重复验证布局及交互。
test.beforeEach(async () => {
  browserErrors = []
  profilePath = await mkdtemp(join(tmpdir(), 'tombot-desktop-test-'))
  companion = new Companion(sampleWorkspace())
  companion.setDeliveryEnabled(false)
  const env: Record<string, string> = {}
  for (const [key, value] of Object.entries(process.env)) {
    if (value !== undefined && key !== 'TOMBOT_SERVER_URL' && key !== 'ELECTRON_RUN_AS_NODE') {
      env[key] = value
    }
  }
  application = await electron.launch({
    args: ['.', '--force-device-scale-factor=1', `--user-data-dir=${profilePath}`],
    env,
  })
  window = await application.firstWindow()
  window.on('pageerror', (error) => browserErrors.push(error.message))
  await expect(window.getByLabel('服务地址', { exact: true })).toBeVisible()
  await companion.attach(application)
  await window.reload()
  await expect(window.getByRole('textbox', { name: '给小黑发消息' })).toBeVisible()
  // 等待原生窗口就绪，避免首次拖动被窗口显示过程打断。
  await expect
    .poll(() =>
      application.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0]!.isVisible()),
    )
    .toBe(true)
  await application.evaluate(({ BrowserWindow }) => BrowserWindow.getAllWindows()[0]!.focus())
  // Playwright 默认模拟浅色；取消覆盖后才能验证 Electron 的原生主题变化。
  await window.emulateMedia({ colorScheme: null })
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
  await mkdir(artifactsRoot, { recursive: true })
})

test.afterEach(async () => {
  try {
    await application?.close()
  } finally {
    try {
      await companion?.stop()
    } finally {
      if (profilePath) await rm(profilePath, { recursive: true, force: true })
      profilePath = ''
    }
  }
  expect(browserErrors).toEqual([])
})

/** 等服务确认保存后再重载，避免把还在传输的草稿看成丢失。 */
async function reloadPersisted(): Promise<void> {
  await expect
    .poll(() => window.evaluate(() => document.documentElement.dataset.workspaceSave ?? 'saved'))
    .toBe('saved')
  await window.reload()
  await expect(window.getByRole('textbox', { name: /发消息/ })).toBeVisible()
}

/** 在保存屏障后注入该用例所需的服务数据，再按正常重载路径读取。 */
async function updateServerWorkspace(mutate: (workspace: AppWorkspace) => void): Promise<void> {
  await expect
    .poll(() => window.evaluate(() => document.documentElement.dataset.workspaceSave ?? 'saved'))
    .toBe('saved')
  companion.updateWorkspace(mutate)
  await window.reload()
  await expect(window.getByRole('textbox', { name: /发消息/ })).toBeVisible()
}

test('点击自建猫咪只切换会话，不会和置顶位交换', async () => {
  await updateServerWorkspace((workspace) => {
    workspace.activeAgentId = 'cat-a'
    workspace.leadAgentId = 'cat-a'
    workspace.agents = [
      {
        id: 'cat-a',
        name: '阿花',
        description: '第一只',
        group: '未分组',
        avatar: 'calico',
        modelId: 'gpt-4o',
        messages: [],
        draft: '',
        artifacts: [],
      },
      {
        id: 'cat-b',
        name: '阿白',
        description: '第二只',
        group: '未分组',
        avatar: 'bombay',
        modelId: 'gpt-4o',
        messages: [],
        draft: '',
        artifacts: [],
      },
    ]
  })
  const pinned = window.locator('.pinned-area')
  const list = window.locator('.agent-groups')
  await expect(pinned.getByRole('button', { name: '阿花', exact: true })).toBeVisible()
  await list.getByRole('button', { name: '阿白', exact: true }).click()
  await expect(window.getByRole('textbox', { name: '给阿白发消息' })).toBeVisible()
  await expect(pinned.getByRole('button', { name: '阿花', exact: true })).toBeVisible()
  await expect(pinned.getByRole('button', { name: '阿白', exact: true })).toHaveCount(0)
  await expect(list.getByRole('button', { name: '阿白', exact: true })).toBeVisible()
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
  await window.screenshot({ path: join(artifactsRoot, 'desktop-home.png'), scale: 'css' })
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
  await expect(window.getByText('本地模式')).toHaveCount(0)
  await expect(window.getByRole('button', { name: '用户信息' })).toContainText(
    new URL(companion.url).host,
  )

  await window.getByRole('button', { name: '新建', exact: true }).click()
  await window.getByRole('menuitem', { name: '招聘猫咪', exact: true }).click()
  const createDialog = window.getByRole('dialog')
  const dialogBox = await createDialog.boundingBox()
  expect(dialogBox!.width).toBe(480)
  await expect(createDialog.getByText('预设头像', { exact: true })).toHaveCount(0)
  await expect(createDialog.getByRole('textbox', { name: /一句话介绍/ })).toHaveCount(0)
  await expect(createDialog.getByRole('combobox', { name: '模型', exact: true })).toHaveCount(0)
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
  await createDialog.getByRole('button', { name: '招聘猫咪', exact: true }).click()
  const input = window.getByRole('textbox', { name: '给写作搭档发消息' })
  await window.getByRole('button', { name: '查看写作搭档资料' }).click()
  const catPanel = window.getByRole('complementary', { name: '写作搭档' })
  await expect(catPanel.getByRole('combobox', { name: '模型', exact: true })).toHaveCount(0)
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
    await expect(window.getByRole('dialog', { name: '设置' })).toBeVisible()
    await window.keyboard.press('Escape')

    await window.getByRole('button', { name: '新建', exact: true }).click()
    await window.getByRole('menuitem', { name: '招聘猫咪', exact: true }).click()
    await expect(window.locator('select')).toHaveCount(0)
    const group = window.getByRole('combobox', { name: '分组', exact: true })
    await group.click()
    const groupMenu = window.getByRole('listbox', { name: '分组', exact: true })
    await expect(groupMenu).toBeVisible()
    expect(await groupMenu.evaluate((element) => element.matches(':popover-open'))).toBe(true)
    await window.screenshot({
      path: test.info().outputPath(`recruit-group-${theme}.png`),
      scale: 'css',
    })
    await group.press('End')
    await group.press('Enter')
    await expect(group).toHaveText('游戏开发')
    await expect(window.getByRole('dialog', { name: '招聘猫咪' })).toBeVisible()
    await group.click()
    await window.keyboard.press('Tab')
    await expect(groupMenu).toBeHidden()
    await expect(
      window.getByRole('dialog').getByRole('button', { name: '招聘猫咪', exact: true }),
    ).toBeFocused()
    await window.keyboard.press('Escape')
  }

  await setPageSize(860, 640)
  await expect.poll(() => window.evaluate(() => innerHeight)).toBe(640)
  await window.getByRole('button', { name: '新建', exact: true }).click()
  await window.getByRole('menuitem', { name: '招聘猫咪', exact: true }).click()
  await window.getByRole('combobox', { name: '分组', exact: true }).click()
  const menu = window.getByRole('listbox', { name: '分组', exact: true })
  const menuBounds = await menu.boundingBox()
  expect(menuBounds!.x).toBeGreaterThanOrEqual(12)
  expect(menuBounds!.y).toBeGreaterThanOrEqual(12)
  expect(menuBounds!.x + menuBounds!.width).toBeLessThanOrEqual(848)
  expect(menuBounds!.y + menuBounds!.height).toBeLessThanOrEqual(628)
  await window.getByRole('option', { name: '游戏开发', exact: true }).click()
  await expect(window.getByRole('combobox', { name: '分组', exact: true })).toHaveText('游戏开发')
  await window.screenshot({ path: test.info().outputPath('recruit-compact.png'), scale: 'css' })
})

test('招聘可加入既有分组，模型策略由服务端管理', async () => {
  await window.getByRole('button', { name: '新建', exact: true }).click()
  await window.getByRole('menuitem', { name: '招聘猫咪', exact: true }).click()
  const recruit = window.getByRole('dialog', { name: '招聘猫咪' })
  await expect(recruit.getByRole('combobox', { name: '模型', exact: true })).toHaveCount(0)
  const group = recruit.getByRole('combobox', { name: '分组', exact: true })
  await expect(group).toBeEnabled()
  await expect(group).toHaveText('未分组')
  await group.click()
  await expect(window.getByRole('option')).toHaveText(['未分组', '我的助手', '游戏开发'])
  await window.getByRole('option', { name: '游戏开发', exact: true }).click()
  await window.getByRole('textbox', { name: '助手名称' }).fill('我的猫咪')
  await window.getByRole('button', { name: '三花猫', exact: true }).click()
  await recruit.getByRole('button', { name: '招聘猫咪', exact: true }).click()
  await expect(window.getByRole('textbox', { name: '给我的猫咪发消息' })).toBeVisible()
  await window.getByRole('button', { name: '查看我的猫咪资料', exact: true }).click()
  const hired = window.getByRole('complementary', { name: '我的猫咪' })
  await expect(hired.getByRole('combobox', { name: '模型' })).toHaveCount(0)
  await expect
    .poll(() => companion.workspace().agents.find((agent) => agent.name === '我的猫咪')?.group)
    .toBe('游戏开发')
  await window.screenshot({ path: join(artifactsRoot, 'recruit-group.png'), scale: 'css' })
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
  const saveButton = window.getByRole('button', { name: '发送' })
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
  await expect(window.getByRole('navigation', { name: '设置分类' }).getByRole('button')).toHaveText(
    ['通用', '电脑', '更新'],
  )
  await window.screenshot({
    path: testInfo.outputPath('desktop-settings-general.png'),
    scale: 'css',
  })
  await chooseOption('主题', '深色')
  await chooseOption('语言', '简体中文')
  await window.getByRole('button', { name: '更新', exact: true }).click()
  await expect(settingsDialog.getByRole('heading', { name: '更新', exact: true })).toBeVisible()
  await expect(settingsDialog).toContainText(companion.url)
  await expect(window.getByLabel('API 密钥')).toHaveCount(0)
  await expect(window.getByRole('spinbutton', { name: /最大输出长度/ })).toHaveCount(0)
  await expect(window.getByRole('switch', { name: /流式响应/ })).toHaveCount(0)
  await window.locator('.settings-pane').evaluate((element) => {
    element.scrollTop = 0
  })
  await window.screenshot({
    path: testInfo.outputPath('desktop-settings-updates.png'),
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
  await window.getByRole('button', { name: '更新', exact: true }).click()
  expect(
    await settingsDialog.evaluate((element) => element.scrollWidth <= element.clientWidth),
  ).toBe(true)
  const bounds = await settingsDialog.boundingBox()
  expect(bounds!.y + bounds!.height).toBeLessThanOrEqual(640)
  await expect(window.getByRole('button', { name: '关闭设置' })).toBeVisible()
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
  await window.getByRole('button', { name: '用户信息' }).click()
  const workspace = window.getByRole('dialog', { name: '我的工作空间' })
  await expect(workspace).toContainText('8')
  await expect(workspace).toContainText('服务端连接')
  await expect(window.getByRole('combobox', { name: '主题' })).toHaveCount(0)
  await window.keyboard.press('Escape')
  await expect(window.getByRole('button', { name: '用户信息' })).toBeFocused()
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
  const language = window.getByRole('combobox', { name: '语言' })
  await language.focus()
  await expect(language).toBeFocused()
  expect(await language.evaluate((element) => getComputedStyle(element).outlineStyle)).toBe('none')
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
      { length: 60 },
      (_, index) => `第 ${index + 1} 行：验证消息在顶部渐变下自然淡出`,
    ).join('\n'),
  )
  await expectHoverScrollbar('.composer textarea')
  await input.press('Enter')
  await expect
    .poll(() =>
      window
        .locator('.messages-scroll')
        .evaluate((element) => element.scrollHeight > element.clientHeight),
    )
    .toBe(true)
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
  const button = window.getByRole('button', { name: '用户信息' })
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

  await companion.publish('run.started', 'missing-bot', {})
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
  companion.setRuns(
    agentIds.map((id) => ({
      id: `run-${id}`,
      bot_id: id,
      status: 'running',
      progress_summary: '',
    })),
  )
  for (const id of agentIds) await companion.publish('run.started', `run-${id}`, { bot_id: id })
  await expect(window.locator('.sidebar .avatar[data-pose="working"]')).toHaveCount(8)
  await expect(window.locator('.sidebar .avatar[data-ear="perk"]')).toHaveCount(8)
  await expect(window.locator('.sidebar .cat-work-mark')).toHaveCount(8)
  await expect(window.getByRole('button', { name: '用户信息' }).locator('.avatar')).toHaveCount(0)
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

  companion.setRuns([])
  for (const id of agentIds) {
    await companion.publish('run.finished', `run-${id}`, { bot_id: id, status: 'succeeded' })
  }
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
  await expect(listed).toHaveAttribute('data-ear', /^perk(?:-half)?$/)
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
  await companion.publish('run.started', 'another-run', { bot_id: 'assistant' })
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
  const artifacts = artifactsRoot
  await mkdir(artifacts, { recursive: true })
  const input = window.getByRole('textbox', { name: '给小黑发消息' })
  companion.setDeliveryEnabled(true)
  await input.fill('原来的想法')
  await input.press('Enter')
  await expect(window.getByRole('log')).toContainText('原来的想法')
  await expect.poll(() => companion.conversationMessages('chat-assistant')).toHaveLength(1)
  const [saved] = companion.conversationMessages('chat-assistant')
  await updateServerWorkspace((workspace) => {
    const agent = workspace.agents.find((item) => item.id === workspace.activeAgentId)
    if (!agent || !saved?.createdAt) throw new Error('服务端没有刚保存的消息')
    const sentAt = new Date(saved.createdAt).getTime()
    agent.messages = [
      {
        id: 'assistant-message',
        role: 'assistant',
        sequence: 1,
        content: '助手已经看到这句。',
        createdAt: new Date(sentAt - 30 * 60 * 1000).toISOString(),
        status: 'sent',
        requestId: 'req-e2e',
        reactions: [{ emoji: '👍', count: 2 }],
      },
      {
        ...saved,
        createdAt: new Date(sentAt - 20 * 60 * 1000).toISOString(),
        sequence: 2,
        status: 'sent',
      },
    ]
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
  companion.setDeliveryEnabled(false)
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
  await expect(window.getByRole('alert')).toContainText('当前模型策略不可用')
  await reloadPersisted()
  await expect(window.getByRole('button', { name: '❤️，点击取消' })).toBeVisible()
  await expect(quote).toBeVisible()
})

test('点击顶部猫咪打开右侧面板，动态整合电脑和任务卡片', async () => {
  const artifacts = artifactsRoot
  await mkdir(artifacts, { recursive: true })
  const trigger = window.getByRole('button', { name: '查看小黑资料', exact: true })
  const chevron = trigger.locator('.pill-chevron')
  await expect(chevron).toHaveClass(/lucide-chevron-right/)
  await expect(chevron).toHaveCSS('opacity', '0')
  await expect(chevron).toHaveCSS('width', '0px')
  await expect(chevron).toHaveCSS('margin-left', '0px')
  const idleWidth = (await trigger.boundingBox())!.width
  await trigger.screenshot({ path: `${artifacts}/chat-chip-idle.png` })
  await trigger.hover()
  await expect(chevron).toHaveCSS('opacity', '1')
  await expect(chevron).toHaveCSS('width', '14px')
  expect((await trigger.boundingBox())!.width).toBe(idleWidth + 22)
  await trigger.screenshot({ path: `${artifacts}/chat-chip-hover.png` })
  await window.getByRole('textbox', { name: '给小黑发消息' }).hover()
  await expect(chevron).toHaveCSS('opacity', '0')
  await expect(chevron).toHaveCSS('width', '0px')
  expect((await trigger.boundingBox())!.width).toBe(idleWidth)
  await trigger.evaluate((element: HTMLElement) => {
    element.focus({ focusVisible: true } as FocusOptions)
  })
  await expect(chevron).toHaveCSS('opacity', '1')
  await expect(chevron).toHaveCSS('width', '14px')
  expect((await trigger.boundingBox())!.width).toBe(idleWidth + 22)
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
  await expect(panel.getByRole('tab', { name: '动态', selected: true })).toBeVisible()
  await expect(panel.getByText('介绍', { exact: true })).toHaveCount(0)
  await expect(panel.getByRole('combobox', { name: '模型', exact: true })).toHaveCount(0)
  await window.screenshot({ path: `${artifacts}/cat-panel-details.png`, scale: 'css' })
  await panel.getByRole('tab', { name: '资料库', exact: true }).click()
  await expect(panel.getByRole('status')).toContainText('资料库还是空的')
  await window.screenshot({ path: `${artifacts}/cat-panel-library.png`, scale: 'css' })
  await panel.getByRole('tab', { name: '动态', exact: true }).click()
  await expect(
    panel.getByRole('region', { name: '电脑', exact: true }).getByRole('status'),
  ).toContainText('未连接')
  await window.screenshot({ path: `${artifacts}/cat-panel-computer.png`, scale: 'css' })
  await panel.getByRole('button', { name: '小黑的电脑', exact: true }).click()
  const viewer = window.getByRole('dialog', { name: '小黑的电脑', exact: true })
  await expect(viewer).toBeVisible()
  await viewer.getByRole('button', { name: '关闭观看' }).click()
  await expect(panel.getByRole('tab', { name: '记忆', exact: true })).toHaveCount(0)
  await expect(panel.getByRole('tab')).toHaveCount(2)
  await panel.getByRole('tab', { name: '动态', exact: true }).click()
  await expect(
    panel.getByRole('region', { name: '任务', exact: true }).getByRole('status'),
  ).toContainText('还没有任务')
  await expect(panel.getByRole('region', { name: '任务', exact: true })).toBeVisible()
  await expect(panel.getByRole('button', { name: '添加定时任务' })).toHaveCount(0)
  await window.screenshot({ path: `${artifacts}/cat-routines.png`, scale: 'css' })
  await trigger.click()
  await expect(panel).toHaveCount(0)
  await trigger.click()
  await expect(panel.getByRole('tab', { name: '动态', selected: true })).toBeVisible()
  await window.keyboard.press('Escape')
  await expect(panel).toHaveCount(0)
})

/** 将已持久化的助手消息交给会话夹具，由页面通过当前事件订阅读取。 */
async function appendAssistantMessage(
  conversationId: string,
  id: string,
  content: string,
  speaker?: string,
): Promise<void> {
  await companion.appendMessage(
    conversationId,
    {
      id,
      role: 'assistant',
      content,
      createdAt: new Date().toISOString(),
      status: 'sent',
      reactions: [],
    },
    speaker,
  )
}

function conversationFor(agentId: string): string {
  const conversationId = companion
    .workspace()
    .agents.find((agent) => agent.id === agentId)?.conversationId
  if (!conversationId) throw new Error(`测试猫咪缺少单聊：${agentId}`)
  return conversationId
}

test('资料库按时间顺序分页展示，图片可预览，电脑不接受外部地址', async () => {
  const conversationId = conversationFor('assistant')
  const now = Date.now()
  const files = Array.from({ length: 8 }, (_, index) => ({
    id: `library-file-${index}`,
    conversation_id: conversationId,
    name:
      index === 0 ? '今天的截图.png' : index === 7 ? '更早的草稿.txt' : `今天的文件 ${index}.txt`,
    media_type: index === 0 ? 'image/png' : 'text/plain',
    size: 100,
    sha256: 'a'.repeat(64),
    revision: 1,
    created_at: new Date(
      now - (index === 7 ? 10 * 86400_000 + 60_000 : index * 60_000),
    ).toISOString(),
  }))
  const imageFile = files[0]!
  const bytes = await readFile(
    join(import.meta.dirname, '../../src/renderer/src/assets/cats/frames/bombay.png'),
  )
  companion.setChatFiles(conversationId, files, 5)
  companion.setChatFilePreview(imageFile.id, {
    file: imageFile,
    image: `data:image/png;base64,${bytes.toString('base64')}`,
  })

  await window.getByRole('button', { name: '查看小黑资料', exact: true }).click()
  const panel = window.getByRole('complementary', { name: '小黑' })
  await panel.getByRole('tab', { name: '资料库', exact: true }).click()
  await expect(panel.getByRole('listitem')).toHaveCount(5)
  await expect(panel.getByRole('listitem').first()).toContainText('今天')
  await expect(panel.getByText('更早的草稿.txt')).toHaveCount(0)
  await panel.getByRole('button', { name: '加载更多' }).click()
  await expect(panel.getByRole('listitem')).toHaveCount(8)
  await expect(panel.getByRole('listitem').last()).toContainText('更早的草稿.txt')
  await expect(panel.getByRole('listitem').last()).toContainText('10 天')
  await window.screenshot({
    path: join(artifactsRoot, 'cat-panel-library-grouped.png'),
    scale: 'css',
  })

  await panel.getByRole('button', { name: '预览 今天的截图.png', exact: true }).click()
  const preview = window.getByRole('dialog', { name: '文件预览' })
  await expect(preview.getByRole('img', { name: imageFile.name, exact: true })).toBeVisible()
  await preview.getByRole('button', { name: '关闭弹窗' }).click()

  await panel.getByRole('tab', { name: '动态', exact: true }).click()
  await expect(
    panel.getByRole('region', { name: '电脑', exact: true }).getByRole('status'),
  ).toHaveText('未连接')
  await window.screenshot({ path: join(artifactsRoot, 'cat-panel-computer.png'), scale: 'css' })
  await panel.getByRole('button', { name: '小黑的电脑', exact: true }).click()
  const viewer = window.getByRole('dialog', { name: '小黑的电脑', exact: true })
  await expect(viewer).toBeVisible()
  await expect(viewer.getByLabel('远端电脑地址')).toHaveCount(0)
  await expect(viewer.locator('iframe')).toHaveCount(0)
  await viewer.getByRole('button', { name: '关闭观看' }).click()
})

test('服务端从聊天创建任务后，任务可禁用、启用和右键删除', async () => {
  const reminder = '请记住我每周三晨跑，并在当天早上9点提醒我'
  companion.setDelivery(async ({ botId, conversationId, content }) => {
    expect(content).toBe(reminder)
    expect(botId).toBe('assistant')
    companion.setTasks('assistant', [
      {
        id: 'running-reminder',
        bot_id: 'assistant',
        conversation_id: conversationId,
        name: '晨跑提醒',
        prompt: '提醒我去晨跑。',
        cron: '0 9 * * 3',
        timezone: 'Asia/Shanghai',
        run_at: null,
        is_enabled: true,
        revision: 1,
        next_run_at: new Date(Date.now() + 86400_000).toISOString(),
        last_run_at: null,
        last_run_id: null,
        last_delivery_status: '',
        last_status: '',
        last_error: '',
      },
    ])
    await appendAssistantMessage(conversationId, 'task-created', '已经记下，并设好周三早上的提醒。')
    await companion.publish('task.updated', 'running-reminder', { bot_id: botId })
  })

  await window.getByRole('textbox', { name: '给小黑发消息' }).fill(reminder)
  await window.getByRole('button', { name: '发送' }).click()
  await expect(window.getByRole('log')).toContainText('已经记下，并设好周三早上的提醒。')
  await expect(window.getByRole('alert')).toHaveCount(0)
  await window.getByRole('button', { name: '查看小黑资料', exact: true }).click()
  const panel = window.getByRole('complementary', { name: '小黑' })
  await expect(panel.getByRole('tab', { name: '记忆', exact: true })).toHaveCount(0)
  await expect(panel.getByText('介绍', { exact: true })).toHaveCount(0)
  await panel.getByRole('tab', { name: '动态', exact: true }).click()
  await expect(panel.getByText('晨跑提醒', { exact: true })).toBeVisible()
  await expect(panel.getByRole('button', { name: '添加定时任务' })).toHaveCount(0)
  await window.screenshot({ path: join(artifactsRoot, 'cat-routines.png'), scale: 'css' })
  await panel.getByRole('switch', { name: '禁用晨跑提醒' }).click()
  await expect(panel.getByRole('switch', { name: '启用晨跑提醒' })).toBeVisible()
  await panel.getByRole('switch', { name: '启用晨跑提醒' }).click()
  await expect(panel.getByRole('switch', { name: '禁用晨跑提醒' })).toBeVisible()
  await panel.getByText('晨跑提醒', { exact: true }).click({ button: 'right' })
  await expect(window.getByRole('menuitem', { name: '删除' })).toBeVisible()
  await window.screenshot({ path: join(artifactsRoot, 'cat-routine-menu.png'), scale: 'css' })
  await window.getByRole('menuitem', { name: '删除' }).click()
  await expect(
    panel.getByRole('region', { name: '任务', exact: true }).getByRole('status'),
  ).toContainText('还没有任务')
})

test('群资料与成员头像、折叠、成员搜索和提及交互', async () => {
  await window.getByRole('button', { name: '新建', exact: true }).click()
  await window.getByRole('menuitem', { name: '创建群聊', exact: true }).click()
  await window.getByRole('textbox', { name: '群名称', exact: true }).fill('创作小队')
  for (const name of ['小黑', '游戏程序', '游戏负责人 + 策划', '游戏美术'])
    await window.getByRole('checkbox', { name, exact: true }).check()
  await window.getByRole('button', { name: '创建群聊', exact: true }).click()
  const capsule = window.getByRole('button', { name: /查看(?:创作|灵感)小队群资料/ })
  await expect(capsule).toBeVisible()
  await expect(capsule.locator('.group-avatar img')).toHaveCount(4)
  await expect(capsule).toHaveCSS('padding-left', '14px')
  await expect(window.getByRole('button', { name: '成员', exact: true })).toHaveCount(0)
  const composer = await window.locator('.composer').boundingBox()
  expect(composer!.y + composer!.height).toBeGreaterThan(
    (await window.evaluate(() => innerHeight)) - 35,
  )
  await capsule.click()
  const panel = window.locator('.group-panel')
  const avatars = await panel
    .locator('.group-avatar img')
    .evaluateAll((images) => images.map((image) => (image as HTMLImageElement).src))
  expect(await panel.evaluate((element) => getComputedStyle(element).borderLeftColor)).toBe(
    await window
      .locator('.sidebar')
      .evaluate((element) => getComputedStyle(element).borderRightColor),
  )
  await expect(panel.getByRole('textbox', { name: '群名称' })).toHaveCount(0)
  await panel.getByRole('button', { name: '关闭群资料' }).click()
  const groupButton = window.getByRole('button', { name: '创作小队', exact: true })
  // 滚动会关闭菜单，等浏览器派发本次侧栏滚动事件后再打开。
  await groupButton.evaluate(async (element) => {
    const list = element.closest('nav')!
    const position = list.scrollTop
    element.scrollIntoView({ block: 'nearest', behavior: 'instant' })
    if (position !== list.scrollTop) {
      await new Promise<void>((resolve) => {
        list.addEventListener('scrollend', () => resolve(), { once: true })
      })
    }
    await new Promise<void>((resolve) => requestAnimationFrame(() => resolve()))
    element.focus({ preventScroll: true })
  })
  await window.keyboard.press('Shift+F10')
  const groupMenu = window.getByRole('menu', { name: '创作小队的菜单', exact: true })
  await expect(groupMenu).toBeVisible()
  await window.keyboard.press('Home')
  for (const name of ['移至', '标为未读', '重命名聊天']) {
    await window.keyboard.press('ArrowDown')
    await expect(groupMenu.getByRole('menuitem', { name, exact: true })).toBeFocused()
  }
  await window.keyboard.press('Enter')
  const groupName = window.getByRole('textbox', { name: '聊天名称', exact: true })
  await groupName.fill('灵感小队')
  await groupName.press('Enter')
  await expect(window.getByRole('button', { name: '查看灵感小队群资料' })).toBeVisible()
  await capsule.click()
  await expect(panel.getByRole('textbox', { name: '群名称' })).toHaveCount(0)
  await panel.getByRole('tab', { name: '资料库' }).click()
  await expect(panel.getByRole('status')).toContainText('资料库还是空的')
  await panel.getByRole('tab', { name: '成员' }).click()
  const remove = panel.getByRole('button', { name: '移出游戏程序' })
  await expect(remove).toHaveCSS('opacity', '0')
  await remove.locator('..').hover()
  await expect(remove).toHaveCSS('opacity', '1')
  await window.screenshot({ path: join(artifactsRoot, 'group-members-hover.png'), scale: 'css' })
  await remove.click()
  await expect(remove).toHaveCount(0)
  await panel.getByRole('button', { name: '添加成员' }).click()
  await panel.getByRole('textbox', { name: '搜索猫咪名称' }).fill('音效')
  await expect(panel.getByRole('option')).toHaveCount(1)
  await window.screenshot({ path: join(artifactsRoot, 'group-member-search.png'), scale: 'css' })
  await panel.getByRole('option', { name: '游戏音效' }).click()
  await expect(panel.getByRole('button', { name: '移出游戏音效' })).toBeAttached()
  const currentAvatars = await panel
    .locator('.group-avatar img')
    .evaluateAll((images) => images.map((image) => (image as HTMLImageElement).src))
  expect(currentAvatars).toHaveLength(4)
  expect(currentAvatars).toEqual(expect.arrayContaining([avatars[0], avatars[2], avatars[3]]))
  expect(currentAvatars).not.toContain(avatars[1])
  expect(currentAvatars.some((source) => source.includes('siamese'))).toBe(true)
  await setPageSize(860, 640)
  await window.screenshot({ path: join(artifactsRoot, 'group-panel-compact.png'), scale: 'css' })
  expect(await window.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true)
  await panel.getByRole('button', { name: '关闭群资料' }).click()
  const input = window.getByRole('textbox', { name: '给灵感小队发消息' })
  await input.fill('@小')
  await expect(window.getByRole('option', { name: '小黑', exact: true })).toBeVisible()
  await window.screenshot({ path: join(artifactsRoot, 'group-mention.png'), scale: 'css' })
  await input.press('Enter')
  await expect(input).toHaveValue('@小黑 ')
  await window.getByRole('button', { name: '收起未分组分组' }).click()
  await expect(window.getByRole('button', { name: '灵感小队', exact: true })).toBeHidden()
  await window.getByRole('button', { name: '收起游戏开发分组' }).click()
  await expect(window.getByRole('button', { name: '游戏程序', exact: true })).toBeHidden()
  await window.reload()
  await expect(window.getByRole('button', { name: '展开未分组分组' })).toBeVisible()
  await expect(window.getByRole('button', { name: '展开游戏开发分组' })).toBeVisible()
  await window.getByRole('button', { name: '展开未分组分组' }).click()
  await window.getByRole('button', { name: '灵感小队', exact: true }).click()
  const restoredAvatars = window
    .getByRole('button', { name: '查看灵感小队群资料' })
    .locator('.group-avatar img')
  await expect(restoredAvatars).toHaveCount(4)
  expect(
    await restoredAvatars.evaluateAll((images) =>
      images.map((image) => (image as HTMLImageElement).src),
    ),
  ).toEqual(currentAvatars)
})

test('群聊、单聊与附件保留会话各自的气泡和记录', async () => {
  let replies = 0
  companion.setDelivery(async ({ conversationId }) => {
    await appendAssistantMessage(
      conversationId,
      `meeting-reply-${++replies}`,
      '收到了',
      'assistant',
    )
  })

  await window.getByRole('button', { name: '新建', exact: true }).click()
  await window.screenshot({ path: join(artifactsRoot, 'create-menu.png'), scale: 'css' })
  await window.getByRole('menuitem', { name: '创建群聊', exact: true }).click()
  await window.getByRole('textbox', { name: '群名称', exact: true }).fill('晨会')
  await window.getByRole('checkbox', { name: '小黑' }).check()
  await window.getByRole('button', { name: '创建群聊', exact: true }).click()
  const groupInput = window.getByRole('textbox', { name: '给晨会发消息' })
  const groupLog = window.getByRole('log', { name: '晨会' })
  await groupInput.fill('大家看一下今天的安排')
  await window.getByRole('button', { name: '发送', exact: true }).click()
  await expect(groupLog.locator('.message-bubble')).toHaveText([
    '大家看一下今天的安排',
    '小黑收到了',
  ])
  await groupInput.fill('第二条安排')
  await window.getByRole('button', { name: '发送', exact: true }).click()
  await expect(groupLog.locator('.message-bubble')).toHaveCount(4)
  await window.getByRole('button', { name: '小黑', exact: true }).click()
  await window.getByRole('button', { name: '晨会', exact: true }).click()
  await expect(groupLog.locator('.message-bubble')).toHaveText([
    '大家看一下今天的安排',
    '小黑收到了',
    '第二条安排',
    '小黑收到了',
  ])
  await window.screenshot({ path: join(artifactsRoot, 'group-chat.png'), scale: 'css' })

  await window.getByRole('button', { name: '小黑', exact: true }).click()
  await window
    .locator('.composer input[type="file"]')
    .setInputFiles(
      join(import.meta.dirname, '../../src/renderer/src/assets/cats/frames/bombay.png'),
    )
  await expect(
    window.getByRole('list', { name: '待发送附件' }).getByRole('img', { name: 'bombay.png' }),
  ).toBeVisible()
  await window.getByRole('textbox', { name: '给小黑发消息' }).fill('看看这张图')
  await window.getByRole('button', { name: '发送', exact: true }).click()
  const directLog = window.getByRole('log')
  await expect(directLog.getByRole('img', { name: 'bombay.png', exact: true })).toBeVisible()
  await expect(directLog).toContainText('收到了')
  await expect(directLog).not.toContainText('第二条安排')
  await window.screenshot({ path: join(artifactsRoot, 'attachment-message.png'), scale: 'css' })
})

/** 在两个单聊中分别写入私信入口，两边查看的是同一个只读会话。 */
async function appendDmNotices(thread: string): Promise<void> {
  const senderConversation = conversationFor('assistant')
  const recipientConversation = conversationFor('developer')
  await companion.appendMessage(senderConversation, {
    id: 'dm-sent',
    role: 'assistant',
    content: '已发消息给游戏程序',
    createdAt: new Date().toISOString(),
    status: 'sent',
    reactions: [],
    dmNotice: {
      direction: 'sent',
      peerId: 'developer',
      peerName: '游戏程序',
      conversationId: thread,
    },
  })
  await companion.appendMessage(recipientConversation, {
    id: 'dm-received',
    role: 'assistant',
    content: '收到小黑的消息',
    createdAt: new Date().toISOString(),
    status: 'sent',
    reactions: [],
    dmNotice: {
      direction: 'received',
      peerId: 'assistant',
      peerName: '小黑',
      conversationId: thread,
    },
  })
  await companion.publish('message.created', 'dm-sent', {
    bot_id: 'assistant',
    conversation_id: senderConversation,
  })
  await companion.publish('message.created', 'dm-received', {
    bot_id: 'developer',
    conversation_id: recipientConversation,
  })
}

test('猫咪发出私信后双方可以打开只读的私信线程', async () => {
  const thread = 'meeting-dm'
  companion.setDelivery(async ({ botId }) => {
    expect(botId).toBe('assistant')
    await appendAssistantMessage(thread, 'dm-first', '晨会我先看了', 'assistant')
    await appendAssistantMessage(thread, 'dm-second', '好的', 'developer')
    await appendDmNotices(thread)
  })

  await window.getByRole('textbox', { name: '给小黑发消息' }).fill('请私下告诉游戏程序一声')
  await window.getByRole('button', { name: '发送' }).click()
  const notice = window.getByRole('button', { name: /已发消息给.*游戏程序/ })
  await expect(notice).toBeVisible()
  await window.screenshot({ path: join(artifactsRoot, 'cat-dm.png'), scale: 'css' })
  await notice.click()
  const dialog = window.getByRole('dialog', { name: '小黑 ↔ 游戏程序' })
  await expect(dialog).toBeVisible()
  await expect(dialog.locator('.dm-bubble')).toHaveText(['晨会我先看了', '好的'])
  await expect(dialog.locator('.dm-capsule')).toContainText('小黑')
  await expect(dialog.locator('.dm-capsule')).toContainText('游戏程序')
  await expect(dialog.locator('.user-bubble, .dialog-heading')).toHaveCount(0)
  await expect(dialog.getByRole('textbox')).toHaveCount(0)
  const viewportHeight = await window.evaluate(() => innerHeight)
  const dialogBox = (await dialog.boundingBox())!
  expect(dialogBox.height).toBeGreaterThan(viewportHeight * 0.7)
  const capsuleBox = (await dialog.locator('.dm-capsule').boundingBox())!
  const firstName = (await dialog.locator('.dm-name').first().boundingBox())!
  expect(firstName.y).toBeGreaterThan(capsuleBox.y + capsuleBox.height + 16)
  expect(await dialog.getByRole('log').evaluate((element) => element.scrollTop)).toBe(0)
  await window.screenshot({ path: join(artifactsRoot, 'cat-dm-open.png'), scale: 'css' })

  await dialog.getByRole('button', { name: '关闭弹窗' }).click()
  await window.getByRole('button', { name: /^游戏程序/ }).click()
  await window.getByRole('button', { name: /收到.*小黑.*消息/ }).click()
  const recipientDialog = window.getByRole('dialog', { name: '游戏程序 ↔ 小黑' })
  await expect(recipientDialog.locator('.dm-bubble')).toHaveText(['晨会我先看了', '好的'])
  await expect(recipientDialog.getByRole('textbox')).toHaveCount(0)
})

test('目标猫咪即时收到任务并在自己的聊天中直接交付', async () => {
  const thread = 'delivery-dm'
  const senderConversation = conversationFor('assistant')
  const recipientConversation = conversationFor('developer')
  const sender = { id: 'sender-run', bot_id: 'assistant', status: 'running', progress_summary: '' }
  const recipient = {
    id: 'recipient-run',
    bot_id: 'developer',
    status: 'running',
    progress_summary: '正在独立核验资料',
  }
  companion.setDelivery(async ({ botId }) => {
    expect(botId).toBe('assistant')
    await appendDmNotices(thread)
    companion.setRuns([recipient])
    await companion.publish('private.run.progress', recipient.id, {
      bot_id: recipient.bot_id,
      text: recipient.progress_summary,
    })
  })

  await window.getByRole('textbox', { name: '给小黑发消息' }).fill('让游戏程序整理结果直接发给我')
  await window.getByRole('button', { name: '发送' }).click()
  await expect(window.getByRole('button', { name: /已发消息给/ })).toBeVisible()
  const progress = window.locator('.composer-area .work-status')
  await expect(progress).toHaveCount(0)
  await window.getByRole('button', { name: /^游戏程序/ }).click()
  await expect(window.getByRole('button', { name: /收到.*小黑.*消息/ })).toBeVisible()
  await expect(progress).toHaveText('正在独立核验资料')
  await expect(progress.locator('.avatar')).toHaveAttribute('data-pose', 'working')
  await window.screenshot({ path: test.info().outputPath('recipient-working.png'), scale: 'css' })

  const answer = '这是游戏程序亲自整理的结果。'
  companion.setRuns([sender])
  await appendAssistantMessage(recipientConversation, 'recipient-result', answer, recipient.bot_id)
  await companion.publish('private.run.finished', recipient.id, {
    bot_id: recipient.bot_id,
    status: 'succeeded',
  })
  await expect(window.getByRole('log')).toContainText(answer)
  await expect(progress).toHaveCount(0)

  await window.getByRole('button', { name: '小黑', exact: true }).click()
  await expect(window.getByRole('log')).not.toContainText(answer)
  await expect(progress).toHaveText('正在思考…')
  companion.setRuns([])
  await appendAssistantMessage(senderConversation, 'sender-result', '好的', sender.bot_id)
  await companion.publish('run.finished', sender.id, { bot_id: sender.bot_id, status: 'succeeded' })
  await expect(window.getByRole('log')).toContainText('好的')
  await expect(progress).toHaveCount(0)
  await window.getByRole('button', { name: '游戏程序', exact: true }).click()
  await expect(window.getByRole('log').getByText(answer, { exact: true })).toHaveCount(1)
})

test('回复生成时只显示输入状态，完成后呈现完整气泡', async () => {
  const conversationId = conversationFor('assistant')
  const typing = {
    id: 'typing-run',
    bot_id: 'assistant',
    status: 'running',
    progress_summary: '正在输入…',
  }
  companion.setDelivery(async () => {
    companion.setRuns([typing])
    await companion.publish('run.progress', typing.id, {
      bot_id: typing.bot_id,
      text: typing.progress_summary,
    })
  })

  await window.getByRole('textbox', { name: '给小黑发消息' }).fill('打个招呼')
  await window.getByRole('button', { name: '发送' }).click()
  const progress = window.locator('.composer-area .work-status')
  const log = window.getByRole('log')
  await expect(progress).toHaveText('正在输入…')
  await expect(log.locator('.message-text')).toHaveText(['打个招呼'])
  await window.screenshot({ path: join(artifactsRoot, 'stream-in-progress.png'), scale: 'css' })

  companion.setRuns([])
  await appendAssistantMessage(conversationId, 'typing-complete', '你好', typing.bot_id)
  await companion.publish('run.finished', typing.id, { bot_id: typing.bot_id, status: 'succeeded' })
  await expect(log.locator('.message-text')).toHaveText(['打个招呼', '你好'])
  await expect(progress).toHaveCount(0)
  await expect(window.getByRole('alert')).toHaveCount(0)
})

test('空工作空间直接招聘第一只猫咪，重载后保持置顶且没有模型设置', async () => {
  companion.updateWorkspace((workspace) => {
    workspace.agents = []
    workspace.activeAgentId = ''
    workspace.leadAgentId = ''
  })
  await window.reload()
  await expect(window.getByRole('heading', { name: '招聘第一只猫咪' })).toBeVisible()
  await expect(window.getByRole('heading', { name: '配置默认模型' })).toHaveCount(0)
  await expect(window.getByLabel('API 密钥')).toHaveCount(0)
  await expect(window.locator('.session-gate')).toHaveCSS('-webkit-app-region', 'drag')
  await expect(window.locator('.session-traffic-lights')).toHaveCSS('-webkit-app-region', 'no-drag')
  await expect(window.locator('.session-page')).toHaveCSS('-webkit-app-region', 'no-drag')
  const controls = await window
    .locator('.session-page :is(input, button, textarea)')
    .evaluateAll((elements) =>
      elements.map((element) =>
        getComputedStyle(element).getPropertyValue('-webkit-app-region').trim(),
      ),
    )
  expect(controls.length).toBeGreaterThan(0)
  expect(controls.every((region) => region === 'no-drag')).toBe(true)
  await window.screenshot({ path: join(artifactsRoot, 'first-cat.png'), scale: 'css' })

  await window.getByRole('textbox', { name: '助手名称' }).fill('阿花')
  await window.getByRole('button', { name: '招聘猫咪' }).click()
  await expect(window.getByRole('textbox', { name: '给阿花发消息' })).toBeVisible()
  const pinned = window.locator('.pinned-area')
  await expect(pinned.getByRole('button', { name: '阿花', exact: true })).toBeVisible()
  await window.screenshot({ path: join(artifactsRoot, 'first-cat-home.png'), scale: 'css' })
  await reloadPersisted()
  await expect(pinned.getByRole('button', { name: '阿花', exact: true })).toBeVisible()

  await window.getByRole('button', { name: '新建', exact: true }).click()
  await window.getByRole('menuitem', { name: '招聘猫咪', exact: true }).click()
  await window.getByRole('textbox', { name: '助手名称' }).fill('阿白')
  await window.getByRole('button', { name: '招聘猫咪' }).click()
  await expect(window.getByRole('textbox', { name: '给阿白发消息' })).toBeVisible()
  await window.locator('.agent-groups').getByRole('button', { name: '阿白', exact: true }).click()
  await expect(pinned.getByRole('button', { name: '阿花', exact: true })).toBeVisible()
  await expect(pinned.getByRole('button', { name: '阿白', exact: true })).toHaveCount(0)
  await window.getByRole('button', { name: '打开设置' }).click()
  await expect(window.getByRole('button', { name: '模型设置', exact: true })).toHaveCount(0)
  await expect(window.getByLabel('API 密钥')).toHaveCount(0)
})

test('自定义猫咪上传头像、介绍并在重载和群成员资料中保留', async () => {
  await window.getByRole('button', { name: '新建', exact: true }).click()
  await window.getByRole('menuitem', { name: '招聘猫咪', exact: true }).click()
  await window.getByRole('button', { name: '自定义猫咪', exact: true }).click()
  await window.getByRole('textbox', { name: '助手名称', exact: true }).fill('花园伙伴')
  await window
    .getByRole('textbox', { name: '猫咪介绍', exact: true })
    .fill('擅长园艺，帮助我整理植物养护习惯。')
  await window.getByRole('button', { name: '招聘猫咪', exact: true }).click()
  await expect(window.getByRole('alert')).toHaveText('请先上传自定义猫咪头像。')
  await window
    .getByLabel('上传猫咪头像')
    .setInputFiles(
      join(import.meta.dirname, '../../src/renderer/src/assets/cats/frames/calico.png'),
    )
  const preview = window.locator('.custom-avatar-upload img')
  await expect(preview).toHaveAttribute('src', /^data:image\/png;base64,/)
  await window.screenshot({ path: test.info().outputPath('custom-cat-recruit.png'), scale: 'css' })
  await window.getByRole('button', { name: '招聘猫咪', exact: true }).click()
  await expect(window.getByRole('textbox', { name: '给花园伙伴发消息' })).toBeVisible()
  await expect(window.locator('.chat-agent-pill .avatar')).toHaveAttribute('data-avatar', 'custom')
  await window.reload()
  await expect(window.getByRole('textbox', { name: '给花园伙伴发消息' })).toBeVisible()
  const profile = await window.evaluate(async () => {
    const snapshot = await (globalThis as unknown as Window).desktop!.backend.loadWorkspace()
    if ('errorCode' in snapshot) throw new Error(snapshot.message)
    const cat = snapshot.workspace.agents.find((agent) => agent.name === '花园伙伴')!
    return { avatar: cat.customAvatar, description: cat.description }
  })
  expect(profile.description).toBe('擅长园艺，帮助我整理植物养护习惯。')
  expect(profile.avatar).toMatch(/^data:image\/png;base64,/)
  await window.getByRole('button', { name: '新建', exact: true }).click()
  await window.getByRole('menuitem', { name: '创建群聊', exact: true }).click()
  await window.getByRole('textbox', { name: '群名称', exact: true }).fill('园艺小组')
  await window.getByRole('checkbox', { name: '花园伙伴' }).check()
  await window.getByRole('button', { name: '创建群聊', exact: true }).click()
  await window.getByRole('button', { name: '查看园艺小组群资料' }).click()
  const member = window.locator('.group-member-row').filter({ hasText: '花园伙伴' })
  await expect(member.locator('.group-member-avatar img')).toHaveAttribute('src', profile.avatar!)
})

test('输入框粘贴系统文件与图片，普通文字保持正常输入', async () => {
  const path = join(profilePath, '粘贴资料.txt')
  await writeFile(path, '来自剪贴板的文件内容')
  await application.evaluate(async ({ clipboard, ClipboardItem }) => {
    const saved = []
    for (const item of await clipboard.read()) {
      const entries = await Promise.all(
        item.types.map(async (type) => [type, await item.getType(type)] as const),
      )
      saved.push(new ClipboardItem(Object.fromEntries(entries)))
    }
    Object.assign(globalThis, { testClipboard: saved })
  })
  try {
    await application.evaluate(async ({ clipboard, ClipboardItem }, url) => {
      const type =
        process.platform === 'darwin'
          ? 'electron application/osclipboard;format="public.file-url"'
          : 'text/uri-list'
      await clipboard.write([new ClipboardItem({ [type]: url })])
    }, pathToFileURL(path).href)
    const input = window.getByRole('textbox', { name: '给小黑发消息' })
    await input.focus()
    await input.press(process.platform === 'darwin' ? 'Meta+V' : 'Control+V')
    await expect(window.getByText('粘贴资料.txt', { exact: true })).toBeVisible()
    await expect(input).toHaveValue('')
    await application.evaluate(async ({ clipboard }) => clipboard.writeText('文字仍然正常粘贴'))
    await input.press(process.platform === 'darwin' ? 'Meta+V' : 'Control+V')
    await expect(input).toHaveValue('文字仍然正常粘贴')
    await input.evaluate((element) => {
      const transfer = new DataTransfer()
      transfer.items.add(new File(['附件正文'], '第二份.md', { type: 'text/markdown' }))
      element.dispatchEvent(
        new ClipboardEvent('paste', { clipboardData: transfer, bubbles: true, cancelable: true }),
      )
    })
    await expect(window.getByText('第二份.md', { exact: true })).toBeVisible()
    const picture = await readFile(
      join(import.meta.dirname, '../../src/renderer/src/assets/cats/frames/calico.png'),
    )
    await input.evaluate((element, encoded) => {
      const transfer = new DataTransfer()
      const bytes = Uint8Array.from(atob(encoded), (char) => char.charCodeAt(0))
      transfer.items.add(new File([bytes], '粘贴图片.png', { type: 'image/png' }))
      element.dispatchEvent(
        new ClipboardEvent('paste', { clipboardData: transfer, bubbles: true, cancelable: true }),
      )
    }, picture.toString('base64'))
    await expect(
      window.getByRole('list', { name: '待发送附件' }).getByRole('img', { name: '粘贴图片.png' }),
    ).toBeVisible()
    await window.screenshot({
      path: test.info().outputPath('pasted-attachments.png'),
      scale: 'css',
    })
  } finally {
    await application.evaluate(async ({ clipboard }) => {
      const holder = globalThis as unknown as { testClipboard: Electron.ClipboardItem[] }
      await clipboard.write(holder.testClipboard)
      Reflect.deleteProperty(holder, 'testClipboard')
    })
  }
})

test('聊天体验：群聊弹窗、搜索、工作摘要、Markdown 和连接状态', async () => {
  const request = '帮我整理计划'
  const work = {
    id: 'planning-run',
    bot_id: 'assistant',
    status: 'running',
    progress_summary: '正在整理创作方向与下一步计划',
  }
  companion.setDelivery(async ({ botId, content }) => {
    expect(botId).toBe(work.bot_id)
    expect(content).toBe(request)
    companion.setRuns([work])
    await companion.publish('run.progress', work.id, {
      bot_id: work.bot_id,
      text: work.progress_summary,
    })
  })

  await window.getByRole('button', { name: '打开设置' }).click()
  await chooseOption('主题', '深色')
  await window.keyboard.press('Escape')
  const serverStatus = window.locator('.server-status')
  await expect(window.getByRole('status', { name: '服务端已连接' })).toBeVisible()
  await expect(serverStatus).toHaveCSS('background-color', 'rgb(50, 166, 106)')

  await window.getByRole('button', { name: '搜索助手和消息' }).click()
  await expect(window.getByRole('group', { name: '设置', exact: true })).toBeVisible()
  await expect(window.getByRole('option', { name: /模型设置/ })).toHaveCount(0)
  await window.getByRole('group', { name: '设置', exact: true }).scrollIntoViewIfNeeded()
  await window.screenshot({ path: test.info().outputPath('search-presets-dark.png'), scale: 'css' })
  await window.getByRole('option', { name: /创建群聊/ }).click()
  const dialog = window.getByRole('dialog', { name: '新建群聊' })
  await expect(dialog).toHaveCSS('width', '480px')
  await window.getByLabel('群名称', { exact: true }).fill('创作小队')
  await window.getByRole('checkbox', { name: '小黑', exact: true }).check()
  await window.getByRole('checkbox', { name: '游戏程序', exact: true }).check()
  await window.screenshot({ path: test.info().outputPath('create-group-dark.png'), scale: 'css' })
  await setPageSize(860, 640)
  const bounds = (await dialog.boundingBox())!
  expect(bounds.y).toBeGreaterThanOrEqual(0)
  expect(bounds.y + bounds.height).toBeLessThanOrEqual(640)
  expect(await dialog.evaluate((element) => element.scrollWidth <= element.clientWidth)).toBe(true)
  await window.getByRole('button', { name: '创建群聊', exact: true }).click()
  await expect(window.getByRole('textbox', { name: '给创作小队发消息' })).toBeVisible()
  await setPageSize(1073, 780)

  await window.getByRole('button', { name: '小黑', exact: true }).click()
  const log = window.getByRole('log')
  await expect(log.locator('.user-row')).toHaveCount(0)
  await expect(log.locator('.message-bubble')).toHaveCount(0)

  await window.getByRole('textbox', { name: '给小黑发消息' }).fill(request)
  await window.getByRole('button', { name: '发送' }).click()
  const progress = window.locator('.composer-area .work-status')
  await expect(progress).toHaveText(work.progress_summary)
  await expect(log).not.toContainText('正在整理创作方向')
  await expect(progress.locator('.avatar')).toHaveAttribute('data-avatar', 'bombay')
  await expect(progress.locator('.avatar')).toHaveAttribute('data-pose', 'working')
  await expect(progress.locator('.cat-motion')).toHaveCSS('animation-name', 'cat-work')
  await window.screenshot({ path: test.info().outputPath('work-summary-dark.png'), scale: 'css' })

  companion.setRuns([])
  await appendAssistantMessage(
    conversationFor(work.bot_id),
    'plan',
    '## 创作计划\n\n| 阶段 | 结果 |\n| --- | --- |\n| 第一阶段 | 规划完成 |',
    work.bot_id,
  )
  await companion.publish('run.finished', work.id, { bot_id: work.bot_id, status: 'succeeded' })
  await expect(window.getByRole('heading', { name: '创作计划' })).toBeVisible()
  await expect(window.getByRole('table')).toContainText('规划完成')
  await expect(progress).toHaveCount(0)
  await window.screenshot({ path: test.info().outputPath('markdown-dark.png'), scale: 'css' })
  await reloadPersisted()
  await expect(log).toContainText(request)
  await expect(window.getByRole('heading', { name: '创作计划' })).toHaveCount(1)

  await window.getByRole('button', { name: '打开设置' }).click()
  await chooseOption('主题', '浅色')
  await window.keyboard.press('Escape')
  await window.screenshot({ path: test.info().outputPath('markdown-light.png'), scale: 'css' })
  await window.getByRole('button', { name: '新建', exact: true }).click()
  await window.getByRole('menuitem', { name: '创建群聊', exact: true }).click()
  await window.screenshot({ path: test.info().outputPath('create-group-light.png'), scale: 'css' })
  await window.keyboard.press('Escape')

  companion.setOffline(true)
  await window.evaluate(() => globalThis.dispatchEvent(new Event('focus')))
  await expect(window.getByRole('status', { name: '服务端连接异常' })).toBeVisible()
  await expect(serverStatus).toHaveCSS('background-color', 'rgb(226, 81, 81)')
  await expect(log).toContainText('规划完成')
  companion.setOffline(false)
  await window.evaluate(() => globalThis.dispatchEvent(new Event('online')))
  await expect(window.getByRole('status', { name: '服务端已连接' })).toBeVisible()
})
