import {
  _electron as electron,
  expect,
  test,
  type ElectronApplication,
  type Page,
} from '@playwright/test'
import { mkdtemp, readFile, rm } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { join } from 'node:path'

let application: ElectronApplication
let window: Page
let profilePath: string
let browserErrors: string[]

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
  profilePath = await mkdtemp(join(tmpdir(), 'tombot-desktop-test-'))
  application = await electron.launch({ args: ['.', `--user-data-dir=${profilePath}`] })
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
  await expect(window.getByRole('textbox', { name: '给小黑发消息' })).toBeVisible()
  await expect
    .poll(() =>
      window
        .locator('.avatar img')
        .evaluateAll((images) =>
          images.every(
            (image) =>
              (image as HTMLImageElement).complete && (image as HTMLImageElement).naturalWidth > 0,
          ),
        ),
    )
    .toBe(true)
})

test.afterEach(async () => {
  await application?.close()
  await rm(profilePath, { recursive: true, force: true })
  expect(browserErrors).toEqual([])
})

test('桌面布局、沙盒、招聘猫咪、草稿恢复与导出', async () => {
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
  const rendererProcesses = await application.evaluate(({ app }) =>
    app.getAppMetrics().filter((metric) => metric.type === 'Tab'),
  )
  expect(rendererProcesses.length).toBeGreaterThan(0)
  expect(rendererProcesses.every((metric) => metric.sandboxed)).toBe(true)
  expect(await window.evaluate(() => globalThis.window.desktop?.platform)).toBe(process.platform)
  expect(await window.evaluate(() => typeof (globalThis as Record<string, unknown>).require)).toBe(
    'undefined',
  )
  expect(await window.evaluate(() => typeof (globalThis as Record<string, unknown>).process)).toBe(
    'undefined',
  )

  await configureModel()
  await window.getByRole('button', { name: '招聘猫咪', exact: true }).click()
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
  await expect(window.getByRole('dialog')).toContainText('项目模型')
  await window.keyboard.press('Escape')
  await input.fill('把想法变成一篇好故事。')
  await input.press('Enter')
  await expect(window.getByRole('log')).toContainText('把想法变成一篇好故事。')
  await expect(window.getByRole('log')).toContainText('未发送')
  await input.fill('第二个尚未完成的想法')
  await window.reload()
  await expect(input).toHaveValue('第二个尚未完成的想法')
  await expect(window.locator('.chat-header .avatar')).toHaveAttribute('data-avatar', 'ragdoll')
  await expect(
    window.getByRole('button', { name: /^写作搭档/ }).locator('.avatar'),
  ).toHaveAttribute('data-avatar', 'ragdoll')
  await expect(window.getByRole('log')).toContainText('把想法变成一篇好故事。')

  await window.getByRole('button', { name: '查看写作搭档资料' }).click()
  await window.getByRole('button', { name: '导出草稿' }).click()
  const exportPath = join(profilePath, '导出的草稿.md')
  // Electron 使用原生下载事件；测试指定临时目标以避开系统保存弹窗，并检查实际落盘内容。
  await application.evaluate(({ session }, exportPath) => {
    session.defaultSession.once('will-download', (_event, item) => item.setSavePath(exportPath))
  }, exportPath)
  await window.getByRole('button', { name: '导出 Markdown' }).click()
  await expect
    .poll(async () => {
      try {
        return await readFile(exportPath, 'utf8')
      } catch (error) {
        if ((error as NodeJS.ErrnoException).code === 'ENOENT') return ''
        throw error
      }
    })
    .toContain('第二个尚未完成的想法')
  const exported = await readFile(exportPath, 'utf8')
  expect(exported).toContain('把想法变成一篇好故事。')
  expect(exported).toContain('未发送')
  await window.getByRole('button', { name: '关闭弹窗' }).click()
  await window.screenshot({ path: testInfo.outputPath('desktop-conversation.png'), scale: 'css' })
})

test('八款猫咪头像加载、搜索一致性及深浅主题选择器', async () => {
  await configureModel()
  const sidebarAvatars = window.locator('.sidebar .avatar')
  await expect(sidebarAvatars).toHaveCount(8)
  const presets = await sidebarAvatars.evaluateAll((elements) =>
    elements.map((element) => element.getAttribute('data-avatar')),
  )
  expect(new Set(presets).size).toBe(8)
  const imageSources = await sidebarAvatars
    .locator('img')
    .evaluateAll((images) => images.map((image) => (image as HTMLImageElement).src))
  expect(new Set(imageSources).size).toBe(8)
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
    await window.getByRole('button', { name: '招聘猫咪', exact: true }).click()
    const choices = window.getByRole('group', { name: '预设猫咪头像' })
    await expect(choices.getByRole('button')).toHaveCount(8)
    for (const choice of await choices.getByRole('button').all()) {
      await choice.click()
      await expect(choice).toHaveAttribute('aria-pressed', 'true')
      await expect(choices.locator('[aria-pressed="true"]')).toHaveCount(1)
      expect(
        await choice
          .locator('img')
          .evaluate((image: HTMLImageElement) => image.complete && image.naturalWidth > 0),
      ).toBe(true)
    }
    await window.screenshot({
      path: test.info().outputPath(`cat-presets-${theme}.png`),
      scale: 'css',
    })
    await window.keyboard.press('Escape')
  }
  await application.evaluate(({ BrowserWindow }) =>
    BrowserWindow.getAllWindows()[0]!.setSize(860, 640),
  )
  await expect.poll(() => window.evaluate(() => innerHeight)).toBe(640)
  await window.getByRole('button', { name: '招聘猫咪', exact: true }).click()
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

    await window.getByRole('button', { name: '招聘猫咪', exact: true }).click()
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

  await application.evaluate(({ BrowserWindow }) =>
    BrowserWindow.getAllWindows()[0]!.setSize(860, 640),
  )
  await expect.poll(() => window.evaluate(() => innerHeight)).toBe(640)
  await window.getByRole('button', { name: '招聘猫咪', exact: true }).click()
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
  await window.getByRole('button', { name: '招聘猫咪', exact: true }).click()
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

  await application.evaluate(({ BrowserWindow }) => {
    BrowserWindow.getAllWindows()[0]!.setSize(860, 640)
  })
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
  await expect(window.getByLabel('API 密钥')).toBeDisabled()
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

  await application.evaluate(({ BrowserWindow }) => {
    BrowserWindow.getAllWindows()[0]!.setSize(860, 640)
  })
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

/** 在真实窗口中拖动分隔线，验证指针捕获在自动收缩后仍能连续工作。 */
async function dragSidebarTo(width: number): Promise<void> {
  const separator = window.getByRole('separator', { name: '调整侧栏宽度' })
  const bounds = await separator.boundingBox()
  await window.mouse.move(bounds!.x + bounds!.width / 2, 350)
  await window.mouse.down()
  await window.mouse.move(width, 350, { steps: 12 })
  await window.mouse.up()
}

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
  await window.getByRole('button', { name: '招聘猫咪', exact: true }).click()
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
    trigger: '招聘猫咪',
    title: '招聘猫咪',
    inputRole: 'textbox' as const,
    inputName: '助手名称',
  },
]) {
  test(`${modal.title}弹窗遮罩覆盖完整窗口并关闭背景拖拽区域`, async () => {
    const trigger = window.getByRole('button', { name: modal.trigger, exact: true })
    const dialog = window.getByRole('dialog', { name: modal.title, exact: true })
    const backgroundRegions = window.locator('.sidebar-toolbar, .chat-header')
    for (const point of [
      { x: 1030, y: 400 },
      { x: 900, y: 20 },
      { x: 700, y: 760 },
      { x: 60, y: 400 },
    ]) {
      await trigger.click()
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
    await window.keyboard.press('Escape')
    await expect(dialog).toHaveCount(0)
    await expect(trigger).toBeFocused()
  })
}
