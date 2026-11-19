import { test, expect } from '@playwright/test'
import { readFile } from 'node:fs/promises'
import { resolve, extname } from 'node:path'
import type { AppWorkspace } from '../../src/renderer/src/features/workspace/model'
import type { DesktopBackend } from '../../src/renderer/src/features/desktop/backend'

// 使用确定的桥接夹具验证真实 Chromium 布局，服务契约由独立主进程和数据库测试覆盖。
test.beforeEach(async ({ page }) => {
  const workspace: AppWorkspace = {
    version: 5,
    activeAgentId: 'assistant',
    leadAgentId: 'assistant',
    agents: (
      [
        ['assistant', '小黑', 'bombay'],
        ['developer', '游戏程序', 'orange-tabby'],
      ] as const
    ).map(([id, name, avatar]) => ({
      id,
      name,
      avatar,
      description: '',
      group: '游戏开发',
      modelId: 'test-model',
      messages: [],
      draft: '',
      artifacts: [],
    })),
  }
  workspace.agents.forEach((agent) => {
    agent.conversationId = `chat-${agent.id}`
  })
  workspace.agents[0]!.messages = Array.from({ length: 35 }, (_, index) => ({
    id: `m-${index}`,
    role: index % 2 ? 'user' : 'assistant',
    content: `第 ${index + 1} 条消息。\n\n这里是一段用于滚动验证的正文，检查返回底部与布局稳定性。`,
    createdAt: '2026-11-17T01:00:00Z',
    status: 'sent',
    reactions: [],
  }))
  workspace.agents[0]!.messages.push({
    id: 'dm',
    role: 'assistant',
    content: '收到私信',
    createdAt: '2026-11-17T01:01:00Z',
    status: 'sent',
    reactions: [],
    dmNotice: {
      direction: 'received',
      peerId: 'developer',
      peerName: '游戏程序',
      conversationId: 'dm-chat',
    },
  })
  await page.addInitScript(
    ({ workspace }) => {
      const saved = localStorage.getItem('test-workspace')
      const current = saved ? JSON.parse(saved) : workspace
      const group = {
        id: 'group',
        name: '项目讨论',
        revision: 1,
        conversation_id: 'group-chat',
        avatar_presets: ['bombay', 'persian'],
        members: [
          { bot_id: 'assistant', name: '小黑' },
          { bot_id: 'developer', name: '游戏程序' },
        ],
      }
      const file = {
        id: 'file',
        conversation_id: 'group-chat',
        name: 'jedis-3.10.0.jar',
        media_type: 'application/java-archive',
        size: 8192,
        revision: 1,
        sha256: '',
        created_at: '2026-11-09T05:47:00Z',
      }
      const unavailable = async () => {
        throw new Error('布局测试不允许此业务操作')
      }
      const session = { phase: 'ready' as const, serverUrl: 'https://tom.example.test' }
      let consoleState = 'viewing' as 'viewing' | 'active' | 'closed'
      const consoleRecord = () => ({
        id: 'test-console',
        bot_id: 'assistant',
        state: consoleState,

        error_code: '',
        expires_at: new Date(Date.now() + 30000).toISOString(),
      })
      const frame = document.createElement('canvas')
      frame.width = 1280
      frame.height = 800
      const paint = frame.getContext('2d')!
      paint.fillStyle = '#e9edf2'
      paint.fillRect(0, 0, 1280, 800)
      paint.fillStyle = '#ffffff'
      paint.fillRect(20, 30, 1200, 740)
      paint.fillStyle = '#333333'
      paint.font = '24px sans-serif'
      paint.fillText('Bot 远端桌面（布局测试画面）', 60, 100)

      window.desktop = {
        platform: 'darwin',
        backend: {
          openConsole: async (botId) => {
            localStorage.setItem('console-bot', botId)
            localStorage.removeItem('console-closed')
            consoleState = 'viewing'
            return consoleRecord()
          },
          consoleStatus: async () => consoleRecord(),
          closeConsole: async () => {
            consoleState = 'closed'
            localStorage.setItem('console-closed', 'yes')
            return consoleRecord()
          },
          controlConsole: async (_id, takeover) => {
            consoleState = takeover ? 'active' : 'viewing'
            return consoleRecord()
          },
          connectConsole: (_id, _streamId, receive) => {
            const received: number[] = []
            let phase = 0,
              closed = false,
              painted = false
            const emit = (bytes: Uint8Array) =>
              queueMicrotask(() => {
                if (!closed) receive({ kind: 'bytes', bytes })
              })
            const image = paint.getImageData(0, 0, 1280, 800).data
            const show = () => {
              if (painted) return
              painted = true
              const pixels = new Uint8Array(16 + 1280 * 800 * 4)
              const view = new DataView(pixels.buffer)
              view.setUint16(2, 1)
              view.setUint16(8, 1280)
              view.setUint16(10, 800)
              for (let i = 0; i < image.length; i += 4) {
                pixels[16 + i] = image[i + 2]!
                pixels[17 + i] = image[i + 1]!
                pixels[18 + i] = image[i]!
              }
              emit(pixels)
            }
            queueMicrotask(() => {
              receive({ kind: 'open' })
              emit(new TextEncoder().encode('RFB 003.008\n'))
            })
            return {
              close: () => {
                closed = true
              },
              send: (bytes) => {
                received.push(...bytes)
                while (received.length) {
                  if (phase === 0) {
                    if (received.length < 12) return
                    received.splice(0, 12)
                    phase++
                    emit(new Uint8Array([1, 1]))
                    continue
                  }
                  if (phase === 1) {
                    received.shift()
                    phase++
                    emit(new Uint8Array(4))
                    continue
                  }
                  if (phase === 2) {
                    received.shift()
                    phase++
                    const header = new Uint8Array(28),
                      view = new DataView(header.buffer)
                    view.setUint16(0, 1280)
                    view.setUint16(2, 800)
                    header[4] = 32
                    header[5] = 24
                    header[7] = 1
                    view.setUint16(8, 255)
                    view.setUint16(10, 255)
                    view.setUint16(12, 255)
                    header[14] = 16
                    header[15] = 8
                    view.setUint32(20, 4)
                    header.set(new TextEncoder().encode('Test'), 24)
                    emit(header)
                    continue
                  }
                  const type = received[0]
                  let size = type === 0 ? 20 : type === 3 ? 10 : type === 4 ? 8 : type === 5 ? 6 : 0
                  if (type === 2) {
                    if (received.length < 4) return
                    size = 4 + 4 * ((received[2]! << 8) | received[3]!)
                  }
                  if (!size) throw new Error('布局测试收到未知 RFB 输入')
                  if (received.length < size) return
                  const packet = received.splice(0, size)
                  if (type === 3) show()
                  if (type === 4 || type === 5) {
                    const history = JSON.parse(localStorage.getItem('console-inputs') ?? '[]')
                    history.push(packet)
                    localStorage.setItem('console-inputs', JSON.stringify(history))
                  }
                }
              },
            }
          },
          connect: unavailable,
          browserLogin: unavailable,
          trustServerCertificate: unavailable,
          logout: unavailable,
          createAgent: unavailable,
          deliverMessage: unavailable,
          uploadChatFile: unavailable,
          downloadChatFile: unavailable,
          renameChatFile: unavailable,
          deleteChatFile: unavailable,
          setReaction: unavailable,
          trySessionSync: () => session,
          session: async () => session,
          loadUserProfile: async () => ({
            username: 'tester',
            displayName: '测试用户',
            avatar: '',
          }),
          watchWindowFocus: () => () => undefined,
          watchEvents: () => () => undefined,
          tryWorkspaceSync: () => ({ workspace: current, pristine: false }),
          loadWorkspace: async () => ({ workspace: current, pristine: false }),
          savesSynchronously: true,
          saveWorkspace: (next) => {
            localStorage.setItem('test-workspace', JSON.stringify(next))
          },
          listGroups: async () => [group],
          groupDetail: async () => group,
          loadConversationPreviews: async (ids) =>
            ids.map((id) => ({
              conversation_id: id,
              sequence: 1,
              content: id === 'group-chat' ? '群聊的最后一条消息' : '机器人最新消息',
            })),
          listConversationMessages: async (id) => ({
            hasMore: false,
            messages: [
              {
                id: 'reply',
                sequence: 1,
                role: 'assistant',
                speaker: 'assistant',
                content:
                  id === 'dm-chat'
                    ? '第一段正文。\n\n第二段正文。\n\n- 列表一\n- 列表二'
                    : '@游戏程序 一起检查这个问题。',
                createdAt: '2026-11-17T01:00:00Z',
                reactions: [],
                attachments: [],
                mentions: [{ id: 'developer', name: '游戏程序' }],
              },
            ],
          }),
          conversationActivity: async () => ({ runs: [] }),
          loadBotActivities: async () => ({ runs: [] }),
          listTasks: async () => [],
          listChatFiles: async (conversationId, _before, query = '') => ({
            files: file.name.includes(query) ? [{ ...file, conversation_id: conversationId }] : [],
            next: '',
            used_bytes: 8192,
            max_bytes: 268435456,
            max_file_bytes: 16777216,
          }),
          previewChatFile: async () => ({ file }),
          checkClientUpdate: async () => null,
          installClientUpdate: async () => true,
          downloadClientUpdate: async () => false,
          watchUpdateRequest: () => () => undefined,
          loadAbout: async () => ({
            clientVersion: '0.1.0',
            serverUrl: session.serverUrl,
            serverVersion: 'test-build',
          }),
        } as DesktopBackend,
      }
    },
    { workspace },
  )
  const root = resolve(import.meta.dirname, '../../out/renderer')
  await page.route('http://tombot.test/**', async (route) => {
    const path = new URL(route.request().url()).pathname
    const file = resolve(root, '.' + (path === '/' ? '/index.html' : path))
    if (!file.startsWith(root + '/')) throw new Error('测试资源路径无效')
    const contentType =
      (
        {
          '.html': 'text/html',
          '.js': 'application/javascript',
          '.css': 'text/css',
          '.png': 'image/png',
        } as Record<string, string>
      )[extname(file)] ?? 'application/octet-stream'
    await route.fulfill({ body: await readFile(file), contentType })
  })
  await page.setViewportSize({ width: 1200, height: 820 })
  await page.goto('http://tombot.test/')
  await expect(page.getByRole('textbox', { name: '给小黑发消息' })).toBeVisible()
})

test('返回底部一次点击生效且按钮居中，输入区域透明，私聊 Markdown 没有多余空行', async ({
  page,
}) => {
  const log = page.locator('.messages-scroll')
  for (let attempt = 0; attempt < 3; attempt++) {
    await log.hover()
    await page.mouse.wheel(0, -10000)
    const button = page.getByRole('button', { name: '回到最新消息' })
    await expect(button).toBeVisible()
    const box = (await button.boundingBox())!
    const composer = (await page.locator('.composer').boundingBox())!
    expect(Math.abs(box.x + box.width / 2 - composer.x - composer.width / 2)).toBeLessThan(1)
    expect(box.y + box.height).toBeLessThan(composer.y)
    await page.mouse.move(box.x + box.width / 2, box.y + box.height / 2)
    await page.mouse.down()
    await expect(button).toHaveCSS(
      'transform',
      `matrix(0.97, 0, 0, 0.97, ${-box.width / 2}, ${-box.height})`,
    )
    const pressed = (await button.boundingBox())!
    expect(Math.abs(pressed.y + pressed.height / 2 - box.y - box.height / 2)).toBeLessThan(1)
    await page.mouse.up()
    await expect
      .poll(() =>
        log.evaluate((element) => element.scrollHeight - element.scrollTop - element.clientHeight),
      )
      .toBeLessThan(2)
  }
  await expect(page.locator('.composer-area')).toHaveCSS('background-color', 'rgba(0, 0, 0, 0)')
  await expect(page.locator('.composer-area')).toHaveCSS('padding-top', '0px')
  await page.locator('.dm-notice').click()
  const markdown = page.locator('.dm-bubble .message-markdown')
  await expect(markdown).toHaveCSS('white-space', 'normal')
  const paragraphs = await markdown.locator('p').evaluateAll((nodes) =>
    nodes.map((node) => {
      const b = node.getBoundingClientRect()
      return { top: b.top, bottom: b.bottom }
    }),
  )
  expect(paragraphs[1]!.top - paragraphs[0]!.bottom).toBeLessThan(18)
  await page.screenshot({ path: 'test-results/screenshots/dm-spacing.png' })
})

test('群聊提及、头像、资料卡和更新页符合布局要求', async ({ page }) => {
  await expect(page.getByRole('button', { name: '项目讨论', exact: true })).toContainText(
    '群聊的最后一条消息',
  )
  await page.getByRole('button', { name: '项目讨论', exact: true }).click()
  await expect(page.locator('.message-speaker .avatar')).toBeVisible()
  await expect(page.locator('.message-speaker')).toContainText('小黑')
  await expect(page.locator('.group-mentions')).toHaveCount(0)
  const input = page.getByRole('textbox', { name: '给项目讨论发消息' })
  await input.fill('@小')
  await input.press('Enter')
  await input.pressSequentially('请检查')
  await expect(input).toHaveValue('@小黑 请检查')
  await expect(page.getByRole('listbox', { name: '提及成员' })).toHaveCount(0)
  await page.getByRole('button', { name: '查看项目讨论群资料' }).click()
  await page.getByRole('tab', { name: '资料库' }).click()
  const card = page.locator('.library-file-card')
  const more = page.getByRole('button', { name: '更多操作 jedis-3.10.0.jar' })
  await page.mouse.move(600, 100)
  await expect(more).toHaveCSS('opacity', '0')
  await card.hover()
  await expect(more).toHaveCSS('opacity', '1')
  await expect(page.getByRole('tooltip')).toContainText('jedis-3.10.0.jar')
  await page.screenshot({ path: 'test-results/screenshots/library-hover.png' })
  await more.click()
  await expect(page.getByRole('menuitem')).toHaveCount(4)
  await page.keyboard.press('Escape')
  await page.getByRole('button', { name: '打开设置' }).click()
  await page.getByRole('button', { name: '更新', exact: true }).click()
  await expect(page.getByRole('region', { name: '更新' })).toContainText('test-build')
  await expect(page.getByRole('button', { name: '更新', exact: true })).toHaveCSS(
    'background-color',
    'rgb(231, 231, 231)',
  )
  await page.screenshot({ path: 'test-results/screenshots/updates.png' })
})

test('原生拖拽群聊到机器人分组后刷新保持分类', async ({ page }) => {
  const group = page.getByRole('button', { name: '项目讨论', exact: true })
  await group.dragTo(page.getByRole('button', { name: '收起游戏开发分组' }))
  const section = page
    .locator('.agent-group')
    .filter({ has: page.getByRole('heading', { name: '游戏开发', exact: true }) })
  await expect(section.getByRole('button', { name: '项目讨论', exact: true })).toBeVisible()
  await page.reload()
  await expect(section.getByRole('button', { name: '项目讨论', exact: true })).toBeVisible()
})

for (const kind of ['bot', 'group'] as const) {
  test(`${kind === 'bot' ? '猫咪' : '群聊'}资料库搜索栏紧凑，移除说明与容量并保留搜索`, async ({
    page,
  }) => {
    if (kind === 'group') await page.getByRole('button', { name: '项目讨论', exact: true }).click()
    await page
      .getByRole('button', { name: kind === 'bot' ? '查看小黑资料' : '查看项目讨论群资料' })
      .click()
    await page.getByRole('tab', { name: '资料库' }).click()
    const library = page.getByRole('region', { name: '聊天资料库' })
    await expect(library.getByText('jedis-3.10.0.jar', { exact: true })).toBeVisible()
    await expect(library.getByText('聊天中你和猫咪发出的文件会自动收录。')).toHaveCount(0)
    await expect(library.getByText(/^已用 /)).toHaveCount(0)
    const form = library.getByRole('search')
    const input = form.getByRole('textbox', { name: '搜索聊天文件' })
    const submit = form.getByRole('button', { name: '搜索文件' })
    await expect(form).toHaveCSS('height', '32px')
    await expect(form).toHaveCSS('background-color', 'rgb(243, 243, 243)')
    await expect(input).toHaveCSS('font-size', '12px')
    await expect(input).toHaveCSS('padding', '0px')
    const searchBox = (await form.boundingBox())!
    const buttonBox = (await submit.boundingBox())!
    expect(buttonBox.x + buttonBox.width).toBeLessThanOrEqual(searchBox.x + searchBox.width)
    expect(buttonBox.y).toBeGreaterThanOrEqual(searchBox.y)
    expect(buttonBox.y + buttonBox.height).toBeLessThanOrEqual(searchBox.y + searchBox.height)
    await page.screenshot({ path: `test-results/screenshots/library-${kind}-compact.png` })
    await input.focus()
    await expect(form).toHaveCSS('box-shadow', 'none')
    await expect(form).toHaveCSS('border-top-width', '1px')
    await expect(form).toHaveCSS('border-top-color', 'rgb(222, 222, 222)')
    await expect(input).toHaveCSS('outline-style', 'none')
    expect(await form.boundingBox()).toEqual(searchBox)
    await page.screenshot({ path: `test-results/screenshots/library-${kind}-focused.png` })
    await input.fill('没有这个文件')
    await input.press('Enter')
    await expect(library.getByText('没有找到匹配的文件。')).toBeVisible()
    await input.fill('')
    await submit.click()
    await expect(library.getByText('jedis-3.10.0.jar', { exact: true })).toBeVisible()
    await page.evaluate(() => {
      document.documentElement.dataset.theme = 'dark'
    })
    await input.focus()
    await expect(form).toHaveCSS('background-color', 'rgb(32, 32, 32)')
    await expect(form).toHaveCSS('border-top-color', 'rgb(58, 58, 58)')
    await expect(form).toHaveCSS('box-shadow', 'none')
    await expect(form).toHaveCSS('border-top-width', '1px')
    await expect(input).toHaveCSS('color', 'rgb(231, 231, 231)')
    await expect(submit).toHaveCSS('background-color', 'rgb(43, 43, 43)')
    await page.screenshot({
      path: `test-results/screenshots/library-${kind}-compact-dark.png`,
      animations: 'disabled',
    })
  })
}

test('设置错误位于标题与内容之间，重试按钮保持在提示右侧并能恢复', async ({ page }) => {
  await page.evaluate(() => {
    let attempts = 0
    window.desktop!.backend.loadAbout = async () => ({
      clientVersion: '0.1.0',
      serverUrl: 'https://tom.example.test',
      serverVersion: ++attempts === 1 ? '' : 'test-build',
      serverError: attempts === 1 ? '服务端版本读取失败，请重试。' : undefined,
    })
  })
  await page.getByRole('button', { name: '打开设置' }).click()
  await page.getByRole('button', { name: '更新', exact: true }).click()
  const pane = page.getByRole('region', { name: '更新' })
  const alert = pane.getByRole('alert')
  await expect(alert).toContainText('服务端版本读取失败')
  const heading = (await pane.getByRole('heading', { name: '更新' }).boundingBox())!
  const notice = (await alert.boundingBox())!
  const content = (await pane.locator('.settings-versions').boundingBox())!
  expect(notice.y).toBeGreaterThanOrEqual(heading.y + heading.height)
  expect(notice.y + notice.height).toBeLessThan(content.y)
  const retry = alert.getByRole('button', { name: '重试读取版本' })
  const button = (await retry.boundingBox())!
  const message = (await alert.locator('span').boundingBox())!
  expect(button.x).toBeGreaterThan(message.x + message.width)
  expect(Math.abs(button.y + button.height / 2 - message.y - message.height / 2)).toBeLessThan(1)
  await page.screenshot({ path: 'test-results/screenshots/updates-error.png' })
  await retry.click()
  await expect(pane).toContainText('test-build')
  await expect(alert).toHaveCount(0)
})

test('按住猫咪头像开始原生拖拽，预览清除压缩和倾斜并保持正方形', async ({ page }) => {
  await page.evaluate(() => {
    const original = DataTransfer.prototype.setDragImage
    DataTransfer.prototype.setDragImage = function (element, x, y) {
      const avatar = element.querySelector<HTMLElement>('.avatar')!
      const portrait = element.querySelector<HTMLElement>('.cat-portrait')!
      const box = portrait.getBoundingClientRect()
      document.documentElement.dataset.dragPreview = JSON.stringify({
        pressed: avatar.dataset.pressed,
        width: box.width,
        height: box.height,
        transforms: ['.cat-tilt', '.cat-motion'].map(
          (selector) => getComputedStyle(element.querySelector(selector)!).transform,
        ),
        animation: getComputedStyle(element.querySelector('.cat-motion')!).animationName,
      })
      original.call(this, element, x, y)
    }
  })
  const avatar = page.getByRole('button', { name: '游戏程序', exact: true }).locator('.avatar')
  await avatar.hover()
  const box = (await avatar.boundingBox())!
  await page.mouse.down()
  await expect(avatar).toHaveAttribute('data-pressed', 'true')
  await page.mouse.move(box.x + box.width / 2, box.y + box.height + 20, { steps: 5 })
  await expect.poll(() => page.locator('html').getAttribute('data-drag-preview')).toBeTruthy()
  const preview = JSON.parse((await page.locator('html').getAttribute('data-drag-preview'))!)
  expect(preview.pressed).toBe('true')
  expect(preview.transforms).toEqual(['none', 'none'])
  expect(preview.animation).toBe('none')
  expect(preview.width).toBe(56)
  expect(preview.height).toBe(56)
  await page.keyboard.press('Escape')
  await page.mouse.up()
  await expect(page.locator('.sidebar-drag-preview')).toHaveCount(0)
})

test('noVNC 观看只读，接管坐标缩放正确，支持中文、滚轮和全屏', async ({ page }) => {
  await page.getByRole('button', { name: '查看小黑资料', exact: true }).click()
  const panel = page.getByRole('complementary', { name: '小黑' })
  const card = panel.getByRole('button', { name: '小黑的电脑', exact: true })
  await card.click()
  const viewer = page.getByRole('dialog', { name: '小黑的电脑', exact: true })
  const display = viewer.locator('.computer-screen canvas')
  await expect(display).toBeVisible()
  await expect(viewer.getByText('正在连接电脑画面…')).toHaveCount(0)
  await expect(viewer.getByText('仅观看')).toBeVisible()
  await expect(viewer).toHaveCSS('background-color', 'rgb(21, 21, 21)')
  expect(await page.evaluate(() => localStorage.getItem('console-bot'))).toBe('assistant')
  const bounds = (await display.boundingBox())!
  await page.mouse.click(bounds.x + bounds.width / 2, bounds.y + bounds.height / 2)
  expect(await page.evaluate(() => localStorage.getItem('console-inputs'))).toBeNull()
  await viewer.getByRole('button', { name: '取得控制权', exact: true }).click()
  await display.scrollIntoViewIfNeeded()
  const controlledBounds = (await display.boundingBox())!
  const clickX = Math.round(controlledBounds.x + controlledBounds.width / 2)
  const clickY = Math.round(controlledBounds.y + controlledBounds.height / 2)
  const expected = {
    x: Math.floor(((clickX - controlledBounds.x) * 1280) / controlledBounds.width),
    y: Math.floor(((clickY - controlledBounds.y) * 800) / controlledBounds.height),
  }
  await page.mouse.click(clickX, clickY)
  await expect
    .poll(() =>
      page.evaluate(
        (expected) =>
          JSON.parse(localStorage.getItem('console-inputs') ?? '[]').some(
            (p: number[]) =>
              p[0] === 5 &&
              p[1] === 1 &&
              Math.abs(((p[2]! << 8) | p[3]!) - expected.x) <= 1 &&
              Math.abs(((p[4]! << 8) | p[5]!) - expected.y) <= 1,
          ),
        expected,
      ),
    )
    .toBe(true)
  const body = panel.locator('.cat-panel-body')
  const before = await body.evaluate((element) => element.scrollTop)
  await page.mouse.wheel(0, 90)
  await expect.poll(() => body.evaluate((element) => element.scrollTop)).toBe(before)
  const remoteText = viewer.getByRole('textbox', { name: '输入到电脑' })
  await expect(remoteText).toHaveCSS('background-color', 'rgb(39, 39, 39)')
  await remoteText.fill('你好')
  await viewer.getByRole('button', { name: '输入', exact: true }).click()
  await expect
    .poll(() =>
      page.evaluate(() =>
        JSON.parse(localStorage.getItem('console-inputs') ?? '[]').some(
          (p: number[]) =>
            p[0] === 4 &&
            p[1] === 1 &&
            ((p[4]! << 24) | (p[5]! << 16) | (p[6]! << 8) | p[7]!) === 0x01004f60,
        ),
      ),
    )
    .toBe(true)
  await viewer.getByRole('button', { name: '全屏查看' }).click()
  await expect(viewer.getByRole('button', { name: '退出全屏' })).toBeVisible()
  const finish = (await viewer.getByRole('button', { name: '交还控制权' }).boundingBox())!
  expect(finish.y + finish.height).toBeLessThan(await page.evaluate(() => window.innerHeight))
  await page.screenshot({ path: 'test-results/console-fullscreen.png' })
  await viewer.getByRole('button', { name: '退出全屏' }).click()
  await viewer.getByRole('button', { name: '交还控制权' }).click()
  await expect(display).toBeVisible()
  await expect(viewer.getByRole('textbox', { name: '输入到电脑' })).toHaveCount(0)
  await viewer.getByRole('button', { name: '关闭观看' }).click()
  await expect(viewer).toHaveCount(0)
  await expect(card).toBeFocused()
  await card.click()
  await expect(display).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(viewer).toHaveCount(0)
  await expect(panel).toBeVisible()
  await expect.poll(() => page.evaluate(() => localStorage.getItem('console-closed'))).toBe('yes')
})

test('粘贴附件在输入框内排列，纯图片独立展示，音视频能在消息内实际解码', async ({ page }) => {
  const media = await Promise.all(
    ['voice.wav', 'clip.webm'].map(async (name) =>
      (await readFile(resolve(import.meta.dirname, 'fixtures', name))).toString('base64'),
    ),
  )
  await page.evaluate(([sound, clip]) => {
    const backend = window.desktop!.backend!
    const canvas = document.createElement('canvas')
    canvas.width = 320
    canvas.height = 256
    const context = canvas.getContext('2d')!
    context.fillStyle = '#24897b'
    context.fillRect(0, 0, 320, 256)
    const picture = canvas.toDataURL('image/png').split(',')[1]!
    const bytes = (base64: string) => Uint8Array.from(atob(base64), (value) => value.charCodeAt(0))
    const batches = [
      [
        { name: '合照.png', type: 'image/png', bytes: bytes(picture) },
        { name: '会议纪要.docx', type: 'application/octet-stream', bytes: new Uint8Array(3456) },
        { name: '程序.jar', type: 'application/octet-stream', bytes: new Uint8Array(128) },
      ],
      [
        { name: '语音.wav', type: 'audio/wav', bytes: bytes(sound!) },
        { name: '短片.webm', type: 'video/webm', bytes: bytes(clip!) },
      ],
    ]
    backend.readClipboardFiles = async () => batches.shift() ?? []
    backend.uploadAttachment = async (file) => {
      const path = 'chat-files/01J00000000000000000000001/' + file.requestId
      const base64 = btoa(Array.from(file.bytes, (byte) => String.fromCharCode(byte)).join(''))
      sessionStorage.setItem(path, 'data:' + file.type + ';base64,' + base64)
      return { path, sha256: 'a'.repeat(64) }
    }
    backend.previewAttachment = async (path) => {
      const source = sessionStorage.getItem(path)!
      if (source.startsWith('data:image/')) {
        const count = Number(sessionStorage.getItem('photo-preview-count')) + 1
        sessionStorage.setItem('photo-preview-count', String(count))
      }
      return source
    }
    backend.deliverMessage = async (_id, _text, requestId) => ({ acceptedMessageId: requestId })
  }, media)
  const input = page.getByRole('textbox', { name: '给小黑发消息' })
  const paste = () =>
    input.evaluate((node) =>
      node.dispatchEvent(
        new ClipboardEvent('paste', {
          clipboardData: new DataTransfer(),
          bubbles: true,
          cancelable: true,
        }),
      ),
    )
  await paste()
  const tray = page.locator('form.composer').getByRole('list', { name: '待发送附件' })
  await expect(tray.getByRole('img', { name: '合照.png' })).toBeVisible()
  await expect(tray.getByText('会议纪要.docx')).toBeVisible()
  await expect(tray.getByText('3.4 KB')).toBeVisible()
  expect(
    await page.evaluate(() =>
      Object.keys(sessionStorage).filter((key) => key.startsWith('chat-files/')),
    ),
  ).toHaveLength(0)
  const last = (await tray.getByText('程序.jar').boundingBox())!
  expect(last.y).toBeLessThan((await input.boundingBox())!.y)
  for (const name of ['会议纪要.docx', '程序.jar']) {
    await tray.getByText(name).hover()
    await tray.getByRole('button', { name: '移除' + name }).click()
  }
  await page.getByRole('button', { name: '发送', exact: true }).click()
  await expect
    .poll(() =>
      page.evaluate(
        () => Object.keys(sessionStorage).filter((key) => key.startsWith('chat-files/')).length,
      ),
    )
    .toBe(1)
  const photo = page.getByRole('button', { name: '查看原图 合照.png' })
  await expect(photo).toBeEnabled()
  await photo.getByRole('img').evaluate((image: HTMLImageElement) => image.decode())
  const photoBubble = page.locator('.user-row .attachment-only').filter({ has: photo })
  const textBubble = page.locator('.user-bubble').first()
  await expect(photoBubble).toHaveCSS('background-color', 'rgba(0, 0, 0, 0)')
  const photoBounds = (await photo.boundingBox())!
  const bubbleBounds = (await photoBubble.boundingBox())!
  const textBounds = (await textBubble.boundingBox())!
  expect(photoBounds.width).toBeGreaterThan(200)
  expect(photoBounds.width).toBeLessThanOrEqual(250)
  expect(bubbleBounds.width).toBeCloseTo(photoBounds.width, 0)
  expect(photoBounds.x + photoBounds.width).toBeCloseTo(textBounds.x + textBounds.width, 0)
  await photo.click()
  const dialog = page.getByRole('dialog', { name: '合照.png' })
  await expect(dialog.getByRole('img')).toBeVisible()
  await page.keyboard.press('Escape')
  await expect(dialog).toHaveCount(0)

  await page.getByRole('button', { name: '游戏程序', exact: true }).click()
  await expect(photo).toHaveCount(0)
  await page.getByRole('button', { name: '小黑', exact: true }).click()
  await photo.getByRole('img').evaluate((image: HTMLImageElement) => image.decode())
  expect(await page.evaluate(() => sessionStorage.getItem('photo-preview-count'))).toBe('1')

  await page.setViewportSize({ width: 860, height: 820 })
  await page.getByRole('button', { name: '查看小黑资料', exact: true }).click()
  const narrowPhotoBounds = (await photo.boundingBox())!
  const narrowTextBounds = (await textBubble.boundingBox())!
  expect(narrowPhotoBounds.width).toBeLessThan(photoBounds.width)
  expect(narrowPhotoBounds.width / narrowPhotoBounds.height).toBeCloseTo(320 / 256)
  expect(narrowPhotoBounds.x + narrowPhotoBounds.width).toBeCloseTo(
    narrowTextBounds.x + narrowTextBounds.width,
    0,
  )
  await page.getByRole('button', { name: '关闭猫咪面板' }).click()
  await page.setViewportSize({ width: 1200, height: 820 })

  await paste()
  await page.getByRole('button', { name: '发送', exact: true }).click()
  for (const [file, tag] of [
    ['语音.wav', 'audio'],
    ['短片.webm', 'video'],
  ]) {
    await page.getByRole('button', { name: '播放 ' + file }).click()
    const element = page.locator(tag! + '[aria-label="' + file + '"]')
    await expect(element).toBeVisible()
    await expect
      .poll(() => element.evaluate((node) => (node as HTMLMediaElement).readyState))
      .toBeGreaterThanOrEqual(2)
    expect(await element.evaluate((node) => (node as HTMLMediaElement).error)).toBeNull()
  }
})

test('群聊选文件和拖放先留在草稿，回车发送，失败重试复用上传结果', async ({ page }) => {
  await page.evaluate(() => {
    const backend = window.desktop!.backend!
    backend.uploadAttachment = async (file, options) => {
      if (options.groupId !== 'group') throw new Error('上传目标群聊错误')
      const uploaded = JSON.parse(sessionStorage.getItem('uploaded') ?? '[]') as string[]
      uploaded.push(file.name)
      sessionStorage.setItem('uploaded', JSON.stringify(uploaded))
      return {
        path: 'chat-files/01J00000000000000000000001/' + file.requestId,
        sha256: 'c'.repeat(64),
      }
    }
    backend.sendConversation = async (_id, _text, files, _reply, _mentions, requestId) => {
      if (!files?.length) throw new Error('缺少已上传附件')
      if (!sessionStorage.getItem('group-request')) {
        sessionStorage.setItem('group-request', requestId)
        throw new Error('消息提交失败')
      }
      if (sessionStorage.getItem('group-request') !== requestId) throw new Error('重试标识变化')
      return { acceptedMessageId: requestId }
    }
  })
  await page.getByRole('button', { name: '项目讨论', exact: true }).click()
  const input = page.getByRole('textbox', { name: '给项目讨论发消息' })
  await page.locator('.composer input[type=file]').setInputFiles({
    name: '议程.docx',
    mimeType: 'application/octet-stream',
    buffer: Buffer.from('议程'),
  })
  await input.evaluate((node) => {
    const files = new DataTransfer()
    files.items.add(new File(['打包'], '程序.jar'))
    node.dispatchEvent(
      new DragEvent('drop', { bubbles: true, cancelable: true, dataTransfer: files }),
    )
  })
  const tray = page.getByRole('list', { name: '待发送附件' })
  await expect(tray.getByText('程序.jar')).toBeVisible()
  await tray.getByText('程序.jar').hover()
  await tray.getByRole('button', { name: '移除程序.jar' }).click()
  expect(await page.evaluate(() => sessionStorage.getItem('uploaded'))).toBeNull()
  await input.press('Enter')
  await expect(page.getByText('消息提交失败', { exact: true })).toBeVisible()
  await expect(tray.getByText('议程.docx')).toBeVisible()
  await page.getByRole('button', { name: '发送', exact: true }).click()
  await expect(tray).toHaveCount(0)
  expect(await page.evaluate(() => JSON.parse(sessionStorage.getItem('uploaded')!))).toEqual([
    '议程.docx',
  ])
})
